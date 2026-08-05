//go:build integration

package incarnation

import (
	"context"
	"errors"
	"testing"
)

// TestIntegration_UnlockForRerun_Day2_ReplaysTheAttemptSnapshot — the day-2 branch
// of rerun-last on REAL PG rather than a fakeTx.
//
// It used to assert the input came from `apply_runs.recipe`. That source is gone
// (NIM-408): the recipe lived on a table with a 30-day purge, so a day-2 rerun of
// an older failure eventually became impossible, and the create path read a
// different source again (`spec.input`) — two answers to one question. The
// replayable snapshot now lives on the attempt's OWN `state_history` row, which
// is also where its outcome is recorded, so the two cannot drift.
//
// The reason to run this against a real database is schema drift in the probe
// SQL. The unit tests hand the reader a fake row, so a SELECT naming a column
// that no longer exists passes every one of them and fails on the first real
// query — which is exactly how `UpdateTraits` was still selecting the dropped
// `spec` column with a green unit suite.
func TestIntegration_UnlockForRerun_Day2_ReplaysTheAttemptSnapshot(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	const (
		name         = "redis-prod"
		failedAID    = "01HDAY20FAILED000000000001"
		snapHistID   = "01HDAY20SNAP00000000000001"
		newApplyID   = "01HDAY20RERUN0000000000001"
		newHistoryID = "01HDAY20HIST00000000000001"
	)
	creator, created := "archon-alice", "create"

	// The incarnation has since been upgraded: its CURRENT pin is v2.0.0, while
	// the attempt below ran against v1.0.0. That gap is the point of the whole
	// snapshot — a rerun must replay the code the attempt used.
	inc := &Incarnation{
		Name: name, Service: "redis", ServiceVersion: "v2.0.0",
		StateSchemaVersion: 1, Status: StatusErrorLocked,
		CreatedScenario: &created, CreatedByAID: &creator,
	}
	if err := Create(ctx, integrationPool, inc); err != nil {
		t.Fatalf("Create error_locked: %v", err)
	}

	// The last attempt: a failed day-2 `add_user` (≠ the created `create`), with
	// its replayable snapshot on its own row.
	if _, err := integrationPool.Exec(ctx, `
INSERT INTO state_history (history_id, incarnation_name, scenario, state_before, state_after, apply_id, run, run_status)
VALUES ($1, $2, 'add_user', '{}'::jsonb, '{}'::jsonb, $3,
        '{"scenario_name":"add_user","input":{"user":"alice"},
          "service_ref":{"Name":"redis","Git":"file:///srv/redis","Ref":"v1.0.0"}}'::jsonb,
        'error_locked')`,
		snapHistID, name, failedAID); err != nil {
		t.Fatalf("seed state_history snapshot: %v", err)
	}

	res, err := UnlockForRerun(ctx, integrationPool, name, "rerun add_user", creator, newHistoryID, newApplyID)
	if err != nil {
		t.Fatalf("UnlockForRerun day-2: %v", err)
	}
	if res.Scenario != "add_user" {
		t.Errorf("Scenario = %q, want add_user (the last failed day-2)", res.Scenario)
	}
	if res.Input == nil || res.Input["user"] != "alice" {
		t.Errorf("Input = %v, want {user:alice} from the attempt's snapshot", res.Input)
	}
	if res.ServiceRefRef != "v1.0.0" {
		t.Errorf("ServiceRefRef = %q, want v1.0.0 — the rerun must replay the ref the ATTEMPT used, "+
			"not the incarnation's current v2.0.0 pin", res.ServiceRefRef)
	}
	if res.ServiceRefGit != "file:///srv/redis" {
		t.Errorf("ServiceRefGit = %q, want the snapshot's URL", res.ServiceRefGit)
	}
	if res.FromUpgrade {
		t.Error("FromUpgrade = true, want false (the snapshot carries no from_upgrade)")
	}
	if res.PreviousStatus != StatusErrorLocked {
		t.Errorf("PreviousStatus = %q, want error_locked", res.PreviousStatus)
	}

	got, err := SelectByName(ctx, integrationPool, name)
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if got.Status != StatusApplying {
		t.Errorf("status = %q, want applying (rerun bypasses ready)", got.Status)
	}
}

// TestIntegration_UnlockForRerun_RefusesAnInputItDoesNotNeed — the boundary,
// against a real database.
//
// "Rerun that" and "run this instead" are different requests. When the attempt
// carries a replayable snapshot, an input in the body is refused rather than
// merged or ignored — and refused BEFORE anything is written, so the incarnation
// stays locked exactly as it was.
func TestIntegration_UnlockForRerun_RefusesAnInputItDoesNotNeed(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	const (
		name       = "redis-prod"
		failedAID  = "01HREFUSEFAILED00000000001"
		snapHistID = "01HREFUSESNAP0000000000001"
	)
	creator, created := "archon-alice", "create"
	inc := &Incarnation{
		Name: name, Service: "redis", ServiceVersion: "v1.0.0",
		StateSchemaVersion: 1, Status: StatusErrorLocked,
		CreatedScenario: &created, CreatedByAID: &creator,
	}
	if err := Create(ctx, integrationPool, inc); err != nil {
		t.Fatalf("Create error_locked: %v", err)
	}
	if _, err := integrationPool.Exec(ctx, `
INSERT INTO state_history (history_id, incarnation_name, scenario, state_before, state_after, apply_id, run, run_status)
VALUES ($1, $2, 'add_user', '{}'::jsonb, '{}'::jsonb, $3,
        '{"scenario_name":"add_user","input":{"user":"alice"}}'::jsonb, 'error_locked')`,
		snapHistID, name, failedAID); err != nil {
		t.Fatalf("seed state_history snapshot: %v", err)
	}

	_, err := UnlockForRerunWithInput(ctx, integrationPool, name, "rerun with different values",
		creator, "01HREFUSEHIST0000000000001", "01HREFUSERERUN000000000001",
		map[string]any{"user": "bob"})
	if err == nil {
		t.Fatal("UnlockForRerunWithInput accepted an input for a replayable attempt, want ErrRerunInputNotNeeded")
	}
	if !errors.Is(err, ErrRerunInputNotNeeded) {
		t.Fatalf("err = %v, want ErrRerunInputNotNeeded", err)
	}

	// The refusal must leave the row untouched: it happens inside the transaction
	// and before any Exec, so a caller that retries correctly finds the same lock.
	got, err := SelectByName(ctx, integrationPool, name)
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if got.Status != StatusErrorLocked {
		t.Errorf("status = %q, want error_locked — a refusal must not move the incarnation", got.Status)
	}
}
