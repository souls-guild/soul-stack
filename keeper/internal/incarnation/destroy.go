package incarnation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// ErrIncarnationNotDestroyable — destroy rejected: the incarnation's current
// status isn't in the set allowed to initiate destroy (409). The handler side
// maps this to the incarnation-locked problem-type.
var ErrIncarnationNotDestroyable = errors.New("incarnation: status does not allow destroy")

// destroyScenarioLabel — the `state_history.scenario` value for the destroy-
// initiation transition. Destroy itself (teardown) is a future run of the
// `destroy` scenario (S-D2); S-D1 only records the transition to `destroying`
// under this label (state_history requires a non-null scenario), symmetric
// with unlock / migration.
const destroyScenarioLabel = "destroy"

// DestroyResult — the outcome of a destroy initiation: the status before the
// transition (for reply / audit) and the recorded state_history snapshot ID.
type DestroyResult struct {
	PreviousStatus Status
	HistoryID      string
}

// canDestroyFrom — the set of statuses allowed to initiate destroy. ready is
// the normal path; error_locked / migration_failed allow tearing down a
// "stuck" instance without a mandatory unlock first (the operator is
// deliberately destroying, not fixing). applying is rejected: a run is in
// progress, FOR UPDATE+status serialize the race with the scenario-runner.
// destroying is rejected too: a repeat initiation (idempotency is S-D3's job;
// here it's an explicit refusal).
//
// drift (ADR-031, Scry, an informational status) is allowed: drift does NOT
// block remediation (same as ready). An operator can destroy an incarnation in
// drift exactly like from ready, without waiting for a fix-apply.
func canDestroyFrom(s Status) bool {
	switch s {
	case StatusReady, StatusErrorLocked, StatusMigrationFailed, StatusDrift:
		return true
	}
	return false
}

