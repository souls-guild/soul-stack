package herald

// Guard-leak тесты dual-mode приёма секрета (ADR-064 митигация b, NIM-11):
// plaintext-секрет НЕ утекает ни в один sink (PG-args / возвращаемый View /
// текст ошибки), пишется ТОЛЬКО в Vault; XOR/disabled отвергаются.

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

// capturingVault — SecretWriter-фейк: запоминает записанные plaintext-значения.
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

// capturingPool — ExecQueryRower-фейк: запоминает args INSERT/UPDATE, отдаёт
// scannable-row для created_at/updated_at.
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

// tsRow сканит любые *time.Time (created_at/updated_at) успешно.
type tsRow struct{}

func (tsRow) Scan(dst ...any) error {
	for _, d := range dst {
		if p, ok := d.(*time.Time); ok {
			*p = time.Unix(0, 0)
		}
	}
	return nil
}

// argsBlob стрингифицирует все PG-args в один blob для leak-проверки. []byte
// (configBytes — marshaled JSONB) декодируется в строку: иначе `%v` печатает
// байты-числа и substring-скан пропустил бы plaintext в config.
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

// assertNoLeak — plaintext НЕ присутствует ни в PG-args, ни в JSON возвращаемого
// Herald-а (source для View/wire).
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

// TestWebhookSecretNoLeak — top-level webhook signing-secret (plaintext) → Vault,
// в PG только ref, plaintext нигде не персистится.
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

	// plaintext записан в Vault.
	if len(vault.writes) != 1 || vault.writes[0] != leakSigning {
		t.Fatalf("vault writes=%v, want [%q]", vault.writes, leakSigning)
	}
	// В PG secret_ref = vault:… , НЕ plaintext.
	if created.SecretRef == nil || *created.SecretRef == leakSigning {
		t.Fatalf("secret_ref=%v — должен быть vault-ref", created.SecretRef)
	}
	// Secret-поле стёрто, маркер записи взведён.
	if created.Secret != nil {
		t.Fatal("Secret plaintext не стёрт после материализации")
	}
	if !created.SecretWritten {
		t.Fatal("SecretWritten не взведён")
	}
	assertNoLeak(t, leakSigning, pool, created)
}

// TestChannelTokenNoLeak — config-секрет канала (telegram bot_token plaintext) →
// Vault, config[bot_token_ref]=ref, config[bot_token] удалён.
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
	// plaintext-поле удалено, ref-поле проставлено.
	if _, ok := created.Config["bot_token"]; ok {
		t.Fatal("config[bot_token] plaintext не удалён")
	}
	ref, ok := created.Config["bot_token_ref"].(string)
	if !ok || ref == leakToken {
		t.Fatalf("config[bot_token_ref]=%v — должен быть vault-ref", created.Config["bot_token_ref"])
	}
	if !created.SecretWritten {
		t.Fatal("SecretWritten не взведён")
	}
	assertNoLeak(t, leakToken, pool, created)
}

// TestSlackWebhookURLNoLeak — config-секрет `webhook_url` (slack) plaintext → Vault.
// ★Особый кейс: `webhook_url` НЕ содержит sensitive-фрагмента → MaskSecrets по
// имени ключа его НЕ маскирует, поэтому удаление из config (materialize) —
// ЕДИНСТВЕННАЯ защита. Проверяем, что plaintext не остаётся в config/PG/View.
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
		t.Fatal("config[webhook_url] plaintext не удалён (masking его не ловит!)")
	}
	if ref, _ := created.Config["webhook_url_ref"].(string); ref == "" || ref == leakURL {
		t.Fatalf("config[webhook_url_ref]=%v — должен быть vault-ref", created.Config["webhook_url_ref"])
	}
	assertNoLeak(t, leakURL, pool, created)
}

// TestUpdateMaterializesSecret — update-путь тоже материализует plaintext (idempotent-write).
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
		t.Fatalf("update не записал plaintext в Vault: %v", vault.writes)
	}
	if !updated.SecretWritten {
		t.Fatal("SecretWritten не переживает re-read на update")
	}
	assertNoLeak(t, leakSigning, pool, updated)
}

// TestSecretRefModeUnchanged — ref-режим (существующее поведение) без записи в Vault.
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
		t.Fatalf("ref-режим не должен писать в Vault, writes=%v", vault.writes)
	}
	if created.SecretWritten {
		t.Fatal("SecretWritten не должен быть взведён в ref-режиме")
	}
}

// TestXORRejected — заданы и secret, и secret_ref → 422, БЕЗ записи в Vault, БЕЗ
// plaintext в тексте ошибки.
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
		t.Fatalf("plaintext утёк в текст ошибки: %v", err)
	}
	if len(vault.writes) != 0 || len(pool.queryArgs) != 0 {
		t.Fatal("при XOR-отказе не должно быть записи в Vault/PG")
	}
}

// TestPlaintextDisabled — accept=false → plaintext отвергается (ADR-064 митигация a).
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
		t.Fatal("при disabled не должно быть записи в Vault/PG")
	}
}

// TestVaultFailureNoLeak — сбой Vault-записи → ошибка БЕЗ plaintext, БЕЗ записи в PG.
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
		t.Fatal("ожидалась ошибка при сбое Vault")
	}
	if strings.Contains(err.Error(), leakSigning) {
		t.Fatalf("plaintext утёк в текст ошибки Vault-сбоя: %v", err)
	}
	if len(pool.queryArgs) != 0 {
		t.Fatal("при сбое Vault не должно быть INSERT в PG")
	}
}
