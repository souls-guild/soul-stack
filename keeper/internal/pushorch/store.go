package pushorch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PushRunStatus is the set of terminal/in-flight statuses for `push_runs` record
// (migration 051). Synchronized with CHECK push_runs_status_valid.
type PushRunStatus string

const (
	StatusPending       PushRunStatus = "pending"
	StatusRunning       PushRunStatus = "running"
	StatusSuccess       PushRunStatus = "success"
	StatusPartialFailed PushRunStatus = "partial_failed"
	StatusFailed        PushRunStatus = "failed"
	StatusCancelled     PushRunStatus = "cancelled"
)

// PushRunRow is a read projection of `push_runs` record (for GET /v1/push/{apply_id}).
// summary is stored as map[string]any (jsonb) — caller serializes to response.
// inventory_sids is TEXT[]; ssh_provider/started_by_aid are nullable.
type PushRunRow struct {
	ApplyID       string
	InventorySIDs []string
	DestinyRef    string
	SSHProvider   string
	Input         map[string]any
	CleanupStale  bool
	Status        PushRunStatus
	StartedAt     time.Time
	FinishedAt    *time.Time
	StartedByAID  string
	StartedByKID  string
	Summary       map[string]any
}

// ExecQueryRower is a narrow interface of pgxpool.Pool needed by Store. Symmetric to
// soul.ExecQueryRower / topology.Querier: allows mocking in unit tests without PG.
type ExecQueryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// ErrNotFound is returned when a push_runs record by apply_id is not found (GET handler).
var ErrNotFound = errors.New("pushorch: push_run not found")

// Store is a read/write CRUD interface to `push_runs` table.
//
// Concurrent-safe: each method is one query; holds no in-memory state. Database-level
// atomicity (single UPDATE with RowsAffected) serves as barrier for single-winner
// between orchestrator goroutine and Reaper purge.
type Store struct {
	db ExecQueryRower
}

// NewStore конструирует Store. db обязателен.
func NewStore(db ExecQueryRower) *Store {
	return &Store{db: db}
}

const insertPushRunSQL = `
INSERT INTO push_runs (
    apply_id, inventory_sids, destiny_ref, ssh_provider, input,
    cleanup_stale, status, started_at, started_by_aid, started_by_kid
) VALUES (
    $1, $2, $3, NULLIF($4, ''), $5,
    $6, $7, NOW(), $8, $9
)
`

// Insert creates a new push_runs record with status `pending`. apply_id is ULID,
// validated by caller (audit.IsValidULID). Stored exactly once (orchestrator spawns
// goroutine after successful INSERT). input/summary are jsonb; empty input → empty
// object (PG DEFAULT '{}'::jsonb would set it, but we send explicitly for predictability).
func (s *Store) Insert(ctx context.Context, row PushRunRow) error {
	inputJSON, err := marshalJSONB(row.Input)
	if err != nil {
		return fmt.Errorf("pushorch: marshal input: %w", err)
	}
	var startedByAID any = row.StartedByAID
	if row.StartedByAID == "" {
		startedByAID = nil
	}
	_, err = s.db.Exec(ctx, insertPushRunSQL,
		row.ApplyID,
		row.InventorySIDs,
		row.DestinyRef,
		row.SSHProvider,
		inputJSON,
		row.CleanupStale,
		string(StatusPending),
		startedByAID,
		row.StartedByKID,
	)
	if err != nil {
		return fmt.Errorf("pushorch: insert push_runs: %w", err)
	}
	return nil
}

const selectPushRunSQL = `
SELECT apply_id, inventory_sids, destiny_ref, COALESCE(ssh_provider, ''),
       input, cleanup_stale, status, started_at, finished_at,
       COALESCE(started_by_aid, ''), started_by_kid, COALESCE(summary, '{}'::jsonb)
FROM push_runs
WHERE apply_id = $1
`