// Destroy initiates incarnation destroy: transitions the row to `destroying`
// (S-D1). Teardown (scenario `destroy`, S-D2) and row DELETE (S-D3) are NOT
// part of this slice.
//
// Atomicity follows the same transactional pattern as [Unlock]: one tx —
// SELECT … FOR UPDATE → status guard → INSERT zero-diff state_history →
// UPDATE status=destroying. FOR UPDATE serializes destroy against a
// concurrent scenario-runner (lockRun locks the same row), closing the
// TOCTOU window between the status probe and the transition.
//
// Transition guard ([canDestroyFrom]):
//   - ready / error_locked / migration_failed → destroy allowed;
//   - applying → [ErrIncarnationNotDestroyable] (a run is in progress);
//   - destroying → [ErrIncarnationNotDestroyable] (destroy already initiated).
//
// force — intent to "destroy without teardown" (force=true → S-D3 deletes the
// row directly, without running the `destroy` scenario). S-D1 doesn't implement
// teardown behavior itself: force is only saved into `status_details.force` so
// S-D3 can read the intent off the already-locked row.
//
// state is NOT modified (destroy doesn't touch the state graph; teardown works
// with hosts, not jsonb). A zero-diff state_history snapshot is written to
// record the initiation itself, symmetric with unlock.
//
// The `incarnation.destroy_started` audit event is written AFTER commit (same
// as UpdateStateFromRun: DB consistency must not depend on the audit write).
// An audit-write failure is logged but does NOT roll back destroy — the
// transition is already committed; silently losing the audit trail is
// unacceptable, but so is blocking destroy because of it. w == nil → no trail
// is written (unit/L0). source / archonAID identify the initiator (api / mcp),
// passed through by the caller.
//
// Returns:
//   - [ErrIncarnationNotFound] — name doesn't exist (404).
//   - [ErrIncarnationNotDestroyable] — status doesn't allow destroy (409).
func Destroy(
	ctx context.Context,
	pool TxBeginner,
	w audit.Writer,
	name string,
	force bool,
	source audit.Source,
	archonAID, historyID string,
	logger *slog.Logger,
) (*DestroyResult, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("incarnation: invalid name %q", name)
	}
	if historyID == "" {
		return nil, fmt.Errorf("incarnation: empty history_id")
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("incarnation: begin destroy tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const selectForUpdateSQL = `
SELECT state, status
FROM incarnation
WHERE name = $1
FOR UPDATE
`
	var (
		stateBytes []byte
		statusStr  string
	)
	if err := tx.QueryRow(ctx, selectForUpdateSQL, name).Scan(&stateBytes, &statusStr); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrIncarnationNotFound
		}
		return nil, fmt.Errorf("incarnation: destroy select: %w", err)
	}
	previous := Status(statusStr)
	if !canDestroyFrom(previous) {
		return nil, fmt.Errorf("%w: %s", ErrIncarnationNotDestroyable, previous)
	}

	var changedByArg any
	if archonAID != "" {
		changedByArg = archonAID
	}

	// state_before == state_after: destroy initiation doesn't change state.
	// apply_id = history_id ($1): initiation isn't tied to an apply run (the
	// schema requires NOT NULL, no FK to apply_runs) — history_id is used as a
	// unique non-null marker, symmetric with unlock.
	const historyInsertSQL = `
INSERT INTO state_history (
    history_id, incarnation_name, scenario, state_before, state_after,
    changed_by_aid, apply_id
) VALUES ($1, $2, $3, $4, $4, $5, $1)
`
	if _, err := tx.Exec(ctx, historyInsertSQL,
		historyID, name, destroyScenarioLabel, stateBytes, changedByArg,
	); err != nil {
		return nil, fmt.Errorf("incarnation: insert destroy state_history: %w", err)
	}

	// status_details.force — intent for S-D3: force=true → DELETE without
	// teardown. No masking needed: force is a bool, carries no secrets.
	detailsBytes, err := json.Marshal(map[string]any{"force": force})
	if err != nil {
		return nil, fmt.Errorf("incarnation: marshal destroy status_details: %w", err)
	}

	const updateSQL = `
UPDATE incarnation
SET status = $2, status_details = $3, updated_at = NOW()
WHERE name = $1
`
	if _, err := tx.Exec(ctx, updateSQL, name, string(StatusDestroying), detailsBytes); err != nil {
		return nil, fmt.Errorf("incarnation: destroy update: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("incarnation: commit destroy tx: %w", err)
	}

	// TODO(S-D4/S-D3): trigger teardown + DELETE row. Teardown execution is
	// already implemented — scenario.Runner.StartDestroy runs the `destroy`
	// scenario against the incarnation's hosts in TerminalDestroy mode (S-D2b):
	// success leaves `destroying` (DELETE is S-D3), failure → destroy_failed.
	// Here (the Destroy service layer) we only transition to `destroying`;
	// calling StartDestroy from the handler after this transaction commits is
	// S-D4 (force=true → S-D3 deletes the row directly, without teardown).

	writeDestroyAudit(ctx, w, source, archonAID, name, previous, force, logger)

	return &DestroyResult{PreviousStatus: previous, HistoryID: historyID}, nil
}

// DeleteResult — the outcome of [DeleteAfterTeardown]. Deleted=false means a
// no-op: no row was found in `destroying` status (someone already removed it /
// changed the status — a repeat call after a successful DELETE is idempotent).
type DeleteResult struct {
	Deleted bool
}

// DeleteAfterTeardown physically removes the incarnation after a successful
// teardown (S-D3, cascade V3). One PG transaction, single-winner:
//
//  1. INSERT INTO incarnation_archive SELECT … FROM incarnation
//     WHERE name=$1 AND status='destroying' — a compliance-minimum snapshot
//     BEFORE deletion.
//  2. INSERT INTO state_history_archive SELECT … FROM state_history
//     WHERE incarnation=$1 — a snapshot of the transition log BEFORE the cascade.
//  3. DELETE FROM incarnation WHERE name=$1 AND status='destroying' — removes
//     the row. WHERE status='destroying' is the single-winner guard: exactly
//     one handler owning the destroying transition wins. RowsAffected==0
//     (no row / status changed / already deleted by someone else) → the
//     transaction rolls back, [DeleteResult.Deleted]=false, an idempotent no-op.
//
// The cascade (ON DELETE CASCADE on live state_history / apply_runs /
// apply_task_register) fires on DELETE; the archive is written before it, so
// compliance data survives. The archive is written inside the SAME tx as
// DELETE: either archive+DELETE commit atomically together, or neither does
// (rollback).
//
// INSERT order: incarnation_archive before state_history_archive, both before
// DELETE — the selects read rows that are still live.
//
// The `incarnation.destroy_completed` audit event is written AFTER commit
// (same pattern as [Destroy]: DB consistency doesn't depend on the audit
// write). Written ONLY on an actual deletion (Deleted=true): a no-op produces
// no event. force is carried in the payload (destroy-without-teardown intent).
// w == nil → no trail is written.
//
// [ErrIncarnationNotFound] is NOT used as a return: the absence of a
// destroying row is a legitimate no-op (Deleted=false), not an error
// (S-D3 idempotency).
func DeleteAfterTeardown(
	ctx context.Context,
	pool TxBeginner,
	w audit.Writer,
	name string,
	force bool,
	logger *slog.Logger,
) (*DeleteResult, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("incarnation: invalid name %q", name)
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("incarnation: begin delete tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// (a) Archive the incarnation row — only if it's in destroying (same guard
	// as DELETE: if the status already changed, don't archive the wrong row).
	const archiveIncarnationSQL = `
INSERT INTO incarnation_archive (
    name, service, service_version, state_schema_version,
    spec, state, status, status_details, created_by_aid,
    created_at, updated_at
)
SELECT name, service, service_version, state_schema_version,
       spec, state, status, status_details, created_by_aid,
       created_at, updated_at
FROM incarnation
WHERE name = $1 AND status = 'destroying'
`
	if _, err := tx.Exec(ctx, archiveIncarnationSQL, name); err != nil {
		return nil, fmt.Errorf("incarnation: archive incarnation: %w", err)
	}

	// (b) Archive the state_history log (the full history of the incarnation
	// being removed, BEFORE the cascade). Not gated on status — the whole log
	// is archived; if no incarnation row is in destroying, the DELETE below
	// yields RowsAffected==0 and the tx rolls back, undoing this INSERT too.
	const archiveHistorySQL = `
INSERT INTO state_history_archive (
    history_id, incarnation_name, scenario, state_before, state_after,
    changed_by_aid, apply_id, at
)
SELECT history_id, incarnation_name, scenario, state_before, state_after,
       changed_by_aid, apply_id, at
FROM state_history
WHERE incarnation_name = $1
`
	if _, err := tx.Exec(ctx, archiveHistorySQL, name); err != nil {
		return nil, fmt.Errorf("incarnation: archive state_history: %w", err)
	}

	// (c) Single-winner DELETE. status='destroying' guarantees only the owner
	// of the destroying transition performs the removal. RowsAffected==0 → no-op.
	const deleteSQL = `
DELETE FROM incarnation
WHERE name = $1 AND status = 'destroying'
`
	tag, err := tx.Exec(ctx, deleteSQL, name)
	if err != nil {
		return nil, fmt.Errorf("incarnation: delete incarnation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// No one won the row: status changed / row already deleted. The rollback
		// (defer Rollback) also undoes the archive written above — it's moot without DELETE.
		return &DeleteResult{Deleted: false}, nil
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("incarnation: commit delete tx: %w", err)
	}

	writeDestroyCompletedAudit(ctx, w, name, force, logger)

	return &DeleteResult{Deleted: true}, nil
}

// writeDestroyCompletedAudit writes the `incarnation.destroy_completed` audit
// event after the row is physically removed. source=keeper_internal (write
// path — the scenario-runner past the barrier, archon_aid column is NULL). A
// write failure doesn't fail destroy (the row is already gone, tx committed),
// it's only logged. No secrets in the payload (name + force).
func writeDestroyCompletedAudit(
	ctx context.Context,
	w audit.Writer,
	name string,
	force bool,
	logger *slog.Logger,
) {
	if w == nil {
		return
	}
	ev := &audit.Event{
		EventType: audit.EventIncarnationDestroyCompleted,
		Source:    audit.SourceKeeperInternal,
		Payload: map[string]any{
			"name":  name,
			"force": force,
		},
	}
	if err := w.Write(ctx, ev); err != nil && logger != nil {
		logger.Warn("incarnation: writing audit incarnation.destroy_completed failed",
			slog.String("name", name), slog.Any("error", err))
	}
}

// writeDestroyAudit writes the destroy-initiation audit event. Split out so
// transition logic doesn't mix with the best-effort audit write. A write
// failure doesn't fail destroy (the transition is already committed), it's
// only logged.
func writeDestroyAudit(
	ctx context.Context,
	w audit.Writer,
	source audit.Source,
	archonAID, name string,
	previous Status,
	force bool,
	logger *slog.Logger,
) {
	if w == nil {
		return
	}
	ev := &audit.Event{
		EventType: audit.EventIncarnationDestroyStarted,
		Source:    source,
		ArchonAID: archonAID,
		Payload: map[string]any{
			"name":            name,
			"previous_status": string(previous),
			"force":           force,
		},
	}
	if err := w.Write(ctx, ev); err != nil && logger != nil {
		logger.Warn("incarnation: writing audit incarnation.destroy_started failed",
			slog.String("name", name), slog.Any("error", err))
	}
}
