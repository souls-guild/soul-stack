package auditpg

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// queryRower — narrow subset of pgxpool.Pool needed by Reader. The narrowing
// lets List be unit-tested via a fake without spinning up Postgres;
// pgxpool.Pool/pgx.Conn/pgx.Tx satisfy it automatically.
type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Reader — read-side wrapper over `audit_log`. Concurrent-safe; holds no
// state of its own. Symmetric with errand.Store / operator.SelectByAID: the
// pool is owned by the caller, Reader is a narrow client.
type Reader struct {
	pool queryRower
}

// NewReader constructs a Reader; pool is required (caller panic — NewServer
// already validates non-nil).
func NewReader(pool queryRower) *Reader {
	return &Reader{pool: pool}
}

// ListFilter — parameters for `GET /v1/audit`. Empty fields = "no filter";
// multi-value Types/Sources are filtered via `IN (…)`. StartedAfter /
// StartedBefore — UTC, zero-time = no filter.
//
// Multi-value by convention `?type=X&type=Y` is parsed by the handler from a
// `url.Values.Get`-equivalent multi-getter (see handler).
type ListFilter struct {
	Types   []string
	Sources []string
	// ArchonAID / CorrelationID — case-insensitive substring (ILIKE `%val%`):
	// the operator searches by a part of the AID/correlation_id in any case.
	// LIKE metacharacters in the input are escaped (search stays literal).
	// Empty = no filter.
	ArchonAID     string
	CorrelationID string
	// PayloadHerald — exact match on the string field `herald` in the JSONB
	// payload (`payload->>'herald'`). For the delivery history of one Herald
	// channel (herald.delivered/herald.failed events carry `payload.herald`).
	// Empty = no filter.
	PayloadHerald string
	// PayloadVoyage — exact match on the string field `voyage_id` in the JSONB
	// payload (`payload->>'voyage_id'`). For Voyage detail: per-incarnation
	// incarnation.run_completed events carry correlation_id=apply_id
	// (per-incarnation, not voyage_id), so filtering by correlation_id doesn't
	// collect a voyage's run events — a payload filter on voyage_id is needed
	// (ADR-052 amend §k). Empty = no filter.
	PayloadVoyage string
	StartedAfter  time.Time
	StartedBefore time.Time
}

// Row — read-projection of an `audit_log` row. Matches the table columns
// (migration 001_create_audit_log.up.sql). Payload — unpacked JSONB.
// keeper_kid is absent from the schema (ADR-022 / the 001_ migration doesn't
// carry the column); if the field is ever needed — a separate migration +
// Row extension.
type Row struct {
	AuditID       string
	CreatedAt     time.Time
	EventType     string
	Source        audit.Source
	ArchonAID     *string
	CorrelationID *string
	Payload       map[string]any
}

