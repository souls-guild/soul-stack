// Package errand — Keeper-side orchestrator of pull-ad-hoc Errands
// (`POST /v1/souls/{sid}/exec`, ADR-033).
//
// One Errand circuit: Operator (HTTP/MCP) → Dispatcher.Dispatch →
// Outbound.SendErrand → Soul EventStream → ErrandResult in FromSoul →
// events_errand.handleErrandResult → ApplyBus.Publish(KindErrandCompleted) →
// Dispatcher subscribe-waiter completes → MarkTerminal in errands-table.
//
// Cross-keeper: SID-stream holder lives in Redis-lease (`soul:<sid>:lock`).
// Local-holder → Outbound.SendErrand. Remote-holder → publish to
// `outbound:<sid>` pub/sub (same channel as apply/cancel). Reply
// travels through applybus channel `apply:<errand_id>` (cluster-bridge).
//
// State invariant: Errand does NOT mutate incarnation.state (ADR-033 §4).
// No ApplyRunDB, no state_changes, no barrier here — only its own
// `errands` table with two terminal transitions (running → terminal).
package errand

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Status — terminal/in-flight status of `errands` row. Matches CHECK
// `errands_status_valid` and enum conversion from proto (see mapping in
// dispatcher.go::statusFromProto).
type Status string

const (
	StatusRunning          Status = "running"
	StatusSuccess          Status = "success"
	StatusFailed           Status = "failed"
	StatusTimedOut         Status = "timed_out"
	StatusCancelled        Status = "cancelled"
	StatusModuleNotAllowed Status = "module_not_allowed"
)

// ErrNotFound — record by errand_id is missing (handler → 404).
var ErrNotFound = errors.New("errand: not found")

// Row — read/write projection of `errands` row. Nullable fields (exit_code,
// finished_at, output, stdout/stderr) are pointers; NULL in DB becomes nil.
// Not all fields are required for Insert constructor (see method).
type Row struct {
	ErrandID        string
	SID             string
	Module          string
	Input           map[string]any
	Status          Status
	ExitCode        *int32
	Stdout          string
	Stderr          string
	StdoutTruncated bool
	StderrTruncated bool
	DurationMs      *int64
	ErrorMessage    string
	Output          map[string]any
	StartedByAID    string
	StartedByKID    string
	StartedAt       time.Time
	FinishedAt      *time.Time
	TTLAt           time.Time
}

// ListFilter — parameters for `GET /v1/errands`. Empty fields = "no filter".
// StartedAfter in UTC; pagination is external (offset/limit, paged response).
// Modules — multi-value exact-match OR (`module IN (…)`), query parameter
// `?module=X&module=Y`.
type ListFilter struct {
	SID          string
	Status       Status
	StartedAfter time.Time
	Modules      []string
}

// ExecQueryRower — narrow surface of pgxpool.Pool, needed by Store
// (symmetric with pushorch / soul / topology). Allows faking in unit tests
// without PG.
type ExecQueryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Store — CRUD wrapper over `errands`. Concurrent-safe; does not maintain
// its own in-memory state.
type Store struct {
	db ExecQueryRower
}

// NewStore constructs Store; db is required.
func NewStore(db ExecQueryRower) *Store {
	return &Store{db: db}
}

const insertSQL = `
INSERT INTO errands (
    errand_id, sid, module, input, status,
    started_by_aid, started_by_kid, started_at, ttl_at
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, $9
)
`

// Insert inserts initial Errand row with status='running'. errand_id is
// ULID, validated by caller. input/output are jsonb; empty input → '{}'::jsonb
// (explicitly serialized for predictability, like pushorch).
func (s *Store) Insert(ctx context.Context, row Row) error {
	inputJSON, err := marshalJSONB(row.Input)
	if err != nil {
		return fmt.Errorf("errand: marshal input: %w", err)
	}
	if _, err := s.db.Exec(ctx, insertSQL,
		row.ErrandID,
		row.SID,
		row.Module,
		inputJSON,
		string(row.Status),
		row.StartedByAID,
		row.StartedByKID,
		row.StartedAt.UTC(),
		row.TTLAt.UTC(),
	); err != nil {
		return fmt.Errorf("errand: insert: %w", err)
	}
	return nil
}

