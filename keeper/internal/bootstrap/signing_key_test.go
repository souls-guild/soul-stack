package bootstrap

import (
	"context"
	"errors"
	"strings"
	"testing"

	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
)

// LoadSigningKey unit tests cover input validation (vc/ref) without
// requiring a real Vault. The happy path + bad-payload scenarios (ReadKV
// roundtrip, ErrSigningKeyMissing) live in integration_test.go under
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
	// A non-nil vault client cannot be constructed without a real Addr;
	// to test the empty-ref case we'd pass nil — but LoadSigningKey checks
	// vc first, then ref. To exercise the empty-ref branch we need a
	// non-nil vc placeholder. A zero-value *keepervault.Client is fine —
	// nothing touches it before the ref check.
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