// List returns a page of rows under the filter, sorted by created_at DESC
// (newest first, parity with push_runs/incarnations/errands). total —
// COUNT(*) under the same filter without LIMIT/OFFSET (for UI pagination).
//
// Read-only; the read itself is NOT written to audit_log (avoids recursion:
// every GET /v1/audit would otherwise double the table).
func (r *Reader) List(ctx context.Context, f ListFilter, offset, limit int) ([]*Row, int, error) {
	whereSQL, args := buildAuditWhere(f)

	var total int
	if err := r.pool.QueryRow(ctx, "SELECT COUNT(*) FROM audit_log"+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("audit: list count: %w", err)
	}

	selectSQL := `SELECT audit_id, created_at, event_type, source, archon_aid, correlation_id, payload
FROM audit_log` + whereSQL + `
ORDER BY created_at DESC
LIMIT $` + strconv.Itoa(len(args)+1) + ` OFFSET $` + strconv.Itoa(len(args)+2)
	args = append(args, limit, offset)

	rows, err := r.pool.Query(ctx, selectSQL, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("audit: list select: %w", err)
	}
	defer rows.Close()

	out := make([]*Row, 0, limit)
	for rows.Next() {
		row, err := scanAuditRow(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("audit: list scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("audit: list iter: %w", err)
	}
	return out, total, nil
}

// buildAuditWhere assembles the WHERE predicate from ListFilter. Multi-value
// fields expand into `IN ($a,$b,…)`. Parameters are appended to args
// incrementally; placeholders $N are positional.
func buildAuditWhere(f ListFilter) (string, []any) {
	var (
		conds []string
		args  []any
	)
	if len(f.Types) > 0 {
		conds = append(conds, "event_type IN ("+placeholders(&args, anyOfStrings(f.Types))+")")
	}
	if len(f.Sources) > 0 {
		conds = append(conds, "source IN ("+placeholders(&args, anyOfStrings(f.Sources))+")")
	}
	if f.ArchonAID != "" {
		args = append(args, likeContains(f.ArchonAID))
		conds = append(conds, "archon_aid ILIKE $"+strconv.Itoa(len(args)))
	}
	if f.CorrelationID != "" {
		args = append(args, likeContains(f.CorrelationID))
		conds = append(conds, "correlation_id ILIKE $"+strconv.Itoa(len(args)))
	}
	if f.PayloadHerald != "" {
		args = append(args, f.PayloadHerald)
		conds = append(conds, "payload->>'herald' = $"+strconv.Itoa(len(args)))
	}
	if f.PayloadVoyage != "" {
		args = append(args, f.PayloadVoyage)
		conds = append(conds, "payload->>'voyage_id' = $"+strconv.Itoa(len(args)))
	}
	if !f.StartedAfter.IsZero() {
		args = append(args, f.StartedAfter.UTC())
		conds = append(conds, "created_at >= $"+strconv.Itoa(len(args)))
	}
	if !f.StartedBefore.IsZero() {
		args = append(args, f.StartedBefore.UTC())
		conds = append(conds, "created_at <= $"+strconv.Itoa(len(args)))
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

// likeContains wraps a user string in `%…%` for an ILIKE "substring
// contains" search. LIKE metacharacters (`\`/`%`/`_`) in the input are
// escaped so the search stays literal: an operator typing `%` or `_` searches
// for that exact character, not a wildcard (the value goes through a bind
// parameter — SQL injection is already excluded at the pgx level; this is
// only about search semantics). Escape char — the default `\`, no separate
// ESCAPE clause needed.
func likeContains(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(s) + "%"
}

// placeholders appends values to args and returns the placeholder string
// "$n,$n+1,…". Helper for `IN (…)` predicates with multi-value filters.
func placeholders(args *[]any, values []any) string {
	out := ""
	for i, v := range values {
		*args = append(*args, v)
		if i > 0 {
			out += ","
		}
		out += "$" + strconv.Itoa(len(*args))
	}
	return out
}

// anyOfStrings — []string → []any conversion for placeholders. Adaptation
// layer between the closed-typed filter fields (caller's typed []string) and
// pgx args (variadic any).
func anyOfStrings(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

// scanAuditRow unpacks one SELECT row into *Row. Symmetric with
// errand.scanRow / operator.scanOperator.
func scanAuditRow(r pgx.Rows) (*Row, error) {
	var (
		row        Row
		sourceStr  string
		payloadRaw []byte
	)
	if err := r.Scan(
		&row.AuditID,
		&row.CreatedAt,
		&row.EventType,
		&sourceStr,
		&row.ArchonAID,
		&row.CorrelationID,
		&payloadRaw,
	); err != nil {
		return nil, err
	}
	row.Source = audit.Source(sourceStr)
	if len(payloadRaw) > 0 {
		if err := json.Unmarshal(payloadRaw, &row.Payload); err != nil {
			return nil, fmt.Errorf("audit: unmarshal payload: %w", err)
		}
	}
	return &row, nil
}
