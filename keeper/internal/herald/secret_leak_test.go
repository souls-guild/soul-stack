package herald

// Guard-leak tests dual-mode secret handling (ADR-064 mitigation b, NIM-11):
// plaintext secret must not leak into any sink (PG args / returned View /
// error text), written only to Vault; XOR/disabled modes are rejected.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	leakSigning = "PLAINTEXT-SIGNING-SECRET-9f3a2b"
	leakToken   = "PLAINTEXT-BOT-TOKEN-7c2b1d"
)

// capturingVault mock SecretWriter that records plaintext values written.
type capturingVault struct {
	writes []string
	fail   bool
}

func (c *capturingVault) WriteString(_ context.Context, domain, entity, field, value string) (string, error) {
	if c.fail {
		return "", errors.New("vault down")
	}
	c.writes = append(c.writes, value)
	return "vault:secret/" + domain + "/" + entity + "/" + field + "#" + field, nil
}

// capturingPool mock ExecQueryRower that records INSERT/UPDATE args and
// returns scannable rows for created_at/updated_at.
type capturingPool struct {
	queryArgs [][]any
	execArgs  [][]any
}

func (p *capturingPool) Exec(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
	p.execArgs = append(p.execArgs, args)
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (p *capturingPool) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	p.queryArgs = append(p.queryArgs, args)
	return tsRow{}
}

func (p *capturingPool) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, errors.New("herald secret_leak_test: Query not stubbed")
}

// tsRow successfully scans any *time.Time (created_at/updated_at).
type tsRow struct{}

func (tsRow) Scan(dst ...any) error {
	for _, d := range dst {
		if p, ok := d.(*time.Time); ok {
			*p = time.Unix(0, 0)
		}
	}
	return nil
}

// argsBlob stringifies all PG args into a single blob for leak checking.
// []byte (configBytes = marshaled JSONB) is decoded as string; otherwise %v
// prints byte numbers and substring scan would miss plaintext in config.
func argsBlob(rows [][]any) string {
	var b string
	for _, r := range rows {
		for _, a := range r {
			if bs, ok := a.([]byte); ok {
				b += string(bs) + "|"
				continue
			}
			b += fmt.Sprintf("%v|", a)
		}
	}
	return b
}

// assertNoLeak verifies plaintext is absent from both PG args and JSON
// returned by Herald (source for View/wire).
func assertNoLeak(t *testing.T, plaintext string, pool *capturingPool, h *Herald) {
	t.Helper()
	blob := argsBlob(pool.queryArgs) + argsBlob(pool.execArgs)
	if strings.Contains(blob, plaintext) {
		t.Fatalf("plaintext leaked into PG args")
	}
	viewJSON, _ := json.Marshal(h)
	if strings.Contains(string(viewJSON), plaintext) {
		t.Fatalf("plaintext leaked into returned Herald JSON: %s", viewJSON)
	}
}

