// Package reaper — Go wrappers for Reaper rules of Keeper
// (see docs/keeper/reaper.md, ADR-022(d)). Reaper loop (cron driver,
// leader election via Redis lease — ADR-006) will arrive in M0.6;
// in M0.4.1c only per-rule SQL call is fixed with unit coverage,
// so loop driver can use the ready block.
//
// Package is pgx-aware (pulls `pgx/v5` types for row scan), lives in
// `keeper/internal/`, not `shared/` — for the same reason as
// `keeper/internal/auditpg` (Soul binary isolation from pgx, ADR-011).
package reaper

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// queryRower — narrow subset of pgxpool.Pool interface needed for
// SELECT purge_audit_old(...). Narrowing allows unit testing
// Purger with a fake implementation without standing up Postgres; real pool from
// keeper/internal/pg satisfies the interface automatically.
//
// Query (not just QueryRow) is needed for lease-aware `mark_disconnected`:
// select_disconnect_candidates returns SETOF text (many rows).
type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// soulLeaseChecker — narrow interface for Redis check "is EventStream to
// SID alive", needed for lease-aware `mark_disconnected`. Narrowing to one method
// isolates the reaper package from full keeperredis.Client and allows faking in
// unit tests. Real implementation is [keeperredis.SoulStreamAlive] wrapper,
// assembled in cmd/keeper (see daemon.setupReaper).
type soulLeaseChecker interface {
	SoulStreamAlive(ctx context.Context, sid string) (bool, error)
}

// Purger — Go wrapper for SQL function `purge_audit_old`. One instance
// per Keeper process; safe for concurrent use — pool itself provides
// thread safety. Each call to [PurgeAuditOld] is exactly one batch
// (default 1000 records); loop logic (drain to 0, retry, cron,
// leader election) is out of scope for M0.4.1c, will appear in M0.6
// reaper runner.
type Purger struct {
	pool queryRower

	// lease — optional Redis check of live EventStream to SID for
	// lease-aware `mark_disconnected` (ADR-006(a)). nil → rule degrades
	// to previous pure-SQL path (migration 014, mark_disconnected): single-
	// instance dev / unit mode without coordination. Production wire-up
	// (cmd/keeper) passes wrapper over shared Redis client.
	lease soulLeaseChecker

	// logger — for warn when Redis is unavailable in lease-aware branch
	// (see MarkDisconnected). nil-safe: when nil, log is suppressed.
	logger *slog.Logger
}

// NewPurger wraps already initialized pgxpool.Pool. Ownership
// of pool remains with caller: Purger does not close pool, lifecycle —
// keeper/internal/pg → keeper/cmd/keeper.
//
// Without Redis check: `mark_disconnected` works in pure-SQL mode
// (migration 014). For lease-aware mode — [NewPurgerWithLease].
func NewPurger(pool *pgxpool.Pool) *Purger {
	return &Purger{pool: pool}
}

// NewPurgerWithLease — constructor of lease-aware Purger: `mark_disconnected`
// cross-checks with Redis (live SID-lease ⇒ Soul is NOT marked disconnected even if
// stale PG `last_seen_at`). Other rules work as in [NewPurger].
//
// `lease` can be nil — then behavior is identical to [NewPurger] (pure-SQL
// fallback). `logger` is optional (nil → warns are suppressed).
func NewPurgerWithLease(pool *pgxpool.Pool, lease soulLeaseChecker, logger *slog.Logger) *Purger {
	return &Purger{pool: pool, lease: lease, logger: logger}
}

// newPurgerFromQueryRower — internal constructor for unit tests,
// accepting narrow interface. Public [NewPurger] fixes type
// `*pgxpool.Pool` so callers don't depend on interface expansion
// in the future.
func newPurgerFromQueryRower(pool queryRower) *Purger {
	return &Purger{pool: pool}
}

// newPurgerWithLeaseFromQueryRower — internal constructor for unit tests
// of lease-aware branch: narrow queryRower + fake lease-checker.
func newPurgerWithLeaseFromQueryRower(pool queryRower, lease soulLeaseChecker, logger *slog.Logger) *Purger {
	return &Purger{pool: pool, lease: lease, logger: logger}
}

