package scenario

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// fakeVaultReader — an InputVaultReader: returns a fixed secret, records
// the paths read (to verify that a reject does NOT reach ReadKV).
type fakeVaultReader struct {
	data map[string]map[string]any
	read []string
}

func (f *fakeVaultReader) ReadKV(_ context.Context, path string) (map[string]any, error) {
	f.read = append(f.read, path)
	if d, ok := f.data[path]; ok {
		return d, nil
	}
	return nil, errors.New("not found")
}

// fakeAuditWriter — collects written events.
type fakeAuditWriter struct {
	events []*audit.Event
}

func (f *fakeAuditWriter) Write(_ context.Context, e *audit.Event) error {
	f.events = append(f.events, e)
	return nil
}

func vaultTestRunner(vc InputVaultReader, aw audit.Writer, deny []string) *Runner {
	return &Runner{
		deps:   Deps{Vault: vc, Audit: aw, InputDenyPaths: deny},
		logger: slog.New(slog.DiscardHandler),
	}
}

// scopedSecret — an InputSchema secret field with the given vault_scope.
func scopedSecret(scope string) *config.InputSchema {
	return &config.InputSchema{Type: "string", Secret: true, VaultScope: scope}
}

func ac() inputVaultAuditCtx {
	return inputVaultAuditCtx{aid: "archon-alice", incarnation: "redis-prod", scenario: "create"}
}

// TestInputVaultResolver_InScopeReads — a ref in scope resolves to the field value,
// audit writes result=ok with path+field, without the secret value.
func TestInputVaultResolver_InScopeReads(t *testing.T) {
	vc := &fakeVaultReader{data: map[string]map[string]any{
		"secret/services/redis/prod": {"password": "s3cr3t-resolved-32ch"},
	}}
	aw := &fakeAuditWriter{}
	r := vaultTestRunner(vc, aw, nil)
	resolve := r.newInputVaultResolver(context.Background(), ac(), nil)

	got, err := resolve("redis_password", scopedSecret("secret/services/redis/*"),
		"vault:secret/services/redis/prod#password")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "s3cr3t-resolved-32ch" {
		t.Fatalf("got=%v", got)
	}
	requireAudit(t, aw, "ok", "secret/services/redis/prod", "redis_password")
	// The secret value must not end up in the payload.
	for _, e := range aw.events {
		for k, v := range e.Payload {
			if s, ok := v.(string); ok && s == "s3cr3t-resolved-32ch" {
				t.Fatalf("секрет утёк в audit payload[%s]", k)
			}
		}
	}
}

// TestInputVaultResolver_NoScopeRejected — a field without vault_scope → reject
// (default-deny), ReadKV isn't called, audit denied/no_scope.
func TestInputVaultResolver_NoScopeRejected(t *testing.T) {
	vc := &fakeVaultReader{}
	aw := &fakeAuditWriter{}
	r := vaultTestRunner(vc, aw, nil)
	resolve := r.newInputVaultResolver(context.Background(), ac(), nil)

	_, err := resolve("pw", &config.InputSchema{Type: "string", Secret: true},
		"vault:secret/services/redis/prod#password")
	if err == nil {
		t.Fatal("ожидался reject default-deny")
	}
	if len(vc.read) != 0 {
		t.Fatalf("ReadKV не должен вызываться при default-deny, read=%v", vc.read)
	}
	requireAuditReason(t, aw, "denied", "no_scope")
}

// TestInputVaultResolver_OutOfScopeRejected — a ref outside scope → reject, no ReadKV.
func TestInputVaultResolver_OutOfScopeRejected(t *testing.T) {
	vc := &fakeVaultReader{}
	aw := &fakeAuditWriter{}
	r := vaultTestRunner(vc, aw, nil)
	resolve := r.newInputVaultResolver(context.Background(), ac(), nil)

	_, err := resolve("pw", scopedSecret("secret/services/redis/*"),
		"vault:secret/services/postgres/prod#password")
	if err == nil {
		t.Fatal("ожидался reject out-of-scope")
	}
	if len(vc.read) != 0 {
		t.Fatalf("ReadKV не должен вызываться вне scope, read=%v", vc.read)
	}
	requireAuditReason(t, aw, "denied", "out_of_scope")
}

