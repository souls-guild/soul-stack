package settingsstore

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// --- pgx fakes ---------------------------------------------------------

type fakeRows struct {
	rows [][2]string
	i    int
	err  error
}

func (r *fakeRows) Next() bool {
	if r.i >= len(r.rows) {
		return false
	}
	r.i++
	return true
}

func (r *fakeRows) Scan(dest ...any) error {
	if len(dest) != 2 {
		return errors.New("fakeRows: expected 2 dest")
	}
	k, ok1 := dest[0].(*string)
	v, ok2 := dest[1].(*string)
	if !ok1 || !ok2 {
		return errors.New("fakeRows: dest is not *string")
	}
	*k, *v = r.rows[r.i-1][0], r.rows[r.i-1][1]
	return nil
}

func (r *fakeRows) Err() error                                   { return r.err }
func (r *fakeRows) Close()                                       {}
func (r *fakeRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeRows) Values() ([]any, error)                       { return nil, nil }
func (r *fakeRows) RawValues() [][]byte                          { return nil }
func (r *fakeRows) Conn() *pgx.Conn                              { return nil }

type fakeDB struct {
	rows  [][2]string
	err   error
	calls int
	sql   string
}

func (d *fakeDB) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	d.calls++
	d.sql = sql
	if d.err != nil {
		return nil, d.err
	}
	return &fakeRows{rows: d.rows}, nil
}

// recordingApplier counts re-merge requests from the store.
type recordingApplier struct {
	calls int
	res   config.ReloadResult
}

func (a *recordingApplier) RefreshOverlay(context.Context) config.ReloadResult {
	a.calls++
	return a.res
}

func testStore(db Querier, applier OverlayApplier) *Store {
	return New(db, applier, 0, slog.New(slog.NewJSONHandler(io.Discard, nil)))
}

// --- tests -------------------------------------------------------------

func TestStore_RefreshPublishesEntries(t *testing.T) {
	db := &fakeDB{rows: [][2]string{
		{"cfg_tempo_voyage_create_rate", "1"},
		{"cfg_toll_threshold", "0.5"},
	}}
	applier := &recordingApplier{res: config.ReloadResult{Swapped: true}}
	s := testStore(db, applier)

	if err := s.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	entries := s.Overlay()
	if len(entries) != 2 {
		t.Fatalf("entries = %+v, want 2", entries)
	}
	// Sorted by yaml path, so the merge order is deterministic.
	if entries[0].Path != "$.tempo.voyage_create.rate" || entries[0].Value != 1.0 {
		t.Errorf("entry[0] = %+v", entries[0])
	}
	if entries[1].Path != "$.toll.threshold" || entries[1].Value != 0.5 {
		t.Errorf("entry[1] = %+v", entries[1])
	}
	if applier.calls != 1 {
		t.Errorf("applier called %d times, want 1", applier.calls)
	}
	if v := s.Values()["cfg_toll_threshold"]; v != 0.5 {
		t.Errorf("Values()[cfg_toll_threshold] = %v", v)
	}
}

// The overlay namespace is disjoint from the ADR-029 well-known keys, and the
// query says so.
func TestStore_ReadsOnlyPrefixedKeys(t *testing.T) {
	db := &fakeDB{}
	s := testStore(db, nil)
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if want := `key LIKE 'cfg\_%'`; !strings.Contains(db.sql, want) {
		t.Errorf("query %q does not restrict to %s", db.sql, want)
	}
}

// ADR-0073(h): a broken row rejects the WHOLE snapshot and the last good one
// stays in effect — never a partial merge, never a reset to the file.
func TestStore_BrokenRowKeepsLastGood(t *testing.T) {
	db := &fakeDB{rows: [][2]string{{"cfg_toll_threshold", "0.5"}}}
	s := testStore(db, nil)
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}

	// Out of range: a type check would happily accept 5.
	db.rows = [][2]string{
		{"cfg_tempo_voyage_create_rate", "1"},
		{"cfg_toll_threshold", "5"},
	}
	if err := s.Refresh(context.Background()); err == nil {
		t.Fatal("expected an error for an out-of-range row")
	}
	entries := s.Overlay()
	if len(entries) != 1 || entries[0].Value != 0.5 {
		t.Errorf("last-good lost: %+v", entries)
	}
}

// Postgres unreachable — at startup or at runtime — is a warning, not a reset:
// the daemon comes up on the file base, and a live instance keeps its overlay.
func TestStore_DBErrorKeepsLastGood(t *testing.T) {
	db := &fakeDB{rows: [][2]string{{"cfg_toll_threshold", "0.5"}}}
	s := testStore(db, nil)
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}

	db.err = errors.New("connection refused")
	if err := s.Refresh(context.Background()); err == nil {
		t.Fatal("expected the DB error to be reported")
	}
	if entries := s.Overlay(); len(entries) != 1 || entries[0].Value != 0.5 {
		t.Errorf("overlay reset on a DB error: %+v", entries)
	}
}

func TestStore_StartupWithoutPostgresIsEmptyNotFatal(t *testing.T) {
	s := testStore(&fakeDB{err: errors.New("connection refused")}, nil)
	if err := s.Refresh(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	if entries := s.Overlay(); len(entries) != 0 {
		t.Errorf("overlay is not empty after a failed first load: %+v", entries)
	}
}

// A key this binary does not know (rolling upgrade, or a leftover from a
// downgrade) must not take the whole snapshot down with it.
func TestStore_UnknownKeyIsSkipped(t *testing.T) {
	db := &fakeDB{rows: [][2]string{
		{"cfg_from_the_future", "42"},
		{"cfg_toll_threshold", "0.5"},
	}}
	s := testStore(db, nil)
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	entries := s.Overlay()
	if len(entries) != 1 || entries[0].Path != "$.toll.threshold" {
		t.Errorf("entries = %+v, want only the known key", entries)
	}
}

// A merged config the validator rejects is reported to the caller (it logs a
// WARN), while config.Store keeps last-good on its own side.
func TestStore_RejectedMergeIsReported(t *testing.T) {
	db := &fakeDB{rows: [][2]string{{"cfg_toll_threshold", "0.5"}}}
	applier := &recordingApplier{res: config.ReloadResult{
		Swapped: false,
		Diagnostics: []diag.Diagnostic{{
			Level:   diag.LevelError,
			Phase:   diag.PhaseSchemaValidate,
			Code:    "range_error",
			Message: "merged config broken",
		}},
	}}
	s := testStore(db, applier)
	if err := s.Refresh(context.Background()); err == nil {
		t.Fatal("expected the rejected merge to be reported")
	}
}

func TestStore_NoDBIsNoOp(t *testing.T) {
	s := testStore(nil, nil)
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh with no DB: %v", err)
	}
	if entries := s.Overlay(); len(entries) != 0 {
		t.Errorf("entries = %+v, want none", entries)
	}
}

// Store must satisfy the shared/config hook.
var _ config.OverlaySource = (*Store)(nil)
