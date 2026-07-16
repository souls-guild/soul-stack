package reaper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/redis"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// orphanApplyingCandidatesSQL — phase 1 of `reconcile_orphan_applying` rule
// (ADR-027 amend (m)): stale candidates for releasing orphaned applying lock.
//
// Predicate (uses partial index `incarnation_applying_scan_idx`, migration 082):
//   - `status='applying'`        — lock is held;
//   - `applying_since < cutoff`  — lock acquired longer than stale_after ago (cutoff =
//     NOW()-stale_after passed as parameter $1 for clock testability);
//   - `applying_by_kid IS NOT NULL` — epoch is KNOWN. NULL-epoch (legacy/pre-082
//     or applying not via S1-lockRun) is NOT reclaimed — without applying_by_kid there is no
//     presence witness of owner death (documented known-gap, fixed by manual Unlock by operator).
//
// Returns (name, applying_by_kid, applying_apply_id) — phase 2 queries
// Conclave about applying_by_kid, phase 3 releases via applying_apply_id.
const orphanApplyingCandidatesSQL = `
SELECT name, applying_by_kid, applying_apply_id
FROM incarnation
WHERE status = 'applying'
  AND applying_since < $1
  AND applying_by_kid IS NOT NULL
`

// orphanApplyingReleaser — narrow surface for releasing orphaned applying lock
// (incarnation.ReleaseApplyingOrphan). Interface instead of direct package-
// function call keeps rule unit-testable without raising Postgres: fake captures
// arguments and programs outcome (released / no-op / error). Real wire-up —
// thin wrapper over incarnation.ReleaseApplyingOrphan (without changing its
// signature — reuse as-is, ADR-027 amend (m-S1)).
type orphanApplyingReleaser interface {
	ReleaseApplyingOrphan(ctx context.Context, name, orphanApplyID, historyID string) error
}

// orphanApplyingPresence — narrow surface for presence check of keeper instance in
// Conclave (redis.InstanceAlive). Interface keeps rule testable without
// raising Redis. Real wire-up — wrapper over redis.InstanceAlive on top of
// shared redis.Client.
type orphanApplyingPresence interface {
	InstanceAlive(ctx context.Context, kid string) (bool, error)
}

// orphanApplyingQuerier — narrow surface of pgxpool.Pool for phase 1 (SELECT
// candidates). Narrowing allows fake in unit tests (pattern voyagesQuerier).
type orphanApplyingQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// OrphanApplyingReconciler — implementation of `reconcile_orphan_applying` Reaper rule
// (ADR-027 amend (m)): releases orphaned applying lock of incarnation left
// from DIRECT (standalone, not under Voyage) scenario-run of crashed Keeper
// owner. Voyage path is closed by amend (l) via back-link voyage_targets; direct
// run has no back-link — this seam closes it symmetrically, but detects
// orphan via epoch columns of incarnation (applying_by_kid/applying_since/
// applying_apply_id, migration 082).
//
// Three phases in one Run:
//   - (1) SQL candidates — stale applying rows with NON-empty epoch
//     (orphanApplyingCandidatesSQL);
//   - (2) presence — for each candidate InstanceAlive(applying_by_kid). Alive →
//     skip (run is actually going). Dead → phase 3. Presence-check error →
//     fail-safe skip (unknown ⇒ do NOT reclaim to avoid breaking live run
//     on Redis flap);
//   - (3) release — ReleaseApplyingOrphan as-is (FENCING-1 no-live-rival +
//     single-winner CAS applying→ready inside). ErrOrphanLockNotReleased /
//     ErrIncarnationNotFound → no-op (race with honest finalize / incarnation destruction).
//
// presence-death (phase 2) — direct proof of owner death in Conclave,
// replacement for Voyage-FENCING-3 VerifyOwnership (standalone has no Voyage-claim,
// voyage.VerifyOwnership is NOT called).
//
// Per-row audit (reaper.reconcile_orphan_applying.executed) on EACH successful
// release. audit is nil-safe (dev without audit works). Run signature is compatible with
// runDurationRule call from Runner.
type OrphanApplyingReconciler struct {
	pool     orphanApplyingQuerier
	presence orphanApplyingPresence
	releaser orphanApplyingReleaser
	audit    audit.Writer
	logger   *slog.Logger
}

// poolReleaser — production wrapper for incarnation.ReleaseApplyingOrphan on top of
// *pgxpool.Pool. Reuse as-is — package function signature is NOT changed.
type poolReleaser struct {
	pool *pgxpool.Pool
}

