package voyageorch

import (
	"context"
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
)

// Прогресс батчей (current_batch_index) инкрементится по ОДНОМУ за каждый
// завершённый Leg: legIdx+1. 5 инкарнаций, batch_size=2 → 3 Leg-а → 1,2,3.
// Терминал current_batch_index == total_batches (3/3 = 100%).
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

// Один Leg (batch_size=NULL → весь прогон одним Leg) → current_batch_index 1.
// Терминал 1/1.
func TestExecuteScenario_BatchProgress_SingleBatch(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: &fakeAwaiter{}}

	v := scenarioVoyage([]string{"a", "b", "c"}, nil, nil, nil, nil)
	w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))

	if got := fdb.batchProgressSeq(); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("batch progress = %v, want [1] (один Leg)", got)
	}
}

// window-режим: НЕ Leg-овый, прогресс НЕ инкрементится (остаётся 0, UI считает
// по targets). UpdateBatchProgress в окне не вызывается.
func TestExecuteScenario_BatchProgress_WindowNotIncremented(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: &fakeAwaiter{}}

	v := windowScenarioVoyage([]string{"a", "b", "c", "d"}, intp(2), nil)
	w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))

	if got := fdb.batchProgressSeq(); len(got) != 0 {
		t.Fatalf("window-режим НЕ должен инкрементить current_batch_index, got %v", got)
	}
}

// abort посреди (fail_threshold): current_batch_index = число ЗАВЕРШЁННЫХ Leg-ов
// ДО abort. Leg0=[a,b] завершён (→1), порог достигнут, Leg1 не стартует.
func TestExecuteScenario_BatchProgress_AbortMid(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{}
	aw := &fakeAwaiter{outcomes: map[string]TargetOutcome{"ap-b": OutcomeFailed}}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: aw}

	abort := voyage.OnFailureAbort
	v := scenarioVoyage([]string{"a", "b", "c", "d"}, intp(2), nil, &abort, nil)
	w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))

	// Leg0 завершён → progress 1; abort до Leg1 → больше нет.
	if got := fdb.batchProgressSeq(); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("abort-progress = %v, want [1] (только завершённый Leg0)", got)
	}
}

// kind=command, барьерный multi-batch: симметрия со scenario. 4 SID, batch_size=2
// → 2 Leg-а → 1,2.
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

// kind=command window: прогресс НЕ инкрементится.
func TestExecuteCommand_BatchProgress_WindowNotIncremented(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeCommandSpawner{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}

	v := windowCommandVoyage([]string{"s1", "s2", "s3"}, intp(2), nil)
	w.executeCommandVoyage(context.Background(), v, make(chan struct{}))

	if got := fdb.batchProgressSeq(); len(got) != 0 {
		t.Fatalf("command window НЕ должен инкрементить current_batch_index, got %v", got)
	}
}
