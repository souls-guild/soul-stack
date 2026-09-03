package incarnation

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// CaptureSpec addresses one mid-run state capture ([ADR-0084]): which
// incarnation is written, and which run gets the credit in `state_history`.
type CaptureSpec struct {
	ID           string
	Scenario     string
	ApplyID      string
	HistoryID    string
	ChangedByAID *string
}

// SelectStateForUpdate reads `incarnation.state` and locks the row for the rest
// of the transaction. For a read that does not write the row back, use
// [SelectByID] — this one holds a write lock until the transaction ends.
func SelectStateForUpdate(ctx context.Context, tx ExecQueryRower, id string) (map[string]any, error) {
	const sql = `SELECT state FROM incarnation WHERE id = $1 FOR UPDATE`
	var b []byte
	if err := tx.QueryRow(ctx, sql, id).Scan(&b); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrIncarnationNotFound
		}
		return nil, fmt.Errorf("incarnation: select state for update: %w", err)
	}
	state, err := unmarshalJSONB(b)
	if err != nil {
		return nil, fmt.Errorf("incarnation: unmarshal state: %w", err)
	}
	return state, nil
}

// SelectState reads `incarnation.state` without locking the row — the unlocked
// read half of mid-run capture ([ADR-0084]), with two callers: the runner
// refreshing what `incarnation.state` means in a CEL expression at a Passage
// boundary, and `core.state.set` asking whether a field already has a value
// before it mints a secret the write would discard.
//
// Unlocked is safe for both because the row is already held `applying` by the run
// doing the reading, so the only writer is that run's own [CaptureState] — which
// re-reads under FOR UPDATE and re-decides there. A stale read here can waste
// work, never produce a wrong write.
func SelectState(ctx context.Context, db ExecQueryRower, id string) (map[string]any, error) {
	const sql = `SELECT state FROM incarnation WHERE id = $1`
	var b []byte
	if err := db.QueryRow(ctx, sql, id).Scan(&b); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrIncarnationNotFound
		}
		return nil, fmt.Errorf("incarnation: select state: %w", err)
	}
	state, err := unmarshalJSONB(b)
	if err != nil {
		return nil, fmt.Errorf("incarnation: unmarshal state: %w", err)
	}
	return state, nil
}

// CaptureState applies mutate to the incarnation's CURRENT state and commits the
// result mid-run: SELECT … FOR UPDATE → mutate → INSERT state_history → UPDATE
// state. Returns what was committed.
//
// This is the write of a `core.state.*` capture step ([ADR-0084]), and it is
// deliberately NOT [UpdateStateFromRun]: the run is not over. The status is left
// where it is, the applying epoch is not cleared, and no run outcome is recorded
// — the history row is a snapshot of one capture, not of an attempt.
//
// The row must be in a working status (applying / destroying). A capture against
// any other status is a capture outside a run: it fails rather than writes,
// because the run that would own the change is not the one holding the row.
func CaptureState(ctx context.Context, pool TxBeginner, spec CaptureSpec, mutate func(map[string]any) (map[string]any, error)) (map[string]any, error) {
	if !ValidID(spec.ID) {
		return nil, fmt.Errorf("incarnation: invalid id %q", spec.ID)
	}
	if spec.ApplyID == "" {
		return nil, fmt.Errorf("incarnation: empty apply_id")
	}
	if spec.HistoryID == "" {
		return nil, fmt.Errorf("incarnation: empty history_id")
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("incarnation: begin capture tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const selectForUpdateSQL = `
SELECT state, status
FROM incarnation
WHERE id = $1
FOR UPDATE
`
	var (
		stateBytes []byte
		statusStr  string
	)
	if err := tx.QueryRow(ctx, selectForUpdateSQL, spec.ID).Scan(&stateBytes, &statusStr); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrIncarnationNotFound
		}
		return nil, fmt.Errorf("incarnation: capture select: %w", err)
	}
	if _, ok := finalizableStatuses[Status(statusStr)]; !ok {
		return nil, fmt.Errorf("%w (status=%s)", ErrAlreadyFinalized, statusStr)
	}
	before, err := unmarshalJSONB(stateBytes)
	if err != nil {
		return nil, fmt.Errorf("incarnation: unmarshal state: %w", err)
	}

	after, err := mutate(before)
	if err != nil {
		return nil, err
	}
	beforeBytes, err := marshalJSONB(before)
	if err != nil {
		return nil, fmt.Errorf("incarnation: marshal state_before: %w", err)
	}
	afterBytes, err := marshalJSONB(after)
	if err != nil {
		return nil, fmt.Errorf("incarnation: marshal state_after: %w", err)
	}
	var changedByArg any
	if spec.ChangedByAID != nil {
		changedByArg = *spec.ChangedByAID
	}

	const historySQL = `
INSERT INTO state_history (
    history_id, incarnation_name, scenario, state_before, state_after,
    changed_by_aid, apply_id
) VALUES ($1, $2, $3, $4, $5, $6, $7)
`
	if _, err := tx.Exec(ctx, historySQL,
		spec.HistoryID, spec.ID, spec.Scenario, beforeBytes, afterBytes, changedByArg, spec.ApplyID,
	); err != nil {
		return nil, fmt.Errorf("incarnation: insert state_history: %w", err)
	}

	const updateSQL = `UPDATE incarnation SET state = $2, updated_at = NOW() WHERE id = $1`
	if _, err := tx.Exec(ctx, updateSQL, spec.ID, afterBytes); err != nil {
		return nil, fmt.Errorf("incarnation: update state: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("incarnation: commit capture: %w", err)
	}
	return after, nil
}
