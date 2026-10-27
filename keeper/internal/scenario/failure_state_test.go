package scenario

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

func failedKey(sid string, planIndex int) auditpg.ChangedTaskKey {
	return auditpg.ChangedTaskKey{SID: sid, PlanIndex: planIndex}
}

func judgedRow(step int, sid string, first *int, reason string) applyrun.HostStatus {
	return applyrun.HostStatus{SID: sid, Status: applyrun.StatusFailed, Passage: step, FailedPlanIndex: first, ErrorSummary: &reason}
}

func okRow(step int, sid string) applyrun.HostStatus {
	return applyrun.HostStatus{SID: sid, Status: applyrun.StatusSuccess, Passage: step}
}

// rosterOf is one host per row, with no labels.
func rosterOf(rows []applyrun.HostStatus) []*topology.HostFacts {
	out := make([]*topology.HostFacts, 0, len(rows))
	for _, hs := range rows {
		out = append(out, &topology.HostFacts{SID: hs.SID})
	}
	return out
}

// barrierRun drives a linear run's barriers as judgeWave meets them: the audit log's
// facts accumulate across steps, and every host is sent the step's tasks.
type barrierRun struct {
	f                *runFailures
	failed, executed map[auditpg.ChangedTaskKey]struct{}
}

func newBarrierRun(covers []render.RescueCover, steps []config.StepNode) *barrierRun {
	f := newRunFailures()
	f.setPlan(covers, steps)
	return &barrierRun{f: f, failed: map[auditpg.ChangedTaskKey]struct{}{}, executed: map[auditpg.ChangedTaskKey]struct{}{}}
}

// step is a judge of step under owner, every host of sids sent tasks.
func (b *barrierRun) step(step int, owner string, sids []string, tasks ...*render.RenderedTask) *stepJudge {
	j := judgeOf(owner, tasks...)
	j.step = step
	for _, sid := range sids {
		j.sent[sid] = tasks
	}
	return j
}

// ran records that sid executed idx, failing it when fail.
func (b *barrierRun) ran(sid string, idx int, fail bool) {
	b.executed[failedKey(sid, idx)] = struct{}{}
	if fail {
		b.failed[failedKey(sid, idx)] = struct{}{}
	}
}

func (b *barrierRun) judge(j *stepJudge, rows []applyrun.HostStatus, hosts []*topology.HostFacts, last bool) []failureEntry {
	j.failedAt, j.executed = failedTasksByHost(maps.Clone(b.failed)), failedTasksByHost(maps.Clone(b.executed))
	if hosts == nil {
		hosts = rosterOf(rows)
	}
	return b.f.barrier(j, rows, hosts, last)
}

// ★ GUARD (ADR-0091 amendment NIM-993, "What the run goroutine keeps"). In a plan
// with no rescue unit ahead of a failure every barrier judges each source where it
// failed, so the sources no unit handled are the run's FAILED facts of the tasks that
// are sources — what the requisite gate and rescue routing read from the audit log
// before. That holds for every task a Soul reported, every record the Keeper wrote
// FAILED for a message it never sent (the verdict names the cause alone), the record
// written for a host that stopped reporting, and a step judged wave by wave. An
// applier's terminal and a rescue unit's task are no source: the terminal's FAILED
// record stands for its children, and a unit's failure fails its unit (rule 1).
//
// Mutations: record only the task the entry names (newSources over the reported
// failure alone) → the Soul's second failure and the Keeper's sibling are missing;
// read no record written for a host that stopped reporting (failedOn without
// j.recorded) → its source is missing; hold a unit task's failure on the rule
// (RescueRule.Fail without its unit case) and count it a source in replay → undo is
// a source.
func TestFailureState_UnhandledKeysAreTheFailedSources(t *testing.T) {
	deploy := &render.RenderedTask{Index: 0, Name: "deploy", Register: "deploy"}
	check := &render.RenderedTask{Index: 1, Name: "check", Register: "check"}
	issue := &render.RenderedTask{Index: 2, Name: "issue", Passage: 1}
	certs := &render.RenderedTask{Index: 3, Name: "certs", Register: "certs", Passage: 1, AggregateOf: []int{2}}
	undo := &render.RenderedTask{Index: 4, Name: "undo", Passage: 2, OnFailIdx: []int{0}, RescueUnit: "2"}
	step0 := []*render.RenderedTask{deploy, check}

	// barrier is one wave's barrier as judgeWave meets it.
	type barrier struct {
		step  int
		tasks []*render.RenderedTask
		// written — the FAILED or TIMED_OUT facts the step's hosts, or the Keeper for
		// them, wrote before the barrier.
		written []auditpg.ChangedTaskKey
		rows    []applyrun.HostStatus
		settled map[string]*settledHost
		// stopped — the FAILED records recordUnreported wrote at the barrier.
		stopped []auditpg.ChangedTaskKey
	}
	cases := []struct {
		name     string
		barriers []barrier
		want     []auditpg.ChangedTaskKey
	}{
		{name: "every task a Soul reported failed", barriers: []barrier{{
			step: 0, tasks: step0,
			written: []auditpg.ChangedTaskKey{failedKey("a", 0), failedKey("a", 1)},
			rows:    []applyrun.HostStatus{judgedRow(0, "a", idx(0), "task 0 core.exec.run: boom")},
		}}, want: []auditpg.ChangedTaskKey{failedKey("a", 0), failedKey("a", 1)}},
		{name: "every record the Keeper wrote FAILED for a message it never sent", barriers: []barrier{{
			step: 0, tasks: step0,
			written: []auditpg.ChangedTaskKey{failedKey("b", 0), failedKey("b", 1)},
			rows:    []applyrun.HostStatus{judgedRow(0, "b", idx(1), "when failed")},
			settled: map[string]*settledHost{"b": {failed: true, records: []keeperRecord{
				{task: check, localIdx: 1, status: keeperv1.TaskStatus_TASK_STATUS_FAILED},
				{task: deploy, status: keeperv1.TaskStatus_TASK_STATUS_FAILED},
			}}},
		}}, want: []auditpg.ChangedTaskKey{failedKey("b", 0), failedKey("b", 1)}},
		{name: "the record written for a host that stopped reporting", barriers: []barrier{{
			step: 0, tasks: step0,
			rows:    []applyrun.HostStatus{judgedRow(0, "c", nil, "soul_unavailable: agent stream lost")},
			stopped: []auditpg.ChangedTaskKey{failedKey("c", 0)},
		}}, want: []auditpg.ChangedTaskKey{failedKey("c", 0)}},
		{name: "an applier's child, its terminal aside", barriers: []barrier{{
			step: 1, tasks: []*render.RenderedTask{issue, certs},
			written: []auditpg.ChangedTaskKey{failedKey("a", 2), failedKey("a", 3)},
			rows:    []applyrun.HostStatus{judgedRow(1, "a", idx(2), "task 0 core.exec.run: boom")},
		}}, want: []auditpg.ChangedTaskKey{failedKey("a", 2)}},
		{name: "a rescue unit's task failing at a later step", barriers: []barrier{
			{
				step: 0, tasks: step0,
				written: []auditpg.ChangedTaskKey{failedKey("a", 0)},
				rows:    []applyrun.HostStatus{judgedRow(0, "a", idx(0), "task 0 core.exec.run: boom")},
			},
			{
				step: 2, tasks: []*render.RenderedTask{undo},
				written: []auditpg.ChangedTaskKey{failedKey("a", 4)},
				rows:    []applyrun.HostStatus{judgedRow(2, "a", idx(4), "task 0 core.exec.run: undo failed")},
			},
		}, want: []auditpg.ChangedTaskKey{failedKey("a", 0)}},
		{name: "a step judged wave by wave", barriers: []barrier{
			{
				step: 0, tasks: step0,
				written: []auditpg.ChangedTaskKey{failedKey("a", 0)},
				rows:    []applyrun.HostStatus{judgedRow(0, "a", idx(0), "task 0 core.exec.run: boom")},
			},
			{
				step: 0, tasks: step0,
				written: []auditpg.ChangedTaskKey{failedKey("b", 0), failedKey("b", 1)},
				rows: []applyrun.HostStatus{
					judgedRow(0, "a", idx(0), "task 0 core.exec.run: boom"),
					judgedRow(0, "b", idx(0), "task 0 core.exec.run: boom"),
				},
			},
		}, want: []auditpg.ChangedTaskKey{failedKey("a", 0), failedKey("b", 0), failedKey("b", 1)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunFailures()
			written := map[auditpg.ChangedTaskKey]struct{}{}
			var j *stepJudge
			for _, b := range tc.barriers {
				for _, k := range b.written {
					written[k] = struct{}{}
				}
				if j == nil || j.step != b.step {
					j = judgeOf(config.OnFailureHost, b.tasks...)
					j.step, j.settled = b.step, b.settled
				}
				for _, hs := range b.rows {
					j.sent[hs.SID] = b.tasks
				}
				j.failedAt = failedTasksByHost(maps.Clone(written))
				for _, k := range b.stopped {
					j.recorded[k.SID] = map[int]bool{k.PlanIndex: true}
				}
				f.barrier(j, b.rows, rosterOf(b.rows), true)
			}
			last := tc.barriers[len(tc.barriers)-1].step
			if got, want := f.view(last+1).unhandledKeys(), keySet(tc.want...); !reflect.DeepEqual(got, want) {
				t.Errorf("unhandled sources = %v, want %v", got, want)
			}
		})
	}
}