const selectByIDSQL = `
SELECT errand_id, sid, module, input, status,
       exit_code, COALESCE(stdout, ''), COALESCE(stderr, ''),
       stdout_truncated, stderr_truncated,
       duration_ms, COALESCE(error_message, ''),
       output, started_by_aid, started_by_kid,
       started_at, finished_at, ttl_at
FROM errands
WHERE errand_id = $1
`

// Get reads Errand row by ULID. Returns ErrNotFound if missing.
func (s *Store) Get(ctx context.Context, errandID string) (*Row, error) {
	r := s.db.QueryRow(ctx, selectByIDSQL, errandID)
	row, err := scanRow(r)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("errand: get: %w", err)
	}
	return row, nil
}

const updateTerminalSQL = `
UPDATE errands
SET status            = $2,
    exit_code         = $3,
    stdout            = NULLIF($4, ''),
    stderr            = NULLIF($5, ''),
    stdout_truncated  = $6,
    stderr_truncated  = $7,
    duration_ms       = $8,
    error_message     = NULLIF($9, ''),
    output            = $10,
    finished_at       = NOW()
WHERE errand_id = $1
  AND status = 'running'
`

// TerminalUpdate — payload for transitioning Errand to terminal status.
// stdout/stderr / output are already capped and masked by caller (mask.go).
type TerminalUpdate struct {
	Status          Status
	ExitCode        *int32
	Stdout          string
	Stderr          string
	StdoutTruncated bool
	StderrTruncated bool
	DurationMs      *int64
	ErrorMessage    string
	Output          map[string]any
}

