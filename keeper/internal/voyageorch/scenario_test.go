package voyageorch

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
)

// claimContains — признак claim-SQL (FOR UPDATE SKIP LOCKED).
func claimContains(sql string) bool { return strings.Contains(sql, "FOR UPDATE SKIP LOCKED") }

// scenarioClaimRow — claim-строка kind=scenario с scenario_name и двумя
// инкарнациями в target_resolved (25 колонок в порядке scanVoyage).
func scenarioClaimRow() scanRow {
	r := claimedVoyageRow("01HVOY", string(voyage.KindScenario), string(voyage.StatusRunning))
	scn := "deploy"
	r.values[2] = &scn                // scenario_name
	r.values[5] = []byte(`["a","b"]`) // target_resolved
	return r
}

// fakeSpawner — stub [ScenarioSpawner]. Возвращает детерминированный applyID
// `ap-<incarnation>` либо ошибку из failNames. Считает вызовы и фиксирует
// max-параллелизм (для проверки concurrency-cap).
type fakeSpawner struct {
	mu          sync.Mutex
	failNames   map[string]bool
	calls       []string
	active      int
	maxParallel int
	delay       time.Duration
}

func (s *fakeSpawner) SpawnScenarioRun(ctx context.Context, voyageID, name, scenario string, input []byte, aid string, cadenceID *string) (string, error) {
	s.mu.Lock()
	s.calls = append(s.calls, name)
	s.active++
	if s.active > s.maxParallel {
		s.maxParallel = s.active
	}
	fail := s.failNames[name]
	s.mu.Unlock()

	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
		}
	}

	s.mu.Lock()
	s.active--
	s.mu.Unlock()

	if fail {
		return "", fmt.Errorf("spawn failed for %s", name)
	}
	return "ap-" + name, nil
}

// fakeAwaiter — stub [IncarnationAwaiter]. По applyID → outcome из map (default
// succeeded). awaitErr — глобальная ошибка ожидания (ctx-fail симуляция).
type fakeAwaiter struct {
	outcomes map[string]TargetOutcome
	awaitErr error
}

func (a *fakeAwaiter) Await(ctx context.Context, applyID string) (TargetOutcome, error) {
	if a.awaitErr != nil {
		return "", a.awaitErr
	}
	if o, ok := a.outcomes[applyID]; ok {
		return o, nil
	}
	return OutcomeSucceeded, nil
}

func scenarioVoyage(names []string, batchSize, concurrency *int, onFailure *voyage.OnFailure, interval *time.Duration) *voyage.Voyage {
	raw, _ := json.Marshal(names)
	scn := "deploy"
	return &voyage.Voyage{
		VoyageID:           "v1",
		Kind:               voyage.KindScenario,
		ScenarioName:       &scn,
		Input:              []byte(`{}`),
		TargetResolved:     raw,
		BatchSize:          batchSize,
		Concurrency:        concurrency,
		OnFailure:          onFailure,
		InterBatchInterval: interval,
		TotalBatches:       1,
		StartedByAID:       "archon-alice",
	}
}

func intp(i int) *int { return &i }

// ---- parseIncarnationTargets ----

func TestParseIncarnationTargets(t *testing.T) {
	t.Parallel()
	got, err := parseIncarnationTargets(json.RawMessage(`["a","b","c"]`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("got %v", got)
	}
}

func TestParseIncarnationTargets_Errors(t *testing.T) {
	t.Parallel()
	cases := map[string]json.RawMessage{
		"empty raw":  nil,
		"empty arr":  json.RawMessage(`[]`),
		"not array":  json.RawMessage(`{"x":1}`),
		"empty name": json.RawMessage(`["a",""]`),
		"duplicate":  json.RawMessage(`["a","a"]`),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseIncarnationTargets(raw); err == nil {
				t.Errorf("want error for %s", name)
			}
		})
	}
}

// ---- chunkIncarnations ----