// defaultBatchSize — fallback batch-size if caller passed <= 0.
// Matches DEFAULT in SQL function (migration 002); duplicated
// here so caller with batchSize=0 gets predictable value in
// logs/metrics before PG call.
const defaultBatchSize = 1000

// PurgeAuditOld removes one batch of expired audit_log records older than
// `maxAge`. Returns count of deleted records in this batch.
// Loop logic (drain to 0, cron, leader-election, retry) — out of scope
// M0.4.1c, will appear in M0.6 reaper-runner.
//
// `maxAge` must be positive; ≤0 returns error without PG call
// (negative duration → PG-interval like `-3600 seconds` syntactically
// valid, but semantics of `NOW() - (-1h) = NOW()+1h` will result in deletion
// of 0 rows and silent config error swallowing; explicit rejection
// — is the only safe mode).
//
// `batchSize <= 0` → [defaultBatchSize] is used (1000), without error.
// `maxAge` is converted to Postgres interval literal via
// [durationToPGInterval]; caller (reaper-runner) reads value from
// `keeper.yml → reaper.rules.purge_audit_old.max_age`, alias for
// `audit.retention_days` (consistency check — in shared/config parser,
// M0/M1.thin).
func (p *Purger) PurgeAuditOld(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error) {
	return p.callIntervalBatch(ctx, "purge_audit_old", maxAge, batchSize)
}

// PurgeExpiredPendingTokens removes batch of unused bootstrap tokens,
// whose `expires_at` has expired (older by `maxAge` beyond expiry). Rule name in
// config — `expire_pending_seeds`; PM decision Reaper.b: semantics —
// DELETE (not UPDATE-with-status), because `bootstrap_tokens` table does not have
// status column, and expired pending token cannot be used.
// Audit of creation lives in `audit_log` under its own retention (ADR-022).
//
// `maxAge` usually = 0 (delete immediately after expiration) or small
// grace period. Passing `0` is forbidden by the same invariant as
// PurgeAuditOld — otherwise caller with default will accidentally delete
// active tokens (NOW() - 0 = NOW() — all expired tokens fall into
// the predicate, which is what's needed; but `-1h` will result in deletion
// of tokens not yet expired).
//
// In practice, `expire_pending_seeds` in keeper.yml has `max_age: 24h`,
// which corresponds to Bootstrap-policy TTL; see docs/keeper/reaper.md.
func (p *Purger) PurgeExpiredPendingTokens(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error) {
	return p.callIntervalBatch(ctx, "expire_pending_seeds", maxAge, batchSize)
}

// PurgeUsedTokens removes batch of used bootstrap tokens
// (`used_at IS NOT NULL`) older than `maxAge` from `used_at`. Default
// `maxAge` = 90d (docs/keeper/reaper.md).
func (p *Purger) PurgeUsedTokens(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error) {
	return p.callIntervalBatch(ctx, "purge_used_tokens", maxAge, batchSize)
}

// PurgeSouls removes batch of `souls` records in specified `statuses`
// (e.g., `[disconnected, expired]`) older than `maxAge`.
// Age is calculated from `COALESCE(last_seen_at, registered_at)` — for
// Souls that never connected, `registered_at` is used.
//
// `statuses` must be non-empty: without status filter, `DELETE`
// would remove live `connected` records (docs/keeper/reaper.md). Empty
// or nil — returns error without PG call.
//
// Valid values — narrow MVP enum souls.status (`pending` |
// `connected` | `disconnected` | `revoked` | `expired`); validation
// is done on semantic phase side of keeper.yml parser, not
// here.
//
// CASCADE: ON DELETE bootstrap_tokens/soul_seeds (CASCADE) automatically
// cleans related records (see migrations 008/009).
func (p *Purger) PurgeSouls(ctx context.Context, statuses []string, maxAge time.Duration, batchSize int) (int64, error) {
	if len(statuses) == 0 {
		return 0, fmt.Errorf("reaper.purge_souls: statuses must be non-empty")
	}
	return p.callStatusesIntervalBatch(ctx, "purge_souls", statuses, maxAge, batchSize)
}

