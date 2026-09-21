//go:build integration

// L1 integration for the Cadence spawn tick (NIM-313).
//
// A CORRECTION TO THE TICKET'S MEASUREMENT. NIM-313 lists `internal/conductor`
// as owning 6 SQL statements. It owns none: all six matches are prose inside
// doc comments, and there is not a single Query/Exec call site in the package.
// The SQL lives in `cadence` and `voyage`.
//
// The package is still category (1), for a stronger reason than the axis
// captured. What conductor owns is the TRANSACTION that composes three other
// packages' statements — SelectDueForUpdate, then per due row HasLiveChild /
// voyage.Insert / voyage.InsertTargets / AdvanceSchedule — and transaction
// behaviour is the one thing a fake cannot have. Specifically:
//
//   - FOR UPDATE SKIP LOCKED is what makes two Keeper instances entering spawn
//     at once produce one Voyage instead of two. Row locks exist only in a
//     database.
//   - "an error on any due row rolls back the WHOLE tick" is a claim about
//     rollback, and `spawnFakeTx.Rollback` merely sets a bool.
//   - HasLiveChild's terminal set is an SQL literal that has to agree with
//     voyage.IsTerminal, a Go function. Two copies of one list, in two
//     languages, that nothing compares.
//
// `cadence_spawn_test.go` dispatches on `strings.Contains(sql, "EXISTS")` and
// returns a canned bool. It can prove processOne calls what it means to call. It
// cannot prove any of the above, and the SKIP LOCKED and rollback properties are
// exactly the ones whose failure is silent: a double spawn looks like an operator
// running something twice, and a wedged `queue` schedule looks like a schedule
// nobody set up.
package conductor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/souls-guild/soul-stack/keeper/internal/cadence"
	"github.com/souls-guild/soul-stack/keeper/internal/integrationenv"
	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
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
			log.Fatalf("conductor integration: setup failed (docker required): %v", err)
		}
		log.Printf("conductor integration: skipping, docker unavailable: %v", err)
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

const testAID = "archon-conductor-it"

// resetAll clears the spawn graph and re-seeds the operator every FK in it
// points at. DELETE rather than TRUNCATE: voyages.cadence_id references
// cadences, and TRUNCATE refuses a referenced table.
func resetAll(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		`DELETE FROM voyage_targets`,
		`DELETE FROM voyages`,
		`DELETE FROM cadences`,
		`DELETE FROM operators`,
	} {
		if _, err := integrationPool.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if _, err := integrationPool.Exec(ctx, `
		INSERT INTO operators (aid, display_name, auth_method, created_by_aid)
		VALUES ($1, 'Conductor IT', 'jwt', NULL)`, testAID); err != nil {
		t.Fatalf("seed operator: %v", err)
	}
}

// --- resolvers ----------------------------------------------------------

// fixedScenarioResolver / fixedCommandResolver stand in for the handlers' PG
// resolvers. Scope resolution is not what this suite is about — the transaction
// around it is — so they answer from a fixed list.
type fixedScenarioResolver struct{ out []string }

func (r fixedScenarioResolver) ResolveIncarnations(context.Context, []string, string, string) ([]string, error) {
	return r.out, nil
}

type fixedCommandResolver struct{ out []string }

func (r fixedCommandResolver) ResolveSIDs(context.Context, []string, []string, string, bool) ([]string, error) {
	return r.out, nil
}

// failOnNthResolver answers normally until the nth call, then fails — a
// mid-tick failure after earlier due rows have already done their writes.
type failOnNthResolver struct {
	failOn int
	calls  int
	out    []string
}

func (r *failOnNthResolver) ResolveIncarnations(context.Context, []string, string, string) ([]string, error) {
	r.calls++
	if r.calls >= r.failOn {
		return nil, errors.New("resolver: simulated failure mid-tick")
	}
	return r.out, nil
}

func newSpawner(t *testing.T, resolved []string) *CadenceSpawner {
	t.Helper()
	return NewCadenceSpawner(
		integrationPool,
		fixedScenarioResolver{out: resolved},
		fixedCommandResolver{out: resolved},
		nil, // audit is nil-safe
		nil, // no console enforcer: the gate records `unconfigured`
		nil,
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
	)
}