func TestChunkIncarnations(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		in        []string
		batchSize int
		want      [][]string
	}{
		{"empty", nil, 3, nil},
		{"zero batch = one leg", []string{"a", "b", "c"}, 0, [][]string{{"a", "b", "c"}}},
		{"negative batch = one leg", []string{"a", "b"}, -1, [][]string{{"a", "b"}}},
		{"exact", []string{"a", "b", "c", "d"}, 2, [][]string{{"a", "b"}, {"c", "d"}}},
		{"remainder", []string{"a", "b", "c"}, 2, [][]string{{"a", "b"}, {"c"}}},
		{"batch>len", []string{"a"}, 5, [][]string{{"a"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := chunkIncarnations(tc.in, tc.batchSize)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("chunk(%v,%d) = %v, want %v", tc.in, tc.batchSize, got, tc.want)
			}
		})
	}
}

// ---- executeScenarioVoyage: один батч ----

func TestExecuteScenario_SingleBatch_AllSucceed(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{}
	aw := &fakeAwaiter{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: aw}

	v := scenarioVoyage([]string{"a", "b", "c"}, nil, nil, nil, nil)
	status, summary, _ := w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))

	if status != voyage.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", status)
	}
	if summary.Total != 3 || summary.Succeeded != 3 || summary.Failed != 0 {
		t.Errorf("summary = %+v", summary)
	}
	if len(sp.calls) != 3 {
		t.Errorf("spawn calls = %d, want 3", len(sp.calls))
	}
	for _, n := range []string{"a", "b", "c"} {
		if got := fdb.targetStatus(n); got != string(voyage.TargetStatusSucceeded) {
			t.Errorf("target %s status = %q, want succeeded", n, got)
		}
	}
}

// ---- executeScenarioVoyage: N батчей (chunking) ----

func TestExecuteScenario_MultipleBatches(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: &fakeAwaiter{}}

	// 5 инкарнаций, batch_size=2 → 3 Leg-а (2/2/1).
	v := scenarioVoyage([]string{"a", "b", "c", "d", "e"}, intp(2), nil, nil, nil)
	status, summary, _ := w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))

	if status != voyage.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", status)
	}
	if summary.Total != 5 || summary.Succeeded != 5 {
		t.Errorf("summary = %+v", summary)
	}
	if len(sp.calls) != 5 {
		t.Errorf("spawn calls = %d, want 5", len(sp.calls))
	}
}

// ---- on_failure: abort vs continue ----

func TestExecuteScenario_OnFailureAbort(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	// "b" провалится на await → abort после первого Leg-а (batch_size=2):
	// Leg0=[a,b] исполнится, провал → Leg1=[c,d] НЕ стартует.
	sp := &fakeSpawner{}
	aw := &fakeAwaiter{outcomes: map[string]TargetOutcome{"ap-b": OutcomeFailed}}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: aw}

	abort := voyage.OnFailureAbort
	v := scenarioVoyage([]string{"a", "b", "c", "d"}, intp(2), nil, &abort, nil)
	status, summary, _ := w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))

	if status != voyage.StatusPartialFailed {
		t.Errorf("status = %q, want partial_failed", status)
	}
	// Только первый Leg исполнен: a(success)+b(failed) = 2 spawn-вызова.
	if len(sp.calls) != 2 {
		t.Errorf("spawn calls = %d, want 2 (abort после Leg0)", len(sp.calls))
	}
	if summary.Succeeded != 1 || summary.Failed != 1 {
		t.Errorf("summary = %+v", summary)
	}
}

func TestExecuteScenario_OnFailureContinue(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{}
	aw := &fakeAwaiter{outcomes: map[string]TargetOutcome{"ap-b": OutcomeFailed}}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: aw}

	cont := voyage.OnFailureContinue
	v := scenarioVoyage([]string{"a", "b", "c", "d"}, intp(2), nil, &cont, nil)
	status, summary, _ := w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))

	if status != voyage.StatusPartialFailed {
		t.Errorf("status = %q, want partial_failed", status)
	}
	// continue: все 4 инкарнации обоих Leg-ов исполнены, несмотря на провал b.
	if len(sp.calls) != 4 {
		t.Errorf("spawn calls = %d, want 4 (continue до конца)", len(sp.calls))
	}
	if summary.Succeeded != 3 || summary.Failed != 1 {
		t.Errorf("summary = %+v", summary)
	}
}

