package applyrun

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/souls-guild/soul-stack/keeper/internal/pgutil"
)

// Sentinel errors of the CRUD layer.
//   - ErrApplyRunAlreadyExists — UNIQUE on composite PK (apply_id, sid): a
//     repeat Insert of the same pair (a scenario-runner programming error).
//   - ErrApplyRunNotFound      — no row for the requested key.
//   - ErrApplyRunNotClaimed    — [MarkDispatched] called on a row that is
//     not in `claimed` status (already dispatched / planned / terminal, or missing).
var (
	ErrApplyRunAlreadyExists = errors.New("applyrun: (apply_id, sid) already exists")
	ErrApplyRunNotFound      = errors.New("applyrun: not found")
	ErrApplyRunNotClaimed    = errors.New("applyrun: row is not in claimed state")
	// ErrApplyRunAlreadyTerminal — append-only single-winner terminal
	// (ADR-027(j), amend GATE-1): [UpdateStatus] called on a row already in a
	// terminal status (success/failed/cancelled). A terminal is NOT
	// overwritten by another execution's terminal (original RunResult vs a
	// recovery race) — first one wins. NOT an error: the caller treats it as
	// a no-op (logs it, doesn't fail the barrier).
	ErrApplyRunAlreadyTerminal = errors.New("applyrun: row is already in a terminal status")
)

const (
	pgErrCodeUniqueViolation     = "23505"
	pgErrCodeForeignKeyViolation = "23503"
	pgErrCodeCheckViolation      = "23514"
)

// ExecQueryRower — the narrow subset of the pgxpool.Pool interface the CRUD
// layer needs. Symmetric with [incarnation.ExecQueryRower] /
// [operator.ExecQueryRower]: unit tests go through a fake without spinning up
// PG, production gets a real pool / Conn / Tx.
type ExecQueryRower interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Compile-time check.
var (
	_ ExecQueryRower = (*pgx.Conn)(nil)
	_ ExecQueryRower = (*pgxpool.Pool)(nil)
	_ ExecQueryRower = (pgx.Tx)(nil)
)

const insertSQL = `
INSERT INTO apply_runs (
    apply_id, sid, incarnation_name, scenario, task_idx, status,
    error_summary, started_by_aid, passage
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING started_at
`

// Insert inserts a run row. [StatusRunning] survives here ONLY for the old
// synchronous path (dispatchWave with acolytes:0): it renders and sends
// `ApplyRequest` right away, so it writes the row as `running`. The Acolyte
// path doesn't go through here — it writes `planned` via [InsertPlanned],
// then moves `claimed → dispatched` ([MarkDispatched]) before SendApply.
//
// Pre-conditions: non-empty ApplyID / SID / IncarnationName / Scenario;
// valid Status.
//
// Returns:
//   - [ErrApplyRunAlreadyExists] on UNIQUE violation of the PK.
//   - wrapped fmt.Errorf on FK violation (incarnation_name / started_by_aid
//     reference a non-existent row) and CHECK violation (status).
func Insert(ctx context.Context, db ExecQueryRower, run *ApplyRun) error {
	if run == nil {
		return fmt.Errorf("applyrun: nil run")
	}
	if run.ApplyID == "" {
		return fmt.Errorf("applyrun: empty apply_id")
	}
	if run.SID == "" {
		return fmt.Errorf("applyrun: empty sid")
	}
	if run.IncarnationName == "" {
		return fmt.Errorf("applyrun: empty incarnation_name")
	}
	if run.Scenario == "" {
		return fmt.Errorf("applyrun: empty scenario")
	}
	if !ValidStatus(run.Status) {
		return fmt.Errorf("applyrun: invalid status %q", run.Status)
	}

	var taskIdxArg any
	if run.TaskIdx != nil {
		taskIdxArg = *run.TaskIdx
	}
	var errorSummaryArg any
	if run.ErrorSummary != nil {
		errorSummaryArg = *run.ErrorSummary
	}
	var startedByArg any
	if run.StartedByAID != nil {
		startedByArg = *run.StartedByAID
	}

	row := db.QueryRow(ctx, insertSQL,
		run.ApplyID,
		run.SID,
		run.IncarnationName,
		run.Scenario,
		taskIdxArg,
		string(run.Status),
		errorSummaryArg,
		startedByArg,
		run.Passage,
	)
	if err := row.Scan(&run.StartedAt); err != nil {
		return mapInsertError(err)
	}
	return nil
}

const insertPlannedSQL = `
INSERT INTO apply_runs (
    apply_id, sid, incarnation_name, scenario, status, started_by_aid, recipe
) VALUES ($1, $2, $3, $4, 'planned', $5, $6)
RETURNING started_at
`

