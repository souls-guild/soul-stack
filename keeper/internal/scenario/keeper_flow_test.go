package scenario

import (
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

func flowTask(idx int, name string, grouped bool, flow map[string]render.HostFlow) *render.RenderedTask {
	return &render.RenderedTask{Index: idx, Name: name, Module: "core.exec.run", Grouped: grouped, FlowBySID: flow}
}

type recordView struct {
	Name   string
	Local  int
	Status keeperv1.TaskStatus
	Code   string
}

func recordViews(sh *settledHost) []recordView {
	if sh == nil {
		return nil
	}
	out := make([]recordView, 0, len(sh.records))
	for _, r := range sh.records {
		v := recordView{Name: r.task.Name, Local: r.localIdx, Status: r.status}
		if r.failure != nil {
			v.Code = r.failure.Code
		}
		out = append(out, v)
	}
	return out
}

func namesOf(tasks []*render.RenderedTask) []string {
	out := make([]string, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, t.Name)
	}
	return out
}

const (
	skipped = keeperv1.TaskStatus_TASK_STATUS_SKIPPED
	failed  = keeperv1.TaskStatus_TASK_STATUS_FAILED
)

// ★ GUARD (decision 4). A skipped step is left out of the host's message and
// recorded SKIPPED by the Keeper; a skipped grouped child stays in the message.
//
// Mutations: keep a skipped step in the slice → the host is sent a task the Keeper
// decided it skips; drop a grouped child → its siblings lose its register.
func TestSettleHosts_SkippedStepIsRecordedAndGroupedChildStays(t *testing.T) {
	step := flowTask(3, "step", false, map[string]render.HostFlow{"a": {}, "b": {Skip: true}})
	child := flowTask(4, "child", true, map[string]render.HostFlow{"g": {Skip: true}})
	perHost := map[string][]*render.RenderedTask{"a": {step}, "b": {step}, "g": {child}}

	out, settled := settleHosts(perHost, nil, 1)
	if got := namesOf(out["a"]); !reflect.DeepEqual(got, []string{"step"}) {
		t.Errorf("a sends %v, want [step]", got)
	}
	if _, sends := out["b"]; sends {
		t.Errorf("b sends %v, want nothing", namesOf(out["b"]))
	}
	if got, want := recordViews(settled["b"]), []recordView{{Name: "step", Local: 0, Status: skipped}}; !reflect.DeepEqual(got, want) {
		t.Errorf("b records = %+v, want %+v", got, want)
	}
	if got := namesOf(out["g"]); !reflect.DeepEqual(got, []string{"child"}) {
		t.Errorf("g sends %v, want the grouped child, as when: false", got)
	}
	if settled["g"] != nil || settled["a"] != nil {
		t.Errorf("records for a=%+v g=%+v, want none", settled["a"], settled["g"])
	}
}

// ★ GUARD (predicate-rendering.md §1). A task failed on a host fails the whole
// message there: the cause is recorded FAILED first, a child the Keeper had decided
// to skip SKIPPED, every other task FAILED, and the host is sent nothing.
//
// Mutations: send the rest of the group → a group rolled as one sends half of
// itself; record the cause after a sibling → error_summary names the wrong task.
func TestSettleHosts_FailureFailsTheWholeMessage(t *testing.T) {
	why := &render.FlowFailure{Code: "predicate_not_renderable", Message: "x"}
	a := flowTask(5, "a", true, map[string]render.HostFlow{"h": {}})
	b := flowTask(6, "b", true, map[string]render.HostFlow{"h": {Skip: true}})
	c := flowTask(7, "c", true, map[string]render.HostFlow{"h": {Failure: why}})
	perHost := map[string][]*render.RenderedTask{"h": {a, b, c}}

	out, settled := settleHosts(perHost, nil, 1)
	if len(out) != 0 {
		t.Errorf("sends %v, want nothing", out)
	}
	sh := settled["h"]
	if sh == nil || !sh.failed {
		t.Fatalf("settled = %+v, want a failed host", sh)
	}
	want := []recordView{
		{Name: "c", Local: 2, Status: failed, Code: "predicate_not_renderable"},
		{Name: "a", Local: 0, Status: failed, Code: "predicate_not_renderable"},
		{Name: "b", Local: 1, Status: skipped},
	}
	if got := recordViews(sh); !reflect.DeepEqual(got, want) {
		t.Errorf("records = %+v, want %+v", got, want)
	}
}

