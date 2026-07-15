package conductor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/cadence"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// --- fakes ---

// spawnFakeTx is a pgx.Tx stub for processOne/Run spawn tests.
// Distinguishes queries by SQL fragment: HasLiveChild (EXISTS) → bool,
// voyage.Insert (RETURNING created_at) → timestamp, AdvanceSchedule (Exec) →
// UPDATE 1, InsertTargets (CopyFrom) → counter.
type spawnFakeTx struct {
	live bool // HasLiveChild answer

	dueRows []*cadence.Cadence // rows for SelectDueForUpdate (Query)

	execCalls    int // AdvanceSchedule
	copyCalls    int // InsertTargets
	insertCalls  int // voyage.Insert
	hasLiveCalls int
	committed    bool
	rolled       bool

	voyageInsertArgs []any // positional args of voyage.Insert (for the late-binding guard)

	execErr error
}

func (t *spawnFakeTx) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	t.execCalls++
	if t.execErr != nil {
		return pgconn.CommandTag{}, t.execErr
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (t *spawnFakeTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "EXISTS"):
		t.hasLiveCalls++
		return boolRow{v: t.live}
	case strings.Contains(sql, "INSERT INTO voyages"):
		t.insertCalls++
		t.voyageInsertArgs = args
		return tsRow{ts: time.Now()}
	default:
		return errScanRow{err: errors.New("spawnFakeTx: unexpected QueryRow: " + sql)}
	}
}

func (t *spawnFakeTx) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	// SelectDueForUpdate. With no prepared rows — behavior of earlier tests
	// (processOne bypasses Query): signal an error.
	if t.dueRows == nil {
		return nil, errors.New("spawnFakeTx: Query unused in processOne test")
	}
	return &dueRows{src: t.dueRows}, nil
}

func (t *spawnFakeTx) CopyFrom(_ context.Context, _ pgx.Identifier, _ []string, src pgx.CopyFromSource) (int64, error) {
	n := int64(0)
	for src.Next() {
		n++
	}
	t.copyCalls++
	return n, nil
}

