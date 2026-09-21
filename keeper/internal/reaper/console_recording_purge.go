package reaper

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// purgeOldConsoleRecordingsSQL — `DELETE FROM console_recordings WHERE ttl_at <
// NOW()`. `ttl_at` is `started_at + console.recording.retention` (default 90d),
// baked into the row on INSERT by consolepg.Store, and the index
// `console_recordings_ttl_idx` (migration 104) makes the condition
// cheap-scanable. The body rows go with it through the `ON DELETE CASCADE` on
// `console_recording_parts`.
//
// Exactly the shape of `purge_old_errands`: the rule's `max_age` parameter does
// NOT enter the predicate, because the TTL a recording was taken under is a
// property of that recording. Retention is not retroactive — lowering it must
// not silently delete recordings an operator kept under the old policy, and
// raising it must not resurrect the expectation that older ones are still there.
const purgeOldConsoleRecordingsSQL = `DELETE FROM console_recordings WHERE ttl_at < NOW()`

// ConsoleRecordingsPurger implements the rule `purge_old_console_recordings`
// (ADR-0074(g), NIM-145).
//
// Console recording is mandatory, so this table grows with every shell anybody
// opens and there is no operator action that stops it. That is exactly why the
// retention has to be enforced by something rather than merely recorded in a
// column: a mandatory recorder with no purge is a disk-growth bug with an audit
// story attached.
type ConsoleRecordingsPurger struct {
	pool   errandsExecer
	logger *slog.Logger
}

// NewConsoleRecordingsPurger constructs a purger. logger is nil-safe.
func NewConsoleRecordingsPurger(pool *pgxpool.Pool, logger *slog.Logger) *ConsoleRecordingsPurger {
	return &ConsoleRecordingsPurger{pool: pool, logger: logger}
}

// newConsoleRecordingsPurgerFromExecer is the unit-test constructor; the public
// one fixes *pgxpool.Pool so callers do not depend on the narrow interface.
func newConsoleRecordingsPurgerFromExecer(pool errandsExecer, logger *slog.Logger) *ConsoleRecordingsPurger {
	return &ConsoleRecordingsPurger{pool: pool, logger: logger}
}

// Run executes one rule iteration. Signature compatible with runDurationRule;
// maxAge and batchSize are ignored (see the type doc-comment).
func (p *ConsoleRecordingsPurger) Run(ctx context.Context, _ time.Duration, _ int) (int64, error) {
	tag, err := p.pool.Exec(ctx, purgeOldConsoleRecordingsSQL)
	if err != nil {
		return 0, fmt.Errorf("reaper.purge_old_console_recordings: %w", err)
	}
	return tag.RowsAffected(), nil
}
