package render

import (
	"context"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// The register→CEL-root boundary of [ADR-0083] §6: a declared secret travels in
// the keeper register as `vault:<path>#<field>`, never as plaintext, and is
// resolved here on the way into the render context.

func regKV() *countingKV {
	return &countingKV{
		secrets: map[string]map[string]any{
			"secret/wb-service-redis/redis-prod/redis_users/alice": {"password": "ALICE-PW"},
			"secret/wb-service-redis/redis-prod/admin_password":    {"value": "ADMIN-PW"},
			"secret/keeper/jwt-signing-key":                        {"key": "CLUSTER-SIGNING-KEY"},
			"secret/other-service/redis-prod/admin_password":       {"value": "NEIGHBOUR-PW"},
		},
		calls: map[string]int{},
	}
}

func regInput(keeper map[string]any) RenderInput {
	return RenderInput{
		Incarnation:    IncarnationMeta{Name: "redis-prod", Service: "wb-service-redis"},
		KeeperRegister: keeper,
	}
}

// TestResolveRegisterSecrets_OwnNamespace — the ref a `core.state.present` task
// produced for this run's own service+incarnation resolves to the value, at every
// depth, and through the render-pass memo (one ReadKV for a repeated path).
func TestResolveRegisterSecrets_OwnNamespace(t *testing.T) {
	kv := regKV()
	p := NewPipeline(kv, nil, nil, nil)
	ctx := cel.WithVaultMemo(context.Background())

	in := regInput(map[string]any{
		"redis_users": map[string]any{
			"effective": []any{
				map[string]any{"name": "alice", "password": "vault:secret/wb-service-redis/redis-prod/redis_users/alice#password"},
			},
		},
		"admin": map[string]any{
			"effective": "vault:secret/wb-service-redis/redis-prod/admin_password#value",
			"again":     "vault:secret/wb-service-redis/redis-prod/admin_password#value",
		},
	})

	out, sealed, err := p.resolveRegisterSecrets(ctx, in)
	if err != nil {
		t.Fatalf("resolveRegisterSecrets: %v", err)
	}
	if !sealed["redis_users"] || !sealed["admin"] {
		t.Errorf("sealed = %v, want both registers marked -- their cells must be masked on the way out", sealed)
	}
	users, _ := out["redis_users"].(map[string]any)
	list, _ := users["effective"].([]any)
	first, _ := list[0].(map[string]any)
	if first["password"] != "ALICE-PW" {
		t.Errorf("nested collection ref = %v, want the resolved value", first["password"])
	}
	admin, _ := out["admin"].(map[string]any)
	if admin["effective"] != "ADMIN-PW" || admin["again"] != "ADMIN-PW" {
		t.Errorf("scalar refs = %v", admin)
	}
	if got := kv.calls["secret/wb-service-redis/redis-prod/admin_password"]; got != 1 {
		t.Errorf("ReadKV calls = %d, want 1 (the boundary must share the render-pass memo)", got)
	}
	// The source map is rendered again per passage and per retry — it must not have
	// been mutated into plaintext.
	src, _ := in.KeeperRegister["admin"].(map[string]any)
	if src["effective"] != "vault:secret/wb-service-redis/redis-prod/admin_password#value" {
		t.Errorf("the input register was mutated: %v", src)
	}
}

// TestResolveRegisterSecrets_ForeignNamespaceLeftLiteral — ★ the guard that keeps
// this boundary from being a read primitive over the whole KV store. A ref outside
// `<mount>/<service>/<incarnation>/` is left as the string it is: not resolved, and
// NOT an error (a keeper module may legitimately carry such a string as data).
func TestResolveRegisterSecrets_ForeignNamespaceLeftLiteral(t *testing.T) {
	foreign := []string{
		"vault:secret/keeper/jwt-signing-key#key",                                   // the cluster's own signing key
		"vault:secret/other-service/redis-prod/admin_password#value",                // a neighbouring service
		"vault:secret/wb-service-redis/other-inc/admin_password#value",              // a neighbouring incarnation
		"vault:secret/wb-service-redis-evil/redis-prod/x#value",                     // prefix, not a whole segment
		"vault:secret/wb-service-redis/redis-prod/../../keeper/jwt-signing-key#key", // traversal
		"vault:secret/wb-service-redis/redis-prod//x#value",                         // empty segment
		"vault:secret/wb-service-redis#value",                                       // too few segments
	}
	for _, ref := range foreign {
		kv := regKV()
		p := NewPipeline(kv, nil, nil, nil)
		out, sealed, err := p.resolveRegisterSecrets(cel.WithVaultMemo(context.Background()), regInput(map[string]any{
			"probe": map[string]any{"stdout": ref},
		}))
		if err != nil {
			t.Fatalf("%s: resolveRegisterSecrets returned an error, want the literal left alone: %v", ref, err)
		}
		if len(sealed) != 0 {
			t.Errorf("%s: sealed = %v, want nothing sealed -- no secret was read", ref, sealed)
		}
		probe, _ := out["probe"].(map[string]any)
		if probe["stdout"] != ref {
			t.Errorf("%s resolved to %v, want the literal string", ref, probe["stdout"])
		}
		if len(kv.calls) != 0 {
			t.Errorf("%s: Vault was read %v, want no read at all", ref, kv.calls)
		}
	}
}

// TestResolveRegisterSecrets_PerHostBucketNeverResolved — ★ the second half of the
// same guard. Per-host buckets carry what SOULS reported. A compromised host that
// registers `vault:secret/<service>/<incarnation>/…` must not have Keeper read it
// back into that host's own params.
//
// The keeper bucket is deliberately NON-EMPTY and carries a resolvable ref: with an
// empty one the function early-returns and a mutation that walks the per-host
// buckets would sail past this test.
func TestResolveRegisterSecrets_PerHostBucketNeverResolved(t *testing.T) {
	kv := regKV()
	p := NewPipeline(kv, nil, nil, nil)
	legit := "vault:secret/wb-service-redis/redis-prod/admin_password#value"
	evil := "vault:secret/wb-service-redis/redis-prod/redis_users/alice#password"
	in := regInput(map[string]any{"admin": map[string]any{"effective": legit}})
	in.Register = map[string]any{"evil": map[string]any{"stdout": evil}}
	in.RegisterByHost = map[string]map[string]any{
		"host-a.example.com": {"evil": map[string]any{"stdout": evil}},
	}

	out, sealed, err := p.resolveRegisterSecrets(cel.WithVaultMemo(context.Background()), in)
	if err != nil {
		t.Fatalf("resolveRegisterSecrets: %v", err)
	}
	if sealed["evil"] {
		t.Errorf("a Soul-reported register was sealed: %v", sealed)
	}
	if !sealed["admin"] {
		t.Errorf("sealed = %v, want the keeper register marked", sealed)
	}
	// The keeper bucket resolved (so the walk really ran) …
	admin, _ := out["admin"].(map[string]any)
	if admin["effective"] != "ADMIN-PW" {
		t.Fatalf("keeper bucket = %v, want it resolved -- otherwise this test proves nothing", out)
	}
	// … and the Soul-reported one did not even get looked at.
	if _, ok := out["evil"]; ok {
		t.Errorf("a Soul-reported register entered the resolved bucket: %v", out)
	}
	if n := kv.calls["secret/wb-service-redis/redis-prod/redis_users/alice"]; n != 0 {
		t.Errorf("a Soul-reported register was read from Vault %d times", n)
	}
	// And the bucket the host actually reads still holds the literal.
	hr := hostRegister(in, &topology.HostFacts{SID: "host-a.example.com"})
	e, _ := hr["evil"].(map[string]any)
	if e["stdout"] != evil {
		t.Errorf("host register = %v, want the literal string", e)
	}
}

// TestResolveRegisterSecrets_UnknownOwner — push/trial/unit callers have no
// service+incarnation. With no owner to check a namespace against, resolve
// NOTHING rather than everything.
func TestResolveRegisterSecrets_UnknownOwner(t *testing.T) {
	kv := regKV()
	p := NewPipeline(kv, nil, nil, nil)
	ref := "vault:secret/wb-service-redis/redis-prod/admin_password#value"
	in := RenderInput{KeeperRegister: map[string]any{"a": ref}}

	out, sealed, err := p.resolveRegisterSecrets(cel.WithVaultMemo(context.Background()), in)
	if err != nil {
		t.Fatalf("resolveRegisterSecrets: %v", err)
	}
	if len(sealed) != 0 {
		t.Errorf("sealed = %v, want nothing sealed without a known owner", sealed)
	}
	if out["a"] != ref {
		t.Errorf("out[a] = %v, want the untouched literal", out["a"])
	}
	if len(kv.calls) != 0 {
		t.Errorf("Vault was read without a known owner: %v", kv.calls)
	}
}

// TestResolveRegisterSecrets_ReadFailureIsAnError — an own-namespace ref that does
// not resolve fails the render. Silently leaving the ref as a string would hand the
// literal `vault:…` to the module as if it were the password.
func TestResolveRegisterSecrets_ReadFailureIsAnError(t *testing.T) {
	kv := &countingKV{secrets: map[string]map[string]any{}, calls: map[string]int{}}
	p := NewPipeline(kv, nil, nil, nil)
	_, _, err := p.resolveRegisterSecrets(cel.WithVaultMemo(context.Background()), regInput(map[string]any{
		"admin": "vault:secret/wb-service-redis/redis-prod/admin_password#value",
	}))
	if err == nil {
		t.Fatal("expected an error for an unresolvable own-namespace ref")
	}
	if !strings.Contains(err.Error(), "secret/wb-service-redis/redis-prod/admin_password") {
		t.Errorf("error does not name the path: %q", err.Error())
	}
}

// TestRender_KeeperRegisterSecretReachesHostTask — ★ the end-to-end pin for
// [ADR-0083] §5+§6 and for the WIRING between them, which the unit tests above
// cannot see: a keeper-side task wrote a declared secret and registered a `vault:`
// reference; a Soul-side task in a later Passage reads `register.<name>.effective`
// and must receive the VALUE.
//
// The same render also carries a ref a HOST reported. It must come through as the
// literal string — the boundary resolves the keeper bucket and nothing else.
func TestRender_KeeperRegisterSecretReachesHostTask(t *testing.T) {
	kv := regKV()
	p := NewPipeline(kv, newEngine(t), nil, nil)
	evil := "vault:secret/wb-service-redis/redis-prod/redis_users/alice#password"

	in := RenderInput{
		Scenario: &config.ScenarioManifest{
			Name: "update_users",
			Tasks: []config.Task{{
				Name: "Write the ACL",
				Module: &config.ModuleTask{
					Module: "core.file.present",
					Params: map[string]any{
						"path":    "/etc/redis/users.acl",
						"content": "${ register.admin.effective }",
						"leak":    "${ register.evil.stdout }",
					},
				},
			}},
		},
		Incarnation:    IncarnationMeta{Name: "redis-prod", Service: "wb-service-redis"},
		Hosts:          []*topology.HostFacts{host("a.example.com", []string{"redis"}, nil)},
		KeeperRegister: map[string]any{"admin": map[string]any{"effective": "vault:secret/wb-service-redis/redis-prod/admin_password#value"}},
		RegisterByHost: map[string]map[string]any{
			"a.example.com": {"evil": map[string]any{"stdout": evil}},
		},
		Sealed: NewSealedSet(),
	}

	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1", len(tasks))
	}
	f := tasks[0].Params.GetFields()
	if got := f["content"].GetStringValue(); got != "ADMIN-PW" {
		t.Errorf("content = %v, want the keeper register's secret resolved to its value", got)
	}
	if got := f["leak"].GetStringValue(); got != evil {
		t.Errorf("leak = %v, want the host-reported ref left as the literal string", got)
	}
	if n := kv.calls["secret/wb-service-redis/redis-prod/redis_users/alice"]; n != 0 {
		t.Errorf("a host-reported ref was read from Vault %d times", n)
	}
	// ★ And the cell that now holds the password is SEALED: a resolved reference
	// is a secret source like any other, so status_details masks it.
	paths := in.Sealed.Paths()
	if !paths["content"] {
		t.Errorf("sealed paths = %v, want the cell reading the secret register sealed", paths)
	}
	if paths["leak"] {
		t.Errorf("sealed paths = %v, want a cell reading an ordinary register left unsealed", paths)
	}
}
