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

// claimContains checks for claim-SQL (FOR UPDATE SKIP LOCKED).
func claimContains(sql string) bool { return strings.Contains(sql, "FOR UPDATE SKIP LOCKED") }

// scenarioClaimRow claim-row kind=scenario with scenario_name and two
// incarnations in target_resolved (25 columns per scanVoyage order).
func scenarioClaimRow() scanRow {
	r := claimedVoyageRow("01HVOY", string(voyage.KindScenario), string(voyage.StatusRunning))
	scn := "deploy"
	r.values[2] = &scn                // scenario_name
	r.values[5] = []byte(`["a","b"]`) // target_resolved
	return r
}

// fakeSpawner stub [ScenarioSpawner]. Returns deterministic applyID
// `ap-<incarnation>` or error from failNames. Counts calls and tracks
// max parallelism (for concurrency-cap check).
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

// fakeAwaiter stub [IncarnationAwaiter]. applyID → outcome from map (default
// succeeded). awaitErr global await-error (ctx-fail simulation).
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

// ---- parseIncarnationTargets ----  (no translation needed for test markers)

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

	// 5 incarnations, batch_size=2 → 3 Legs (2/2/1).
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
	// "b" fails on await → abort after first Leg (batch_size=2):
	// Leg0=[a,b] executes, failure → Leg1=[c,d] does not start.
	sp := &fakeSpawner{}
	aw := &fakeAwaiter{outcomes: map[string]TargetOutcome{"ap-b": OutcomeFailed}}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: aw}

	abort := voyage.OnFailureAbort
	v := scenarioVoyage([]string{"a", "b", "c", "d"}, intp(2), nil, &abort, nil)
	status, summary, _ := w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))

	if status != voyage.StatusPartialFailed {
		t.Errorf("status = %q, want partial_failed", status)
	}
	// Only first Leg executed: a(success)+b(failed) = 2 spawn calls.
	if len(sp.calls) != 2 {
		t.Errorf("spawn calls = %d, want 2 (abort after Leg0)", len(sp.calls))
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
	// continue: all 4 incarnations of both Legs executed despite failure of b.
	if len(sp.calls) != 4 {
		t.Errorf("spawn calls = %d, want 4 (continue to end)", len(sp.calls))
	}
	if summary.Succeeded != 3 || summary.Failed != 1 {
		t.Errorf("summary = %+v", summary)
	}
}

// ---- all failed → failed ----

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

// ---- spawn-fail (incarnation not started) counts as failed ----

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

// ---- no_match benign (not failure) ----

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
	// 3 incarnations, batch_size=1 → 3 Legs → 2 pauses between ≈ 120ms.
	v := scenarioVoyage([]string{"a", "b", "c"}, intp(1), nil, nil, &interval)

	start := time.Now()
	status, _, _ := w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))
	elapsed := time.Since(start)

	if status != voyage.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", status)
	}
	if elapsed < 2*interval {
		t.Errorf("elapsed %v < 2*interval %v — pause between Legs not honored", elapsed, 2*interval)
	}
}

func TestExecuteScenario_InterBatchInterval_AbortedByLeaseLost(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: &fakeAwaiter{}}

	interval := time.Hour // long pause, interrupt via leaseLost
	leaseLost := make(chan struct{})
	v := scenarioVoyage([]string{"a", "b"}, intp(1), nil, nil, &interval)

	done := make(chan voyage.Status, 1)
	go func() {
		st, _, _ := w.executeScenarioVoyage(context.Background(), v, leaseLost)
		done <- st
	}()

	// Wait for first Leg (spawn "a"), then lose lease during pause.
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
			t.Fatal("first Leg did not start")
		case <-time.After(2 * time.Millisecond):
		}
	}
	close(leaseLost)

	select {
	case st := <-done:
		if st != "" {
			t.Errorf("status = %q, want \"\" (lease lost — skip finalize)", st)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("executeScenarioVoyage did not exit after leaseLost during pause")
	}
}

// ---- lease lost MID serial Leg: remaining not spawned ----

func TestExecuteScenario_LeaseLostMidLeg_StopsSpawn(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	// Long spawn (delay) — window to lose lease while first incarnation still
	// "working", before distributing remaining.
	sp := &fakeSpawner{delay: time.Hour}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: &fakeAwaiter{}}

	// batch_size=NULL → one Leg = all 4; concurrency=1 → strictly serial Leg.
	names := []string{"a", "b", "c", "d"}
	v := scenarioVoyage(names, nil, intp(1), nil, nil)

	leaseLost := make(chan struct{})
	done := make(chan voyage.Status, 1)
	go func() {
		st, _, _ := w.executeScenarioVoyage(context.Background(), v, leaseLost)
		done <- st
	}()

	// Wait for first incarnation to start (stuck in delay), then lose lease.
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
			t.Fatal("first incarnation of Leg did not start")
		case <-time.After(2 * time.Millisecond):
		}
	}
	close(leaseLost)

	select {
	case st := <-done:
		// Skip finalize on lease lost (Reaper-reclaim).
		if st != "" {
			t.Errorf("status = %q, want \"\" (lease lost mid Leg — skip finalize)", st)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("executeScenarioVoyage did not exit after leaseLost mid Leg")
	}

	// Remaining incarnations (b/c/d) not spawned: spawn stopped at acquire.
	sp.mu.Lock()
	calls := append([]string(nil), sp.calls...)
	sp.mu.Unlock()
	if len(calls) >= len(names) {
		t.Errorf("spawn calls = %v (%d), want < %d (remaining not spawned)", calls, len(calls), len(names))
	}
	for _, c := range calls {
		if c != "a" {
			t.Errorf("extra incarnation %q spawned after lease lost (calls=%v)", c, calls)
		}
	}
	// Voyage finalize not called.
	if got := fdb.finalCallCount.Load(); got != 0 {
		t.Errorf("Finalize called %d times, want 0 (lease lost — Reaper-reclaim)", got)
	}
}