func (r poolReleaser) ReleaseApplyingOrphan(ctx context.Context, name, orphanApplyID, historyID string) error {
	return incarnation.ReleaseApplyingOrphan(ctx, r.pool, name, orphanApplyID, historyID)
}

// clientPresence — production wrapper for redis.InstanceAlive on top of redis.Client.
type clientPresence struct {
	client *redis.Client
}

func (p clientPresence) InstanceAlive(ctx context.Context, kid string) (bool, error) {
	return redis.InstanceAlive(ctx, p.client, kid)
}

// NewOrphanApplyingReconciler constructs rule for production wire-up
// (daemon.setupReaper). pool/client are required; audit is nil-safe (emit is skipped),
// logger is nil-safe (warns are suppressed). Presence and releaser wrap shared
// redis.Client / *pgxpool.Pool — incarnation.ReleaseApplyingOrphan is reused
// without signature changes.
func NewOrphanApplyingReconciler(pool *pgxpool.Pool, client *redis.Client, aud audit.Writer, logger *slog.Logger) *OrphanApplyingReconciler {
	return &OrphanApplyingReconciler{
		pool:     pool,
		presence: clientPresence{client: client},
		releaser: poolReleaser{pool: pool},
		audit:    aud,
		logger:   logger,
	}
}

// newOrphanApplyingReconcilerForTest — internal constructor for unit tests
// (fake presence / releaser / querier without raising PG+Redis).
func newOrphanApplyingReconcilerForTest(pool orphanApplyingQuerier, presence orphanApplyingPresence, releaser orphanApplyingReleaser, aud audit.Writer, logger *slog.Logger) *OrphanApplyingReconciler {
	return &OrphanApplyingReconciler{
		pool:     pool,
		presence: presence,
		releaser: releaser,
		audit:    aud,
		logger:   logger,
	}
}

// orphanApplyingCandidate — snapshot of one stale applying row (phase 1).
type orphanApplyingCandidate struct {
	name    string
	prevKID string
	applyID string
}

// Run executes one iteration of the rule: collects stale candidates, releases
// applying lock for dead (presence) owners and emits per-row audit. Returns
// (affected, err): affected — number of ACTUALLY released locks (presence-skip /
// defensive-skip / honest-terminal race not counted; callers add to
// keeper_reaper_* metrics).
//
// Signature is compatible with runDurationRule (`(ctx, duration, batch) → (int64, error)`):
// staleAfter — `stale_after` of rule (cutoff = NOW()-staleAfter), batchSize
// is ignored (number of applying rows per cluster — units/tens, we cut not
// by batch but by partial index).
//
// nil-presence — test affordance: in prod NewOrphanApplyingReconciler ALWAYS
// wraps non-nil clientPresence (reaper branch is gated on non-nil Redis in
// daemon.go), so this branch is unreachable in prod. Real presence gate
// against unavailable Redis — at InstanceAlive level: presence-check error ⇒
// fail-safe skip of candidate (reconcileOne), not no-op of whole rule.
func (r *OrphanApplyingReconciler) Run(ctx context.Context, staleAfter time.Duration, _ int) (int64, error) {
	if r.pool == nil {
		return 0, fmt.Errorf("reaper.reconcile_orphan_applying: pool is nil")
	}
	if r.presence == nil {
		// Test affordance: unreachable in prod (see docstring Run). Real
		// presence gate — InstanceAlive→error fail-safe skip in reconcileOne, not
		// this branch. Without presence client, rule cannot prove owner
		// death — graceful no-op (NOT an error).
		if r.logger != nil {
			r.logger.Info("reaper.reconcile_orphan_applying: skipped — presence client not set (test-affordance)")
		}
		return 0, nil
	}

	cutoff := time.Now().Add(-staleAfter)
	rows, err := r.pool.Query(ctx, orphanApplyingCandidatesSQL, cutoff)
	if err != nil {
		return 0, fmt.Errorf("reaper.reconcile_orphan_applying: query candidates: %w", err)
	}

	// Read candidates BEFORE presence/release I/O — don't keep cursor open during
	// Redis EXISTS check and CAS tx in PG (pattern VoyageReclaimer).
	var candidates []orphanApplyingCandidate
	for rows.Next() {
		var (
			name    string
			prevKID *string
			applyID *string
		)
		if scanErr := rows.Scan(&name, &prevKID, &applyID); scanErr != nil {
			rows.Close()
			return 0, fmt.Errorf("reaper.reconcile_orphan_applying: scan: %w", scanErr)
		}
		c := orphanApplyingCandidate{name: name}
		if prevKID != nil {
			c.prevKID = *prevKID
		}
		if applyID != nil {
			c.applyID = *applyID
		}
		candidates = append(candidates, c)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		rows.Close()
		return 0, fmt.Errorf("reaper.reconcile_orphan_applying: rows: %w", rowsErr)
	}
	rows.Close()

	var affected int64
	for _, c := range candidates {
		if r.reconcileOne(ctx, c) {
			affected++
		}
	}
	return affected, nil
}