// The requisite gate's drops become SKIPPED records under linear dispatch; a
// grouped child it drops stays in the message, skipped on that host only.
//
// Mutations: let the gate drop a grouped child → it leaves the message; share one
// clone across hosts → host-b would skip it too.
func TestSettleHosts_GateDropsAStepAndKeepsAGroupedChild(t *testing.T) {
	src := &render.RenderedTask{Index: 0, Name: "conf", Register: "conf", Passage: 0}
	step := &render.RenderedTask{Index: 1, Name: "restart", Passage: 1, OnChangesIdx: []int{0}}
	child := &render.RenderedTask{Index: 2, Name: "child", Passage: 1, OnChangesIdx: []int{0}, Grouped: true}
	g := gateChangedOn([]*render.RenderedTask{src, step, child}, "host-b", 0)

	perHost := map[string][]*render.RenderedTask{"host-a": {step, child}, "host-b": {step, child}}
	out, settled := settleHosts(perHost, g, 1)
	if got, want := recordViews(settled["host-a"]), []recordView{{Name: "restart", Local: 0, Status: skipped}}; !reflect.DeepEqual(got, want) {
		t.Errorf("host-a records = %+v, want %+v", got, want)
	}
	if settled["host-b"] != nil {
		t.Errorf("host-b records = %+v, want none", recordViews(settled["host-b"]))
	}
	kept := out["host-a"]
	if len(kept) != 1 || kept[0].Name != "child" || !kept[0].FlowBySID["host-a"].Skip {
		t.Fatalf("host-a sends %+v, want child skipped there", kept)
	}
	if wire := render.ToProtoTasksForHost(kept, "host-a")[0]; wire.GetWhen() != "false" || len(wire.GetOnchangesIdx()) != 0 {
		t.Errorf("host-a child = {when:%q onchanges:%v}, want when: false and no requisite into an earlier step", wire.GetWhen(), wire.GetOnchangesIdx())
	}
	if got := namesOf(out["host-b"]); !reflect.DeepEqual(got, []string{"restart", "child"}) {
		t.Errorf("host-b sends %v, want both", got)
	}
	if out["host-b"][1].FlowBySID != nil {
		t.Errorf("host-b's child carries %+v, want the shared task untouched", out["host-b"][1].FlowBySID)
	}
}

// A grouped child sent as `when: false` carries nothing that gates a task that runs:
// a requisite into an earlier step would travel as a dangling index.
//
// Mutation: keep any of the requisites on the clone → that field is set.
func TestSkippedOn_ClearsWhatGatesARunningTask(t *testing.T) {
	child := &render.RenderedTask{Index: 4, Name: "child", Grouped: true, OnChangesIdx: []int{0}, OnFailIdx: []int{1}, RequireIdx: []int{2}, RequireAll: true}
	sent := skippedOn(child, "h")
	if sent.OnChangesIdx != nil || sent.OnFailIdx != nil || sent.RequireIdx != nil || sent.RequireAll {
		t.Errorf("sent = %+v, want no requisites", sent)
	}
	if !sent.FlowBySID["h"].Skip || child.FlowBySID != nil || child.OnFailIdx == nil {
		t.Errorf("clone or original wrong: sent=%+v original=%+v", sent.FlowBySID, child)
	}
}

// gateChangedOn is a gate for passage 1 whose only CHANGED fact is src on sid.
func gateChangedOn(tasks []*render.RenderedTask, sid string, src int) *crossPassageGate {
	changed := map[auditpg.ChangedTaskKey]struct{}{{SID: sid, PlanIndex: src}: {}}
	return newCrossPassageGate(tasks, nil, changed, nil)
}

