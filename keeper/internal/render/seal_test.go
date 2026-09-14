package render

import (
	"context"
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

func sealTestEngine(t *testing.T) *cel.Engine {
	t.Helper()
	kv := stubKVRender{secrets: map[string]map[string]any{
		"secret/redis/admin": {"password": "s3cr3t"},
	}}
	e, err := cel.New(cel.WithVault(kv))
	if err != nil {
		t.Fatalf("cel.New(WithVault): %v", err)
	}
	return e
}

// secretInputNames extracts secret:true names from the scenario schema.
func TestSecretInputNames(t *testing.T) {
	scn := &config.ScenarioManifest{Input: config.InputSchemaMap{
		"password": {Type: "string", Secret: true},
		"hostname": {Type: "string"},
		"api_key":  {Type: "string", Secret: true},
	}}
	got := secretInputNames(scn)
	if !got["password"] || !got["api_key"] {
		t.Errorf("secret names not collected: %v", got)
	}
	if got["hostname"] {
		t.Errorf("non-secret hostname ended up in the set: %v", got)
	}
}

// (a) a secret-input value in a GENERIC field (content) → path sealed.
// (f) an ordinary generic field (non-secret input) → NOT sealed (no over-seal).
func TestCollectSealed_SecretInputInGenericField(t *testing.T) {
	e := sealTestEngine(t)
	set := NewSealedSet()
	sources := cel.SealSources{Fields: map[string]bool{cel.FieldAddr("input", "admin_password"): true}}

	params := map[string]any{
		"content": "requirepass ${ input.admin_password }", // (a) generic field, secret
		"port":    "${ input.port }",                       // (f) non-secret input
		"label":   "static-config",                         // pure literal
	}
	collectSealed(e, set, params, sources, "")

	paths := set.Paths()
	if !paths["content"] {
		t.Errorf("content (secret-input in a generic field) not sealed: %v", paths)
	}
	if paths["port"] {
		t.Errorf("port (non-secret input) sealed - over-seal: %v", paths)
	}
	if paths["label"] {
		t.Errorf("label (literal) sealed - over-seal: %v", paths)
	}
}

// (b) a vault() value → path sealed (no schema needed — the detector catches vault itself).
func TestCollectSealed_VaultValue(t *testing.T) {
	e := sealTestEngine(t)
	set := NewSealedSet()

	params := map[string]any{
		"token": "${ vault('secret/redis/admin#password') }",
		"plain": "no-secret",
	}
	collectSealed(e, set, params, cel.SealSources{}, "")

	paths := set.Paths()
	if !paths["token"] {
		t.Errorf("vault() cell not sealed: %v", paths)
	}
	if paths["plain"] {
		t.Errorf("plain sealed — over-seal: %v", paths)
	}
}

// (c) a ternary reading a secret-input → path sealed (whole-cell).
func TestCollectSealed_TernaryReadsSecret(t *testing.T) {
	e := sealTestEngine(t)
	set := NewSealedSet()
	sources := cel.SealSources{Fields: map[string]bool{cel.FieldAddr("input", "tls_cert"): true}}

	params := map[string]any{
		"cert": "${ has(input.tls_cert) ? input.tls_cert : '' }",
	}
	collectSealed(e, set, params, sources, "")
	if !set.Paths()["cert"] {
		t.Errorf("ternary with secret-input not sealed: %v", set.Paths())
	}
}

// (d) a mixed value (literal + secret) → path sealed (whole-value taint).
func TestCollectSealed_MixedLiteralSecret(t *testing.T) {
	e := sealTestEngine(t)
	set := NewSealedSet()
	sources := cel.SealSources{Fields: map[string]bool{cel.FieldAddr("input", "password"): true}}

	params := map[string]any{
		"line": "user=admin pass=${ input.password } host=db",
	}
	collectSealed(e, set, params, sources, "")
	if !set.Paths()["line"] {
		t.Errorf("mixed literal+secret not sealed: %v", set.Paths())
	}
}

// map/list nesting — the path is tracked the same way as renderValue (joinKey/joinIdx).
func TestCollectSealed_NestedPaths(t *testing.T) {
	e := sealTestEngine(t)
	set := NewSealedSet()
	sources := cel.SealSources{Fields: map[string]bool{cel.FieldAddr("input", "pw"): true}}

	params := map[string]any{
		"acl": []any{
			map[string]any{"name": "alice", "secret": "${ input.pw }"},
		},
		"nested": map[string]any{"token": "${ vault('secret/redis/admin#password') }"},
	}
	collectSealed(e, set, params, sources, "")

	paths := set.Paths()
	if !paths["acl[0].secret"] {
		t.Errorf("acl[0].secret not sealed: %v", paths)
	}
	if !paths["nested.token"] {
		t.Errorf("nested.token not sealed: %v", paths)
	}
}

// nil Sealed → no-op (collection disabled, push/trial/Acolyte — bit-for-bit).
func TestCollectSealed_NilSetNoop(t *testing.T) {
	e := sealTestEngine(t)
	params := map[string]any{"x": "${ vault('secret/redis/admin#password') }"}
	// doesn't panic on a nil set
	collectSealed(e, nil, params, cel.SealSources{}, "")
	var nilSet *SealedSet
	if nilSet.Paths() != nil {
		t.Error("nil-SealedSet.Paths() should be nil")
	}
}

// Path traversal matches renderValue (joinKey/joinIdx) — guards against a
// drift that would break the correspondence between sealed paths and masking paths.
func TestCollectSealed_PathConventionMatchesRenderValue(t *testing.T) {
	e := sealTestEngine(t)
	set := NewSealedSet()
	sources := cel.SealSources{Fields: map[string]bool{cel.FieldAddr("input", "s"): true}}
	params := map[string]any{"a": map[string]any{"b": []any{"${ input.s }"}}}
	collectSealed(e, set, params, sources, "")
	// renderValue would build the path "a.b[0]" for this.
	if !set.Paths()["a.b[0]"] {
		t.Errorf("path is not a.b[0]: %v", set.Paths())
	}
}

// The PEM content of a redis destiny task (core.file.present, content =
// vault(input.tls.<x>_ref)) gets marked sealed by the VAULT layer alone, with no
// schema in the sources at all. Guards PEM masking (ADR-010 §7.4) at its
// narrowest: vault() in the content cell itself is what catches it, and that
// holds however little else the pass knows.
//
// The empty sources here are no longer what a destiny pass actually gets — since
// NIM-812 it carries the destiny's own `input:` schema, so an already-resolved
// PEM handed in as `${ input.tls_cert }` is sealed too (that path is guarded by
// TestRender_ApplyDestinySealsItsOwnSecretInput, which renders it end to end).
// This case is kept as the floor: the vault layer must not come to DEPEND on the
// schema being there. Mirrors L0 tls-enabled-standalone (same content shape).
func TestCollectSealed_RedisTLSPEMContentViaVault(t *testing.T) {
	e := sealTestEngine(t)
	set := NewSealedSet()

	// Shape of the content cell for a redis destiny PEM task (server.yml).
	// sources without a schema — as in the destiny pass (destinyIn.Scenario
	// without Input → secretInputNames is empty).
	params := map[string]any{
		"path":    "/etc/redis/tls/redis.key",
		"content": "${ vault(input.tls.key_ref) }",
		"mode":    "0600",
		"owner":   "redis",
	}
	collectSealed(e, set, params, cel.SealSources{}, "")

	paths := set.Paths()
	if !paths["content"] {
		t.Errorf("PEM content (vault() in the cell) NOT sealed - PEM will leak into state/error: %v", paths)
	}
	if paths["path"] || paths["mode"] || paths["owner"] {
		t.Errorf("non-secret fields sealed - over-seal: %v", paths)
	}
}

// literalRefParams — the cell shapes a bare `vault:` ref can take, next to the
// near-misses that must NOT be sealed. Used by both tests below so the seal
// side and the resolve side are asked about the same strings.
func literalRefParams() map[string]any {
	return map[string]any{
		// The NIM-668 shape: `credentials: vault:secret/cloud/demo-dev` on a
		// core.cloud.provisioned step — a whole-cell ref with no `${ … }` for
		// DetectSealed to read.
		"credentials": "vault:secret/redis/admin",
		"field_ref":   "vault:secret/redis/admin#password",
		// Near-misses: `vault:` not at position 0. walkVaultValue resolves by
		// prefix, so these stay literal text and there is nothing to seal.
		"mid_string": "see vault:secret/redis/admin for the credentials",
		"plain":      "no-secret",
	}
}

// TestCollectSealed_LiteralVaultRefCell ★ — a bare `vault:<mount>/<path>` cell
// is sealed on the RAW params, before the vault-resolve phase replaces it with
// the secret. Without this the resolved secret would reach the masker's
// declarative layers unannounced, and only the key-name regex could still catch
// it — masking a `credentials` key but not a `cloud_auth` one, and raising a
// declarative-gap alarm on every run where it worked.
func TestCollectSealed_LiteralVaultRefCell(t *testing.T) {
	e := sealTestEngine(t)
	set := NewSealedSet()

	collectSealed(e, set, literalRefParams(), cel.SealSources{}, "")

	paths := set.Paths()
	for _, k := range []string{"credentials", "field_ref"} {
		if !paths[k] {
			t.Errorf("%s (literal vault: ref) not sealed — the resolved secret would arrive unannounced: %v", k, paths)
		}
	}
	for _, k := range []string{"mid_string", "plain"} {
		if paths[k] {
			t.Errorf("%s sealed — over-seal on a cell the vault phase leaves alone: %v", k, paths)
		}
	}
}

// TestCollectSealed_MatchesVaultResolve ★ — the seal predicate and the resolve
// predicate are the same predicate. Rather than restate `HasPrefix` in the
// assertion (which would pass even if walkVaultValue moved to, say, a full-ref
// regex), this runs the actual vault-resolve over the same params and requires
// exactly the cells it REPLACED to be exactly the cells that were sealed.
func TestCollectSealed_MatchesVaultResolve(t *testing.T) {
	e := sealTestEngine(t)
	set := NewSealedSet()
	params := literalRefParams()

	collectSealed(e, set, params, cel.SealSources{}, "")

	kv := stubKVRender{secrets: map[string]map[string]any{
		"secret/redis/admin": {"password": "s3cr3t"},
	}}
	resolved, err := resolveVaultRefs(context.Background(), kv, params)
	if err != nil {
		t.Fatalf("resolveVaultRefs: %v", err)
	}

	replaced := map[string]bool{}
	for k, before := range params {
		if !reflect.DeepEqual(before, resolved[k]) {
			replaced[k] = true
		}
	}
	if sealed := set.Paths(); !reflect.DeepEqual(replaced, sealed) {
		t.Errorf("cells the vault phase replaced = %v, cells sealed = %v — the two predicates have drifted apart", replaced, sealed)
	}
}

// TestMaskSecretsSealed_ResolvedVaultRefSubtree ★ — the point of sealing the
// raw cell: after resolve, `credentials` holds the secret MAP, and the seal
// makes the DECLARATIVE layer mask the whole subtree. The alarm hook must stay
// silent — it signals a declarative gap, and firing it on a step that named its
// secret properly would train an operator to ignore it.
func TestMaskSecretsSealed_ResolvedVaultRefSubtree(t *testing.T) {
	e := sealTestEngine(t)
	set := NewSealedSet()
	params := map[string]any{
		"driver":      "democloud",
		"credentials": "vault:secret/redis/admin",
	}
	collectSealed(e, set, params, cel.SealSources{}, "")

	// What the cell looks like once the vault phase has replaced it.
	rendered := map[string]any{
		"driver":      "democloud",
		"credentials": map[string]any{"password": "s3cr3t", "access_key_id": "AKIA"},
	}

	var alarms []string
	masked := audit.MaskSecretsSealed(rendered, audit.SealOpts{
		Sealed:        set.Paths(),
		RegexFallback: func(path string) { alarms = append(alarms, path) },
	})

	if masked["credentials"] == nil {
		t.Fatal("credentials cell disappeared instead of being masked")
	}
	if sub, isMap := masked["credentials"].(map[string]any); isMap {
		t.Fatalf("credentials masked per-key instead of as a whole subtree: %v", sub)
	}
	if masked["driver"] != "democloud" {
		t.Errorf("driver = %v, want it untouched", masked["driver"])
	}
	if len(alarms) != 0 {
		t.Errorf("regex-fallback alarm fired for %v — the seal should have caught the cell declaratively", alarms)
	}
}

// GUARD (NIM-758): the seal's path spelling and the masker's are ONE spelling.
//
// [collectSealed] produces the paths, [SealedValues] consumes them, and nothing
// but this test holds the two together — a divergence in joinKey/joinIdx would
// leave the masker silently masking nothing, which is the worst shape a masking
// defect can take. So the paths here come from the real producer, never from a
// literal: hand-spelling them would assert a human's guess against itself.
func TestSealedValues_PathSpellingComesFromTheCollector(t *testing.T) {
	e := sealTestEngine(t)
	set := NewSealedSet()
	sources := cel.SealSources{Fields: map[string]bool{cel.FieldAddr("input", "admin_password"): true}}

	// RAW params, as the author wrote them — what the seal is collected from.
	raw := map[string]any{
		"credentials": map[string]any{
			"token": "${ input.admin_password }",
			"user":  "svc",
		},
		"keys":   []any{"public", "${ vault('secret/redis/admin#password') }"},
		"region": "ru-central1",
	}
	collectSealed(e, set, raw, sources, "")

	// RENDERED params, as they reach the module — the same cells, values arrived.
	rendered := map[string]any{
		"credentials": map[string]any{
			"token": "resolved-admin-password",
			"user":  "svc",
		},
		"keys":   []any{"public", "s3cr3t"},
		"region": "ru-central1",
	}
	got := SealedValues(rendered, set.Paths())

	want := []string{"resolved-admin-password", "s3cr3t"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SealedValues = %v, want %v (paths from collectSealed: %v)", got, want, set.Paths())
	}
}

// GUARD (NIM-758): a sealed cell whose RESOLVED value is a composite is masked
// WHOLE, every string leaf under it.
//
// This is not an edge case, it is the credentials-as-params shape the keeper-side
// plugin path exists for. `vault:<mount>/<path>` with no `#field` seals ONE path
// (the raw cell is one string) and resolves to the entire KV map, so recording
// only a string AT the sealed path would find nothing and mask nothing —
// silently, on exactly the input the masking was built for.
func TestSealedValues_SealedSubtreeIsMaskedWhole(t *testing.T) {
	e := sealTestEngine(t)
	set := NewSealedSet()

	// One raw cell, one sealed path: the bare vault-ref branch of walkSealed.
	raw := map[string]any{"creds": "vault:secret/democloud/prod/cloud", "region": "ru-central1"}
	collectSealed(e, set, raw, cel.SealSources{}, "")
	if paths := set.Paths(); !paths["creds"] || len(paths) != 1 {
		t.Fatalf("sealed paths = %v, want exactly {creds} (the raw cell is one string)", paths)
	}

	// readVaultRef with no `#field` hands back the WHOLE KV map.
	rendered := map[string]any{
		"creds": map[string]any{
			"token": "s3cr3t",
			"user":  "svc",
			"nested": map[string]any{
				"refresh": "r3fr3sh",
			},
		},
		"region": "ru-central1",
	}
	got := SealedValues(rendered, set.Paths())

	want := []string{"r3fr3sh", "s3cr3t", "svc"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SealedValues = %v, want %v — every string leaf of the sealed subtree", got, want)
	}
	for _, v := range got {
		if v == "ru-central1" {
			t.Error("an unsealed sibling cell was collected — the subtree rule must not leak outward")
		}
	}
}
