package provider

// Guard-leak тесты dual-mode приёма credentials (ADR-064 митигация b, NIM-11):
// plaintext-credentials НЕ утекают ни в один sink (PG-args / возвращаемый
// Provider / текст ошибки), пишутся ТОЛЬКО в Vault; XOR/disabled отвергаются.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const leakCred = "PLAINTEXT-AWS-SECRET-KEY-4d9f2c"

// fakeCredVault — SecretWriter-фейк: запоминает записанный credentials-map.
type fakeCredVault struct {
	got  map[string]any
	fail bool
}

func (v *fakeCredVault) WriteMap(_ context.Context, domain, entity, field string, data map[string]any) (string, error) {
	if v.fail {
		return "", errors.New("vault down")
	}
	v.got = data
	return "vault:secret/" + domain + "/" + entity + "/" + field, nil
}

// newCredService собирает Service с fakeDB (Insert возвращает created_at).
func newCredService(t *testing.T, v SecretWriter, db *fakeDB, accept bool) *Service {
	t.Helper()
	db.queryRowFunc = func(string) pgx.Row { return staticRow{values: []any{time.Unix(0, 0)}} }
	svc, err := NewService(ServiceDeps{Pool: db, SecretWriter: v, AcceptPlaintext: accept})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func credInput() CreateInput {
	return CreateInput{
		Name:        "aws-prod",
		Type:        "aws",
		Region:      "eu-west-1",
		Credentials: map[string]any{"access_key": "AKIA0000", "secret_key": leakCred},
	}
}

// TestProviderCredentialsNoLeak — plaintext credentials → Vault, в PG только ref.
func TestProviderCredentialsNoLeak(t *testing.T) {
	v := &fakeCredVault{}
	db := &fakeDB{}
	svc := newCredService(t, v, db, true)

	p, err := svc.Create(context.Background(), credInput())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Весь credentials-map записан в Vault.
	if v.got == nil || v.got["secret_key"] != leakCred {
		t.Fatalf("vault got=%v, want plaintext creds", v.got)
	}
	// credentials_ref в PG = vault:… , НЕ plaintext.
	if p.CredentialsRef == "" || strings.Contains(p.CredentialsRef, leakCred) {
		t.Fatalf("credentials_ref=%q — должен быть vault-ref", p.CredentialsRef)
	}
	if !p.SecretWritten {
		t.Fatal("SecretWritten не взведён")
	}
	// PG-args (INSERT) НЕ содержат plaintext.
	if blob := fmt.Sprintf("%v", db.queryRowArgs); strings.Contains(blob, leakCred) {
		t.Fatal("plaintext утёк в PG args")
	}
	// Возвращаемый Provider (source для View/wire) НЕ содержит plaintext.
	if j, _ := json.Marshal(p); strings.Contains(string(j), leakCred) {
		t.Fatalf("plaintext утёк в JSON Provider-а: %s", j)
	}
}

// TestProviderRefModeUnchanged — ref-режим без записи в Vault.
func TestProviderRefModeUnchanged(t *testing.T) {
	v := &fakeCredVault{}
	db := &fakeDB{}
	svc := newCredService(t, v, db, true)

	p, err := svc.Create(context.Background(), CreateInput{
		Name: "aws-prod", Type: "aws", Region: "eu-west-1",
		CredentialsRef: "vault:secret/keeper/providers/aws",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if v.got != nil {
		t.Fatalf("ref-режим не должен писать в Vault, got=%v", v.got)
	}
	if p.SecretWritten {
		t.Fatal("SecretWritten не должен быть взведён в ref-режиме")
	}
}

// TestProviderXORRejected — заданы и credentials, и credentials_ref → 422 без Vault-записи.
func TestProviderXORRejected(t *testing.T) {
	v := &fakeCredVault{}
	db := &fakeDB{}
	svc := newCredService(t, v, db, true)

	in := credInput()
	in.CredentialsRef = "vault:secret/keeper/providers/aws"
	_, err := svc.Create(context.Background(), in)
	if err == nil || !IsValidationError(err) {
		t.Fatalf("XOR: err=%v, want ErrValidation", err)
	}
	if strings.Contains(err.Error(), leakCred) {
		t.Fatalf("plaintext утёк в текст ошибки: %v", err)
	}
	if v.got != nil {
		t.Fatal("при XOR-отказе не должно быть записи в Vault")
	}
}

// TestProviderCredentialsRequired — не задано ни credentials, ни ref → 422.
func TestProviderCredentialsRequired(t *testing.T) {
	v := &fakeCredVault{}
	db := &fakeDB{}
	svc := newCredService(t, v, db, true)

	_, err := svc.Create(context.Background(), CreateInput{Name: "aws-prod", Type: "aws", Region: "eu-west-1"})
	if err == nil || !IsValidationError(err) {
		t.Fatalf("neither: err=%v, want ErrValidation", err)
	}
}

// TestProviderPlaintextDisabled — accept=false → plaintext отвергается (ADR-064 митигация a).
func TestProviderPlaintextDisabled(t *testing.T) {
	v := &fakeCredVault{}
	db := &fakeDB{}
	svc := newCredService(t, v, db, false) // accept=false

	_, err := svc.Create(context.Background(), credInput())
	if err == nil || !errors.Is(err, ErrPlaintextDisabled) {
		t.Fatalf("disabled: err=%v, want ErrPlaintextDisabled", err)
	}
	if v.got != nil {
		t.Fatal("при disabled не должно быть записи в Vault")
	}
}

// TestProviderVaultFailureNoLeak — сбой Vault → ошибка БЕЗ plaintext, БЕЗ INSERT.
func TestProviderVaultFailureNoLeak(t *testing.T) {
	v := &fakeCredVault{fail: true}
	db := &fakeDB{}
	svc := newCredService(t, v, db, true)

	_, err := svc.Create(context.Background(), credInput())
	if err == nil {
		t.Fatal("ожидалась ошибка при сбое Vault")
	}
	if strings.Contains(err.Error(), leakCred) {
		t.Fatalf("plaintext утёк в текст ошибки Vault-сбоя: %v", err)
	}
	if db.queryRowCalls != 0 {
		t.Fatal("при сбое Vault не должно быть INSERT в PG")
	}
}
