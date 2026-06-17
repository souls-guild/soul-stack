package voyageorch

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
)

// commandClaimRow — claim-строка kind=command с module и двумя хостами в
// target_resolved (25 колонок в порядке scanVoyage).
func commandClaimRow() scanRow {
	r := claimedVoyageRow("01HVOYCMD", string(voyage.KindCommand), string(voyage.StatusRunning))
	mod := "core.cmd.shell"
	r.values[3] = &mod                                  // module
	r.values[5] = []byte(`["s1.example","s2.example"]`) // target_resolved
	return r
}

// fakeCommandSpawner — stub [CommandSpawner]. Возвращает детерминированный
// errandID `er-<sid>` и статус из statuses (default "success") либо ошибку из
// failSIDs. Считает вызовы и фиксирует max-параллелизм (для concurrency-cap).
type fakeCommandSpawner struct {
	mu          sync.Mutex
	failSIDs    map[string]bool
	statuses    map[string]string // sid → errand-status (default "success")
	calls       []string
	active      int
	maxParallel int
	delay       time.Duration
}

func (s *fakeCommandSpawner) SpawnCommand(ctx context.Context, voyageID, sid, module, aid string, input []byte) (string, string, error) {
	s.mu.Lock()
	s.calls = append(s.calls, sid)
	s.active++
	if s.active > s.maxParallel {
		s.maxParallel = s.active
	}
	fail := s.failSIDs[sid]
	status, hasStatus := s.statuses[sid]
	s.mu.Unlock()

	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			s.mu.Lock()
			s.active--
			s.mu.Unlock()
			return "", "", ctx.Err()
		}
	}

	s.mu.Lock()
	s.active--
	s.mu.Unlock()

	if fail {
		return "", "", fmt.Errorf("spawn failed for %s", sid)
	}
	if !hasStatus {
		status = "success"
	}
	return "er-" + sid, status, nil
}

func commandVoyage(sids []string, batchSize, concurrency *int, onFailure *voyage.OnFailure, interval *time.Duration) *voyage.Voyage {
	raw, _ := json.Marshal(sids)
	mod := "core.cmd.shell"
	return &voyage.Voyage{
		VoyageID:           "v1",
		Kind:               voyage.KindCommand,
		Module:             &mod,
		Input:              []byte(`{"cmd":"uptime"}`),
		TargetResolved:     raw,
		BatchSize:          batchSize,
		Concurrency:        concurrency,
		OnFailure:          onFailure,
		InterBatchInterval: interval,
		TotalBatches:       1,
		StartedByAID:       "archon-alice",
	}
}

// windowCommandVoyage — kind=command в batch_mode=window (S-W1). batch_size не
// используется (ширина окна = concurrency).
func windowCommandVoyage(sids []string, concurrency *int, onFailure *voyage.OnFailure) *voyage.Voyage {
	v := commandVoyage(sids, nil, concurrency, onFailure, nil)
	wm := voyage.BatchModeWindow
	v.BatchMode = &wm
	return v
}

// ---- parseSIDTargets ----

func TestParseSIDTargets(t *testing.T) {
	t.Parallel()
	got, err := parseSIDTargets(json.RawMessage(`["s1","s2","s3"]`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"s1", "s2", "s3"}) {
		t.Errorf("got %v", got)
	}
}

func TestParseSIDTargets_Errors(t *testing.T) {
	t.Parallel()
	cases := map[string]json.RawMessage{
		"empty raw": nil,
		"empty arr": json.RawMessage(`[]`),
		"not array": json.RawMessage(`{"x":1}`),
		"empty sid": json.RawMessage(`["s1",""]`),
		"duplicate": json.RawMessage(`["s1","s1"]`),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseSIDTargets(raw); err == nil {
				t.Errorf("want error for %s", name)
			}
		})
	}
}

// ---- chunkSIDs ----

func TestChunkSIDs(t *testing.T) {
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
			got := chunkSIDs(tc.in, tc.batchSize)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("chunk(%v,%d) = %v, want %v", tc.in, tc.batchSize, got, tc.want)
			}
		})
	}
}

// ---- executeCommandVoyage: один батч, все хосты success ----

