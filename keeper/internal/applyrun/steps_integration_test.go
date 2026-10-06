//go:build integration

package applyrun

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Guards for ADR-027 amend (p): a host of a linear run holds one apply_runs row
// per step, and the queue must address the step, not the host.

const stepsApplyID = "01HSTEPS"

func seedStepRow(t *testing.T, sid string, passage int, status Status, attempt int) {
	t.Helper()
	const q = `INSERT INTO apply_runs (apply_id, sid, incarnation_name, scenario, status, passage, attempt)
		VALUES ($1, $2, 'redis-prod', 'create', $3, $4, $5)`
	if _, err := integrationPool.Exec(context.Background(), q, stepsApplyID, sid, string(status), passage, attempt); err != nil {
		t.Fatalf("seed step row %s/%d: %v", sid, passage, err)
	}
}

func plannedStep(t *testing.T, sid string, passage int) {
	t.Helper()
	if err := InsertPlanned(context.Background(), integrationPool, &ApplyRun{
		ApplyID: stepsApplyID, SID: sid, IncarnationName: "redis-prod", Scenario: "create",
		Recipe: &Recipe{ScenarioName: "create"}, Passage: passage,
	}); err != nil {
		t.Fatalf("InsertPlanned %s/%d: %v", sid, passage, err)
	}
}

type stepRowState struct {
	status     Status
	attempt    int
	claimByKID *string
}

func readStepRow(t *testing.T, sid string, passage int) stepRowState {
	t.Helper()
	var (
		s   stepRowState
		raw string
	)
	const q = `SELECT status, attempt, claim_by_kid FROM apply_runs WHERE apply_id = $1 AND sid = $2 AND passage = $3`
	if err := integrationPool.QueryRow(context.Background(), q, stepsApplyID, sid, passage).Scan(&raw, &s.attempt, &s.claimByKID); err != nil {
		t.Fatalf("read step row %s/%d: %v", sid, passage, err)
	}
	s.status = Status(raw)
	return s
}

// reclaimRow does what reclaim_apply_runs does to one expired Ward; the reaper
// package imports this one, so its SQL cannot be called from here.
func reclaimRow(t *testing.T, sid string, passage int) {
	t.Helper()
	const q = `UPDATE apply_runs SET status = 'planned', claim_by_kid = NULL, claim_at = NULL, claim_expires_at = NULL
		WHERE apply_id = $1 AND sid = $2 AND passage = $3 AND status = 'claimed'`
	tag, err := integrationPool.Exec(context.Background(), q, stepsApplyID, sid, passage)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("reclaim %s/%d: rows=%d err=%v", sid, passage, tag.RowsAffected(), err)
	}
}

func claimOne(t *testing.T, kid string) *ApplyRun {
	t.Helper()
	claimed, err := ClaimNext(context.Background(), integrationPool, kid, 30*time.Second, 10, "")
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed %d rows, want 1", len(claimed))
	}
	return claimed[0]
}

func resetSteps(t *testing.T) {
	t.Helper()
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
}

// Step 0 is terminal and step 1 is planned on the same host: the claim takes
// step 1 and leaves step 0 exactly as it was. Keyed by (apply_id, sid) with no
// status filter — the form before NIM-960 — the claim also took step 0's
// terminal row; the step key and the status filter each keep it out alone.
func TestIntegration_Steps_ClaimTakesOnlyTheOpenStep(t *testing.T) {
	resetSteps(t)
	seedStepRow(t, "host-a", 0, StatusSuccess, 2)
	plannedStep(t, "host-a", 1)

	got := claimOne(t, "keeper-1")
	if got.Passage != 1 {
		t.Errorf("claimed passage = %d, want 1", got.Passage)
	}
	step0 := readStepRow(t, "host-a", 0)
	if step0.status != StatusSuccess || step0.attempt != 2 || step0.claimByKID != nil {
		t.Errorf("step 0 = %+v, want success/attempt 2/unclaimed (untouched)", step0)
	}
	if again, err := ClaimNext(context.Background(), integrationPool, "keeper-1", 30*time.Second, 10, ""); err != nil || len(again) != 0 {
		t.Errorf("second claim: %d rows, err %v; want none left", len(again), err)
	}
}

