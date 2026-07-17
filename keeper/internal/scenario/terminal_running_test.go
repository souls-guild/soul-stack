//go:build integration

// Integration tests for [Runner.ensureTerminalApplyRun] — handling running
// rows BECAUSE OF an abort (keepRunning). Close a regression: on
// operator-Cancel, running is left untouched (live host, an honest RunResult
// will arrive); on timeout/dead-host, force-fail (RunResult never arrives, the
// reaper doesn't pick up running — ADR-027 narrowed to claimed → otherwise
// apply_run hangs forever, BAG-1 recovery). Matrix:
//   - errCancelRequested (keepRunning=true): planned/claimed/dispatched →
//     cancelled; running → do NOT touch.
//   - timeout/ctx/other (keepRunning=false): planned/claimed/dispatched →
//     failed; running → failed (force-fail).
// Reuses the integration_test.go harness (TestMain/integrationPool/resetAll/seed*).

package scenario

import (
	"context"
	"log/slog"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// seedApplyRunRow inserts an apply_runs row in an arbitrary (including
// running) status for a direct terminalization test. applyrun.Insert doesn't
// validate the status as "non-terminal", which is exactly what we need — we
// fix running before calling the method.
func seedApplyRunRow(t *testing.T, applyID, sid string, status applyrun.Status) {
	t.Helper()
	run := applyrun.ApplyRun{
		ApplyID:         applyID,
		SID:             sid,
		IncarnationName: "noop-prod",
		Scenario:        "create",
		Status:          status,
		StartedByAID:    startedByPtr("archon-alice"),
	}
	if err := applyrun.Insert(context.Background(), integrationPool, &run); err != nil {
		t.Fatalf("seedApplyRunRow(%s=%s): %v", sid, status, err)
	}
}

func statusByApplyID(t *testing.T, applyID string) map[string]applyrun.Status {
	t.Helper()
	rows, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	out := make(map[string]applyrun.Status, len(rows))
	for _, r := range rows {
		out[r.SID] = r.Status
	}
	return out
}

// TestIntegration_EnsureTerminal_TimeoutForceFailsRunning — a regression
// (architect advice): a non-cancel abort (timeout/dead-host) force-fails a
// running row instead of leaving it hanging forever. Mirrors
// NoClaim_BarrierTimeout but with a running row at terminalization time. All
// non-terminal rows → failed.
func TestIntegration_EnsureTerminal_TimeoutForceFailsRunning(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")

	applyID := audit.NewULID()
	seedApplyRunRow(t, applyID, "host-running.example.com", applyrun.StatusRunning)
	seedApplyRunRow(t, applyID, "host-dispatched.example.com", applyrun.StatusDispatched)
	seedApplyRunRow(t, applyID, "host-claimed.example.com", applyrun.StatusClaimed)
	seedApplyRunRow(t, applyID, "host-success.example.com", applyrun.StatusSuccess)

	r := &Runner{deps: Deps{DB: integrationPool}}
	spec := RunSpec{ApplyID: applyID, IncarnationName: "noop-prod", ScenarioName: "create", StartedByAID: "archon-alice"}

	// keepRunning=false (timeout): force-fail including running.
	r.ensureTerminalApplyRun(context.Background(), spec, "run_timeout", applyrun.StatusFailed, false, slog.Default())

	got := statusByApplyID(t, applyID)
	if got["host-running.example.com"] != applyrun.StatusFailed {
		t.Errorf("running host = %q, want failed (BAG-1: timeout force-fails running, RunResult will not arrive)", got["host-running.example.com"])
	}
	if got["host-dispatched.example.com"] != applyrun.StatusFailed {
		t.Errorf("dispatched host = %q, want failed", got["host-dispatched.example.com"])
	}
	if got["host-claimed.example.com"] != applyrun.StatusFailed {
		t.Errorf("claimed host = %q, want failed", got["host-claimed.example.com"])
	}
	// success — already terminal, don't touch.
	if got["host-success.example.com"] != applyrun.StatusSuccess {
		t.Errorf("success host = %q, want success (terminal state is not overwritten)", got["host-success.example.com"])
	}
}

// TestIntegration_EnsureTerminal_CancelKeepsRunning — operator-Cancel: running
// on a LIVE host is NOT touched (honest reporting, RunResult will arrive),
// non-terminal rows (planned/claimed/dispatched) → cancelled. Verifies the
// cancel→cancelled path goes THROUGH ensureTerminalApplyRun (not the old
// Acolyte-claim path).
func TestIntegration_EnsureTerminal_CancelKeepsRunning(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")

	applyID := audit.NewULID()
	seedApplyRunRow(t, applyID, "host-running.example.com", applyrun.StatusRunning)
	seedApplyRunRow(t, applyID, "host-planned.example.com", applyrun.StatusPlanned)
	seedApplyRunRow(t, applyID, "host-claimed.example.com", applyrun.StatusClaimed)
	seedApplyRunRow(t, applyID, "host-success.example.com", applyrun.StatusSuccess)

	r := &Runner{deps: Deps{DB: integrationPool}}
	spec := RunSpec{ApplyID: applyID, IncarnationName: "noop-prod", ScenarioName: "create", StartedByAID: "archon-alice"}

	// keepRunning=true (operator-Cancel): planned/claimed → cancelled, running — keep.
	r.ensureTerminalApplyRun(context.Background(), spec, "cancelled", applyrun.StatusCancelled, true, slog.Default())

	got := statusByApplyID(t, applyID)
	if got["host-running.example.com"] != applyrun.StatusRunning {
		t.Errorf("running host = %q, want running (Cancel does NOT touch a live apply - an honest RunResult will arrive)", got["host-running.example.com"])
	}
	if got["host-planned.example.com"] != applyrun.StatusCancelled {
		t.Errorf("planned host = %q, want cancelled (operator-Cancel)", got["host-planned.example.com"])
	}
	if got["host-claimed.example.com"] != applyrun.StatusCancelled {
		t.Errorf("claimed host = %q, want cancelled (operator-Cancel)", got["host-claimed.example.com"])
	}
	// success — already terminal, cancel doesn't overwrite it (mirrors the timeout test).
	if got["host-success.example.com"] != applyrun.StatusSuccess {
		t.Errorf("success host = %q, want success (terminal state is not overwritten on cancel)", got["host-success.example.com"])
	}
}

// TestUnit_FailureTerminalStatus — sentinel boundary: errCancelRequested →
// cancelled, everything else (ctx.Err/RunTimeout/an arbitrary provider fail) →
// failed. The same sentinel used to determine keepRunning in abort.
func TestUnit_FailureTerminalStatus(t *testing.T) {
	if got := failureTerminalStatus(errCancelRequested); got != applyrun.StatusCancelled {
		t.Errorf("errCancelRequested → %q, want cancelled", got)
	}
	if got := failureTerminalStatus(context.DeadlineExceeded); got != applyrun.StatusFailed {
		t.Errorf("DeadlineExceeded → %q, want failed", got)
	}
	if got := failureTerminalStatus(context.Canceled); got != applyrun.StatusFailed {
		t.Errorf("Canceled → %q, want failed", got)
	}
}
