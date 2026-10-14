package voyageorch

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
)

// refusingSpawner refuses every incarnation in refuse with its error, and starts
// the rest.
type refusingSpawner struct {
	refuse map[string]error
}

func (s *refusingSpawner) SpawnScenarioRun(_ context.Context, _, name, _ string, _ []byte, _ string, _ *string) (string, error) {
	if err, ok := s.refuse[name]; ok {
		return "", err
	}
	return "ap-" + name, nil
}

// A target refused before its run existed keeps the spawner's reason on its row:
// nothing else in the API says why it failed (NIM-989). A refusal wrapped further
// up the chain still counts, and a plain error records the fallback code without
// its text, which was not composed for an operator.
func TestExecuteScenario_SpawnRefusalReasonReachesTargetRow(t *testing.T) {
	t.Parallel()
	locked := voyage.TargetReason{
		Code:    voyage.TargetErrorIncarnationErrorLocked,
		Message: "incarnation locked is error_locked by run 01HLOCK",
	}
	sp := &refusingSpawner{refuse: map[string]error{
		"locked": fmt.Errorf("outer: %w", &SpawnRefusal{Reason: locked, Err: errors.New("cause")}),
		"broken": errors.New("runner.Start: input secret=hunter2 rejected"),
	}}
	fdb := &fakeDB{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: &fakeAwaiter{}}
	cont := voyage.OnFailureContinue

	status, _, _ := w.executeScenarioVoyage(context.Background(),
		scenarioVoyage([]string{"locked", "broken", "fine"}, nil, nil, &cont, nil), make(chan struct{}))

	if status != voyage.StatusPartialFailed {
		t.Errorf("status = %q, want partial_failed", status)
	}
	if got := fdb.targetReason("locked"); got != locked {
		t.Errorf("locked target reason = %+v, want %+v", got, locked)
	}
	if got := fdb.targetReason("broken"); got != (voyage.TargetReason{Code: voyage.TargetErrorSpawn}) {
		t.Errorf("plain spawn error reason = %+v, want code %q and no message", got, voyage.TargetErrorSpawn)
	}
	if got := fdb.targetReason("fine"); got != (voyage.TargetReason{}) {
		t.Errorf("succeeded target reason = %+v, want none", got)
	}
}

// The window executor goes through the same per-incarnation spawn and must not
// lose the reason on the way.
func TestExecuteScenario_Window_SpawnRefusalReasonReachesTargetRow(t *testing.T) {
	t.Parallel()
	locked := voyage.TargetReason{Code: voyage.TargetErrorIncarnationErrorLocked, Message: "locked"}
	sp := &refusingSpawner{refuse: map[string]error{"locked": &SpawnRefusal{Reason: locked}}}
	fdb := &fakeDB{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: &fakeAwaiter{}}
	v := scenarioVoyage([]string{"locked", "fine"}, nil, intp(2), nil, nil)
	window := voyage.BatchModeWindow
	v.BatchMode = &window

	w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))

	if got := fdb.targetReason("locked"); got != locked {
		t.Errorf("locked target reason = %+v, want %+v", got, locked)
	}
}

// A host refused before dispatch carries the spawner's code (NIM-617); its error
// text does not reach the row.
func TestExecuteCommand_SpawnErrorCodeReachesTargetRow(t *testing.T) {
	t.Parallel()
	sp := &fakeCommandSpawner{
		failSIDs:  map[string]bool{"down": true},
		failCodes: map[string]string{"down": voyage.TargetErrorSoulNotConnected},
		statuses:  map[string]string{"fine": "success"},
	}
	fdb := &fakeDB{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: sp}
	cont := voyage.OnFailureContinue

	w.executeCommandVoyage(context.Background(), commandVoyage([]string{"down", "fine"}, nil, nil, &cont, nil), make(chan struct{}))

	if got := fdb.targetReason("down"); got != (voyage.TargetReason{Code: voyage.TargetErrorSoulNotConnected}) {
		t.Errorf("refused host reason = %+v, want code soul_not_connected and no message", got)
	}
	if got := fdb.targetReason("fine"); got != (voyage.TargetReason{}) {
		t.Errorf("succeeded host reason = %+v, want none", got)
	}
}

// abortAwareSpawner answers the way the Errand bridge does: a ctx cancelled while
// it awaits the Errand returns status `cancelled` with no error (byStatus), or the
// ctx error before dispatch (byError).
type abortAwareSpawner struct{ byError bool }

func (s abortAwareSpawner) SpawnCommand(ctx context.Context, _, sid, _, _ string, _ []byte, _ bool) (string, string, string, error) {
	<-ctx.Done()
	if s.byError {
		return "", "failed", voyage.TargetErrorSpawn, ctx.Err()
	}
	return "er-" + sid, "cancelled", voyage.TargetErrorCancelled, nil
}

