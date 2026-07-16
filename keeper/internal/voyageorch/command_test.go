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

// commandClaimRow is a kind=command claim row with module and two hosts in
// target_resolved (25 columns in scanVoyage order).
func commandClaimRow() scanRow {
	r := claimedVoyageRow("01HVOYCMD", string(voyage.KindCommand), string(voyage.StatusRunning))
	mod := "core.cmd.shell"
	r.values[3] = &mod                                  // module
	r.values[5] = []byte(`["s1.example","s2.example"]`) // target_resolved
	return r
}

// fakeCommandSpawner is a [CommandSpawner] stub. It returns deterministic errandID
// `er-<sid>` and status from statuses (default "success") or error from failSIDs.
// It counts calls and records max parallelism (for concurrency-cap).
type fakeCommandSpawner struct {
	mu          sync.Mutex
	failSIDs    map[string]bool
	statuses    map[string]string // sid -> errand-status (default "success")
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

// windowCommandVoyage is kind=command in batch_mode=window (S-W1). batch_size is
// not used (window width = concurrency).
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

// ---- executeCommandVoyage: one batch, all hosts success ----

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
	// voyage_targets SID tracking: every host reached succeeded.
	for _, s := range []string{"s1", "s2", "s3"} {
		if got := fdb.targetStatus(s); got != string(voyage.TargetStatusSucceeded) {
			t.Errorf("target %s status = %q, want succeeded", s, got)
		}
	}
}

// ---- chunking by SID: N batches ----

func TestExecuteCommand_MultipleBatches(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeCommandSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	// 5 hosts, batch_size=2 -> 3 Legs (2/2/1).
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
	// "b" returns failed status -> abort after the first Leg (batch_size=2):
	// Leg0=[a,b] runs, failure -> Leg1=[c,d] does NOT start.
	sp := &fakeCommandSpawner{statuses: map[string]string{"b": "failed"}}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	abort := voyage.OnFailureAbort
	v := commandVoyage([]string{"a", "b", "c", "d"}, intp(2), nil, &abort, nil)
	status, summary, _ := w.executeCommandVoyage(context.Background(), v, make(chan struct{}))

	if status != voyage.StatusPartialFailed {
		t.Errorf("status = %q, want partial_failed", status)
	}
	if len(sp.calls) != 2 {
		t.Errorf("spawn calls = %d, want 2 (abort after Leg0)", len(sp.calls))
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
		t.Errorf("spawn calls = %d, want 4 (continue to the end)", len(sp.calls))
	}
	if summary.Succeeded != 3 || summary.Failed != 1 {
		t.Errorf("summary = %+v", summary)
	}
}

// ---- fail_threshold: generalized abort-gate (S-W3) ----

// barrier: fail_threshold=2 means the run continues while cumulative failures < 2;
// on the 2nd failure (after the Leg where it accumulated), stop. 6 hosts with
// batch_size=1 -> Legs of 1; "b" and "d" fail: after Leg "d" failed=2 -> stop,
// "e"/"f" are skipped.
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
	// Legs a,b,c,d executed (4 spawns), failed=2 after "d" -> stop; e,f skipped.
	if len(sp.calls) != 4 {
		t.Errorf("spawn calls = %d, want 4 (stop at fail_threshold=2 after d)", len(sp.calls))
	}
	if summary.Failed != 2 {
		t.Errorf("summary.Failed = %d, want 2", summary.Failed)
	}
}

// barrier: fail_threshold=3 with 1 failure does NOT fire; run to the end
// (intermediate tolerance).
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
		t.Errorf("spawn calls = %d, want 4 (1 failure < threshold 3 -> to the end)", len(sp.calls))
	}
}

// window: fail_threshold=2 stops spawning new items once 2 failures are reached;
// already pulled items finish, queue remainder -> cancelled. All hosts fail,
// concurrency=1 (deterministic pull order).
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
		t.Errorf("status = %q, want failed (all failures, no successes)", status)
	}
	// concurrency=1: a,b spawn -> failed=2 -> cancelFan; c,d remain in queue.
	if len(sp.calls) != 2 {
		t.Errorf("spawn calls = %d, want 2 (stop at fail_threshold=2)", len(sp.calls))
	}
	if summary.Failed != 2 || summary.Cancelled != 2 {
		t.Errorf("summary = %+v, want Failed=2 Cancelled=2", summary)
	}
}

