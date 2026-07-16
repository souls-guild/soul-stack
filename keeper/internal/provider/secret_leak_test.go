package provider

// Guard-leak tests for dual-mode credential ingestion (ADR-064 mitigation b,
// NIM-11): plaintext credentials do not leak to any sink (PG args / returned
// Provider / error text), are written only to Vault, and XOR/disabled are rejected.

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

// fakeCredVault is a SecretWriter fake that records the written credentials map.
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

// newCredService builds Service with fakeDB (Insert returns created_at).
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

// TestProviderCredentialsNoLeak verifies plaintext credentials -> Vault, only ref in PG.
func TestProviderCredentialsNoLeak(t *testing.T) {
	v := &fakeCredVault{}
	db := &fakeDB{}
	svc := newCredService(t, v, db, true)

	p, err := svc.Create(context.Background(), credInput())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Whole credentials map is written to Vault.
	if v.got == nil || v.got["secret_key"] != leakCred {
		t.Fatalf("vault got=%v, want plaintext creds", v.got)
	}
	// credentials_ref in PG is vault ref, not plaintext.
	if p.CredentialsRef == "" || strings.Contains(p.CredentialsRef, leakCred) {
		t.Fatalf("credentials_ref=%q must be vault-ref", p.CredentialsRef)
	}
	if !p.SecretWritten {
		t.Fatal("SecretWritten was not set")
	}
	// PG args (INSERT) do not contain plaintext.
	if blob := fmt.Sprintf("%v", db.queryRowArgs); strings.Contains(blob, leakCred) {
		t.Fatal("plaintext leaked into PG args")
	}
	// Returned Provider (source for View/wire) does not contain plaintext.
	if j, _ := json.Marshal(p); strings.Contains(string(j), leakCred) {
		t.Fatalf("plaintext leaked into Provider JSON: %s", j)
	}
}

// TestProviderRefModeUnchanged covers ref mode without writing to Vault.
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
		t.Fatalf("ref mode must not write to Vault, got=%v", v.got)
	}
	if p.SecretWritten {
		t.Fatal("SecretWritten must not be set in ref mode")
	}
}

// TestProviderXORRejected covers both credentials and credentials_ref set -> 422
// without Vault write.
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
		t.Fatalf("plaintext leaked into error text: %v", err)
	}
	if v.got != nil {
		t.Fatal("XOR rejection must not write to Vault")
	}
}

// TestProviderCredentialsRequired covers neither credentials nor ref set -> 422.
func TestProviderCredentialsRequired(t *testing.T) {
	v := &fakeCredVault{}
	db := &fakeDB{}
	svc := newCredService(t, v, db, true)

	_, err := svc.Create(context.Background(), CreateInput{Name: "aws-prod", Type: "aws", Region: "eu-west-1"})
	if err == nil || !IsValidationError(err) {
		t.Fatalf("neither: err=%v, want ErrValidation", err)
	}
}

// TestProviderPlaintextDisabled covers accept=false -> plaintext rejected
// (ADR-064 mitigation a).
func TestProviderPlaintextDisabled(t *testing.T) {
	v := &fakeCredVault{}
	db := &fakeDB{}
	svc := newCredService(t, v, db, false) // accept=false

	_, err := svc.Create(context.Background(), credInput())
	if err == nil || !errors.Is(err, ErrPlaintextDisabled) {
		t.Fatalf("disabled: err=%v, want ErrPlaintextDisabled", err)
	}
	if v.got != nil {
		t.Fatal("disabled mode must not write to Vault")
	}
}

// TestProviderVaultFailureNoLeak covers Vault failure -> error without plaintext
// and without INSERT.
func TestProviderVaultFailureNoLeak(t *testing.T) {
	v := &fakeCredVault{fail: true}
	db := &fakeDB{}
	svc := newCredService(t, v, db, true)

	_, err := svc.Create(context.Background(), credInput())
	if err == nil {
		t.Fatal("expected error on Vault failure")
	}
	if strings.Contains(err.Error(), leakCred) {
		t.Fatalf("plaintext leaked into Vault failure error text: %v", err)
	}
	if db.queryRowCalls != 0 {
		t.Fatal("Vault failure must not INSERT into PG")
	}
}