// The Soul fences by apply_id alone and drops an attempt below the highest it
// has accepted for that run. Step 0 reached attempt 2 through a re-claim, so
// step 1's FIRST claim must already be above 2, or the Soul drops it and the
// step barrier waits out the run. The epoch is the host's in this run: another
// host of the run, and the same host in another run, have their own.
func TestIntegration_Steps_FirstClaimOutranksEveryEarlierAttempt(t *testing.T) {
	resetSteps(t)
	seedStepRow(t, "host-a", 0, StatusSuccess, 2)
	seedStepRow(t, "host-b", 0, StatusSuccess, 5)
	if _, err := integrationPool.Exec(context.Background(), `INSERT INTO apply_runs
		(apply_id, sid, incarnation_name, scenario, status, passage, attempt)
		VALUES ('01HOTHERRUN', 'host-a', 'redis-prod', 'create', 'success', 0, 7)`); err != nil {
		t.Fatalf("seed another run: %v", err)
	}
	plannedStep(t, "host-a", 1)

	got := claimOne(t, "keeper-1")
	if got.Attempt != 3 {
		t.Errorf("step 1 first attempt = %d, want 3 (above this host's 2 in this run, blind to host-b's 5 and the other run's 7)", got.Attempt)
	}

	// A re-claim of the same step still grows it.
	reclaimRow(t, "host-a", 1)
	if again := claimOne(t, "keeper-2"); again.Attempt != 4 {
		t.Errorf("re-claimed step 1 attempt = %d, want 4", again.Attempt)
	}
}

// RequestCancel flags only non-terminal rows, so the step-0 row of a host keeps
// cancel_requested=false while step 1 carries the flag; the read before
// SendApply must look at step 1's own row.
func TestIntegration_Steps_CancelReadIsTheStepRow(t *testing.T) {
	resetSteps(t)
	seedStepRow(t, "host-a", 0, StatusSuccess, 1)
	plannedStep(t, "host-a", 1)
	ctx := context.Background()

	if n, err := RequestCancel(ctx, integrationPool, stepsApplyID); err != nil || n != 1 {
		t.Fatalf("RequestCancel: n=%d err=%v, want 1 row (step 1)", n, err)
	}
	if got, err := SelectCancelRequested(ctx, integrationPool, stepsApplyID, "host-a", 1); err != nil || !got {
		t.Errorf("step 1 cancel_requested = %v (err %v), want true", got, err)
	}
	if got, err := SelectCancelRequested(ctx, integrationPool, stepsApplyID, "host-a", 0); err != nil || got {
		t.Errorf("step 0 cancel_requested = %v (err %v), want false", got, err)
	}
}

// An Acolyte whose Ward was reclaimed and re-claimed at attempt 2 must not hand
// the task off with its stale attempt 1: the Soul would run attempt 1, the
// receive check would drop its result, and the row would never close.
func TestIntegration_Steps_MarkDispatchedFencedByAttempt(t *testing.T) {
	resetSteps(t)
	plannedStep(t, "host-a", 0)
	ctx := context.Background()

	stale := claimOne(t, "keeper-1")
	reclaimRow(t, "host-a", 0)
	live := claimOne(t, "keeper-2")

	if err := MarkDispatched(ctx, integrationPool, stepsApplyID, "host-a", 0, stale.Attempt, ""); !errors.Is(err, ErrWardLost) {
		t.Fatalf("stale mark: err = %v, want ErrWardLost", err)
	}
	if row := readStepRow(t, "host-a", 0); row.status != StatusClaimed || row.attempt != live.Attempt {
		t.Fatalf("after stale mark: %+v, want claimed at attempt %d", row, live.Attempt)
	}
	if err := MarkDispatched(ctx, integrationPool, stepsApplyID, "host-a", 0, live.Attempt, ""); err != nil {
		t.Fatalf("live mark: %v", err)
	}
	if row := readStepRow(t, "host-a", 0); row.status != StatusDispatched {
		t.Errorf("after live mark: status %q, want dispatched", row.status)
	}
}

