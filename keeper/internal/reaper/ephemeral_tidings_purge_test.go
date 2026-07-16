package reaper

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// fakeEphemeralTidingsExecer is a fake ephemeralTidingsExecer for unit tests.
type fakeEphemeralTidingsExecer struct {
	calls   int
	lastSQL string
	args    []any
	rowsAff int64
	err     error
}

func (f *fakeEphemeralTidingsExecer) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.calls++
	f.lastSQL = sql
	f.args = args
	if f.err != nil {
		return pgconn.CommandTag{}, f.err
	}
	return pgconn.NewCommandTag("DELETE " + itoaForTag(f.rowsAff)), nil
}

func TestEphemeralTidingsPurger_Run_PassesGraceAndReturnsCount(t *testing.T) {
	fe := &fakeEphemeralTidingsExecer{rowsAff: 3}
	p := newEphemeralTidingsPurgerFromExecer(fe, silentLogger())

	grace := 5 * time.Minute
	got, err := p.Run(context.Background(), grace, 1000)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got != 3 {
		t.Errorf("affected = %d, want 3", got)
	}
	if fe.calls != 1 {
		t.Fatalf("calls = %d, want 1", fe.calls)
	}
	if fe.lastSQL != purgeOrphanEphemeralTidingsSQL {
		t.Errorf("SQL mismatch:\n got: %q\nwant: %q", fe.lastSQL, purgeOrphanEphemeralTidingsSQL)
	}
	// Unlike errand purge, where TTL is embedded in the SQL string, grace is part
	// of the predicate as the $1 argument. Without it, deletion could outrun
	// delivery of the terminal notification (ADR-052(g)).
	if len(fe.args) != 1 {
		t.Fatalf("args len = %d, want 1 (grace interval in predicate)", len(fe.args))
	}
	if d, ok := fe.args[0].(time.Duration); !ok || d != grace {
		t.Errorf("args[0] = %v, want grace=%v", fe.args[0], grace)
	}
}

// TestEphemeralTidingsPurger_SQL_TerminalAndOrphan guards that SQL deletes ONLY
// ephemeral tidings, accounts for terminal+grace, and handles nonexistent
// voyages. This protects the predicate from regressions that would delete
// in-flight or non-terminal rules.
func TestEphemeralTidingsPurger_SQL_TerminalAndOrphan(t *testing.T) {
	sql := purgeOrphanEphemeralTidingsSQL
	for _, must := range []string{
		"t.ephemeral",                // ephemeral only
		"NOT EXISTS",                 // orphaned, voyage deleted
		"v.status IN",                // terminal only
		"v.finished_at < NOW() - $1", // grace period
	} {
		if !strings.Contains(sql, must) {
			t.Errorf("SQL does not contain required fragment %q:\n%s", must, sql)
		}
	}
	// It must not touch non-terminal statuses directly; the IN list is the guard
	// because running/pending/scheduled are absent from it.
	if strings.Contains(sql, "'running'") || strings.Contains(sql, "'pending'") {
		t.Errorf("SQL predicate must not reference non-terminal statuses:\n%s", sql)
	}
}

func TestEphemeralTidingsPurger_Run_PropagatesError(t *testing.T) {
	want := errors.New("pg down")
	fe := &fakeEphemeralTidingsExecer{err: want}
	p := newEphemeralTidingsPurgerFromExecer(fe, silentLogger())

	if _, err := p.Run(context.Background(), time.Minute, 1000); !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
}