// TestInputVaultResolver_DenyListRejected — a ref in scope, but on the hard deny-list
// (secret/keeper/*) → reject, no ReadKV, audit denied/deny_list.
//
// Covers deny-list bypasses via a non-normalized path (security regression):
// `secret//keeper/x` (double slash), `secret/keeper/../keeper/x`, and
// `secret/./keeper/x` (dot segments) under an accidentally broad scope `secret/*`. Before
// the ParseRef fix, all three bypassed DeniedByVaultFloor (HasPrefix didn't match), and
// ReadKV would resolve the path down to the actually forbidden `secret/keeper/...`. Now the ref
// is normalized in ParseRef: `//` collapses → caught by the deny-list (denied/
// deny_list), `.`/`..` are rejected earlier → denied/parse_error. In both cases
// ReadKV is NOT called.
func TestInputVaultResolver_DenyListRejected(t *testing.T) {
	cases := []struct {
		name       string
		ref        string
		wantReason string
	}{
		{"plain", "vault:secret/keeper/jwt-signing-key#key", "deny_list"},
		// `//` normalizes to `/` → falls under the deny-list.
		{"double_slash", "vault:secret//keeper/jwt-signing-key#key", "deny_list"},
		// `..`/`.` segments are rejected by ParseRef BEFORE the deny-check.
		{"dot_dot", "vault:secret/keeper/../keeper/jwt-signing-key#key", "parse_error"},
		{"single_dot", "vault:secret/./keeper/jwt-signing-key#key", "parse_error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			vc := &fakeVaultReader{}
			aw := &fakeAuditWriter{}
			r := vaultTestRunner(vc, aw, nil)
			resolve := r.newInputVaultResolver(context.Background(), ac(), nil)

			// scope is accidentally broad (secret/*) — protection relies only on deny-list/ParseRef.
			_, err := resolve("bad", scopedSecret("secret/*"), c.ref)
			if err == nil {
				t.Fatalf("ожидался reject для %q", c.ref)
			}
			if len(vc.read) != 0 {
				t.Fatalf("ReadKV не должен вызываться (обход deny-list), read=%v", vc.read)
			}
			requireAuditReason(t, aw, "denied", c.wantReason)
		})
	}
}

// TestInputVaultResolver_ConfigDenyExtends — config-extension of the deny-list works.
func TestInputVaultResolver_ConfigDenyExtends(t *testing.T) {
	vc := &fakeVaultReader{data: map[string]map[string]any{
		"secret/team/x": {"k": "v"},
	}}
	aw := &fakeAuditWriter{}
	r := vaultTestRunner(vc, aw, []string{"secret/team/"})
	resolve := r.newInputVaultResolver(context.Background(), ac(), []string{"secret/team/"})

	_, err := resolve("pw", scopedSecret("secret/*"), "vault:secret/team/x#k")
	if err == nil {
		t.Fatal("ожидался reject config-deny secret/team/")
	}
	requireAuditReason(t, aw, "denied", "deny_list")
}

// TestInputVaultResolver_NilVault — the factory with a nil Vault returns a nil resolver
// (input-vault-refs aren't supported).
func TestInputVaultResolver_NilVault(t *testing.T) {
	r := vaultTestRunner(nil, &fakeAuditWriter{}, nil)
	if got := r.newInputVaultResolver(context.Background(), ac(), nil); got != nil {
		t.Fatal("при nil-Vault резолвер должен быть nil")
	}
}

func requireAudit(t *testing.T, aw *fakeAuditWriter, result, path, field string) {
	t.Helper()
	if len(aw.events) == 0 {
		t.Fatal("audit-event не записан")
	}
	e := aw.events[len(aw.events)-1]
	if e.EventType != audit.EventInputVaultResolved {
		t.Fatalf("event_type=%q", e.EventType)
	}
	if e.Source != audit.SourceKeeperInternal {
		t.Fatalf("source=%q", e.Source)
	}
	if e.Payload["result"] != result {
		t.Fatalf("result=%v want %q", e.Payload["result"], result)
	}
	if path != "" && e.Payload["path"] != path {
		t.Fatalf("path=%v want %q", e.Payload["path"], path)
	}
	if e.Payload["field"] != field {
		t.Fatalf("field=%v want %q", e.Payload["field"], field)
	}
	if e.Payload["aid"] != "archon-alice" {
		t.Fatalf("aid=%v", e.Payload["aid"])
	}
}

func requireAuditReason(t *testing.T, aw *fakeAuditWriter, result, reason string) {
	t.Helper()
	if len(aw.events) == 0 {
		t.Fatal("audit-event не записан (denied тоже аудируется)")
	}
	e := aw.events[len(aw.events)-1]
	if e.Payload["result"] != result {
		t.Fatalf("result=%v want %q", e.Payload["result"], result)
	}
	if e.Payload["reason"] != reason {
		t.Fatalf("reason=%v want %q", e.Payload["reason"], reason)
	}
}