// PurgeOldSeeds removes batch of `soul_seeds` records in specified
// `statuses` (default `[superseded, expired, revoked]`) with
// `issued_at` older than `maxAge`. Active seeds are excluded via
// status filter.
//
// `statuses` must be non-empty — without filter DELETE would remove
// active certificates.
func (p *Purger) PurgeOldSeeds(ctx context.Context, statuses []string, maxAge time.Duration, batchSize int) (int64, error) {
	if len(statuses) == 0 {
		return 0, fmt.Errorf("reaper.purge_old_seeds: statuses must be non-empty")
	}
	return p.callStatusesIntervalBatch(ctx, "purge_old_seeds", statuses, maxAge, batchSize)
}

// PurgeOldCerts removes batch of `warrant` registry records (migration 092) in
// specified `statuses` (default `[superseded, expired, failed]`) with `issued_at`
// older than `maxAge`. Retention of growing history of service cert rotations (R4,
// cert-rotation Var1). Active/rotating are excluded via status filter (live
// material / cert in rotation process).
//
// `statuses` must be non-empty — without filter DELETE would remove active certs.
// Parity with PurgeOldSeeds; SQL function `purge_old_certs` (093).
func (p *Purger) PurgeOldCerts(ctx context.Context, statuses []string, maxAge time.Duration, batchSize int) (int64, error) {
	if len(statuses) == 0 {
		return 0, fmt.Errorf("reaper.purge_old_certs: statuses must be non-empty")
	}
	return p.callStatusesIntervalBatch(ctx, "purge_old_certs", statuses, maxAge, batchSize)
}

// PurgeApplyRuns removes batch of completed apply-runs from registry
// `apply_runs` (migration 018) with `finished_at` older than `maxAge`. Default
// `maxAge` = 30d (docs/keeper/reaper.md).
//
// Only finished records are deleted (`success`/`failed`/`cancelled` with
// `finished_at IS NOT NULL`); `running` SQL function does not touch — filter
// is embedded in `purge_apply_runs` (021), no additional check
// is required. Age is calculated from `finished_at`.
func (p *Purger) PurgeApplyRuns(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error) {
	return p.callIntervalBatch(ctx, "purge_apply_runs", maxAge, batchSize)
}

// PurgeApplyRunPlan removes batch of "run task plan" records (`apply_run_plan`,
// migration 096, NIM-37) of runs, completed and older than `gracePeriod` (by
// `created_at`). Default grace = 30d (align with `purge_apply_runs`).
//
// FK cascade for plan does not exist (apply_id is not PK in any table), so without this
// rule, plan rows would grow as orphans after deletion of `apply_runs`. Plan
// of ACTIVE run (non-terminal apply_run) SQL function does not touch — filter
// is embedded in `purge_apply_run_plan` (097). Mirror of PurgeApplyTaskRegister, but by
// apply_id (plan is host-invariant, has no sid).
func (p *Purger) PurgeApplyRunPlan(ctx context.Context, gracePeriod time.Duration, batchSize int) (int64, error) {
	return p.callIntervalBatch(ctx, "purge_apply_run_plan", gracePeriod, batchSize)
}

// PurgeVoyages removes batch of completed Voyage-runs from `voyages` registry
// (migration 059) with `finished_at` older than `maxAge`. Retention of growing history
// of runs (implementation of deferred `purge_voyages`, ADR-046 §79). Default
// `maxAge` = 30d (docs/keeper/reaper.md).
//
// Only finished records are deleted (`succeeded`/`failed`/`partial_failed`/
// `cancelled` with `finished_at IS NOT NULL`); `scheduled`/`pending`/`running`
// SQL function does not touch — filter is embedded in `purge_voyages` (075). Age
// is calculated from `finished_at` (parity with PurgeApplyRuns).
//
// Cascade: `voyage_targets` are removed by `ON DELETE CASCADE` (059). soft-links
// `voyage_targets.apply_id`/`errand_id` (to apply_runs/errands) and
// `tidings.voyage_id` (ephemeral) are NOT FK to voyages — purge does not
// delete them and does not leave broken references (ephemeral-Tidings are removed earlier
// by rule `purge_orphan_ephemeral_tidings`). Correlation invariant: window
// by default aligned with `purge_apply_runs` (30d), so drill "voyage →
// apply_runs" does not lose one side (see migration 075).
func (p *Purger) PurgeVoyages(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error) {
	return p.callIntervalBatch(ctx, "purge_voyages", maxAge, batchSize)
}