func TestExecuteCommand_SingleBatch_AllSucceed(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeCommandSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	v := commandVoyage([]string{"s1", "s2", "s3"}, nil, nil, nil, nil)
	status, summary, _ := w.executeCommandVoyage(context.Background(), v, make(chan struct{}))

	if status != voyage.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", status)
	}
	if summary.Total != 3 || summary.Succeeded != 3 || summary.Failed != 0 {
		t.Errorf("summary = %+v", summary)
	}
	if len(sp.calls) != 3 {
		t.Errorf("spawn calls = %d, want 3", len(sp.calls))
	}
	// voyage_targets sid-трекинг: каждый хост дошёл до succeeded.
	for _, s := range []string{"s1", "s2", "s3"} {
		if got := fdb.targetStatus(s); got != string(voyage.TargetStatusSucceeded) {
			t.Errorf("target %s status = %q, want succeeded", s, got)
		}
	}
}

// ---- chunking по SID: N батчей ----

func TestExecuteCommand_MultipleBatches(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeCommandSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	// 5 хостов, batch_size=2 → 3 Leg-а (2/2/1).
	v := commandVoyage([]string{"a", "b", "c", "d", "e"}, intp(2), nil, nil, nil)
	status, summary, _ := w.executeCommandVoyage(context.Background(), v, make(chan struct{}))

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

func TestExecuteCommand_OnFailureAbort(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	// "b" вернёт failed-статус → abort после первого Leg-а (batch_size=2):
	// Leg0=[a,b] исполнится, провал → Leg1=[c,d] НЕ стартует.
	sp := &fakeCommandSpawner{statuses: map[string]string{"b": "failed"}}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	abort := voyage.OnFailureAbort
	v := commandVoyage([]string{"a", "b", "c", "d"}, intp(2), nil, &abort, nil)
	status, summary, _ := w.executeCommandVoyage(context.Background(), v, make(chan struct{}))

	if status != voyage.StatusPartialFailed {
		t.Errorf("status = %q, want partial_failed", status)
	}
	if len(sp.calls) != 2 {
		t.Errorf("spawn calls = %d, want 2 (abort после Leg0)", len(sp.calls))
	}
	if summary.Succeeded != 1 || summary.Failed != 1 {
		t.Errorf("summary = %+v", summary)
	}
}

func TestExecuteCommand_OnFailureContinue(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeCommandSpawner{statuses: map[string]string{"b": "failed"}}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	cont := voyage.OnFailureContinue
	v := commandVoyage([]string{"a", "b", "c", "d"}, intp(2), nil, &cont, nil)
	status, summary, _ := w.executeCommandVoyage(context.Background(), v, make(chan struct{}))

	if status != voyage.StatusPartialFailed {
		t.Errorf("status = %q, want partial_failed", status)
	}
	if len(sp.calls) != 4 {
		t.Errorf("spawn calls = %d, want 4 (continue до конца)", len(sp.calls))
	}
	if summary.Succeeded != 3 || summary.Failed != 1 {
		t.Errorf("summary = %+v", summary)
	}
}

// ---- fail_threshold: обобщённый abort-gate (S-W3) ----

// barrier: fail_threshold=2 — прогон идёт, пока кумулятив провалов < 2; на 2-м
// провале (после Leg-а, в котором он накопился) — стоп. 6 хостов batch_size=1 →
// Leg-и по 1; "b" и "d" падают: после Leg "d" failed=2 → стоп, "e"/"f" пропущены.
func TestExecuteCommand_FailThreshold_StopsAtN(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeCommandSpawner{statuses: map[string]string{"b": "failed", "d": "failed"}}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	v := commandVoyage([]string{"a", "b", "c", "d", "e", "f"}, intp(1), nil, nil, nil)
	thr := 2
	v.FailThreshold = &thr
	status, summary, _ := w.executeCommandVoyage(context.Background(), v, make(chan struct{}))

	if status != voyage.StatusPartialFailed {
		t.Errorf("status = %q, want partial_failed", status)
	}
	// Leg-и a,b,c,d исполнены (4 спавна), на failed=2 после "d" — стоп; e,f пропущены.
	if len(sp.calls) != 4 {
		t.Errorf("spawn calls = %d, want 4 (стоп на fail_threshold=2 после d)", len(sp.calls))
	}
	if summary.Failed != 2 {
		t.Errorf("summary.Failed = %d, want 2", summary.Failed)
	}
}

// barrier: fail_threshold=3 при 1 провале → НЕ срабатывает, прогон до конца
// (промежуточная толерантность).
func TestExecuteCommand_FailThreshold_ToleratesBelowN(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeCommandSpawner{statuses: map[string]string{"b": "failed"}}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	v := commandVoyage([]string{"a", "b", "c", "d"}, intp(1), nil, nil, nil)
	thr := 3
	v.FailThreshold = &thr
	status, _, _ := w.executeCommandVoyage(context.Background(), v, make(chan struct{}))

	if status != voyage.StatusPartialFailed {
		t.Errorf("status = %q, want partial_failed", status)
	}
	if len(sp.calls) != 4 {
		t.Errorf("spawn calls = %d, want 4 (1 провал < порога 3 → до конца)", len(sp.calls))
	}
}

// window: fail_threshold=2 — окно прекращает спавн новых при достижении 2-х
// провалов; уже-выработанные доработают, остаток очереди → cancelled. Все хосты
// failed, concurrency=1 (детерминированный порядок выборки).
func TestExecuteCommand_Window_FailThreshold(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeCommandSpawner{statuses: map[string]string{
		"a": "failed", "b": "failed", "c": "failed", "d": "failed",
	}}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	v := windowCommandVoyage([]string{"a", "b", "c", "d"}, intp(1), nil)
	thr := 2
	v.FailThreshold = &thr
	status, summary, _ := w.executeCommandVoyage(context.Background(), v, make(chan struct{}))

	if status != voyage.StatusFailed {
		t.Errorf("status = %q, want failed (все провалы, ни одного успеха)", status)
	}
	// concurrency=1: спавнятся a,b → failed=2 → cancelFan; c,d остаются в очереди.
	if len(sp.calls) != 2 {
		t.Errorf("spawn calls = %d, want 2 (стоп на fail_threshold=2)", len(sp.calls))
	}
	if summary.Failed != 2 || summary.Cancelled != 2 {
		t.Errorf("summary = %+v, want Failed=2 Cancelled=2", summary)
	}
}

// ---- inter_unit_interval: per-unit throttle в window (S-W3) ----

// window с inter_unit_interval > 0: пауза перед каждой единицей удлиняет прогон
// минимум на (N-1)×interval (грубая проверка: общее время ≥ суммы пауз).
func TestExecuteCommand_Window_InterUnitInterval(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeCommandSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	// concurrency=1, 3 хоста, пауза 30ms перед каждой единицей → ≥ 90ms суммарно.
	v := windowCommandVoyage([]string{"a", "b", "c"}, intp(1), nil)
	iu := 30 * time.Millisecond
	v.InterUnitInterval = &iu

	start := time.Now()
	status, summary, _ := w.executeCommandVoyage(context.Background(), v, make(chan struct{}))
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

// Отмена (fanCtx) ВО ВРЕМЯ inter_unit-паузы прекращает спавн: единица, снятая с
// очереди и ждущая throttle, помечается cancelled, SpawnCommand НЕ вызывается
// (qa-gap к happy-path InterUnitInterval). concurrency=1, длинная пауза → первая
// единица гарантированно стоит в throttle, когда отменяется родительский ctx.
func TestExecuteCommand_Window_InterUnitInterval_CancelledDuringPause(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeCommandSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	v := windowCommandVoyage([]string{"a", "b", "c"}, intp(1), nil)
	iu := time.Hour // длинная пауза — единица застревает в throttle, отменим в ней
	v.InterUnitInterval = &iu

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *voyage.Summary, 1)
	go func() {
		_, summary, _ := w.executeCommandVoyage(ctx, v, make(chan struct{}))
		done <- summary
	}()

	// Дать воркеру снять "a" с очереди и встать в inter_unit-паузу, затем отменить.
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case summary := <-done:
		sp.mu.Lock()
		calls := append([]string(nil), sp.calls...)
		sp.mu.Unlock()
		if len(calls) != 0 {
			t.Errorf("spawn calls = %v, want 0 (отмена в паузе — SpawnCommand не вызван)", calls)
		}
		if got := fdb.targetStatus("a"); got != string(voyage.TargetStatusCancelled) {
			t.Errorf("target a status = %q, want cancelled (снят с очереди, не стартовал)", got)
		}
		if summary == nil || summary.Cancelled == 0 {
			t.Errorf("summary = %+v, want Cancelled ≥ 1", summary)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("executeCommandVoyage не вышел после отмены ctx в inter_unit-паузе")
	}
}

// ---- timed_out / module_not_allowed считаются провалом ----

func TestExecuteCommand_NonSuccessStatusesAreFailure(t *testing.T) {
	t.Parallel()
	for _, st := range []string{"failed", "timed_out", "module_not_allowed"} {
		t.Run(st, func(t *testing.T) {
			fdb := &fakeDB{}
			sp := &fakeCommandSpawner{statuses: map[string]string{"b": st}}
			w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

			cont := voyage.OnFailureContinue
			v := commandVoyage([]string{"a", "b"}, nil, nil, &cont, nil)
			status, summary, _ := w.executeCommandVoyage(context.Background(), v, make(chan struct{}))

			if status != voyage.StatusPartialFailed {
				t.Errorf("status = %q, want partial_failed", status)
			}
			if summary.Succeeded != 1 || summary.Failed != 1 {
				t.Errorf("summary = %+v", summary)
			}
			if got := fdb.targetStatus("b"); got != string(voyage.TargetStatusFailed) {
				t.Errorf("target b status = %q, want failed", got)
			}
		})
	}
}

// ---- spawn-ошибка (Errand не запустился) учитывается как failed ----

func TestExecuteCommand_SpawnError(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeCommandSpawner{failSIDs: map[string]bool{"b": true}}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	cont := voyage.OnFailureContinue
	v := commandVoyage([]string{"a", "b"}, nil, nil, &cont, nil)
	status, summary, _ := w.executeCommandVoyage(context.Background(), v, make(chan struct{}))

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

// ---- все провалились → failed ----

func TestExecuteCommand_AllFailed(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeCommandSpawner{statuses: map[string]string{"a": "failed", "b": "failed"}}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	cont := voyage.OnFailureContinue
	v := commandVoyage([]string{"a", "b"}, nil, nil, &cont, nil)
	status, summary, _ := w.executeCommandVoyage(context.Background(), v, make(chan struct{}))

	if status != voyage.StatusFailed {
		t.Errorf("status = %q, want failed", status)
	}
	if summary.Failed != 2 || summary.Succeeded != 0 {
		t.Errorf("summary = %+v", summary)
	}
}

// ---- inter_batch_interval: пауза между Leg-ами ----

func TestExecuteCommand_InterBatchInterval(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeCommandSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	interval := 60 * time.Millisecond
	// 3 хоста, batch_size=1 → 3 Leg-а → 2 паузы между ними ≈ 120ms.
	v := commandVoyage([]string{"a", "b", "c"}, intp(1), nil, nil, &interval)

	start := time.Now()
	status, _, _ := w.executeCommandVoyage(context.Background(), v, make(chan struct{}))
	elapsed := time.Since(start)

	if status != voyage.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", status)
	}
	if elapsed < 2*interval {
		t.Errorf("elapsed %v < 2*interval %v — пауза между Leg-ами не соблюдена", elapsed, 2*interval)
	}
}

func TestExecuteCommand_InterBatchInterval_AbortedByLeaseLost(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeCommandSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	interval := time.Hour // длинная пауза, прервём через leaseLost
	leaseLost := make(chan struct{})
	v := commandVoyage([]string{"a", "b"}, intp(1), nil, nil, &interval)

	done := make(chan voyage.Status, 1)
	go func() {
		st, _, _ := w.executeCommandVoyage(context.Background(), v, leaseLost)
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
		t.Fatal("executeCommandVoyage не вышел после leaseLost на паузе")
	}
}

// ---- lease lost ПОСРЕДИ серийного Leg-а: оставшиеся не спавнятся (КАК S2) ----

func TestExecuteCommand_LeaseLostMidLeg_StopsSpawn(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	// Длинный spawn (delay) — окно, чтобы потерять lease, пока первый хост
	// ещё «в работе», до раздачи оставшихся.
	sp := &fakeCommandSpawner{delay: time.Hour}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	// batch_size=NULL → один Leg = все 4; concurrency=1 → строго серийный Leg.
	sids := []string{"a", "b", "c", "d"}
	v := commandVoyage(sids, nil, intp(1), nil, nil)

	leaseLost := make(chan struct{})
	done := make(chan voyage.Status, 1)
	go func() {
		st, _, _ := w.executeCommandVoyage(context.Background(), v, leaseLost)
		done <- st
	}()

	// Ждём, пока стартует первый хост (висит в delay), затем теряем lease.
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
			t.Fatal("первый хост Leg-а не стартовал")
		case <-time.After(2 * time.Millisecond):
		}
	}
	close(leaseLost)

	select {
	case st := <-done:
		if st != "" {
			t.Errorf("status = %q, want \"\" (lease lost посреди Leg-а — не финализируем)", st)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("executeCommandVoyage не вышел после leaseLost посреди Leg-а")
	}

	// Оставшиеся хосты (b/c/d) НЕ спавнились: spawn остановлен на acquire.
	sp.mu.Lock()
	calls := append([]string(nil), sp.calls...)
	sp.mu.Unlock()
	if len(calls) >= len(sids) {
		t.Errorf("spawn calls = %v (%d), want < %d (оставшиеся не спавнятся)", calls, len(calls), len(sids))
	}
	for _, c := range calls {
		if c != "a" {
			t.Errorf("заспавнен лишний хост %q после потери lease (calls=%v)", c, calls)
		}
	}
	// Финализация Voyage не вызывалась.
	if got := fdb.finalCallCount.Load(); got != 0 {
		t.Errorf("Finalize вызван %d раз, want 0 (lease lost — Reaper-reclaim)", got)
	}
}

