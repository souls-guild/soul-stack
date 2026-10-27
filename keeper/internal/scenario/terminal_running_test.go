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
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
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

// readCountingPool is a pool on the integration database counting the reads of a run's
// rows ([applyrun.SelectStatusesByApplyID]).
func readCountingPool(t *testing.T) (*pgxpool.Pool, *atomic.Int64) {
	t.Helper()
	var reads atomic.Int64
	pool, _ := refusingPool(t, func(data pgx.TraceQueryStartData) bool {
		if statusesRead(data) {
			reads.Add(1)
		}
		return false
	})
	return pool, &reads
}

// assertClosedAsBefore checks that a late abort whose every update landed read the
// run's rows once and wrote no run sentinel, as it did before NIM-993.
func assertClosedAsBefore(t *testing.T, got map[string]applyrun.Status, reads int64) {
	t.Helper()
	if reads != 1 {
		t.Errorf("the run's rows were read %d times, want once", reads)
	}
	if st, ok := got[render.RunSentinelSID]; ok {
		t.Errorf("run sentinel written (%q) beside rows that carry the abort", st)
	}
}

// TestIntegration_EnsureTerminal_TimeoutForceFailsRunning — a regression
// (architect advice): a non-cancel abort (timeout/dead-host) force-fails a
// running row instead of leaving it hanging forever. Mirrors
// NoClaim_BarrierTimeout but with a running row at terminalization time. All
// non-terminal rows → failed, read once, with no sentinel.
//
// Mutation: read the rows again after every late abort → two reads.
func TestIntegration_EnsureTerminal_TimeoutForceFailsRunning(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")

	applyID := audit.NewULID()
	seedApplyRunRow(t, applyID, "host-running.example.com", applyrun.StatusRunning)
	seedApplyRunRow(t, applyID, "host-dispatched.example.com", applyrun.StatusDispatched)
	seedApplyRunRow(t, applyID, "host-claimed.example.com", applyrun.StatusClaimed)
	seedApplyRunRow(t, applyID, "host-success.example.com", applyrun.StatusSuccess)

	pool, reads := readCountingPool(t)
	r := &Runner{deps: Deps{DB: pool}}
	spec := RunSpec{ApplyID: applyID, IncarnationName: "noop-prod", ScenarioName: "create", StartedByAID: "archon-alice"}

	// keepRunning=false (timeout): force-fail including running.
	r.ensureTerminalApplyRun(context.Background(), spec, "run_timeout", 0, applyrun.StatusFailed, false, nil, slog.Default())

	got := statusByApplyID(t, applyID)
	assertClosedAsBefore(t, got, reads.Load())
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
// Acolyte-claim path). The rows are read once, and no sentinel is written.
//
// Mutation: read the rows again after every late abort → two reads.
func TestIntegration_EnsureTerminal_CancelKeepsRunning(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")

	applyID := audit.NewULID()
	seedApplyRunRow(t, applyID, "host-running.example.com", applyrun.StatusRunning)
	seedApplyRunRow(t, applyID, "host-dispatched.example.com", applyrun.StatusDispatched)
	seedApplyRunRow(t, applyID, "host-planned.example.com", applyrun.StatusPlanned)
	seedApplyRunRow(t, applyID, "host-claimed.example.com", applyrun.StatusClaimed)
	seedApplyRunRow(t, applyID, "host-success.example.com", applyrun.StatusSuccess)

	pool, reads := readCountingPool(t)
	r := &Runner{deps: Deps{DB: pool}}
	spec := RunSpec{ApplyID: applyID, IncarnationName: "noop-prod", ScenarioName: "create", StartedByAID: "archon-alice"}

	// keepRunning=true (operator-Cancel): planned/claimed/dispatched → cancelled, running — keep.
	r.ensureTerminalApplyRun(context.Background(), spec, "cancelled", 0, applyrun.StatusCancelled, true, nil, slog.Default())

	got := statusByApplyID(t, applyID)
	assertClosedAsBefore(t, got, reads.Load())
	if got["host-running.example.com"] != applyrun.StatusRunning {
		t.Errorf("running host = %q, want running (Cancel does NOT touch a live apply - an honest RunResult will arrive)", got["host-running.example.com"])
	}
	if got["host-dispatched.example.com"] != applyrun.StatusCancelled {
		t.Errorf("dispatched host = %q, want cancelled (operator-Cancel)", got["host-dispatched.example.com"])
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

// ★ GUARD (NIM-993). A run aborting between steps with only a success row and a
// failed row a rescue handled — or one that dropped its host — still writes the
// run sentinel: the fold reads neither row as a failure of the run, so without it
// the run would read success (or partial_failed) once the lock goes. A row the
// abort is still to mark handled is decided as marked: the marks follow the
// sentinel, and the sentinel never reads them.
//
// Mutations: carriesAbort counting a handled row → no sentinel, the run reads
// success; counting a dropped row → partial_failed; deciding without handled
// (carriesAbort ignoring the rows to mark) → no sentinel for the row to mark.
func TestIntegration_EnsureTerminal_HandledOrDroppedRowCarriesNoAbort(t *testing.T) {
	toMark := []runRow{{sid: "host-b.example.com", step: 0}}
	for _, tc := range []struct {
		name        string
		mark        func(context.Context, applyrun.ExecQueryRower, string, string, int) error
		handled     []runRow
		terminal    applyrun.Status
		keepRunning bool
		want        applyrun.RunStatus
	}{
		{"handled, failure", applyrun.MarkHandled, nil, applyrun.StatusFailed, false, applyrun.RunStatusFailed},
		{"handled, cancel", applyrun.MarkHandled, nil, applyrun.StatusCancelled, true, applyrun.RunStatusCancelled},
		{"dropped, failure", applyrun.MarkDropped, nil, applyrun.StatusFailed, false, applyrun.RunStatusFailed},
		{"to mark, failure", applyrun.MarkHandled, toMark, applyrun.StatusFailed, false, applyrun.RunStatusFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetAll(t)
			seedOperator(t, "archon-alice")
			seedIncarnation(t, "noop-prod")
			ctx := context.Background()

			applyID := audit.NewULID()
			seedApplyRunRow(t, applyID, "host-a.example.com", applyrun.StatusSuccess)
			seedApplyRunRow(t, applyID, "host-b.example.com", applyrun.StatusFailed)
			mark := func() {
				if err := tc.mark(ctx, integrationPool, applyID, "host-b.example.com", 0); err != nil {
					t.Fatalf("mark host-b: %v", err)
				}
			}
			if tc.handled == nil {
				mark()
			}

			r := &Runner{deps: Deps{DB: integrationPool}}
			spec := RunSpec{ApplyID: applyID, IncarnationName: "noop-prod", ScenarioName: "create", StartedByAID: "archon-alice"}
			if !r.ensureTerminalApplyRun(ctx, spec, "render_failed", 1, tc.terminal, tc.keepRunning, tc.handled, slog.Default()) {
				t.Fatal("ensureTerminalApplyRun = false, want true: the sentinel was written")
			}
			if tc.handled != nil {
				mark()
			}

			if got := statusByApplyID(t, applyID)[render.RunSentinelSID]; got != tc.terminal {
				t.Errorf("run sentinel = %q, want %q", got, tc.terminal)
			}
			assertRunReads(t, "noop-prod", applyID, tc.want, true)
		})
	}
}

// rowUpdateOf matches an update of host sid's row status.
func rowUpdateOf(sid string) func(pgx.TraceQueryStartData) bool {
	return func(data pgx.TraceQueryStartData) bool {
		return strings.Contains(data.SQL, "UPDATE apply_runs") && len(data.Args) > 1 && data.Args[1] == any(sid)
	}
}

func statusesRead(data pgx.TraceQueryStartData) bool {
	return strings.Contains(data.SQL, "SELECT sid, status, task_idx")
}

// ★ GUARD (NIM-993). host-a's row is the only one to carry the abort, and the abort's
// update of it misses: either the host's own success lands between the abort's read and
// its update, or the update fails and the success lands after the abort. The abort
// reads the rows again and, with no settled row carrying it, writes the run sentinel; a
// row it is still to mark handled is decided as marked there too. A second read that
// fails leaves the rows unmarked, a failure of the run, and writes the sentinel all the
// same: the rows it could not read may all end in success, and a sentinel never makes an
// aborted run read so. A run whose every update lands reads its rows once and writes no
// sentinel, as before NIM-993 ([assertClosedAsBefore]).
//
// Mutations: no second read (true after the updates) → no sentinel, the run reads
// success; the second read deciding without handled → no sentinel for the row to mark;
// the second read counting a row still open → no sentinel when the update failed; true
// when the second read fails → ensureTerminalApplyRun reports true; a second read that
// fails writing no sentinel → the run reads success when no row is left to mark.
func TestIntegration_EnsureTerminal_ARowEndingOnItsOwnLeavesTheSentinel(t *testing.T) {
	const hostA = "host-a.example.com"
	toMark := []runRow{{sid: "host-b.example.com", step: 0}}
	for _, tc := range []struct {
		name    string
		handled []runRow
		// landFirst lands host-a's success before the update, which then finds the row
		// terminal; otherwise the update fails and the success lands after the abort.
		landFirst     bool
		rereadRefused bool
	}{
		{"success before the update", nil, true, false},
		{"success before the update, a row to mark", toMark, true, false},
		{"update failed, success after", nil, false, false},
		{"success before the update, the second read refused", toMark, true, true},
		{"success before the update, no row to mark, the second read refused", nil, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetAll(t)
			seedOperator(t, "archon-alice")
			seedIncarnation(t, "noop-prod")
			ctx := context.Background()

			applyID := audit.NewULID()
			seedApplyRunRow(t, applyID, hostA, applyrun.StatusDispatched)
			if tc.handled != nil {
				seedApplyRunRow(t, applyID, "host-b.example.com", applyrun.StatusFailed)
			}
			landSuccess := func() error {
				return applyrun.UpdateStatus(ctx, integrationPool, applyID, hostA, 0, applyrun.StatusSuccess, nil)
			}
			var reads, landed atomic.Int64
			pool, refuser := refusingPool(t, func(data pgx.TraceQueryStartData) bool {
				if statusesRead(data) {
					return reads.Add(1) == 2 && tc.rereadRefused
				}
				if !rowUpdateOf(hostA)(data) {
					return false
				}
				if tc.landFirst && landSuccess() == nil {
					landed.Add(1)
				}
				return !tc.landFirst
			})

			r := &Runner{deps: Deps{DB: pool}}
			spec := RunSpec{ApplyID: applyID, IncarnationName: "noop-prod", ScenarioName: "create", StartedByAID: "archon-alice"}
			ok := r.ensureTerminalApplyRun(ctx, spec, "run_timeout", 1, applyrun.StatusFailed, false, tc.handled, slog.Default())
			if ok == tc.rereadRefused {
				t.Errorf("ensureTerminalApplyRun = %v, want %v", ok, !tc.rereadRefused)
			}
			if ok {
				r.markHandled(ctx, spec, tc.handled, slog.Default())
			}
			if !tc.landFirst {
				if err := landSuccess(); err != nil {
					t.Fatalf("landing host-a's success after the abort: %v", err)
				}
			}
			var wantLanded, wantRefused int64
			if tc.landFirst {
				wantLanded = 1
			} else {
				wantRefused++
			}
			if tc.rereadRefused {
				wantRefused++
			}
			if landed.Load() != wantLanded || refuser.refused.Load() != wantRefused {
				t.Fatalf("landed first %d, refused %d; want %d, %d", landed.Load(), refuser.refused.Load(), wantLanded, wantRefused)
			}

			got := statusByApplyID(t, applyID)
			if got[hostA] != applyrun.StatusSuccess {
				t.Errorf("host-a = %q, want its own success", got[hostA])
			}
			if st, written := got[render.RunSentinelSID]; !written || st != applyrun.StatusFailed {
				t.Errorf("run sentinel = %q (written %v), want written, failed", st, written)
			}
			assertRunReads(t, "noop-prod", applyID, applyrun.RunStatusFailed, true)
		})
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