// ---- inter_unit_interval: per-unit throttle in window (S-W3) ----

// window with inter_unit_interval > 0: pause before each unit lengthens the run by
// at least (N-1)*interval (rough check: total time >= pause sum).
func TestExecuteCommand_Window_InterUnitInterval(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeCommandSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	// concurrency=1, 3 hosts, 30ms pause before each unit -> >= 90ms total.
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
		t.Errorf("elapsed = %v, want >= ~90ms (3*30ms inter_unit throttle)", elapsed)
	}
}

// Cancellation (fanCtx) DURING inter_unit pause stops spawning: unit taken from
// the queue and waiting in throttle is marked cancelled, SpawnCommand is NOT called
// (qa-gap to happy-path InterUnitInterval). concurrency=1, long pause -> first
// unit is guaranteed to be in throttle when parent ctx is cancelled.
func TestExecuteCommand_Window_InterUnitInterval_CancelledDuringPause(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeCommandSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	v := windowCommandVoyage([]string{"a", "b", "c"}, intp(1), nil)
	iu := time.Hour // long pause: unit gets stuck in throttle; cancel there
	v.InterUnitInterval = &iu

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *voyage.Summary, 1)
	go func() {
		_, summary, _ := w.executeCommandVoyage(ctx, v, make(chan struct{}))
		done <- summary
	}()

	// Let worker take "a" from the queue and enter inter_unit pause, then cancel.
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case summary := <-done:
		sp.mu.Lock()
		calls := append([]string(nil), sp.calls...)
		sp.mu.Unlock()
		if len(calls) != 0 {
			t.Errorf("spawn calls = %v, want 0 (cancelled in pause - SpawnCommand not called)", calls)
		}
		if got := fdb.targetStatus("a"); got != string(voyage.TargetStatusCancelled) {
			t.Errorf("target a status = %q, want cancelled (taken from queue, did not start)", got)
		}
		if summary == nil || summary.Cancelled == 0 {
			t.Errorf("summary = %+v, want Cancelled >= 1", summary)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("executeCommandVoyage did not exit after ctx cancellation in inter_unit pause")
	}
}

// ---- timed_out / module_not_allowed count as failure ----

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

// ---- spawn error (Errand did not start) counts as failed ----

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

// ---- all failed -> failed ----

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

// ---- inter_batch_interval: pause between Legs ----

func TestExecuteCommand_InterBatchInterval(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeCommandSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	interval := 60 * time.Millisecond
	// 3 hosts, batch_size=1 -> 3 Legs -> 2 pauses between them, about 120ms.
	v := commandVoyage([]string{"a", "b", "c"}, intp(1), nil, nil, &interval)

	start := time.Now()
	status, _, _ := w.executeCommandVoyage(context.Background(), v, make(chan struct{}))
	elapsed := time.Since(start)

	if status != voyage.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", status)
	}
	if elapsed < 2*interval {
		t.Errorf("elapsed %v < 2*interval %v - pause between Legs not respected", elapsed, 2*interval)
	}
}

func TestExecuteCommand_InterBatchInterval_AbortedByLeaseLost(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeCommandSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	interval := time.Hour // long pause, interrupt through leaseLost
	leaseLost := make(chan struct{})
	v := commandVoyage([]string{"a", "b"}, intp(1), nil, nil, &interval)

	done := make(chan voyage.Status, 1)
	go func() {
		st, _, _ := w.executeCommandVoyage(context.Background(), v, leaseLost)
		done <- st
	}()

	// Wait for the first Leg (spawn "a"), then lose lease during pause.
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
			t.Errorf("status = %q, want \"\" (lease lost - do not finalize)", st)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("executeCommandVoyage did not exit after leaseLost during pause")
	}
}

// ---- lease lost IN THE MIDDLE of a serial Leg: remaining hosts do not spawn (AS S2) ----