func newLeakService(t *testing.T, vault SecretWriter, pool *capturingPool, accept bool) *Service {
	t.Helper()
	svc, err := NewService(ServiceDeps{Pool: pool, SecretWriter: vault, AcceptPlaintext: accept})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

// TestWebhookSecretNoLeak verifies top-level webhook signing secret (plaintext)
// goes to Vault only; PG stores only ref, plaintext persists nowhere.
func TestWebhookSecretNoLeak(t *testing.T) {
	vault := &capturingVault{}
	pool := &capturingPool{}
	svc := newLeakService(t, vault, pool, true)

	h := &Herald{
		Name:   "ops-hook",
		Type:   HeraldWebhook,
		Config: map[string]any{"url": "https://example.com/hook"},
		Secret: strptr(leakSigning),
	}
	created, err := svc.CreateHerald(context.Background(), h)
	if err != nil {
		t.Fatalf("CreateHerald: %v", err)
	}

	// Plaintext written to Vault.
	if len(vault.writes) != 1 || vault.writes[0] != leakSigning {
		t.Fatalf("vault writes=%v, want [%q]", vault.writes, leakSigning)
	}
	// PG secret_ref = vault:…, not plaintext.
	if created.SecretRef == nil || *created.SecretRef == leakSigning {
		t.Fatalf("secret_ref=%v must be vault-ref", created.SecretRef)
	}
	// Secret field cleared, write marker set.
	if created.Secret != nil {
		t.Fatal("Secret plaintext not cleared after materialization")
	}
	if !created.SecretWritten {
		t.Fatal("SecretWritten was not set")
	}
	assertNoLeak(t, leakSigning, pool, created)
}

// TestChannelTokenNoLeak verifies channel config secret (telegram bot_token plaintext)
// goes to Vault; config[bot_token_ref]=ref, config[bot_token] is removed.
func TestChannelTokenNoLeak(t *testing.T) {
	vault := &capturingVault{}
	pool := &capturingPool{}
	svc := newLeakService(t, vault, pool, true)

	h := &Herald{
		Name: "tg-alerts",
		Type: HeraldTelegram,
		Config: map[string]any{
			"chat_id":   "123456",
			"bot_token": leakToken,
		},
	}
	created, err := svc.CreateHerald(context.Background(), h)
	if err != nil {
		t.Fatalf("CreateHerald: %v", err)
	}

	if len(vault.writes) != 1 || vault.writes[0] != leakToken {
		t.Fatalf("vault writes=%v, want [%q]", vault.writes, leakToken)
	}
	// Plaintext field removed, ref field set.
	if _, ok := created.Config["bot_token"]; ok {
		t.Fatal("config[bot_token] plaintext not removed")
	}
	ref, ok := created.Config["bot_token_ref"].(string)
	if !ok || ref == leakToken {
		t.Fatalf("config[bot_token_ref]=%v must be vault-ref", created.Config["bot_token_ref"])
	}
	if !created.SecretWritten {
		t.Fatal("SecretWritten was not set")
	}
	assertNoLeak(t, leakToken, pool, created)
}

// TestSlackWebhookURLNoLeak verifies config secret `webhook_url` (slack plaintext)
// goes to Vault. Note: `webhook_url` lacks a sensitive fragment, so MaskSecrets
// does not mask it by key name — removal from config (materialize) is the only
// protection. Verifies plaintext does not remain in config/PG/View.
func TestSlackWebhookURLNoLeak(t *testing.T) {
	vault := &capturingVault{}
	pool := &capturingPool{}
	svc := newLeakService(t, vault, pool, true)

	const leakURL = "https://hooks.slack.com/services/T00/B00/PLAINTEXT-SLACK-4e5f6a"
	h := &Herald{
		Name:   "slack-alerts",
		Type:   HeraldSlack,
		Config: map[string]any{"webhook_url": leakURL},
	}
	created, err := svc.CreateHerald(context.Background(), h)
	if err != nil {
		t.Fatalf("CreateHerald: %v", err)
	}
	if len(vault.writes) != 1 || vault.writes[0] != leakURL {
		t.Fatalf("vault writes=%v, want [%q]", vault.writes, leakURL)
	}
	if _, ok := created.Config["webhook_url"]; ok {
		t.Fatal("config[webhook_url] plaintext not removed (masking does not catch it!)")
	}
	if ref, _ := created.Config["webhook_url_ref"].(string); ref == "" || ref == leakURL {
		t.Fatalf("config[webhook_url_ref]=%v must be vault-ref", created.Config["webhook_url_ref"])
	}
	assertNoLeak(t, leakURL, pool, created)
}

// TestUpdateMaterializesSecret verifies update path also materializes plaintext (idempotent-write).
func TestUpdateMaterializesSecret(t *testing.T) {
	vault := &capturingVault{}
	pool := &capturingPool{}
	svc := newLeakService(t, vault, pool, true)

	h := &Herald{
		Name:   "ops-hook",
		Type:   HeraldWebhook,
		Config: map[string]any{"url": "https://example.com/hook"},
		Secret: strptr(leakSigning),
	}
	updated, err := svc.UpdateHerald(context.Background(), h)
	if err != nil {
		t.Fatalf("UpdateHerald: %v", err)
	}
	if len(vault.writes) != 1 || vault.writes[0] != leakSigning {
		t.Fatalf("update did not write plaintext to Vault: %v", vault.writes)
	}
	if !updated.SecretWritten {
		t.Fatal("SecretWritten does not survive re-read on update")
	}
	assertNoLeak(t, leakSigning, pool, updated)
}

// TestSecretRefModeUnchanged verifies ref-mode (existing behavior) without Vault write.
func TestSecretRefModeUnchanged(t *testing.T) {
	vault := &capturingVault{}
	pool := &capturingPool{}
	svc := newLeakService(t, vault, pool, true)

	h := &Herald{
		Name:      "ops-hook",
		Type:      HeraldWebhook,
		Config:    map[string]any{"url": "https://example.com/hook"},
		SecretRef: strptr("vault:secret/ops/webhook#sig"),
	}
	created, err := svc.CreateHerald(context.Background(), h)
	if err != nil {
		t.Fatalf("CreateHerald: %v", err)
	}
	if len(vault.writes) != 0 {
		t.Fatalf("ref-mode must not write to Vault, writes=%v", vault.writes)
	}
	if created.SecretWritten {
		t.Fatal("SecretWritten must not be set in ref-mode")
	}
}

// TestXORRejected verifies when both secret and secret_ref are set → 422 without
// Vault write and without plaintext in error text.
func TestXORRejected(t *testing.T) {
	vault := &capturingVault{}
	pool := &capturingPool{}
	svc := newLeakService(t, vault, pool, true)

	h := &Herald{
		Name:      "ops-hook",
		Type:      HeraldWebhook,
		Config:    map[string]any{"url": "https://example.com/hook"},
		Secret:    strptr(leakSigning),
		SecretRef: strptr("vault:secret/ops/webhook#sig"),
	}
	_, err := svc.CreateHerald(context.Background(), h)
	if err == nil || !IsValidationError(err) {
		t.Fatalf("XOR: err=%v, want ErrValidation", err)
	}
	if strings.Contains(err.Error(), leakSigning) {
		t.Fatalf("plaintext leaked into error text: %v", err)
	}
	if len(vault.writes) != 0 || len(pool.queryArgs) != 0 {
		t.Fatal("XOR rejection must not write to Vault/PG")
	}
}

// TestPlaintextDisabled verifies plaintext is rejected when accept=false (ADR-064 mitigation a).
func TestPlaintextDisabled(t *testing.T) {
	vault := &capturingVault{}
	pool := &capturingPool{}
	svc := newLeakService(t, vault, pool, false) // accept=false

	h := &Herald{
		Name:   "ops-hook",
		Type:   HeraldWebhook,
		Config: map[string]any{"url": "https://example.com/hook"},
		Secret: strptr(leakSigning),
	}
	_, err := svc.CreateHerald(context.Background(), h)
	if err == nil || !errors.Is(err, ErrPlaintextDisabled) {
		t.Fatalf("disabled: err=%v, want ErrPlaintextDisabled", err)
	}
	if len(vault.writes) != 0 || len(pool.queryArgs) != 0 {
		t.Fatal("disabled mode must not write to Vault/PG")
	}
}

// TestVaultFailureNoLeak verifies Vault write failure returns error without plaintext
// and without PG write.
func TestVaultFailureNoLeak(t *testing.T) {
	vault := &capturingVault{fail: true}
	pool := &capturingPool{}
	svc := newLeakService(t, vault, pool, true)

	h := &Herald{
		Name:   "ops-hook",
		Type:   HeraldWebhook,
		Config: map[string]any{"url": "https://example.com/hook"},
		Secret: strptr(leakSigning),
	}
	_, err := svc.CreateHerald(context.Background(), h)
	if err == nil {
		t.Fatal("expected error on Vault failure")
	}
	if strings.Contains(err.Error(), leakSigning) {
		t.Fatalf("plaintext leaked into Vault failure error text: %v", err)
	}
	if len(pool.queryArgs) != 0 {
		t.Fatal("Vault failure must not INSERT into PG")
	}
}