// ---- concurrency-cap внутри Leg-а (fan-out) ----

func TestExecuteCommand_ConcurrencyCap(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeCommandSpawner{delay: 20 * time.Millisecond}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	// 6 хостов одним Leg-ом, concurrency=2 → maxParallel должен быть == 2.
	v := commandVoyage([]string{"a", "b", "c", "d", "e", "f"}, nil, intp(2), nil, nil)
	status, _, _ := w.executeCommandVoyage(context.Background(), v, make(chan struct{}))

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

// ---- nil CommandSpawner → fail-closed ----

func TestExecuteCommand_NilSpawner_FailClosed(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger()} // нет CommandSpawner
	v := commandVoyage([]string{"a"}, nil, nil, nil, nil)
	v.TotalBatches = 1
	status, summary, _ := w.executeCommandVoyage(context.Background(), v, make(chan struct{}))
	if status != voyage.StatusFailed {
		t.Errorf("status = %q, want failed (fail-closed)", status)
	}
	if summary == nil {
		t.Fatal("summary nil")
	}
}

// ---- nil module → fail-closed ----

func TestExecuteCommand_NilModule_FailClosed(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeCommandSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}
	v := commandVoyage([]string{"a"}, nil, nil, nil, nil)
	v.Module = nil
	status, _, _ := w.executeCommandVoyage(context.Background(), v, make(chan struct{}))
	if status != voyage.StatusFailed {
		t.Errorf("status = %q, want failed (nil module fail-closed)", status)
	}
	if len(sp.calls) != 0 {
		t.Errorf("spawn calls = %d, want 0 (module nil — спавн не стартует)", len(sp.calls))
	}
}