// PurgePushRuns removes batch of completed push-runs from registry
// `push_runs` (migration 051) with `finished_at` older than `maxAge`. Retention
// of growing run-history on push side (default `maxAge` = 30d,
// docs/keeper/reaper.md). Mirror of PurgeApplyRuns / PurgeVoyages.
//
// Only finished records are deleted (`success`/`partial_failed`/`failed`/
// `cancelled` with `finished_at IS NOT NULL`); `pending`/`running` SQL function
// does not touch — filter is embedded in `purge_push_runs` (076). Age is calculated from
// `finished_at`.
//
// Do NOT confuse with rule `purge_orphan_push_runs` (push_orphan.go): that
// terminates in-flight zombies (pending/running older than TTL → cancelled),
// while this deletes already completed records. Cascade does not exist — per-host
// results are stored inline in `push_runs.summary` (jsonb), no child FK to
// `push_runs` (051).
func (p *Purger) PurgePushRuns(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error) {
	return p.callIntervalBatch(ctx, "purge_push_runs", maxAge, batchSize)
}

// PurgeIncarnationArchive removes batch of archive records of deleted incarnations
// (`incarnation_archive`, migration 039) with `archived_at` older than `maxAge`.
// Retention compliance-class — historical-audit data, so window
// is conservative (default 365d, docs/keeper/reaper.md). Age is calculated from
// `archived_at` (moment of archiving at destroy); filter is embedded in
// `purge_incarnation_archive` (077).
//
// Cascade does not exist — `incarnation_archive` has no child FK tables (039:
// archive intentionally has no referential integrity to live registry). DELETE of broken
// references does not occur.
func (p *Purger) PurgeIncarnationArchive(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error) {
	return p.callIntervalBatch(ctx, "purge_incarnation_archive", maxAge, batchSize)
}

// PurgeStateHistoryArchive removes batch of state_history log archive records
// of deleted incarnations (`state_history_archive`, migration 039) with `archived_at`
// older than `maxAge`. Retention compliance-class (default 365d,
// docs/keeper/reaper.md), parity with PurgeIncarnationArchive. Age is calculated from
// `archived_at`; filter is embedded in `purge_state_history_archive` (077).
//
// Cascade does not exist — no child FK to `state_history_archive` (039).
func (p *Purger) PurgeStateHistoryArchive(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error) {
	return p.callIntervalBatch(ctx, "purge_state_history_archive", maxAge, batchSize)
}

// PurgeArchivedStateHistory physically removes batch of soft-deleted snapshots
// (`archived_at IS NOT NULL`) from LIVE table `state_history` (migration 048) with
// `archived_at` older than `maxAge`. Retention compliance-class (default 365d,
// docs/keeper/reaper.md), parity with PurgeIncarnationArchive.
//
// Do NOT confuse with `archive_state_history` (049 / [Purger.ArchiveStateHistory]): that
// ONLY sets soft-delete flag `archived_at = NOW()` for active snapshots
// beyond last N, while this rule physically removes already soft-deleted rows after
// window expiration. Active snapshots (`archived_at IS NULL`) are NOT touched — filter
// is embedded in `purge_archived_state_history` (077). Age is calculated from `archived_at`
// (moment of soft-delete), not from `at`.
func (p *Purger) PurgeArchivedStateHistory(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error) {
	return p.callIntervalBatch(ctx, "purge_archived_state_history", maxAge, batchSize)
}

// PurgeApplyTaskRegister removes batch of register rows from accumulator
// `apply_task_register` (migration 022) for runs whose `apply_runs` already in
// terminal status (`success`/`failed`/`cancelled`) and completed
// (`finished_at`) older than `gracePeriod`. Default `gracePeriod` = 1h
// (docs/keeper/reaper.md).
//
// Purpose — protective hygiene of transient run-state: register_data —
// plaintext-JSONB of probe results (potentially with secrets), needed
// by scenario-runner exactly once after cross-host barrier for rendering
// state_changes.sets. FK `ON DELETE CASCADE` cleans it cascadingly with
// apply_run (rule `purge_apply_runs`, 30d), but this rule removes register
// earlier — immediately through grace after terminal, reducing plaintext-storage window.
//
// Criterion "terminal status + grace" (not TTL by created_at): register
// of ACTIVE (`running`) run is NEVER deleted, regardless of its
// duration — filter by `apply_runs.status`/`finished_at` is embedded in
// `purge_apply_task_register` (023). Age is calculated from `finished_at`.
func (p *Purger) PurgeApplyTaskRegister(ctx context.Context, gracePeriod time.Duration, batchSize int) (int64, error) {
	return p.callIntervalBatch(ctx, "purge_apply_task_register", gracePeriod, batchSize)
}

