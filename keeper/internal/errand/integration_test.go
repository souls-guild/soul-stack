//go:build integration

// L1 integration for the Errand store (NIM-313, sibling of the pushprovider
// suite from NIM-239).
//
// WHY. `store.go` owns six SQL statements against `errands` and not one of them
// had ever met a Postgres: the package's four test files cover the dispatcher,
// cancellation and masking, all against a fake ExecQueryRower. Four properties
// of this layer live entirely in the database:
//
//   - `input` and `output` are jsonb, so what a module put in is not necessarily
//     what the next reader gets out — numbers come back float64.
//   - `output` distinguishes SQL NULL ("the module returned no structured
//     output") from '{}' ("it returned an empty object"). That distinction is
//     the whole reason marshalJSONBNullable exists, and only a real column can
//     hold it.
//   - stdout / stderr / error_message make a round-trip through NULL: NULLIF
//     against the empty string on write, COALESCE back to it on read. A fake
//     echoes the string it was handed and never visits either side.
//   - `SweepOrphanRunning` casts a Go time.Duration to `$3::interval`. pgx
//     5.10's interval codec has no time.Duration plan, so the value only reaches
//     Postgres through the fmt.Stringer fallback, as the text "5m0s". Whether
//     Postgres parses that spelling is a property of Postgres.
//
// The last one is the sharpest: SweepOrphanRunning runs on keeper restart, and
// its job is to close out Errands whose waiting goroutine died with the previous
// process. If the cast fails, the sweep errors on every startup and those rows
// stay `running` forever — an operator polling for a result waits on a reply
// that can never arrive.
package errand

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/souls-guild/soul-stack/keeper/internal/integrationenv"
	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	"github.com/souls-guild/soul-stack/keeper/migrations"
)

var integrationPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	ctx, cancel := integrationenv.SetupContext()
	defer cancel()

	ctr, err := integrationenv.Start(ctx, "postgres", func(ctx context.Context) (*tcpostgres.PostgresContainer, error) {
		return tcpostgres.Run(ctx,
			"postgres:16-alpine",
			tcpostgres.WithDatabase("keeper_test"),
			tcpostgres.WithUsername("keeper"),
			tcpostgres.WithPassword("keeper"),
			tcpostgres.BasicWaitStrategies(),
		)
	})
	if err != nil {
		if requireDocker() {
			log.Fatalf("errand integration: setup failed (docker required): %v", err)
		}
		log.Printf("errand integration: skipping, docker unavailable: %v", err)
		return 0
	}
	defer func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		_ = ctr.Terminate(termCtx)
	}()

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Printf("ConnectionString: %v", err)
		return 1
	}
	if err := migrate.Apply(ctx, dsn, migrations.FS, "."); err != nil {
		log.Printf("migrate.Apply: %v", err)
		return 1
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Printf("pgxpool.New: %v", err)
		return 1
	}
	defer pool.Close()
	integrationPool = pool

	return m.Run()
}

const (
	testAID = "archon-errand-it"
	testKID = "keeper-errand-it"
)

// resetAll truncates and re-seeds the operator that
// `errands_started_by_aid_fk` points at.
func resetAll(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	if _, err := integrationPool.Exec(ctx,
		`TRUNCATE TABLE errands, operators CASCADE`); err != nil {
		t.Fatalf("TRUNCATE: %v", err)
	}
	if _, err := integrationPool.Exec(ctx, `
		INSERT INTO operators (aid, display_name, auth_method, created_by_aid)
		VALUES ($1, 'Errand IT', 'jwt', NULL)`, testAID); err != nil {
		t.Fatalf("seed operator: %v", err)
	}
	return NewStore(integrationPool)
}

// newRow builds a `running` row the way Dispatcher.Dispatch does.
func newRow(id, sid, module string) Row {
	now := time.Now().UTC()
	return Row{
		ErrandID:     id,
		SID:          sid,
		Module:       module,
		Input:        map[string]any{"cmd": "uptime", "timeout": float64(30)},
		Status:       StatusRunning,
		StartedByAID: testAID,
		StartedByKID: testKID,
		StartedAt:    now,
		TTLAt:        now.Add(7 * 24 * time.Hour),
	}
}

func i32(v int32) *int32 { return &v }
func i64(v int64) *int64 { return &v }