// sidFailingAuditWriter refuses every event about host sid and writes the rest.
type sidFailingAuditWriter struct {
	sid     string
	written []*audit.Event
}

func (w *sidFailingAuditWriter) Write(_ context.Context, e *audit.Event) error {
	if e.Payload["sid"] == w.sid {
		return errors.New("audit log unavailable")
	}
	w.written = append(w.written, e)
	return nil
}

// ★ GUARD (ADR-0091 amendment NIM-993, "What the run goroutine keeps"). Of the FAILED
// records written for a host that stopped reporting, only those the audit log holds
// become sources: the run goroutine's requisite gate reads the sources and a claim's
// reads the audit log, and a record whose write was lost would fire a rescue in one
// and not in the other. A host whose row names its failed task, one the audit log
// holds a FAILED fact of the step for, and one the Keeper failed before sending have
// a failure tied to a task: nothing is written for them.
//
// Mutations: return every FAILED record whatever became of its write → d's record is
// returned under the failing writer, and both are returned with no writer; report a
// failed audit write as written (applysink) → d's record is returned; report a write
// skipped for want of a writer as written → both are returned with no writer; record a
// host whatever its row names (no reportedFailure/failedAtStep/keeperFailed check) →
// e, g and k are returned.
func TestRecordUnreported_ReturnsOnlyTheRecordsTheAuditLogHolds(t *testing.T) {
	deploy := &render.RenderedTask{Index: 0, Name: "deploy", Register: "deploy"}
	check := &render.RenderedTask{Index: 1, Name: "check", Register: "check"}
	for _, tc := range []struct {
		name   string
		writer audit.Writer
		want   []auditpg.ChangedTaskKey
	}{
		{name: "every write succeeds", writer: &sidFailingAuditWriter{}, want: []auditpg.ChangedTaskKey{failedKey("c", 0), failedKey("d", 0)}},
		{name: "the writes about d fail", writer: &sidFailingAuditWriter{sid: "d"}, want: []auditpg.ChangedTaskKey{failedKey("c", 0)}},
		{name: "no audit writer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runCompletedRunner(tc.writer, &fakeChangedReader{})
			j := judgeOf(config.OnFailureHost, deploy, check)
			j.step = 0
			rows := []applyrun.HostStatus{}
			for _, sid := range []string{"c", "d", "e", "g", "k"} {
				j.sent[sid] = []*render.RenderedTask{deploy, check}
				rows = append(rows, judgedRow(0, sid, nil, "soul_unavailable: agent stream lost"))
			}
			rows[2].FailedPlanIndex = idx(1)
			j.failedAt = map[string]map[int]bool{"g": {1: true}}
			j.settled = map[string]*settledHost{"k": {failed: true, records: []keeperRecord{{task: deploy, status: keeperv1.TaskStatus_TASK_STATUS_FAILED}}}}
			got := map[auditpg.ChangedTaskKey]struct{}{}
			for sid, idxs := range r.recordUnreported(t.Context(), runCompletedSpec(), r.logger, j, rows) {
				for i := range idxs {
					got[failedKey(sid, i)] = struct{}{}
				}
			}
			if want := keySet(tc.want...); !reflect.DeepEqual(got, want) {
				t.Errorf("FAILED records returned = %v, want %v", got, want)
			}
		})
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 4). A source a Soul reported failed is
// judged by its task's own `on_failure`, whatever radius the host's verdict took, also
// on a host the Keeper skipped a task of. A host judged as a whole — one that stopped
// reporting, or one the Keeper failed before sending, which is judged by the cause
// alone — records every source by the radius its verdict took: the barrier owner's
// for a host that stopped reporting, the cause's for k and m, whose own `on_failure`
// is not the owner's. That holds for a sibling carrying the cause's failure and for
// one that failed there before dispatch of its own, and a sibling under a stricter
// radius than the cause's is judged by the cause's.
//
// Mutations: take the barrier owner's radius in place of the cause's for a host the
// Keeper failed (whole set to the owner's) → `migrate` is under `host` on k; take each
// task's own radius on a host the Keeper failed (whole not read) → `reload` and
// `warm` are under `host` on k, `deploy` under `scenario` on m; take the task's own
// radius for a record written for a host that stopped reporting (newSources without
// the recorded case) → `deploy` is under `scenario` there.
func TestFailureState_ASourceTakesItsRadius(t *testing.T) {
	deploy := &render.RenderedTask{Index: 0, Name: "deploy", Register: "deploy", OnFailure: config.OnFailureScenario}
	check := &render.RenderedTask{Index: 1, Name: "check", Register: "check"}
	cause := &render.FlowFailure{Predicate: "when", Code: "when_eval_failed", Message: "no such key"}
	migrate := &render.RenderedTask{Index: 2, Name: "migrate", Register: "migrate", OnFailure: config.OnFailureScenario,
		FlowBySID: map[string]render.HostFlow{"k": {Failure: cause}}}
	reload := &render.RenderedTask{Index: 3, Name: "reload", Register: "reload"}
	warm := &render.RenderedTask{Index: 4, Name: "warm", Register: "warm", OnFailure: config.OnFailureHost,
		FlowBySID: map[string]render.HostFlow{"k": {Failure: &render.FlowFailure{Predicate: "failed_when", Code: "predicate_render_failed", Message: "no such key"}}}}
	sibling := &render.FlowFailure{Predicate: "when", Code: "when_eval_failed", Message: `task "migrate" failed on this host before dispatch: no such key`}
	checkSibling := &render.FlowFailure{Predicate: "when", Code: "when_eval_failed", Message: `task "check" failed on this host before dispatch: no such key`}
	for _, tc := range []struct {
		name    string
		row     applyrun.HostStatus
		sent    []*render.RenderedTask
		written []auditpg.ChangedTaskKey
		settled map[string]*settledHost
		stopped []auditpg.ChangedTaskKey
		want    map[int]string
	}{
		{
			name:    "every task the Soul reported failed",
			row:     judgedRow(0, "a", idx(0), "task 0 core.exec.run: boom"),
			sent:    []*render.RenderedTask{deploy, check},
			written: []auditpg.ChangedTaskKey{failedKey("a", 0), failedKey("a", 1)},
			want:    map[int]string{0: config.OnFailureScenario, 1: config.OnFailureHost},
		},
		{
			name:    "every task a Soul reported failed, with a task the Keeper skipped on the host",
			row:     judgedRow(0, "s", idx(0), "task 0 core.exec.run: boom"),
			sent:    []*render.RenderedTask{deploy, check},
			written: []auditpg.ChangedTaskKey{failedKey("s", 0), failedKey("s", 1)},
			settled: map[string]*settledHost{"s": {records: []keeperRecord{
				{task: reload, status: keeperv1.TaskStatus_TASK_STATUS_SKIPPED},
			}}},
			want: map[int]string{0: config.OnFailureScenario, 1: config.OnFailureHost},
		},
		{
			name:    "the record written for a host that stopped reporting",
			row:     judgedRow(0, "a", nil, "soul_unavailable: agent stream lost"),
			sent:    []*render.RenderedTask{deploy, check},
			stopped: []auditpg.ChangedTaskKey{failedKey("a", 0)},
			want:    map[int]string{0: config.OnFailureHost},
		},
		{
			name:    "every record the Keeper wrote FAILED for a message it never sent",
			row:     judgedRow(0, "k", idx(2), "when failed"),
			written: []auditpg.ChangedTaskKey{failedKey("k", 2), failedKey("k", 3), failedKey("k", 4)},
			settled: map[string]*settledHost{"k": {failed: true, records: []keeperRecord{
				{task: migrate, status: keeperv1.TaskStatus_TASK_STATUS_FAILED, failure: cause},
				{task: reload, localIdx: 1, status: keeperv1.TaskStatus_TASK_STATUS_FAILED, failure: sibling},
				{task: warm, localIdx: 2, status: keeperv1.TaskStatus_TASK_STATUS_FAILED, failure: warm.FlowBySID["k"].Failure},
			}}},
			want: map[int]string{2: config.OnFailureScenario, 3: config.OnFailureScenario, 4: config.OnFailureScenario},
		},
		{
			name:    "a cause under host with a sibling under scenario",
			row:     judgedRow(0, "m", idx(1), "when failed"),
			written: []auditpg.ChangedTaskKey{failedKey("m", 0), failedKey("m", 1)},
			settled: map[string]*settledHost{"m": {failed: true, records: []keeperRecord{
				{task: check, localIdx: 1, status: keeperv1.TaskStatus_TASK_STATUS_FAILED, failure: cause},
				{task: deploy, status: keeperv1.TaskStatus_TASK_STATUS_FAILED, failure: checkSibling},
			}}},
			want: map[int]string{0: config.OnFailureHost, 1: config.OnFailureHost},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := judgeOf(config.OnFailureHost, deploy, check, migrate, reload, warm)
			j.step, j.settled = 0, tc.settled
			j.sent[tc.row.SID] = tc.sent
			j.failedAt = failedTasksByHost(keySet(tc.written...))
			for _, k := range tc.stopped {
				j.recorded[k.SID] = map[int]bool{k.PlanIndex: true}
			}
			f := newRunFailures()
			f.barrier(j, []applyrun.HostStatus{tc.row}, rosterOf([]applyrun.HostStatus{tc.row}), true)
			got := map[int]string{}
			for k, s := range f.sources {
				got[k.index] = s.radius
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("radius by plan index = %v, want %v", got, tc.want)
			}
		})
	}
}

