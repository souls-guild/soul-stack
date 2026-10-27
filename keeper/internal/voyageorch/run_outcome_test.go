package voyageorch

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
)

// TestOutcomeOfRun_EveryRunStatusHasADecidedOutcome — OutcomeOfRun is the one
// place a run status becomes a Voyage outcome. A value added to RunStatuses
// without a row here goes red instead of quietly taking the default branch.
func TestOutcomeOfRun_EveryRunStatusHasADecidedOutcome(t *testing.T) {
	type res struct {
		outcome TargetOutcome
		done    bool
	}
	want := map[applyrun.RunStatus]res{
		applyrun.RunStatusApplying:      {"", false},
		applyrun.RunStatusSuccess:       {OutcomeSucceeded, true},
		applyrun.RunStatusFailed:        {OutcomeFailed, true},
		applyrun.RunStatusPartialFailed: {OutcomeFailed, true},
		applyrun.RunStatusCancelled:     {OutcomeCancelled, true},
	}
	for _, s := range applyrun.RunStatuses() {
		w, ok := want[s]
		if !ok {
			t.Errorf("run status %q: no decided Voyage outcome", s)
			continue
		}
		if o, done := OutcomeOfRun(s); o != w.outcome || done != w.done {
			t.Errorf("OutcomeOfRun(%q) = (%q, %v), want (%q, %v)", s, o, done, w.outcome, w.done)
		}
	}
	if len(want) != len(applyrun.RunStatuses()) {
		t.Errorf("table has %d rows for %d run statuses", len(want), len(applyrun.RunStatuses()))
	}
	if o, done := OutcomeOfRun("no_such_status"); o != OutcomeFailed || !done {
		t.Errorf("unknown run status = (%q, %v), want fail-closed (failed, true)", o, done)
	}
}

// TestOutcomeOfRun_OverHostRows pins the production awaiter's verdicts, which it
// used to compute with a fold of its own: host rows → AggregateRunStatus →
// OutcomeOfRun must answer as that fold did.
func TestOutcomeOfRun_OverHostRows(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rows    []applyrun.Status
		outcome TargetOutcome
		done    bool
	}{
		{"no rows yet", nil, "", false},
		{"still running", []applyrun.Status{applyrun.StatusSuccess, applyrun.StatusRunning}, "", false},
		{"dispatched dominates failure", []applyrun.Status{applyrun.StatusFailed, applyrun.StatusDispatched}, "", false},
		{"all success", []applyrun.Status{applyrun.StatusSuccess, applyrun.StatusSuccess}, OutcomeSucceeded, true},
		{"success + no_match", []applyrun.Status{applyrun.StatusSuccess, applyrun.StatusNoMatch}, OutcomeSucceeded, true},
		{"only no_match", []applyrun.Status{applyrun.StatusNoMatch}, OutcomeSucceeded, true},
		{"one failed", []applyrun.Status{applyrun.StatusSuccess, applyrun.StatusFailed}, OutcomeFailed, true},
		{"orphaned", []applyrun.Status{applyrun.StatusSuccess, applyrun.StatusOrphaned}, OutcomeFailed, true},
		{"failed beats cancelled", []applyrun.Status{applyrun.StatusCancelled, applyrun.StatusFailed}, OutcomeFailed, true},
		{"cancelled", []applyrun.Status{applyrun.StatusSuccess, applyrun.StatusCancelled}, OutcomeCancelled, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, done := OutcomeOfRun(applyrun.AggregateRunStatus(tc.rows, 0, 0, false))
			if o != tc.outcome || done != tc.done {
				t.Errorf("rows %v → (%q, %v), want (%q, %v)", tc.rows, o, done, tc.outcome, tc.done)
			}
		})
	}
}

// runStatusAwaiter answers like the production awaiter does: from the run status
// of each apply_id, through OutcomeOfRun. Unlisted runs succeeded.
type runStatusAwaiter struct {
	statuses map[string]applyrun.RunStatus
}

func (a *runStatusAwaiter) Await(_ context.Context, applyID string) (TargetOutcome, error) {
	s, ok := a.statuses[applyID]
	if !ok {
		s = applyrun.RunStatusSuccess
	}
	o, _ := OutcomeOfRun(s)
	return o, nil
}

// TestExecuteScenario_PartialFailedRunCountsAsFailure — the decision the user made
// for a run that finished without some hosts: the Voyage counts it as failed. The
// counter and the target row are what an operator reads, and fail_threshold reads
// the same count.
func TestExecuteScenario_PartialFailedRunCountsAsFailure(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	sp := &fakeSpawner{}
	aw := &runStatusAwaiter{statuses: map[string]applyrun.RunStatus{"ap-b": applyrun.RunStatusPartialFailed}}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), ScenarioSpawner: sp, ScenarioAwaiter: aw}

	cont := voyage.OnFailureContinue
	v := scenarioVoyage([]string{"a", "b", "c"}, nil, nil, &cont, nil)
	status, summary, _ := w.executeScenarioVoyage(context.Background(), v, make(chan struct{}))

	if summary.Failed != 1 || summary.Succeeded != 2 || summary.Cancelled != 0 {
		t.Errorf("summary = %+v, want the partial_failed run counted as the one failure", summary)
	}
	if got := fdb.targetStatus("b"); got != string(voyage.TargetStatusFailed) {
		t.Errorf("target b = %q, want failed", got)
	}
	if status != voyage.StatusPartialFailed {
		t.Errorf("voyage status = %q, want partial_failed (one of three failed)", status)
	}
}
