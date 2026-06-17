package sigil

import (
	"context"
	"strings"
	"testing"
)

// Unit-тесты keys-CRUD-а покрывают input-guard-ы, отрабатывающие ДО обращения к
// БД (nil-pool гарантирует: guard сработал раньше QueryRow/BeginTx — иначе
// был бы nil-panic). Транзакционные инварианты (≥1 active, single primary,
// partial-unique) полноценно проверяются integration-тестами через
// testcontainers (keys_integration_test.go) — симметрично store_test.go vs
// store_integration_test.go.

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
				t.Fatalf("Introduce(%s) должен вернуть ошибку", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("ошибка = %q, ожидалось упоминание %q", err, tc.want)
			}
		})
	}
}

func TestSetPrimary_GuardEmptyKeyID(t *testing.T) {
	err := SetPrimary(context.Background(), nil, "", "archon-a")
	if err == nil {
		t.Fatal("SetPrimary с пустым key_id должен вернуть ошибку")
	}
	if !strings.Contains(err.Error(), "key_id") {
		t.Errorf("ошибка = %q, ожидалось упоминание key_id", err)
	}
}

func TestRetire_GuardEmptyFields(t *testing.T) {
	if err := Retire(context.Background(), nil, "", "archon-a"); err == nil {
		t.Fatal("Retire с пустым key_id должен вернуть ошибку")
	}
	// callerAID обязателен (audit-инвариант).
	err := Retire(context.Background(), nil, "kid", "")
	if err == nil {
		t.Fatal("Retire с пустым callerAID должен вернуть ошибку")
	}
	if !strings.Contains(err.Error(), "retired_by_aid") {
		t.Errorf("ошибка = %q, ожидалось упоминание retired_by_aid", err)
	}
}