func TestExecuteCommand_LeaseLostMidLeg_StopsSpawn(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	// Long spawn (delay) creates a window to lose lease while the first host is
	// still "in progress", before assigning the remaining ones.
	sp := &fakeCommandSpawner{delay: time.Hour}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	// batch_size=NULL -> one Leg = all 4; concurrency=1 -> strictly serial Leg.
	sids := []string{"a", "b", "c", "d"}
	v := commandVoyage(sids, nil, intp(1), nil, nil)

	leaseLost := make(chan struct{})
	done := make(chan voyage.Status, 1)
	go func() {
		st, _, _ := w.executeCommandVoyage(context.Background(), v, leaseLost)
		done <- st
	}()

	// Wait until the first host starts (blocked in delay), then lose lease.
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
			t.Fatal("first host in Leg did not start")
		case <-time.After(2 * time.Millisecond):
		}
	}
	close(leaseLost)

	select {
	case st := <-done:
		if st != "" {
			t.Errorf("status = %q, want \"\" (lease lost in the middle of a Leg - do not finalize)", st)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("executeCommandVoyage did not exit after leaseLost in the middle of a Leg")
	}

	// Remaining hosts (b/c/d) did NOT spawn: spawn stopped on acquire.
	sp.mu.Lock()
	calls := append([]string(nil), sp.calls...)
	sp.mu.Unlock()
	if len(calls) >= len(sids) {
		t.Errorf("spawn calls = %v (%d), want < %d (remaining hosts do not spawn)", calls, len(calls), len(sids))
	}
	for _, c := range calls {
		if c != "a" {
			t.Errorf("extra host %q spawned after lease loss (calls=%v)", c, calls)
		}
	}
	// Voyage finalization was not called.
	if got := fdb.finalCallCount.Load(); got != 0 {
		t.Errorf("Finalize called %d times, want 0 (lease lost - Reaper-reclaim)", got)
	}
}

// ---- concurrency-cap inside Leg (fan-out) ----

func TestExecuteCommand_ConcurrencyCap(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeCommandSpawner{delay: 20 * time.Millisecond}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	// 6 hosts in one Leg, concurrency=2 -> maxParallel must be == 2.
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
		t.Errorf("maxParallel = %d - parallelism not reached (expected 2)", mp)
	}
}

// ---- nil CommandSpawner -> fail-closed ----

func TestExecuteCommand_NilSpawner_FailClosed(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger()} // no CommandSpawner
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

// ---- nil module -> fail-closed ----

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
		t.Errorf("spawn calls = %d, want 0 (module nil - spawn does not start)", len(sp.calls))
	}
}

// ---- voyage_targets back-link: errand_id, not apply_id ----

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
		t.Fatal("MarkTargetRunning not called")
	}
	if want := "er-s1"; gotArg != want {
		t.Errorf("back-link arg = %q, want %q (errand_id)", gotArg, want)
	}
}

// fencingSpawner is a spawner for mid-Leg reclaim test (S-med-2): after dispatch
// of the first host, onFirst fires (test raises reclaim through fdb.verifyLeaseLost).
// Counts spawned SIDs.
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

// ---- reclaim in the middle of a Leg: fencing before dispatch does not send Errand (S-med-2) ----
//
// Scenario: serial Leg [a,b]. Host "a" is spawned (worker still owns it). Before
// dispatch of "b", Voyage is reclaimed by another Keeper (attempt++) -> VerifyOwnership
// returns ErrLeaseLost -> "b" Errand is NOT sent (no duplicate), Leg is interrupted,
// executeCommandVoyage does not finalize.
func TestExecuteCommand_ReclaimMidLeg_FenceStopsDispatch(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fencingSpawner{}
	// After dispatch of first host, emulate reclaim: VerifyOwnership for the next
	// host returns ErrLeaseLost.
	sp.onFirst = func() { fdb.verifyLeaseLost.Store(true) }
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	// batch_size=NULL -> one Leg = [a,b]; concurrency=1 -> strictly serial.
	v := commandVoyage([]string{"a", "b"}, nil, intp(1), nil, nil)
	v.Attempt = 7 // worker claim epoch (passed to VerifyOwnership/MarkTargetRunning).

	status, summary, _ := w.executeCommandVoyage(context.Background(), v, make(chan struct{}))

	// Lease lost in the middle of a Leg -> do NOT finalize.
	if status != "" {
		t.Errorf("status = %q, want \"\" (fence lost - Reaper-reclaim, do not finalize)", status)
	}
	if summary != nil {
		t.Errorf("summary = %+v, want nil (do not finalize)", summary)
	}

	// "b" is NOT spawned (fencing stopped dispatch before sending Errand).
	calls := sp.callList()
	if len(calls) != 1 || calls[0] != "a" {
		t.Errorf("spawn calls = %v, want [a] (b not sent after reclaim)", calls)
	}
	// "b" recorded as cancelled (fencing path), not succeeded/failed.
	if got := fdb.targetStatus("b"); got != string(voyage.TargetStatusCancelled) {
		t.Errorf("target b status = %q, want cancelled (fence lost)", got)
	}
}