// InsertPlanned inserts a planned task awaiting Acolyte-claim (ADR-027,
// Phase 1.4.2): a row in `planned` status with a persisted [Recipe] (render
// instructions, recipe column from migration 029). attempt stays DEFAULT 0 —
// [ClaimNext] increments the fencing epoch on claim. task_idx/error_summary
// are not written (no task yet at dispatch time), Ward-claim columns are
// NULL until claim.
//
// Difference from [Insert]: that one writes `running` immediately (old path,
// dispatch renders and sends ApplyRequest synchronously); InsertPlanned
// writes `planned` — render/SendApply are deferred to the Acolyte at claim
// time. Invariant A (ADR-027): the recipe carries the vault ref AS-IS,
// secrets never land in PG.
//
// Pre-conditions: non-empty ApplyID / SID / IncarnationName / Scenario;
// non-nil run.Recipe (an Acolyte can't render a planned task without one).
//
// Returns:
//   - [ErrApplyRunAlreadyExists] on UNIQUE violation of the PK.
//   - wrapped fmt.Errorf on FK violation / recipe marshal failure.
func InsertPlanned(ctx context.Context, db ExecQueryRower, run *ApplyRun) error {
	if run == nil {
		return fmt.Errorf("applyrun: nil run")
	}
	if run.ApplyID == "" {
		return fmt.Errorf("applyrun: empty apply_id")
	}
	if run.SID == "" {
		return fmt.Errorf("applyrun: empty sid")
	}
	if run.IncarnationName == "" {
		return fmt.Errorf("applyrun: empty incarnation_name")
	}
	if run.Scenario == "" {
		return fmt.Errorf("applyrun: empty scenario")
	}
	if run.Recipe == nil {
		return fmt.Errorf("applyrun: planned task without recipe")
	}

	recipeJSON, err := MarshalRecipe(run.Recipe)
	if err != nil {
		return err
	}

	var startedByArg any
	if run.StartedByAID != nil {
		startedByArg = *run.StartedByAID
	}

	row := db.QueryRow(ctx, insertPlannedSQL,
		run.ApplyID,
		run.SID,
		run.IncarnationName,
		run.Scenario,
		startedByArg,
		recipeJSON,
	)
	if err := row.Scan(&run.StartedAt); err != nil {
		return mapInsertError(err)
	}
	run.Status = StatusPlanned
	return nil
}

func mapInsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeUniqueViolation:
			return fmt.Errorf("%w (constraint %s): %w",
				ErrApplyRunAlreadyExists, pgErr.ConstraintName, err)
		case pgErrCodeForeignKeyViolation:
			return fmt.Errorf("applyrun: FK violation on %s: %w", pgErr.ConstraintName, err)
		case pgErrCodeCheckViolation:
			return fmt.Errorf("applyrun: CHECK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("applyrun: insert: %w", err)
}

// updateStatusSQL moves a row to a terminal (or other) status with an
// append-only single-winner guard (ADR-027(j), amend GATE-1): filtering on
// the source status `status IN ('planned','claimed','running','dispatched')`
// guarantees a terminal is NOT overwritten by another execution's terminal —
// a transition to terminal is possible ONLY from a non-terminal state.
// `dispatched` is part of the guard (ADR-027 amend S3): after MarkDispatched
// a row is `dispatched` by the time RunResult arrives, and the terminal must
// commit from that state (dispatched → success/failed/cancelled). An
// already-terminal row → RowsAffected==0 (first one won); [UpdateStatus]
// distinguishes this from not-found with a status probe.
//
// `finished_at = NOW()` is set on a transition to any status except
// `running` (a terminal is stamped with the actual completion time); for
// `running` it stays NULL. error_summary uses COALESCE: a NULL argument
// doesn't clear an already-recorded value.
//
// WHERE on (apply_id, sid, passage): staged-render (ADR-056, S3) writes N
// rows per host (one per Passage), the PK is a triple since migration 078. A
// terminal is addressed by the passage echoed in RunResult.passage
// (correlateRunResult). An N=1 run carries passage=0 on every row and every
// terminal — WHERE hits exactly the same single row as before staged-render
// (bit-for-bit).
const updateStatusSQL = `
UPDATE apply_runs
SET status        = $3,
    error_summary = COALESCE($4, error_summary),
    finished_at   = CASE WHEN $3 = 'running' THEN finished_at ELSE NOW() END
WHERE apply_id = $1 AND sid = $2 AND passage = $5
  AND status IN ('planned', 'claimed', 'running', 'dispatched')
`

const probeStatusSQL = `
SELECT status FROM apply_runs WHERE apply_id = $1 AND sid = $2 AND passage = $3
`

