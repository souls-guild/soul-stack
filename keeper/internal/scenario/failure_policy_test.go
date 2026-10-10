package scenario

import (
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/config"
)

func judgeOf(owner string, tasks ...*render.RenderedTask) *stepJudge {
	settings := config.DefaultScenarioSettings()
	settings.OnFailure = &owner
	byIndex := map[int]*render.RenderedTask{}
	for _, t := range tasks {
		byIndex[t.Index] = t
	}
	return &stepJudge{step: 2, owner: settings, byIndex: byIndex, sent: map[string][]*render.RenderedTask{}, judged: map[string]bool{}}
}

func failedRow(sid string, planIndex *int) applyrun.HostStatus {
	summary := "boom"
	return applyrun.HostStatus{SID: sid, Status: applyrun.StatusFailed, Passage: 2, FailedPlanIndex: planIndex, ErrorSummary: &summary}
}

func idx(i int) *int { return &i }

// ★ GUARD (settings.md §4). A host is judged by the radius of the task it failed at;
// a later failure on the same host under `scenario` — a rescue left at the default —
// ends the run although the first was under `host`; the FAILED records the Keeper
// wrote for the rest of a message it never sent are not judged; a failure tied to
// no task takes the barrier owner's radius; a cancel ends the run whatever the
// radius.
//
// Mutations: judge by the first failure only → the rescue case reads `host`; judge
// the Keeper's sibling records → the settled case reads `scenario`; default a
// failure with no task to `scenario` → the owner's `host` is lost.
func TestStepJudge_VerdictFollowsTheFailedTask(t *testing.T) {
	install := &render.RenderedTask{Index: 4, Name: "install", OnFailure: config.OnFailureHost}
	rescue := &render.RenderedTask{Index: 5, Name: "rescue", OnFailure: config.OnFailureScenario}

	j := judgeOf(config.OnFailureScenario, install, rescue)
	if e := j.verdict(failedRow("a", idx(4))); e.radius != config.OnFailureHost || e.task != "install" {
		t.Errorf("first failure under host = %+v, want host at install", e)
	}

	j.failedAt = map[string]map[int]bool{"b": {4: true, 5: true}}
	if e := j.verdict(failedRow("b", idx(4))); e.radius != config.OnFailureScenario || e.task != "rescue" {
		t.Errorf("a scenario-radius rescue failing after = %+v, want scenario at rescue", e)
	}

	j.settled = map[string]*settledHost{"c": {failed: true, records: []keeperRecord{
		{task: install, status: keeperv1.TaskStatus_TASK_STATUS_FAILED},
		{task: rescue, status: keeperv1.TaskStatus_TASK_STATUS_FAILED},
	}}}
	if e := j.verdict(failedRow("c", nil)); e.radius != config.OnFailureHost || e.task != "install" {
		t.Errorf("a host the Keeper failed = %+v, want judged by the cause alone", e)
	}

	owner := judgeOf(config.OnFailureHost, install)
	if e := owner.verdict(failedRow("d", nil)); e.radius != config.OnFailureHost || e.task != "" {
		t.Errorf("a failure tied to no task = %+v, want the owner's host", e)
	}

	cancelled := applyrun.HostStatus{SID: "e", Status: applyrun.StatusCancelled, Passage: 2}
	if e := owner.verdict(cancelled); e.radius != radiusCancelled {
		t.Errorf("a cancelled row = %+v, want %s", e, radiusCancelled)
	}
}

// ★ GUARD (settings.md §4). The failure list keeps every entry: the error that ends
// the run names the hosts dropped before it as well, and once closed it takes no more.
func TestRunFailures_EndingErrorListsEveryEntry(t *testing.T) {
	f := newRunFailures()
	owner := config.DefaultScenarioSettings()
	host := config.OnFailureHost
	owner.OnFailure = &host

	roster := []*topology.HostFacts{{SID: "a"}, {SID: "b"}, {SID: "c"}}
	drops, err := f.settle(0, []failureEntry{{step: 0, sid: "a", radius: config.OnFailureHost, reason: "first"}}, owner, 3, roster)
	if err != nil || len(drops) != 1 || f.dropped["a"].facts != roster[0] {
		t.Fatalf("settle(host) = %v, %v, dropped %v, want host a dropped with its facts", drops, err, f.dropped)
	}

	_, err = f.settle(1, []failureEntry{
		{step: 1, sid: "b", radius: config.OnFailureScenario, reason: "second"},
		{step: 1, sid: "c", radius: config.OnFailureHost, reason: "third"},
	}, owner, 3, roster)
	if !errors.Is(err, errRunEnded) || !f.ended || f.endedAt != 1 {
		t.Fatalf("a scenario-radius failure = %v (ended %v at %d), want the run ended at step 1", err, f.ended, f.endedAt)
	}
	if _, gone := f.dropped["c"]; gone {
		t.Error("a host-radius failure beside the one that ended the run was committed as a drop")
	}

	end := f.withEnd(err)
	for _, want := range []string{"step 1 failed", "host a", "host b", "host c"} {
		if !strings.Contains(end.Error(), want) {
			t.Errorf("ending error lacks %q: %v", want, end)
		}
	}
	if err := f.append(failureEntry{sid: "d"}); err == nil {
		t.Error("a closed failure list took another entry")
	}
}

// A dropped host stays out of a roster read again later.
func TestRunFailures_LiveDropsTheDroppedFromAFreshRoster(t *testing.T) {
	f := newRunFailures()
	f.dropped["b"] = droppedHost{facts: &topology.HostFacts{SID: "b"}}
	got := f.live([]*topology.HostFacts{{SID: "a"}, {SID: "b"}, {SID: "c"}})
	if len(got) != 2 || got[0].SID != "a" || got[1].SID != "c" {
		t.Errorf("live = %v, want [a c]", got)
	}
	var none *runFailures
	if roster := []*topology.HostFacts{{SID: "a"}}; len(none.live(roster)) != 1 {
		t.Error("a staged run's nil list filtered the roster")
	}
}

// ★ GUARD (settings.md §2.1). `tolerate` is checked at every barrier, one with no new
// failure included: a step that tightens it ends a run whose earlier drops it no
// longer allows.
//
// Mutation: return from settle when the barrier brought no verdict → the run goes
// on past the tightened step.
func TestRunFailures_TolerateIsCheckedAtABarrierWithNoFailure(t *testing.T) {
	f := newRunFailures()
	roster := []*topology.HostFacts{{SID: "a"}, {SID: "b"}, {SID: "c"}}
	loose := config.DefaultScenarioSettings()
	host := config.OnFailureHost
	loose.OnFailure = &host
	loose.Tolerate = &config.Tolerate{N: 1}
	if _, err := f.settle(0, []failureEntry{{step: 0, sid: "a", radius: config.OnFailureHost, reason: "first"}}, loose, 3, roster); err != nil {
		t.Fatalf("one drop under tolerate: 1 ended the run: %v", err)
	}
	if _, err := f.settle(1, nil, loose, 3, roster); err != nil {
		t.Fatalf("a quiet barrier under the same tolerate ended the run: %v", err)
	}
	tight := loose
	tight.Tolerate = &config.Tolerate{N: 0}
	if _, err := f.settle(2, nil, tight, 3, roster); !errors.Is(err, errRunEnded) || !strings.Contains(f.withEnd(err).Error(), "host a") {
		t.Errorf("a quiet barrier under tolerate: 0 after one drop = %v, want the run ended naming host a", err)
	}
}