func (t *spawnFakeTx) Commit(_ context.Context) error   { t.committed = true; return nil }
func (t *spawnFakeTx) Rollback(_ context.Context) error { t.rolled = true; return nil }
func (t *spawnFakeTx) Begin(context.Context) (pgx.Tx, error) {
	panic("spawnFakeTx.Begin unused")
}
func (t *spawnFakeTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults {
	panic("spawnFakeTx.SendBatch unused")
}
func (t *spawnFakeTx) LargeObjects() pgx.LargeObjects { panic("spawnFakeTx.LargeObjects unused") }
func (t *spawnFakeTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	panic("spawnFakeTx.Prepare unused")
}
func (t *spawnFakeTx) Conn() *pgx.Conn { return nil }

type boolRow struct{ v bool }

func (r boolRow) Scan(dest ...any) error {
	*dest[0].(*bool) = r.v
	return nil
}

type tsRow struct{ ts time.Time }

func (r tsRow) Scan(dest ...any) error {
	*dest[0].(*time.Time) = r.ts
	return nil
}

type errScanRow struct{ err error }

func (r errScanRow) Scan(_ ...any) error { return r.err }

// dueRows is a pgx.Rows stub for SelectDueForUpdate: yields pre-built
// *cadence.Cadence via a scanCadence-compatible Scan (27 dest in scanCadence
// order). Covers the batch/fail-threshold fields (needed by the spawn
// late-binding tests); the rest are nullable — nil.
type dueRows struct {
	src []*cadence.Cadence
	pos int
}

func (r *dueRows) Next() bool {
	if r.pos >= len(r.src) {
		return false
	}
	r.pos++
	return true
}

func (r *dueRows) Scan(dest ...any) error {
	c := r.src[r.pos-1]
	*dest[0].(*string) = c.ID
	*dest[1].(*string) = c.Name
	*dest[2].(*bool) = c.Enabled
	*dest[3].(*string) = string(c.ScheduleKind)
	*dest[4].(**int) = c.IntervalSeconds
	*dest[5].(**string) = c.CronExpr
	*dest[6].(*string) = string(c.OverlapPolicy)
	*dest[7].(*string) = string(c.Kind)
	*dest[8].(**string) = c.ScenarioName
	*dest[9].(**string) = c.Module
	*dest[10].(*json.RawMessage) = c.Target
	*dest[11].(*[]byte) = c.Input
	*dest[12].(**string) = nil
	*dest[13].(**int) = c.BatchSize
	*dest[14].(**int) = c.BatchPercent
	*dest[15].(**int) = nil
	*dest[16].(**int) = c.FailThreshold
	*dest[17].(**int) = c.FailThresholdPercent
	*dest[18].(**float64) = nil
	*dest[19].(**float64) = nil
	*dest[20].(**bool) = nil
	*dest[21].(**string) = nil
	*dest[22].(**time.Time) = c.NextRunAt
	*dest[23].(**time.Time) = c.LastRunAt
	*dest[24].(*string) = c.CreatedByAID
	*dest[25].(*time.Time) = c.CreatedAt
	*dest[26].(*time.Time) = c.UpdatedAt
	return nil
}

func (r *dueRows) Close()                                       {}
func (r *dueRows) Err() error                                   { return nil }
func (r *dueRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *dueRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *dueRows) Values() ([]any, error)                       { return nil, nil }
func (r *dueRows) RawValues() [][]byte                          { return nil }
func (r *dueRows) Conn() *pgx.Conn                              { return nil }

// stubResolver resolves to a fixed snapshot (for both scenario and command).
type stubResolver struct {
	out []string
	err error
}

func (s stubResolver) ResolveIncarnations(context.Context, []string, string, string) ([]string, error) {
	return s.out, s.err
}
func (s stubResolver) ResolveSIDs(context.Context, []string, []string, string, bool) ([]string, error) {
	return s.out, s.err
}

type spawnAudit struct{ events []*audit.Event }

func (a *spawnAudit) Write(_ context.Context, ev *audit.Event) error {
	a.events = append(a.events, ev)
	return nil
}

func intervalScenarioCadence(policy cadence.OverlapPolicy) *cadence.Cadence {
	sec := 300
	next := time.Now().Add(-time.Minute) // due
	name := "converge"
	return &cadence.Cadence{
		ID:              "01H000000000000000000CAD00",
		Name:            "nightly",
		Enabled:         true,
		ScheduleKind:    cadence.ScheduleKindInterval,
		IntervalSeconds: &sec,
		OverlapPolicy:   policy,
		Kind:            cadence.KindScenario,
		ScenarioName:    &name,
		Target:          json.RawMessage(`{"service":"redis"}`),
		NextRunAt:       &next,
		CreatedByAID:    "archon-alice",
	}
}

func newSpawnerFor(tx *spawnFakeTx, res stubResolver, ad audit.Writer) *CadenceSpawner {
	// processOne goes through the passed tx directly; pool isn't used here,
	// but the constructor requires non-nil for the Run test — supply a beginner wrapper.
	return newCadenceSpawnerFromBeginner(spawnOneBeginner{tx: tx}, res, res, ad, silentLogger())
}

type spawnOneBeginner struct{ tx *spawnFakeTx }

func (b spawnOneBeginner) Begin(context.Context) (pgx.Tx, error) { return b.tx, nil }

// --- overlap policy: parallel — always spawns (live ignored) ---

func TestProcessOne_Parallel_SpawnsIgnoringLive(t *testing.T) {
	tx := &spawnFakeTx{live: true} // live child exists, but parallel ignores it
	s := newSpawnerFor(tx, stubResolver{out: []string{"i1", "i2"}}, nil)
	c := intervalScenarioCadence(cadence.OverlapPolicyParallel)

	rec, spawned, err := s.processOne(context.Background(), tx, c, time.Now())
	if err != nil {
		t.Fatalf("processOne: %v", err)
	}
	if !spawned {
		t.Fatal("parallel должен спавнить")
	}
	if tx.hasLiveCalls != 0 {
		t.Errorf("parallel не должен проверять live; hasLiveCalls=%d", tx.hasLiveCalls)
	}
	if tx.insertCalls != 1 || tx.copyCalls != 1 || tx.execCalls != 1 {
		t.Errorf("ожидался insert+targets+advance; insert=%d copy=%d exec=%d",
			tx.insertCalls, tx.copyCalls, tx.execCalls)
	}
	if rec == nil || rec.voyageID == "" {
		t.Error("ожидалась spawned-запись с voyage_id")
	}
}

// --- skip: live child exists → don't spawn, audit skipped, next_run advances ---

func TestProcessOne_Skip_LiveChild_SkipsButAdvances(t *testing.T) {
	tx := &spawnFakeTx{live: true}
	s := newSpawnerFor(tx, stubResolver{out: []string{"i1"}}, nil)
	c := intervalScenarioCadence(cadence.OverlapPolicySkip)

	rec, spawned, err := s.processOne(context.Background(), tx, c, time.Now())
	if err != nil {
		t.Fatalf("processOne: %v", err)
	}
	if spawned {
		t.Fatal("skip с живым ребёнком не должен спавнить")
	}
	if tx.insertCalls != 0 || tx.copyCalls != 0 {
		t.Errorf("skip не должен вставлять voyage; insert=%d copy=%d", tx.insertCalls, tx.copyCalls)
	}
	if tx.execCalls != 1 {
		t.Errorf("skip всё равно двигает next_run (AdvanceSchedule); exec=%d, want 1", tx.execCalls)
	}
	if rec == nil || !rec.skipped {
		t.Error("ожидалась skipped-запись для audit")
	}
}

// --- skip with no live child → spawns ---

func TestProcessOne_Skip_NoLiveChild_Spawns(t *testing.T) {
	tx := &spawnFakeTx{live: false}
	s := newSpawnerFor(tx, stubResolver{out: []string{"i1"}}, nil)
	c := intervalScenarioCadence(cadence.OverlapPolicySkip)

	_, spawned, err := s.processOne(context.Background(), tx, c, time.Now())
	if err != nil {
		t.Fatalf("processOne: %v", err)
	}
	if !spawned {
		t.Fatal("skip без живого ребёнка должен спавнить")
	}
	if tx.insertCalls != 1 {
		t.Errorf("insert=%d, want 1", tx.insertCalls)
	}
}

// voyageInsertFailThresholdIdx is the position of fail_threshold in voyage
// insertSQL (column $19 → 0-indexed args[18]; see voyage/crud.go insertSQL
// VALUES).
const voyageInsertFailThresholdIdx = 18

// TestProcessOne_FailThresholdPercent_ResolvedOnSpawnScope is the KEY
// late-binding test across conductor (ADR-043 amendment 2026-06-09,
// Cadence-recipe S3): the cadence recipe carries fail_threshold_percent=30;
// the spawn scope resolves to 10 units; the spawned Voyage must get the
// ABSOLUTE fail_threshold=ceil(10*30/100)=3 (resolved on the spawn scope in
// BuildVoyage, NOT at create time). We check the positional voyage-insert
// arg — the threshold actually reaches the inserted Voyage row.
func TestProcessOne_FailThresholdPercent_ResolvedOnSpawnScope(t *testing.T) {
	tx := &spawnFakeTx{live: false}
	resolved := []string{"i1", "i2", "i3", "i4", "i5", "i6", "i7", "i8", "i9", "i10"} // scope=10
	s := newSpawnerFor(tx, stubResolver{out: resolved}, nil)

	c := intervalScenarioCadence(cadence.OverlapPolicyParallel)
	pct := 30
	c.FailThresholdPercent = &pct // percent recipe, the absolute is unknown until spawn

	_, spawned, err := s.processOne(context.Background(), tx, c, time.Now())
	if err != nil {
		t.Fatalf("processOne: %v", err)
	}
	if !spawned {
		t.Fatal("ожидался спавн")
	}
	if len(tx.voyageInsertArgs) <= voyageInsertFailThresholdIdx {
		t.Fatalf("voyageInsertArgs len=%d, ожидался хотя бы %d", len(tx.voyageInsertArgs), voyageInsertFailThresholdIdx+1)
	}
	got := tx.voyageInsertArgs[voyageInsertFailThresholdIdx]
	n, ok := got.(int)
	if !ok {
		t.Fatalf("voyage fail_threshold arg = %T (%v), want int", got, got)
	}
	if n != 3 { // ceil(10*30/100)
		t.Errorf("спавнящийся Voyage.fail_threshold = %d, want 3 (30%% от spawn-scope=10)", n)
	}
}

// TestProcessOne_FailThresholdPercent_DifferentSpawnScope — the same recipe
// percent with a DIFFERENT resolved scope yields a different absolute
// (proves the resolve happens on the spawn scope, not a fixed create-time
// scope): 30% of 100 = 30.
func TestProcessOne_FailThresholdPercent_DifferentSpawnScope(t *testing.T) {
	tx := &spawnFakeTx{live: false}
	resolved := make([]string, 100)
	for i := range resolved {
		resolved[i] = "u"
	}
	s := newSpawnerFor(tx, stubResolver{out: resolved}, nil)

	c := intervalScenarioCadence(cadence.OverlapPolicyParallel)
	pct := 30
	c.FailThresholdPercent = &pct

	if _, _, err := s.processOne(context.Background(), tx, c, time.Now()); err != nil {
		t.Fatalf("processOne: %v", err)
	}
	got := tx.voyageInsertArgs[voyageInsertFailThresholdIdx]
	if n, ok := got.(int); !ok || n != 30 {
		t.Errorf("спавнящийся Voyage.fail_threshold = %v, want 30 (30%% от 100)", got)
	}
}

// --- queue: live child exists → don't spawn AND don't advance next_run (wait) ---

func TestProcessOne_Queue_LiveChild_WaitsNoAdvance(t *testing.T) {
	tx := &spawnFakeTx{live: true}
	s := newSpawnerFor(tx, stubResolver{out: []string{"i1"}}, nil)
	c := intervalScenarioCadence(cadence.OverlapPolicyQueue)

	rec, spawned, err := s.processOne(context.Background(), tx, c, time.Now())
	if err != nil {
		t.Fatalf("processOne: %v", err)
	}
	if spawned {
		t.Fatal("queue с живым ребёнком не должен спавнить")
	}
	if tx.execCalls != 0 {
		t.Errorf("queue НЕ двигает next_run при живом ребёнке; exec=%d, want 0", tx.execCalls)
	}
	if rec != nil {
		t.Error("queue-ожидание не пишет audit (это не пропуск)")
	}
}

// --- queue with no live child → spawns ---

func TestProcessOne_Queue_NoLiveChild_Spawns(t *testing.T) {
	tx := &spawnFakeTx{live: false}
	s := newSpawnerFor(tx, stubResolver{out: []string{"i1", "i2"}}, nil)
	c := intervalScenarioCadence(cadence.OverlapPolicyQueue)

	_, spawned, err := s.processOne(context.Background(), tx, c, time.Now())
	if err != nil {
		t.Fatalf("processOne: %v", err)
	}
	if !spawned {
		t.Fatal("queue без живого ребёнка должен спавнить")
	}
	if tx.insertCalls != 1 || tx.execCalls != 1 {
		t.Errorf("insert=%d exec=%d, want 1/1", tx.insertCalls, tx.execCalls)
	}
}

// --- empty resolve → doesn't spawn, but advances the schedule (not an error) ---

func TestProcessOne_EmptyScope_AdvancesNoSpawn(t *testing.T) {
	tx := &spawnFakeTx{live: false}
	s := newSpawnerFor(tx, stubResolver{out: nil}, nil)
	c := intervalScenarioCadence(cadence.OverlapPolicyParallel)

	rec, spawned, err := s.processOne(context.Background(), tx, c, time.Now())
	if err != nil {
		t.Fatalf("processOne: %v", err)
	}
	if spawned || rec != nil {
		t.Fatal("пустой резолв не спавнит и не пишет audit")
	}
	if tx.insertCalls != 0 {
		t.Errorf("пустой резолв не вставляет voyage; insert=%d", tx.insertCalls)
	}
	if tx.execCalls != 1 {
		t.Errorf("пустой резолв двигает расписание; exec=%d, want 1", tx.execCalls)
	}
}

// --- resolver failed → the error propagates (tx rollback) ---

func TestProcessOne_ResolverError_Propagates(t *testing.T) {
	tx := &spawnFakeTx{live: false}
	want := errors.New("pg down")
	s := newSpawnerFor(tx, stubResolver{err: want}, nil)
	c := intervalScenarioCadence(cadence.OverlapPolicyParallel)

	if _, _, err := s.processOne(context.Background(), tx, c, time.Now()); !errors.Is(err, want) {
		t.Fatalf("err = %v, want wrap of %v", err, want)
	}
}

// --- audit: spawned event of the correct type/source ---

func TestSpawner_Emit_SpawnedEvent(t *testing.T) {
	ad := &spawnAudit{}
	s := newSpawnerFor(&spawnFakeTx{}, stubResolver{}, ad)
	s.emit(context.Background(), &spawnedRecord{
		cadenceID:    "C1",
		voyageID:     "V1",
		scheduledFor: time.Now(),
		scopeSize:    3,
	})
	if len(ad.events) != 1 {
		t.Fatalf("events = %d, want 1", len(ad.events))
	}
	ev := ad.events[0]
	if ev.EventType != audit.EventCadenceSpawned {
		t.Errorf("event_type = %q, want cadence.spawned", ev.EventType)
	}
	// source stays background after the Reaper→Conductor move (ADR-048 §4: a
	// new `scheduler` source is NOT introduced).
	if ev.Source != audit.SourceBackground {
		t.Errorf("source = %q, want background", ev.Source)
	}
	// archon_aid for a background spawn = NULL (no initiating operator).
	// Recipe authorship lives in Voyage.started_by_aid, not here.
	if ev.ArchonAID != "" {
		t.Errorf("archon_aid = %q, want \"\" (NULL для background)", ev.ArchonAID)
	}
	if ev.CorrelationID != "V1" {
		t.Errorf("correlation_id = %q, want voyage_id", ev.CorrelationID)
	}
}

func TestSpawner_Emit_SkippedEvent(t *testing.T) {
	ad := &spawnAudit{}
	s := newSpawnerFor(&spawnFakeTx{}, stubResolver{}, ad)
	s.emit(context.Background(), &spawnedRecord{
		cadenceID:    "C1",
		scheduledFor: time.Now(),
		skipped:      true,
	})
	if len(ad.events) != 1 || ad.events[0].EventType != audit.EventCadenceSkippedOverlap {
		t.Fatalf("ожидался cadence.skipped_overlap, got %+v", ad.events)
	}
	if ad.events[0].CorrelationID != "C1" {
		t.Errorf("skip correlation_id = %q, want cadence_id", ad.events[0].CorrelationID)
	}
	if ad.events[0].Source != audit.SourceBackground {
		t.Errorf("skip source = %q, want background", ad.events[0].Source)
	}
	if ad.events[0].ArchonAID != "" {
		t.Errorf("skip archon_aid = %q, want \"\" (NULL для background)", ad.events[0].ArchonAID)
	}
}

// --- Run: nil resolvers / nil pool → error (fail-closed) ---

func TestSpawner_Run_NilResolvers(t *testing.T) {
	s := newCadenceSpawnerFromBeginner(spawnOneBeginner{tx: &spawnFakeTx{}}, nil, nil, nil, silentLogger())
	if _, err := s.Run(context.Background(), 0, 10); err == nil {
		t.Fatal("ожидалась ошибка при nil-резолверах")
	}
}

func TestSpawner_Run_NilPool(t *testing.T) {
	s := &CadenceSpawner{scenarioR: stubResolver{}, commandR: stubResolver{}}
	if _, err := s.Run(context.Background(), 0, 10); err == nil {
		t.Fatal("ожидалась ошибка при nil pool")
	}
}

// --- Run: ошибка в processOne → откат всей tx, next_run не сдвинут ---

// TestSpawner_Run_ProcessError_RollsBack доказывает атомарность тика (и switchover-
// безопасность ADR-048: после переезда исполнителя гарантия «нет двойного спавна»
// сохранена дословно). Одна due-cadence, resolver падает внутри processOne → Run
// возвращает ошибку БЕЗ commit-а, defer вызывает Rollback. affected==0 (ничего не
// создано). Раз tx откачена, AdvanceSchedule не зафиксирован → на следующем тике
// строка снова due → задвоения нет. То же страхует транзиент C3→C4: если бы оба
// исполнителя (reaper+conductor) на миг тикали, FOR UPDATE SKIP LOCKED отдал бы
// строку лишь одному, а advance next_run_at в той же tx убрал бы её из due.
func TestSpawner_Run_ProcessError_RollsBack(t *testing.T) {
	want := errors.New("pg down")
	tx := &spawnFakeTx{
		dueRows: []*cadence.Cadence{intervalScenarioCadence(cadence.OverlapPolicyParallel)},
	}
	s := newCadenceSpawnerFromBeginner(
		spawnOneBeginner{tx: tx},
		stubResolver{err: want}, stubResolver{err: want},
		nil, silentLogger(),
	)

	affected, err := s.Run(context.Background(), 0, 10)
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want wrap of %v", err, want)
	}
	if affected != 0 {
		t.Errorf("affected = %d, want 0 (ничего не создано при откате)", affected)
	}
	if tx.committed {
		t.Error("tx.committed = true, want false (commit не должен был случиться)")
	}
	if !tx.rolled {
		t.Error("tx.rolled = false, want true (defer обязан откатить)")
	}
	if tx.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0 (resolver упал до вставки voyage)", tx.insertCalls)
	}
	if tx.execCalls != 0 {
		t.Errorf("execCalls = %d, want 0 (AdvanceSchedule не вызван → next_run не сдвинут)", tx.execCalls)
	}
}