// UpdateStatus moves row `(applyID, sid)` to a new status. On a terminal
// status (anything but [StatusRunning]) it sets `finished_at = NOW()`.
// errorSummary != nil is written to error_summary (nil doesn't clear an
// existing value).
//
// Append-only single-winner (ADR-027(j)): the transition is allowed ONLY
// from a non-terminal source status (planned/claimed/running/dispatched). A
// terminal is not overwritten by another terminal — protects against a race
// between the original RunResult and a recovery race
// ([correlateRunResult] / the barrier classifier).
//
// passage addresses the Passage row of staged-render (ADR-056): a terminal
// arrives for row (apply_id, sid, passage). An N=1 run calls with passage=0
// (the host's only row) — bit-for-bit behavior.
//
// Returns:
//   - [ErrApplyRunNotFound]        — no row at all.
//   - [ErrApplyRunAlreadyTerminal] — the row exists but is already terminal
//     (the first committer won): the caller treats it as a no-op (logs it,
//     doesn't fail the barrier), NOT a consistency error.
func UpdateStatus(ctx context.Context, db ExecQueryRower, applyID, sid string, passage int, status Status, errorSummary *string) error {
	if applyID == "" {
		return fmt.Errorf("applyrun: empty apply_id")
	}
	if sid == "" {
		return fmt.Errorf("applyrun: empty sid")
	}
	if !ValidStatus(status) {
		return fmt.Errorf("applyrun: invalid status %q", status)
	}

	var errorSummaryArg any
	if errorSummary != nil {
		errorSummaryArg = *errorSummary
	}

	tag, err := db.Exec(ctx, updateStatusSQL, applyID, sid, string(status), errorSummaryArg, passage)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgErrCodeCheckViolation {
			return fmt.Errorf("applyrun: CHECK violation on %s: %w", pgErr.ConstraintName, err)
		}
		return fmt.Errorf("applyrun: update status: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}

	// 0 rows: either the row doesn't exist, or it's already terminal
	// (the append-only guard blocked the overwrite). Disambiguate with a
	// status probe (same pattern as MarkDispatched).
	var statusStr string
	if perr := db.QueryRow(ctx, probeStatusSQL, applyID, sid, passage).Scan(&statusStr); perr != nil {
		if errors.Is(perr, pgx.ErrNoRows) {
			return ErrApplyRunNotFound
		}
		return fmt.Errorf("applyrun: update status probe: %w", perr)
	}
	return fmt.Errorf("%w (status=%s)", ErrApplyRunAlreadyTerminal, statusStr)
}

const selectByApplyIDSQL = `
SELECT apply_id, sid, incarnation_name, scenario, task_idx, status,
       error_summary, started_at, finished_at, started_by_aid,
       claim_by_kid, claim_at, claim_expires_at, attempt, recipe
FROM apply_runs
WHERE apply_id = $1 AND sid = $2
`

// SelectByApplyID reads a run row by composite PK, including the Ward-claim
// columns (claim_by_kid/claim_at/claim_expires_at/attempt, migration 025)
// and recipe (migration 029). [ErrApplyRunNotFound] on pgx.ErrNoRows.
func SelectByApplyID(ctx context.Context, db ExecQueryRower, applyID, sid string) (*ApplyRun, error) {
	row := db.QueryRow(ctx, selectByApplyIDSQL, applyID, sid)
	var (
		run          ApplyRun
		statusStr    string
		taskIdx      *int
		errorSummary *string
		startedBy    *string
		recipeJSON   []byte
	)
	err := row.Scan(
		&run.ApplyID,
		&run.SID,
		&run.IncarnationName,
		&run.Scenario,
		&taskIdx,
		&statusStr,
		&errorSummary,
		&run.StartedAt,
		&run.FinishedAt,
		&startedBy,
		&run.ClaimByKID,
		&run.ClaimAt,
		&run.ClaimExpiresAt,
		&run.Attempt,
		&recipeJSON,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrApplyRunNotFound
		}
		return nil, fmt.Errorf("applyrun: scan: %w", err)
	}
	run.Status = Status(statusStr)
	run.TaskIdx = taskIdx
	run.ErrorSummary = errorSummary
	run.StartedByAID = startedBy
	recipe, err := UnmarshalRecipe(recipeJSON)
	if err != nil {
		return nil, err
	}
	run.Recipe = recipe
	return &run, nil
}