// reclaimApplyRunsSQL — recovery-scan of under-delivered Ward (ADR-027 amend, S4).
// Returns to `planned` ONLY jobs that died BEFORE delivery to Soul:
// claimed by dead Acolyte (`status = 'claimed'` with expired lease
// `claim_expires_at < NOW()`), resetting owner and lease
// (`claim_by_kid`/`claim_at`/`claim_expires_at` → NULL).
//
// `dispatched` is NOT reclaimed (change of rule nature, GATE-1 redesign): after
// MarkDispatched, job is delivered to Soul, and run is owned by Soul — re-claim
// dispatched = SECOND SendApply = double apply. `running` (vestigial) also outside
// predicate: Acolyte-flow no longer writes it, and re-claim of hypothetical running
// would be the same double apply. Rule now "finish under-delivered"
// (claimed, died before delivery), not "reclaim zombie-running".
//
// `attempt` is NOT reset: next [applyrun.ClaimNext] increments it
// (fencing-epoch grows), and Keeper-guard on RunResult receipt (S1/S5) cuts off
// stale result of previous attempt by `attempt`. Without this, re-claim of stale
// claimed could conflict with late RunResult — therefore rule
// `reclaim_apply_runs` is enabled ONLY with rolled-out attempt-fencing
// (see docs/keeper/reaper.md).
//
// Idiom `WITH … SELECT count(*)`: UPDATE with subquery over `apply_runs_claim_scan_idx`
// (migration 025, partial-index on `status IN ('planned','claimed','running')` —
// covers `claimed`) + LIMIT $1 batch; outer SELECT returns affected as
// BIGINT — preserves common `queryRower` path of Purger (`QueryRow`), without separate
// SQL function and migration.
//
// One parameter — `$1 batch` (LIMIT). lease is NOT passed in SQL: predicate
// compares `claim_expires_at < NOW()` directly (actual lease embedded in
// claim_expires_at when capturing Ward), therefore no interval argument — otherwise
// PG would not infer unused parameter type (42P18).
//
//	$1 batch  — LIMIT of batch returned per run
const reclaimApplyRunsSQL = `
WITH reclaimed AS (
    UPDATE apply_runs
    SET status           = 'planned',
        claim_by_kid     = NULL,
        claim_at         = NULL,
        claim_expires_at = NULL
    WHERE (apply_id, sid) IN (
        SELECT apply_id, sid
        FROM apply_runs
        WHERE status = 'claimed'
          AND claim_expires_at < NOW()
        ORDER BY claim_expires_at ASC
        LIMIT $1
    )
    RETURNING 1
)
SELECT count(*) FROM reclaimed
`

// ReclaimApplyRuns returns batch of under-delivered Ward (died BEFORE delivery
// to Soul: `status = 'claimed'` with `claim_expires_at < NOW()`) to `planned` for
// re-claim, resetting `claim_by_kid`/`claim_at`/`claim_expires_at`.
// `dispatched`/`running` are NOT reclaimed — after delivery run is owned by Soul,
// re-claim = double apply (ADR-027 amend, S4). `attempt` is PRESERVED —
// fencing-epoch is incremented by next claim, Keeper-guard on RunResult receipt
// cuts off stale result of previous attempt.
// Returns count of returned jobs in batch.
//
// `lease` here — formal argument of duration-rule signature (recovery
// compares `claim_expires_at < NOW()` directly, without offset); value
// is validated (>0) for consistency with other rules, but does not enter predicate.
// `batchSize <= 0` → [defaultBatchSize].
//
// Rule `reclaim_apply_runs` is DISABLED by default — enable only when
// rolled-out attempt-fencing (RunResult receipt), else recovery may conflict
// with stale result.
func (p *Purger) ReclaimApplyRuns(ctx context.Context, lease time.Duration, batchSize int) (int64, error) {
	if lease <= 0 {
		return 0, fmt.Errorf("reaper.reclaim_apply_runs: lease must be > 0, got %v", lease)
	}
	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}
	var count int64
	row := p.pool.QueryRow(ctx, reclaimApplyRunsSQL, batchSize)
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("reaper.reclaim_apply_runs: %w", err)
	}
	return count, nil
}