// TestSpawner_Run_ExecError_RollsBack — вариант с execErr!=nil: резолв успешен,
// но AdvanceSchedule (Exec) падает после Insert voyage → весь тик откатывается
// (вставленный voyage не зафиксирован), affected==0, next_run не сдвинут.
func TestSpawner_Run_ExecError_RollsBack(t *testing.T) {
	execErr := errors.New("advance failed")
	tx := &spawnFakeTx{
		dueRows: []*cadence.Cadence{intervalScenarioCadence(cadence.OverlapPolicyParallel)},
		execErr: execErr,
	}
	s := newCadenceSpawnerFromBeginner(
		spawnOneBeginner{tx: tx},
		stubResolver{out: []string{"i1"}}, stubResolver{out: []string{"i1"}},
		nil, silentLogger(),
	)

	affected, err := s.Run(context.Background(), 0, 10)
	if !errors.Is(err, execErr) {
		t.Fatalf("err = %v, want wrap of %v", err, execErr)
	}
	if affected != 0 {
		t.Errorf("affected = %d, want 0", affected)
	}
	if tx.committed {
		t.Error("tx.committed = true, want false")
	}
	if !tx.rolled {
		t.Error("tx.rolled = false, want true (откат после фейла AdvanceSchedule)")
	}
}

// TestSpawner_Run_Success_Commits — happy-path: одна due-cadence, резолв непустой
// → Insert voyage + advance + commit, affected==1, tx зафиксирована.
func TestSpawner_Run_Success_Commits(t *testing.T) {
	tx := &spawnFakeTx{
		dueRows: []*cadence.Cadence{intervalScenarioCadence(cadence.OverlapPolicyParallel)},
	}
	s := newCadenceSpawnerFromBeginner(
		spawnOneBeginner{tx: tx},
		stubResolver{out: []string{"i1"}}, stubResolver{out: []string{"i1"}},
		nil, silentLogger(),
	)

	affected, err := s.Run(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if affected != 1 {
		t.Errorf("affected = %d, want 1", affected)
	}
	if !tx.committed {
		t.Error("tx.committed = false, want true (успешный тик должен зафиксироваться)")
	}
}
