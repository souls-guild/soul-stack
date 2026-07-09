//go:build integration

package incarnation

import (
	"context"
	"testing"
)

// TestIntegration_UnlockForRerun_Ops_ReusesRecipeInput — операционная ветка rerun-last на
// РЕАЛЬНОМ PG (не fakeTx-unit): последний упавший сценарий ≠ created_scenario →
// UnlockForRerun восстанавливает input из apply_runs.recipe, НЕ из spec.input.
// Гард закрывает класс fixture/schema-drift в recipe-пробе (SELECT recipe FROM
// apply_runs), который unit-fakeTx не ловит (NIM-65).
func TestIntegration_UnlockForRerun_Ops_ReusesRecipeInput(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	const (
		name         = "redis-prod"
		failedAID    = "01HDAY20FAILED000000000001" // apply_id упавшего операционного прогона (state_history + apply_runs)
		snapHistID   = "01HDAY20SNAP00000000000001"
		newApplyID   = "01HDAY20RERUN0000000000001" // apply_id нового rerun-прогона
		newHistoryID = "01HDAY20HIST00000000000001"
	)
	creator, created := "archon-alice", "create"
	// Инкарнация создана `create`; version в spec.input НЕ должен просочиться (операционный путь
	// обязан брать recipe.input упавшего add_user).
	inc := &Incarnation{
		Name: name, Service: "redis", ServiceVersion: "v1.0.0",
		StateSchemaVersion: 1, Status: StatusErrorLocked,
		Spec:            map[string]any{"input": map[string]any{"version": "8.6.1"}},
		CreatedScenario: &created, CreatedByAID: &creator,
	}
	if err := Create(ctx, integrationPool, inc); err != nil {
		t.Fatalf("Create error_locked: %v", err)
	}
	// Последний snapshot: упавший операционный add_user (≠ created `create`) + его apply_id.
	if _, err := integrationPool.Exec(ctx, `
INSERT INTO state_history (history_id, incarnation_name, scenario, state_before, state_after, apply_id)
VALUES ($1, $2, 'add_user', '{}'::jsonb, '{}'::jsonb, $3)`,
		snapHistID, name, failedAID); err != nil {
		t.Fatalf("seed state_history: %v", err)
	}
	// recipe упавшего прогона — единственный источник его input.
	if _, err := integrationPool.Exec(ctx, `
INSERT INTO apply_runs (apply_id, sid, incarnation_name, scenario, status, started_by_aid, recipe)
VALUES ($1, 'host-1', $2, 'add_user', 'failed', $3,
        '{"scenario_name":"add_user","input":{"user":"alice"}}'::jsonb)`,
		failedAID, name, creator); err != nil {
		t.Fatalf("seed apply_runs recipe: %v", err)
	}

	res, err := UnlockForRerun(ctx, integrationPool, name, "rerun add_user", creator, newHistoryID, newApplyID)
	if err != nil {
		t.Fatalf("UnlockForRerun operational: %v", err)
	}
	if res.Scenario != "add_user" {
		t.Errorf("Scenario = %q, want add_user (последний упавший операционный сценарий)", res.Scenario)
	}
	if res.Input == nil || res.Input["user"] != "alice" {
		t.Errorf("Input = %v, want {user:alice} из recipe (операционный сценарий берёт recipe.input, не spec)", res.Input)
	}
	if _, leaked := res.Input["version"]; leaked {
		t.Error("Input несёт spec.input[version] — операционный сценарий обязан брать recipe.input, не spec")
	}
	if res.FromUpgrade {
		t.Error("FromUpgrade = true, want false (recipe без from_upgrade)")
	}
	if res.PreviousStatus != StatusErrorLocked {
		t.Errorf("PreviousStatus = %q, want error_locked", res.PreviousStatus)
	}

	got, err := SelectByName(ctx, integrationPool, name)
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if got.Status != StatusApplying {
		t.Errorf("status = %q, want applying (rerun минуя ready)", got.Status)
	}
}