// TestIntegration_Errand_RoundTrip — every column, in and back out.
func TestIntegration_Errand_RoundTrip(t *testing.T) {
	s := resetAll(t)
	ctx := context.Background()

	in := newRow("01H0000000000000000ERRND1", "web-01.example.com", "core.cmd.shell")
	if err := s.Insert(ctx, in); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, err := s.Get(ctx, in.ErrandID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// sid and module are adjacent TEXT columns in the INSERT list and in both
	// SELECT lists; transposing them would still scan cleanly (NIM-289).
	if got.SID != "web-01.example.com" {
		t.Errorf("SID = %q, want web-01.example.com", got.SID)
	}
	if got.Module != "core.cmd.shell" {
		t.Errorf("Module = %q, want core.cmd.shell", got.Module)
	}
	if got.Status != StatusRunning {
		t.Errorf("Status = %q, want %q", got.Status, StatusRunning)
	}
	if got.StartedByAID != testAID {
		t.Errorf("StartedByAID = %q, want %q", got.StartedByAID, testAID)
	}
	if got.StartedByKID != testKID {
		t.Errorf("StartedByKID = %q, want %q", got.StartedByKID, testKID)
	}
	if got.Input["cmd"] != "uptime" {
		t.Errorf("input.cmd = %v, want uptime — the jsonb round-trip lost a value", got.Input["cmd"])
	}
	// jsonb gives numbers back as float64. Pinning the type is the point: a
	// caller that type-asserts int compiles and then panics in production.
	if got.Input["timeout"] != float64(30) {
		t.Errorf("input.timeout = %#v (%T), want float64(30)", got.Input["timeout"], got.Input["timeout"])
	}
	if got.StartedAt.IsZero() {
		t.Error("StartedAt is zero")
	}
	if got.TTLAt.IsZero() {
		t.Error("TTLAt is zero")
	}
	// A running row has nothing terminal yet. These are the NULL columns, and
	// each one carries meaning: a non-nil ExitCode on a running Errand would
	// read as "finished with that code".
	if got.ExitCode != nil {
		t.Errorf("ExitCode = %d on a running row, want nil", *got.ExitCode)
	}
	if got.FinishedAt != nil {
		t.Errorf("FinishedAt = %v on a running row, want nil", *got.FinishedAt)
	}
	if got.DurationMs != nil {
		t.Errorf("DurationMs = %d on a running row, want nil", *got.DurationMs)
	}
	if got.Output != nil {
		t.Errorf("Output = %v on a running row, want nil", got.Output)
	}
	// COALESCE(col, '') turns the NULL text columns into empty strings.
	if got.Stdout != "" || got.Stderr != "" || got.ErrorMessage != "" {
		t.Errorf("stdout/stderr/error_message = %q/%q/%q on a running row, want all empty",
			got.Stdout, got.Stderr, got.ErrorMessage)
	}
	if got.StdoutTruncated || got.StderrTruncated {
		t.Error("truncation flags set on a running row, want both false")
	}
}

// TestIntegration_Errand_GetNotFound — the handler maps this one sentinel to a
// 404. pgx.ErrNoRows only comes from a real query.
func TestIntegration_Errand_GetNotFound(t *testing.T) {
	s := resetAll(t)
	if _, err := s.Get(context.Background(), "01H0000000000000000MISSING"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get on an absent id: err = %v, want ErrNotFound", err)
	}
}

// TestIntegration_Errand_MarkTerminalRoundTrip — the terminal update, which is
// where the NULL round-trips and the output jsonb live.
func TestIntegration_Errand_MarkTerminalRoundTrip(t *testing.T) {
	s := resetAll(t)
	ctx := context.Background()

	in := newRow("01H0000000000000000ERRND2", "web-01.example.com", "core.augur.facts")
	if err := s.Insert(ctx, in); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	ok, err := s.MarkTerminal(ctx, in.ErrandID, TerminalUpdate{
		Status:          StatusSuccess,
		ExitCode:        i32(0),
		Stdout:          "load average: 0.15",
		Stderr:          "",
		StdoutTruncated: true,
		StderrTruncated: false,
		DurationMs:      i64(1234),
		ErrorMessage:    "",
		Output:          map[string]any{"uptime_seconds": float64(98765), "host": "web-01"},
	})
	if err != nil {
		t.Fatalf("MarkTerminal: %v", err)
	}
	if !ok {
		t.Fatal("MarkTerminal returned false on a running row, want true")
	}

	got, err := s.Get(ctx, in.ErrandID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusSuccess {
		t.Errorf("Status = %q, want %q", got.Status, StatusSuccess)
	}
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Errorf("ExitCode = %v, want 0 — a zero exit code must survive as 0, not collapse to NULL", got.ExitCode)
	}
	if got.Stdout != "load average: 0.15" {
		t.Errorf("Stdout = %q, want the captured text", got.Stdout)
	}
	// Written as '' through NULLIF, therefore stored as NULL, therefore read
	// back as '' through COALESCE. The two halves have to agree; a fake sees
	// neither.
	if got.Stderr != "" {
		t.Errorf("Stderr = %q, want empty — the NULLIF/COALESCE pair disagrees", got.Stderr)
	}
	if got.ErrorMessage != "" {
		t.Errorf("ErrorMessage = %q, want empty", got.ErrorMessage)
	}
	// The two truncation flags are adjacent BOOLEAN columns with different
	// values here, so a transposition shows up instead of cancelling out.
	if !got.StdoutTruncated {
		t.Error("StdoutTruncated = false, want true")
	}
	if got.StderrTruncated {
		t.Error("StderrTruncated = true, want false")
	}
	if got.DurationMs == nil || *got.DurationMs != 1234 {
		t.Errorf("DurationMs = %v, want 1234", got.DurationMs)
	}
	if got.Output["uptime_seconds"] != float64(98765) {
		t.Errorf("output.uptime_seconds = %#v (%T), want float64(98765)",
			got.Output["uptime_seconds"], got.Output["uptime_seconds"])
	}
	if got.Output["host"] != "web-01" {
		t.Errorf("output.host = %v, want web-01", got.Output["host"])
	}
	if got.FinishedAt == nil {
		t.Error("FinishedAt is nil after a terminal update — the NOW() assignment did not fire")
	}
}

// TestIntegration_Errand_NilOutputIsSQLNullNotEmptyObject — marshalJSONBNullable's
// entire reason for existing, stated against a real jsonb column.
//
// A shell module returns no structured output, and that has to stay
// distinguishable from a module that returned `{}`. Collapsing the two would
// make an empty result indistinguishable from no result, which is the difference
// between "the module ran and found nothing" and "the module returned nothing".
func TestIntegration_Errand_NilOutputIsSQLNullNotEmptyObject(t *testing.T) {
	s := resetAll(t)
	ctx := context.Background()

	shell := newRow("01H0000000000000000ERRND3", "web-01.example.com", "core.cmd.shell")
	empty := newRow("01H0000000000000000ERRND4", "web-01.example.com", "core.augur.facts")
	for _, r := range []Row{shell, empty} {
		if err := s.Insert(ctx, r); err != nil {
			t.Fatalf("Insert(%s): %v", r.ErrandID, err)
		}
	}

	if _, err := s.MarkTerminal(ctx, shell.ErrandID, TerminalUpdate{
		Status: StatusSuccess, Output: nil,
	}); err != nil {
		t.Fatalf("MarkTerminal(nil output): %v", err)
	}
	if _, err := s.MarkTerminal(ctx, empty.ErrandID, TerminalUpdate{
		Status: StatusSuccess, Output: map[string]any{},
	}); err != nil {
		t.Fatalf("MarkTerminal(empty output): %v", err)
	}

	// At the column level: one is SQL NULL, the other is a jsonb object.
	var shellNull, emptyNull bool
	if err := integrationPool.QueryRow(ctx,
		`SELECT output IS NULL FROM errands WHERE errand_id = $1`, shell.ErrandID).Scan(&shellNull); err != nil {
		t.Fatalf("probe shell output: %v", err)
	}
	if err := integrationPool.QueryRow(ctx,
		`SELECT output IS NULL FROM errands WHERE errand_id = $1`, empty.ErrandID).Scan(&emptyNull); err != nil {
		t.Fatalf("probe empty output: %v", err)
	}
	if !shellNull {
		t.Error("a nil Output was not stored as SQL NULL — no-output is indistinguishable from empty-output")
	}
	if emptyNull {
		t.Error("an empty map Output was stored as SQL NULL — empty-output is indistinguishable from no-output")
	}

	// And at the Go level, after the scan: nil versus a non-nil empty map.
	shellGot, err := s.Get(ctx, shell.ErrandID)
	if err != nil {
		t.Fatalf("Get(shell): %v", err)
	}
	if shellGot.Output != nil {
		t.Errorf("Output = %#v for a shell module, want nil", shellGot.Output)
	}
	emptyGot, err := s.Get(ctx, empty.ErrandID)
	if err != nil {
		t.Fatalf("Get(empty): %v", err)
	}
	if emptyGot.Output == nil {
		t.Error("Output = nil for a module that returned {} — the distinction was lost on the way back")
	} else if len(emptyGot.Output) != 0 {
		t.Errorf("Output = %#v, want an empty map", emptyGot.Output)
	}
}

// TestIntegration_Errand_MarkTerminalIsSingleWinner — the `AND status='running'`
// guard. Two paths race to finalise an Errand (the synchronous waiter and the
// timeout escalation); the first must win and the second must be told it lost,
// so it does not overwrite the real result with a timeout.
//
// RowsAffected here is a real command tag rather than a fake's return value.
func TestIntegration_Errand_MarkTerminalIsSingleWinner(t *testing.T) {
	s := resetAll(t)
	ctx := context.Background()

	in := newRow("01H0000000000000000ERRND5", "web-01.example.com", "core.cmd.shell")
	if err := s.Insert(ctx, in); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	ok, err := s.MarkTerminal(ctx, in.ErrandID, TerminalUpdate{Status: StatusSuccess, ExitCode: i32(0)})
	if err != nil {
		t.Fatalf("first MarkTerminal: %v", err)
	}
	if !ok {
		t.Fatal("first MarkTerminal = false, want true")
	}

	ok, err = s.MarkTerminal(ctx, in.ErrandID, TerminalUpdate{
		Status: StatusTimedOut, ErrorMessage: "deadline exceeded",
	})
	if err != nil {
		t.Fatalf("second MarkTerminal: %v", err)
	}
	if ok {
		t.Error("second MarkTerminal = true, want false — the status='running' guard is not holding")
	}

	// The loser must not have overwritten anything.
	got, err := s.Get(ctx, in.ErrandID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusSuccess {
		t.Errorf("Status = %q, want %q — the late timeout overwrote the real result", got.Status, StatusSuccess)
	}
	if got.ErrorMessage != "" {
		t.Errorf("ErrorMessage = %q, want empty — the losing update leaked its reason in", got.ErrorMessage)
	}

	// An absent id is also "not running", and reports the same way.
	ok, err = s.MarkTerminal(ctx, "01H0000000000000000MISSING", TerminalUpdate{Status: StatusFailed})
	if err != nil {
		t.Fatalf("MarkTerminal on an absent id: %v", err)
	}
	if ok {
		t.Error("MarkTerminal on an absent id = true, want false")
	}
}

// TestIntegration_Errand_ListFiltersAndOrder — buildListWhere assembles $N
// placeholders by hand, so an off-by-one in the numbering is a runtime error that
// only Postgres raises. This runs every filter, alone and combined, and checks
// that `total` is the count under the same predicate rather than the page size.
func TestIntegration_Errand_ListFiltersAndOrder(t *testing.T) {
	s := resetAll(t)
	ctx := context.Background()

	seed := []struct {
		id     string
		sid    string
		module string
		status Status
	}{
		{"01H0000000000000000ERRNDA", "web-01.example.com", "core.cmd.shell", StatusSuccess},
		{"01H0000000000000000ERRNDB", "web-01.example.com", "core.augur.facts", StatusFailed},
		{"01H0000000000000000ERRNDC", "db-01.example.com", "core.cmd.shell", StatusSuccess},
		{"01H0000000000000000ERRNDD", "db-01.example.com", "core.pkg.present", StatusRunning},
	}
	for _, sd := range seed {
		r := newRow(sd.id, sd.sid, sd.module)
		if err := s.Insert(ctx, r); err != nil {
			t.Fatalf("Insert(%s): %v", sd.id, err)
		}
		if sd.status != StatusRunning {
			if _, err := s.MarkTerminal(ctx, sd.id, TerminalUpdate{Status: sd.status}); err != nil {
				t.Fatalf("MarkTerminal(%s): %v", sd.id, err)
			}
		}
	}

	cases := []struct {
		name   string
		filter ListFilter
		want   int
	}{
		{"no filter", ListFilter{}, 4},
		{"sid", ListFilter{SID: "web-01.example.com"}, 2},
		{"status", ListFilter{Status: StatusSuccess}, 2},
		{"one module", ListFilter{Modules: []string{"core.cmd.shell"}}, 2},
		{"two modules OR", ListFilter{Modules: []string{"core.cmd.shell", "core.pkg.present"}}, 3},
		// Every predicate at once: this is where a mis-numbered placeholder in
		// buildListWhere surfaces, because the multi-value IN() list shifts the
		// LIMIT/OFFSET positions that follow it.
		{
			"sid AND status AND modules",
			ListFilter{
				SID:     "web-01.example.com",
				Status:  StatusSuccess,
				Modules: []string{"core.cmd.shell", "core.augur.facts"},
			},
			1,
		},
		{"unknown module matches nothing", ListFilter{Modules: []string{"core.nope.absent"}}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rows, total, err := s.List(ctx, c.filter, 0, 50)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if total != c.want {
				t.Errorf("total = %d, want %d", total, c.want)
			}
			if len(rows) != c.want {
				t.Errorf("len(rows) = %d, want %d", len(rows), c.want)
			}
		})
	}

	// StartedAfter is strict (`started_at > $n`), so a bound at now excludes
	// everything already written.
	if _, total, err := s.List(ctx, ListFilter{StartedAfter: time.Now().UTC().Add(time.Minute)}, 0, 50); err != nil {
		t.Fatalf("List(StartedAfter future): %v", err)
	} else if total != 0 {
		t.Errorf("total with a future StartedAfter = %d, want 0", total)
	}
	if _, total, err := s.List(ctx, ListFilter{StartedAfter: time.Now().UTC().Add(-time.Hour)}, 0, 50); err != nil {
		t.Fatalf("List(StartedAfter past): %v", err)
	} else if total != 4 {
		t.Errorf("total with a past StartedAfter = %d, want 4", total)
	}

	// Ordering is `started_at DESC` — newest first. Checked pairwise against the
	// invariant rather than against a hardcoded permutation: the rows are written
	// in one burst and may share a timestamp, so a fixed expected order would be
	// an assertion about the clock. What the contract promises is monotonicity.
	rows, _, err := s.List(ctx, ListFilter{}, 0, 50)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].StartedAt.After(rows[i-1].StartedAt) {
			t.Errorf("row[%d] (%s) is newer than row[%d] (%s) — order is not started_at DESC",
				i, rows[i].ErrandID, i-1, rows[i-1].ErrandID)
		}
	}

	// Pagination: total stays the count under the filter, not the page length.
	page, total, err := s.List(ctx, ListFilter{}, 1, 2)
	if err != nil {
		t.Fatalf("List(offset=1,limit=2): %v", err)
	}
	if total != 4 {
		t.Errorf("total with offset/limit = %d, want 4 — COUNT(*) picked up the LIMIT", total)
	}
	if len(page) != 2 {
		t.Errorf("len(page) = %d, want 2", len(page))
	}
}