// recordTaskFailureSQL records data for a host's first failed task (BUG-3):
// task_idx / failed_plan_index and error_summary are written only if not
// already set (COALESCE with the stored value → first-failure-wins). The row
// status is left alone — the terminal is stamped by RunResult
// ([UpdateStatus]); until then the row stays `running`, but already carries
// the failure reason for later aggregation.
//
// failed_plan_index ($6) — the GLOBAL end-to-end plan_index of the failed
// task (across the whole plan, all Passages; ADR-056 §S1 fix Variant B).
// This is the correlation key to RenderedTask.Index for the failed task's
// module/action (drift report) and no_log suppression (barrier). task_idx
// ($3) — the LOCAL position within its own Passage's ApplyRequest
// (informational). COALESCE on BOTH fields under one first-failure
// condition: they describe the same first failed task, written atomically.
//
// WHERE on (apply_id, sid, passage): staged-render (ADR-056) — a failed task
// belongs to a specific Passage, so we write the reason to its row. passage
// comes from the TaskEvent.passage echo. N=1 → passage=0 (the host's only
// row), bit-for-bit.
const recordTaskFailureSQL = `
UPDATE apply_runs
SET task_idx          = COALESCE(task_idx, $3),
    error_summary     = COALESCE(error_summary, $4),
    failed_plan_index = COALESCE(failed_plan_index, $6)
WHERE apply_id = $1 AND sid = $2 AND passage = $5
`

