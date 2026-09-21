package reaper

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// errandsExecer — narrow interface to pgxpool.Pool needed for the
// `purge_old_errands` rule. Narrowing allows faking in unit tests without
// standing up Postgres; real *pgxpool.Pool satisfies the interface automatically
// (pattern of queryRower / PushRunCanceller).
type errandsExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// purgeOldErrandsSQL — `DELETE FROM errands WHERE ttl_at < NOW()`. `ttl_at`
// is populated on INSERT (`started_at + reaper.errands.ttl`, default
// 7d — see errand.TTLDefault), index `errands_ttl_idx` (migration 052)
// makes the condition cheap-scanable. The rule's `max_age` parameter does
// NOT participate in the predicate (TTL is baked into the row on creation); the
// parameter remains in config for compatibility with runDurationRule runner
// and as a documented override for future ttl-logic migrations.
const purgeOldErrandsSQL = `DELETE FROM errands WHERE ttl_at < NOW()`

// ErrandsPurger — implementation of rule `purge_old_errands` (ADR-033,
// docs/keeper/reaper.md → §purge_old_errands). One batch pass = one
// DELETE via index `errands_ttl_idx`. Run signature compatible with
// runDurationRule invocation by Runner.
//
// The rule's `maxAge` parameter does NOT enter the predicate: TTL is baked into `ttl_at`
// on INSERT by dispatcher (`started_at + errand.TTLDefault`). We preserve
// the argument in the signature for compatibility with the general duration runner,
// but the body ignores it. `batchSize` is also not used (`DELETE` cuts in one
// SQL statement; for millions of TTL-index rows this is not a problem,
// for billions partitioning would be needed — separate task).
type ErrandsPurger struct {
	pool   errandsExecer
	logger *slog.Logger
}

// NewErrandsPurger constructs a purger. logger is nil-safe (warnings
// are suppressed).
func NewErrandsPurger(pool *pgxpool.Pool, logger *slog.Logger) *ErrandsPurger {
	return &ErrandsPurger{pool: pool, logger: logger}
}

// newErrandsPurgerFromExecer — internal constructor for unit tests.
// Public [NewErrandsPurger] fixes *pgxpool.Pool so callers don't
// depend on interface extension.
func newErrandsPurgerFromExecer(pool errandsExecer, logger *slog.Logger) *ErrandsPurger {
	return &ErrandsPurger{pool: pool, logger: logger}
}

// Run executes one rule iteration: `DELETE FROM errands WHERE ttl_at <
// NOW()`. Returns (affected, err): affected — number of actually deleted
// rows. callers (Runner.runDurationRule) will sum this into keeper_reaper_* metrics.
//
// Signature compatible with runDurationRule (`(ctx, duration, batch) → (int64, error)`),
// maxAge/batchSize arguments are ignored (see type doc-comment).
func (p *ErrandsPurger) Run(ctx context.Context, _ time.Duration, _ int) (int64, error) {
	tag, err := p.pool.Exec(ctx, purgeOldErrandsSQL)
	if err != nil {
		return 0, fmt.Errorf("reaper.purge_old_errands: %w", err)
	}
	return tag.RowsAffected(), nil
}
