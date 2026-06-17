package bootstrap

import (
	"context"
	"errors"
	"strings"
	"testing"

	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
)

// Unit-тесты LoadSigningKey покрывают входную валидацию (vc/ref), не
// требуя реального Vault. Happy path + bad-payload-сценарии (ReadKV
// roundtrip, ErrSigningKeyMissing) живут в integration_test.go под
// `//go:build integration`.

func TestLoadSigningKey_NilVault(t *testing.T) {
	_, err := LoadSigningKey(context.Background(), nil, "vault:secret/keeper/jwt-signing-key")
	if err == nil {
		t.Fatal("LoadSigningKey: expected error for nil vault client, got nil")
	}
	if !strings.Contains(err.Error(), "vault client is nil") {
		t.Errorf("err = %v, want substring \"vault client is nil\"", err)
	}
}

func TestLoadSigningKey_EmptyRef(t *testing.T) {
	// Не-nil vault — конструировать нельзя без реального Addr; для
	// проверки empty-ref передаём nil — но порядок проверок в
	// LoadSigningKey: сначала vc, потом ref. Чтобы покрыть empty-ref-
	// ветку, нужен non-nil vc-плейсхолдер. zero-value *keepervault.Client
	// корректен — обращений к нему до проверки ref нет.
	vc := &keepervault.Client{}
	_, err := LoadSigningKey(context.Background(), vc, "")
	if err == nil {
		t.Fatal("LoadSigningKey: expected error for empty ref, got nil")
	}
	if !strings.Contains(err.Error(), "signing_key_ref is empty") {
		t.Errorf("err = %v, want substring \"signing_key_ref is empty\"", err)
	}
}

func TestLoadSigningKey_InvalidRefFormat(t *testing.T) {
	vc := &keepervault.Client{}
	_, err := LoadSigningKey(context.Background(), vc, "not-a-vault-ref")
	if err == nil {
		t.Fatal("LoadSigningKey: expected error for invalid ref, got nil")
	}
	if !errors.Is(err, keepervault.ErrInvalidVaultRef) {
		t.Errorf("err = %v, want errors.Is keepervault.ErrInvalidVaultRef", err)
	}
}