// RecordTaskFailure records the local and global index and a short
// description of a host's first failed task into row `(applyID, sid,
// passage)`. Idempotent via first-failure-wins: a repeat call (a second
// failed task / retry) doesn't clear the already-recorded
// task_idx/failed_plan_index/error_summary (COALESCE). Called by the
// RunResult pipeline from handleTaskEvent on a TaskEvent with status
// FAILED/TIMED_OUT — so the failure reason survives cross-Keeper routing
// (TaskEvent and the run goroutine can live on different instances,
// ADR-002) through to aggregation in the run's error_summary.
//
// taskIdx — the LOCAL position of the failed task in its Passage's
// ApplyRequest.tasks[] (TaskEvent.task_idx echo, informational for triage).
// planIndex — the GLOBAL end-to-end plan_index across the whole plan
// (TaskEvent.plan_index echo, = RenderedTask.Index): the correlation key to
// plan metadata (ADR-056 §S1 fix Variant B). They differ under
// staged/per-host-where; N=1 → planIndex==taskIdx.
//
// summary — already composed and passed through MaskSecrets
// (`task <idx> <module>: <message>`); the CRUD layer doesn't interpret it.
//
// passage addresses the Passage row of staged-render (ADR-056); N=1 → 0.
//
// Returns [ErrApplyRunNotFound] if the row doesn't exist (TaskEvent beat
// Insert, or an ad-hoc push without a scenario-runner).
func RecordTaskFailure(ctx context.Context, db ExecQueryRower, applyID, sid string, passage, taskIdx, planIndex int, summary string) error {
	if applyID == "" {
		return fmt.Errorf("applyrun: empty apply_id")
	}
	if sid == "" {
		return fmt.Errorf("applyrun: empty sid")
	}
	if taskIdx < 0 {
		return fmt.Errorf("applyrun: negative task_idx %d", taskIdx)
	}
	if planIndex < 0 {
		return fmt.Errorf("applyrun: negative plan_index %d", planIndex)
	}

	tag, err := db.Exec(ctx, recordTaskFailureSQL, applyID, sid, taskIdx, summary, passage, planIndex)
	if err != nil {
		return fmt.Errorf("applyrun: record task failure: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrApplyRunNotFound
	}
	return nil
}

const selectStatusesByApplyIDSQL = `
SELECT sid, status, task_idx, failed_plan_index, error_summary, cancel_requested, passage
FROM apply_runs
WHERE apply_id = $1
ORDER BY sid ASC, passage ASC
`

// HostStatus — a narrow projection of an apply_runs row for
// scenario-runner's fan-in poll: scenario-runner polls
// [SelectStatusesByApplyID] until every SID of the run reaches a terminal
// status.
//
// TaskIdx — the LOCAL position of the failed task within its Passage's
// ApplyRequest (filled by [RecordTaskFailure] on a failed host; nil on
// success / still-running / a dispatch-level failure with no TaskEvent).
// Informational (triage); NOT the correlation key against the global plan
// under staged/per-host-where — see FailedPlanIndex.
//
// FailedPlanIndex — the GLOBAL end-to-end plan_index of the failed task
// across the whole plan (all Passages; migration 081, ADR-056 §S1 fix
// Variant B). The correlation key to RenderedTask.Index for the failed
// task's module/action (drift report) and no_log suppression (barrier). nil
// under the same conditions as TaskIdx; N=1 → ==TaskIdx.
//
// CancelRequested — the cluster-wide Cancel flag (G1, migration 024): any
// Keeper sets it via [RequestCancel] on Cancel; the owning run goroutine
// sees it on the next barrier tick and cancels the run. The flag is the same
// for every row of a run (RequestCancel writes by apply_id), but the
// projection carries it per-host — the barrier only needs to see true on any
// one row.
type HostStatus struct {
	SID             string
	Status          Status
	TaskIdx         *int
	FailedPlanIndex *int
	ErrorSummary    *string
	CancelRequested bool

	// Passage — the row's staged-render Passage index (ADR-056, S3). Each
	// Passage's barrier counts terminals within its own slice (passage==N).
	// An N=1 run carries passage=0 on every row — the slice matches all of
	// the host's rows, and the barrier behaves bit-for-bit as before
	// staged-render.
	Passage int
}

// SelectStatusesByApplyID returns statuses for every host of one run (one
// `apply_id`, distinct `sid`s), sorted by SID. Used by scenario-runner for
// cross-host barrier fan-in (poll until every host is terminal,
// orchestration.md §7). The `apply_runs_apply_idx` index (migration 018)
// covers the query. Carries `cancel_requested` (G1) in the same query — the
// barrier reads the cancel flag without a second round trip.
//
// An empty result means an apply with not a single row (a scenario-runner
// programming error: polling before Insert); the caller treats it as
// "nothing dispatched yet".
func SelectStatusesByApplyID(ctx context.Context, db ExecQueryRower, applyID string) ([]HostStatus, error) {
	rows, err := db.Query(ctx, selectStatusesByApplyIDSQL, applyID)
	if err != nil {
		return nil, fmt.Errorf("applyrun: statuses query: %w", err)
	}
	defer rows.Close()

	var out []HostStatus
	for rows.Next() {
		var (
			hs        HostStatus
			statusStr string
		)
		if err := rows.Scan(&hs.SID, &statusStr, &hs.TaskIdx, &hs.FailedPlanIndex, &hs.ErrorSummary, &hs.CancelRequested, &hs.Passage); err != nil {
			return nil, fmt.Errorf("applyrun: statuses scan: %w", err)
		}
		hs.Status = Status(statusStr)
		out = append(out, hs)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("applyrun: statuses iter: %w", err)
	}
	return out, nil
}

const requestCancelSQL = `
UPDATE apply_runs
SET cancel_requested = true
WHERE apply_id = $1 AND status IN ('planned', 'claimed', 'running')
`

// RequestCancel sets the cluster-wide Cancel flag (G1) on ALL still-
// non-terminal rows of run `apply_id` (planned/claimed/running). Any Keeper
// instance can call it; the instance holding the run goroutine sees the flag
// on its next barrier poll ([SelectStatusesByApplyID]) and cancels the run
// through the same path as a local Cancel.
//
// The filter includes planned/claimed (ADR-027 cutover, minor fix "Cancel in
// the planned/claimed window"): cancelling BEFORE SendApply is safe — the
// Acolyte checks the flag before sending ApplyRequest
// ([ClaimRunner.execute] → [SelectCancelRequested]) and skips the apply if
// Cancel was requested. Without this, a Cancel in the planned window
// (between dispatch and claim) would touch no rows and the run would still
// go out to the Souls.
//
// Idempotent: terminal rows (success/failed/cancelled) are excluded —
// cancelling an already-finished run touches no rows. A repeat call on a
// still-non-terminal run just sets true to true again. Returns affected —
// the number of touched rows (0 → the run doesn't exist or is already
// terminal: the caller treats it as a no-op).
func RequestCancel(ctx context.Context, db ExecQueryRower, applyID string) (int64, error) {
	if applyID == "" {
		return 0, fmt.Errorf("applyrun: empty apply_id")
	}
	tag, err := db.Exec(ctx, requestCancelSQL, applyID)
	if err != nil {
		return 0, fmt.Errorf("applyrun: request cancel: %w", err)
	}
	return tag.RowsAffected(), nil
}

const selectCancelRequestedSQL = `
SELECT cancel_requested
FROM apply_runs
WHERE apply_id = $1 AND sid = $2
`

// SelectCancelRequested reads the current `cancel_requested` flag for row
// `(applyID, sid)`. The Acolyte calls it in claim-execute BEFORE SendApply
// (ADR-027 cutover): if Cancel was requested between claim and send, the
// apply does NOT go out to the Soul. A separate narrow read (rather than the
// flag from [ClaimNext]'s RETURNING) — because the claim→SendApply window is
// wider than the claim transaction: the flag could have been set after the
// Ward was already claimed.
//
// Returns [ErrApplyRunNotFound] if the row doesn't exist.
func SelectCancelRequested(ctx context.Context, db ExecQueryRower, applyID, sid string) (bool, error) {
	var cancelRequested bool
	err := db.QueryRow(ctx, selectCancelRequestedSQL, applyID, sid).Scan(&cancelRequested)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrApplyRunNotFound
		}
		return false, fmt.Errorf("applyrun: select cancel_requested: %w", err)
	}
	return cancelRequested, nil
}