// --- seeding ------------------------------------------------------------

func ptrInt(v int) *int              { return &v }
func ptrStr(v string) *string        { return &v }
func ptrTime(v time.Time) *time.Time { return &v }

// seedCadence inserts an enabled interval schedule whose next_run_at is `dueIn`
// from now — negative to make it due.
func seedCadence(t *testing.T, id string, policy cadence.OverlapPolicy, dueIn time.Duration) *cadence.Cadence {
	t.Helper()
	c := &cadence.Cadence{
		ID:              id,
		Name:            "spawn-" + id,
		Enabled:         true,
		ScheduleKind:    cadence.ScheduleKindInterval,
		IntervalSeconds: ptrInt(60),
		OverlapPolicy:   policy,
		Kind:            cadence.KindScenario,
		ScenarioName:    ptrStr("converge"),
		Target:          json.RawMessage(`{"coven":["prod"]}`),
		NextRunAt:       ptrTime(time.Now().UTC().Add(dueIn)),
		CreatedByAID:    testAID,
	}
	if err := cadence.Insert(context.Background(), integrationPool, c); err != nil {
		t.Fatalf("seed cadence %s: %v", id, err)
	}
	return c
}

// cadenceID builds a distinct 26-char ULID-like id.
func cadenceID(n int) string { return fmt.Sprintf("01H000000000000000000CAD%02d", n) }

// readSchedule reads back the two timing columns the tick is supposed to move.
func readSchedule(t *testing.T, id string) (nextRun time.Time, lastRun *time.Time) {
	t.Helper()
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT next_run_at, last_run_at FROM cadences WHERE id = $1`, id).Scan(&nextRun, &lastRun); err != nil {
		t.Fatalf("read schedule %s: %v", id, err)
	}
	return nextRun, lastRun
}

func countVoyages(t *testing.T, cadenceID string) int {
	t.Helper()
	var n int
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM voyages WHERE cadence_id = $1`, cadenceID).Scan(&n); err != nil {
		t.Fatalf("count voyages: %v", err)
	}
	return n
}

// --- tests --------------------------------------------------------------

// TestIntegration_Spawn_DueSelectionAndCommit — the happy tick, end to end
// through real SQL: a due schedule produces one Voyage with its targets, the
// back-link is set, and the schedule advances past now.
func TestIntegration_Spawn_DueSelectionAndCommit(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	due := seedCadence(t, cadenceID(1), cadence.OverlapPolicyParallel, -time.Minute)
	// Not due yet: `next_run_at <= NOW()` must exclude it.
	future := seedCadence(t, cadenceID(2), cadence.OverlapPolicyParallel, time.Hour)
	// Due, but disabled: the `enabled` half of the predicate.
	disabled := seedCadence(t, cadenceID(3), cadence.OverlapPolicyParallel, -time.Minute)
	if _, err := integrationPool.Exec(ctx,
		`UPDATE cadences SET enabled = false WHERE id = $1`, disabled.ID); err != nil {
		t.Fatalf("disable %s: %v", disabled.ID, err)
	}

	s := newSpawner(t, []string{"web-01", "web-02", "db-01"})
	spawned, err := s.Run(ctx, 0, 10)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if spawned != 1 {
		t.Fatalf("spawned = %d, want 1 (only the due, enabled schedule)", spawned)
	}

	if got := countVoyages(t, due.ID); got != 1 {
		t.Errorf("voyages for the due cadence = %d, want 1", got)
	}
	if got := countVoyages(t, future.ID); got != 0 {
		t.Errorf("voyages for the future cadence = %d, want 0 — next_run_at <= NOW() is not filtering", got)
	}
	if got := countVoyages(t, disabled.ID); got != 0 {
		t.Errorf("voyages for the disabled cadence = %d, want 0 — the enabled predicate is not filtering", got)
	}

	// The child carries the recipe: kind, scenario and the resolved scope.
	var (
		kind, scenario string
		totalBatches   int
		startedBy      string
		resolvedJSON   []byte
	)
	if err := integrationPool.QueryRow(ctx, `
		SELECT kind, scenario_name, total_batches, started_by_aid, target_resolved
		FROM voyages WHERE cadence_id = $1`, due.ID).
		Scan(&kind, &scenario, &totalBatches, &startedBy, &resolvedJSON); err != nil {
		t.Fatalf("read spawned voyage: %v", err)
	}
	if kind != string(cadence.KindScenario) {
		t.Errorf("voyage kind = %q, want %q", kind, cadence.KindScenario)
	}
	if scenario != "converge" {
		t.Errorf("voyage scenario_name = %q, want converge", scenario)
	}
	// ADR-046 §7: the spawned run acts as the recipe's author, not as the
	// background source. Getting this wrong would run a schedule under nobody's
	// authority.
	if startedBy != testAID {
		t.Errorf("voyage started_by_aid = %q, want the recipe's creator %q", startedBy, testAID)
	}
	var resolved []string
	if err := json.Unmarshal(resolvedJSON, &resolved); err != nil {
		t.Fatalf("unmarshal target_resolved: %v", err)
	}
	if len(resolved) != 3 {
		t.Errorf("target_resolved = %v, want the 3 resolved units", resolved)
	}

	// Targets land in the same tx as the Voyage.
	var targets int
	if err := integrationPool.QueryRow(ctx, `
		SELECT COUNT(*) FROM voyage_targets vt
		JOIN voyages v ON v.voyage_id = vt.voyage_id
		WHERE v.cadence_id = $1`, due.ID).Scan(&targets); err != nil {
		t.Fatalf("count targets: %v", err)
	}
	if targets != 3 {
		t.Errorf("voyage_targets = %d, want 3 — InsertTargets did not commit with the Voyage", targets)
	}

	// The schedule advanced and stamped the run.
	nextRun, lastRun := readSchedule(t, due.ID)
	if !nextRun.After(time.Now().UTC()) {
		t.Errorf("next_run_at = %v, want a future time — the schedule did not advance and will re-spawn forever", nextRun)
	}
	if lastRun == nil {
		t.Error("last_run_at is NULL after a spawn, want the spawn moment")
	}

	// Untouched schedules keep their timings.
	if _, lr := readSchedule(t, future.ID); lr != nil {
		t.Error("the not-due schedule got a last_run_at")
	}
}