// ArchiveStateHistory soft-deletes (`archived_at = NOW()`) active snapshots
// of `state_history` beyond last `keepLastN` per incarnation (by `at DESC`),
// optionally excluding snapshots of state_schema migration steps
// (`scenario = 'migration'`, see ADR-019). Implementation — SQL function
// `archive_state_history(integer, boolean, integer)` from migration 049.
// Returns count of marked snapshots in this batch.
//
// `keepLastN <= 0` is rejected without PG call: zero keep would mean
// "archive everything", which is almost certainly config error (`enabled:
// true` without conscious policy choice). Caller (Reaper-runner) substitutes
// default 50 for empty cfg value.
//
// `batchSize <= 0` → [defaultBatchSize]: matches common contract
// of Purger rules.
//
// `keepVersionBump = true` — version-bump-snapshots (scenario='migration')
// are never archived; restorable anchor for migrations ADR-019 (recovery
// of schema on rollback). false — rule archives them equally with regular ones.
func (p *Purger) ArchiveStateHistory(ctx context.Context, keepLastN int, keepVersionBump bool, batchSize int) (int64, error) {
	if keepLastN <= 0 {
		return 0, fmt.Errorf("reaper.archive_state_history: keep_last_n must be > 0, got %d", keepLastN)
	}
	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}
	var count int64
	row := p.pool.QueryRow(ctx, "SELECT archive_state_history($1, $2, $3)", keepLastN, keepVersionBump, batchSize)
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("reaper.archive_state_history: %w", err)
	}
	return count, nil
}

// MarkDisconnected reconciles snapshot `souls.status` with fact of Redis SID-lease in
// BOTH directions (lazy reconcile, ADR-006(a)). Action `set_status` in reaper.md.
// Returns total count of updated rows (disconnect + reconnect).
//
// `souls.status` — "last known" for Operator API, NOT source of presence
// (online/offline is decided by lease). Reconcile brings snapshot to fact in background:
//
//   - connected → disconnected: `last_seen_at` older than `staleAfter` AND no live
//     SID-lease (really dead);
//   - disconnected → connected: alive SID-lease (Soul online; reconnect of already-
//     onboarded Soul does not touch Bootstrap-RPC, eventstream presence in
//     PG is not written on hot-path — snapshot is fixed only by this reconcile).
//
// Without reverse direction, snapshot latched in `disconnected` forever after
// first "break+sweep" (Operator API returned status=disconnected with fresh
// last_seen_at and live lease).
//
// `staleAfter` usually = 90s (docs/keeper/reaper.md), corresponding to
// several heartbeat intervals. Too short value risks
// flapping (connected ↔ disconnected) on network jitter;
// validation — on operator side via semantic phase,
// here only formal sanity (>0).
//
// Lease-aware (Purger built via [NewPurgerWithLease]): rule is two-phase in
// each direction — (1) select PG candidates
// (select_disconnect_candidates / select_reconnect_candidates), (2) verify with
// Redis SID-lease, (3) apply (mark_disconnected_sids / mark_connected_sids).
// This closes both false disconnect of idle Soul (PG `last_seen_at` stale, but
// stream is live), and latch of `disconnected` after reconnect.
//
// Without lease-checker (Purger from [NewPurger], single-instance dev / unit without
// Redis) — fallback to old ONE-WAY pure-SQL rule mark_disconnected
// (migration 014): there stale `last_seen_at` ⇔ no stream by construction (one
// instance), and no latch — reconnect immediately makes `last_seen_at` fresh.
func (p *Purger) MarkDisconnected(ctx context.Context, staleAfter time.Duration, batchSize int) (int64, error) {
	if p.lease == nil {
		return p.callIntervalBatch(ctx, "mark_disconnected", staleAfter, batchSize)
	}
	return p.reconcileLeaseAware(ctx, staleAfter, batchSize)
}

