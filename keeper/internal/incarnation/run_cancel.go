package incarnation

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// RequestRunCancel flags a cluster-wide Cancel of run applyID on the applying
// lock it holds (ADR-027 amend (r), migration 121). Any Keeper instance may
// call it. The flag on `apply_runs` rows reaches only a row that is not yet
// terminal, and a run has none before its first row and between one step's
// barrier and the next step's rows; the lock is held for the whole run.
//
// Returns false when no lock names the run: it is over, it holds `destroying`
// (which carries no epoch), or its lock predates the epoch.
func RequestRunCancel(ctx context.Context, db ExecQueryRower, applyID string) (bool, error) {
	if applyID == "" {
		return false, fmt.Errorf("incarnation: empty apply_id")
	}
	const sql = `
UPDATE incarnation
SET applying_cancel_requested = true
WHERE applying_apply_id = $1 AND status = 'applying'
`
	tag, err := db.Exec(ctx, sql, applyID)
	if err != nil {
		return false, fmt.Errorf("incarnation: request run cancel: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// RunCancelRequested reports whether [RequestRunCancel] flagged the lock run
// applyID holds. False when no lock names the run.
func RunCancelRequested(ctx context.Context, db ExecQueryRower, applyID string) (bool, error) {
	const sql = `
SELECT applying_cancel_requested
FROM incarnation
WHERE applying_apply_id = $1 AND status = 'applying'
`
	var requested bool
	if err := db.QueryRow(ctx, sql, applyID).Scan(&requested); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("incarnation: read run cancel: %w", err)
	}
	return requested, nil
}
