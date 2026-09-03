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
// drift (ADR-031(d), an informational status) is allowed: drift does NOT block
// remediation (same as ready). An operator can destroy an incarnation in drift
// exactly like from ready, without waiting for a fix-apply.
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
	id string,
	force bool,
	source audit.Source,
	archonAID, historyID string,
	logger *slog.Logger,
) (*DestroyResult, error) {
	if !ValidID(id) {
		return nil, fmt.Errorf("incarnation: invalid id %q", id)
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
WHERE id = $1
FOR UPDATE
`
	var (
		stateBytes []byte
		statusStr  string
	)
	if err := tx.QueryRow(ctx, selectForUpdateSQL, id).Scan(&stateBytes, &statusStr); err != nil {
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
		historyID, id, destroyScenarioLabel, stateBytes, changedByArg,
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
WHERE id = $1
`
	if _, err := tx.Exec(ctx, updateSQL, id, string(StatusDestroying), detailsBytes); err != nil {
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

	writeDestroyAudit(ctx, w, source, archonAID, id, previous, force, logger)

	return &DestroyResult{PreviousStatus: previous, HistoryID: historyID}, nil
}

// Archive statuses — the TERMINAL status written into `incarnation_archive`.
// Deliberately NOT [Status] values: these exist only in the archive and must
// never be assignable to a live `incarnation` row (whose CHECK constraint does
// not know them). The live row is always `destroying` at archive time, so
// copying its status verbatim would stamp every archived incarnation with a
// non-terminal value that distinguishes nothing — the defect NIM-395 fixes.
//
//   - ArchiveStatusDestroyed — teardown RAN and passed on every host: the service
//     had a `destroy` scenario and its run reached a successful terminal. That is
//     the whole guarantee. Which resources the scenario actually released is up
//     to the service author — the keeper does not require a
//     `core.cloud.destroyed` step and does not verify one ran. A `destroy`
//     scenario that tears down nothing archives as `destroyed` all the same.
//   - ArchiveStatusForceDestroyed — teardown was SKIPPED (force): the record was
//     removed without running anything. Whatever the incarnation held is still
//     out there; [UnreleasedResources] in `status_details.unreleased` says what.
const (
	ArchiveStatusDestroyed      = "destroyed"
	ArchiveStatusForceDestroyed = "force_destroyed"
)

// State keys the keeper-side cloud teardown reads (`core.cloud.destroyed` gets
// them through the `destroy` scenario's `params:`). They are a SERVICE-AUTHOR
// convention, not a keeper contract — a service may name its state differently —
// so they are read best-effort: absent/mistyped keys yield empty fields rather
// than an error. The authoritative part of [UnreleasedResources] is SIDs, read
// from `incarnation_membership`; the full state is archived in any case.
const (
	stateKeyProvisionedProvider = "provisioned_provider"
	stateKeyProvisionedVMIDs    = "provisioned_vm_ids"
)

// UnreleasedResources — what a force-destroy left behind. "Removing the record"
// and "releasing the resource" are different operations, and force does only the
// first: the `destroy` scenario never runs, so cloud VMs keep running (and
// billing), while `incarnation_membership` — the only record of which hosts the
// incarnation held — is wiped by the FK cascade on DELETE.
//
// Captured inside the delete transaction, BEFORE the cascade, and written to two
// durable places: `incarnation_archive.status_details.unreleased` and the
// `incarnation.destroy_completed` audit payload. Also returned to the caller so
// the operator sees it in the destroy reply instead of a bare success.
//
// Fields:
//   - SIDs — member hosts from `incarnation_membership`. Authoritative (keeper
//     owns this relation) and genuinely lost otherwise — the cascade deletes it.
//   - Provider / VMIDs — cloud coordinates from `incarnation.state`, best-effort
//     by the convention above. Empty for a service that provisions nothing.
type UnreleasedResources struct {
	Provider string   `json:"provider,omitempty"`
	VMIDs    []string `json:"vm_ids,omitempty"`
	SIDs     []string `json:"sids,omitempty"`
}

// IsEmpty reports that nothing outlived the record — no member hosts and no
// cloud coordinates. It is NOT the same as "teardown ran": an empty set on a
// force-destroy still means teardown was skipped, it just had nothing to skip.
// The distinction lives in the archive status, not here.
func (u *UnreleasedResources) IsEmpty() bool {
	return u == nil || (u.Provider == "" && len(u.VMIDs) == 0 && len(u.SIDs) == 0)
}

// auditPayload renders the record the way every other surface renders it, for
// embedding in an audit payload.
//
// The audit writer re-masks the payload by walking it with reflect, and that walk
// normalizes a struct to a map by json-tag name while ignoring `omitempty`. Handed
// the struct directly, the one surface built to outlive the response would be the
// one answering `"vm_ids": []` where the reply, the archive and the MCP result all
// omit the key — reintroducing per-field exactly the "checked-clean or
// could-not-tell" ambiguity this object rejects at the top level.
//
// The round-trip goes through the same json tags rather than listing the fields a
// second time: a fourth dimension added to the struct is carried here by
// construction instead of being silently dropped. A marshal failure is impossible
// for three string-ish fields; if it ever happens, fall back to the struct — a
// slightly noisier payload beats losing the record.
func (u *UnreleasedResources) auditPayload() any {
	b, err := json.Marshal(u)
	if err != nil {
		return u
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return u
	}
	return m
}

// DeleteResult — the outcome of [DeleteAfterTeardown]. Deleted=false means a
// no-op: no row was found in `destroying` status (someone already removed it /
// changed the status — a repeat call after a successful DELETE is idempotent).
//
// ArchiveStatus — the terminal status stamped on the archived row (one of the
// ArchiveStatus* constants). Unreleased — non-nil ONLY on the force path
// (teardown skipped); nil after a real teardown, where the resources are gone by
// definition. Both are zero when Deleted=false.
type DeleteResult struct {
	Deleted       bool
	ArchiveStatus string
	Unreleased    *UnreleasedResources
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
//
// secrets is the declarative secret layer for this incarnation's `state`
// ([StateSchemaSecrets] over the service artifact the caller already holds) and
// is used by the force-path capture below. It is a POSITIONAL parameter rather
// than a field on some options struct on purpose: every call site holds the
// artifact, and a site that forgets to pass one is a compile error instead of a
// destroy that quietly ships a declared secret into `status_details` (NIM-531 —
// there were three sites and the capture reached all of them). nil is legal and
// means "artifact unreadable" — the capture degrades to vault+regex, never fails.
func DeleteAfterTeardown(
	ctx context.Context,
	pool TxBeginner,
	w audit.Writer,
	id string,
	force bool,
	secrets audit.SecretSchema,
	logger *slog.Logger,
) (*DeleteResult, error) {
	if !ValidID(id) {
		return nil, fmt.Errorf("incarnation: invalid id %q", id)
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("incarnation: begin delete tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// (a0) force ⇒ teardown was skipped: capture what the record was holding
	// BEFORE the archive+DELETE, while `incarnation_membership` still exists (the
	// FK cascade on DELETE wipes it) — NIM-395. Read inside this tx so the
	// recorded set is exactly the state that is about to be deleted.
	archiveStatus := ArchiveStatusDestroyed
	var unreleased *UnreleasedResources
	detailsPatch := []byte(`{}`)
	if force {
		archiveStatus = ArchiveStatusForceDestroyed
		u, cerr := collectUnreleased(ctx, tx, id, secrets)
		if cerr != nil {
			return nil, cerr
		}
		// Nothing abandoned ⇒ no record at all, not an empty one. An operator
		// reading `"unreleased": {}` has to work out whether it means "checked,
		// clean" or "could not tell"; absence says the same thing without the
		// question, and `force_destroyed` already marks the row as a force.
		if !u.IsEmpty() {
			unreleased = u
			patch, merr := json.Marshal(map[string]any{"unreleased": u})
			if merr != nil {
				return nil, fmt.Errorf("incarnation: marshal unreleased resources: %w", merr)
			}
			detailsPatch = patch
		}
	}

	// (a) Archive the incarnation row — only if it's in destroying (same guard
	// as DELETE: if the status already changed, don't archive the wrong row).
	//
	// `status` is NOT copied from the live row: at this point it is always
	// `destroying`, a non-terminal value that says nothing about the outcome
	// (NIM-395). $2 stamps the terminal status instead, and $3 merges the
	// unreleased-resources record into status_details (`||` keeps the existing
	// keys, notably `force`). Merging rather than replacing matters: status_details
	// is the destroy-intent record written by [Destroy].
	//
	// `spec` is no longer copied: the column is gone from `incarnation` (NIM-408).
	// The archive KEEPS its own `spec` column rather than dropping it — rows
	// archived before this release hold real data there, and a compliance archive
	// is the last place to delete history. New rows get its `{}` default.
	//
	// The two sides of this statement spell the identifier DIFFERENTLY, and that
	// is correct rather than sloppy. `incarnation.id` was renamed by migration
	// 118 (NIM-729); `incarnation_archive.name` was not — the archive is not one
	// of the ten registry tables that migration converts, so its column keeps the
	// name it was created with in 039. Spelling the target `id` fails with
	// `column "id" of relation "incarnation_archive" does not exist`.
	//
	// It joins the `*_name` tail — `state_history.incarnation_name` and the four
	// others — which converts in NIM-732. Until then the INSERT column list is
	// archive-side spelling and the SELECT is live-side spelling.
	const archiveIncarnationSQL = `
INSERT INTO incarnation_archive (
    name, service, service_version, state_schema_version,
    state, status, status_details, created_by_aid,
    created_at, updated_at
)
SELECT id, service, service_version, state_schema_version,
       state, $2, COALESCE(status_details, '{}'::jsonb) || $3::jsonb, created_by_aid,
       created_at, updated_at
FROM incarnation
WHERE id = $1 AND status = 'destroying'
`
	if _, err := tx.Exec(ctx, archiveIncarnationSQL, id, archiveStatus, detailsPatch); err != nil {
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
	if _, err := tx.Exec(ctx, archiveHistorySQL, id); err != nil {
		return nil, fmt.Errorf("incarnation: archive state_history: %w", err)
	}

	// (c) Single-winner DELETE. status='destroying' guarantees only the owner
	// of the destroying transition performs the removal. RowsAffected==0 → no-op.
	const deleteSQL = `
DELETE FROM incarnation
WHERE id = $1 AND status = 'destroying'
`
	tag, err := tx.Exec(ctx, deleteSQL, id)
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

	writeDestroyCompletedAudit(ctx, w, id, force, archiveStatus, unreleased, logger)

	return &DeleteResult{Deleted: true, ArchiveStatus: archiveStatus, Unreleased: unreleased}, nil
}

// collectUnreleased reads what a force-destroy is about to abandon, inside the
// delete transaction (see [UnreleasedResources]). Runs BEFORE the DELETE so
// `incarnation_membership` is still there.
//
// Never returns nil on success — an incarnation that held nothing yields an empty
// record, which is a meaningful statement ("teardown skipped, nothing to leak")
// rather than missing data. A row absent from `destroying` yields the empty
// record too: the DELETE below then reports RowsAffected==0 and the whole tx
// rolls back, so the value is never observed.
//
// State parsing is best-effort by design: `provisioned_*` is a service-author
// convention (see the const block), and a service that names its state otherwise
// must NOT fail its own destroy over it. A malformed/absent key drops the cloud
// coordinates, never the SIDs, and never the deletion.
//
// The state is masked BEFORE the two fields are read, so every consumer of this
// record — the destroy reply, the MCP result, the WARN line and the archived
// `status_details` — carries the same masked value from one place. `state` is
// service-authored, and every other surface that shows it masks it (the
// incarnation view, the history view, the run-event stream); reading two keys
// straight out of it would be the one path that does not.
//
// All four masking layers run, not two ([ADR-010] §7.4). secrets carries the
// declarative layer, built by the caller from the service artifact it already
// holds; [audit.MaskSecretsWithSchema] adds vault-origin and regex-last-resort on
// top. Until NIM-531 this function had no reader for the artifact and used the
// vault+regex pair alone, so a key a service declared `secret: true` — and that
// neither looked like a vault ref nor was named anything the regex knows — was
// masked by `GET /v1/incarnations/{id}` and printed here. The same value, two
// answers, decided by which endpoint the operator happened to call. A nil secrets
// still degrades to exactly that older pair: an unreadable artifact costs the
// declarative layer, never the destroy.
//
// Masking here protects the reply, the MCP result, the WARN line and
// `status_details`. It does NOT protect `incarnation_archive.state`, which the
// archive INSERT copies verbatim by design — a compliance archive keeps the raw
// record.
//
// SIDs are deliberately NOT masked: `incarnation_membership` is keeper-owned and
// holds validated FQDNs, so there is no service-authored content in it to hide.
//
// The state read takes no FOR UPDATE, unlike the one in [Destroy] (S-D1), and a
// lock would not buy what it looks like it buys. `state` does have a writer that
// accepts `destroying` — [UpdateStateFromRun], there for the teardown run — and
// it is reachable even on this path: an orphan-reconcile releases an `applying`
// row back to `ready` without bumping the attempt, a force takes it from there,
// and a RunResult from the abandoned run still passes the epoch gate.
//
// What keeps the capture honest is not the absence of that writer but a property
// of it: it sets `state` and `status` in the same UPDATE and always to a terminal
// status, never back to `destroying`. Both halves of the operation below are
// gated on `destroying` — this SELECT and the DELETE — so any interleaving takes
// the row out from under both at once: the capture reads no rows, the DELETE
// matches none, and the transaction rolls back whole. The failure mode is a
// destroy that did not happen, never an `unreleased` that lies.
//
// The membership half has no such gate. `incarnation_membership` is written by
// the bind/unbind endpoints, which neither read the incarnation's status nor
// touch its row — so no lock taken here would serialize them, and a bind landing
// between this read and the DELETE is cascaded away without ever appearing in
// the record. That belongs on those endpoints; tracked as NIM-541.
func collectUnreleased(ctx context.Context, tx pgx.Tx, id string, secrets audit.SecretSchema) (*UnreleasedResources, error) {
	u := &UnreleasedResources{}

	const selectStateSQL = `
SELECT state
FROM incarnation
WHERE id = $1 AND status = 'destroying'
`
	var stateBytes []byte
	switch err := tx.QueryRow(ctx, selectStateSQL, id).Scan(&stateBytes); {
	case errors.Is(err, pgx.ErrNoRows):
		return u, nil
	case err != nil:
		return nil, fmt.Errorf("incarnation: read state for unreleased resources: %w", err)
	}

	var state map[string]any
	if len(stateBytes) > 0 && json.Unmarshal(stateBytes, &state) == nil {
		masked := audit.MaskSecretsWithSchema(state, secrets)
		u.Provider, _ = masked[stateKeyProvisionedProvider].(string)
		u.VMIDs = jsonStringSlice(masked[stateKeyProvisionedVMIDs])
	}

	sids, err := ListMemberSIDs(ctx, tx, id)
	if err != nil {
		return nil, fmt.Errorf("incarnation: read membership for unreleased resources: %w", err)
	}
	u.SIDs = sids

	return u, nil
}

// jsonStringSlice narrows a jsonb array decoded into `any` down to []string.
// Non-arrays and non-string elements are skipped rather than erroring: this reads
// service-authored state (see [collectUnreleased]), where a wrong shape must not
// fail a destroy. nil for "nothing usable found" so the field stays omitted.
func jsonStringSlice(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// writeDestroyCompletedAudit writes the `incarnation.destroy_completed` audit
// event after the row is physically removed. source=keeper_internal (write
// path — the scenario-runner past the barrier, archon_aid column is NULL). A
// write failure doesn't fail destroy (the row is already gone, tx committed),
// it's only logged.
//
// The payload carries the terminal archive status and, on the force path, the
// resources left behind (NIM-395). The audit trail is the one operator-readable
// record that survives independently of the incarnation: `incarnation_archive`
// has no read API yet, so without this the fate of the cloud VMs is unknowable
// after the fact. No secrets: names, SIDs and provider VM ids only.
func writeDestroyCompletedAudit(
	ctx context.Context,
	w audit.Writer,
	id string,
	force bool,
	archiveStatus string,
	unreleased *UnreleasedResources,
	logger *slog.Logger,
) {
	if w == nil {
		return
	}
	payload := map[string]any{
		"id":             id,
		"force":          force,
		"archive_status": archiveStatus,
	}
	if force {
		// `teardown` is written even when nothing was left behind: skipping it is
		// the fact being recorded, and an absent key would read as "not skipped".
		// `unreleased` is the opposite — it lists things, so an empty list is an
		// absent key rather than a `null` an audit reader has to interpret.
		payload["teardown"] = "skipped"
		if !unreleased.IsEmpty() {
			payload["unreleased"] = unreleased.auditPayload()
		}
	}
	ev := &audit.Event{
		EventType: audit.EventIncarnationDestroyCompleted,
		Source:    audit.SourceKeeperInternal,
		Payload:   payload,
	}
	if err := w.Write(ctx, ev); err != nil && logger != nil {
		logger.Warn("incarnation: writing audit incarnation.destroy_completed failed",
			slog.String("id", id), slog.Any("error", err))
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
	archonAID, id string,
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
			"id":              id,
			"previous_status": string(previous),
			"force":           force,
		},
	}
	if err := w.Write(ctx, ev); err != nil && logger != nil {
		logger.Warn("incarnation: writing audit incarnation.destroy_started failed",
			slog.String("id", id), slog.Any("error", err))
	}
}