// TestIntegration_Errand_SweepOrphanRunningCastsInterval — the sweep that runs on
// keeper restart, and the `$3::interval` cast inside it.
//
// A time.Duration is not a type pgx's interval codec knows, so this statement
// reaching Postgres at all depends on a fallback. It runs exactly once per
// process start, in a path an operator never watches, and a failure leaves rows
// stuck `running` forever rather than raising anything visible. That combination
// is why it belongs in an L1 and not in a fake.
func TestIntegration_Errand_SweepOrphanRunningCastsInterval(t *testing.T) {
	s := resetAll(t)
	ctx := context.Background()

	// Three running rows, ours and a neighbour's, with different ages.
	old := newRow("01H0000000000000000ERRNDE", "web-01.example.com", "core.cmd.shell")
	old.StartedAt = time.Now().UTC().Add(-time.Hour)
	recent := newRow("01H0000000000000000ERRNDF", "web-01.example.com", "core.cmd.shell")
	foreign := newRow("01H0000000000000000ERRNDG", "web-01.example.com", "core.cmd.shell")
	foreign.StartedByKID = "keeper-somebody-else"
	foreign.StartedAt = time.Now().UTC().Add(-time.Hour)
	// A terminal row of ours, old enough to match the age predicate: the sweep
	// must leave it alone, because `status='running'` is the other half of the
	// WHERE clause.
	done := newRow("01H0000000000000000ERRNDH", "web-01.example.com", "core.cmd.shell")
	done.StartedAt = time.Now().UTC().Add(-time.Hour)

	for _, r := range []Row{old, recent, foreign, done} {
		if err := s.Insert(ctx, r); err != nil {
			t.Fatalf("Insert(%s): %v", r.ErrandID, err)
		}
	}
	if _, err := s.MarkTerminal(ctx, done.ErrandID, TerminalUpdate{Status: StatusSuccess}); err != nil {
		t.Fatalf("MarkTerminal(done): %v", err)
	}

	const reason = "keeper restarted while the errand was in flight"
	swept, err := s.SweepOrphanRunning(ctx, testKID, 5*time.Minute, reason)
	if err != nil {
		t.Fatalf("SweepOrphanRunning: %v — the time.Duration did not survive the ::interval cast", err)
	}

	if len(swept) != 1 || swept[0] != old.ErrandID {
		t.Fatalf("swept = %v, want exactly [%s]", swept, old.ErrandID)
	}

	// The swept row is timed_out and carries the reason.
	got, err := s.Get(ctx, old.ErrandID)
	if err != nil {
		t.Fatalf("Get(old): %v", err)
	}
	if got.Status != StatusTimedOut {
		t.Errorf("swept row status = %q, want %q", got.Status, StatusTimedOut)
	}
	if got.ErrorMessage != reason {
		t.Errorf("swept row error_message = %q, want %q", got.ErrorMessage, reason)
	}
	if got.FinishedAt == nil {
		t.Error("swept row FinishedAt is nil — the NOW() assignment did not fire")
	}

	// The grace window is what the interval actually controls: a row younger
	// than it stays running. If the cast silently produced something else — a
	// zero interval, say — this is the assertion that notices, because the
	// recent row would have been swept too.
	stillRunning, err := s.Get(ctx, recent.ErrandID)
	if err != nil {
		t.Fatalf("Get(recent): %v", err)
	}
	if stillRunning.Status != StatusRunning {
		t.Errorf("recent row status = %q, want %q — the grace interval did not apply",
			stillRunning.Status, StatusRunning)
	}

	// Another instance's orphan is not ours to close.
	foreignGot, err := s.Get(ctx, foreign.ErrandID)
	if err != nil {
		t.Fatalf("Get(foreign): %v", err)
	}
	if foreignGot.Status != StatusRunning {
		t.Errorf("another KID's row status = %q, want %q — the sweep is not scoped to started_by_kid",
			foreignGot.Status, StatusRunning)
	}

	// And an already-finished row keeps its result.
	doneGot, err := s.Get(ctx, done.ErrandID)
	if err != nil {
		t.Fatalf("Get(done): %v", err)
	}
	if doneGot.Status != StatusSuccess {
		t.Errorf("terminal row status = %q, want %q — the sweep overwrote a finished Errand",
			doneGot.Status, StatusSuccess)
	}

	// A second sweep finds nothing: the transition is not repeatable.
	again, err := s.SweepOrphanRunning(ctx, testKID, 5*time.Minute, reason)
	if err != nil {
		t.Fatalf("second SweepOrphanRunning: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second sweep returned %v, want nothing", again)
	}
}

