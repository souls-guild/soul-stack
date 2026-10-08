package applyrun

import (
	"context"
	"fmt"
)

// failLostSQL fails a step a host was sent and has not answered, keeping a reason
// already recorded: a task that failed before the stream went is the better
// account of the host than the loss.
const failLostSQL = `
UPDATE apply_runs
SET status        = 'failed',
    error_summary = COALESCE(error_summary, $4),
    finished_at   = NOW()
WHERE apply_id = $1 AND sid = $2 AND passage = $3
  AND status = 'dispatched'
`

// FailLost moves a `dispatched` row to failed for a host whose agent stream stayed
// lost (docs/scenario/settings.md §2.2, `soul_timeout`). Single-winner: a row a
// RunResult or the WardRoster sweep already closed is left alone, and the result
// is false.
func FailLost(ctx context.Context, db ExecQueryRower, applyID, sid string, passage int, summary string) (bool, error) {
	tag, err := db.Exec(ctx, failLostSQL, applyID, sid, passage, summary)
	if err != nil {
		return false, fmt.Errorf("applyrun: fail lost: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

const stampFinishedSQL = `
UPDATE apply_runs
SET finished_at = NOW()
WHERE apply_id = $1 AND sid = $2 AND passage = $3
  AND finished_at IS NULL
`

// StampFinished sets finished_at on a row [Insert] wrote already terminal — the
// insert leaves it NULL.
func StampFinished(ctx context.Context, db ExecQueryRower, applyID, sid string, passage int) error {
	if _, err := db.Exec(ctx, stampFinishedSQL, applyID, sid, passage); err != nil {
		return fmt.Errorf("applyrun: stamp finished: %w", err)
	}
	return nil
}

// withdrawUnsentSQL removes a `dispatched` row whose ApplyRequest never left the
// Keeper. Nothing can reference it yet — no event comes back for a request never
// sent.
const withdrawUnsentSQL = `
DELETE FROM apply_runs
WHERE apply_id = $1 AND sid = $2 AND passage = $3
  AND status = 'dispatched'
`

// WithdrawUnsent deletes the row a linear step inserted for a host and then found
// no stream to send on, so the host waits for one without a row the WardRoster
// sweep would orphan: a dispatched row its Soul does not declare is one the sweep
// takes for lost. false — the row had already left `dispatched` (a sweep closed
// it first) and stays as it is.
func WithdrawUnsent(ctx context.Context, db ExecQueryRower, applyID, sid string, passage int) (bool, error) {
	tag, err := db.Exec(ctx, withdrawUnsentSQL, applyID, sid, passage)
	if err != nil {
		return false, fmt.Errorf("applyrun: withdraw unsent: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

const runCancelFlaggedSQL = `
SELECT EXISTS (SELECT 1 FROM apply_runs WHERE apply_id = $1 AND cancel_requested)
`

// RunCancelFlagged reports whether a row of run applyID carries the cluster-wide
// Cancel flag — the one carrier of a Cancel of a run that holds no applying lock
// (`destroying`), and none while every open row is `dispatched`, which
// [RequestCancel] does not flag (ADR-027 amend (r)(e)).
func RunCancelFlagged(ctx context.Context, db ExecQueryRower, applyID string) (bool, error) {
	var flagged bool
	if err := db.QueryRow(ctx, runCancelFlaggedSQL, applyID).Scan(&flagged); err != nil {
		return false, fmt.Errorf("applyrun: run cancel flagged: %w", err)
	}
	return flagged, nil
}
