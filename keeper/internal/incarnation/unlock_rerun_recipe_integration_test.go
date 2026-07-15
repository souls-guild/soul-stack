//go:build integration

package incarnation

import (
	"context"
	"testing"
)

// TestIntegration_UnlockForRerun_Day2_ReusesRecipeInput — day-2 branch of rerun-last on
// REAL PG (not fakeTx-unit): last failed scenario ≠ created_scenario →
// UnlockForRerun recovers input from apply_runs.recipe, NOT from spec.input.
// Guard closes fixture/schema-drift class in recipe-probe (SELECT recipe FROM
// apply_runs), which unit-fakeTx doesn't catch (NIM-65).
func TestIntegration_UnlockForRerun_Day2_ReusesRecipeInput(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	const (
		name         = "redis-prod"
		failedAID    = "01HDAY20FAILED000000000001" // apply_id of failed day-2 (state_history + apply_runs)
		snapHistID   = "01HDAY20SNAP00000000000001"
		newApplyID   = "01HDAY20RERUN0000000000001" // apply_id of new rerun-run
		newHistoryID = "01HDAY20HIST00000000000001"
	)
	creator, created := "archon-alice", "create"
	// Incarnation created by `create`; version in spec.input must NOT leak (day-2
	// must take recipe.input of failed add_user).
	inc := &Incarnation{
		Name: name, Service: "redis", ServiceVersion: "v1.0.0",
		StateSchemaVersion: 1, Status: StatusErrorLocked,
		Spec:            map[string]any{"input": map[string]any{"version": "8.6.1"}},
		CreatedScenario: &created, CreatedByAID: &creator,
	}
	if err := Create(ctx, integrationPool, inc); err != nil {
		t.Fatalf("Create error_locked: %v", err)
	}
	// Last snapshot: failed day-2 add_user (≠ created `create`) + its apply_id.
	if _, err := integrationPool.Exec(ctx, `
INSERT INTO state_history (history_id, incarnation_name, scenario, state_before, state_after, apply_id)
VALUES ($1, $2, 'add_user', '{}'::jsonb, '{}'::jsonb, $3)`,
		snapHistID, name, failedAID); err != nil {
		t.Fatalf("seed state_history: %v", err)
	}
	// recipe of failed run — sole source of its input.
	if _, err := integrationPool.Exec(ctx, `
INSERT INTO apply_runs (apply_id, sid, incarnation_name, scenario, status, started_by_aid, recipe)
VALUES ($1, 'host-1', $2, 'add_user', 'failed', $3,
        '{"scenario_name":"add_user","input":{"user":"alice"}}'::jsonb)`,
		failedAID, name, creator); err != nil {
		t.Fatalf("seed apply_runs recipe: %v", err)
	}

	res, err := UnlockForRerun(ctx, integrationPool, name, "rerun add_user", creator, newHistoryID, newApplyID)
	if err != nil {
		t.Fatalf("UnlockForRerun day-2: %v", err)
	}
	if res.Scenario != "add_user" {
		t.Errorf("Scenario = %q, want add_user (last failed day-2)", res.Scenario)
	}
	if res.Input == nil || res.Input["user"] != "alice" {
		t.Errorf("Input = %v, want {user:alice} from recipe (day-2 takes recipe.input, not spec)", res.Input)
	}
	if _, leaked := res.Input["version"]; leaked {
		t.Error("Input carries spec.input[version] — day-2 must take recipe.input, not spec")
	}
	if res.FromUpgrade {
		t.Error("FromUpgrade = true, want false (recipe without from_upgrade)")
	}
	if res.PreviousStatus != StatusErrorLocked {
		t.Errorf("PreviousStatus = %q, want error_locked", res.PreviousStatus)
	}

	got, err := SelectByName(ctx, integrationPool, name)
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if got.Status != StatusApplying {
		t.Errorf("status = %q, want applying (rerun bypassing ready)", got.Status)
	}
}
