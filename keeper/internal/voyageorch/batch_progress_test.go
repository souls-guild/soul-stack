package voyageorch

import (
	"context"
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
)

// Batch progress (current_batch_index) increments ONE per completed Leg:
// legIdx+1. 5 incarnations, batch_size=2 → 3 Legs → 1,2,3.
// Terminal current_batch_index == total_batches (3/3 = 100%).
func TestExecuteScenario_BatchProgress_MultiBatch(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: &fakeAwaiter{}}

	v := scenarioVoyage([]string{"a", "b", "c", "d", "e"}, intp(2), nil, nil, nil)
	if _, _, _ = w.executeScenarioVoyage(context.Background(), v, make(chan struct{})); true {
	}

	if got := fdb.batchProgressSeq(); !reflect.DeepEqual(got, []int{1, 2, 3}) {
		t.Fatalf("batch progress = %v, want [1 2 3]", got)
	}
}

// One Leg (batch_size=NULL → all run as one Leg) → current_batch_index 1.
// Terminal 1/1.
func TestExecuteScenario_BatchProgress_SingleBatch(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: &fakeAwaiter{}}

	v := scenarioVoyage([]string{"a", "b", "c"}, nil, nil, nil, nil)
	w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))

	if got := fdb.batchProgressSeq(); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("batch progress = %v, want [1] (one Leg)", got)
	}
}

// window-mode: NOT Leg-based, progress does NOT increment (stays 0, UI counts
// by targets). UpdateBatchProgress not called in window.
func TestExecuteScenario_BatchProgress_WindowNotIncremented(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: &fakeAwaiter{}}

	v := windowScenarioVoyage([]string{"a", "b", "c", "d"}, intp(2), nil)
	w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))

	if got := fdb.batchProgressSeq(); len(got) != 0 {
		t.Fatalf("window-mode must NOT increment current_batch_index, got %v", got)
	}
}

// abort mid (fail_threshold): current_batch_index = count of COMPLETED Legs
// BEFORE abort. Leg0=[a,b] completed (→1), threshold reached, Leg1 does not start.
func TestExecuteScenario_BatchProgress_AbortMid(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{}
	aw := &fakeAwaiter{outcomes: map[string]TargetOutcome{"ap-b": OutcomeFailed}}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: aw}

	abort := voyage.OnFailureAbort
	v := scenarioVoyage([]string{"a", "b", "c", "d"}, intp(2), nil, &abort, nil)
	w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))

	// Leg0 completed → progress 1; abort before Leg1 → no more.
	if got := fdb.batchProgressSeq(); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("abort-progress = %v, want [1] (only completed Leg0)", got)
	}
}

// kind=command, barrier multi-batch: symmetry with scenario. 4 SID, batch_size=2
// → 2 Legs → 1,2.
func TestExecuteCommand_BatchProgress_MultiBatch(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeCommandSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	v := commandVoyage([]string{"s1", "s2", "s3", "s4"}, intp(2), nil, nil, nil)
	w.executeCommandVoyage(context.Background(), v, make(chan struct{}))

	if got := fdb.batchProgressSeq(); !reflect.DeepEqual(got, []int{1, 2}) {
		t.Fatalf("command batch progress = %v, want [1 2]", got)
	}
}

// kind=command window: progress does NOT increment.
func TestExecuteCommand_BatchProgress_WindowNotIncremented(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeCommandSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	v := windowCommandVoyage([]string{"s1", "s2", "s3"}, intp(2), nil)
	w.executeCommandVoyage(context.Background(), v, make(chan struct{}))

	if got := fdb.batchProgressSeq(); len(got) != 0 {
		t.Fatalf("command window must NOT increment current_batch_index, got %v", got)
	}
}
