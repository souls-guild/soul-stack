package voyage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// voyageTargetsTable / voyageTargetsColumns — table and column order for
// CopyFrom insert of run units. Match former per-row INSERT (same set and
// order); serial-PK not in set — target_id composite from known data, CopyFrom
// does not return rows and they are not needed.
var (
	voyageTargetsTable   = pgx.Identifier{"voyage_targets"}
	voyageTargetsColumns = []string{"voyage_id", "target_kind", "target_id", "batch_index", "status"}
)

// InsertTargets inserts run units (Leg split) of one Voyage in
// [TargetStatusAwaiting] status. Called on Voyage creation (S5-handler /
// orchestrator pre-plan) — snapshot of targets fixed immediately after INSERT of
// `voyages` row itself (ADR-043: snapshot-scope does not «drift» between Legs).
//
// All targets must reference same voyageID, have valid TargetKind and non-empty
// TargetID. Caller must pass db = pgx.Tx if atomicity with Voyage Insert needed
// (CRUD does not open transaction itself).
//
// Insert goes via single COPY (pgx CopyFrom), not per-row INSERT loop (S-med-3):
// Voyage scope can reach [config.DefaultVoyageMaxScope] units, and N separate
// round-trips in one transaction would hit INSERT-rate limit. Atomicity preserved
// — CopyFrom goes via same tx as Voyage Insert. Validation (same voyageID / valid
// TargetKind / non-empty TargetID / non-negative BatchIndex) happens BEFORE COPY:
// invalid target → error without write.
func InsertTargets(ctx context.Context, db ExecQueryRower, voyageID string, targets []VoyageTarget) error {
	if voyageID == "" {
		return fmt.Errorf("voyage: empty voyage_id")
	}
	if len(targets) == 0 {
		return fmt.Errorf("voyage: empty targets")
	}
	for i := range targets {
		t := &targets[i]
		if t.VoyageID != "" && t.VoyageID != voyageID {
			return fmt.Errorf("voyage: target[%d] voyage_id %q != %q", i, t.VoyageID, voyageID)
		}
		if !ValidTargetKind(t.TargetKind) {
			return fmt.Errorf("voyage: target[%d] invalid target_kind %q", i, t.TargetKind)
		}
		if t.TargetID == "" {
			return fmt.Errorf("voyage: target[%d] empty target_id", i)
		}
		if t.BatchIndex < 0 {
			return fmt.Errorf("voyage: target[%d] negative batch_index %d", i, t.BatchIndex)
		}
	}

	src := pgx.CopyFromSlice(len(targets), func(i int) ([]any, error) {
		t := &targets[i]
		status := t.Status
		if status == "" {
			status = TargetStatusAwaiting
		}
		return []any{
			voyageID,
			string(t.TargetKind),
			t.TargetID,
			t.BatchIndex,
			string(status),
		}, nil
	})

	if _, err := db.CopyFrom(ctx, voyageTargetsTable, voyageTargetsColumns, src); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch pgErr.Code {
			case pgErrCodeUniqueViolation:
				return fmt.Errorf("voyage: duplicate target on %s: %w", pgErr.ConstraintName, err)
			case pgErrCodeForeignKeyViolation:
				return fmt.Errorf("voyage: target FK violation on %s: %w", pgErr.ConstraintName, err)
			case pgErrCodeCheckViolation:
				return fmt.Errorf("voyage: target CHECK violation on %s: %w", pgErr.ConstraintName, err)
			}
		}
		return fmt.Errorf("voyage: copy targets: %w", err)
	}
	return nil
}

const selectTargetsSQL = `
SELECT voyage_id, target_kind, target_id, batch_index, status, apply_id, errand_id, finished_at
FROM voyage_targets
WHERE voyage_id = $1
ORDER BY batch_index ASC, target_kind ASC, target_id ASC
`