// ---- voyage_targets back-link: errand_id, не apply_id ----

func TestExecuteCommand_TargetBacklinkErrandID(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	fdb.recordRunningArgs = true
	sp := &fakeCommandSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	v := commandVoyage([]string{"s1"}, nil, nil, nil, nil)
	status, _, _ := w.executeCommandVoyage(context.Background(), v, make(chan struct{}))
	if status != voyage.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", status)
	}
	fdb.mu.Lock()
	gotSQL := fdb.runningSQL
	gotArg := fdb.runningBacklink
	fdb.mu.Unlock()
	if gotSQL == "" {
		t.Fatal("MarkTargetRunning не вызван")
	}
	if want := "er-s1"; gotArg != want {
		t.Errorf("back-link arg = %q, want %q (errand_id)", gotArg, want)
	}
}

// fencingSpawner — спавнер для теста реклейма посреди Leg-а (S-med-2): после
// dispatch-а первого хоста срабатывает onFirst (тест поднимает реклейм через
// fdb.verifyLeaseLost). Считает заспавненные SID-ы.
type fencingSpawner struct {
	mu      sync.Mutex
	calls   []string
	onFirst func()
}

func (s *fencingSpawner) SpawnCommand(ctx context.Context, voyageID, sid, module, aid string, input []byte) (string, string, error) {
	s.mu.Lock()
	first := len(s.calls) == 0
	s.calls = append(s.calls, sid)
	s.mu.Unlock()
	if first && s.onFirst != nil {
		s.onFirst()
	}
	return "er-" + sid, "success", nil
}

