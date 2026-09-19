package grpc

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// handleRunResult — handler for the [keeperv1.RunResult] payload (M2.4).
//
// PM-decision (4):
//   - SUCCESS  → UPDATE incarnation.state + state_history + status=ready.
//   - FAILED / CANCELLED / ERROR_LOCKED → UPDATE status=error_locked (state is
//     not changed — we keep the last known-good snapshot).
//
// Atomicity — a single transaction via [pgx.BeginFunc]:
//   - INSERT state_history;
//   - UPDATE incarnation.state/status/status_details.
//
// Audit `run.completed` is written **after** the commit — DB consistency does
// not depend on the audit write path (pattern identical to Bootstrap).
//
// Incarnation addressing (M2.x): `apply_id` is carried by the Soul itself; the
// incarnation name is absent from the proto (see apply.proto: RunResult only
// carries apply_id/status/state_changes -- the last one RESERVED and never
// populated, see apply.proto). Correlation is closed by the `apply_runs`
// table (migration 018): the scenario-runner writes a `(apply_id, sid)` row on
// dispatch of `ApplyRequest`, and this handler reads it via
// [applyrun.SelectIncarnationByApplyID] and moves it to a terminal status
// ([correlateRunResult]). An apply_id not found in `apply_runs` (ad-hoc push
// without a scenario-runner) → log+skip.
//
// Writing `incarnation.state` is not this handler's business at all. Since
// [ADR-0084] the write is a `core.state.<verb>` keeper-side task that commits its
// own field at its own step ([coremod/state]), so by the time a RunResult lands
// the state is already whatever the run captured. We do NOT touch state here.
//
// [ADR-0084]: docs/adr/0084-explicit-state-capture.md
func (h *eventStreamHandler) handleRunResult(ctx context.Context, sid, sessionID string, ev *keeperv1.RunResult) {
	if ev == nil {
		h.logger.Warn("eventstream: RunResult payload is nil",
			slog.String("sid", sid), slog.String("session_id", sessionID))
		return
	}

	// The observable half (audit + SSE) is shared with the push branch of the
	// scenario dispatcher (NIM-880). The terminal transition below is not: only
	// this path has a re-claim epoch to check and an incarnation to resolve.
	h.events.RunResult(ctx, sid, ev)
	h.correlateRunResult(ctx, sid, sessionID, ev)
}

// correlateRunResult resolves the incarnation by `(apply_id, sid)` via the
// `apply_runs` registry and moves the run row to a terminal status
// (`success`/`failed`/`cancelled`). Closes the correlation gap "Keeper doesn't
// know which incarnation a run belongs to."
//
// ApplyRunDB=nil (unit build without PG / ad-hoc push) → no-op.
// apply_id not found in `apply_runs` (push without a scenario-runner) → log+skip.
func (h *eventStreamHandler) correlateRunResult(ctx context.Context, sid, sessionID string, ev *keeperv1.RunResult) {
	if h.deps.ApplyRunDB == nil {
		return
	}
	applyID := ev.GetApplyId()
	// passage (ADR-056 staged-render): RunResult correlates with the
	// (apply_id, sid, passage) row. The Soul echoes passage from ApplyRequest as-is; N=1 →
	// 0 (the host's only row), correlation is BIT-FOR-BIT as before staged-render.
	passage := int(ev.GetPassage())
	name, scenario, rowAttempt, err := applyrun.SelectIncarnationByApplyID(ctx, h.deps.ApplyRunDB, applyID, sid, passage)
	if err != nil {
		if errors.Is(err, applyrun.ErrApplyRunNotFound) {
			h.logger.Info("eventstream: RunResult without an apply_runs row - correlation skipped",
				slog.String("sid", sid),
				slog.String("session_id", sessionID),
				slog.String("apply_id", applyID))
			return
		}
		h.logger.Warn("eventstream: resolve incarnation by apply_id failed",
			slog.String("sid", sid),
			slog.String("apply_id", applyID),
			slog.Any("error", err))
		return
	}

	// epoch-check (gate-1, ADR-027(g)): we don't commit a result from a stale attempt.
	//   recvAttempt == 0          → old Soul without echo (forward-compat) → commit;
	//   recvAttempt <  rowAttempt → stale: a re-claim with a bigger epoch exists →
	//                               DROP (keeper_runresult_stale_total metric),
	//                               state is NOT committed;
	//   recvAttempt == rowAttempt → current → commit;
	//   recvAttempt >  rowAttempt → impossible invariant (apply_runs.attempt only grows
	//                               on claim, RunResult.attempt echoes the
	//                               captured epoch) → defensive warn + commit
	//                               anyway (fail-safe: we don't lose the result of a
	//                               live run due to a desync/row-read anomaly).
	recvAttempt := ev.GetAttempt()
	if recvAttempt != 0 && recvAttempt < rowAttempt {
		h.deps.Metrics.ObserveRunResultStale()
		h.logger.Info("eventstream: RunResult from a stale attempt - stale-drop (commit rejected)",
			slog.String("sid", sid),
			slog.String("apply_id", applyID),
			slog.Int("recv_attempt", int(recvAttempt)),
			slog.Int("row_attempt", int(rowAttempt)),
			slog.String("incarnation", name),
			slog.String("scenario", scenario))
		return
	}
	if recvAttempt > rowAttempt {
		// The "attempt only grows" invariant is violated: keeper↔soul epoch desync
		// or a row-read anomaly. We don't drop — we commit (fail-safe), but
		// flag the invariant violation for triage.
		h.logger.Warn("eventstream: RunResult.attempt greater than the row attempt - 'attempt only grows' invariant violated (commit fail-safe)",
			slog.String("sid", sid),
			slog.String("apply_id", applyID),
			slog.Int("recv_attempt", int(recvAttempt)),
			slog.Int("row_attempt", int(rowAttempt)))
	}

	status := runStatusToApplyStatus(ev.GetStatus())
	// error_summary is NOT overwritten here: on FAILED the reason is already recorded
	// per-task ([recordTaskFailure] → RecordTaskFailure) and carries idx+module+
	// message of the failed task (BUG-3). UpdateStatus uses COALESCE — nil doesn't overwrite
	// what's already recorded. If there was no TaskEvent with an error (dispatch-level failure),
	// error_summary stays NULL, and barrier classify falls back to the status itself
	// (`failed`) — without a meaningless `run_status=RUN_STATUS_FAILED`.
	if err := applyrun.UpdateStatus(ctx, h.deps.ApplyRunDB, applyID, sid, passage, status, nil); err != nil {
		// Append-only single-winner (ADR-027(j)): another handler already moved the row
		// to terminal (recovery takeover / repeated RunResult). NOT an error
		// — the first one won, we don't write a duplicate terminal. Log it as a no-op.
		if errors.Is(err, applyrun.ErrApplyRunAlreadyTerminal) {
			h.logger.Info("eventstream: apply_runs already terminal - correlation no-op (first committer won)",
				slog.String("sid", sid),
				slog.String("apply_id", applyID),
				slog.String("incarnation", name),
				slog.String("scenario", scenario))
			return
		}
		h.logger.Warn("eventstream: update apply_runs status failed",
			slog.String("sid", sid),
			slog.String("apply_id", applyID),
			slog.String("incarnation", name),
			slog.String("scenario", scenario),
			slog.Any("error", err))
		return
	}
	h.logger.Info("eventstream: apply_runs correlated",
		slog.String("sid", sid),
		slog.String("apply_id", applyID),
		slog.String("incarnation", name),
		slog.String("scenario", scenario),
		slog.String("status", string(status)))
}

