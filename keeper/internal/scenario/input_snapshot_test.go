package scenario

import (
	"encoding/json"
	"testing"
)

// TestMaskedInputSnapshot: the run-history input snapshot masks secrets (by-name
// keys + vault-ref values -> ***MASKED***) while non-secret fields, including
// nested ones, stay intact; empty input -> nil (NULL column).
func TestMaskedInputSnapshot(t *testing.T) {
	if got := maskedInputSnapshot(nil); got != nil {
		t.Errorf("nil input -> %s, want nil", got)
	}
	if got := maskedInputSnapshot(map[string]any{}); got != nil {
		t.Errorf("empty input -> %s, want nil", got)
	}

	in := map[string]any{
		"version":     "7.2",
		"db_password": "s3cr3t",                  // sensitive-by-name -> masked
		"redis_ref":   "vault:secret/redis#pass", // vault-ref value -> masked
		"replicas":    3,
		"users": []any{
			map[string]any{"name": "alice", "password": "p1"},
		},
	}
	raw := maskedInputSnapshot(in)
	if raw == nil {
		t.Fatal("snapshot nil, want masked json")
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}

	const masked = "***MASKED***"
	if out["version"] != "7.2" {
		t.Errorf("version = %v, want 7.2 (non-secret intact)", out["version"])
	}
	if out["replicas"] != float64(3) {
		t.Errorf("replicas = %v, want 3 (non-secret intact)", out["replicas"])
	}
	if out["db_password"] != masked {
		t.Errorf("db_password = %v, want %s", out["db_password"], masked)
	}
	if out["redis_ref"] != masked {
		t.Errorf("redis_ref (vault-ref) = %v, want %s", out["redis_ref"], masked)
	}
	users, ok := out["users"].([]any)
	if !ok || len(users) != 1 {
		t.Fatalf("users = %v, want 1-element slice", out["users"])
	}
	u := users[0].(map[string]any)
	if u["name"] != "alice" {
		t.Errorf("users[0].name = %v, want alice (nested non-secret intact)", u["name"])
	}
	if u["password"] != masked {
		t.Errorf("users[0].password = %v, want %s (nested secret masked)", u["password"], masked)
	}

	// MaskSecrets copies — the original input is not mutated.
	if in["db_password"] != "s3cr3t" {
		t.Errorf("original input mutated: db_password = %v", in["db_password"])
	}
}