// TestIntegration_Errand_StatusCheckRejectsUnknown — `errands_status_valid` is
// the schema's copy of the Status enum, and the two drift independently. A
// status Go accepts but the CHECK does not is a write that fails at runtime,
// which is why the constraint is worth touching from here.
func TestIntegration_Errand_StatusCheckRejectsUnknown(t *testing.T) {
	s := resetAll(t)
	ctx := context.Background()

	bad := newRow("01H0000000000000000ERRNDI", "web-01.example.com", "core.cmd.shell")
	bad.Status = Status("almost_done")
	if err := s.Insert(ctx, bad); err == nil {
		t.Error("Insert with an unknown status succeeded — errands_status_valid is not enforcing the enum")
	}

	// Every status the Go layer declares must be one the CHECK admits. Driven
	// off the declared set, so adding a Status without amending the migration
	// fails here rather than in production.
	for i, st := range []Status{
		StatusRunning, StatusSuccess, StatusFailed,
		StatusTimedOut, StatusCancelled, StatusModuleNotAllowed,
	} {
		r := newRow(fmt.Sprintf("01H000000000000000STATU%02d", i), "web-01.example.com", "core.cmd.shell")
		r.Status = st
		if err := s.Insert(ctx, r); err != nil {
			t.Errorf("Insert with status %q: %v — the Go enum and errands_status_valid have drifted", st, err)
		}
	}
}