// claimNextSQL — atomically claims a batch of planned tasks (Ward-claim,
// ADR-027(d)). A work-queue idiom: the inner SELECT … FOR UPDATE SKIP LOCKED
// locks the chosen planned rows and SKIPS rows already locked by
// competitors — two Acolytes on different instances never claim the same
// row. The outer UPDATE moves them to `claimed`, stamps the owner/lease, and
// increments `attempt` (fencing epoch, ADR-027(g)). RETURNING hands the
// claimed rows back to this exact Acolyte.
//
//	$1 claim_by_kid   — KID of the claiming Acolyte
//	$2 lease          — interval until claim_expires_at (NOW() + $2)
//	$3 batch          — LIMIT of the claimed batch
//
// passage-aware claim — S3: claim by `(apply_id, sid)` is unique as long as
// every row carries passage=0 (S1/S2: stratification computes passage
// statically, but the stage loop that writes passage>0 apply_runs rows is
// S3). Once staged-render starts laying down N rows per host (one per
// Passage), claim will become passage-aware (claim only rows of the
// currently active Passage — a task becomes `(apply_id, sid, passage)`,
// ADR-056/ADR-027 amend). Symmetric with updateStatusSQL.
const claimNextSQL = `
UPDATE apply_runs AS r
SET status           = 'claimed',
    claim_by_kid     = $1,
    claim_at         = NOW(),
    claim_expires_at = NOW() + $2::interval,
    attempt          = r.attempt + 1
WHERE (r.apply_id, r.sid) IN (
    SELECT c.apply_id, c.sid
    FROM apply_runs AS c
    WHERE c.status = 'planned'
    ORDER BY c.started_at ASC
    FOR UPDATE SKIP LOCKED
    LIMIT $3
)
RETURNING apply_id, sid, incarnation_name, scenario, task_idx, status,
          error_summary, started_at, finished_at, started_by_aid,
          claim_by_kid, claim_at, claim_expires_at, attempt, recipe
`

