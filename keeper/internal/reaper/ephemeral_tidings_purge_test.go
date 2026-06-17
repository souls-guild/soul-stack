package reaper

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// fakeEphemeralTidingsExecer — fake ephemeralTidingsExecer для unit-тестов.
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
	// В отличие от errand-purge (TTL зашит в строку), grace ВХОДИТ в предикат
	// как $1-аргумент — без него снос опередил бы доставку терминального
	// уведомления (ADR-052(g)).
	if len(fe.args) != 1 {
		t.Fatalf("args len = %d, want 1 (grace-интервал в предикате)", len(fe.args))
	}
	if d, ok := fe.args[0].(time.Duration); !ok || d != grace {
		t.Errorf("args[0] = %v, want grace=%v", fe.args[0], grace)
	}
}

// TestEphemeralTidingsPurger_SQL_TerminalAndOrphan — guard: SQL сносит ТОЛЬКО
// ephemeral, учитывает terminal+grace И несуществующий voyage. Защита от
// регресса предиката (снёс бы in-flight / non-terminal правила).
func TestEphemeralTidingsPurger_SQL_TerminalAndOrphan(t *testing.T) {
	sql := purgeOrphanEphemeralTidingsSQL
	for _, must := range []string{
		"t.ephemeral",                // только разовые
		"NOT EXISTS",                 // осиротевший (voyage удалён)
		"v.status IN",                // только терминал
		"v.finished_at < NOW() - $1", // grace-период
	} {
		if !strings.Contains(sql, must) {
			t.Errorf("SQL не содержит обязательный фрагмент %q:\n%s", must, sql)
		}
	}
	// Не должен трогать non-terminal статусы напрямую — гарантия через IN-список
	// (running/pending/scheduled в нём отсутствуют).
	if strings.Contains(sql, "'running'") || strings.Contains(sql, "'pending'") {
		t.Errorf("SQL предикат не должен ссылаться на non-terminal статусы:\n%s", sql)
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