// ---- concurrency-cap within Leg ----

func TestExecuteScenario_ConcurrencyCap(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{delay: 20 * time.Millisecond}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: &fakeAwaiter{}}

	// 6 incarnations one Leg, concurrency=2 → maxParallel should be ≤ 2.
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
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger()} // no Spawner/Awaiter
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

// ---- await ctx-fail → cancelled (not silent success) ----

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
	// cancelled-only → failed (was interruption before single success).
	if status != voyage.StatusFailed {
		t.Errorf("status = %q, want failed", status)
	}
}

// ---- worker dispatch by kind: scenario via executeVoyage ----

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
		t.Errorf("spawn calls = %d, want 2 (two incarnations in target_resolved)", calls)
	}
}

// ---- batch_mode=window for kind=scenario (S-W2): window unit = incarnation ----

// windowScenarioVoyage scenario-Voyage in sliding window mode. batch_size not
// used (window width = concurrency, ADR-043 amendment §1).
func windowScenarioVoyage(names []string, concurrency *int, onFailure *voyage.OnFailure) *voyage.Voyage {
	v := scenarioVoyage(names, nil, concurrency, onFailure, nil)
	wm := voyage.BatchModeWindow
	v.BatchMode = &wm
	return v
}

// concurrency-cap: window holds ≤ concurrency active scenario-runs
// (maxParallel == concurrency when len > concurrency).
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
		t.Errorf("maxParallel = %d, want <= 2 (window concurrency-cap)", mp)
	}
	if mp < 2 {
		t.Errorf("maxParallel = %d — parallelism not reached (expected 2)", mp)
	}
}

// concurrency ≥ count of incarnations → all start simultaneously (queue does not
// block, maxParallel == len). Parity command Window_ConcurrencyGEQLen.
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
		t.Errorf("maxParallel = %d, want %d (concurrency >= len → all at once, queue does not block)", mp, len(names))
	}
	if calls != len(names) {
		t.Errorf("spawn calls = %d, want %d", calls, len(names))
	}
}

// 1 incarnation in window → one worker, one SpawnScenarioRun, succeeded.
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
		t.Errorf("spawn calls = %v, want [only] (one SpawnScenarioRun)", calls)
	}
	if mp != 1 {
		t.Errorf("maxParallel = %d, want 1 (one incarnation — one worker active)", mp)
	}
}

// queue drains: concurrency=1 → strictly serial order [a,b,c].
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
		t.Errorf("spawn order = %v, want [a b c] (concurrency=1, FIFO-queue)", calls)
	}
}

// abort + fail_threshold (via on_failure=abort ≡ threshold 1): first failure
// stops spawn of new, unspawned → cancelled (parity barrier).
func TestExecuteScenario_Window_AbortStopsSpawn(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{}
	aw := &fakeAwaiter{outcomes: map[string]TargetOutcome{"ap-b": OutcomeFailed}}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: aw}

	abort := voyage.OnFailureAbort
	// concurrency=1 strictly serial: a(ok) → b(fail→abort) → stop. c/d/e not spawned.
	v := windowScenarioVoyage([]string{"a", "b", "c", "d", "e"}, intp(1), &abort)
	status, summary, _ := w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))

	if status != voyage.StatusPartialFailed {
		t.Errorf("status = %q, want partial_failed", status)
	}
	sp.mu.Lock()
	calls := append([]string(nil), sp.calls...)
	sp.mu.Unlock()
	if len(calls) != 2 {
		t.Errorf("spawn calls = %v (%d), want 2 (abort stopped spawn after failure)", calls, len(calls))
	}
	if summary.Succeeded != 1 || summary.Failed != 1 {
		t.Errorf("summary = %+v, want succeeded=1 failed=1", summary)
	}
	if summary.Total != 5 {
		t.Errorf("summary.Total = %d, want 5 (полный scope)", summary.Total)
	}
	if summary.Cancelled != 3 {
		t.Errorf("summary.Cancelled = %d, want 3 (c/d/e unspawned → cancelled)", summary.Cancelled)
	}
	if summary.Total != summary.Succeeded+summary.Failed+summary.Cancelled {
		t.Errorf("баланс summary не закрыт: %+v", summary)
	}
	for _, n := range []string{"c", "d", "e"} {
		if got := fdb.targetStatus(n); got != string(voyage.TargetStatusCancelled) {
			t.Errorf("target %s status = %q, want cancelled (unspawned at abort)", n, got)
		}
	}
}