// ---- fencing before FIRST host: Errand is not sent at all ----
//
// If lease is lost before the first host starts (verifyLeaseLost=true from the
// beginning), VerifyOwnership fails "a" -> Errand is not sent to any host.
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
		t.Errorf("status = %q, want \"\" (fence lost - do not finalize)", status)
	}
	if summary != nil {
		t.Errorf("summary = %+v, want nil", summary)
	}
	if calls := sp.callList(); len(calls) != 0 {
		t.Errorf("spawn calls = %v, want [] (no Errand sent)", calls)
	}
}

// ============================================================================
// S-W1: batch_mode=window (sliding window across hosts, kind=command)
// ============================================================================

// ---- window: entire queue drains, all success ----

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
	// Queue drained completely: every SID spawned exactly once.
	if len(sp.calls) != 5 {
		t.Errorf("spawn calls = %d, want 5 (queue drained)", len(sp.calls))
	}
	for _, s := range sids {
		if got := fdb.targetStatus(s); got != string(voyage.TargetStatusSucceeded) {
			t.Errorf("target %s status = %q, want succeeded", s, got)
		}
	}
}

// ---- window: keeps <= concurrency active, but drains the entire queue ----
//
// concurrency=3, 9 hosts, each spawn holds delay. If window behaved like a chunk
// barrier, maxParallel would jump by batches; sliding window keeps STRICTLY 3
// active while single-pool without barriers drains all 9.
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
		t.Errorf("maxParallel = %d, want <= 3 (window keeps no more than concurrency)", mp)
	}
	if mp < 3 {
		t.Errorf("maxParallel = %d - window not filled to concurrency (expected 3)", mp)
	}
	if calls != 9 {
		t.Errorf("spawn calls = %d, want 9 (entire queue drained by one pool)", calls)
	}
}

// ---- window: lease-fencing is called per-unit (VerifyOwnership for each SID) ----

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
	// runOneCommand calls VerifyOwnership before each dispatch -> exactly one
	// per window unit.
	if got := fdb.verifyCalls.Load(); got != int64(len(sids)) {
		t.Errorf("VerifyOwnership calls = %d, want %d (per-unit fencing)", got, len(sids))
	}
}

// ---- window on_failure=abort: first failure stops spawning new items ----
//
// concurrency=1 (strictly serial window), 5 hosts, "b" fails. abort must stop
// polling the queue after "b" failure -> c/d/e are not spawned, but are marked
// cancelled (parity with barrier "remaining Legs skipped", qa-gap).
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
	// concurrency=1 strictly serial: a(ok) -> b(fail->abort) -> stop. c/d/e do not spawn.
	sp.mu.Lock()
	calls := append([]string(nil), sp.calls...)
	sp.mu.Unlock()
	if len(calls) != 2 {
		t.Errorf("spawn calls = %v (%d), want 2 (abort stopped spawning after failure)", calls, len(calls))
	}
	if summary.Succeeded != 1 || summary.Failed != 1 {
		t.Errorf("summary = %+v, want succeeded=1 failed=1", summary)
	}
	if summary.Total != 5 {
		t.Errorf("summary.Total = %d, want 5 (full scope)", summary.Total)
	}
	// parity with barrier: unspawned c/d/e are marked cancelled, balance is closed.
	if summary.Cancelled != 3 {
		t.Errorf("summary.Cancelled = %d, want 3 (c/d/e unspawned -> cancelled)", summary.Cancelled)
	}
	if summary.Total != summary.Succeeded+summary.Failed+summary.Cancelled {
		t.Errorf("summary balance is not closed: %+v (Total != succeeded+failed+cancelled)", summary)
	}
	// voyage_targets: c/d/e are recorded cancelled (drill UI sees them like barrier).
	for _, s := range []string{"c", "d", "e"} {
		if got := fdb.targetStatus(s); got != string(voyage.TargetStatusCancelled) {
			t.Errorf("target %s status = %q, want cancelled (unspawned on abort)", s, got)
		}
	}
}