// Get reads a record by apply_id. ErrNotFound means record does not exist (caller → 404).
func (s *Store) Get(ctx context.Context, applyID string) (*PushRunRow, error) {
	row := s.db.QueryRow(ctx, selectPushRunSQL, applyID)
	var (
		r           PushRunRow
		statusStr   string
		finishedAt  *time.Time
		inputJSON   []byte
		summaryJSON []byte
	)
	if err := row.Scan(
		&r.ApplyID,
		&r.InventorySIDs,
		&r.DestinyRef,
		&r.SSHProvider,
		&inputJSON,
		&r.CleanupStale,
		&statusStr,
		&r.StartedAt,
		&finishedAt,
		&r.StartedByAID,
		&r.StartedByKID,
		&summaryJSON,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("pushorch: select push_run: %w", err)
	}
	r.Status = PushRunStatus(statusStr)
	r.FinishedAt = finishedAt
	if len(inputJSON) > 0 {
		if err := json.Unmarshal(inputJSON, &r.Input); err != nil {
			return nil, fmt.Errorf("pushorch: unmarshal input: %w", err)
		}
	}
	if len(summaryJSON) > 0 {
		if err := json.Unmarshal(summaryJSON, &r.Summary); err != nil {
			return nil, fmt.Errorf("pushorch: unmarshal summary: %w", err)
		}
	}
	return &r, nil
}

const updateStatusRunningSQL = `
UPDATE push_runs
SET status = $2
WHERE apply_id = $1
`

// MarkRunning transitions pending → running. Idempotent single-step without guard
// on current status — orchestrator calls exactly once AFTER successful Insert.
func (s *Store) MarkRunning(ctx context.Context, applyID string) error {
	if _, err := s.db.Exec(ctx, updateStatusRunningSQL, applyID, string(StatusRunning)); err != nil {
		return fmt.Errorf("pushorch: mark running: %w", err)
	}
	return nil
}

const updateTerminalSQL = `
UPDATE push_runs
SET status      = $2,
    summary     = $3,
    finished_at = NOW()
WHERE apply_id = $1
`

// MarkTerminal sets final status (success/failed/partial_failed/cancelled),
// summary, and finished_at=NOW(). Single UPDATE in one transaction.
func (s *Store) MarkTerminal(ctx context.Context, applyID string, status PushRunStatus, summary map[string]any) error {
	summaryJSON, err := marshalJSONB(summary)
	if err != nil {
		return fmt.Errorf("pushorch: marshal summary: %w", err)
	}
	if _, err := s.db.Exec(ctx, updateTerminalSQL, applyID, string(status), summaryJSON); err != nil {
		return fmt.Errorf("pushorch: mark terminal: %w", err)
	}
	return nil
}

const cancelOrphanSQL = `
UPDATE push_runs
SET status      = 'cancelled',
    summary     = COALESCE(summary, '{}'::jsonb) || jsonb_build_object(
        'orphan_purged', true,
        'reason', $2
    ),
    finished_at = NOW()
WHERE apply_id = $1
  AND status IN ('pending', 'running')
`

// CancelOrphan transitions in-flight (pending/running) record to `cancelled` with
// orphan_purged marker in summary. Used by Reaper rule purge_orphan_push_runs for
// runs whose Keeper instance died during execution. WHERE-guard (status IN pending/running)
// — single-winner: race with real MarkTerminal loses (rows_affected==0).
func (s *Store) CancelOrphan(ctx context.Context, applyID, reason string) (bool, error) {
	tag, err := s.db.Exec(ctx, cancelOrphanSQL, applyID, reason)
	if err != nil {
		return false, fmt.Errorf("pushorch: cancel orphan %s: %w", applyID, err)
	}
	return tag.RowsAffected() > 0, nil
}

const listOrphansSQL = `
SELECT apply_id
FROM push_runs
WHERE status IN ('pending', 'running')
  AND started_at < NOW() - $1::interval
ORDER BY started_at ASC
LIMIT $2
`