// fail_threshold=2: window tolerates one failure, stops spawn at second.
func TestExecuteScenario_Window_FailThreshold(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{}
	aw := &fakeAwaiter{outcomes: map[string]TargetOutcome{"ap-a": OutcomeFailed, "ap-b": OutcomeFailed}}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: aw}

	v := windowScenarioVoyage([]string{"a", "b", "c", "d"}, intp(1), nil)
	thr := 2
	v.FailThreshold = &thr // explicit threshold: stop at 2nd failure
	status, summary, _ := w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))

	if status != voyage.StatusFailed {
		t.Errorf("status = %q, want failed", status)
	}
	sp.mu.Lock()
	calls := len(sp.calls)
	sp.mu.Unlock()
	// a(fail) → b(fail, threshold reached → stop). c/d not spawned.
	if calls != 2 {
		t.Errorf("spawn calls = %d, want 2 (stop at 2nd failure)", calls)
	}
	if summary.Failed != 2 || summary.Cancelled != 2 {
		t.Errorf("summary = %+v, want failed=2 cancelled=2", summary)
	}
}

// continue: window drains queue to end despite failure.
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
		t.Errorf("spawn calls = %d, want 4 (continue drained all queue)", len(sp.calls))
	}
	if summary.Succeeded != 3 || summary.Failed != 1 {
		t.Errorf("summary = %+v, want succeeded=3 failed=1", summary)
	}
}

// VerifyOwnership called per-unit (one check per incarnation).
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

// lease lost mid-window (renewLoop-channel) → skip finalize (Reaper-reclaim).
func TestExecuteScenario_Window_LeaseLostMidRun_NoFinalize(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{delay: time.Hour} // keep workers active
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
			t.Fatal("window did not start")
		case <-time.After(2 * time.Millisecond):
		}
	}
	close(leaseLost)

	select {
	case st := <-done:
		if st != "" {
			t.Errorf("status = %q, want \"\" (lease lost mid-window — skip finalize)", st)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runScenarioSlidingWindow did not exit after leaseLost")
	}
	if got := fdb.finalCallCount.Load(); got != 0 {
		t.Errorf("Finalize called %d times, want 0 (lease lost — Reaper-reclaim)", got)
	}
}

// reclaim mid-window (fencing-CAS VerifyOwnership) → skip finalize, no
// scenario-run spawned.
func TestExecuteScenario_Window_FenceLostMidRun_NoFinalize(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	fdb.verifyLeaseLost.Store(true) // VerifyOwnership fails all units
	sp := &fakeSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: &fakeAwaiter{}}

	v := windowScenarioVoyage([]string{"a", "b"}, intp(1), nil)
	v.Attempt = 5
	status, summary, _ := w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))

	if status != "" {
		t.Errorf("status = %q, want \"\" (fence lost — skip finalize)", status)
	}
	if summary != nil {
		t.Errorf("summary = %+v, want nil", summary)
	}
	if len(sp.calls) != 0 {
		t.Errorf("spawn calls = %d, want 0 (fencing before spawn)", len(sp.calls))
	}
}

// inter_unit_interval: per-unit pause in window before spawn of each incarnation.
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

// cancellation DURING inter_unit-pause: dequeued incarnation cancelled,
// SpawnScenarioRun not called.
func TestExecuteScenario_Window_InterUnitInterval_CancelledDuringPause(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: &fakeAwaiter{}}

	v := windowScenarioVoyage([]string{"a", "b", "c"}, intp(1), nil)
	iu := time.Hour // long pause — unit stuck in throttle
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
			t.Errorf("spawn calls = %v, want 0 (cancel in pause — SpawnScenarioRun not called)", calls)
		}
		if got := fdb.targetStatus("a"); got != string(voyage.TargetStatusCancelled) {
			t.Errorf("target a status = %q, want cancelled (dequeued, waited throttle)", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("executeScenarioVoyage did not exit after cancel in pause")
	}
}

// batch_index=0 for all incarnations in window. In voyageorch batch_index
// written by S5-Insert (handler test TestVoyageCreate_ScenarioWindowOK covers);
// here verify window-executor does not create Leg-enumeration — all
// incarnations run flat (one scope, no Leg boundaries).
func TestExecuteScenario_Window_FlatNoLegs(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: &fakeAwaiter{}}

	// 5 incarnations, concurrency=2: barrier would slice Legs, window does not.
	v := windowScenarioVoyage([]string{"a", "b", "c", "d", "e"}, intp(2), nil)
	status, summary, _ := w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))

	if status != voyage.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", status)
	}
	if summary.Total != 5 || summary.Succeeded != 5 {
		t.Errorf("summary = %+v, want total=5 succeeded=5", summary)
	}
	if len(sp.calls) != 5 {
		t.Errorf("spawn calls = %d, want 5 (all incarnations one flat run)", len(sp.calls))
	}
}
