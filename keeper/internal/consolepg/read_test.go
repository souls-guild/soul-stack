package consolepg

// Guard tests for the read half (NIM-148). They assert on the SQL the reader
// emits, because the properties that matter here are properties of the query:
// a scope predicate that a filter could widen, or an INNER join that hides a
// decommissioned host's sessions, both look like working code and fail only in
// the case they exist to cover.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// captureQuerier records what the reader asked Postgres for.
type captureQuerier struct {
	countSQL  string
	countArgs []any

	selectSQL  string
	selectArgs []any

	rows *stubRows
}

func (q *captureQuerier) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	q.countSQL, q.countArgs = sql, args
	return stubRow{}
}

func (q *captureQuerier) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	q.selectSQL, q.selectArgs = sql, args
	if q.rows != nil {
		return q.rows, nil
	}
	return &stubRows{}, nil
}

// hostScope is a stand-in for a `host=<sid>` purview rendered by rbac.PurviewSQL.
func hostScope(sid string) func(int) (string, []any, int) {
	return func(startIdx int) (string, []any, int) {
		return "(r.sid = $" + itoa(startIdx) + ")", []any{sid}, startIdx + 1
	}
}

func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}

// TestList_ScopeIsAndedAfterEveryFilter — the scope predicate is the LAST
// conjunct, so no filter can widen it. A caller narrowing by `sid` still only
// sees the hosts their purview allows; an OR anywhere here would hand them the
// whole table.
func TestList_ScopeIsAndedAfterEveryFilter(t *testing.T) {
	q := &captureQuerier{}
	r := NewReader(q)

	if _, _, err := r.List(context.Background(), ListFilter{
		SID:          "db-01.example.com",
		ArchonAID:    "archon-alice",
		Kind:         "interactive",
		StartedAfter: time.Unix(1, 0),
		Scope:        hostScope("web-01.example.com"),
	}, 0, 50); err != nil {
		t.Fatalf("List: %v", err)
	}

	where := q.selectSQL[strings.Index(q.selectSQL, "WHERE"):]
	if strings.Contains(where, " OR ") {
		t.Fatalf("WHERE contains an OR — a filter can widen the scope:\n%s", where)
	}
	scopeAt := strings.Index(where, "r.sid = $5")
	if scopeAt < 0 {
		t.Fatalf("scope predicate is not in the WHERE (or not last):\n%s", where)
	}
	for _, earlier := range []string{"r.archon_aid", "r.kind", "r.started_at >"} {
		if strings.Index(where, earlier) > scopeAt {
			t.Fatalf("filter %q lands after the scope predicate:\n%s", earlier, where)
		}
	}
	// The scope arg is appended in placeholder order, after the four filters.
	if len(q.selectArgs) != 7 { // 4 filters + scope + limit + offset
		t.Fatalf("args = %v, want 4 filters + scope + limit + offset", q.selectArgs)
	}
	if q.selectArgs[4] != "web-01.example.com" {
		t.Fatalf("scope arg = %v, want the purview's host", q.selectArgs[4])
	}
}

// TestList_CountAndSelectShareTheScope — the total must describe the same rows
// the page does. A count that skipped the purview would advertise recordings the
// caller cannot open, which is an information leak with a page number on it.
func TestList_CountAndSelectShareTheScope(t *testing.T) {
	q := &captureQuerier{}
	r := NewReader(q)

	if _, _, err := r.List(context.Background(), ListFilter{Scope: hostScope("db-01.example.com")}, 0, 50); err != nil {
		t.Fatalf("List: %v", err)
	}
	if !strings.Contains(q.countSQL, "r.sid = $1") {
		t.Fatalf("count query has no scope predicate:\n%s", q.countSQL)
	}
	if len(q.countArgs) != 1 || q.countArgs[0] != "db-01.example.com" {
		t.Fatalf("count args = %v, want the scope arg", q.countArgs)
	}
}

// TestList_JoinsTheHostLeft — a recording of a host since removed from the
// registry must stay visible to an unrestricted operator. An INNER join would
// make deleting the host delete the evidence, which is the one thing this table
// exists to prevent.
func TestList_JoinsTheHostLeft(t *testing.T) {
	q := &captureQuerier{}
	r := NewReader(q)

	if _, _, err := r.List(context.Background(), ListFilter{}, 0, 50); err != nil {
		t.Fatalf("List: %v", err)
	}
	if !strings.Contains(q.selectSQL, "LEFT JOIN souls") {
		t.Fatalf("recordings are not LEFT JOINed to souls:\n%s", q.selectSQL)
	}
	if strings.Contains(q.selectSQL, "INNER JOIN") {
		t.Fatalf("an INNER join hides recordings of decommissioned hosts:\n%s", q.selectSQL)
	}
}

