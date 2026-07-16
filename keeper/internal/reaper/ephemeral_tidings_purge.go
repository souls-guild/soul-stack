package reaper

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ephemeralTidingsExecer — narrow interface to pgxpool.Pool needed for the
// `purge_orphan_ephemeral_tidings` rule. Narrowing allows faking in unit tests
// without standing up Postgres; real *pgxpool.Pool satisfies automatically
// (pattern of errandsExecer / orphanPurger).
type ephemeralTidingsExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// purgeOrphanEphemeralTidingsSQL — cleanup of orphaned ephemeral Tidings
// (ADR-052(g) amendment N2, cleanup). Ephemeral rule is tied to one Voyage;
// the terminal of the run should remove its subscription. This rule is a SAFEGUARD:
// removes an ephemeral row if its run either
//   - does not exist (voyages row was deleted / run was never created —
//     tx rollback excludes orphaning by construction, but protection against manual
//     interference / future paths);
//   - is in TERMINAL longer than grace period ($1) — by grace time tap is guaranteed to have
//     matched the terminal event against the rule and enqueued a notification
//     (dispatcher works asynchronously through bounded channel; synchronous removal at
//     finalize time would race delivery — see ADR-052(g) "Cleanup").
//
// Grace is a mandatory correctness condition, not cosmetics: without it the rule,
// firing exactly on terminal, would delete Tiding BEFORE tap-consumer
// finishes reading the event from the buffer → completion notification would not be sent.
//
// Uses partial index `tidings_ephemeral_voyage_idx` (migration 072,
// WHERE ephemeral): permanent rules don't appear in scan. One DELETE in one
// statement (ephemeral rules are few — tens per in-flight runs).
const purgeOrphanEphemeralTidingsSQL = `
DELETE FROM tidings t
WHERE t.ephemeral
  AND (
        NOT EXISTS (
            SELECT 1 FROM voyages v WHERE v.voyage_id = t.voyage_id
        )
        OR EXISTS (
            SELECT 1 FROM voyages v
            WHERE v.voyage_id = t.voyage_id
              AND v.status IN ('succeeded', 'failed', 'partial_failed', 'cancelled')
              AND v.finished_at < NOW() - $1::interval
        )
      )`

// EphemeralTidingsPurger — implementation of rule `purge_orphan_ephemeral_tidings`
// (ADR-052(g) amendment N2, docs/keeper/reaper.md). One batch pass = one
// DELETE via partial index `tidings_ephemeral_voyage_idx`. Run signature
// compatible with runDurationRule invocation by Runner (parity ErrandsPurger /
// orphanPurger).
//
// Unlike `purge_old_errands` (TTL baked into the row), here the rule's `maxAge`
// is GRACE after Voyage terminal, ENTERING the predicate as an interval (parity
// `purge_apply_task_register`: max_age-as-grace). batchSize is not used
// (one DELETE statement; ephemeral rules are few).
type EphemeralTidingsPurger struct {
	pool   ephemeralTidingsExecer
	logger *slog.Logger
}

// NewEphemeralTidingsPurger constructs a purger. logger is nil-safe.
func NewEphemeralTidingsPurger(pool *pgxpool.Pool, logger *slog.Logger) *EphemeralTidingsPurger {
	return &EphemeralTidingsPurger{pool: pool, logger: logger}
}

// newEphemeralTidingsPurgerFromExecer — internal constructor for unit tests.
// Public [NewEphemeralTidingsPurger] fixes *pgxpool.Pool so callers
// don't depend on interface extension.
func newEphemeralTidingsPurgerFromExecer(pool ephemeralTidingsExecer, logger *slog.Logger) *EphemeralTidingsPurger {
	return &EphemeralTidingsPurger{pool: pool, logger: logger}
}

// Run executes one rule iteration: cleanup of orphaned ephemeral Tidings
// (run does not exist OR in terminal > grace). grace is passed as an interval
// to the predicate. Returns (affected, err): affected — number of deleted rows
// (Runner.runDurationRule will sum into keeper_reaper_* metrics).
//
// Signature compatible with runDurationRule (`(ctx, duration, batch) → (int64, error)`);
// batchSize argument is ignored (see type doc-comment).
func (p *EphemeralTidingsPurger) Run(ctx context.Context, grace time.Duration, _ int) (int64, error) {
	// pgx accepts time.Duration as Postgres interval directly (microsecond precision).
	tag, err := p.pool.Exec(ctx, purgeOrphanEphemeralTidingsSQL, grace)
	if err != nil {
		return 0, fmt.Errorf("reaper.purge_orphan_ephemeral_tidings: %w", err)
	}
	return tag.RowsAffected(), nil
}
