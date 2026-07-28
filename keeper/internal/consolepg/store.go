// Package consolepg implements [console.RecordingStore] on top of pgxpool.Pool
// — the persistence half of mandatory console session recording (ADR-0074(g),
// NIM-145).
//
// It is a separate package for the reason auditpg is: `keeper/internal/console`
// is imported by code that has no business pulling in pgx, and the split keeps
// the session manager testable without a database.
//
// Postgres rather than a file on the Keeper's disk. Keeper is a stateless
// horizontally-scalable cluster (ADR-002, ADR-005): the instance that held the
// operator's socket is rarely the instance that later serves the playback, so a
// local file would be a recording only one machine can read — and only until it
// is replaced.
package consolepg

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/souls-guild/soul-stack/keeper/internal/console"
)

// DefaultRetention is how long a recording is kept when `console.recording.retention`
// is not set. It is deliberately long: the artifact answers "what did that
// operator actually do", a question that is usually asked well after the fact.
const DefaultRetention = 90 * 24 * time.Hour

// execer is the narrow slice of pgxpool.Pool this store needs, so the guard
// tests can drive it without Postgres (the auditpg pattern).
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

const (
	beginSQL = `
INSERT INTO console_recordings
    (recording_id, session_id, kind, sid, archon_aid, cast_header, started_at, ttl_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
`

	appendSQL = `
INSERT INTO console_recording_parts (recording_id, seq, body)
VALUES ($1, $2, $3)
`

	finishSQL = `
UPDATE console_recordings
SET finished_at = $2, close_reason = $3, event_count = $4, byte_count = $5, truncated = $6
WHERE recording_id = $1
`
)

// Store is the Postgres [console.RecordingStore].
type Store struct {
	pool      execer
	retention time.Duration
}

// NewStore wraps an initialized pool. Ownership of the pool stays with the
// caller. A non-positive retention resolves to [DefaultRetention]: there is no
// "keep forever", because a recording of every console ever opened is a table
// nobody prunes.
func NewStore(pool *pgxpool.Pool, retention time.Duration) *Store {
	return newStoreFromExecer(pool, retention)
}

func newStoreFromExecer(pool execer, retention time.Duration) *Store {
	if retention <= 0 {
		retention = DefaultRetention
	}
	return &Store{pool: pool, retention: retention}
}

// Begin creates the recording row. Its error is what refuses the console: no
// row, no session.
//
// `ttl_at` is baked in here rather than computed at purge time, the way
// `errands.ttl_at` is (migration 052) — one indexed DELETE for the Reaper, and
// a retention change that does not silently re-date recordings already taken.
func (s *Store) Begin(ctx context.Context, meta console.RecordingMeta) error {
	header, err := json.Marshal(meta.Header)
	if err != nil {
		return fmt.Errorf("consolepg: marshal cast header: %w", err)
	}
	started := meta.StartedAt
	if started.IsZero() {
		started = time.Now()
	}
	_, err = s.pool.Exec(ctx, beginSQL,
		meta.RecordingID,
		meta.SessionID,
		string(meta.Kind),
		meta.SID,
		meta.AID,
		header,
		started,
		started.Add(s.retention),
	)
	if err != nil {
		return fmt.Errorf("consolepg: begin recording: %w", err)
	}
	return nil
}

// Append writes one part of the cast body.
func (s *Store) Append(ctx context.Context, recordingID string, seq int64, body string) error {
	if _, err := s.pool.Exec(ctx, appendSQL, recordingID, seq, body); err != nil {
		return fmt.Errorf("consolepg: append recording part %d: %w", seq, err)
	}
	return nil
}

// Finish stamps the terminal state.
func (s *Store) Finish(ctx context.Context, recordingID string, res console.RecordingResult) error {
	finished := res.FinishedAt
	if finished.IsZero() {
		finished = time.Now()
	}
	if _, err := s.pool.Exec(ctx, finishSQL,
		recordingID,
		finished,
		res.CloseReason,
		res.EventCount,
		res.ByteCount,
		res.Truncated,
	); err != nil {
		return fmt.Errorf("consolepg: finish recording: %w", err)
	}
	return nil
}