func (s *fencingSpawner) callList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// ---- реклейм посреди Leg-а: fencing перед dispatch не шлёт Errand (S-med-2) ----
//
// Сценарий: серийный Leg [a,b]. Хост "a" заспавнен (воркер ещё владелец). Перед
// dispatch-ем "b" Voyage реклеймнут другим Keeper-ом (attempt++) → VerifyOwnership
// вернёт ErrLeaseLost → "b" Errand НЕ отправляется (нет дубля), Leg прерван,
// executeCommandVoyage не финализирует.
func TestExecuteCommand_ReclaimMidLeg_FenceStopsDispatch(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fencingSpawner{}
	// После dispatch-а первого хоста имитируем реклейм: VerifyOwnership для
	// следующего хоста вернёт ErrLeaseLost.
	sp.onFirst = func() { fdb.verifyLeaseLost.Store(true) }
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	// batch_size=NULL → один Leg = [a,b]; concurrency=1 → строго серийно.
	v := commandVoyage([]string{"a", "b"}, nil, intp(1), nil, nil)
	v.Attempt = 7 // claim-epoch воркера (передаётся в VerifyOwnership/MarkTargetRunning).

	status, summary, _ := w.executeCommandVoyage(context.Background(), v, make(chan struct{}))

	// Lease потеряна посреди Leg-а → НЕ финализируем.
	if status != "" {
		t.Errorf("status = %q, want \"\" (fence lost — Reaper-reclaim, не финализируем)", status)
	}
	if summary != nil {
		t.Errorf("summary = %+v, want nil (не финализируем)", summary)
	}

	// "b" НЕ заспавнен (fencing остановил dispatch до отправки Errand-а).
	calls := sp.callList()
	if len(calls) != 1 || calls[0] != "a" {
		t.Errorf("spawn calls = %v, want [a] (b не отправлен после реклейма)", calls)
	}
	// "b" зафиксирован cancelled (fencing-путь), не succeeded/failed.
	if got := fdb.targetStatus("b"); got != string(voyage.TargetStatusCancelled) {
		t.Errorf("target b status = %q, want cancelled (fence lost)", got)
	}
}

