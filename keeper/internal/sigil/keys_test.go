package sigil

import (
	"context"
	"strings"
	"testing"
)

// Unit tests for keys-CRUD cover input guards that run BEFORE database access
// (nil-pool guarantees: guard fired before QueryRow/BeginTx — else nil-panic).
// Transactional invariants (≥1 active, single primary, partial-unique) are
// fully verified by integration tests via testcontainers (keys_integration_test.go)
// — symmetrical to store_test.go vs store_integration_test.go.

func TestIntroduce_GuardEmptyFields(t *testing.T) {
	cases := []struct {
		name      string
		keyID     string
		pubkeyPEM string
		vaultRef  string
		want      string
	}{
		{"empty key_id", "", "PEM", "vault:kv/x", "key_id"},
		{"empty pubkey_pem", "kid", "", "vault:kv/x", "pubkey_pem"},
		{"empty vault_ref", "kid", "PEM", "", "vault_ref"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Introduce(context.Background(), nil, tc.keyID, tc.pubkeyPEM, tc.vaultRef, false, nil)
			if err == nil {
				t.Fatalf("Introduce(%s) should return error", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, expected mention of %q", err, tc.want)
			}
		})
	}
}

func TestSetPrimary_GuardEmptyKeyID(t *testing.T) {
	err := SetPrimary(context.Background(), nil, "", "archon-a")
	if err == nil {
		t.Fatal("SetPrimary with empty key_id should return error")
	}
	if !strings.Contains(err.Error(), "key_id") {
		t.Errorf("error = %q, expected mention of key_id", err)
	}
}

func TestRetire_GuardEmptyFields(t *testing.T) {
	if err := Retire(context.Background(), nil, "", "archon-a"); err == nil {
		t.Fatal("Retire with empty key_id should return error")
	}
	// callerAID is required (audit invariant).
	err := Retire(context.Background(), nil, "kid", "")
	if err == nil {
		t.Fatal("Retire with empty callerAID should return error")
	}
	if !strings.Contains(err.Error(), "retired_by_aid") {
		t.Errorf("error = %q, expected mention of retired_by_aid", err)
	}
}