// ---- все провалились → failed ----

func TestExecuteScenario_AllFailed(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{}
	aw := &fakeAwaiter{outcomes: map[string]TargetOutcome{"ap-a": OutcomeFailed, "ap-b": OutcomeFailed}}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: aw}

	cont := voyage.OnFailureContinue
	v := scenarioVoyage([]string{"a", "b"}, nil, nil, &cont, nil)
	status, summary, _ := w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))

	if status != voyage.StatusFailed {
		t.Errorf("status = %q, want failed", status)
	}
	if summary.Failed != 2 || summary.Succeeded != 0 {
		t.Errorf("summary = %+v", summary)
	}
}

// ---- spawn-фейл (инкарнация не запустилась) учитывается как failed ----

func TestExecuteScenario_SpawnError(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{failNames: map[string]bool{"b": true}}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: &fakeAwaiter{}}

	cont := voyage.OnFailureContinue
	v := scenarioVoyage([]string{"a", "b"}, nil, nil, &cont, nil)
	status, summary, _ := w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))

	if status != voyage.StatusPartialFailed {
		t.Errorf("status = %q, want partial_failed", status)
	}
	if summary.Succeeded != 1 || summary.Failed != 1 {
		t.Errorf("summary = %+v", summary)
	}
	if got := fdb.targetStatus("b"); got != string(voyage.TargetStatusFailed) {
		t.Errorf("target b status = %q, want failed", got)
	}
}

// ---- no_match benign (не провал) ----

func TestExecuteScenario_NoMatchBenign(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{}
	aw := &fakeAwaiter{outcomes: map[string]TargetOutcome{"ap-b": OutcomeNoMatch}}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: aw}

	v := scenarioVoyage([]string{"a", "b"}, nil, nil, nil, nil)
	status, summary, _ := w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))

	if status != voyage.StatusSucceeded {
		t.Errorf("status = %q, want succeeded (no_match benign)", status)
	}
	if summary.NoMatch != 1 || summary.Succeeded != 2 {
		t.Errorf("summary = %+v", summary)
	}
}

// ---- inter_batch_interval: пауза между Leg-ами ----

func TestExecuteScenario_InterBatchInterval(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: &fakeAwaiter{}}

	interval := 60 * time.Millisecond
	// 3 инкарнации, batch_size=1 → 3 Leg-а → 2 паузы между ними ≈ 120ms.
	v := scenarioVoyage([]string{"a", "b", "c"}, intp(1), nil, nil, &interval)

	start := time.Now()
	status, _, _ := w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))
	elapsed := time.Since(start)

	if status != voyage.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", status)
	}
	if elapsed < 2*interval {
		t.Errorf("elapsed %v < 2*interval %v — пауза между Leg-ами не соблюдена", elapsed, 2*interval)
	}
}

func TestExecuteScenario_InterBatchInterval_AbortedByLeaseLost(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: &fakeAwaiter{}}

	interval := time.Hour // длинная пауза, прервём через leaseLost
	leaseLost := make(chan struct{})
	v := scenarioVoyage([]string{"a", "b"}, intp(1), nil, nil, &interval)

	done := make(chan voyage.Status, 1)
	go func() {
		st, _, _ := w.executeScenarioVoyage(context.Background(), v, leaseLost)
		done <- st
	}()

	// Дожидаемся первого Leg-а (spawn "a"), затем теряем lease на паузе.
	deadline := time.After(2 * time.Second)
	for {
		sp.mu.Lock()
		n := len(sp.calls)
		sp.mu.Unlock()
		if n >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("первый Leg не стартовал")
		case <-time.After(2 * time.Millisecond):
		}
	}
	close(leaseLost)

	select {
	case st := <-done:
		if st != "" {
			t.Errorf("status = %q, want \"\" (lease lost — не финализируем)", st)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("executeScenarioVoyage не вышел после leaseLost на паузе")
	}
}