// MarkTerminal transitions running → terminal with CAPACITY-masked output.
// WHERE-guard `status='running'` — single-winner between two terminal
// paths (sync waiter and async-escalation / timed_out): first one wins.
// Returns false if row was already not running (duplicate/late-event).
func (s *Store) MarkTerminal(ctx context.Context, errandID string, upd TerminalUpdate) (bool, error) {
	outputJSON, err := marshalJSONBNullable(upd.Output)
	if err != nil {
		return false, fmt.Errorf("errand: marshal output: %w", err)
	}
	tag, err := s.db.Exec(ctx, updateTerminalSQL,
		errandID,
		string(upd.Status),
		upd.ExitCode,
		upd.Stdout,
		upd.Stderr,
		upd.StdoutTruncated,
		upd.StderrTruncated,
		upd.DurationMs,
		upd.ErrorMessage,
		outputJSON,
	)
	if err != nil {
		return false, fmt.Errorf("errand: update terminal: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// List returns a page of rows matching the filter, sorted by
// started_at DESC (newest on top, parity with push_runs/incarnations). total is
// COUNT(*) under the same filter without LIMIT/OFFSET (for UI pagination).
//
// Queries are simple without prepared statements — filter is small
// (3 optional fields), prepared statement is reused by driver.
func (s *Store) List(ctx context.Context, f ListFilter, offset, limit int) ([]*Row, int, error) {
	whereSQL, args := buildListWhere(f)

	countSQL := "SELECT COUNT(*) FROM errands" + whereSQL
	var total int
	if err := s.db.QueryRow(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("errand: list count: %w", err)
	}

	selectSQL := `
SELECT errand_id, sid, module, input, status,
       exit_code, COALESCE(stdout, ''), COALESCE(stderr, ''),
       stdout_truncated, stderr_truncated,
       duration_ms, COALESCE(error_message, ''),
       output, started_by_aid, started_by_kid,
       started_at, finished_at, ttl_at
FROM errands` + whereSQL + `
ORDER BY started_at DESC
LIMIT $` + itoa(len(args)+1) + ` OFFSET $` + itoa(len(args)+2)
	args = append(args, limit, offset)

	rows, err := s.db.Query(ctx, selectSQL, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("errand: list select: %w", err)
	}
	defer rows.Close()

	out := make([]*Row, 0, limit)
	for rows.Next() {
		row, err := scanRow(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("errand: list scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("errand: list iter: %w", err)
	}
	return out, total, nil
}

// buildListWhere builds WHERE predicate for ListFilter. Parameters are
// written incrementally to args ([]any), placeholders $N are positional.
// Returns string with leading " WHERE …" or "" if filter is empty.
func buildListWhere(f ListFilter) (string, []any) {
	var (
		conds []string
		args  []any
	)
	if f.SID != "" {
		args = append(args, f.SID)
		conds = append(conds, "sid = $"+itoa(len(args)))
	}
	if f.Status != "" {
		args = append(args, string(f.Status))
		conds = append(conds, "status = $"+itoa(len(args)))
	}
	if !f.StartedAfter.IsZero() {
		args = append(args, f.StartedAfter.UTC())
		conds = append(conds, "started_at > $"+itoa(len(args)))
	}
	if len(f.Modules) > 0 {
		// IN ($n,$n+1,…) — exact-match OR; regex/glob not introduced (spec).
		ph := ""
		for i, m := range f.Modules {
			args = append(args, m)
			if i > 0 {
				ph += ","
			}
			ph += "$" + itoa(len(args))
		}
		conds = append(conds, "module IN ("+ph+")")
	}
	if len(conds) == 0 {
		return "", args
	}
	out := " WHERE "
	for i, c := range conds {
		if i > 0 {
			out += " AND "
		}
		out += c
	}
	return out, args
}

const sweepOrphanRunningSQL = `
UPDATE errands
SET status        = 'timed_out',
    error_message = $2,
    finished_at   = NOW()
WHERE started_by_kid = $1
  AND status = 'running'
  AND started_at < NOW() - $3::interval
RETURNING errand_id
`

// SweepOrphanRunning transitions "orphaned" running-Errands of this instance
// to timed_out. Source of orphaned Errands — keeper restart: each running-row
// whose `started_by_kid` equals our KID and `started_at < now - grace` will
// definitely not receive ErrandResult (background goroutine died with process).
// Returns list of transitioned errand_id for subsequent audit-write
// (caller decides whether to write events — Store has no Writer dependency).
//
// WHERE-guard `status='running'` — single-winner; parallel live-handler
// on other node will not break transition.
func (s *Store) SweepOrphanRunning(ctx context.Context, kid string, grace time.Duration, reason string) ([]string, error) {
	rows, err := s.db.Query(ctx, sweepOrphanRunningSQL, kid, reason, grace)
	if err != nil {
		return nil, fmt.Errorf("errand: sweep orphan running: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("errand: sweep scan: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("errand: sweep iter: %w", err)
	}
	return out, nil
}

// scanner — common interface for pgx.Row and pgx.Rows for scanRow function.
type scanner interface {
	Scan(dest ...any) error
}

// scanRow unpacks one SELECT-row into *Row. Used by both Get
// (pgx.Row) and List (pgx.Rows).
func scanRow(r scanner) (*Row, error) {
	var (
		row        Row
		statusStr  string
		exitCode   *int32
		duration   *int64
		finishedAt *time.Time
		inputJSON  []byte
		outputJSON []byte
	)
	if err := r.Scan(
		&row.ErrandID,
		&row.SID,
		&row.Module,
		&inputJSON,
		&statusStr,
		&exitCode,
		&row.Stdout,
		&row.Stderr,
		&row.StdoutTruncated,
		&row.StderrTruncated,
		&duration,
		&row.ErrorMessage,
		&outputJSON,
		&row.StartedByAID,
		&row.StartedByKID,
		&row.StartedAt,
		&finishedAt,
		&row.TTLAt,
	); err != nil {
		return nil, err
	}
	row.Status = Status(statusStr)
	row.ExitCode = exitCode
	row.DurationMs = duration
	row.FinishedAt = finishedAt
	if len(inputJSON) > 0 {
		if err := json.Unmarshal(inputJSON, &row.Input); err != nil {
			return nil, fmt.Errorf("errand: unmarshal input: %w", err)
		}
	}
	if len(outputJSON) > 0 {
		if err := json.Unmarshal(outputJSON, &row.Output); err != nil {
			return nil, fmt.Errorf("errand: unmarshal output: %w", err)
		}
	}
	return &row, nil
}

// marshalJSONB encodes map to jsonb-bytes. nil → "{}" (input always non-null).
func marshalJSONB(m map[string]any) ([]byte, error) {
	if m == nil {
		return []byte(`{}`), nil
	}
	return json.Marshal(m)
}

// marshalJSONBNullable — for output: nil → SQL NULL (returned as nil-bytes),
// otherwise serialized JSON. shell/exec-modules don't return output → NULL
// (not "{}", to distinguish "module did not put structured output" from
// "put empty object").
func marshalJSONBNullable(m map[string]any) ([]byte, error) {
	if m == nil {
		return nil, nil
	}
	return json.Marshal(m)
}

// itoa — small helper for building $N placeholders. strconv.Itoa imports;
// for one small function in WHERE-builder simpler to inline.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [10]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