// ---- fencing перед ПЕРВЫМ хостом: Errand не шлётся вообще ----
//
// Если lease потеряна ещё до старта первого хоста (verifyLeaseLost=true с самого
// начала), VerifyOwnership заваливает "a" → Errand не отправлен ни одному хосту.
func TestExecuteCommand_ReclaimBeforeLeg_NoDispatch(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	fdb.verifyLeaseLost.Store(true)
	sp := &fencingSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	v := commandVoyage([]string{"a", "b"}, nil, intp(1), nil, nil)
	v.Attempt = 7
	status, summary, _ := w.executeCommandVoyage(context.Background(), v, make(chan struct{}))

	if status != "" {
		t.Errorf("status = %q, want \"\" (fence lost — не финализируем)", status)
	}
	if summary != nil {
		t.Errorf("summary = %+v, want nil", summary)
	}
	if calls := sp.callList(); len(calls) != 0 {
		t.Errorf("spawn calls = %v, want [] (ни один Errand не отправлен)", calls)
	}
}

// ============================================================================
// S-W1: batch_mode=window (скользящее окно по хостам, kind=command)
// ============================================================================

// ---- window: вся очередь вырабатывается, все success ----

func TestExecuteCommand_Window_AllSucceed(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeCommandSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	sids := []string{"s1", "s2", "s3", "s4", "s5"}
	v := windowCommandVoyage(sids, intp(2), nil)
	status, summary, _ := w.executeCommandVoyage(context.Background(), v, make(chan struct{}))

	if status != voyage.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", status)
	}
	if summary.Total != 5 || summary.Succeeded != 5 || summary.Failed != 0 {
		t.Errorf("summary = %+v, want total=5 succeeded=5", summary)
	}
	// Очередь выработана полностью: каждый SID заспавнен ровно один раз.
	if len(sp.calls) != 5 {
		t.Errorf("spawn calls = %d, want 5 (очередь выработана)", len(sp.calls))
	}
	for _, s := range sids {
		if got := fdb.targetStatus(s); got != string(voyage.TargetStatusSucceeded) {
			t.Errorf("target %s status = %q, want succeeded", s, got)
		}
	}
}