// ---- lease lost ПОСРЕДИ серийного Leg-а: оставшиеся не спавнятся ----

func TestExecuteScenario_LeaseLostMidLeg_StopsSpawn(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	// Длинный spawn (delay) — окно, чтобы потерять lease, пока первая инкарнация
	// ещё «в работе», до раздачи оставшихся.
	sp := &fakeSpawner{delay: time.Hour}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: &fakeAwaiter{}}

	// batch_size=NULL → один Leg = все 4; concurrency=1 → строго серийный Leg.
	names := []string{"a", "b", "c", "d"}
	v := scenarioVoyage(names, nil, intp(1), nil, nil)

	leaseLost := make(chan struct{})
	done := make(chan voyage.Status, 1)
	go func() {
		st, _, _ := w.executeScenarioVoyage(context.Background(), v, leaseLost)
		done <- st
	}()

	// Ждём, пока стартует первая инкарнация (висит в delay), затем теряем lease.
	deadline := time.After(2 * time.Second)
	for {
		sp.mu.Lock()
		n := len(sp.calls)
		sp.mu.Unlock()
		if n >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("первая инкарнация Leg-а не стартовала")
		case <-time.After(2 * time.Millisecond):
		}
	}
	close(leaseLost)

	select {
	case st := <-done:
		// Не финализируем при потере lease (Reaper-reclaim).
		if st != "" {
			t.Errorf("status = %q, want \"\" (lease lost посреди Leg-а — не финализируем)", st)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("executeScenarioVoyage не вышел после leaseLost посреди Leg-а")
	}

	// Оставшиеся инкарнации (b/c/d) НЕ спавнились: spawn остановлен на acquire.
	sp.mu.Lock()
	calls := append([]string(nil), sp.calls...)
	sp.mu.Unlock()
	if len(calls) >= len(names) {
		t.Errorf("spawn calls = %v (%d), want < %d (оставшиеся не спавнятся)", calls, len(calls), len(names))
	}
	for _, c := range calls {
		if c != "a" {
			t.Errorf("заспавнена лишняя инкарнация %q после потери lease (calls=%v)", c, calls)
		}
	}
	// Финализация Voyage не вызывалась.
	if got := fdb.finalCallCount.Load(); got != 0 {
		t.Errorf("Finalize вызван %d раз, want 0 (lease lost — Reaper-reclaim)", got)
	}
}

// ---- concurrency-cap внутри Leg-а ----

func TestExecuteScenario_ConcurrencyCap(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{delay: 20 * time.Millisecond}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: &fakeAwaiter{}}

	// 6 инкарнаций одним Leg-ом, concurrency=2 → maxParallel должен быть ≤ 2.
	v := scenarioVoyage([]string{"a", "b", "c", "d", "e", "f"}, nil, intp(2), nil, nil)
	status, _, _ := w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))

	if status != voyage.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", status)
	}
	sp.mu.Lock()
	mp := sp.maxParallel
	sp.mu.Unlock()
	if mp > 2 {
		t.Errorf("maxParallel = %d, want <= 2 (concurrency-cap)", mp)
	}
	if mp < 2 {
		t.Errorf("maxParallel = %d — параллелизм не достигнут (ожидали 2)", mp)
	}
}

// ---- nil Spawner/Awaiter → fail-closed ----

func TestExecuteScenario_NilDeps_FailClosed(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger()} // нет Spawner/Awaiter
	v := scenarioVoyage([]string{"a"}, nil, nil, nil, nil)
	v.TotalBatches = 1
	status, summary, _ := w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))
	if status != voyage.StatusFailed {
		t.Errorf("status = %q, want failed (fail-closed)", status)
	}
	if summary == nil {
		t.Fatal("summary nil")
	}
}

// ---- await ctx-fail → cancelled (не молча success) ----