// reconcileLeaseAware — bidirectional lease-aware reconcile (see
// [MarkDisconnected]). Both directions executed in one run; returns
// sum of updated rows. Error in any PG phase interrupts run (return err) —
// next tick will retry; error of Redis-check for specific SID is fail-safe
// skips it (see filterByLease).
func (p *Purger) reconcileLeaseAware(ctx context.Context, staleAfter time.Duration, batchSize int) (int64, error) {
	if staleAfter <= 0 {
		return 0, fmt.Errorf("reaper.mark_disconnected: duration must be > 0, got %v", staleAfter)
	}
	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}

	disconnected, err := p.reconcileDisconnect(ctx, staleAfter, batchSize)
	if err != nil {
		return 0, err
	}
	reconnected, err := p.reconcileReconnect(ctx, batchSize)
	if err != nil {
		return 0, err
	}
	return disconnected + reconnected, nil
}

// reconcileDisconnect — direction connected → disconnected: candidates by stale
// `last_seen_at`, keep those whose lease is DEAD (really dead), mark.
func (p *Purger) reconcileDisconnect(ctx context.Context, staleAfter time.Duration, batchSize int) (int64, error) {
	candidates, err := p.selectDisconnectCandidates(ctx, staleAfter, batchSize)
	if err != nil {
		return 0, err
	}
	stale := p.filterByLease(ctx, candidates, false)
	if len(stale) == 0 {
		return 0, nil
	}
	return p.markDisconnectedSIDs(ctx, stale)
}

// reconcileReconnect — direction disconnected → connected: candidates —
// disconnected-souls (any last_seen), keep those whose lease is ALIVE (Soul
// online), mark back to connected. Closes latch of snapshot.
func (p *Purger) reconcileReconnect(ctx context.Context, batchSize int) (int64, error) {
	candidates, err := p.selectReconnectCandidates(ctx, batchSize)
	if err != nil {
		return 0, err
	}
	online := p.filterByLease(ctx, candidates, true)
	if len(online) == 0 {
		return 0, nil
	}
	return p.markConnectedSIDs(ctx, online)
}

// filterByLease checks each candidate against Redis SID-lease and returns SIDs
// whose lease matches `wantAlive` (true → online candidates for reconnect, false →
// dead for disconnect). Error of Redis-check for specific SID — fail-safe:
// SID is skipped (not marked either way; next run will retry),
// warn in log. Live stream is more important than timeliness of snapshot in both directions.
func (p *Purger) filterByLease(ctx context.Context, candidates []string, wantAlive bool) []string {
	if len(candidates) == 0 {
		return nil
	}
	out := make([]string, 0, len(candidates))
	for _, sid := range candidates {
		alive, checkErr := p.lease.SoulStreamAlive(ctx, sid)
		if checkErr != nil {
			if p.logger != nil {
				p.logger.Warn("reaper.mark_disconnected: lease check failed, skipping (fail-safe)",
					slog.String("sid", sid),
					slog.Bool("want_alive", wantAlive),
					slog.Any("error", checkErr),
				)
			}
			continue
		}
		if alive == wantAlive {
			out = append(out, sid)
		}
	}
	return out
}

// selectDisconnectCandidates — phase 1: SIDs of connected-souls with stale
// `last_seen_at` (SQL function select_disconnect_candidates, migration 043).
func (p *Purger) selectDisconnectCandidates(ctx context.Context, staleAfter time.Duration, batchSize int) ([]string, error) {
	pgInterval := durationToPGInterval(staleAfter)
	rows, err := p.pool.Query(ctx, "SELECT select_disconnect_candidates($1::interval, $2)", pgInterval, batchSize)
	if err != nil {
		return nil, fmt.Errorf("reaper.mark_disconnected: select candidates: %w", err)
	}
	defer rows.Close()

	var sids []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return nil, fmt.Errorf("reaper.mark_disconnected: scan candidate: %w", err)
		}
		sids = append(sids, sid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reaper.mark_disconnected: iter candidates: %w", err)
	}
	return sids, nil
}

