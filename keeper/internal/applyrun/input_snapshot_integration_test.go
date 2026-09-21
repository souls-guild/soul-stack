//go:build integration

package applyrun

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// TestIntegration_ApplyRunInputSnapshot verifies the masked operator-input snapshot
// (migration 101) at the store boundary: audit.MaskSecrets (the write-path masker) ->
// Insert into apply_runs.input -> SelectRunDetail reads it back masked (the exact read
// projection the runs-detail API uses). The snapshot is run-invariant — written
// identically on every host row, the read takes the first non-null.
func TestIntegration_ApplyRunInputSnapshot(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()
	aid := "archon-alice"

	// Raw operator input with an inline secret, a vault-ref, and a nested secret,
	// masked by the same write-path masker scenario.maskedInputSnapshot uses.
	raw := map[string]any{
		"version":     "7.2",
		"db_password": "s3cr3t",
		"redis_ref":   "vault:secret/redis#pass",
		"users":       []any{map[string]any{"name": "alice", "password": "p1"}},
	}
	snap, err := json.Marshal(audit.MaskSecrets(raw))
	if err != nil {
		t.Fatalf("marshal masked input: %v", err)
	}

	for _, sid := range []string{"host-a", "host-b"} {
		run := &ApplyRun{
			ApplyID: "01HINPUTSNAP", SID: sid, IncarnationName: "redis-prod",
			Scenario: "create", Status: StatusRunning, StartedByAID: &aid,
			Input: json.RawMessage(snap),
		}
		if err := Insert(ctx, integrationPool, run); err != nil {
			t.Fatalf("Insert(%s): %v", sid, err)
		}
	}

	d, err := SelectRunDetail(ctx, integrationPool, "01HINPUTSNAP", "redis-prod")
	if err != nil {
		t.Fatalf("SelectRunDetail: %v", err)
	}
	if d.Input == nil {
		t.Fatalf("RunDetail.Input nil, want masked snapshot")
	}

	const masked = "***MASKED***"
	if d.Input["version"] != "7.2" {
		t.Errorf("input.version = %v, want 7.2 (non-secret intact)", d.Input["version"])
	}
	if d.Input["db_password"] != masked {
		t.Errorf("input.db_password = %v, want %s", d.Input["db_password"], masked)
	}
	if d.Input["redis_ref"] != masked {
		t.Errorf("input.redis_ref (vault-ref) = %v, want %s", d.Input["redis_ref"], masked)
	}
	users, ok := d.Input["users"].([]any)
	if !ok || len(users) != 1 {
		t.Fatalf("input.users = %v, want 1-element slice", d.Input["users"])
	}
	u := users[0].(map[string]any)
	if u["name"] != "alice" {
		t.Errorf("input.users[0].name = %v, want alice (nested non-secret intact)", u["name"])
	}
	if u["password"] != masked {
		t.Errorf("input.users[0].password = %v, want %s (nested secret masked)", u["password"], masked)
	}
}