func TestExecuteScenario_AwaitCtxFail_Cancelled(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{}
	aw := &fakeAwaiter{awaitErr: context.Canceled}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: aw}

	cont := voyage.OnFailureContinue
	v := scenarioVoyage([]string{"a"}, nil, nil, &cont, nil)
	status, summary, _ := w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))

	if summary.Cancelled != 1 {
		t.Errorf("summary = %+v, want Cancelled=1", summary)
	}
	// cancelled-only → failed (был обрыв до единого успеха).
	if status != voyage.StatusFailed {
		t.Errorf("status = %q, want failed", status)
	}
}

// ---- worker dispatch by kind: scenario через executeVoyage ----

func TestWorker_DispatchScenarioKind(t *testing.T) {
	t.Parallel()
	var claimedOnce atomic.Bool
	fdb := &fakeDB{}
	fdb.queryRowFn = func(sql string, _ []any) pgx.Row {
		if claimContains(sql) && claimedOnce.CompareAndSwap(false, true) {
			return scenarioClaimRow()
		}
		return errRow{err: pgx.ErrNoRows}
	}
	sp := &fakeSpawner{}
	w := &VoyageWorker{
		KID: "k", Pool: fdb, LeaseTTL: time.Minute, RenewInterval: time.Hour,
		PollInterval: 5 * time.Millisecond, Logger: quietLogger(),
		ScenarioSpawner: sp, ScenarioAwaiter: &fakeAwaiter{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	deadline := time.After(2 * time.Second)
	for fdb.finalCallCount.Load() == 0 {
		select {
		case <-deadline:
			cancel()
			t.Fatal("Finalize не вызван")
		case <-time.After(2 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, _ := fdb.finalStatusArg.Load().(string); got != string(voyage.StatusSucceeded) {
		t.Errorf("finalize status = %q, want succeeded", got)
	}
	sp.mu.Lock()
	calls := len(sp.calls)
	sp.mu.Unlock()
	if calls != 2 {
		t.Errorf("spawn calls = %d, want 2 (две инкарнации в target_resolved)", calls)
	}
}

// ---- batch_mode=window для kind=scenario (S-W2): единица окна = инкарнация ----

// windowScenarioVoyage — scenario-Voyage в режиме скользящего окна. batch_size не
// используется (ширина окна = concurrency, ADR-043 amendment §1).
func windowScenarioVoyage(names []string, concurrency *int, onFailure *voyage.OnFailure) *voyage.Voyage {
	v := scenarioVoyage(names, nil, concurrency, onFailure, nil)
	wm := voyage.BatchModeWindow
	v.BatchMode = &wm
	return v
}

// concurrency-cap: окно держит ≤ concurrency активных scenario-run-ов
// (maxParallel == concurrency при len > concurrency).
func TestExecuteScenario_Window_ConcurrencyCap(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{delay: 20 * time.Millisecond}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: &fakeAwaiter{}}

	v := windowScenarioVoyage([]string{"a", "b", "c", "d", "e", "f"}, intp(2), nil)
	status, summary, _ := w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))

	if status != voyage.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", status)
	}
	if summary.Total != 6 || summary.Succeeded != 6 {
		t.Errorf("summary = %+v, want total=6 succeeded=6", summary)
	}
	sp.mu.Lock()
	mp := sp.maxParallel
	sp.mu.Unlock()
	if mp > 2 {
		t.Errorf("maxParallel = %d, want <= 2 (concurrency-cap окна)", mp)
	}
	if mp < 2 {
		t.Errorf("maxParallel = %d — параллелизм не достигнут (ожидали 2)", mp)
	}
}

// concurrency ≥ числа инкарнаций → все стартуют одновременно (очередь не
// блокирует, maxParallel == len). Parity command Window_ConcurrencyGEQLen.
func TestExecuteScenario_Window_ConcurrencyGEQLen_AllParallel(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{delay: 20 * time.Millisecond}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: &fakeAwaiter{}}

	names := []string{"a", "b", "c"}
	v := windowScenarioVoyage(names, intp(5), nil)
	status, summary, _ := w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))

	if status != voyage.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", status)
	}
	if summary.Total != 3 || summary.Succeeded != 3 {
		t.Errorf("summary = %+v, want total=3 succeeded=3", summary)
	}
	sp.mu.Lock()
	mp := sp.maxParallel
	calls := len(sp.calls)
	sp.mu.Unlock()
	if mp != len(names) {
		t.Errorf("maxParallel = %d, want %d (concurrency >= len → все сразу, очередь не блокирует)", mp, len(names))
	}
	if calls != len(names) {
		t.Errorf("spawn calls = %d, want %d", calls, len(names))
	}
}

