package consolepg

// The read half of the recording store — playback (ADR-0074 amendment, NIM-148).
//
// NIM-145 left the read path stated but unbuilt: "a recording is read with
// `SELECT body FROM console_recording_parts WHERE recording_id = $1 ORDER BY
// seq` behind `console_recordings.cast_header`". That is exactly what this does,
// and deliberately nothing more — the cast is served byte-for-byte as it was
// written. Masking happened once, at record time, across chunk boundaries; a
// reader that re-processed the body could only weaken that, and there is no
// plaintext left to recover anyway.
//
// A separate type from [Store] rather than more methods on it: the writer is
// constructed with a retention and is handed to the Hub, which must not be able
// to read back; the reader is handed to the API, which must not be able to
// write. The split is what keeps a playback route from ever being a way into the
// recording.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/console"
)

// querier is the narrow slice of pgxpool.Pool the reader needs, so the guard
// tests can drive it without Postgres (the [execer] pattern of the write half).
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Reader serves recorded sessions back. Ownership of the pool stays with the
// caller.
type Reader struct {
	db querier
}

// NewReader wraps an initialized pool.
func NewReader(db querier) *Reader { return &Reader{db: db} }

// ErrNotFound — no recording with that id.
var ErrNotFound = fmt.Errorf("consolepg: recording not found")

// Recording is the metadata of one recorded session — the `console_recordings`
// row plus the host's scope dimensions, which the caller needs to decide
// whether this recording may be shown at all.
//
// The cast body is NOT here. It is streamed by [Reader.WriteCast]: a recording
// runs to the per-session cap of 256 MiB, so materializing one in memory to
// answer a metadata query would be a way to run a Keeper instance out of it.
type Recording struct {
	RecordingID string
	SessionID   string
	Kind        string
	SID         string
	ArchonAID   string
	Header      console.CastHeader
	StartedAt   time.Time
	FinishedAt  *time.Time
	CloseReason string
	EventCount  int64
	ByteCount   int64
	Truncated   bool

	// Covens / Traits come from the LEFT JOIN on `souls` and are the host's
	// CURRENT ones, not the ones it had during the session. They exist for the
	// scope check and are not part of the wire shape.
	//
	// Both are empty when the host is gone from the registry. That is
	// fail-closed by construction: a coven-scoped operator can no longer show
	// the recording is inside their coven, while a `host=`-scoped one still
	// matches on `sid`, which the recording carries itself.
	Covens []string
	Traits map[string]any
}

// Live reports whether the recording has no terminal stamp — either the session
// is still running, or the Keeper instance holding it died before it could say
// goodbye (NIM-145). Both are replayable; neither is complete.
func (r Recording) Live() bool { return r.FinishedAt == nil }

// ListFilter narrows a recording listing. Zero fields are no filter.
type ListFilter struct {
	SID           string
	ArchonAID     string
	Kind          string
	StartedAfter  time.Time
	StartedBefore time.Time

	// Scope renders the caller's purview into a SQL boolean over the joined
	// columns, starting at placeholder $startIdx. A nil Scope applies NO
	// narrowing and must therefore only ever be passed by a caller that has
	// already established the operator is unrestricted — the API layer resolves
	// it from `soul.console` and fails closed on doubt.
	Scope func(startIdx int) (string, []any, int)
}

// The column aliases a [ListFilter.Scope] predicate must be written against.
// Exported so the RBAC-side mapping is built from these rather than from a
// second copy of the same strings — a scope predicate naming a column this
// query does not have is a runtime SQL error on a security path.
//
// [ScopeHostColumn] is the RECORDING's own `sid`, not the joined one: a
// recording of a host since removed from the registry must stay reachable by an
// operator scoped to that host — an artifact outliving its subject is the normal
// case for an audit trail, not an edge one. Coven and traits necessarily come
// from the join and go NULL with the host (see [Recording.Covens]).
//
// There is no service or incarnation column: a recording carries neither, so a
// condition on those dimensions must render FALSE (fail-closed, ADR-047).
const (
	ScopeHostColumn   = "r.sid"
	ScopeCovenColumn  = "s.coven"
	ScopeTraitsColumn = "s.traits"
)

const recordingColumns = `
    r.recording_id, r.session_id, r.kind, r.sid, r.archon_aid, r.cast_header,
    r.started_at, r.finished_at, COALESCE(r.close_reason, ''),
    r.event_count, r.byte_count, r.truncated,
    COALESCE(s.coven, '{}'), s.traits`

// fromClause joins the host so coven/trait-scoped operators can be answered.
// LEFT, not INNER: an INNER join would make a decommissioned host's recordings
// invisible to EVERYONE, including an unrestricted operator — deleting the host
// would quietly delete the evidence, which is the one thing this table exists to
// prevent.
const fromClause = `
FROM console_recordings r
LEFT JOIN souls s ON s.sid = r.sid`