// markDisconnectedSIDs — phase 3: mark filtered SIDs (SQL function
// mark_disconnected_sids, migration 043). Returns count of updated rows.
func (p *Purger) markDisconnectedSIDs(ctx context.Context, sids []string) (int64, error) {
	var count int64
	row := p.pool.QueryRow(ctx, "SELECT mark_disconnected_sids($1::text[])", sids)
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("reaper.mark_disconnected: mark sids: %w", err)
	}
	return count, nil
}

// selectReconnectCandidates — phase 1 of reverse direction: SIDs of disconnected-
// souls with any `last_seen_at` (SQL function select_reconnect_candidates, migration
// 043). Without duration predicate — online status is decided by lease, not snapshot freshness.
func (p *Purger) selectReconnectCandidates(ctx context.Context, batchSize int) ([]string, error) {
	rows, err := p.pool.Query(ctx, "SELECT select_reconnect_candidates($1)", batchSize)
	if err != nil {
		return nil, fmt.Errorf("reaper.mark_disconnected: select reconnect candidates: %w", err)
	}
	defer rows.Close()

	var sids []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return nil, fmt.Errorf("reaper.mark_disconnected: scan reconnect candidate: %w", err)
		}
		sids = append(sids, sid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reaper.mark_disconnected: iter reconnect candidates: %w", err)
	}
	return sids, nil
}

// markConnectedSIDs — phase 3 of reverse direction: return disconnected → connected
// for online-SIDs (SQL function mark_connected_sids, migration 043). Returns
// count of updated rows.
func (p *Purger) markConnectedSIDs(ctx context.Context, sids []string) (int64, error) {
	var count int64
	row := p.pool.QueryRow(ctx, "SELECT mark_connected_sids($1::text[])", sids)
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("reaper.mark_disconnected: mark connected sids: %w", err)
	}
	return count, nil
}

// callIntervalBatch — common SQL function call with signature
// `(interval, integer) → BIGINT`. Used for all rules
// that do not have statuses[] parameter.
//
// Validation semantics (`maxAge <= 0` → error without PG, `batchSize <= 0`
// → defaultBatchSize) matches docstring of PurgeAuditOld and
// is fixed by tests per-method.
func (p *Purger) callIntervalBatch(ctx context.Context, fnName string, duration time.Duration, batchSize int) (int64, error) {
	if duration <= 0 {
		return 0, fmt.Errorf("reaper.%s: duration must be > 0, got %v", fnName, duration)
	}
	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}
	var count int64
	pgInterval := durationToPGInterval(duration)
	sql := fmt.Sprintf("SELECT %s($1::interval, $2)", fnName)
	row := p.pool.QueryRow(ctx, sql, pgInterval, batchSize)
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("reaper.%s: %w", fnName, err)
	}
	return count, nil
}

// callStatusesIntervalBatch — common SQL function call with signature
// `(text[], interval, integer) → BIGINT`. Caller guarantees that
// `statuses` is non-empty (see PurgeSouls / PurgeOldSeeds).
func (p *Purger) callStatusesIntervalBatch(ctx context.Context, fnName string, statuses []string, duration time.Duration, batchSize int) (int64, error) {
	if duration <= 0 {
		return 0, fmt.Errorf("reaper.%s: duration must be > 0, got %v", fnName, duration)
	}
	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}
	var count int64
	pgInterval := durationToPGInterval(duration)
	sql := fmt.Sprintf("SELECT %s($1::text[], $2::interval, $3)", fnName)
	row := p.pool.QueryRow(ctx, sql, statuses, pgInterval, batchSize)
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("reaper.%s: %w", fnName, err)
	}
	return count, nil
}

// durationToPGInterval converts time.Duration to Postgres
// interval literal in seconds. Seconds chosen as universal
// format: eliminate Postgres day-precision anomalies
// (`'1 day'::interval` ≠ `'24 hours'::interval` on daylight
// time transitions, see PG docs "9.9.4 Interval Input"), and any duration
// (including sub-second in tests) representable without loss.
func durationToPGInterval(d time.Duration) string {
	return fmt.Sprintf("%d seconds", int64(d.Seconds()))
}
