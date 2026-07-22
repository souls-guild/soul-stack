package toll

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// BaselineReader — narrow surface for the Leader: return the current snapshot
// count of `souls.status='connected'`. Default implementation — [PGBaselineReader]
// on top of pgxpool; the interface lets Leader unit tests use a fake instead
// of a live PG (see leader_test.go::fakeBaseline).
type BaselineReader interface {
	BaselineConnected(ctx context.Context) (int64, error)
}

// PGQuerier — narrow `QueryRow` contract on top of *pgxpool.Pool / pgx.Conn.
// Narrowing to one method keeps [PGBaselineReader] lightweight to test-fake
// and lets it accept any pgx source (pool/conn/tx). Matches the signature of
// pgxpool.Pool.QueryRow.
type PGQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// cachedBaseline — Leader-side cache with a TTL. SELECT COUNT(*) FROM souls is
// cheap (indexed on status), but the Leader ticks every 5s — a 60s cache
// ([Config.WindowSize]) cuts PG load by a factor of 12. Mutex — because the cache
// lives across ticks (a single Leader loop), but the Leader loop is one goroutine;
// the mutex is there in case of future extension (e.g. exposing baseline reads
// on /metrics).
type cachedBaseline struct {
	reader BaselineReader
	ttl    time.Duration

	mu        sync.Mutex
	value     int64
	fetchedAt time.Time
	hasValue  bool
}

func newCachedBaseline(reader BaselineReader, ttl time.Duration) *cachedBaseline {
	return &cachedBaseline{reader: reader, ttl: ttl}
}

// get returns the cached value if it's still fresh, otherwise fetches. On a
// fetch error — returns err AND the stale value (if any): the leader itself
// decides whether to use the stale value (better than a false-positive bug on
// an empty baseline) or skip the tick. The now parameter is for testability
// (tests inject the clock).
func (c *cachedBaseline) get(ctx context.Context, now time.Time) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.hasValue && now.Sub(c.fetchedAt) < c.ttl {
		return c.value, nil
	}
	v, err := c.reader.BaselineConnected(ctx)
	if err != nil {
		// Stale-fallback: if a value already existed — return it AND the error.
		// The caller (Leader) tells by err != nil and decides for itself.
		if c.hasValue {
			return c.value, err
		}
		return 0, err
	}
	c.value = v
	c.fetchedAt = now
	c.hasValue = true
	return v, nil
}

// PGBaselineReader — production impl on top of pgxpool. SELECT COUNT(*) FROM
// souls WHERE status='connected'. The status column is read-only in this query
// (ADR-006 amend: presence is now a Redis lease, but souls.status as the last
// known-good is a good enough approximation of the baseline for a cluster-level
// metric).
type PGBaselineReader struct {
	pool PGQuerier
}

// NewPGBaselineReader assembles a reader on top of pgxpool (via the pgxToQuerier adapter).
// Accepts a narrow [PGQuerier]: the caller (daemon) wraps *pgxpool.Pool with a
// trivial adapter so the toll package doesn't pull in pgx as a direct dep.
func NewPGBaselineReader(pool PGQuerier) (*PGBaselineReader, error) {
	if pool == nil {
		return nil, errors.New("toll.NewPGBaselineReader: nil pool")
	}
	return &PGBaselineReader{pool: pool}, nil
}

// BaselineConnected — SELECT COUNT(*) FROM souls WHERE status='connected'.
//
// Returns 0 without an error on an empty table (the normal path for a fresh
// cluster) — the Leader interprets baseline=0 as "nothing to divide by, ratio
// is undefined, don't raise degraded" (ADR-038 protection against division by zero).
func (r *PGBaselineReader) BaselineConnected(ctx context.Context) (int64, error) {
	var n int64
	row := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM souls WHERE status = 'connected'`)
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("toll.BaselineConnected: scan: %w", err)
	}
	return n, nil
}