// 1 инкарнация в окне → один воркер, один SpawnScenarioRun, succeeded.
// Parity command Window_SingleSID.
func TestExecuteScenario_Window_SingleIncarnation(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: &fakeAwaiter{}}

	v := windowScenarioVoyage([]string{"only"}, intp(4), nil)
	status, summary, _ := w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))

	if status != voyage.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", status)
	}
	if summary.Total != 1 || summary.Succeeded != 1 {
		t.Errorf("summary = %+v, want total=1 succeeded=1", summary)
	}
	sp.mu.Lock()
	calls := append([]string(nil), sp.calls...)
	mp := sp.maxParallel
	sp.mu.Unlock()
	if !reflect.DeepEqual(calls, []string{"only"}) {
		t.Errorf("spawn calls = %v, want [only] (один SpawnScenarioRun)", calls)
	}
	if mp != 1 {
		t.Errorf("maxParallel = %d, want 1 (одна инкарнация — один воркер активен)", mp)
	}
}

// очередь вырабатывается: concurrency=1 → строго серийный порядок [a,b,c].
func TestExecuteScenario_Window_DrainsQueueSerial(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: &fakeAwaiter{}}

	v := windowScenarioVoyage([]string{"a", "b", "c"}, intp(1), nil)
	status, summary, _ := w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))

	if status != voyage.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", status)
	}
	if summary.Succeeded != 3 {
		t.Errorf("summary = %+v, want succeeded=3", summary)
	}
	sp.mu.Lock()
	calls := append([]string(nil), sp.calls...)
	sp.mu.Unlock()
	if !reflect.DeepEqual(calls, []string{"a", "b", "c"}) {
		t.Errorf("spawn order = %v, want [a b c] (concurrency=1, FIFO-очередь)", calls)
	}
}

// abort + fail_threshold (через on_failure=abort ≡ threshold 1): первый провал
// прекращает спавн новых, недоспавненные → cancelled (parity barrier).
func TestExecuteScenario_Window_AbortStopsSpawn(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{}
	aw := &fakeAwaiter{outcomes: map[string]TargetOutcome{"ap-b": OutcomeFailed}}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: aw}

	abort := voyage.OnFailureAbort
	// concurrency=1 строго серийно: a(ok) → b(fail→abort) → стоп. c/d/e не спавнятся.
	v := windowScenarioVoyage([]string{"a", "b", "c", "d", "e"}, intp(1), &abort)
	status, summary, _ := w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))

	if status != voyage.StatusPartialFailed {
		t.Errorf("status = %q, want partial_failed", status)
	}
	sp.mu.Lock()
	calls := append([]string(nil), sp.calls...)
	sp.mu.Unlock()
	if len(calls) != 2 {
		t.Errorf("spawn calls = %v (%d), want 2 (abort прекратил спавн после провала)", calls, len(calls))
	}
	if summary.Succeeded != 1 || summary.Failed != 1 {
		t.Errorf("summary = %+v, want succeeded=1 failed=1", summary)
	}
	if summary.Total != 5 {
		t.Errorf("summary.Total = %d, want 5 (полный scope)", summary.Total)
	}
	if summary.Cancelled != 3 {
		t.Errorf("summary.Cancelled = %d, want 3 (c/d/e недоспавнены → cancelled)", summary.Cancelled)
	}
	if summary.Total != summary.Succeeded+summary.Failed+summary.Cancelled {
		t.Errorf("баланс summary не закрыт: %+v", summary)
	}
	for _, n := range []string{"c", "d", "e"} {
		if got := fdb.targetStatus(n); got != string(voyage.TargetStatusCancelled) {
			t.Errorf("target %s status = %q, want cancelled (недоспавнен при abort)", n, got)
		}
	}
}

