package reaper

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// fakeErrandsExecer is a fake errandsExecer for unit tests. It captures the
// last SQL plus arguments and returns a preprogrammed CommandTag or error.
type fakeErrandsExecer struct {
	calls   int
	lastSQL string
	args    []any
	rowsAff int64
	err     error
}

func (f *fakeErrandsExecer) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.calls++
	f.lastSQL = sql
	f.args = args
	if f.err != nil {
		return pgconn.CommandTag{}, f.err
	}
	// pgconn.NewCommandTag takes raw bytes like "DELETE <N>".
	tag := pgconn.NewCommandTag("DELETE " + itoaForTag(f.rowsAff))
	return tag, nil
}

// itoaForTag is strconv.Itoa without importing it inside the test.
func itoaForTag(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
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

func TestErrandsPurger_Run_DeletesRowsAndReturnsCount(t *testing.T) {
	fe := &fakeErrandsExecer{rowsAff: 17}
	p := newErrandsPurgerFromExecer(fe, silentLogger())

	got, err := p.Run(context.Background(), 7*24*time.Hour, 1000)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got != 17 {
		t.Errorf("affected = %d, want 17", got)
	}
	if fe.calls != 1 {
		t.Fatalf("calls = %d, want 1", fe.calls)
	}
	if fe.lastSQL != purgeOldErrandsSQL {
		t.Errorf("SQL mismatch:\n got: %q\nwant: %q", fe.lastSQL, purgeOldErrandsSQL)
	}
	if len(fe.args) != 0 {
		t.Errorf("args len = %d, want 0 (max_age is not passed to the predicate)", len(fe.args))
	}
}

func TestErrandsPurger_Run_NoRows(t *testing.T) {
	fe := &fakeErrandsExecer{rowsAff: 0}
	p := newErrandsPurgerFromExecer(fe, silentLogger())

	got, err := p.Run(context.Background(), 7*24*time.Hour, 1000)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got != 0 {
		t.Errorf("affected = %d, want 0", got)
	}
}

func TestErrandsPurger_Run_PropagatesError(t *testing.T) {
	want := errors.New("pg down")
	fe := &fakeErrandsExecer{err: want}
	p := newErrandsPurgerFromExecer(fe, silentLogger())

	_, err := p.Run(context.Background(), 7*24*time.Hour, 1000)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
}