// List returns one page of recordings, newest first, and the total matching the
// filter (scope included, so the count never advertises rows the caller cannot
// open).
func (r *Reader) List(ctx context.Context, f ListFilter, offset, limit int) ([]Recording, int, error) {
	where, args := f.build()

	var total int
	if err := r.db.QueryRow(ctx, "SELECT COUNT(*)"+fromClause+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("consolepg: list count: %w", err)
	}

	selectSQL := "SELECT" + recordingColumns + fromClause + where +
		" ORDER BY r.started_at DESC, r.recording_id DESC" +
		" LIMIT $" + strconv.Itoa(len(args)+1) + " OFFSET $" + strconv.Itoa(len(args)+2)
	rows, err := r.db.Query(ctx, selectSQL, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("consolepg: list select: %w", err)
	}
	defer rows.Close()

	out := make([]Recording, 0, limit)
	for rows.Next() {
		rec, err := scanRecording(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("consolepg: list scan: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("consolepg: list iter: %w", err)
	}
	return out, total, nil
}

// Get returns one recording's metadata. The scope check is the CALLER's — Get
// answers "does this exist", and the API layer turns "exists but out of scope"
// into the same 404 an absent id gets, so the route cannot be used to probe
// which hosts have been consoled into.
func (r *Reader) Get(ctx context.Context, recordingID string) (Recording, error) {
	rows, err := r.db.Query(ctx, "SELECT"+recordingColumns+fromClause+" WHERE r.recording_id = $1", recordingID)
	if err != nil {
		return Recording{}, fmt.Errorf("consolepg: get recording: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Recording{}, fmt.Errorf("consolepg: get recording: %w", err)
		}
		return Recording{}, ErrNotFound
	}
	rec, err := scanRecording(rows)
	if err != nil {
		return Recording{}, fmt.Errorf("consolepg: get scan: %w", err)
	}
	return rec, nil
}

// WriteCast streams the asciicast v2 file of a recording to write: the header
// line from `cast_header`, then every body part in `seq` order.
//
// Streamed rather than returned, because the artifact is capped at 256 MiB and
// an HTTP handler that buffered one would hold that per concurrent viewer. A
// part never splits a cast line (migration 104), so concatenation IS the file —
// no re-joining, no re-encoding, and no opportunity to alter what was recorded.
func (r *Reader) WriteCast(ctx context.Context, rec Recording, write func([]byte) error) error {
	header, err := json.Marshal(rec.Header)
	if err != nil {
		return fmt.Errorf("consolepg: marshal cast header: %w", err)
	}
	if err := write(append(header, '\n')); err != nil {
		return err
	}

	rows, err := r.db.Query(ctx,
		"SELECT body FROM console_recording_parts WHERE recording_id = $1 ORDER BY seq",
		rec.RecordingID)
	if err != nil {
		return fmt.Errorf("consolepg: select parts: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return fmt.Errorf("consolepg: scan part: %w", err)
		}
		if err := write([]byte(body)); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("consolepg: iterate parts: %w", err)
	}
	return nil
}

// build renders the WHERE clause. The scope predicate is ANDed LAST so it can
// never be widened by a filter: a caller narrowing by `sid` still only sees the
// hosts their purview allows.
func (f ListFilter) build() (string, []any) {
	var (
		conds []string
		args  []any
	)
	add := func(expr string, v any) {
		args = append(args, v)
		conds = append(conds, expr+"$"+strconv.Itoa(len(args)))
	}
	if f.SID != "" {
		add("r.sid = ", f.SID)
	}
	if f.ArchonAID != "" {
		add("r.archon_aid = ", f.ArchonAID)
	}
	if f.Kind != "" {
		add("r.kind = ", f.Kind)
	}
	if !f.StartedAfter.IsZero() {
		add("r.started_at > ", f.StartedAfter.UTC())
	}
	if !f.StartedBefore.IsZero() {
		add("r.started_at < ", f.StartedBefore.UTC())
	}
	if f.Scope != nil {
		sql, scopeArgs, _ := f.Scope(len(args) + 1)
		conds = append(conds, sql)
		args = append(args, scopeArgs...)
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// scanRecording reads one joined row.
func scanRecording(rows pgx.Rows) (Recording, error) {
	var (
		rec    Recording
		header []byte
		traits map[string]any
	)
	if err := rows.Scan(
		&rec.RecordingID, &rec.SessionID, &rec.Kind, &rec.SID, &rec.ArchonAID, &header,
		&rec.StartedAt, &rec.FinishedAt, &rec.CloseReason,
		&rec.EventCount, &rec.ByteCount, &rec.Truncated,
		&rec.Covens, &traits,
	); err != nil {
		return Recording{}, err
	}
	// A header that will not parse is not a reason to hide the recording: the
	// body is the artifact, and a player can size a terminal from a default.
	_ = json.Unmarshal(header, &rec.Header)
	rec.Traits = traits
	return rec, nil
}