// SelectTargets reads all run units of Voyage by voyage_id, sorted by
// (batch_index, target_kind, target_id) — order two-level drill for All-runs
// view (S5). Empty result — not error (Voyage without targets or foreign id;
// caller checks Voyage existence via SelectByID).
func SelectTargets(ctx context.Context, db ExecQueryRower, voyageID string) ([]VoyageTarget, error) {
	if voyageID == "" {
		return nil, fmt.Errorf("voyage: empty voyage_id")
	}
	rows, err := db.Query(ctx, selectTargetsSQL, voyageID)
	if err != nil {
		return nil, fmt.Errorf("voyage: select targets: %w", err)
	}
	defer rows.Close()

	var out []VoyageTarget
	for rows.Next() {
		t, scanErr := scanTarget(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("voyage: select targets scan: %w", scanErr)
		}
		out = append(out, *t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("voyage: select targets iter: %w", err)
	}
	return out, nil
}

// updateTargetRunningApplySQL — back-link to child scenario-run
// (kind=incarnation, S2): write apply_id. JOIN on voyages.attempt fences CAS per
// capture epoch (see [MarkTargetRunning]).
const updateTargetRunningApplySQL = `
UPDATE voyage_targets AS vt
SET status   = 'running',
    apply_id = $4
FROM voyages AS v
WHERE vt.voyage_id   = $1
  AND vt.target_kind = $2
  AND vt.target_id   = $3
  AND vt.status       = 'awaiting'
  AND v.voyage_id     = vt.voyage_id
  AND v.attempt       = $5
`

// updateTargetRunningErrandSQL — back-link to child Errand (kind=sid, S3):
// write errand_id. Column differs from scenario variant because voyage_targets
// carries separate nullable apply_id / errand_id (migration 059): same
// awaiting→running transition, but different back-link column per target_kind.
// JOIN on voyages.attempt fences CAS per capture epoch (see [MarkTargetRunning]).
const updateTargetRunningErrandSQL = `
UPDATE voyage_targets AS vt
SET status    = 'running',
    errand_id = $4
FROM voyages AS v
WHERE vt.voyage_id   = $1
  AND vt.target_kind = $2
  AND vt.target_id   = $3
  AND vt.status       = 'awaiting'
  AND v.voyage_id     = vt.voyage_id
  AND v.attempt       = $5
`

// MarkTargetRunning transitions run unit awaiting→running and sets back-link to
// child run. Per target_kind selects back-link column:
//   - [TargetKindIncarnation] → apply_id (per-incarnation scenario-run, S2);
//   - [TargetKindSID]         → errand_id (per-host Errand, S3).
//
// backlinkID — applyID (scenario) or errandID (command). attempt — worker claim
// epoch (voyages.attempt from ClaimNext). Called by orchestrator immediately
// after child run spawn, BEFORE awaiting its terminal, so All-runs view (S5)
// shows «in progress» with correct drill.
//
// WHERE narrowed to status='awaiting' + JOIN voyages.attempt=$attempt
// (idempotent + fencing guard, S-med-2): repeat call after failover-reclaim —
// no-op RowsAffected=0 (target already running OR attempt shifted on reclaim).
// This deterministically distinguishes «own running» from orphaned: worker of
// prior claim-epoch (attempt=N) will not overwrite running after reclaim
// (voyages.attempt=N+1). PG CRUD error raised to caller; «no rows» — caller
// treats as already-processed (recovery), not fatal.
func MarkTargetRunning(ctx context.Context, db ExecQueryRower, voyageID string, kind TargetKind, targetID, backlinkID string, attempt int) error {
	if voyageID == "" {
		return fmt.Errorf("voyage: empty voyage_id")
	}
	if !ValidTargetKind(kind) {
		return fmt.Errorf("voyage: invalid target_kind %q", kind)
	}
	if targetID == "" {
		return fmt.Errorf("voyage: empty target_id")
	}
	if backlinkID == "" {
		return fmt.Errorf("voyage: empty back-link id")
	}
	sql := updateTargetRunningApplySQL
	if kind == TargetKindSID {
		sql = updateTargetRunningErrandSQL
	}
	if _, err := db.Exec(ctx, sql, voyageID, string(kind), targetID, backlinkID, attempt); err != nil {
		return fmt.Errorf("voyage: mark target running (%s/%s): %w", kind, targetID, err)
	}
	return nil
}

const updateTargetTerminalSQL = `
UPDATE voyage_targets
SET status      = $4,
    finished_at = NOW()
WHERE voyage_id   = $1
  AND target_kind = $2
  AND target_id   = $3
  AND status NOT IN ('succeeded', 'failed', 'cancelled', 'no_match')
`

// MarkTargetTerminal fixes terminal of run unit (succeeded / failed /
// cancelled / no_match) + finished_at = NOW(). Called by orchestrator after
// child run of target reaches terminal.
//
// WHERE excludes already-terminal rows (idempotent guard, parity
// MarkTargetRunning): repeat finalize after failover — no-op RowsAffected=0.
// status must be terminal TargetStatus; awaiting/running → [ErrInvalidStatus]
// (caller programming error).
func MarkTargetTerminal(ctx context.Context, db ExecQueryRower, voyageID string, kind TargetKind, targetID string, status TargetStatus) error {
	if voyageID == "" {
		return fmt.Errorf("voyage: empty voyage_id")
	}
	if !ValidTargetKind(kind) {
		return fmt.Errorf("voyage: invalid target_kind %q", kind)
	}
	if targetID == "" {
		return fmt.Errorf("voyage: empty target_id")
	}
	if !isTerminalTargetStatus(status) {
		return fmt.Errorf("%w: target status %q is not terminal", ErrInvalidStatus, status)
	}
	if _, err := db.Exec(ctx, updateTargetTerminalSQL, voyageID, string(kind), targetID, string(status)); err != nil {
		return fmt.Errorf("voyage: mark target terminal (%s/%s=%s): %w", kind, targetID, status, err)
	}
	return nil
}

// isTerminalTargetStatus — terminal subset of [TargetStatus] (succeeded /
// failed / cancelled / no_match). awaiting/running — not terminal.
func isTerminalTargetStatus(s TargetStatus) bool {
	switch s {
	case TargetStatusSucceeded, TargetStatusFailed, TargetStatusCancelled, TargetStatusNoMatch:
		return true
	}
	return false
}

func scanTarget(row pgx.Row) (*VoyageTarget, error) {
	var (
		t          VoyageTarget
		kindStr    string
		statusStr  string
		applyID    *string
		errandID   *string
		finishedAt *time.Time
	)
	if err := row.Scan(
		&t.VoyageID,
		&kindStr,
		&t.TargetID,
		&t.BatchIndex,
		&statusStr,
		&applyID,
		&errandID,
		&finishedAt,
	); err != nil {
		return nil, err
	}
	t.TargetKind = TargetKind(kindStr)
	t.Status = TargetStatus(statusStr)
	t.ApplyID = applyID
	t.ErrandID = errandID
	t.FinishedAt = finishedAt
	return &t, nil
}