// ClaimNext atomically claims up to batch planned tasks for Acolyte kid,
// moving them planned → claimed: stamps claim_by_kid/claim_at,
// claim_expires_at = NOW()+lease, and increments attempt (fencing epoch).
// Race-freedom between competing Acolytes on different instances is
// guaranteed by `FOR UPDATE SKIP LOCKED` (locked rows are skipped). FIFO by
// started_at.
//
// Returns the claimed rows (already in `claimed` status, attempt
// incremented). An empty slice (not an error) means no planned tasks exist,
// or they were all already claimed by competitors.
func ClaimNext(ctx context.Context, db ExecQueryRower, kid string, lease time.Duration, batch int) ([]*ApplyRun, error) {
	if kid == "" {
		return nil, fmt.Errorf("applyrun: empty kid")
	}
	if lease <= 0 {
		return nil, fmt.Errorf("applyrun: non-positive lease %s", lease)
	}
	if batch <= 0 {
		return nil, fmt.Errorf("applyrun: non-positive batch %d", batch)
	}

	rows, err := db.Query(ctx, claimNextSQL, kid, pgutil.Interval(lease), batch)
	if err != nil {
		return nil, fmt.Errorf("applyrun: claim next: %w", err)
	}
	defer rows.Close()

	var out []*ApplyRun
	for rows.Next() {
		run, scanErr := scanClaimedRow(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("applyrun: claim scan: %w", scanErr)
		}
		out = append(out, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("applyrun: claim iter: %w", err)
	}
	return out, nil
}

// scanClaimedRow reads a full apply_runs row (including the Ward-claim
// columns and recipe) from [claimNextSQL]'s RETURNING into *ApplyRun. recipe
// (jsonb, migration 029) is parsed into run.Recipe — the Acolyte renders the
// task from it ([RenderForHost]); NULL (the old path, if it ever reached
// claim) → nil Recipe.
func scanClaimedRow(row pgx.Row) (*ApplyRun, error) {
	var (
		run        ApplyRun
		statusStr  string
		recipeJSON []byte
	)
	if err := row.Scan(
		&run.ApplyID,
		&run.SID,
		&run.IncarnationName,
		&run.Scenario,
		&run.TaskIdx,
		&statusStr,
		&run.ErrorSummary,
		&run.StartedAt,
		&run.FinishedAt,
		&run.StartedByAID,
		&run.ClaimByKID,
		&run.ClaimAt,
		&run.ClaimExpiresAt,
		&run.Attempt,
		&recipeJSON,
	); err != nil {
		return nil, err
	}
	run.Status = Status(statusStr)
	recipe, err := UnmarshalRecipe(recipeJSON)
	if err != nil {
		return nil, err
	}
	run.Recipe = recipe
	return &run, nil
}

const markDispatchedSQL = `
UPDATE apply_runs
SET status = 'dispatched'
WHERE apply_id = $1 AND sid = $2 AND status = 'claimed'
`

// MarkDispatched moves the claimed task `(applyID, sid)` from claimed →
// dispatched — the Acolyte calls it AND COMMITS IT TO PG STRICTLY BEFORE
// SendApply (a deliver-once intent marker, ADR-027 amend S3). Once a row is
// dispatched, recovery-reclaim does NOT touch it (reclaim is scoped to
// `status='claimed'`, S4): the row is no longer "under-delivered", the run
// now belongs to the Soul, and a repeat SendApply would mean a double apply.
// This is the heart of the anti-double-apply invariant.
//
// The `status = 'claimed'` filter is a guard: the transition is possible
// ONLY from claimed; dispatched→dispatched / planned→dispatched /
// terminal→dispatched don't go through (idempotency protection against a
// repeat / racing transition). Ward columns (claim_by_kid/lease/attempt)
// are left alone — the fencing epoch was already fixed by ClaimNext and
// rides along in ApplyRequest.attempt.
//
// Returns:
//   - [ErrApplyRunNotFound]   — no row at all.
//   - [ErrApplyRunNotClaimed] — the row exists but isn't in `claimed` status.
func MarkDispatched(ctx context.Context, db ExecQueryRower, applyID, sid string) error {
	if applyID == "" {
		return fmt.Errorf("applyrun: empty apply_id")
	}
	if sid == "" {
		return fmt.Errorf("applyrun: empty sid")
	}

	tag, err := db.Exec(ctx, markDispatchedSQL, applyID, sid)
	if err != nil {
		return fmt.Errorf("applyrun: mark dispatched: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	// 0 rows affected: either the row doesn't exist, or it's not in
	// `claimed`. Disambiguate for an informative error (guard vs not-found)
	// with a single status probe.
	var statusStr string
	scanErr := db.QueryRow(ctx,
		`SELECT status FROM apply_runs WHERE apply_id = $1 AND sid = $2`,
		applyID, sid).Scan(&statusStr)
	if scanErr != nil {
		if errors.Is(scanErr, pgx.ErrNoRows) {
			return ErrApplyRunNotFound
		}
		return fmt.Errorf("applyrun: mark dispatched probe: %w", scanErr)
	}
	return fmt.Errorf("%w (status=%s)", ErrApplyRunNotClaimed, statusStr)
}

// orphanDispatchedErrorSummary — the fixed reason for the `orphaned`
// terminal. Written to error_summary of orphaned rows so triage can see it's
// a Soul-reconcile, not a run failure.
const orphanDispatchedErrorSummary = "orphaned: RunResult lost, Soul does not track apply_id"

// orphanDispatchedSQL terminalizes a SID's orphaned dispatched rows
// (Soul-reconcile, ADR-027(g), S6). Single-winner: the `status='dispatched'`
// filter guarantees a terminal does NOT overwrite another execution's
// terminal and doesn't touch non-dispatched phases; RowsAffected → how many
// were actually orphaned.
//
// epoch-fenced: a row is orphaned ONLY if its apply_id is absent from the
// set the Soul declared, `$2` (ARRAY known apply_ids). Any apply_id present
// in the set (with any attempt) protects the row — the Soul is declaring
// the run is still tracked. This also closes the attempt-mismatch case: if
// the set carries the same apply_id but with a different (higher) attempt
// (a re-claim is in progress), the row is NOT terminalized — it's safer to
// skip an orphan than to make one falsely.
//
//	$1 sid   — host whose dispatched rows we're checking
//	$2 known — ARRAY of apply_id declared alive in WardRoster (may be empty)
const orphanDispatchedSQL = `
UPDATE apply_runs
SET status        = 'orphaned',
    error_summary = $3,
    finished_at   = NOW()
WHERE sid = $1
  AND status = 'dispatched'
  AND apply_id != ALL($2)
`

// OrphanDispatched terminalizes `sid`'s dispatched rows whose apply_id the
// Soul did NOT declare alive in [WardRoster] (Soul-reconcile, ADR-027(g),
// S6). Closes the dispatched-orphan hole "both Keeper and Soul died after
// handoff": otherwise the row would stay stuck in `dispatched` forever
// (reclaim is scoped to `claimed`, and there's no Reaper dispatched-timeout).
//
// known — apply_ids the Soul declared tracked (from WardRoster.active). An
// empty set is an explicit declaration of "nothing is tracked" → ALL of the
// SID's dispatched rows are terminalized (correct: after a Soul restart, no
// in-flight run physically exists). A nil set is treated as empty.
//
// Single-winner (the `status='dispatched'` filter, RowsAffected): a race
// between the sweep and an incoming RunResult is safe — whichever moves the
// row out of `dispatched` first wins, the other sees 0 rows. Authority is
// the shared PG (a reconnect to any cluster instance checks against the
// same table).
//
// Returns the number of orphaned rows (for metrics/logging). NOT an error at
// 0 — nothing to orphan (everything is already terminal, or everything is
// declared alive).
func OrphanDispatched(ctx context.Context, db ExecQueryRower, sid string, known []*ActiveApply) (int64, error) {
	if sid == "" {
		return 0, fmt.Errorf("applyrun: empty sid")
	}

	// ARRAY of known apply_id for PG's `!= ALL($2)`. Empty/nil → empty array:
	// `apply_id != ALL('{}')` is true for all → all dispatched are terminalized.
	knownIDs := make([]string, 0, len(known))
	for _, a := range known {
		if a != nil && a.ApplyID != "" {
			knownIDs = append(knownIDs, a.ApplyID)
		}
	}

	tag, err := db.Exec(ctx, orphanDispatchedSQL, sid, knownIDs, orphanDispatchedErrorSummary)
	if err != nil {
		return 0, fmt.Errorf("applyrun: orphan dispatched: %w", err)
	}
	return tag.RowsAffected(), nil
}

const selectAccessByApplyIDSQL = `
SELECT incarnation_name, started_by_aid
FROM apply_runs
WHERE apply_id = $1
ORDER BY started_at ASC
LIMIT 1
`

// Access — a narrow projection for SSE-RBAC: a run's owner and its
// incarnation. StartedByAID is `NULL` for runs without an Archon identity
// (Soul-initiated / system); the caller treats nil as "no owner" (access
// gated by incarnation permission only).
type Access struct {
	IncarnationName string
	StartedByAID    *string
}

// SelectAccessByApplyID resolves `apply_id → (incarnation, started_by_aid)`
// from any row of the run (apply_id can have several fan-out SIDs;
// incarnation and started_by_aid are the same for all of them — we take the
// earliest by started_at). Used by the SSE handler for subscription RBAC
// checks.
//
// Returns [ErrApplyRunNotFound] if the run doesn't exist (the SSE handler
// treats it as 403 — anti-enum: the same response as an access denial).
func SelectAccessByApplyID(ctx context.Context, db ExecQueryRower, applyID string) (*Access, error) {
	row := db.QueryRow(ctx, selectAccessByApplyIDSQL, applyID)
	var (
		acc       Access
		startedBy *string
	)
	if err := row.Scan(&acc.IncarnationName, &startedBy); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrApplyRunNotFound
		}
		return nil, fmt.Errorf("applyrun: resolve access: %w", err)
	}
	acc.StartedByAID = startedBy
	return &acc, nil
}

const selectIncarnationByApplyIDSQL = `
SELECT incarnation_name, scenario, attempt
FROM apply_runs
WHERE apply_id = $1 AND sid = $2 AND passage = $3
`

// SelectIncarnationByApplyID — a narrow resolve of `(apply_id, sid) →
// (incarnation, scenario, attempt)` for RunResult correlation. Returns only
// the fields needed by state-commit and the epoch check on result receipt
// (ADR-027(g), gate-1); [SelectByApplyID] gives the full row.
//
// attempt — the row's current fencing epoch (incremented by [ClaimNext] on
// Ward claim): correlateRunResult compares it against RunResult.attempt and
// rejects a result from a stale attempt (recvAttempt < row.attempt → a
// re-claim with a higher epoch exists → stale-drop).
//
// passage addresses the Passage row of staged-render (ADR-056):
// RunResult.passage echo indicates whose terminal we're correlating; N=1 →
// 0 (the host's only row).
//
// Returns [ErrApplyRunNotFound] if the row doesn't exist (an apply without a
// scenario-runner — ad-hoc push / standalone apply; the caller handles it as
// log+skip).
func SelectIncarnationByApplyID(ctx context.Context, db ExecQueryRower, applyID, sid string, passage int) (incarnationName, scenario string, attempt int32, err error) {
	row := db.QueryRow(ctx, selectIncarnationByApplyIDSQL, applyID, sid, passage)
	if scanErr := row.Scan(&incarnationName, &scenario, &attempt); scanErr != nil {
		if errors.Is(scanErr, pgx.ErrNoRows) {
			return "", "", 0, ErrApplyRunNotFound
		}
		return "", "", 0, fmt.Errorf("applyrun: resolve incarnation: %w", scanErr)
	}
	return incarnationName, scenario, attempt, nil
}