// unitPlan is a linear plan of five steps — deploy, reload, two rescue units of
// deploy, warm — with the units' covers.
func unitPlan() (steps []config.StepNode, deploy, reload, u1, u2, warm *render.RenderedTask, covers []render.RescueCover) {
	steps = []config.StepNode{{Step: 0}, {Step: 1}, {Step: 2}, {Step: 3}, {Step: 4}}
	deploy = &render.RenderedTask{Index: 0, Name: "deploy", Register: "deploy"}
	reload = &render.RenderedTask{Index: 1, Name: "reload", Register: "reload", Passage: 1}
	u1 = &render.RenderedTask{Index: 2, Name: "u1", Passage: 2, OnFailIdx: []int{0}, RescueUnit: "2"}
	u2 = &render.RenderedTask{Index: 3, Name: "u2", Passage: 3, OnFailIdx: []int{0}, RescueUnit: "3"}
	warm = &render.RenderedTask{Index: 4, Name: "warm", Passage: 4}
	covers = []render.RescueCover{
		{Path: "2", Sources: []string{"deploy"}, First: 2, Last: 2, RescueUnit: "2", DecidedAt: 2},
		{Path: "3", Sources: []string{"deploy"}, First: 3, Last: 3, RescueUnit: "3", DecidedAt: 3},
	}
	return steps, deploy, reload, u1, u2, warm, covers
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 2 and the net of rule 6). At the barrier
// of the step a host failed at, its failures are held only if all of them are: each
// with a rescue unit ahead whose `on:` chain selects the host. A group rolled as one
// that the Keeper failed before sending is judged at once by its first source among its
// records, a task of a unit that fires there skipped, while a one-task step it failed
// is held; a group of only unit tasks it failed, the unit firing there, fails its unit
// and is judged by nothing. Where the cause's unit does not fire — its source the
// sibling the Keeper recorded FAILED after it, or none held at all — the cause is a
// source like any other, and the host is judged by it, under its own radius; that unit,
// decided at the group's barrier, handles nothing, so the sibling stays judged. A
// `loop:`'s failed iterations are one source, held together. A host that stopped
// reporting is held for the task its records mark FAILED, under the barrier owner's
// radius.
//
// Mutations: hold a host when its first failure has a unit ahead (decideHost reading
// the first alone) → "a second failure with no unit" is held; read no cover's `on:`
// (drop HostCovered) → "a unit whose on: does not select the host" is held; count a
// unit at the step itself as ahead (c.Last < step) → "a unit at the source's own
// step" is held; hold a group the Keeper failed (no keeperFailedGroup check) → "a
// group the Keeper failed" is held; name the cause whatever it is (judgedTasks taking
// records[0]) → it names `u`; replay a host the Keeper failed in plan order, its cause
// not first → in "a group the Keeper failed by a unit that did not fire" `u` is its
// unit's failure and no source; make every unit task's failure its unit's
// (RescueRule.Fail without the Fires check) → the same, and "a group of only unit
// tasks the Keeper failed, the unit not firing" is left no source; decide that unit as
// one that executed and did not fail (RescueRule.Decide not reading lost) → it handles
// `deploy` there, which is no longer judged; key a loop's
// iterations apart (no byRegister) → two sources; take the task's own radius for a host
// that stopped reporting → `deploy` is held under `scenario`.
func TestFailureState_DecideHost(t *testing.T) {
	steps, deploy, _, _, _, _, covers := unitPlan()
	migrate := &render.RenderedTask{Index: 5, Name: "migrate", Register: "migrate"}
	unitChild := &render.RenderedTask{Index: 6, Name: "u", Register: "u", OnFailIdx: []int{9}, RescueUnit: "0/1", Grouped: true}
	pairChild := &render.RenderedTask{Index: 6, Name: "u", Register: "u", OnFailIdx: []int{0}, RescueUnit: "0/1", Grouped: true, OnFailure: config.OnFailureScenario}
	groupDeploy := *deploy
	groupDeploy.Grouped = true
	iter0 := &render.RenderedTask{Index: 7, Name: "each", Register: "each"}
	iter1 := &render.RenderedTask{Index: 8, Name: "each", Register: "each"}
	scenarioDeploy := *deploy
	scenarioDeploy.OnFailure = config.OnFailureScenario
	eachCover := render.RescueCover{Path: "2", Sources: []string{"each"}, First: 2, Last: 2, RescueUnit: "2", DecidedAt: 2}
	web := render.RescueCover{Path: "3", Sources: []string{"migrate"}, First: 3, Last: 3, RescueUnit: "3", DecidedAt: 3, Covens: []string{"web"}}
	own := render.RescueCover{Path: "0", Sources: []string{"migrate"}, First: 0, Last: 0, RescueUnit: "0", DecidedAt: 0}
	// Step 0 a group rolled as one, so the in-group unit "0/1" is decided at its barrier.
	groupSteps := slices.Clone(steps)
	groupSteps[0] = config.StepNode{Step: 0, Group: true}
	rec := func(t *render.RenderedTask, local int) keeperRecord {
		return keeperRecord{task: t, localIdx: local, status: keeperv1.TaskStatus_TASK_STATUS_FAILED}
	}
	type want struct {
		held      bool
		lastUnit  int
		entryTask string
		radius    string
		sources   int
		unitFails bool
	}
	for _, tc := range []struct {
		name    string
		tasks   []*render.RenderedTask
		covers  []render.RescueCover
		failed  []int
		row     applyrun.HostStatus
		settled *settledHost
		stopped []int
		// heldBefore — positions held on the host's rule from an earlier step.
		heldBefore []int
		// steps — the plan's step shape; nil for unitPlan's.
		steps []config.StepNode
		want  want
	}{
		{name: "a source with a unit ahead", tasks: []*render.RenderedTask{deploy}, covers: covers, failed: []int{0},
			row: judgedRow(0, "h", idx(0), "boom"), want: want{held: true, lastUnit: 3, sources: 1}},
		{name: "a second failure with no unit", tasks: []*render.RenderedTask{deploy, migrate}, covers: covers, failed: []int{0, 5},
			row: judgedRow(0, "h", idx(0), "boom"), want: want{entryTask: "deploy", radius: config.OnFailureHost, sources: 2}},
		{name: "a unit whose on: does not select the host", tasks: []*render.RenderedTask{migrate}, covers: []render.RescueCover{web}, failed: []int{5},
			row: judgedRow(0, "h", idx(5), "boom"), want: want{entryTask: "migrate", radius: config.OnFailureHost, sources: 1}},
		{name: "a unit at the source's own step", tasks: []*render.RenderedTask{migrate}, covers: []render.RescueCover{own}, failed: []int{5},
			row: judgedRow(0, "h", idx(5), "boom"), want: want{entryTask: "migrate", radius: config.OnFailureHost, sources: 1}},
		{name: "a group the Keeper failed", tasks: []*render.RenderedTask{&groupDeploy, unitChild}, covers: covers,
			row:        judgedRow(0, "h", idx(6), "when failed"),
			settled:    &settledHost{failed: true, records: []keeperRecord{rec(unitChild, 1), rec(&groupDeploy, 0)}},
			heldBefore: []int{9},
			want:       want{entryTask: "deploy", radius: config.OnFailureHost, sources: 1, unitFails: true}},
		{name: "a group the Keeper failed by a unit that did not fire", tasks: []*render.RenderedTask{&groupDeploy, pairChild}, covers: covers, steps: groupSteps,
			row:     judgedRow(0, "h", idx(6), "when failed"),
			settled: &settledHost{failed: true, records: []keeperRecord{rec(pairChild, 1), rec(&groupDeploy, 0)}},
			want:    want{entryTask: "u", radius: config.OnFailureScenario, sources: 2}},
		{name: "a one-task step the Keeper failed", tasks: []*render.RenderedTask{deploy}, covers: covers,
			row:     judgedRow(0, "h", idx(0), "when failed"),
			settled: &settledHost{failed: true, records: []keeperRecord{rec(deploy, 0)}},
			want:    want{held: true, lastUnit: 3, sources: 1}},
		{name: "a group of only unit tasks the Keeper failed", tasks: []*render.RenderedTask{unitChild}, covers: covers,
			row:        judgedRow(0, "h", idx(6), "when failed"),
			settled:    &settledHost{failed: true, records: []keeperRecord{rec(unitChild, 0), rec(unitChild, 1)}},
			heldBefore: []int{9},
			want:       want{unitFails: true}},
		{name: "a group of only unit tasks the Keeper failed, the unit not firing", tasks: []*render.RenderedTask{unitChild}, covers: covers,
			row:     judgedRow(0, "h", idx(6), "when failed"),
			settled: &settledHost{failed: true, records: []keeperRecord{rec(unitChild, 0), rec(unitChild, 1)}},
			want:    want{entryTask: "u", radius: config.OnFailureHost, sources: 1}},
		{name: "a loop's failed iterations", tasks: []*render.RenderedTask{iter0, iter1}, covers: []render.RescueCover{eachCover}, failed: []int{7, 8},
			row: judgedRow(0, "h", idx(7), "boom"), want: want{held: true, lastUnit: 2, sources: 1}},
		{name: "a host that stopped reporting", tasks: []*render.RenderedTask{&scenarioDeploy}, covers: covers, stopped: []int{0},
			row: judgedRow(0, "h", nil, "soul_unavailable: agent stream lost"), want: want{held: true, lastUnit: 3, sources: 1, radius: config.OnFailureHost}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shape := steps
			if tc.steps != nil {
				shape = tc.steps
			}
			b := newBarrierRun(tc.covers, shape)
			j := b.step(0, config.OnFailureHost, []string{"h"}, tc.tasks...)
			for _, i := range tc.failed {
				b.ran("h", i, true)
			}
			if tc.settled != nil {
				j.settled = map[string]*settledHost{"h": tc.settled}
			}
			for _, i := range tc.stopped {
				j.recorded["h"] = map[int]bool{i: true}
			}
			for _, i := range tc.heldBefore {
				rule := b.f.ruleOf("h")
				rule.Task(i, 0, nil, false, nil)
				rule.Fail(i, "failed earlier")
			}
			entries := b.judge(j, []applyrun.HostStatus{tc.row}, nil, true)
			held := b.f.hosts["h"].openPeriod() != nil
			if held != tc.want.held || len(b.f.sources) != tc.want.sources {
				t.Fatalf("held %v with %d sources, want held %v with %d", held, len(b.f.sources), tc.want.held, tc.want.sources)
			}
			switch {
			case tc.want.held:
				if len(entries) != 0 || b.f.hosts["h"].periods[0].heldFrom != 1 {
					t.Errorf("entries %v, hold %+v - want none, held from step 1", entries, b.f.hosts["h"].periods)
				}
				for _, s := range b.f.sources {
					if s.state != sourceHeld || s.lastUnit == nil || *s.lastUnit != tc.want.lastUnit {
						t.Errorf("source %+v - want held for its last unit, decided at step %d", s, tc.want.lastUnit)
					}
					if tc.want.radius != "" && s.radius != tc.want.radius {
						t.Errorf("held source's radius = %s, want %s", s.radius, tc.want.radius)
					}
				}
			case tc.want.entryTask != "":
				if len(entries) != 1 || entries[0].task != tc.want.entryTask || entries[0].radius != tc.want.radius {
					t.Errorf("entries %v - want one at %s under %s", entries, tc.want.entryTask, tc.want.radius)
				}
				for _, s := range b.f.sources {
					if s.state != sourceJudged {
						t.Errorf("source %s is %v, want judged at once", s.task, s.state)
					}
				}
			default:
				if len(entries) != 0 {
					t.Errorf("entries %v, want none: the failure is its unit's", entries)
				}
			}
			if got := b.f.view(1).unitFailed("h", "0/1"); got != tc.want.unitFails {
				t.Errorf("unit 0/1 failed = %v, want %v", got, tc.want.unitFails)
			}
		})
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, rules 1-4). Through its barriers a held host
// misses the ordinary steps, keeping the register each would have left; a unit that
// fails attaches its error and its row to the source and fails no host by itself; the
// next unit that succeeds handles the source and returns the host after its step, the
// steps it missed still held in the view of each of them, and the source's row and the
// failed unit's are the rows to mark handled (rule 5). When no unit succeeds the
// source is judged at its last unit's barrier, by its own radius, from the held entry:
// under `host` the entry names it with its rescues' errors and the host leaves with
// the source's row and the failed units' rows, not the judging step's, which a unit
// that executed nothing did not fail; under `scenario` the run ends there. A host a
// unit did not reach at its step — a `run_once:` unit went elsewhere — is judged at the
// step's last barrier. A held host
// is no drop for `tolerate`, and a returned host that fails again with a unit ahead is
// held for a period of its own.
//
// Mutations: decide no unit on a host its step did not reach (barrier's last loop
// skipped) → b is never judged; attach no failed unit (attachUnitFailures a no-op) →
// the entry carries no rescue error and a's rows are [0 3]; return a host at its
// unit's own step (rejoin(step)) → a is live at step 3; judge a held source at any
// barrier (sweep reading no lastUnit) → a is judged where it failed; record no
// missed register (recordMissed a no-op) → no `reload` at step 4; count a held host as
// dropped (dropCount counting open hold periods) → tolerate: 0 ends the run; open one
// hold period per host (hold returning once any period exists) → a's second hold is
// lost; mark only a source's own row handled (handledRows over s.rows[:1]) → a's row
// of u1 is left out; mark the judging step dropped (droppedRows adding each judged
// source's changedAt) → a's rows when the last unit executes nothing are [0 2 3].
func TestFailureState_HoldReturnAndJudge(t *testing.T) {
	steps, deploy, reload, u1, u2, _, covers := unitPlan()
	hosts := []*topology.HostFacts{{SID: "a"}, {SID: "b"}}

	// last — what u2 does on a: "handles", "fails", or "skipped", executing nothing.
	run := func(t *testing.T, owner string, last string) (*barrierRun, [][]failureEntry) {
		b := newBarrierRun(covers, steps)
		b.f.useRoster(hosts)
		var got [][]failureEntry
		j := b.step(0, owner, []string{"a", "b"}, deploy)
		b.ran("a", 0, true)
		b.ran("b", 0, false)
		got = append(got, b.judge(j, []applyrun.HostStatus{judgedRow(0, "a", idx(0), "deploy broke"), okRow(0, "b")}, hosts, true))

		j = b.step(1, owner, []string{"b"}, reload)
		b.f.recordMissed(1, []*render.RenderedTask{reload}, []render.DispatchPlan{{TaskIndex: 1, TargetSIDs: []string{"b"}, NotReached: []string{"a"}}})
		b.ran("b", 1, false)
		got = append(got, b.judge(j, []applyrun.HostStatus{okRow(1, "b")}, hosts, true))

		j = b.step(2, owner, []string{"a"}, u1)
		b.ran("a", 2, true)
		got = append(got, b.judge(j, []applyrun.HostStatus{judgedRow(2, "a", idx(2), "u1 broke")}, hosts, true))

		j = b.step(3, owner, []string{"a"}, u2)
		if last != "skipped" {
			b.ran("a", 3, last == "fails")
		}
		row := okRow(3, "a")
		if last == "fails" {
			row = judgedRow(3, "a", idx(3), "u2 broke")
		}
		got = append(got, b.judge(j, []applyrun.HostStatus{row}, hosts, true))
		return b, got
	}

	t.Run("the second unit handles", func(t *testing.T) {
		b, entries := run(t, config.OnFailureHost, "handles")
		for step, e := range entries {
			if len(e) != 0 {
				t.Errorf("step %d judged %v, want nothing: held failures and unit failures are no entries", step, e)
			}
		}
		for step, want := range map[int]string{1: "held", 2: "held", 3: "held", 4: "live"} {
			live, held, _ := b.f.view(step).split()
			got := "live"
			if slices.ContainsFunc(held, func(h *topology.HostFacts) bool { return h.SID == "a" }) {
				got = "held"
			} else if !slices.ContainsFunc(live, func(h *topology.HostFacts) bool { return h.SID == "a" }) {
				got = "absent"
			}
			if got != want {
				t.Errorf("a at step %d is %s, want %s", step, got, want)
			}
		}
		s := b.f.sources[sourceKey{"a", 0}]
		if s.state != sourceHandled || s.changedAt != 3 || !reflect.DeepEqual(s.unitErrs, []string{"u1 broke"}) {
			t.Errorf("deploy on a = %+v, want handled at step 3 with u1's error", s)
		}
		if got := b.f.view(4).rescue().HandledAt; !reflect.DeepEqual(got, map[string]map[string]int{"a": {"deploy": 3}}) {
			t.Errorf("handled at = %v, want deploy on a at step 3", got)
		}
		in := render.RenderInput{Standing: b.f.view(4).standing()}
		if _, ok := in.ForStep(4).RegisterByHost["a"]["reload"]; !ok {
			t.Error("a reads no `reload` at step 4: the register of the step it missed is not synthesized")
		}
		if b.f.anyHeld() {
			t.Error("a source is still held after its unit handled it")
		}
		if got, want := b.f.handledRows(), []runRow{{"a", 0}, {"a", 2}}; !reflect.DeepEqual(got, want) {
			t.Errorf("rows to mark handled = %v, want %v: the source's and its failed unit's", got, want)
		}
	})
	t.Run("no unit handles, under host", func(t *testing.T) {
		b, entries := run(t, config.OnFailureHost, "fails")
		if len(entries[2]) != 0 {
			t.Errorf("step 2 judged %v, want nothing: u2 is ahead", entries[2])
		}
		e := entries[3]
		if len(e) != 1 || e[0].task != "deploy" || e[0].radius != config.OnFailureHost || e[0].failedAt == nil || *e[0].failedAt != 0 ||
			!reflect.DeepEqual(e[0].rescues, []string{"u1 broke", "u2 broke"}) {
			t.Fatalf("step 3 judged %+v, want deploy of step 0 under host with both rescues' errors", e)
		}
		if !strings.Contains(e[0].String(), "deploy broke") || !strings.Contains(e[0].String(), "u2 broke") {
			t.Errorf("entry %q names neither the source's error nor its rescue's", e[0])
		}
		if drops, err := b.f.settle(3, e, hostOwner(), 2, hosts); err != nil || len(drops) != 1 {
			t.Fatalf("settle = %v, %v - want a dropped", drops, err)
		}
		if got := b.f.droppedRows("a"); !reflect.DeepEqual(got, []int{0, 2, 3}) {
			t.Errorf("a's rows to mark dropped = %v, want [0 2 3]", got)
		}
		if got := b.f.handledRows(); len(got) != 0 {
			t.Errorf("rows to mark handled = %v, want none: no unit handled deploy", got)
		}
	})
	t.Run("the last unit executes nothing, under host", func(t *testing.T) {
		b, entries := run(t, config.OnFailureHost, "skipped")
		if len(entries[3]) != 1 || entries[3][0].task != "deploy" {
			t.Fatalf("step 3 judged %+v, want deploy", entries[3])
		}
		if drops, err := b.f.settle(3, entries[3], hostOwner(), 2, hosts); err != nil || len(drops) != 1 {
			t.Fatalf("settle = %v, %v - want a dropped", drops, err)
		}
		if got := b.f.droppedRows("a"); !reflect.DeepEqual(got, []int{0, 2}) {
			t.Errorf("a's rows to mark dropped = %v, want [0 2]: the judging step failed no unit", got)
		}
	})
	t.Run("no unit handles, under scenario", func(t *testing.T) {
		b, entries := run(t, config.OnFailureScenario, "fails")
		owner := config.DefaultScenarioSettings()
		if _, err := b.f.settle(3, entries[3], owner, 2, hosts); !errors.Is(err, errRunEnded) {
			t.Errorf("settle = %v, want the run ended at the last unit's barrier", err)
		}
	})
	t.Run("a held host is not counted by tolerate", func(t *testing.T) {
		b := newBarrierRun(covers, steps)
		j := b.step(0, config.OnFailureHost, []string{"a", "b"}, deploy)
		b.ran("a", 0, true)
		entries := b.judge(j, []applyrun.HostStatus{judgedRow(0, "a", idx(0), "a broke"), okRow(0, "b")}, hosts, true)
		owner := hostOwner()
		owner.Tolerate = &config.Tolerate{N: 0}
		if _, err := b.f.settle(0, entries, owner, 2, hosts); err != nil {
			t.Errorf("settle under tolerate: 0 = %v, want the run going on: a held host is no drop", err)
		}
	})
	t.Run("a returned host is held again", func(t *testing.T) {
		again := []config.StepNode{{Step: 0}, {Step: 1}, {Step: 2}, {Step: 3}}
		first := &render.RenderedTask{Index: 1, Name: "first", Passage: 1, OnFailIdx: []int{0}, RescueUnit: "1"}
		warmed := &render.RenderedTask{Index: 2, Name: "warm", Register: "warm", Passage: 2}
		then := &render.RenderedTask{Index: 3, Name: "then", Passage: 3, OnFailIdx: []int{2}, RescueUnit: "3"}
		b := newBarrierRun([]render.RescueCover{
			{Path: "1", Sources: []string{"deploy"}, First: 1, Last: 1, RescueUnit: "1", DecidedAt: 1},
			{Path: "3", Sources: []string{"warm"}, First: 3, Last: 3, RescueUnit: "3", DecidedAt: 3},
		}, again)
		b.f.useRoster(hosts)
		for _, s := range []struct {
			task *render.RenderedTask
			fail bool
		}{{deploy, true}, {first, false}, {warmed, true}, {then, false}} {
			j := b.step(s.task.Passage, config.OnFailureHost, []string{"a"}, s.task)
			b.ran("a", s.task.Index, s.fail)
			row := okRow(s.task.Passage, "a")
			if s.fail {
				row = judgedRow(s.task.Passage, "a", idx(s.task.Index), s.task.Name+" broke")
			}
			if got := b.judge(j, []applyrun.HostStatus{row}, hosts, true); len(got) != 0 {
				t.Fatalf("step %d judged %v, want nothing", s.task.Passage, got)
			}
		}
		var periods [][2]int
		for _, p := range b.f.hosts["a"].periods {
			periods = append(periods, [2]int{p.heldFrom, *p.returnedAt})
		}
		if want := [][2]int{{1, 2}, {3, 4}}; !reflect.DeepEqual(periods, want) {
			t.Errorf("a's hold periods = %v, want %v", periods, want)
		}
	})
	t.Run("a held host the last unit did not reach", func(t *testing.T) {
		b := newBarrierRun(covers, steps)
		j := b.step(0, config.OnFailureHost, []string{"a", "b"}, deploy)
		b.ran("a", 0, true)
		b.ran("b", 0, true)
		b.judge(j, []applyrun.HostStatus{judgedRow(0, "a", idx(0), "a broke"), judgedRow(0, "b", idx(0), "b broke")}, hosts, true)
		j = b.step(3, config.OnFailureHost, []string{"a"}, u2)
		b.ran("a", 3, false)
		if got := b.judge(j, []applyrun.HostStatus{okRow(3, "a")}, hosts, false); len(got) != 0 {
			t.Errorf("the wave judged %v, want nothing yet", got)
		}
		got := b.judge(j, nil, hosts, true)
		if len(got) != 1 || got[0].sid != "b" || got[0].task != "deploy" {
			t.Errorf("the step's last barrier judged %v, want b's deploy", got)
		}
	})
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 5). A failed row is marked handled only
// when every failure it holds was. a fails `deploy` and `migrate` in one message, both
// held; `rollback` handles `deploy` on a and on b, and `unmigrate` fails on a, which is
// judged by `migrate` and dropped. b's row of step 0 is marked; a's holds `migrate` as
// well and is not, nor is the row of the unit that failed, and a leaves with both. A
// row that also holds a failure no source stands for is not marked, and a source a
// message handled on a row the Soul finished `success` marks nothing.
//
// Mutations: mark a row when any source it holds is handled (handledRows not
// requiring every one) → a's row of step 0 is marked; drop the sourceless case
// (handledRows not reading f.sourceless) → c's row of step 1 is marked; mark a row the
// barriers did not judge failed (handledRows not reading f.failedRows) → d's success
// row is marked.
func TestFailureState_HandledRows(t *testing.T) {
	t.Run("a row holding an unhandled source", func(t *testing.T) {
		steps := []config.StepNode{{Step: 0}, {Step: 1}, {Step: 2}}
		deploy := &render.RenderedTask{Index: 0, Name: "deploy", Register: "deploy"}
		migrate := &render.RenderedTask{Index: 1, Name: "migrate", Register: "migrate"}
		rollback := &render.RenderedTask{Index: 2, Name: "rollback", Passage: 1, OnFailIdx: []int{0}, RescueUnit: "1"}
		unmigrate := &render.RenderedTask{Index: 3, Name: "unmigrate", Passage: 2, OnFailIdx: []int{1}, RescueUnit: "2"}
		b := newBarrierRun([]render.RescueCover{
			{Path: "1", Sources: []string{"deploy"}, First: 1, Last: 1, RescueUnit: "1", DecidedAt: 1},
			{Path: "2", Sources: []string{"migrate"}, First: 2, Last: 2, RescueUnit: "2", DecidedAt: 2},
		}, steps)
		hosts := []*topology.HostFacts{{SID: "a"}, {SID: "b"}}
		b.f.useRoster(hosts)

		j := b.step(0, config.OnFailureHost, []string{"a", "b"}, deploy, migrate)
		b.ran("a", 0, true)
		b.ran("a", 1, true)
		b.ran("b", 0, true)
		b.judge(j, []applyrun.HostStatus{judgedRow(0, "a", idx(0), "deploy broke"), judgedRow(0, "b", idx(0), "deploy broke")}, hosts, true)
		j = b.step(1, config.OnFailureHost, []string{"a", "b"}, rollback)
		b.ran("a", 2, false)
		b.ran("b", 2, false)
		b.judge(j, []applyrun.HostStatus{okRow(1, "a"), okRow(1, "b")}, hosts, true)
		j = b.step(2, config.OnFailureHost, []string{"a"}, unmigrate)
		b.ran("a", 3, true)
		entries := b.judge(j, []applyrun.HostStatus{judgedRow(2, "a", idx(3), "unmigrate broke")}, hosts, true)
		if len(entries) != 1 || entries[0].task != "migrate" {
			t.Fatalf("step 2 judged %+v, want a's migrate", entries)
		}
		if _, err := b.f.settle(2, entries, hostOwner(), 2, hosts); err != nil {
			t.Fatalf("settle: %v", err)
		}
		if got, want := b.f.handledRows(), []runRow{{"b", 0}}; !reflect.DeepEqual(got, want) {
			t.Errorf("rows to mark handled = %v, want %v", got, want)
		}
		if got := b.f.droppedRows("a"); !reflect.DeepEqual(got, []int{0, 2}) {
			t.Errorf("a's rows to mark dropped = %v, want [0 2]", got)
		}
	})
	t.Run("a sourceless failure and a success row", func(t *testing.T) {
		f := newRunFailures()
		for _, s := range []struct {
			key  sourceKey
			rows []int
		}{{sourceKey{"c", 0}, []int{0, 1}}, {sourceKey{"d", 0}, []int{0}}} {
			f.sources[s.key] = &failedSource{task: "deploy", members: []int{0}, state: sourceHandled, rows: s.rows}
			f.bySID[s.key.sid] = append(f.bySID[s.key.sid], s.key)
		}
		f.failedRows[runRow{"c", 0}], f.failedRows[runRow{"c", 1}] = true, true
		f.sourceless["c"] = []int{1}
		if got, want := f.handledRows(), []runRow{{"c", 0}}; !reflect.DeepEqual(got, want) {
			t.Errorf("rows to mark handled = %v, want %v", got, want)
		}
	})
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 6, fail-closed). A row reported failed
// whose message carried only the tasks of a unit — an applier unit's child and its
// terminal here — is a failure of that unit, though no task of it is recorded failed:
// the unit has not succeeded, the source stays held for the next unit, and no entry is
// judged.
//
// Mutations: count a unit's task as a source (isSource set for u != 0 in replay) →
// the row reads as a source row, is judged at once as a failure tied to no task, and
// the unit has not failed; count a terminal as one (isSource for every task of no
// unit) → the same; drop the unit row's fail-closed in judgeRow → the unit succeeds
// and handles deploy at step 2.
func TestFailureState_AFailedUnitRowFailsItsUnit(t *testing.T) {
	steps, deploy, _, _, _, _, covers := unitPlan()
	child := &render.RenderedTask{Index: 2, Name: "child", Passage: 2, OnFailIdx: []int{0}, RescueUnit: "2"}
	terminal := &render.RenderedTask{Index: 5, Name: "applier-register undo", Register: "undo", Passage: 2, AggregateOf: []int{2}}
	b := newBarrierRun(covers, steps)
	hosts := []*topology.HostFacts{{SID: "a"}}
	j := b.step(0, config.OnFailureHost, []string{"a"}, deploy)
	b.ran("a", 0, true)
	b.judge(j, []applyrun.HostStatus{judgedRow(0, "a", idx(0), "deploy broke")}, hosts, true)

	j = b.step(2, config.OnFailureHost, []string{"a"}, child, terminal)
	b.ran("a", 2, false)
	b.ran("a", 5, false)
	if got := b.judge(j, []applyrun.HostStatus{judgedRow(2, "a", nil, "RunResult FAILED")}, hosts, true); len(got) != 0 {
		t.Errorf("judged %v, want nothing: the failure is its unit's", got)
	}
	if !b.f.view(3).unitFailed("a", "2") {
		t.Error("the unit of a failed unit row has not failed")
	}
	if s := b.f.sources[sourceKey{"a", 0}]; s.state != sourceHeld {
		t.Errorf("deploy on a is %v, want held for the next unit", s.state)
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 6, fail-closed, and rule 4). A row reported
// failed whose message carried only the tasks of a unit that did not fire there, no task
// of it recorded failed, names no task: it is a failure tied to no task, judged at once
// by the barrier owner's radius, and no source stands at the unit's first task.
//
// Mutation: hand the unit's first task to the net where its unit does not fire
// (judgeRow failing it whatever Fires says, and taking a held result for a source) →
// the entry names child under its own scenario, with a source recorded.
func TestFailureState_AUnitRowWhoseUnitDidNotFireIsTiedToNoTask(t *testing.T) {
	steps, _, _, _, _, _, covers := unitPlan()
	child := &render.RenderedTask{Index: 2, Name: "child", Passage: 2, OnFailIdx: []int{0}, RescueUnit: "2", OnFailure: config.OnFailureScenario}
	b := newBarrierRun(covers, steps)
	j := b.step(2, config.OnFailureHost, []string{"a"}, child)
	b.ran("a", 2, false)
	got := b.judge(j, []applyrun.HostStatus{judgedRow(2, "a", nil, "RunResult FAILED")}, []*topology.HostFacts{{SID: "a"}}, true)
	if len(got) != 1 || got[0].task != "" || got[0].radius != config.OnFailureHost {
		t.Errorf("judged %+v, want one entry tied to no task, under the owner's host", got)
	}
	if len(b.f.sources) != 0 || !reflect.DeepEqual(b.f.sourceless["a"], []int{2}) {
		t.Errorf("sources %v, tied to no task at %v - want none, and step 2", b.f.sources, b.f.sourceless["a"])
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 2 and the net of rule 6). The Keeper failed
// h before sending it a group, by `check`'s `when:`: only the cause is a failure of its
// own, and the FAILED records it wrote for the rest of the message, on either side of
// the cause, are of tasks that never ran. `deploy` is a source the host is judged with,
// by the cause. `stray`, before the cause, whose unit does not fire, is no net source;
// `mend`, after it, whose unit the cause fires, fails no unit and gives `check` no
// rescue's error.
//
// Mutation: feed every record of such a host to the replay (no isSource check beside the
// cause in replay) → stray is a third source, and mend's unit has failed.
func TestFailureState_AGroupTheKeeperFailedFailsByItsCauseAlone(t *testing.T) {
	steps := []config.StepNode{{Step: 0, Group: true}}
	deploy := &render.RenderedTask{Index: 0, Name: "deploy", Register: "deploy", Grouped: true}
	stray := &render.RenderedTask{Index: 1, Name: "stray", OnFailIdx: []int{9}, RescueUnit: "0/1", Grouped: true}
	check := &render.RenderedTask{Index: 2, Name: "check", Register: "check", Grouped: true}
	mend := &render.RenderedTask{Index: 3, Name: "mend", OnFailIdx: []int{2}, RescueUnit: "0/3", Grouped: true}
	b := newBarrierRun(nil, steps)
	j := b.step(0, config.OnFailureHost, []string{"h"}, deploy, stray, check, mend)
	var records []keeperRecord
	for _, rec := range []struct {
		task  *render.RenderedTask
		local int
	}{{check, 2}, {deploy, 0}, {stray, 1}, {mend, 3}} {
		records = append(records, keeperRecord{task: rec.task, localIdx: rec.local, status: keeperv1.TaskStatus_TASK_STATUS_FAILED})
	}
	j.settled = map[string]*settledHost{"h": {failed: true, records: records}}
	got := b.judge(j, []applyrun.HostStatus{judgedRow(0, "h", idx(2), "when failed")}, nil, true)
	if len(got) != 1 || got[0].task != "check" {
		t.Errorf("judged %v, want one entry naming check", got)
	}
	var names []string
	for _, k := range b.f.sourceKeys() {
		s := b.f.sources[k]
		names = append(names, s.task)
		if s.state != sourceJudged || len(s.unitErrs) != 0 {
			t.Errorf("source %s is %v with rescue errors %v, want judged at once with none", s.task, s.state, s.unitErrs)
		}
	}
	if want := []string{"deploy", "check"}; !reflect.DeepEqual(names, want) {
		t.Errorf("sources %v, want %v", names, want)
	}
	for _, unit := range []string{"0/1", "0/3"} {
		if b.f.view(1).unitFailed("h", unit) {
			t.Errorf("unit %s failed on h, want no unit failed by a task that never ran", unit)
		}
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 6, fail-closed). A Soul that ignores the
// marks ran a group under fail-stop: `src` failed, `u1`, its first unit, succeeded, and
// `u2`, its second, failed. In the replay `u1` handles `src`, but `u2` fired through
// `src`, which failed before it in that message: its failure is its unit's, not the
// net's. The row reads failed with every source handled, so `src` is set not handled,
// and the host is judged by it.
//
// Mutation: Fail without its sourceFailed check → u2 is a source of its own, the entry
// names it, and src stays handled.
func TestFailureState_ASecondUnitAFailStopSoulRanFailsItsUnit(t *testing.T) {
	steps := []config.StepNode{{Step: 0, Group: true}}
	src := &render.RenderedTask{Index: 0, Name: "src", Register: "src", Grouped: true}
	u1 := &render.RenderedTask{Index: 1, Name: "u1", OnFailIdx: []int{0}, RescueUnit: "0/1", Grouped: true}
	u2 := &render.RenderedTask{Index: 2, Name: "u2", OnFailIdx: []int{0}, RescueUnit: "0/2", Grouped: true}
	b := newBarrierRun(nil, steps)
	j := b.step(0, config.OnFailureHost, []string{"h"}, src, u1, u2)
	b.ran("h", 0, true)
	b.ran("h", 1, false)
	b.ran("h", 2, true)
	got := b.judge(j, []applyrun.HostStatus{judgedRow(0, "h", idx(0), "src broke")}, nil, true)
	if len(got) != 1 || got[0].task != "src" {
		t.Errorf("judged %v, want one entry naming src", got)
	}
	if s := b.f.sources[sourceKey{"h", 0}]; len(b.f.sources) != 1 || s == nil || s.state != sourceJudged {
		t.Errorf("sources %v, want src alone, judged", b.f.sources)
	}
	if !b.f.view(1).unitFailed("h", "0/2") {
		t.Error("u2's unit has not failed")
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 1). A unit that failed on a host at an
// earlier step sends that host none of its later tasks — a block unit spanning steps,
// its second child handed to the gate for both hosts here — as the Soul skips them
// inside one message, and its record is SKIPPED; on a host where it did not fail it
// goes as before. Render routes no such task to the host
// ([render.Rescue.FailedUnits]); the gate skips one should it be sent, as a defence.
// The failure is in the view of every later step and in the rescue routing render
// reads, and not in the view of the step it failed at, which renders the same on every
// pass.
//
// Mutations: drop the unitFailed check from crossPassageGate.verdict → the second child
// is sent to a; record no unit failure (recordUnitFails a no-op) → the same, and
// render's rescue routing holds none; read a failure at the view's own step (view
// keeping at >= step) → the view of step 2 holds it.
func TestCrossPassageGate_AFailedUnitSendsNoLaterTask(t *testing.T) {
	steps, deploy, _, _, _, _, _ := unitPlan()
	first := &render.RenderedTask{Index: 2, Name: "first", Passage: 2, OnFailIdx: []int{0, 4}, RescueUnit: "2"}
	second := &render.RenderedTask{Index: 3, Name: "second", Passage: 3, OnFailIdx: []int{0, 4}, RescueUnit: "2"}
	verify := &render.RenderedTask{Index: 4, Name: "verify", Register: "verify", Passage: 4}
	block := []render.RescueCover{{Path: "2", Sources: []string{"deploy", "verify"}, First: 2, Last: 3, RescueUnit: "2", DecidedAt: 3}}
	steps[2] = config.StepNode{Step: 2, Children: []config.StepNode{{Step: 2}, {Step: 3}}}
	steps = append(steps[:3], config.StepNode{Step: 4})
	b := newBarrierRun(block, steps)
	hosts := []*topology.HostFacts{{SID: "a"}, {SID: "b"}}
	j := b.step(0, config.OnFailureHost, []string{"a", "b"}, deploy)
	b.ran("a", 0, true)
	b.ran("b", 0, true)
	b.judge(j, []applyrun.HostStatus{judgedRow(0, "a", idx(0), "a broke"), judgedRow(0, "b", idx(0), "b broke")}, hosts, true)
	j = b.step(2, config.OnFailureHost, []string{"a", "b"}, first)
	b.ran("a", 2, true)
	b.ran("b", 2, false)
	b.judge(j, []applyrun.HostStatus{judgedRow(2, "a", idx(2), "first broke"), okRow(2, "b")}, hosts, true)

	if b.f.view(2).unitFailed("a", "2") {
		t.Error("the view of step 2 holds the unit failure of its own barrier")
	}
	if got := b.f.view(3).rescue().FailedUnits; !reflect.DeepEqual(got, map[string]map[string]int{"a": {"2": 2}}) {
		t.Errorf("rescue routing's failed units = %v, want unit 2 on a, failed at step 2", got)
	}
	gate := newCrossPassageGate([]*render.RenderedTask{deploy, first, second, verify}, nil, nil, b.f.view(3).unhandledKeys())
	gate.unitFailed = b.f.view(3).unitFailed
	perHost, settled := settleHosts(map[string][]*render.RenderedTask{"a": {second}, "b": {second}}, gate, 3)
	if _, sent := perHost["a"]; sent {
		t.Errorf("a is sent %v, want nothing: its unit failed at step 2", namesOf(perHost["a"]))
	}
	if got := recordViews(settled["a"]); !reflect.DeepEqual(got, []recordView{{Name: "second", Status: skipped}}) {
		t.Errorf("a's records = %v, want second SKIPPED", got)
	}
	if got := namesOf(perHost["b"]); !reflect.DeepEqual(got, []string{"second"}) {
		t.Errorf("b is sent %v, want [second]", got)
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, rules 2 and 9). A failure still held when the
// failure list closes is appended at that point, so the run's error names it: a
// Cancel stops the run before its unit's barrier. A `loop:`'s held iterations are one
// failure there, and a run stopped with nothing held keeps its error as it is.
//
// Mutations: close without appending the held sources → the Cancel's error names no
// deploy; append one entry per member → `each` is named twice.
func TestRunFailures_HeldFailuresAreAppendedAtClose(t *testing.T) {
	steps, deploy, _, _, _, _, covers := unitPlan()
	iter0 := &render.RenderedTask{Index: 7, Name: "each", Register: "each"}
	iter1 := &render.RenderedTask{Index: 8, Name: "each", Register: "each"}
	covers = append(covers, render.RescueCover{Path: "3", Sources: []string{"each"}, First: 3, Last: 3, RescueUnit: "3", DecidedAt: 3})
	b := newBarrierRun(covers, steps)
	hosts := []*topology.HostFacts{{SID: "a"}, {SID: "b"}}
	j := b.step(0, config.OnFailureHost, nil, deploy, iter0, iter1)
	j.sent["a"] = []*render.RenderedTask{deploy}
	j.sent["b"] = []*render.RenderedTask{iter0, iter1}
	b.ran("a", 0, true)
	b.ran("b", 7, true)
	b.ran("b", 8, true)
	if got := b.judge(j, []applyrun.HostStatus{judgedRow(0, "a", idx(0), "deploy broke"), judgedRow(0, "b", idx(7), "each broke")}, hosts, true); len(got) != 0 {
		t.Fatalf("judged %v, want both held", got)
	}
	cancel := errors.New("scenario: cancel requested at step 1")
	got := b.f.withHeld(cancel)
	if !errors.Is(got, cancel) || !strings.Contains(got.Error(), `task "deploy"`) || strings.Count(got.Error(), `task "each"`) != 1 {
		t.Errorf("the stopped run's error = %q, want the Cancel with deploy and each held, each once", got)
	}
	if quiet := newRunFailures(); quiet.withHeld(cancel) != cancel {
		t.Error("a run with nothing held changed its error")
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, "What the run goroutine keeps"). A view is a
// copy: a barrier deciding a source's or a host's state after the view was taken
// does not reach it.
//
// Mutations: share with the state in view the source's lastUnit → the view reads the
// later step; its unitErrs → the view reads the later error; its members → the view
// reads the later index; a period's missed → the view reads the later register; a
// period's returnedAt → the view reads the later step.
func TestFailureView_IsNotChangedByALaterBarrier(t *testing.T) {
	f := newRunFailures()
	key := sourceKey{"a", 0}
	f.sources[key] = &failedSource{register: "deploy", members: []int{0}, failedAt: 0, state: sourceJudged, lastUnit: idx(2), unitErrs: []string{"undo failed"}, rows: []int{0}}
	skipped := map[string]any{"skipped": true}
	f.hosts["a"] = &hostFailures{facts: &topology.HostFacts{SID: "a"}, periods: []holdPeriod{{heldFrom: 1, returnedAt: idx(2), missed: map[string]any{"reload": skipped}}}}
	v := f.view(3)

	*f.sources[key].lastUnit = 5
	f.sources[key].unitErrs[0] = "later"
	f.sources[key].rows[0] = 3
	f.sources[key].members[0] = 9
	p := f.hosts["a"].periods[0]
	p.missed["reload"] = "later"
	*p.returnedAt = 6

	if got := v.sources[key]; *got.lastUnit != 2 || got.unitErrs[0] != "undo failed" || got.rows[0] != 0 || got.members[0] != 0 {
		t.Errorf("view's source = lastUnit %d, unitErrs %v, rows %v, members %v - want the state as step 3 saw it (2, [undo failed], [0], [0])", *got.lastUnit, got.unitErrs, got.rows, got.members)
	}
	got := v.hosts["a"].periods[0]
	if !reflect.DeepEqual(got.missed["reload"], skipped) || *got.returnedAt != 2 {
		t.Errorf("view's hold period = missed %v, returnedAt %d - want the state as step 3 saw it (reload skipped, 2)", got.missed, *got.returnedAt)
	}
}

// ★ GUARD (fork 1, NIM-931). Rescue routing reads a failed child as a failure of its
// applier's register as well as its own, and a register failing at two steps by the
// first of them.
//
// Mutation: record a source without its applier's register → `certs` is missing.
func TestFailureView_RescueFoldsAnApplierWithItsChild(t *testing.T) {
	probe := &render.RenderedTask{Index: 0, Name: "probe", Register: "probe"}
	issue := &render.RenderedTask{Index: 1, Name: "issue", Register: "issue", Passage: 1}
	certs := &render.RenderedTask{Index: 2, Name: "certs", Register: "certs", Passage: 1, AggregateOf: []int{1}}
	again := &render.RenderedTask{Index: 4, Name: "probe again", Register: "probe", Passage: 3}

	f := newRunFailures()
	for _, s := range []struct {
		step  int
		tasks []*render.RenderedTask
		index int
	}{{0, []*render.RenderedTask{probe}, 0}, {1, []*render.RenderedTask{issue, certs}, 1}, {3, []*render.RenderedTask{again}, 4}} {
		j := judgeOf(config.OnFailureHost, s.tasks...)
		j.step = s.step
		j.sent["a"] = s.tasks
		j.failedAt = map[string]map[int]bool{"a": {s.index: true}}
		f.barrier(j, []applyrun.HostStatus{judgedRow(s.step, "a", idx(s.index), "boom")}, []*topology.HostFacts{{SID: "a"}}, true)
	}
	f.drop([]failureEntry{{step: 0, sid: "a", radius: config.OnFailureHost}}, nil)

	want := map[string]map[string]int{"a": {"probe": 0, "issue": 1, "certs": 1}}
	if got := f.view(4).rescue().Failed; !reflect.DeepEqual(got, want) {
		t.Errorf("failed registers = %v, want %v", got, want)
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, "What the run goroutine keeps"). A step reads
// the failure state as it stood when that step was dispatched: a host held twice is
// held, back, and held again at the steps between; a later drop and a later hold are
// not there yet; a source failed at or after the step is absent, and one handled at
// or after it is still held there. A host carries the facts it left with only while it
// is held or dropped at the step: a host not out yet, or back, takes the roster's
// entry, and one the roster read again no longer lists is gone with it. Render reads
// the same from the standing: an earlier step has a host dropped since live, the
// registers synthesized for what a returned host missed lie beneath its own from its
// return on, and a dropped host carries the facts it left with whatever a roster read
// again says.
//
// Mutations: keep a return of a later step → h is live at step 3; show a state a
// later barrier decided → the handled source is gone at step 3; leave Synth out of
// the standing → no `reload` at step 5; take a dropped host's facts from the roster
// read again → d's facts are the fresh ones; merge in the stored facts of every host
// whose standing changed (runRoster over every changed host) → z is live at step 1 and
// r is live at step 3; take a returned host's stored facts → h's are not the fresh ones
// at step 4.
func TestFailureView_AsEachStepSawIt(t *testing.T) {
	a := &topology.HostFacts{SID: "a"}
	hLeft, hFresh := &topology.HostFacts{SID: "h", Status: "left"}, &topology.HostFacts{SID: "h", Status: "fresh"}
	dLeft, dFresh := &topology.HostFacts{SID: "d", Status: "left"}, &topology.HostFacts{SID: "d", Status: "fresh"}
	r, z := &topology.HostFacts{SID: "r"}, &topology.HostFacts{SID: "z"}
	skipped := map[string]any{"changed": false, "failed": false, "timed_out": false, "skipped": true}

	f := newRunFailures()
	f.useRoster([]*topology.HostFacts{a, dFresh, hFresh})
	// h fails `deploy` at step 1, a unit at step 3 handles it, it is back from 4 and
	// misses `reload` meanwhile; it fails `warm` at step 5 and is held from 6. d fails
	// `reload` at step 2 and is dropped; so is z, gone from the roster read again. r is
	// held at step 2 only and gone from the roster read again too.
	f.hosts["h"] = &hostFailures{facts: hLeft, periods: []holdPeriod{
		{heldFrom: 2, returnedAt: idx(4), missed: map[string]any{"reload": skipped}},
		{heldFrom: 6},
	}}
	f.hosts["d"] = &hostFailures{facts: dLeft, droppedAt: idx(3)}
	f.hosts["z"] = &hostFailures{facts: z, droppedAt: idx(3)}
	f.hosts["r"] = &hostFailures{facts: r, periods: []holdPeriod{{heldFrom: 2, returnedAt: idx(3)}}}
	f.sources[sourceKey{"h", 0}] = &failedSource{register: "deploy", members: []int{0}, failedAt: 1, state: sourceHandled, changedAt: 3}
	f.sources[sourceKey{"d", 1}] = &failedSource{register: "reload", members: []int{1}, failedAt: 2, state: sourceJudged, changedAt: 2}
	f.sources[sourceKey{"h", 7}] = &failedSource{register: "warm", members: []int{7}, failedAt: 5, state: sourceHeld, changedAt: 5}

	sids := func(hosts []*topology.HostFacts) []string {
		out := []string{}
		for _, x := range hosts {
			out = append(out, x.SID)
		}
		return out
	}
	for _, tc := range []struct {
		step                int
		live, held, dropped []string
		hFacts              *topology.HostFacts
		unhandled           []auditpg.ChangedTaskKey
		failed, handledAt   map[string]map[string]int
	}{
		{step: 1, live: []string{"a", "d", "h"}, held: []string{}, dropped: []string{}, hFacts: hFresh},
		{step: 2, live: []string{"a", "d"}, held: []string{"h", "r"}, dropped: []string{}, hFacts: hLeft,
			unhandled: []auditpg.ChangedTaskKey{failedKey("h", 0)},
			failed:    map[string]map[string]int{"h": {"deploy": 1}}},
		{step: 3, live: []string{"a"}, held: []string{"h"}, dropped: []string{"d", "z"}, hFacts: hLeft,
			unhandled: []auditpg.ChangedTaskKey{failedKey("h", 0), failedKey("d", 1)},
			failed:    map[string]map[string]int{"h": {"deploy": 1}, "d": {"reload": 2}}},
		{step: 4, live: []string{"a", "h"}, held: []string{}, dropped: []string{"d", "z"}, hFacts: hFresh,
			unhandled: []auditpg.ChangedTaskKey{failedKey("d", 1)},
			failed:    map[string]map[string]int{"h": {"deploy": 1}, "d": {"reload": 2}},
			handledAt: map[string]map[string]int{"h": {"deploy": 3}}},
		{step: 6, live: []string{"a"}, held: []string{"h"}, dropped: []string{"d", "z"}, hFacts: hLeft,
			unhandled: []auditpg.ChangedTaskKey{failedKey("d", 1), failedKey("h", 7)},
			failed:    map[string]map[string]int{"h": {"deploy": 1, "warm": 5}, "d": {"reload": 2}},
			handledAt: map[string]map[string]int{"h": {"deploy": 3}}},
	} {
		v := f.view(tc.step)
		live, held, dropped := v.split()
		if got := [3][]string{sids(live), sids(held), sids(dropped)}; !reflect.DeepEqual(got, [3][]string{tc.live, tc.held, tc.dropped}) {
			t.Errorf("step %d: live, held, dropped = %v, want %v", tc.step, got, [3][]string{tc.live, tc.held, tc.dropped})
		}
		for _, x := range v.roster {
			if x.SID == "h" && x != tc.hFacts {
				t.Errorf("step %d: h's facts = %+v, want %+v", tc.step, x, tc.hFacts)
			}
		}
		if got := v.unhandledKeys(); !reflect.DeepEqual(got, keySet(tc.unhandled...)) {
			t.Errorf("step %d: unhandled sources = %v, want %v", tc.step, got, tc.unhandled)
		}
		rescue := v.rescue()
		if tc.failed == nil {
			if rescue != nil {
				t.Errorf("step %d: rescue = %+v, want nil while no host left the run", tc.step, rescue)
			}
			continue
		}
		if rescue == nil || !reflect.DeepEqual(rescue.Failed, tc.failed) || !reflect.DeepEqual(rescue.HandledAt, tc.handledAt) {
			t.Errorf("step %d: rescue = %+v, want failed %v, handled %v", tc.step, rescue, tc.failed, tc.handledAt)
		}
	}

	if f.view(1).standing() != nil {
		t.Error("a step before any host's standing changed got a standing")
	}
	in := render.RenderInput{
		Standing:       f.view(6).standing(),
		RegisterByHost: map[string]map[string]any{"h": {"deploy": map[string]any{"failed": true}}},
	}
	for _, tc := range []struct {
		step                int
		live, held, dropped []string
		reload              bool
	}{
		{step: 1, live: []string{"a", "d", "h", "z"}, held: []string{}, dropped: []string{}},
		{step: 3, live: []string{"a"}, held: []string{"h"}, dropped: []string{"d", "z"}},
		{step: 5, live: []string{"a", "h"}, held: []string{}, dropped: []string{"d", "z"}, reload: true},
		{step: 6, live: []string{"a"}, held: []string{"h"}, dropped: []string{"d", "z"}, reload: true},
	} {
		at := in.ForStep(tc.step)
		if got := [3][]string{sids(at.Hosts), sids(at.HeldHosts), sids(at.Dropped)}; !reflect.DeepEqual(got, [3][]string{tc.live, tc.held, tc.dropped}) {
			t.Errorf("render at step %d: live, held, dropped = %v, want %v", tc.step, got, [3][]string{tc.live, tc.held, tc.dropped})
		}
		if _, ok := at.RegisterByHost["h"]["reload"]; ok != tc.reload {
			t.Errorf("render at step %d: h's synthesized `reload` present = %v, want %v", tc.step, ok, tc.reload)
		}
		if at.RegisterByHost["h"]["deploy"] == nil {
			t.Errorf("render at step %d: h lost its own `deploy` register", tc.step)
		}
	}
	for _, x := range in.Standing.Roster {
		if x.SID == "d" && x != dLeft {
			t.Errorf("d's facts in the standing = %+v, want those it left with", x)
		}
	}
}
