package pushorch

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeStoreDB — узкий ExecQueryRower-stub для unit-тестов Store.SelectAll.
// Хранит SQL и args последнего вызова, отвечает преднастроенными значениями.
// Симметричен fakeDB из tide/crud_test.go, в облегчённой форме (без full Insert-
// path-а).
type fakeStoreDB struct {
	queryRowSQL  string
	queryRowArgs []any
	countV       int
	countErr     error

	queryCalls int
	querySQL   string
	queryArgs  []any
	queryRows  pgx.Rows
	queryErr   error
}

func (f *fakeStoreDB) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("fakeStoreDB.Exec: not configured")
}

func (f *fakeStoreDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.queryRowSQL = sql
	f.queryRowArgs = args
	if f.countErr != nil {
		return errPgRow{err: f.countErr}
	}
	return intPgRow{v: f.countV}
}

func (f *fakeStoreDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	f.queryCalls++
	f.querySQL = sql
	f.queryArgs = args
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	if f.queryRows != nil {
		return f.queryRows, nil
	}
	return &emptyPgRows{}, nil
}

type errPgRow struct{ err error }

func (r errPgRow) Scan(_ ...any) error { return r.err }

type intPgRow struct{ v int }

func (r intPgRow) Scan(dest ...any) error {
	if len(dest) != 1 {
		return errors.New("intPgRow: expected 1 dest")
	}
	ip, ok := dest[0].(*int)
	if !ok {
		return errors.New("intPgRow: dest is not *int")
	}
	*ip = r.v
	return nil
}

type emptyPgRows struct{}

func (r *emptyPgRows) Next() bool                                   { return false }
func (r *emptyPgRows) Scan(_ ...any) error                          { return nil }
func (r *emptyPgRows) Err() error                                   { return nil }
func (r *emptyPgRows) Close()                                       {}
func (r *emptyPgRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *emptyPgRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *emptyPgRows) Values() ([]any, error)                       { return nil, nil }
func (r *emptyPgRows) RawValues() [][]byte                          { return nil }
func (r *emptyPgRows) Conn() *pgx.Conn                              { return nil }

func TestValidStatus(t *testing.T) {
	t.Parallel()
	valid := []PushRunStatus{StatusPending, StatusRunning, StatusSuccess, StatusPartialFailed, StatusFailed, StatusCancelled}
	for _, s := range valid {
		if !ValidStatus(s) {
			t.Errorf("ValidStatus(%q) = false, want true", s)
		}
	}
	for _, s := range []PushRunStatus{"", "weird", "Success"} {
		if ValidStatus(s) {
			t.Errorf("ValidStatus(%q) = true, want false", s)
		}
	}
}

func TestStore_SelectAll_EmptyResult(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := &fakeStoreDB{countV: 0}
	s := NewStore(db)
	out, total, err := s.SelectAll(ctx, ListFilter{}, 0, 50)
	if err != nil {
		t.Fatalf("SelectAll: %v", err)
	}
	if total != 0 {
		t.Errorf("total = %d, want 0", total)
	}
	if len(out) != 0 {
		t.Errorf("len(out) = %d, want 0", len(out))
	}
	if !strings.Contains(db.queryRowSQL, "SELECT COUNT(*)") {
		t.Errorf("expected COUNT SQL, got: %.200s", db.queryRowSQL)
	}
	if db.queryCalls != 1 {
		t.Errorf("Query calls = %d, want 1 (SELECT)", db.queryCalls)
	}
}

func TestStore_SelectAll_NoFilters_NilArgs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := &fakeStoreDB{countV: 0}
	s := NewStore(db)
	if _, _, err := s.SelectAll(ctx, ListFilter{}, 0, 50); err != nil {
		t.Fatalf("SelectAll: %v", err)
	}
	// COUNT-args: [statusesArg, providerArg]
	if got := db.queryRowArgs[0]; got != nil {
		t.Errorf("COUNT statuses arg = %v, want nil (no filter)", got)
	}
	if got := db.queryRowArgs[1]; got != nil {
		t.Errorf("COUNT provider arg = %v, want nil (no filter)", got)
	}
}

func TestStore_SelectAll_StatusesFilter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := &fakeStoreDB{countV: 5}
	s := NewStore(db)
	filter := ListFilter{Statuses: []PushRunStatus{StatusRunning, StatusPartialFailed}}
	if _, _, err := s.SelectAll(ctx, filter, 0, 50); err != nil {
		t.Fatalf("SelectAll: %v", err)
	}
	got, ok := db.queryRowArgs[0].([]string)
	if !ok {
		t.Fatalf("COUNT statuses arg type = %T, want []string", db.queryRowArgs[0])
	}
	if len(got) != 2 || got[0] != "running" || got[1] != "partial_failed" {
		t.Errorf("COUNT statuses arg = %v, want [running partial_failed]", got)
	}
}

func TestStore_SelectAll_ProviderFilter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := &fakeStoreDB{countV: 0}
	s := NewStore(db)
	filter := ListFilter{SSHProvider: "openssh"}
	if _, _, err := s.SelectAll(ctx, filter, 100, 25); err != nil {
		t.Fatalf("SelectAll: %v", err)
	}
	if got := db.queryRowArgs[1]; got != "openssh" {
		t.Errorf("COUNT provider arg = %v, want openssh", got)
	}
	// SELECT-args: [statusesArg, providerArg, limit, offset]
	if got := db.queryArgs[2]; got != 25 {
		t.Errorf("SELECT limit arg = %v, want 25", got)
	}
	if got := db.queryArgs[3]; got != 100 {
		t.Errorf("SELECT offset arg = %v, want 100", got)
	}
}

func TestStore_SelectAll_CountError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := &fakeStoreDB{countErr: errors.New("PG down")}
	s := NewStore(db)
	_, _, err := s.SelectAll(ctx, ListFilter{}, 0, 50)
	if err == nil || !strings.Contains(err.Error(), "count all") {
		t.Errorf("err = %v, want count all", err)
	}
}

func TestStore_SelectAll_QueryError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := &fakeStoreDB{countV: 1, queryErr: errors.New("PG broken")}
	s := NewStore(db)
	_, _, err := s.SelectAll(ctx, ListFilter{}, 0, 50)
	if err == nil || !strings.Contains(err.Error(), "select all") {
		t.Errorf("err = %v, want select all", err)
	}
}
