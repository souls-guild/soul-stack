package scenario

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// fakeVaultReader — InputVaultReader: отдаёт фикс-секрет, регистрирует
// прочитанные пути (чтобы проверить, что reject НЕ доходит до ReadKV).
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

// fakeAuditWriter — собирает записанные events.
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

// scopedSecret — InputSchema secret-поля с заданным vault_scope.
func scopedSecret(scope string) *config.InputSchema {
	return &config.InputSchema{Type: "string", Secret: true, VaultScope: scope}
}

func ac() inputVaultAuditCtx {
	return inputVaultAuditCtx{aid: "archon-alice", incarnation: "redis-prod", scenario: "create"}
}

// TestInputVaultResolver_InScopeReads — ref в scope резолвится в значение поля,
// audit пишет result=ok с path+field, без значения секрета.
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
	// значение секрета не должно попасть в payload.
	for _, e := range aw.events {
		for k, v := range e.Payload {
			if s, ok := v.(string); ok && s == "s3cr3t-resolved-32ch" {
				t.Fatalf("секрет утёк в audit payload[%s]", k)
			}
		}
	}
}

// TestInputVaultResolver_NoScopeRejected — поле без vault_scope → reject
// (default-deny), ReadKV не вызывается, audit denied/no_scope.
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

// TestInputVaultResolver_OutOfScopeRejected — ref вне scope → reject, без ReadKV.
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

// TestInputVaultResolver_DenyListRejected — ref в scope, но в hard deny-list
// (secret/keeper/*) → reject, без ReadKV, audit denied/deny_list.
//
// Покрывает обходы deny-list через ненормализованный путь (security-regression):
// `secret//keeper/x` (двойной слэш), `secret/keeper/../keeper/x` и
// `secret/./keeper/x` (dot-сегменты) при ошибочно широком scope `secret/*`. До
// фикса ParseRef все три обходили DeniedByVaultFloor (HasPrefix не матчил), а
// ReadKV сводил путь к реальному запрещённому `secret/keeper/...`. Теперь ref
// нормализуется в ParseRef: `//` схлопывается → ловится deny-list-ом (denied/
// deny_list), `.`/`..` отвергаются раньше → denied/parse_error. В обоих случаях
// ReadKV НЕ вызывается.
func TestInputVaultResolver_DenyListRejected(t *testing.T) {
	cases := []struct {
		name       string
		ref        string
		wantReason string
	}{
		{"plain", "vault:secret/keeper/jwt-signing-key#key", "deny_list"},
		// `//` нормализуется в `/` → попадает под deny-list.
		{"double_slash", "vault:secret//keeper/jwt-signing-key#key", "deny_list"},
		// `..`/`.`-сегменты отвергаются ParseRef-ом ДО deny-check.
		{"dot_dot", "vault:secret/keeper/../keeper/jwt-signing-key#key", "parse_error"},
		{"single_dot", "vault:secret/./keeper/jwt-signing-key#key", "parse_error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			vc := &fakeVaultReader{}
			aw := &fakeAuditWriter{}
			r := vaultTestRunner(vc, aw, nil)
			resolve := r.newInputVaultResolver(context.Background(), ac(), nil)

			// scope ошибочно широкий (secret/*) — защита только на deny-list/ParseRef.
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

// TestInputVaultResolver_ConfigDenyExtends — config-расширение deny-list работает.
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

// TestInputVaultResolver_NilVault — фабрика с nil-Vault возвращает nil-резолвер
// (input-vault-refs не поддержаны).
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