// The claim path's own terminal writes are fenced the same way: a writer that
// lost the Ward writes nothing, whether its row is planned again (reclaim keeps
// attempt, so only the status filter catches this) or held by a newer claim.
// The live owner may still close its row after the hand-off (`dispatched`).
func TestIntegration_Steps_WardStatusFencedByPhaseAndAttempt(t *testing.T) {
	resetSteps(t)
	plannedStep(t, "host-a", 0)
	ctx := context.Background()
	summary := "send_apply_failed"

	stale := claimOne(t, "keeper-1")
	reclaimRow(t, "host-a", 0)
	if err := UpdateWardStatus(ctx, integrationPool, stepsApplyID, "host-a", 0, stale.Attempt, StatusFailed, &summary); !errors.Is(err, ErrWardLost) {
		t.Fatalf("stale write on a reclaimed row: err = %v, want ErrWardLost", err)
	}
	if row := readStepRow(t, "host-a", 0); row.status != StatusPlanned {
		t.Fatalf("reclaimed row after stale write: %q, want planned", row.status)
	}

	live := claimOne(t, "keeper-2")
	if err := MarkDispatched(ctx, integrationPool, stepsApplyID, "host-a", 0, live.Attempt, ""); err != nil {
		t.Fatalf("live mark: %v", err)
	}
	if err := UpdateWardStatus(ctx, integrationPool, stepsApplyID, "host-a", 0, stale.Attempt, StatusFailed, &summary); !errors.Is(err, ErrWardLost) {
		t.Fatalf("stale write on a re-claimed row: err = %v, want ErrWardLost", err)
	}
	if row := readStepRow(t, "host-a", 0); row.status != StatusDispatched {
		t.Fatalf("re-claimed row after stale write: %q, want dispatched", row.status)
	}
	if err := UpdateWardStatus(ctx, integrationPool, stepsApplyID, "host-a", 0, live.Attempt, StatusFailed, &summary); err != nil {
		t.Fatalf("live write after hand-off: %v", err)
	}
	if row := readStepRow(t, "host-a", 0); row.status != StatusFailed {
		t.Errorf("after live write: %q, want failed", row.status)
	}
}

// Soul-reconcile is keyed by apply_id; with one open row per host and run the
// apply_id names that row. Only the open step is orphaned, the finished one
// keeps its terminal.
func TestIntegration_Steps_OrphanDispatchedTouchesOnlyTheOpenStep(t *testing.T) {
	resetSteps(t)
	seedStepRow(t, "host-a", 0, StatusSuccess, 1)
	plannedStep(t, "host-a", 1)
	ctx := context.Background()

	live := claimOne(t, "keeper-1")
	if err := MarkDispatched(ctx, integrationPool, stepsApplyID, "host-a", 1, live.Attempt, ""); err != nil {
		t.Fatalf("mark step 1: %v", err)
	}

	if n, err := OrphanDispatched(ctx, integrationPool, "host-a", []*ActiveApply{{ApplyID: stepsApplyID, Attempt: int32(live.Attempt)}}); err != nil || n != 0 {
		t.Fatalf("declared alive: orphaned %d (err %v), want 0", n, err)
	}
	if n, err := OrphanDispatched(ctx, integrationPool, "host-a", nil); err != nil || n != 1 {
		t.Fatalf("declared nothing: orphaned %d (err %v), want 1", n, err)
	}
	if row := readStepRow(t, "host-a", 1); row.status != StatusOrphaned {
		t.Errorf("step 1 = %q, want orphaned", row.status)
	}
	if row := readStepRow(t, "host-a", 0); row.status != StatusSuccess {
		t.Errorf("step 0 = %q, want success (untouched)", row.status)
	}
}
