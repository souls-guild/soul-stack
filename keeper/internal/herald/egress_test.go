package herald

import (
	"context"
	"strings"
	"testing"
)

func TestValidateDeliveryEndpoint(t *testing.T) {
	cases := []struct {
		name         string
		url          string
		httpAllowed  bool
		allowPrivate bool
		wantErr      bool
	}{
		{"https public", "https://hooks.example.com/x", false, false, false},
		{"http denied by default", "http://hooks.example.com/x", false, false, true},
		{"http allowed opt-out", "http://hooks.example.com/x", true, false, false},
		{"literal private IP denied", "https://10.0.0.5/x", false, false, true},
		{"literal loopback denied", "https://127.0.0.1/x", false, false, true},
		{"metadata IP denied", "https://169.254.169.254/x", false, false, true},
		// allow_private снимает dial-guard → literal private проходит pre-валидацию
		// (фактический dial-guard выключен в guardedDeliveryClient).
		{"literal private IP allowed by opt-out", "https://10.0.0.5/x", false, true, false},
		{"no host", "https://", false, false, true},
		{"bad scheme", "ftp://host/x", false, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateDeliveryEndpoint(c.url, c.httpAllowed, c.allowPrivate)
			if (err != nil) != c.wantErr {
				t.Fatalf("validateDeliveryEndpoint(%q, http=%v, priv=%v) err=%v, wantErr=%v",
					c.url, c.httpAllowed, c.allowPrivate, err, c.wantErr)
			}
		})
	}
}

// TestResolveSigningKey_FieldSelection — выбор поля signing-token из секрета.
func TestResolveSigningKey_FieldSelection(t *testing.T) {
	t.Run("explicit #field", func(t *testing.T) {
		kv := stubKV{data: map[string]any{"token": "abc", "other": "x"}}
		key, err := resolveSigningKey(t.Context(), kv, "vault:secret/keeper/sign#token")
		if err != nil {
			t.Fatalf("resolveSigningKey: %v", err)
		}
		if string(key) != "abc" {
			t.Fatalf("key = %q, want abc", key)
		}
	})
	t.Run("single field default", func(t *testing.T) {
		kv := stubKV{data: map[string]any{"only": "s3cr3t"}}
		key, err := resolveSigningKey(t.Context(), kv, "vault:secret/keeper/sign")
		if err != nil {
			t.Fatalf("resolveSigningKey: %v", err)
		}
		if string(key) != "s3cr3t" {
			t.Fatalf("key = %q, want s3cr3t", key)
		}
	})
	t.Run("ambiguous multi-field without #field", func(t *testing.T) {
		kv := stubKV{data: map[string]any{"a": "1", "b": "2"}}
		_, err := resolveSigningKey(t.Context(), kv, "vault:secret/keeper/sign")
		if err == nil {
			t.Fatal("expected error for ambiguous secret without #field")
		}
	})
	t.Run("missing field", func(t *testing.T) {
		kv := stubKV{data: map[string]any{"token": "x"}}
		_, err := resolveSigningKey(t.Context(), kv, "vault:secret/keeper/sign#nope")
		if err == nil || !strings.Contains(err.Error(), "no field") {
			t.Fatalf("expected missing-field error, got %v", err)
		}
	})
}

// stubKV — KVReader, возвращающий фиксированный секрет.
type stubKV struct {
	data map[string]any
}

func (s stubKV) ReadKV(_ context.Context, _ string) (map[string]any, error) {
	return s.data, nil
}