// fail_threshold=2: окно терпит один провал, прекращает спавн на втором.
func TestExecuteScenario_Window_FailThreshold(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{}
	aw := &fakeAwaiter{outcomes: map[string]TargetOutcome{"ap-a": OutcomeFailed, "ap-b": OutcomeFailed}}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: aw}

	v := windowScenarioVoyage([]string{"a", "b", "c", "d"}, intp(1), nil)
	thr := 2
	v.FailThreshold = &thr // явный порог: стоп на 2-м провале
	status, summary, _ := w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))

	if status != voyage.StatusFailed {
		t.Errorf("status = %q, want failed", status)
	}
	sp.mu.Lock()
	calls := len(sp.calls)
	sp.mu.Unlock()
	// a(fail) → b(fail, порог достигнут → стоп). c/d не спавнятся.
	if calls != 2 {
		t.Errorf("spawn calls = %d, want 2 (стоп на 2-м провале)", calls)
	}
	if summary.Failed != 2 || summary.Cancelled != 2 {
		t.Errorf("summary = %+v, want failed=2 cancelled=2", summary)
	}
}

// continue: окно вырабатывает очередь до конца, несмотря на провал.
func TestExecuteScenario_Window_ContinueDrainsQueue(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{}
	aw := &fakeAwaiter{outcomes: map[string]TargetOutcome{"ap-b": OutcomeFailed}}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: aw}

	cont := voyage.OnFailureContinue
	v := windowScenarioVoyage([]string{"a", "b", "c", "d"}, intp(1), &cont)
	status, summary, _ := w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))

	if status != voyage.StatusPartialFailed {
		t.Errorf("status = %q, want partial_failed", status)
	}
	if len(sp.calls) != 4 {
		t.Errorf("spawn calls = %d, want 4 (continue выработал всю очередь)", len(sp.calls))
	}
	if summary.Succeeded != 3 || summary.Failed != 1 {
		t.Errorf("summary = %+v, want succeeded=3 failed=1", summary)
	}
}

// VerifyOwnership вызывается per-unit (по одной проверке на инкарнацию).
func TestExecuteScenario_Window_FencingPerUnit(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: &fakeAwaiter{}}

	names := []string{"a", "b", "c", "d"}
	v := windowScenarioVoyage(names, intp(2), nil)
	v.Attempt = 3
	status, _, _ := w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))

	if status != voyage.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", status)
	}
	if got := fdb.verifyCalls.Load(); got != int64(len(names)) {
		t.Errorf("VerifyOwnership calls = %d, want %d (per-unit fencing)", got, len(names))
	}
}

// lease lost посреди окна (renewLoop-канал) → не финализируем (Reaper-reclaim).
func TestExecuteScenario_Window_LeaseLostMidRun_NoFinalize(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{delay: time.Hour} // держим воркеры активными
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: &fakeAwaiter{}}

	v := windowScenarioVoyage([]string{"a", "b", "c", "d"}, intp(2), nil)
	leaseLost := make(chan struct{})
	done := make(chan voyage.Status, 1)
	go func() {
		st, _, _ := w.executeScenarioVoyage(context.Background(), v, leaseLost)
		done <- st
	}()

	deadline := time.After(2 * time.Second)
	for {
		sp.mu.Lock()
		n := len(sp.calls)
		sp.mu.Unlock()
		if n >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("окно не стартовало")
		case <-time.After(2 * time.Millisecond):
		}
	}
	close(leaseLost)

	select {
	case st := <-done:
		if st != "" {
			t.Errorf("status = %q, want \"\" (lease lost посреди окна — не финализируем)", st)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runScenarioSlidingWindow не вышел после leaseLost")
	}
	if got := fdb.finalCallCount.Load(); got != 0 {
		t.Errorf("Finalize вызван %d раз, want 0 (lease lost — Reaper-reclaim)", got)
	}
}

