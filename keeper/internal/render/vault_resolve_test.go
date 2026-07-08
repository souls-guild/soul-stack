package render

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// TestResolveVaultRefs_NoRefs — params без vault-refs проходят насквозь без
// обращения к Vault (PM-decision 2, no-op). vc=nil безопасен.
func TestResolveVaultRefs_NoRefs(t *testing.T) {
	params := map[string]any{
		"cmd":    "echo hello",
		"nested": map[string]any{"k": "v"},
		"list":   []any{"a", "b"},
		"num":    int64(7),
	}
	out, err := resolveVaultRefs(context.Background(), nil, params)
	if err != nil {
		t.Fatalf("resolveVaultRefs: %v", err)
	}
	if out["cmd"] != "echo hello" {
		t.Errorf("command = %v", out["cmd"])
	}
	if n := out["nested"].(map[string]any); n["k"] != "v" {
		t.Errorf("nested = %v", n)
	}
}

// TestResolveVaultRefs_Empty — пустые/nil params → no-op.
func TestResolveVaultRefs_Empty(t *testing.T) {
	out, err := resolveVaultRefs(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("resolveVaultRefs(nil): %v", err)
	}
	if out != nil {
		t.Errorf("out = %v, want nil", out)
	}
}

// TestReadVaultRef_InterpolationMarker — ${…} внутри vault-ref → ошибка
// (vault-ref должен быть статической строкой, граница фаз ADR-010).
func TestReadVaultRef_InterpolationMarker(t *testing.T) {
	_, err := readVaultRef(context.Background(), nil, "vault:secret/db/${ input.x }")
	if err == nil {
		t.Fatal("ожидалась ошибка ${…} в vault-ref")
	}
	if !strings.Contains(err.Error(), "статической строкой") {
		t.Errorf("err = %v", err)
	}
}

// TestReadVaultRef_NilClient — vault-ref при отсутствии Vault-client → ошибка.
func TestReadVaultRef_NilClient(t *testing.T) {
	_, err := readVaultRef(context.Background(), nil, "vault:secret/db/password")
	if err == nil {
		t.Fatal("ожидалась ошибка nil-client при наличии vault-ref")
	}
	if !strings.Contains(err.Error(), "не сконфигурирован") {
		t.Errorf("err = %v", err)
	}
}

// TestReadVaultRef_EmptyField — пустое имя поля после '#' → ошибка.
func TestReadVaultRef_EmptyField(t *testing.T) {
	_, err := readVaultRef(context.Background(), nil, "vault:secret/db/creds#")
	if err == nil {
		t.Fatal("ожидалась ошибка пустого поля после '#'")
	}
}

// TestResolveVaultRefs_RefDetectedInNested — ref в глубине структуры обнаружен
// (доходит до readVaultRef, который без client падает — подтверждает обход).
func TestResolveVaultRefs_RefDetectedInNested(t *testing.T) {
	params := map[string]any{
		"outer": map[string]any{
			"list": []any{"plain", "vault:secret/db/password"},
		},
	}
	_, err := resolveVaultRefs(context.Background(), nil, params)
	if err == nil {
		t.Fatal("ожидалась ошибка: ref во вложенной структуре должен дойти до Vault")
	}
}

// resolveVaultStubKV — KVReader с РЕАЛИСТИЧНЫМ not-found (как keeper/internal/
// vault: `vault: KV path not found: <plain path>`, путь БЕЗ vault:-префикса).
// Отдельный от pipelineStubKV, который искусственно префиксит `vault:` к пути
// (симуляция старого leak-а) и потому не годится для проверки actionable-текста.
type resolveVaultStubKV struct{ secrets map[string]map[string]any }

func (s resolveVaultStubKV) ReadKV(_ context.Context, path string) (map[string]any, error) {
	data, ok := s.secrets[path]
	if !ok {
		return nil, errors.New("vault: KV path not found: " + path)
	}
	return data, nil
}

// TestReadVaultRef_MissingSecretActionable (NIM-73): not-found vault-ref в params
// даёт actionable-ошибку — путь в ПЛОСКОЙ форме, переживает production-маскинг
// status_details/error_summary (симметрия с shared/cel.callVault).
func TestReadVaultRef_MissingSecretActionable(t *testing.T) {
	kv := resolveVaultStubKV{secrets: map[string]map[string]any{}}
	_, err := readVaultRef(context.Background(), kv, "vault:secret/redis/nosql/users/alice#password")
	if err == nil {
		t.Fatal("ожидали ошибку отсутствующего секрета")
	}
	assertResolveVaultActionable(t, err.Error(), "secret/redis/nosql/users/alice")
}

// TestReadVaultRef_MissingFieldActionable (NIM-73): секрет есть, поля нет →
// actionable путь+поле, переживает маскинг; значения других полей не утекают.
func TestReadVaultRef_MissingFieldActionable(t *testing.T) {
	kv := resolveVaultStubKV{secrets: map[string]map[string]any{
		"secret/redis/admin": {"password": "TOP-SECRET-VALUE"},
	}}
	_, err := readVaultRef(context.Background(), kv, "vault:secret/redis/admin#nope")
	if err == nil {
		t.Fatal("ожидали ошибку отсутствующего поля")
	}
	assertResolveVaultActionable(t, err.Error(), "secret/redis/admin")
	if !strings.Contains(err.Error(), "nope") {
		t.Fatalf("текст не называет отсутствующее поле nope: %q", err.Error())
	}
	if strings.Contains(err.Error(), "TOP-SECRET-VALUE") {
		t.Fatalf("значение другого поля секрета утекло: %q", err.Error())
	}
}

// assertResolveVaultActionable: текст ошибки vault-ref (а) несёт путь в ПЛОСКОЙ
// форме, (б) НЕ несёт vault:-ref-маркер, (в) переживает production-маскинг
// (audit.MaskSecretsSealed) — не `***MASKED***`, путь остаётся виден.
func assertResolveVaultActionable(t *testing.T, errText, path string) {
	t.Helper()
	if !strings.Contains(errText, path) {
		t.Fatalf("текст не несёт путь %q: %q", path, errText)
	}
	if strings.Contains(errText, "vault:"+path) {
		t.Fatalf("текст несёт vault:-ref-форму (маскинг съест целиком): %q", errText)
	}
	masked := audit.MaskSecretsSealed(map[string]any{"error": errText}, audit.SealOpts{})
	got, _ := masked["error"].(string)
	if got == "***MASKED***" {
		t.Fatalf("actionable-ошибка замаскирована целиком: %q", got)
	}
	if !strings.Contains(got, path) {
		t.Fatalf("путь %q пропал после маскинга: %q", path, got)
	}
}