// ---- window on_failure=continue: window drains queue to the end ----

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
		t.Errorf("spawn calls = %d, want 4 (continue drained the entire queue)", len(sp.calls))
	}
	if summary.Succeeded != 3 || summary.Failed != 1 {
		t.Errorf("summary = %+v, want succeeded=3 failed=1", summary)
	}
}

// ---- window: all failed -> failed ----

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

// ---- window: lease lost in the middle of window -> do not finalize (Reaper-reclaim) ----

func TestExecuteCommand_Window_LeaseLostMidRun_NoFinalize(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeCommandSpawner{delay: time.Hour} // keep workers active
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	v := windowCommandVoyage([]string{"a", "b", "c", "d"}, intp(2), nil)
	leaseLost := make(chan struct{})
	done := make(chan voyage.Status, 1)
	go func() {
		st, _, _ := w.executeCommandVoyage(context.Background(), v, leaseLost)
		done <- st
	}()

	// Wait until the window fills (at least one worker enters delay), then lose lease.
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
			t.Errorf("status = %q, want \"\" (lease lost in the middle of window - do not finalize)", st)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runSlidingWindow did not exit after leaseLost")
	}
	if got := fdb.finalCallCount.Load(); got != 0 {
		t.Errorf("Finalize called %d times, want 0 (lease lost - Reaper-reclaim)", got)
	}
}

// ---- window: concurrency >= len(sids) -> window degenerates into "all at once" ----
//
// concurrency=5 with 3 hosts: queue does not block any worker, all start in
// parallel (maxParallel == len(sids)), all success (qa-gap high).
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
		t.Errorf("maxParallel = %d, want %d (concurrency >= len -> all at once, queue does not block)", mp, len(sids))
	}
	if calls != len(sids) {
		t.Errorf("spawn calls = %d, want %d", calls, len(sids))
	}
}

// ---- window concurrency=1: strictly serial call ORDER ----
//
// One worker pulls the FIFO queue -> SpawnCommand is called strictly [a,b,c]
// (qa-gap high: window with concurrency=1 = deterministic serial run).
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
		t.Errorf("spawn order = %v, want [a b c] (concurrency=1 strictly serial)", calls)
	}
	if mp != 1 {
		t.Errorf("maxParallel = %d, want 1 (one worker)", mp)
	}
}

// ---- window: 1 SID -> one worker, one dispatch ----
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
		t.Errorf("spawn calls = %v, want [only] (one dispatch)", calls)
	}
	if mp != 1 {
		t.Errorf("maxParallel = %d, want 1 (one SID - one worker active)", mp)
	}
}

// ---- window: reclaim in the middle of window (fencing-CAS) -> do not finalize ----

func TestExecuteCommand_Window_FenceLostMidRun_NoFinalize(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	fdb.verifyLeaseLost.Store(true) // VerifyOwnership fails all units
	sp := &fakeCommandSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	v := windowCommandVoyage([]string{"a", "b"}, intp(1), nil)
	v.Attempt = 5
	status, summary, _ := w.executeCommandVoyage(context.Background(), v, make(chan struct{}))

	if status != "" {
		t.Errorf("status = %q, want \"\" (fence lost - do not finalize)", status)
	}
	if summary != nil {
		t.Errorf("summary = %+v, want nil", summary)
	}
	// fencing stopped dispatch - no Errand was sent.
	if len(sp.calls) != 0 {
		t.Errorf("spawn calls = %d, want 0 (fencing before dispatch)", len(sp.calls))
	}
}