// ---- window: окно держит ≤ concurrency активных, но прокачивает всю очередь ----
//
// concurrency=3, 9 хостов, каждый spawn держится delay. Если бы окно работало
// как chunk-барьер, maxParallel прыгал бы по пачкам; скользящее окно держит
// СТРОГО 3 активных, при этом single-pool без барьеров прокачивает все 9.
func TestExecuteCommand_Window_HoldsConcurrencyActive(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeCommandSpawner{delay: 20 * time.Millisecond}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	v := windowCommandVoyage([]string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}, intp(3), nil)
	status, summary, _ := w.executeCommandVoyage(context.Background(), v, make(chan struct{}))

	if status != voyage.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", status)
	}
	if summary.Succeeded != 9 {
		t.Errorf("summary = %+v, want succeeded=9", summary)
	}
	sp.mu.Lock()
	mp := sp.maxParallel
	calls := len(sp.calls)
	sp.mu.Unlock()
	if mp > 3 {
		t.Errorf("maxParallel = %d, want <= 3 (окно держит не больше concurrency)", mp)
	}
	if mp < 3 {
		t.Errorf("maxParallel = %d — окно не заполнено до concurrency (ожидали 3)", mp)
	}
	if calls != 9 {
		t.Errorf("spawn calls = %d, want 9 (вся очередь прокачана одним пулом)", calls)
	}
}

// ---- window: lease-fencing вызывается per-unit (VerifyOwnership на каждый SID) ----

func TestExecuteCommand_Window_FencingPerUnit(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeCommandSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	sids := []string{"s1", "s2", "s3", "s4"}
	v := windowCommandVoyage(sids, intp(2), nil)
	v.Attempt = 3
	status, _, _ := w.executeCommandVoyage(context.Background(), v, make(chan struct{}))

	if status != voyage.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", status)
	}
	// runOneCommand вызывает VerifyOwnership перед каждым dispatch → ровно по
	// одному на единицу окна.
	if got := fdb.verifyCalls.Load(); got != int64(len(sids)) {
		t.Errorf("VerifyOwnership calls = %d, want %d (per-unit fencing)", got, len(sids))
	}
}

// ---- window on_failure=abort: первый провал прекращает спавн новых ----
//
// concurrency=1 (строго серийное окно), 5 хостов, "b" провалится. abort должен
// прекратить выборку из очереди после провала "b" → c/d/e не спавнятся, но
// помечаются cancelled (parity barrier «оставшиеся Leg-и пропущены», qa-gap).
func TestExecuteCommand_Window_AbortStopsSpawn(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeCommandSpawner{statuses: map[string]string{"b": "failed"}}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	abort := voyage.OnFailureAbort
	v := windowCommandVoyage([]string{"a", "b", "c", "d", "e"}, intp(1), &abort)
	status, summary, _ := w.executeCommandVoyage(context.Background(), v, make(chan struct{}))

	if status != voyage.StatusPartialFailed {
		t.Errorf("status = %q, want partial_failed", status)
	}
	// concurrency=1 строго серийно: a(ok) → b(fail→abort) → стоп. c/d/e не спавнятся.
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
	// parity barrier: недоспавненные c/d/e помечены cancelled, баланс закрыт.
	if summary.Cancelled != 3 {
		t.Errorf("summary.Cancelled = %d, want 3 (c/d/e недоспавнены → cancelled)", summary.Cancelled)
	}
	if summary.Total != summary.Succeeded+summary.Failed+summary.Cancelled {
		t.Errorf("баланс summary не закрыт: %+v (Total != succeeded+failed+cancelled)", summary)
	}
	// voyage_targets: c/d/e записаны cancelled (drill UI видит их как barrier).
	for _, s := range []string{"c", "d", "e"} {
		if got := fdb.targetStatus(s); got != string(voyage.TargetStatusCancelled) {
			t.Errorf("target %s status = %q, want cancelled (недоспавнен при abort)", s, got)
		}
	}
}

// ---- window on_failure=continue: окно вырабатывает очередь до конца ----

func TestExecuteCommand_Window_ContinueDrainsQueue(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeCommandSpawner{statuses: map[string]string{"b": "failed"}}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	cont := voyage.OnFailureContinue
	v := windowCommandVoyage([]string{"a", "b", "c", "d"}, intp(1), &cont)
	status, summary, _ := w.executeCommandVoyage(context.Background(), v, make(chan struct{}))

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

// ---- window: все провалились → failed ----

func TestExecuteCommand_Window_AllFailed(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeCommandSpawner{statuses: map[string]string{"a": "failed", "b": "failed"}}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	cont := voyage.OnFailureContinue
	v := windowCommandVoyage([]string{"a", "b"}, intp(2), &cont)
	status, summary, _ := w.executeCommandVoyage(context.Background(), v, make(chan struct{}))

	if status != voyage.StatusFailed {
		t.Errorf("status = %q, want failed", status)
	}
	if summary.Failed != 2 || summary.Succeeded != 0 {
		t.Errorf("summary = %+v, want failed=2", summary)
	}
}

// ---- window: lease lost посреди окна → не финализируем (Reaper-reclaim) ----

func TestExecuteCommand_Window_LeaseLostMidRun_NoFinalize(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeCommandSpawner{delay: time.Hour} // держим воркеры активными
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	v := windowCommandVoyage([]string{"a", "b", "c", "d"}, intp(2), nil)
	leaseLost := make(chan struct{})
	done := make(chan voyage.Status, 1)
	go func() {
		st, _, _ := w.executeCommandVoyage(context.Background(), v, leaseLost)
		done <- st
	}()

	// Ждём, пока окно заполнится (хотя бы один воркер встал в delay), теряем lease.
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
		t.Fatal("runSlidingWindow не вышел после leaseLost")
	}
	if got := fdb.finalCallCount.Load(); got != 0 {
		t.Errorf("Finalize вызван %d раз, want 0 (lease lost — Reaper-reclaim)", got)
	}
}