// ★ GUARD (decision 4, the Soul's order). A `when:` that failed fails the task even
// where its requisites are not met; a post-module predicate that failed does not,
// since a task the requisites skip never evaluates one; and inside a failed message
// a child the gate skips is recorded SKIPPED, not FAILED — onfail: reads FAILED.
//
// Mutations: apply the gate before a `when:` failure → the first case is skipped;
// fail on a post-module predicate before the gate → the second host fails; treat a
// gate-skipped sibling of a failed host as failed → the third case records FAILED.
func TestSettleHosts_OrdersAsTheSoul(t *testing.T) {
	src := &render.RenderedTask{Index: 0, Name: "conf", Register: "conf", Passage: 0}
	whenBroken := &render.FlowFailure{Predicate: "when", Code: "flowcontrol.when_error", Message: "w"}
	predBroken := &render.FlowFailure{Predicate: "failed_when", Code: "predicate_not_renderable", Message: "p"}
	gated := func(idx int, name string, grouped bool, f *render.FlowFailure) *render.RenderedTask {
		rt := flowTask(idx, name, grouped, map[string]render.HostFlow{"h": {Failure: f}})
		rt.Passage, rt.OnChangesIdx = 1, []int{0}
		return rt
	}
	cases := []struct {
		name       string
		tasks      []*render.RenderedTask
		wantFailed bool
		want       []recordView
	}{
		{
			name:       "when failure beats the gate",
			tasks:      []*render.RenderedTask{gated(1, "x", false, whenBroken)},
			wantFailed: true,
			want:       []recordView{{Name: "x", Local: 0, Status: failed, Code: "flowcontrol.when_error"}},
		},
		{
			name:  "post-module failure on a task the gate skips",
			tasks: []*render.RenderedTask{gated(1, "x", false, predBroken)},
			want:  []recordView{{Name: "x", Local: 0, Status: skipped}},
		},
		{
			name: "a gate-skipped sibling of a failed host",
			tasks: []*render.RenderedTask{
				gated(1, "c", true, nil),
				flowTask(2, "d", true, map[string]render.HostFlow{"h": {Failure: predBroken}}),
			},
			wantFailed: true,
			want: []recordView{
				{Name: "d", Local: 1, Status: failed, Code: "predicate_not_renderable"},
				{Name: "c", Local: 0, Status: skipped},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := gateChangedOn(append([]*render.RenderedTask{src}, tc.tasks...), "elsewhere", 0)
			out, settled := settleHosts(map[string][]*render.RenderedTask{"h": tc.tasks}, g, 1)
			if len(out) != 0 {
				t.Errorf("sends %v, want nothing", namesOf(out["h"]))
			}
			sh := settled["h"]
			if sh == nil || sh.failed != tc.wantFailed {
				t.Fatalf("settled = %+v, want failed=%v", sh, tc.wantFailed)
			}
			if got := recordViews(sh); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("records = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A host that is sent a message gets its records numbered after what it is sent:
// the Soul numbers task_idx by what it received.
func TestNumberedAfter(t *testing.T) {
	recs := []keeperRecord{{localIdx: 0}, {localIdx: 3}}
	got := numberedAfter(recs, 2)
	if got[0].localIdx != 2 || got[1].localIdx != 3 || recs[0].localIdx != 0 {
		t.Errorf("numberedAfter = %+v (input %+v), want 2, 3 and the input untouched", got, recs)
	}
}

// ★ GUARD. A host the Keeper failed before sending does not break the barrier while
// the hosts sent the step still run; another host's failure still does.
//
// Mutation: classify without the exception → the barrier fails while A runs.
func TestClassifyExcept_HostFailedEarlyWaitsForTheRest(t *testing.T) {
	summary := "task 0 core.exec.run: when"
	rows := []applyrun.HostStatus{
		{SID: "b", Passage: 1, Status: applyrun.StatusFailed, ErrorSummary: &summary},
		{SID: "a", Passage: 1, Status: applyrun.StatusRunning},
	}
	early := map[string]bool{"b": true}
	if done, err := classifyExcept(rows, 1, 2, early); done || err != nil {
		t.Errorf("A running: done=%v err=%v, want waiting", done, err)
	}
	rows[1].Status = applyrun.StatusSuccess
	if done, err := classifyExcept(rows, 1, 2, early); !done || err != nil {
		t.Errorf("A finished: done=%v err=%v, want done", done, err)
	}
	if _, err := classify(rows, 1, 2); err == nil {
		t.Errorf("classify without the exception: no failure, want B's")
	}
	rows[1].Status = applyrun.StatusFailed
	if _, err := classifyExcept(rows, 1, 2, early); err == nil {
		t.Errorf("A failed: no failure, want A's")
	}
}

// A Keeper-written record carries the register a Soul writes for a task it did
// not run, and names the plan index the register and the audit fact are keyed by.
func TestKeeperRecord_EventMirrorsTheSoul(t *testing.T) {
	rec := keeperRecord{task: &render.RenderedTask{Index: 9, Module: "core.exec.run", SecretOutput: []string{"token"}}, localIdx: 2, status: skipped}
	ev := rec.event("apply-1", 4)
	if ev.GetPlanIndex() != 9 || ev.GetTaskIdx() != 2 || ev.GetPassage() != 4 || ev.GetError() != nil {
		t.Errorf("event = %v", ev)
	}
	want := map[string]any{"changed": false, "failed": false, "timed_out": false, "skipped": true}
	if got := ev.GetRegisterData().AsMap(); !reflect.DeepEqual(got, want) {
		t.Errorf("register = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(ev.GetSecretOutput(), []string{"token"}) {
		t.Errorf("secret_output = %v", ev.GetSecretOutput())
	}

	rec.status, rec.failure = failed, &render.FlowFailure{Code: "flowcontrol.when_error", Message: "m"}
	ev = rec.event("apply-1", 4)
	if e := ev.GetError(); e.GetCode() != "flowcontrol.when_error" || e.GetModule() != "core.exec.run" || e.GetMessage() != "m" {
		t.Errorf("error = %v", e)
	}
	if !ev.GetRegisterData().AsMap()["failed"].(bool) {
		t.Errorf("register.failed = false for a failed record")
	}
}