// A host our own abort, lease loss or shutdown stops is cancelled, not refused:
// it records no code on either path the spawner can report it by. Only an Errand
// that was itself cancelled says `cancelled`.
func TestRunOneCommand_OwnAbortRecordsNoCode(t *testing.T) {
	t.Parallel()
	for _, byError := range []bool{false, true} {
		fdb := &fakeDB{}
		w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: abortAwareSpawner{byError: byError}}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var fenceLost atomic.Bool
		res := w.runOneCommand(ctx, commandVoyage([]string{"h"}, nil, nil, nil, nil), "h", func() {}, &fenceLost)

		if res.Outcome != OutcomeCancelled {
			t.Errorf("byError=%v: outcome = %q, want cancelled", byError, res.Outcome)
		}
		// The fake ignores ctx, so the write lands here; the assertion below then
		// reads a real row, not the zero value of one never written. Whether it
		// lands on a real pool under a cancelled ctx is NIM-1001.
		if got := fdb.targetStatus("h"); got != string(voyage.TargetStatusCancelled) {
			t.Fatalf("byError=%v: terminal write = %q, want cancelled", byError, got)
		}
		if got := fdb.targetReason("h"); got != (voyage.TargetReason{}) {
			t.Errorf("byError=%v: reason = %+v, want none — the Errand was not cancelled, we stopped waiting", byError, got)
		}
	}

	fdb := &fakeDB{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), CommandSpawner: cancelledErrandSpawner{}}
	var fenceLost atomic.Bool
	w.runOneCommand(context.Background(), commandVoyage([]string{"h"}, nil, nil, nil, nil), "h", func() {}, &fenceLost)
	if got := fdb.targetReason("h"); got != (voyage.TargetReason{Code: voyage.TargetErrorCancelled}) {
		t.Errorf("Errand cancelled under a live ctx: reason = %+v, want code cancelled", got)
	}
}

// cancelledErrandSpawner reports an Errand that was itself cancelled.
type cancelledErrandSpawner struct{}

func (cancelledErrandSpawner) SpawnCommand(_ context.Context, _, sid, _, _ string, _ []byte, _ bool) (string, string, string, error) {
	return "er-" + sid, "cancelled", voyage.TargetErrorCancelled, nil
}

func TestCommandReason(t *testing.T) {
	t.Parallel()
	cases := []struct {
		outcome TargetOutcome
		code    string
		want    voyage.TargetReason
	}{
		{OutcomeSucceeded, "", voyage.TargetReason{}},
		{OutcomeSucceeded, voyage.TargetErrorErrandFailed, voyage.TargetReason{}},
		{OutcomeFailed, voyage.TargetErrorErrandFailed, voyage.TargetReason{Code: voyage.TargetErrorErrandFailed}},
		{OutcomeCancelled, voyage.TargetErrorCancelled, voyage.TargetReason{Code: voyage.TargetErrorCancelled}},
	}
	for _, tc := range cases {
		if got := commandReason(tc.outcome, tc.code); got != tc.want {
			t.Errorf("commandReason(%s, %q) = %+v, want %+v", tc.outcome, tc.code, got, tc.want)
		}
	}
}

// Every reason the orchestrator composes itself uses a documented code.
func TestOrchestratorReasonsUseDocumentedCodes(t *testing.T) {
	t.Parallel()
	for _, r := range []voyage.TargetReason{
		ownershipCheckFailedReason("incarnation x"),
		spawnFailureReason(errors.New("x")),
	} {
		if !voyage.IsTargetErrorCode(r.Code) {
			t.Errorf("reason code %q is not in voyage.TargetErrorCodes", r.Code)
		}
	}
}

// A scenario target written after its run was stopped — here a spawn refusal
// arriving after the stop — gets no code. The rule lives in the terminal writer,
// so every path inherits it.
func TestRunOneIncarnation_StoppedRunRecordsNoCode(t *testing.T) {
	t.Parallel()
	locked := voyage.TargetReason{Code: voyage.TargetErrorIncarnationErrorLocked, Message: "locked"}
	fdb := &fakeDB{}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(),
		ScenarioSpawner: &refusingSpawner{refuse: map[string]error{"inc": &SpawnRefusal{Reason: locked}}}, ScenarioAwaiter: &fakeAwaiter{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	w.runOneIncarnation(ctx, scenarioVoyage([]string{"inc"}, nil, nil, nil, nil), "inc")

	if got := fdb.targetStatus("inc"); got == "" {
		t.Fatal("no terminal write recorded")
	}
	if got := fdb.targetReason("inc"); got != (voyage.TargetReason{}) {
		t.Errorf("reason = %+v, want none — the run was stopped", got)
	}
}