// ---- window: concurrency >= len(sids) → окно вырождается в «все сразу» ----
//
// concurrency=5 при 3 хостах: очередь не блокирует ни одного воркера, все
// стартуют параллельно (maxParallel == len(sids)), все success (qa-gap high).
func TestExecuteCommand_Window_ConcurrencyGEQLen_AllParallel(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeCommandSpawner{delay: 20 * time.Millisecond}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	sids := []string{"a", "b", "c"}
	v := windowCommandVoyage(sids, intp(5), nil)
	status, summary, _ := w.executeCommandVoyage(context.Background(), v, make(chan struct{}))

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
	if mp != len(sids) {
		t.Errorf("maxParallel = %d, want %d (concurrency >= len → все сразу, очередь не блокирует)", mp, len(sids))
	}
	if calls != len(sids) {
		t.Errorf("spawn calls = %d, want %d", calls, len(sids))
	}
}

// ---- window concurrency=1: строго серийный ПОРЯДОК вызовов ----
//
// Один воркер тянет FIFO-очередь → SpawnCommand вызывается строго [a,b,c]
// (qa-gap high: окно при concurrency=1 = детерминированный серийный прогон).
func TestExecuteCommand_Window_Concurrency1_SerialOrder(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeCommandSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	v := windowCommandVoyage([]string{"a", "b", "c"}, intp(1), nil)
	status, summary, _ := w.executeCommandVoyage(context.Background(), v, make(chan struct{}))

	if status != voyage.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", status)
	}
	if summary.Succeeded != 3 {
		t.Errorf("summary = %+v, want succeeded=3", summary)
	}
	sp.mu.Lock()
	calls := append([]string(nil), sp.calls...)
	mp := sp.maxParallel
	sp.mu.Unlock()
	if !reflect.DeepEqual(calls, []string{"a", "b", "c"}) {
		t.Errorf("spawn order = %v, want [a b c] (concurrency=1 строго серийно)", calls)
	}
	if mp != 1 {
		t.Errorf("maxParallel = %d, want 1 (один воркер)", mp)
	}
}

// ---- window: 1 SID → один воркер, один dispatch ----
func TestExecuteCommand_Window_SingleSID(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeCommandSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	v := windowCommandVoyage([]string{"only"}, intp(4), nil)
	status, summary, _ := w.executeCommandVoyage(context.Background(), v, make(chan struct{}))

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
		t.Errorf("spawn calls = %v, want [only] (один dispatch)", calls)
	}
	if mp != 1 {
		t.Errorf("maxParallel = %d, want 1 (один SID — один воркер активен)", mp)
	}
}

// ---- window: реклейм посреди окна (fencing-CAS) → не финализируем ----

func TestExecuteCommand_Window_FenceLostMidRun_NoFinalize(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	fdb.verifyLeaseLost.Store(true) // VerifyOwnership заваливает все единицы
	sp := &fakeCommandSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	v := windowCommandVoyage([]string{"a", "b"}, intp(1), nil)
	v.Attempt = 5
	status, summary, _ := w.executeCommandVoyage(context.Background(), v, make(chan struct{}))

	if status != "" {
		t.Errorf("status = %q, want \"\" (fence lost — не финализируем)", status)
	}
	if summary != nil {
		t.Errorf("summary = %+v, want nil", summary)
	}
	// fencing остановил dispatch — ни один Errand не отправлен.
	if len(sp.calls) != 0 {
		t.Errorf("spawn calls = %d, want 0 (fencing до dispatch)", len(sp.calls))
	}
}