// reconcileOne processes one candidate: presence check → (dead) release.
// Returns true ONLY on actual lock release (for affected counter).
func (r *OrphanApplyingReconciler) reconcileOne(ctx context.Context, c orphanApplyingCandidate) bool {
	// defensive-skip: empty epoch is unreachable with correct lockRun (SQL filter
	// already filtered applying_by_kid IS NULL; applying_apply_id is written atomically with it),
	// but fail-safe from legacy / manual row edits — we do NOT release without full epoch.
	if c.prevKID == "" || c.applyID == "" {
		if r.logger != nil {
			r.logger.Warn("reaper.reconcile_orphan_applying: defensive-skip — incomplete epoch",
				slog.String("incarnation", c.name),
				slog.String("prev_kid", c.prevKID),
				slog.String("apply_id", c.applyID))
		}
		return false
	}

	// Phase 2 — presence: is the lock owner alive in Conclave.
	alive, err := r.presence.InstanceAlive(ctx, c.prevKID)
	if err != nil {
		// fail-safe: presence unknown (Redis flap) → do NOT declare dead, do NOT
		// reclaim (live run may be in progress). Warn for triage.
		if r.logger != nil {
			r.logger.Warn("reaper.reconcile_orphan_applying: presence check failed — skip (fail-safe)",
				slog.String("incarnation", c.name),
				slog.String("prev_kid", c.prevKID),
				slog.Any("error", err))
		}
		return false
	}
	if alive {
		// Owner is alive — run is actually going, lock is NOT orphaned.
		return false
	}

	// Phase 3 — release: owner is dead in Conclave. ReleaseApplyingOrphan as-is
	// (FENCING-1 no-live-rival + single-winner CAS inside). historyID generated
	// here (identical to Voyage adapter).
	historyID := audit.NewULID()
	if relErr := r.releaser.ReleaseApplyingOrphan(ctx, c.name, c.applyID, historyID); relErr != nil {
		switch {
		case errors.Is(relErr, incarnation.ErrOrphanLockNotReleased):
			// no-op: honest finalize of previous owner already moved row out of applying
			// (single-winner) OR live rival holds foreign apply_id (FENCING-1).
			return false
		case errors.Is(relErr, incarnation.ErrIncarnationNotFound):
			// Incarnation destroyed between phase 1 and release — nothing to release.
			return false
		default:
			if r.logger != nil {
				r.logger.Error("reaper.reconcile_orphan_applying: lock release failed",
					slog.String("incarnation", c.name),
					slog.String("prev_kid", c.prevKID),
					slog.Any("error", relErr))
			}
			return false
		}
	}

	r.emitExecuted(ctx, c)
	if r.logger != nil {
		r.logger.Info("reaper.reconcile_orphan_applying: orphaned applying lock released",
			slog.String("incarnation", c.name),
			slog.String("prev_kid", c.prevKID),
			slog.String("apply_id", c.applyID))
	}
	return true
}

// emitExecuted writes reaper.reconcile_orphan_applying.executed (ADR-027 amend (m),
// area reaper.*). source=keeper_internal, archon_aid="" (NULL). nil-safe.
func (r *OrphanApplyingReconciler) emitExecuted(ctx context.Context, c orphanApplyingCandidate) {
	if r.audit == nil {
		return
	}
	ev := &audit.Event{
		EventType: audit.EventReconcileOrphanApplyingExecuted,
		Source:    audit.SourceKeeperInternal,
		Payload: map[string]any{
			"incarnation": c.name,
			"prev_kid":    c.prevKID,
			"apply_id":    c.applyID,
		},
	}
	if err := r.audit.Write(ctx, ev); err != nil && r.logger != nil {
		r.logger.Warn("reaper.reconcile_orphan_applying: audit write failed",
			slog.String("incarnation", c.name), slog.Any("error", err))
	}
}