// runStatusToApplyStatus maps [keeperv1.RunStatus] to [applyrun.Status].
// FAILED / ERROR_LOCKED / other → failed (terminal lock); CANCELLED →
// cancelled; SUCCESS → success.
func runStatusToApplyStatus(rs keeperv1.RunStatus) applyrun.Status {
	switch rs {
	case keeperv1.RunStatus_RUN_STATUS_SUCCESS:
		return applyrun.StatusSuccess
	case keeperv1.RunStatus_RUN_STATUS_CANCELLED:
		return applyrun.StatusCancelled
	default:
		return applyrun.StatusFailed
	}
}

// commitRunState — atomically commits run results into the incarnation.
// Exported for the future scenario-runner (M0.6c-2), which owns the
// apply_id ↔ incarnation mapping and orchestrates apply from the Operator API
// through to RunResult.
//
// pool — *pgxpool.Pool or a compatible type. scenario / name / applyID are taken
// by the caller from the incarnation-state table. stateBefore — the current value of
// `incarnation.state` (read under SELECT FOR UPDATE inside the transaction);
// stateAfter — the state to commit, merged by the caller.
//
// UNUSED since the scenario-runner took over the terminal transition, and doubly
// so since [ADR-0084] moved the state write into `core.state.<verb>` steps that
// commit per-field. Kept as the reference shape of the atomic
// state+status+history transition.
//
// On RUN_STATUS_SUCCESS the status becomes `ready`; on the rest —
// `error_locked` with status_details, so triage can see the reason.
//
// Single-winner (ADR-027(j) W1): [incarnation.UpdateStateFromRun] commits under
// the guard `status IN ('applying','destroying')`. If another committer already
// moved the row out of applying (recovery takeover), it returns
// [incarnation.ErrAlreadyFinalized] — the caller must treat it as a no-op
// (log, don't fail the path), not as a consistency error.
// runFailureSummary lifts the human-readable cause out of a terminal's
// status_details, already masked by the caller. Empty on success.
func runFailureSummary(details map[string]any) string {
	if details == nil {
		return ""
	}
	if e, ok := details["error"].(string); ok {
		return e
	}
	if reason, ok := details["reason"].(string); ok {
		return reason
	}
	return ""
}

func commitRunState(
	ctx context.Context,
	pool TxBeginner,
	name, scenario, applyID, historyID string,
	stateBefore, stateAfter map[string]any,
	runStatus keeperv1.RunStatus,
) error {
	status := incarnation.StatusReady
	var details map[string]any
	switch runStatus {
	case keeperv1.RunStatus_RUN_STATUS_SUCCESS:
		// stateAfter is whatever the caller decided to commit.
	default:
		status = incarnation.StatusErrorLocked
		details = map[string]any{
			"reason":     "run_failed",
			"run_status": runStatus.String(),
			"apply_id":   applyID,
		}
		// On error we do NOT overwrite state — we keep stateBefore.
		stateAfter = stateBefore
	}

	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return incarnation.UpdateStateFromRun(
			ctx, tx,
			name, scenario, applyID,
			stateBefore, stateAfter,
			status, details,
			nil, // soul_grpc — no Archon AID.
			historyID,
			// No engine stamp (ADR-0076(l)): this path commits a RunResult without
			// a scenario-runner, so it never rendered and has no engine facts of
			// its own to record. The per-host versions are on the apply_runs rows.
			nil,
			// The outcome without a replayable snapshot, for the same reason: this
			// terminal has no RunSpec in hand — the run it closes was started by
			// somebody else's goroutine, possibly on another Keeper. rerun-last of
			// such a row answers 422 asking for the input, which is honest.
			&incarnation.RunOutcome{
				Status:       string(status),
				FinishedAt:   time.Now().UTC(),
				ErrorSummary: runFailureSummary(details),
			},
		)
	})
}