// реклейм посреди окна (fencing-CAS VerifyOwnership) → не финализируем, ни один
// scenario-run не заспавнен.
func TestExecuteScenario_Window_FenceLostMidRun_NoFinalize(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	fdb.verifyLeaseLost.Store(true) // VerifyOwnership заваливает все единицы
	sp := &fakeSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: &fakeAwaiter{}}

	v := windowScenarioVoyage([]string{"a", "b"}, intp(1), nil)
	v.Attempt = 5
	status, summary, _ := w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))

	if status != "" {
		t.Errorf("status = %q, want \"\" (fence lost — не финализируем)", status)
	}
	if summary != nil {
		t.Errorf("summary = %+v, want nil", summary)
	}
	if len(sp.calls) != 0 {
		t.Errorf("spawn calls = %d, want 0 (fencing до spawn)", len(sp.calls))
	}
}

// inter_unit_interval: per-unit пауза в окне перед спавном каждой инкарнации.
func TestExecuteScenario_Window_InterUnitInterval(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: &fakeAwaiter{}}

	v := windowScenarioVoyage([]string{"a", "b", "c"}, intp(1), nil)
	iu := 30 * time.Millisecond
	v.InterUnitInterval = &iu

	start := time.Now()
	status, summary, _ := w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))
	elapsed := time.Since(start)

	if status != voyage.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", status)
	}
	if summary.Succeeded != 3 {
		t.Errorf("summary = %+v, want Succeeded=3", summary)
	}
	if elapsed < 80*time.Millisecond {
		t.Errorf("elapsed = %v, want ≥ ~90ms (3×30ms inter_unit throttle)", elapsed)
	}
}

// отмена ВО ВРЕМЯ inter_unit-паузы: снятая с очереди инкарнация cancelled,
// SpawnScenarioRun не вызывается.
func TestExecuteScenario_Window_InterUnitInterval_CancelledDuringPause(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: &fakeAwaiter{}}

	v := windowScenarioVoyage([]string{"a", "b", "c"}, intp(1), nil)
	iu := time.Hour // длинная пауза — единица застревает в throttle
	v.InterUnitInterval = &iu

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *voyage.Summary, 1)
	go func() {
		_, summary, _ := w.executeScenarioVoyage(ctx, v, make(chan struct{}))
		done <- summary
	}()

	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case <-done:
		sp.mu.Lock()
		calls := append([]string(nil), sp.calls...)
		sp.mu.Unlock()
		if len(calls) != 0 {
			t.Errorf("spawn calls = %v, want 0 (отмена в паузе — SpawnScenarioRun не вызван)", calls)
		}
		if got := fdb.targetStatus("a"); got != string(voyage.TargetStatusCancelled) {
			t.Errorf("target a status = %q, want cancelled (снята с очереди, ждала throttle)", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("executeScenarioVoyage не вышел после отмены в паузе")
	}
}

// batch_index=0 у всех инкарнаций window-окна. В voyageorch batch_index пишет
// S5-Insert (handler-тест TestVoyageCreate_ScenarioWindowOK покрывает это);
// здесь проверяем, что window-исполнитель не порождает Leg-нумерацию — все
// инкарнации проходят одним плоским прогоном (один scope, нет Leg-границ).
func TestExecuteScenario_Window_FlatNoLegs(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: &fakeAwaiter{}}

	// 5 инкарнаций, concurrency=2: barrier нарезал бы Leg-и, окно — нет.
	v := windowScenarioVoyage([]string{"a", "b", "c", "d", "e"}, intp(2), nil)
	status, summary, _ := w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))

	if status != voyage.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", status)
	}
	if summary.Total != 5 || summary.Succeeded != 5 {
		t.Errorf("summary = %+v, want total=5 succeeded=5", summary)
	}
	if len(sp.calls) != 5 {
		t.Errorf("spawn calls = %d, want 5 (все инкарнации одним плоским прогоном)", len(sp.calls))
	}
}