// ListOrphans returns apply_id values of in-flight runs older than maxAge (TTL for
// purge_orphan_push_runs). Symmetric to reaper-purger functions: one batch, LIMIT $2.
// Used by Reaper for subsequent CancelOrphan on each.
func (s *Store) ListOrphans(ctx context.Context, maxAge time.Duration, batchSize int) ([]string, error) {
	rows, err := s.db.Query(ctx, listOrphansSQL, maxAge, batchSize)
	if err != nil {
		return nil, fmt.Errorf("pushorch: list orphans: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("pushorch: scan orphan: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pushorch: orphans iter: %w", err)
	}
	return out, nil
}

// marshalJSONB encodes map to jsonb-bytes. nil → "{}".
func marshalJSONB(m map[string]any) ([]byte, error) {
	if m == nil {
		return []byte(`{}`), nil
	}
	return json.Marshal(m)
}

// ListFilter is a set of filters for [Store.SelectAll] global list endpoint
// `GET /v1/push-runs` (UI-4). Statuses is multi-value filter (multiple values
// combined via OR); empty slice means no status filter. SSHProvider is exact-match;
// empty string means all providers. Invalid status — caller-handler rejects with 422
// before calling CRUD.
type ListFilter struct {
	Statuses    []PushRunStatus
	SSHProvider string
}

// SQL for global list endpoint `GET /v1/push-runs` (UI-4). Parameters:
//
//	$1 — text[] of statuses (NULL/empty → no filter; non-empty → status = ANY($1));
//	$2 — ssh_provider (NULL/empty → no filter; non-empty → exact match);
//	$3 — limit, $4 — offset.
//
// `cardinality($1::text[]) = 0` is canonical "multi-value filter is optional" form
// (same as tide.SelectAll): empty array means "not specified", not "matches nothing".
const selectAllPushRunsSQL = `
SELECT apply_id, inventory_sids, destiny_ref, COALESCE(ssh_provider, ''),
       input, cleanup_stale, status, started_at, finished_at,
       COALESCE(started_by_aid, ''), started_by_kid, COALESCE(summary, '{}'::jsonb)
FROM push_runs
WHERE ($1::text[] IS NULL OR cardinality($1::text[]) = 0 OR status = ANY($1::text[]))
  AND ($2::text IS NULL OR ssh_provider = $2)
ORDER BY started_at DESC
LIMIT $3 OFFSET $4
`

const countAllPushRunsSQL = `
SELECT COUNT(*) FROM push_runs
WHERE ($1::text[] IS NULL OR cardinality($1::text[]) = 0 OR status = ANY($1::text[]))
  AND ($2::text IS NULL OR ssh_provider = $2)
`

// SelectAll returns a page of push_runs records for the global list endpoint
// `GET /v1/push-runs` (UI-4). Sorted by `started_at DESC` (fresh first).
//
// Total is the total count of records matching the filter (separate COUNT). limit/offset
// are not validated — caller-handler already passed through sharedapi.ParsePage.
//
// summary is present in full in result rows (forward-compat: list endpoint does not
// yet trim hosts[] on client-side; UI-4 spec promises "subset without summary.hosts",
// but that is visible part of DTO mapping in handler, not SQL projection).
func (s *Store) SelectAll(ctx context.Context, filter ListFilter, offset, limit int) ([]*PushRunRow, int, error) {
	var statusesArg any
	if len(filter.Statuses) > 0 {
		ss := make([]string, len(filter.Statuses))
		for i, st := range filter.Statuses {
			ss[i] = string(st)
		}
		statusesArg = ss
	}
	var providerArg any
	if filter.SSHProvider != "" {
		providerArg = filter.SSHProvider
	}

	var total int
	if err := s.db.QueryRow(ctx, countAllPushRunsSQL, statusesArg, providerArg).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("pushorch: count all: %w", err)
	}

	rows, err := s.db.Query(ctx, selectAllPushRunsSQL, statusesArg, providerArg, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("pushorch: select all: %w", err)
	}
	defer rows.Close()

	out := make([]*PushRunRow, 0, limit)
	for rows.Next() {
		r := &PushRunRow{}
		var (
			statusStr   string
			finishedAt  *time.Time
			inputJSON   []byte
			summaryJSON []byte
		)
		if err := rows.Scan(
			&r.ApplyID,
			&r.InventorySIDs,
			&r.DestinyRef,
			&r.SSHProvider,
			&inputJSON,
			&r.CleanupStale,
			&statusStr,
			&r.StartedAt,
			&finishedAt,
			&r.StartedByAID,
			&r.StartedByKID,
			&summaryJSON,
		); err != nil {
			return nil, 0, fmt.Errorf("pushorch: select all scan: %w", err)
		}
		r.Status = PushRunStatus(statusStr)
		r.FinishedAt = finishedAt
		if len(inputJSON) > 0 {
			if err := json.Unmarshal(inputJSON, &r.Input); err != nil {
				return nil, 0, fmt.Errorf("pushorch: select all unmarshal input: %w", err)
			}
		}
		if len(summaryJSON) > 0 {
			if err := json.Unmarshal(summaryJSON, &r.Summary); err != nil {
				return nil, 0, fmt.Errorf("pushorch: select all unmarshal summary: %w", err)
			}
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("pushorch: select all iter: %w", err)
	}
	return out, total, nil
}

// ValidStatus reports whether status is in [PushRunStatus] enum. Used by caller-handler
// for `GET /v1/push-runs` to validate query filter before CRUD call (422 before SQL access).
func ValidStatus(s PushRunStatus) bool {
	switch s {
	case StatusPending, StatusRunning, StatusSuccess, StatusPartialFailed, StatusFailed, StatusCancelled:
		return true
	}
	return false
}