// TestList_OrdersNewestFirst — the operator question is "what happened
// recently", and a stable tiebreak keeps offset pagination from repeating or
// skipping a row when two sessions share a timestamp.
func TestList_OrdersNewestFirst(t *testing.T) {
	q := &captureQuerier{}
	r := NewReader(q)

	if _, _, err := r.List(context.Background(), ListFilter{}, 0, 50); err != nil {
		t.Fatalf("List: %v", err)
	}
	if !strings.Contains(q.selectSQL, "ORDER BY r.started_at DESC, r.recording_id DESC") {
		t.Fatalf("unexpected ordering:\n%s", q.selectSQL)
	}
}

// TestWriteCast_HeaderThenPartsInSeqOrder — the file is the header line followed
// by the parts concatenated in `seq` order, and nothing else. A part never
// splits a cast line (migration 104), so this IS the asciicast — no re-joining
// and no re-encoding, which is also what keeps the recorder's masking intact.
func TestWriteCast_HeaderThenPartsInSeqOrder(t *testing.T) {
	q := &captureQuerier{rows: &stubRows{bodies: []string{"[0.1,\"o\",\"a\"]\n", "[0.2,\"o\",\"b\"]\n"}}}
	r := NewReader(q)

	var out strings.Builder
	rec := Recording{RecordingID: "rec-1"}
	rec.Header.Version = 2
	rec.Header.Width = 80
	rec.Header.Height = 24
	if err := r.WriteCast(context.Background(), rec, func(b []byte) error {
		out.Write(b)
		return nil
	}); err != nil {
		t.Fatalf("WriteCast: %v", err)
	}

	if !strings.Contains(q.selectSQL, "ORDER BY seq") {
		t.Fatalf("parts are not read in seq order:\n%s", q.selectSQL)
	}
	want := "{\"version\":2,\"width\":80,\"height\":24,\"timestamp\":0}\n[0.1,\"o\",\"a\"]\n[0.2,\"o\",\"b\"]\n"
	if out.String() != want {
		t.Fatalf("cast =\n%q\nwant\n%q", out.String(), want)
	}
}

// TestWriteCast_StopsOnAWriteError — a client that hung up must not make the
// reader keep pulling a 256 MiB body out of Postgres.
func TestWriteCast_StopsOnAWriteError(t *testing.T) {
	q := &captureQuerier{rows: &stubRows{bodies: []string{"a\n", "b\n", "c\n"}}}
	r := NewReader(q)

	writes := 0
	err := r.WriteCast(context.Background(), Recording{RecordingID: "rec-1"}, func([]byte) error {
		writes++
		if writes == 2 {
			return context.Canceled
		}
		return nil
	})
	if err == nil {
		t.Fatal("want the write error to surface")
	}
	if writes != 2 {
		t.Fatalf("writes = %d, want the stream to stop at the failing one", writes)
	}
}

// --- pgx stubs ---------------------------------------------------------

type stubRow struct{}

func (stubRow) Scan(dest ...any) error {
	if len(dest) == 1 {
		if p, ok := dest[0].(*int); ok {
			*p = 0
		}
	}
	return nil
}

// stubRows yields one single-column row per body (the parts query).
type stubRows struct {
	bodies []string
	i      int
}

func (r *stubRows) Next() bool {
	if r.i >= len(r.bodies) {
		return false
	}
	r.i++
	return true
}

func (r *stubRows) Scan(dest ...any) error {
	if len(dest) == 1 {
		if p, ok := dest[0].(*string); ok {
			*p = r.bodies[r.i-1]
		}
	}
	return nil
}

func (r *stubRows) Close()                                       {}
func (r *stubRows) Err() error                                   { return nil }
func (r *stubRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *stubRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *stubRows) Values() ([]any, error)                       { return nil, nil }
func (r *stubRows) RawValues() [][]byte                          { return nil }
func (r *stubRows) Conn() *pgx.Conn                              { return nil }
