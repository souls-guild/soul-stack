package render

import (
	"context"
	"strings"
	"testing"
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