// TestIntegration_Spawn_BatchSizeCaps — batchSize is the anti-avalanche guard
// after a long downtime, and it is enforced by `LIMIT $1` inside the locked
// SELECT. A fake returning a canned slice cannot honour a LIMIT.
func TestIntegration_Spawn_BatchSizeCaps(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	for i := 10; i < 15; i++ {
		seedCadence(t, cadenceID(i), cadence.OverlapPolicyParallel, -time.Minute)
	}

	s := newSpawner(t, []string{"web-01"})
	spawned, err := s.Run(ctx, 0, 2)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if spawned != 2 {
		t.Fatalf("spawned = %d with batchSize=2, want 2 — the LIMIT is not capping the tick", spawned)
	}

	// The remaining three are still due and get picked up by later ticks.
	var totalVoyages, stillDue int
	if err := integrationPool.QueryRow(ctx, `SELECT COUNT(*) FROM voyages`).Scan(&totalVoyages); err != nil {
		t.Fatalf("count voyages: %v", err)
	}
	if totalVoyages != 2 {
		t.Errorf("total voyages = %d, want 2", totalVoyages)
	}
	if err := integrationPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM cadences WHERE enabled AND next_run_at <= NOW()`).Scan(&stillDue); err != nil {
		t.Fatalf("count still-due: %v", err)
	}
	if stillDue != 3 {
		t.Errorf("still-due cadences = %d, want 3 — the capped rows must stay due", stillDue)
	}
}

// TestIntegration_Spawn_SkipLockedPreventsDoubleSpawn — the property the whole
// design rests on, and the one a fake tx is structurally incapable of showing.
//
// Two ticks run concurrently against the same due row. FOR UPDATE SKIP LOCKED
// means the second transaction does not wait for the first and does not see the
// row: exactly one Voyage exists afterwards. Without SKIP LOCKED the second tick
// would block, then read the already-advanced row and spawn again — a schedule
// that fires twice per slot on a two-instance cluster, which reads to an
// operator as somebody running the job by hand.
//
// The first tick is held open by driving the transaction directly, so the
// overlap is deterministic rather than a race the test hopes to hit.
func TestIntegration_Spawn_SkipLockedPreventsDoubleSpawn(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	due := seedCadence(t, cadenceID(20), cadence.OverlapPolicyParallel, -time.Minute)

	// Tick A: open a tx and take the lock, then hold it.
	txA, err := integrationPool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin A: %v", err)
	}
	defer func() { _ = txA.Rollback(ctx) }()

	dueA, err := cadence.SelectDueForUpdate(ctx, txA, 10)
	if err != nil {
		t.Fatalf("SelectDueForUpdate A: %v", err)
	}
	if len(dueA) != 1 {
		t.Fatalf("tick A saw %d due rows, want 1", len(dueA))
	}

	// Tick B, while A still holds the lock: the row is skipped, not waited on.
	// A plain FOR UPDATE would block here until A finishes, so reaching this
	// line at all is part of the assertion.
	txB, err := integrationPool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin B: %v", err)
	}
	defer func() { _ = txB.Rollback(ctx) }()

	lockedCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	dueB, err := cadence.SelectDueForUpdate(lockedCtx, txB, 10)
	if err != nil {
		t.Fatalf("SelectDueForUpdate B: %v — a locked row blocked instead of being skipped", err)
	}
	if len(dueB) != 0 {
		t.Fatalf("tick B saw %d due rows while tick A held the lock, want 0 — "+
			"SKIP LOCKED is gone and two instances will double-spawn", len(dueB))
	}

	// A completes its tick.
	voyageID := cadence.NewVoyageID()
	row, targets := cadence.BuildVoyage(dueA[0], voyageID, []string{"web-01"})
	if err := voyage.Insert(ctx, txA, row); err != nil {
		t.Fatalf("voyage.Insert in A: %v", err)
	}
	if err := voyage.InsertTargets(ctx, txA, voyageID, targets); err != nil {
		t.Fatalf("voyage.InsertTargets in A: %v", err)
	}
	next, err := cadence.NextRunAnchored(dueA[0], time.Now().UTC(), time.Now().UTC())
	if err != nil {
		t.Fatalf("NextRunAnchored: %v", err)
	}
	now := time.Now().UTC()
	if err := cadence.AdvanceSchedule(ctx, txA, dueA[0].ID, next, &now); err != nil {
		t.Fatalf("AdvanceSchedule in A: %v", err)
	}
	if err := txA.Commit(ctx); err != nil {
		t.Fatalf("Commit A: %v", err)
	}
	_ = txB.Rollback(ctx)

	if got := countVoyages(t, due.ID); got != 1 {
		t.Errorf("voyages after two overlapping ticks = %d, want exactly 1", got)
	}
}

// TestIntegration_Spawn_ErrorRollsBackTheWholeTick — atomicity, stated against a
// real rollback rather than a bool on a fake.
//
// Run processes due rows in one transaction and returns without committing when
// any of them fails. Nothing from the tick may persist: not the Voyage of a row
// that already succeeded, and not the advanced schedule — otherwise a failing
// schedule would advance past its slot and the run it owed would simply never
// happen.
//
// The failure is provoked in scope resolution of the SECOND due row, after the
// first has already inserted its Voyage and advanced its schedule. That ordering
// is the point: the damage a non-atomic tick does is to the rows that succeeded.
// Resolvers are the honest place for it — they query Postgres themselves, so a
// mid-tick failure there is the realistic case rather than a contrived one.
func TestIntegration_Spawn_ErrorRollsBackTheWholeTick(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	// Two due schedules. Rows are processed in `next_run_at ASC` order, so the
	// older one is handled first and the failure lands on the second.
	good := seedCadence(t, cadenceID(30), cadence.OverlapPolicyParallel, -2*time.Minute)
	seedCadence(t, cadenceID(31), cadence.OverlapPolicyParallel, -time.Minute)

	beforeNext, _ := readSchedule(t, good.ID)

	s := NewCadenceSpawner(
		integrationPool,
		&failOnNthResolver{failOn: 2, out: []string{"web-01"}},
		fixedCommandResolver{out: []string{"web-01"}},
		nil, nil, nil,
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
	)
	if _, err := s.Run(ctx, 0, 10); err == nil {
		t.Fatal("Run: err = nil, want the failure from the second due row")
	}

	// Nothing from the tick survived — including the first row's work, which had
	// already succeeded when the second one failed.
	var voyages int
	if err := integrationPool.QueryRow(ctx, `SELECT COUNT(*) FROM voyages`).Scan(&voyages); err != nil {
		t.Fatalf("count voyages: %v", err)
	}
	if voyages != 0 {
		t.Errorf("voyages after a failed tick = %d, want 0 — the tick was not atomic", voyages)
	}
	afterNext, afterLast := readSchedule(t, good.ID)
	if !afterNext.Equal(beforeNext) {
		t.Errorf("next_run_at moved from %v to %v on a rolled-back tick — the schedule skipped a slot",
			beforeNext, afterNext)
	}
	if afterLast != nil {
		t.Errorf("last_run_at = %v after a rolled-back tick, want NULL", *afterLast)
	}
}

// TestIntegration_Spawn_OverlapPolicies — the skip/queue/parallel matrix against
// a real HasLiveChild.
//
// The difference between the three is entirely in what happens to next_run_at,
// and it is the difference between a series that keeps its grid, one that waits,
// and one that piles up:
//
//   - parallel — always spawn.
//   - skip     — do not spawn, but DO advance (the series does not stall).
//   - queue    — do not spawn and do NOT advance (retry on the next tick).
//
// A stub answers HasLiveChild from a bool the test set. Here the answer comes
// from a real non-terminal Voyage row, which is the only way to know the EXISTS
// query and the live/terminal split agree.
func TestIntegration_Spawn_OverlapPolicies(t *testing.T) {
	cases := []struct {
		policy      cadence.OverlapPolicy
		wantSpawn   int64
		wantAdvance bool
		wantLastRun bool
	}{
		{cadence.OverlapPolicyParallel, 1, true, true},
		{cadence.OverlapPolicySkip, 0, true, false},
		{cadence.OverlapPolicyQueue, 0, false, false},
	}
	for _, c := range cases {
		t.Run(string(c.policy), func(t *testing.T) {
			resetAll(t)
			ctx := context.Background()

			cad := seedCadence(t, cadenceID(40), c.policy, -time.Minute)
			beforeNext, _ := readSchedule(t, cad.ID)

			// A live child: a Voyage in a non-terminal status, linked back.
			seedChild(t, cad.ID, voyage.StatusRunning)

			s := newSpawner(t, []string{"web-01"})
			spawned, err := s.Run(ctx, 0, 10)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if spawned != c.wantSpawn {
				t.Errorf("spawned = %d, want %d", spawned, c.wantSpawn)
			}

			afterNext, afterLast := readSchedule(t, cad.ID)
			advanced := !afterNext.Equal(beforeNext)
			if advanced != c.wantAdvance {
				if c.wantAdvance {
					t.Errorf("next_run_at did not advance under %s — the series stalls", c.policy)
				} else {
					t.Errorf("next_run_at advanced under %s — the queued run was dropped instead of retried", c.policy)
				}
			}
			if (afterLast != nil) != c.wantLastRun {
				t.Errorf("last_run_at set = %v, want %v", afterLast != nil, c.wantLastRun)
			}
		})
	}
}

// seedChild inserts a Voyage in the given status linked to cadenceID.
//
// The claim and finished_at columns are filled per status because `voyages`
// enforces two CHECKs on them — running implies a claim, terminal implies a
// finish time. A hand-written INSERT that ignores them is rejected, which is
// itself a reminder of what this suite is for.
func seedChild(t *testing.T, cadenceID string, status voyage.Status) string {
	t.Helper()
	id := cadence.NewVoyageID()

	var claimedBy, claimExpires, finishedAt any
	if status == voyage.StatusRunning {
		claimedBy = "keeper-conductor-it"
		claimExpires = time.Now().UTC().Add(time.Minute)
	}
	if voyage.IsTerminal(status) {
		finishedAt = time.Now().UTC()
	}

	if _, err := integrationPool.Exec(context.Background(), `
		INSERT INTO voyages (voyage_id, kind, scenario_name, target_resolved, target_origin,
		                     total_batches, started_by_aid, status, cadence_id,
		                     claimed_by_kid, claim_expires_at, finished_at)
		VALUES ($1, 'scenario', 'converge', '[]'::jsonb, '{}'::jsonb, 1, $2, $3, $4, $5, $6, $7)`,
		id, testAID, string(status), cadenceID, claimedBy, claimExpires, finishedAt); err != nil {
		t.Fatalf("seed child (%s): %v", status, err)
	}
	return id
}

// TestIntegration_Spawn_LiveChildMatchesIsTerminal — the SQL literal in
// hasLiveChildSQL and the Go function voyage.IsTerminal are two copies of one
// list, and nothing in the build compares them.
//
// Driven off the declared Status set rather than a hand-written pair of lists,
// so a status added to voyage without amending the SQL fails here. The
// consequence of drift is one-sided and quiet: a new terminal status the SQL
// does not know keeps counting as "live", so every skip/queue schedule with a
// finished child stops spawning and nothing reports it.
func TestIntegration_Spawn_LiveChildMatchesIsTerminal(t *testing.T) {
	all := []voyage.Status{
		voyage.StatusScheduled, voyage.StatusPending, voyage.StatusRunning,
		voyage.StatusSucceeded, voyage.StatusFailed, voyage.StatusPartialFailed,
		voyage.StatusCancelled,
	}
	for _, st := range all {
		t.Run(string(st), func(t *testing.T) {
			resetAll(t)
			ctx := context.Background()

			cad := seedCadence(t, cadenceID(50), cadence.OverlapPolicySkip, -time.Minute)
			seedChild(t, cad.ID, st)

			live, err := cadence.HasLiveChild(ctx, integrationPool, cad.ID)
			if err != nil {
				t.Fatalf("HasLiveChild: %v", err)
			}
			want := !voyage.IsTerminal(st)
			if live != want {
				t.Errorf("HasLiveChild with a %q child = %v, want %v — "+
					"the terminal set in hasLiveChildSQL and voyage.IsTerminal have drifted",
					st, live, want)
			}
		})
	}
}

// TestIntegration_Spawn_EmptyResolveAdvancesWithoutVoyage — a target that
// resolves to nothing is normal in the background (hosts drained, incarnations
// gone), not an error. The schedule must advance anyway; leaving it due would
// make the tick spin on it forever.
func TestIntegration_Spawn_EmptyResolveAdvancesWithoutVoyage(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	cad := seedCadence(t, cadenceID(60), cadence.OverlapPolicyParallel, -time.Minute)
	beforeNext, _ := readSchedule(t, cad.ID)

	s := newSpawner(t, nil) // resolves to no units
	spawned, err := s.Run(ctx, 0, 10)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if spawned != 0 {
		t.Errorf("spawned = %d on an empty resolve, want 0", spawned)
	}
	if got := countVoyages(t, cad.ID); got != 0 {
		t.Errorf("voyages = %d on an empty resolve, want 0", got)
	}

	afterNext, afterLast := readSchedule(t, cad.ID)
	if !afterNext.After(beforeNext) {
		t.Errorf("next_run_at = %v (was %v) — an empty resolve left the row due and the tick will spin",
			afterNext, beforeNext)
	}
	if afterLast == nil {
		t.Error("last_run_at is NULL after an empty-resolve tick, want the tick moment (it was handled)")
	}
}

// TestIntegration_Spawn_NoDueRowsIsANoOp — the quiet case. Nothing due means no
// writes at all; a tick that advanced something here would be corrupting
// schedules on every idle poll.
func TestIntegration_Spawn_NoDueRowsIsANoOp(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	cad := seedCadence(t, cadenceID(70), cadence.OverlapPolicyParallel, time.Hour)
	beforeNext, _ := readSchedule(t, cad.ID)

	s := newSpawner(t, []string{"web-01"})
	spawned, err := s.Run(ctx, 0, 10)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if spawned != 0 {
		t.Errorf("spawned = %d with nothing due, want 0", spawned)
	}

	afterNext, afterLast := readSchedule(t, cad.ID)
	if !afterNext.Equal(beforeNext) {
		t.Errorf("next_run_at moved on an idle tick: %v → %v", beforeNext, afterNext)
	}
	if afterLast != nil {
		t.Error("last_run_at was stamped on an idle tick")
	}
}
