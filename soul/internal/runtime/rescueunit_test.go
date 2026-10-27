package runtime

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/shared/obs"
	"github.com/souls-guild/soul-stack/shared/obs/obstest"
	coretask "github.com/souls-guild/soul-stack/soul/internal/coremod/task"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// ADR-0091 amendment NIM-993, rule 6: inside one message a rescue unit that
// succeeds handles its source's failure. Every case below is paired with the
// near miss that tells the rule from fail-stop.

const (
	tF = keeperv1.TaskStatus_TASK_STATUS_FAILED
	tC = keeperv1.TaskStatus_TASK_STATUS_CHANGED
	tS = keeperv1.TaskStatus_TASK_STATUS_SKIPPED
	tO = keeperv1.TaskStatus_TASK_STATUS_OK

	rOK     = keeperv1.RunStatus_RUN_STATUS_SUCCESS
	rFailed = keeperv1.RunStatus_RUN_STATUS_FAILED

	// whenErrors fails the task if it is ever evaluated: no task registers
	// `nosuch`, so the read is a CEL runtime error.
	whenErrors = "register.nosuch.failed"
)

// scriptModule runs every task of a plan by its name: a name in fail fails with
// that message, any other reports changed. Safe for async flows.
type scriptModule struct {
	module.BaseModule
	fail map[string]string
}

func (m *scriptModule) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	name := req.GetParams().GetFields()["name"].GetStringValue()
	if msg, ok := m.fail[name]; ok {
		return stream.Send(&pluginv1.ApplyEvent{Failed: true, Message: msg})
	}
	return stream.Send(&pluginv1.ApplyEvent{Changed: true})
}

// st is a script task in rescue unit `unit` (0 = none) with `onfail:` on the
// given message indices.
func st(name string, unit int32, onfail ...int32) *keeperv1.RenderedTask {
	return &keeperv1.RenderedTask{
		Name:       name,
		Module:     "core.script.run",
		Register:   name,
		RescueUnit: unit,
		OnfailIdx:  onfail,
		Params: &structpb.Struct{Fields: map[string]*structpb.Value{
			"name": structpb.NewStringValue(name),
		}},
	}
}

func withWhen(t *keeperv1.RenderedTask, expr string) *keeperv1.RenderedTask {
	t.When = expr
	return t
}

func asAsync(t *keeperv1.RenderedTask) *keeperv1.RenderedTask {
	t.Async = true
	return t
}

func requires(t *keeperv1.RenderedTask, idx ...int32) *keeperv1.RenderedTask {
	t.RequireIdx = idx
	return t
}

// iter is one iteration of a `loop:` task of no unit: every iteration shares
// the register `loop`, so an `onfail: [loop]` lists all of their indices.
func iter(name string) *keeperv1.RenderedTask {
	t := st(name, 0)
	t.Register = "loop"
	return t
}

// terminal is the applier `app`'s terminal over the given children.
func terminal(children ...int32) *keeperv1.RenderedTask {
	return &keeperv1.RenderedTask{Name: "app", Module: "core.noop.run", Register: "app", AggregateOf: children}
}

// eventSink records from several flows; read only after Run returns.
type eventSink struct {
	mu     sync.Mutex
	events []*keeperv1.TaskEvent
	result *keeperv1.RunResult
	onTask func(*keeperv1.TaskEvent)
}

func (s *eventSink) SendTaskEvent(ev *keeperv1.TaskEvent) error {
	if s.onTask != nil {
		s.onTask(ev)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
	return nil
}

func (s *eventSink) SendRunResult(rr *keeperv1.RunResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.result = rr
	return nil
}

func (s *eventSink) byName(t *testing.T, tasks []*keeperv1.RenderedTask, name string) *keeperv1.TaskEvent {
	t.Helper()
	for i, task := range tasks {
		if task.GetName() != name {
			continue
		}
		for _, ev := range s.events {
			if ev.GetTaskIdx() == int32(i) {
				return ev
			}
		}
		t.Fatalf("no TaskEvent for %q", name)
	}
	t.Fatalf("no task %q in the plan", name)
	return nil
}

func runPlan(t *testing.T, fail map[string]string, tasks []*keeperv1.RenderedTask) *eventSink {
	t.Helper()
	return runWith(t, NewApplyRunner(scriptRegistry(fail), nil), tasks)
}

func runWith(t *testing.T, r *ApplyRunner, tasks []*keeperv1.RenderedTask) *eventSink {
	t.Helper()
	sink := &eventSink{}
	if err := r.Run(context.Background(), &keeperv1.ApplyRequest{ApplyId: "rescue-unit", Tasks: tasks}, sink); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sink.events) != len(tasks) {
		t.Fatalf("TaskEvents = %d, want one per task (%d)", len(sink.events), len(tasks))
	}
	return sink
}

func scriptRegistry(fail map[string]string) mapRegistry {
	return mapRegistry{
		"core.script": &scriptModule{fail: fail},
		"core.noop":   okNoopModule(nil),
		"core.task":   coretask.New(),
	}
}

func TestRescueUnits_Outcomes(t *testing.T) {
	boom := func(names ...string) map[string]string {
		m := map[string]string{}
		for _, n := range names {
			m[n] = n + " broke"
		}
		return m
	}
	cases := []struct {
		name  string
		fail  map[string]string
		tasks []*keeperv1.RenderedTask
		want  map[string]keeperv1.TaskStatus
		run   keeperv1.RunStatus
	}{
		{
			name:  "no marks: fail-stop, the rescue does not undo the failure",
			fail:  boom("src"),
			tasks: []*keeperv1.RenderedTask{st("src", 0), st("between", 0), st("rescue", 0, 0), st("after", 0)},
			want:  map[string]keeperv1.TaskStatus{"src": tF, "between": tS, "rescue": tC, "after": tS},
			run:   rFailed,
		},
		{
			name:  "a unit that succeeds handles the failure and the ordinary tasks after it run",
			fail:  boom("src"),
			tasks: []*keeperv1.RenderedTask{st("src", 0), st("between", 0), st("rescue", 1, 0), st("after", 0)},
			want:  map[string]keeperv1.TaskStatus{"src": tF, "between": tS, "rescue": tC, "after": tC},
			run:   rOK,
		},
		{
			name:  "a failed unit gives the next unit of its source its chance",
			fail:  boom("src", "u1"),
			tasks: []*keeperv1.RenderedTask{st("src", 0), st("u1", 1, 0), st("u2", 2, 0), st("after", 0)},
			want:  map[string]keeperv1.TaskStatus{"src": tF, "u1": tF, "u2": tC, "after": tC},
			run:   rOK,
		},
		{
			name:  "no unit succeeds: the failure stays held and the ordinary tasks stay skipped",
			fail:  boom("src", "u1", "u2"),
			tasks: []*keeperv1.RenderedTask{st("src", 0), st("u1", 1, 0), st("u2", 2, 0), st("after", 0)},
			want:  map[string]keeperv1.TaskStatus{"src": tF, "u1": tF, "u2": tF, "after": tS},
			run:   rFailed,
		},
		{
			name:  "a unit's first failure skips its later tasks",
			fail:  boom("src", "u1a"),
			tasks: []*keeperv1.RenderedTask{st("src", 0), st("u1a", 1, 0), st("u1b", 1, 0), st("u2", 2, 0), st("after", 0)},
			want:  map[string]keeperv1.TaskStatus{"src": tF, "u1a": tF, "u1b": tS, "u2": tC, "after": tC},
			run:   rOK,
		},
		{
			name:  "the first unit that succeeds handles it: a later unit of the source is not evaluated",
			fail:  boom("src"),
			tasks: []*keeperv1.RenderedTask{st("src", 0), st("u1", 1, 0), withWhen(st("u2", 2, 0), whenErrors), st("after", 0)},
			want:  map[string]keeperv1.TaskStatus{"src": tF, "u1": tC, "u2": tS, "after": tC},
			run:   rOK,
		},
		{
			name:  "a unit whose source did not fail is not evaluated",
			tasks: []*keeperv1.RenderedTask{st("src", 0), withWhen(st("u1", 1, 0), whenErrors), st("after", 0)},
			want:  map[string]keeperv1.TaskStatus{"src": tC, "u1": tS, "after": tC},
			run:   rOK,
		},
		{
			name:  "no marks: an onfail task's when is read before its requisite",
			tasks: []*keeperv1.RenderedTask{st("src", 0), withWhen(st("rescue", 0, 0), whenErrors), st("after", 0)},
			want:  map[string]keeperv1.TaskStatus{"src": tC, "rescue": tF, "after": tS},
			run:   rFailed,
		},
		{
			name:  "a unit in which nothing executed has not succeeded",
			fail:  boom("src"),
			tasks: []*keeperv1.RenderedTask{st("src", 0), withWhen(st("u1", 1, 0), "false"), st("after", 0)},
			want:  map[string]keeperv1.TaskStatus{"src": tF, "u1": tS, "after": tS},
			run:   rFailed,
		},
		{
			name: "two sources: one handled, one not",
			fail: boom("a", "b", "ub"),
			tasks: []*keeperv1.RenderedTask{
				st("a", 0), st("ua", 1, 0), st("b", 0), st("ub", 2, 2), st("after", 0),
			},
			want: map[string]keeperv1.TaskStatus{"a": tF, "ua": tC, "b": tF, "ub": tF, "after": tS},
			run:  rFailed,
		},
		{
			name: "two sources: both handled",
			fail: boom("a", "b"),
			tasks: []*keeperv1.RenderedTask{
				st("a", 0), st("ua", 1, 0), st("b", 0), st("ub", 2, 2), st("after", 0),
			},
			want: map[string]keeperv1.TaskStatus{"a": tF, "ua": tC, "b": tF, "ub": tC, "after": tC},
			run:  rOK,
		},
		{
			// A block with onfail: [a] holding a task with onfail: [b]: the task
			// carries the outer mark, and the outer unit's failure leaves b
			// unhandled although the inner task, b's own unit, succeeded. ua then
			// handles a alone, so only b can keep `after` skipped.
			name: "nested units of different sources: the outermost decides both",
			fail: boom("a", "b", "o2"),
			tasks: []*keeperv1.RenderedTask{
				asAsync(st("a", 0)), st("b", 0),
				st("o1", 1, 0), st("inner", 1, 0, 1), st("o2", 1, 0), st("ua", 2, 0), st("after", 0),
			},
			want: map[string]keeperv1.TaskStatus{"a": tF, "b": tF, "o1": tC, "inner": tC, "o2": tF, "ua": tC, "after": tS},
			run:  rFailed,
		},
		{
			name: "nested units of different sources: the outer unit succeeding handles both",
			fail: boom("a", "b"),
			tasks: []*keeperv1.RenderedTask{
				asAsync(st("a", 0)), st("b", 0),
				st("o1", 1, 0), st("inner", 1, 0, 1), st("o2", 1, 0), st("after", 0),
			},
			want: map[string]keeperv1.TaskStatus{"a": tF, "b": tF, "o1": tC, "inner": tC, "o2": tC, "after": tC},
			run:  rOK,
		},
		{
			name: "nested units of different sources: the inner source alone fires the outer unit",
			fail: boom("b"),
			tasks: []*keeperv1.RenderedTask{
				st("a", 0), st("b", 0),
				st("o1", 1, 0), st("inner", 1, 0, 1), st("o2", 1, 0), st("after", 0),
			},
			want: map[string]keeperv1.TaskStatus{"a": tC, "b": tF, "o1": tS, "inner": tC, "o2": tS, "after": tC},
			run:  rOK,
		},
		{
			// The unit names both a and b; a was handled by ua, so a task naming
			// only a is settled by its requisite, and before its when:.
			name: "a unit is decided per source: a handled source no longer fires a task",
			fail: boom("a", "b"),
			tasks: []*keeperv1.RenderedTask{
				st("a", 0), st("ua", 1, 0), st("b", 0),
				withWhen(st("outer", 2, 0), whenErrors), st("inner", 2, 0, 2), st("after", 0),
			},
			want: map[string]keeperv1.TaskStatus{"a": tF, "ua": tC, "b": tF, "outer": tS, "inner": tC, "after": tC},
			run:  rOK,
		},
		{
			name:  "a unit task with no requisite of its own is skipped with its unit",
			tasks: []*keeperv1.RenderedTask{st("src", 0), st("u1", 1, 0), st("bare", 1), st("after", 0)},
			want:  map[string]keeperv1.TaskStatus{"src": tC, "u1": tS, "bare": tS, "after": tC},
			run:   rOK,
		},
		{
			name:  "a unit task with no requisite of its own runs with its unit",
			fail:  boom("src"),
			tasks: []*keeperv1.RenderedTask{st("src", 0), st("u1", 1, 0), st("bare", 1), st("after", 0)},
			want:  map[string]keeperv1.TaskStatus{"src": tF, "u1": tC, "bare": tC, "after": tC},
			run:   rOK,
		},
		{
			name:  "an async source is held where it is awaited, then a unit handles it",
			fail:  boom("a"),
			tasks: []*keeperv1.RenderedTask{asAsync(st("a", 0)), st("x", 0), st("ua", 1, 0), st("y", 0)},
			want:  map[string]keeperv1.TaskStatus{"a": tF, "x": tC, "ua": tC, "y": tC},
			run:   rOK,
		},
		{
			name:  "an async source no unit awaits is held by the final barrier",
			fail:  boom("a"),
			tasks: []*keeperv1.RenderedTask{asAsync(st("a", 0)), st("x", 0), st("u", 1)},
			want:  map[string]keeperv1.TaskStatus{"a": tF, "x": tC, "u": tS},
			run:   rFailed,
		},
		{
			name:  "a unit's async task is awaited before the unit is decided",
			fail:  boom("src", "u1"),
			tasks: []*keeperv1.RenderedTask{st("src", 0), asAsync(st("u1", 1, 0)), st("u2", 1, 0), st("after", 0)},
			want:  map[string]keeperv1.TaskStatus{"src": tF, "u1": tF, "u2": tC, "after": tS},
			run:   rFailed,
		},
		{
			name:  "a unit of one async task that succeeds handles the source",
			fail:  boom("src"),
			tasks: []*keeperv1.RenderedTask{st("src", 0), asAsync(st("u1", 1, 0)), st("after", 0)},
			want:  map[string]keeperv1.TaskStatus{"src": tF, "u1": tC, "after": tC},
			run:   rOK,
		},
		{
			name:  "a unit that ends the message is decided",
			fail:  boom("src"),
			tasks: []*keeperv1.RenderedTask{st("src", 0), st("u1", 1, 0)},
			want:  map[string]keeperv1.TaskStatus{"src": tF, "u1": tC},
			run:   rOK,
		},
		{
			name: "an async source awaited by require: is held there",
			fail: boom("a"),
			tasks: []*keeperv1.RenderedTask{
				asAsync(st("a", 0)), requires(st("y", 0), 0), st("z", 0), st("u", 1),
			},
			want: map[string]keeperv1.TaskStatus{"a": tF, "y": tS, "z": tS, "u": tS},
			run:  rFailed,
		},
		{
			name:  "a when: error is a source like any failure",
			tasks: []*keeperv1.RenderedTask{withWhen(st("src", 0), whenErrors), st("u1", 1, 0), st("after", 0)},
			want:  map[string]keeperv1.TaskStatus{"src": tF, "u1": tC, "after": tC},
			run:   rOK,
		},
		{
			name: "a when: error in a unit fails the unit, not the run",
			fail: boom("src"),
			tasks: []*keeperv1.RenderedTask{
				st("src", 0), st("u1a", 1, 0), withWhen(st("u1b", 1, 0), whenErrors), st("u2", 2, 0), st("after", 0),
			},
			want: map[string]keeperv1.TaskStatus{"src": tF, "u1a": tC, "u1b": tF, "u2": tC, "after": tC},
			run:  rOK,
		},
		{
			name:  "an unmarked onfail: task is not satisfied by a source a unit handled",
			fail:  boom("src"),
			tasks: []*keeperv1.RenderedTask{st("src", 0), st("u1", 1, 0), st("late", 0, 0), st("after", 0)},
			want:  map[string]keeperv1.TaskStatus{"src": tF, "u1": tC, "late": tS, "after": tC},
			run:   rOK,
		},
		{
			name:  "a loop source: a unit fires when one iteration failed",
			fail:  boom("l1"),
			tasks: []*keeperv1.RenderedTask{iter("l0"), iter("l1"), iter("l2"), st("u", 1, 0, 1, 2), st("after", 0)},
			want:  map[string]keeperv1.TaskStatus{"l0": tC, "l1": tF, "l2": tS, "u": tC, "after": tC},
			run:   rOK,
		},
		{
			name: "a loop source: a unit that succeeds handles every failed iteration",
			fail: boom("l0", "l2"),
			tasks: []*keeperv1.RenderedTask{
				asAsync(iter("l0")), asAsync(iter("l1")), asAsync(iter("l2")), st("u", 1, 0, 1, 2), st("after", 0),
			},
			want: map[string]keeperv1.TaskStatus{"l0": tF, "l1": tC, "l2": tF, "u": tC, "after": tC},
			run:  rOK,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := runPlan(t, tc.fail, tc.tasks)
			for name, want := range tc.want {
				if got := sink.byName(t, tc.tasks, name).GetStatus(); got != want {
					t.Errorf("%s = %v, want %v", name, got, want)
				}
			}
			if got := sink.result.GetStatus(); got != tc.run {
				t.Errorf("RunResult = %v, want %v", got, tc.run)
			}
		})
	}
}

// A handled failure is not undone (rule 5): the source still reports FAILED,
// with its error, and its register keeps failed: true for later readers.
func TestRescueUnits_HandledSourceStillReportsItsFailure(t *testing.T) {
	tasks := []*keeperv1.RenderedTask{st("src", 0), st("rescue", 1, 0), withWhen(st("reads", 0), "register.src.failed")}
	sink := runPlan(t, map[string]string{"src": "disk full"}, tasks)

	src := sink.byName(t, tasks, "src")
	if src.GetStatus() != tF || src.GetError().GetMessage() != "disk full" {
		t.Errorf("src = %v %q, want FAILED with its error", src.GetStatus(), src.GetError().GetMessage())
	}
	if !src.GetRegisterData().GetFields()["failed"].GetBoolValue() {
		t.Error("src register failed = false, want true")
	}
	if got := sink.byName(t, tasks, "reads").GetStatus(); got != tC {
		t.Errorf("a task gated on register.src.failed = %v, want CHANGED", got)
	}
	if got := sink.result.GetStatus(); got != rOK {
		t.Errorf("RunResult = %v, want SUCCESS", got)
	}
}

// An applier fails only by an unhandled failure of its children (rule 6).
func TestRescueUnits_ApplierFold(t *testing.T) {
	cases := []struct {
		name       string
		fail       map[string]string
		tasks      []*keeperv1.RenderedTask
		wantFailed bool
		run        keeperv1.RunStatus
	}{
		{
			name:       "a child source the destiny's unit handled does not count",
			fail:       map[string]string{"c0": "x"},
			tasks:      []*keeperv1.RenderedTask{st("c0", 0), st("c1", 1, 0), terminal(0, 1)},
			wantFailed: false,
			run:        rOK,
		},
		{
			name:       "no marks: every child failure counts",
			fail:       map[string]string{"c0": "x"},
			tasks:      []*keeperv1.RenderedTask{st("c0", 0), st("c1", 0, 0), terminal(0, 1)},
			wantFailed: true,
			run:        rFailed,
		},
		{
			name:       "a child source no unit handled counts",
			fail:       map[string]string{"c0": "x", "c1": "y"},
			tasks:      []*keeperv1.RenderedTask{st("c0", 0), st("c1", 1, 0), terminal(0, 1)},
			wantFailed: true,
			run:        rFailed,
		},
		{
			name:       "a failed unit whose source a later unit handled does not count",
			fail:       map[string]string{"c0": "x", "c1": "y"},
			tasks:      []*keeperv1.RenderedTask{st("c0", 0), st("c1", 1, 0), st("c2", 2, 0), terminal(0, 1, 2)},
			wantFailed: false,
			run:        rOK,
		},
		{
			// s is observed at x's barrier after the destiny handled its own c0, so
			// the terminal folds while a failure outside it is held.
			name: "a handled child does not count while another failure is held",
			fail: map[string]string{"s": "x", "c0": "y"},
			tasks: []*keeperv1.RenderedTask{
				asAsync(st("s", 0)), st("c0", 0), st("c1", 1, 1), withWhen(st("x", 0), "register.s.changed"), terminal(1, 2),
			},
			wantFailed: false,
			run:        rFailed,
		},
		{
			// The destiny is itself the unit of a source outside it: its failure
			// means that source is not handled.
			name:       "a failed unit whose source is still held counts",
			fail:       map[string]string{"s": "x", "c0": "y"},
			tasks:      []*keeperv1.RenderedTask{st("s", 0), st("c0", 1, 0), terminal(1)},
			wantFailed: true,
			run:        rFailed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := runPlan(t, tc.fail, tc.tasks)
			app := sink.byName(t, tc.tasks, "app")
			if got := app.GetRegisterData().GetFields()["failed"].GetBoolValue(); got != tc.wantFailed {
				t.Errorf("register.app.failed = %v, want %v", got, tc.wantFailed)
			}
			if app.GetStatus() != tO {
				t.Errorf("terminal = %v, want OK (it is never reported FAILED)", app.GetStatus())
			}
			if got := sink.result.GetStatus(); got != tc.run {
				t.Errorf("RunResult = %v, want %v", got, tc.run)
			}
		})
	}
}

// A unit naming an applier names the applier's children: the terminal is never
// reported FAILED, so it is never held itself (rule 6). The terminal keeps the
// failed fold it recorded, as a handled source keeps its failure (rule 5).
func TestRescueUnits_UnitNamingAnApplier(t *testing.T) {
	cases := []struct {
		name      string
		fail      map[string]string
		tasks     []*keeperv1.RenderedTask
		want      map[string]keeperv1.TaskStatus
		appFailed bool
		run       keeperv1.RunStatus
	}{
		{
			name:      "fires when a child failed, and handles it",
			fail:      map[string]string{"c0": "x"},
			tasks:     []*keeperv1.RenderedTask{st("c0", 0), st("c1", 0), terminal(0, 1), st("r", 1, 2), st("after", 0)},
			want:      map[string]keeperv1.TaskStatus{"c0": tF, "c1": tS, "r": tC, "after": tC},
			appFailed: true,
			run:       rOK,
		},
		{
			name:  "does not fire when no child failed",
			tasks: []*keeperv1.RenderedTask{st("c0", 0), st("c1", 0), terminal(0, 1), withWhen(st("r", 1, 2), whenErrors), st("after", 0)},
			want:  map[string]keeperv1.TaskStatus{"c0": tC, "c1": tC, "r": tS, "after": tC},
			run:   rOK,
		},
		{
			name:  "does not fire when the destiny handled its own failure",
			fail:  map[string]string{"c0": "x"},
			tasks: []*keeperv1.RenderedTask{st("c0", 0), st("c1", 1, 0), terminal(0, 1), withWhen(st("r", 2, 2), whenErrors), st("after", 0)},
			want:  map[string]keeperv1.TaskStatus{"c0": tF, "c1": tC, "r": tS, "after": tC},
			run:   rOK,
		},
		{
			name:      "fires when the destiny's own unit failed, and handles what it left",
			fail:      map[string]string{"c0": "x", "c1": "y"},
			tasks:     []*keeperv1.RenderedTask{st("c0", 0), st("c1", 1, 0), terminal(0, 1), st("r", 2, 2), st("after", 0)},
			want:      map[string]keeperv1.TaskStatus{"c0": tF, "c1": tF, "r": tC, "after": tC},
			appFailed: true,
			run:       rOK,
		},
		{
			name:      "a unit naming an applier that fails leaves the child held",
			fail:      map[string]string{"c0": "x", "r": "y"},
			tasks:     []*keeperv1.RenderedTask{st("c0", 0), st("c1", 0), terminal(0, 1), st("r", 1, 2), st("after", 0)},
			want:      map[string]keeperv1.TaskStatus{"c0": tF, "c1": tS, "r": tF, "after": tS},
			appFailed: true,
			run:       rFailed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := runPlan(t, tc.fail, tc.tasks)
			for name, want := range tc.want {
				if got := sink.byName(t, tc.tasks, name).GetStatus(); got != want {
					t.Errorf("%s = %v, want %v", name, got, want)
				}
			}
			app := sink.byName(t, tc.tasks, "app")
			if got := app.GetRegisterData().GetFields()["failed"].GetBoolValue(); got != tc.appFailed {
				t.Errorf("register.app.failed = %v, want %v", got, tc.appFailed)
			}
			if got := sink.result.GetStatus(); got != tc.run {
				t.Errorf("RunResult = %v, want %v", got, tc.run)
			}
		})
	}
}

// core.task.failed in a unit defaults its message to the original error of the
// sources its unit holds, joined in plan order (rule 8).
func TestRescueUnits_TaskFailedDefaultMessage(t *testing.T) {
	taskFailed := func(unit int32, message *structpb.Value, onfail ...int32) *keeperv1.RenderedTask {
		task := &keeperv1.RenderedTask{
			Name: "tf", Module: "core.task.failed", RescueUnit: unit, OnfailIdx: onfail,
			Params: &structpb.Struct{Fields: map[string]*structpb.Value{}},
		}
		if message != nil {
			task.Params.Fields["message"] = message
		}
		return task
	}
	cases := []struct {
		name  string
		fail  map[string]string
		tasks []*keeperv1.RenderedTask
		want  string
	}{
		{
			name:  "the held source's error",
			fail:  map[string]string{"src": "disk full"},
			tasks: []*keeperv1.RenderedTask{st("src", 0), taskFailed(1, nil, 0)},
			want:  "disk full",
		},
		{
			name:  "an explicit message wins",
			fail:  map[string]string{"src": "disk full"},
			tasks: []*keeperv1.RenderedTask{st("src", 0), taskFailed(1, structpb.NewStringValue("custom"), 0)},
			want:  "custom",
		},
		{
			name:  "an empty message is no message",
			fail:  map[string]string{"src": "disk full"},
			tasks: []*keeperv1.RenderedTask{st("src", 0), taskFailed(1, structpb.NewStringValue(""), 0)},
			want:  "disk full",
		},
		{
			name:  "a null message is no message",
			fail:  map[string]string{"src": "disk full"},
			tasks: []*keeperv1.RenderedTask{st("src", 0), taskFailed(1, structpb.NewNullValue(), 0)},
			want:  "disk full",
		},
		{
			// b fails first in time, a is observed later at the unit's barrier;
			// both are named by both tasks of the unit.
			name: "several held sources, once each, in plan order",
			fail: map[string]string{"a": "a broke", "b": "b broke"},
			tasks: []*keeperv1.RenderedTask{
				asAsync(st("a", 0)), st("b", 0), st("t1", 1, 1, 0), taskFailed(1, nil, 0, 1),
			},
			want: "a broke; b broke",
		},
		{
			name: "a source a unit already handled is not in it",
			fail: map[string]string{"a": "a broke", "b": "b broke"},
			tasks: []*keeperv1.RenderedTask{
				st("a", 0), st("ua", 1, 0), st("b", 0), taskFailed(2, nil, 0, 2),
			},
			want: "b broke",
		},
		{
			name: "a unit naming an applier: its held children's errors",
			fail: map[string]string{"c0": "c0 broke", "c1": "c1 broke"},
			tasks: []*keeperv1.RenderedTask{
				asAsync(st("c0", 0)), asAsync(st("c1", 0)), terminal(0, 1), taskFailed(1, nil, 2),
			},
			want: "c0 broke; c1 broke",
		},
		{
			name: "a unit naming a loop: every failed iteration's error",
			fail: map[string]string{"l0": "l0 broke", "l2": "l2 broke"},
			tasks: []*keeperv1.RenderedTask{
				asAsync(iter("l0")), asAsync(iter("l1")), asAsync(iter("l2")), taskFailed(1, nil, 0, 1, 2),
			},
			want: "l0 broke; l2 broke",
		},
		{
			name:  "no marks: the module's generic text",
			fail:  map[string]string{"src": "disk full"},
			tasks: []*keeperv1.RenderedTask{st("src", 0), taskFailed(0, nil, 0)},
			want:  coretask.GenericMessage,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := runPlan(t, tc.fail, tc.tasks)
			ev := sink.byName(t, tc.tasks, "tf")
			if ev.GetStatus() != tF {
				t.Fatalf("tf = %v, want FAILED", ev.GetStatus())
			}
			if got := ev.GetError().GetMessage(); got != tc.want {
				t.Errorf("message = %q, want %q", got, tc.want)
			}
			if sink.result.GetStatus() != rFailed {
				t.Errorf("RunResult = %v, want FAILED (core.task.failed fails its unit)", sink.result.GetStatus())
			}
		})
	}
}

// The request's own task is the Keeper's message; filling the default must not
// write into it.
func TestRescueUnits_DefaultMessageLeavesTheRequestAlone(t *testing.T) {
	tf := &keeperv1.RenderedTask{Name: "tf", Module: "core.task.failed", RescueUnit: 1, OnfailIdx: []int32{0}}
	runPlan(t, map[string]string{"src": "disk full"}, []*keeperv1.RenderedTask{st("src", 0), tf})
	if tf.GetParams() != nil {
		t.Errorf("request task params = %v, want untouched (nil)", tf.GetParams())
	}
}

// Cancel ends the message at once, and a held failure does not turn it into
// FAILED (rule 9).
func TestRescueUnits_CancelStaysCancelled(t *testing.T) {
	tasks := []*keeperv1.RenderedTask{st("src", 0), st("u1", 1, 0), st("after", 0)}
	r := NewApplyRunner(scriptRegistry(map[string]string{"src": "x"}), nil)
	sink := &eventSink{onTask: func(ev *keeperv1.TaskEvent) {
		if ev.GetTaskIdx() == 1 {
			r.Cancel("rescue-unit")
		}
	}}
	if err := r.Run(context.Background(), &keeperv1.ApplyRequest{ApplyId: "rescue-unit", Tasks: tasks}, sink); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := sink.result.GetStatus(); got != keeperv1.RunStatus_RUN_STATUS_CANCELLED {
		t.Errorf("RunResult = %v, want CANCELLED", got)
	}
}

// hangModule cancels the run from inside Apply and holds its task until the
// cancel reaches it, so the task is provably in flight when the run is
// cancelled.
type hangModule struct {
	module.BaseModule
	cancel func()
}

func (m *hangModule) Apply(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	m.cancel()
	<-stream.Context().Done()
	return stream.Context().Err()
}

// A Cancel that reaches a unit's async task still in flight when the loop ends
// leaves the run CANCELLED (rule 9): neither SUCCESS, as if the cancelled task
// had handled the source, nor FAILED for the source it left held.
func TestRescueUnits_CancelDuringAUnitsAsyncTask(t *testing.T) {
	hang := &hangModule{}
	reg := scriptRegistry(map[string]string{"src": "x"})
	reg["core.hang"] = hang
	r := NewApplyRunner(reg, nil)
	hang.cancel = func() { r.Cancel("rescue-unit") }
	u1 := &keeperv1.RenderedTask{
		Name: "u1", Module: "core.hang.run", Register: "u1", RescueUnit: 1, OnfailIdx: []int32{0}, Async: true,
	}
	tasks := []*keeperv1.RenderedTask{st("src", 0), u1}

	sink := runWith(t, r, tasks)
	if got := sink.byName(t, tasks, "u1").GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_CANCELLED {
		t.Errorf("u1 = %v, want CANCELLED", got)
	}
	if got := sink.result.GetStatus(); got != keeperv1.RunStatus_RUN_STATUS_CANCELLED {
		t.Errorf("RunResult = %v, want CANCELLED", got)
	}
}

// A Cancel that lands once every task and flow has finished stopped nothing, so
// the settled verdict stands, as it does on the unmarked path. The Cancel is
// issued from the last TaskEvent: that task's status is already decided, and no
// loop iteration is left to see the dead context.
func TestRescueUnits_CancelAfterEverythingFinishedKeepsTheVerdict(t *testing.T) {
	cases := []struct {
		name  string
		fail  map[string]string
		tasks []*keeperv1.RenderedTask
		run   keeperv1.RunStatus
	}{
		{
			name:  "a source left held stays FAILED",
			fail:  map[string]string{"src": "x", "u1": "y"},
			tasks: []*keeperv1.RenderedTask{st("src", 0), st("u1", 1, 0)},
			run:   rFailed,
		},
		{
			name:  "a source handled by a unit's finished async task stays SUCCESS",
			fail:  map[string]string{"src": "x"},
			tasks: []*keeperv1.RenderedTask{st("src", 0), asAsync(st("u1", 1, 0))},
			run:   rOK,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewApplyRunner(scriptRegistry(tc.fail), nil)
			last := int32(len(tc.tasks) - 1)
			sink := &eventSink{onTask: func(ev *keeperv1.TaskEvent) {
				if ev.GetTaskIdx() == last {
					r.Cancel("rescue-unit")
				}
			}}
			if err := r.Run(context.Background(), &keeperv1.ApplyRequest{ApplyId: "rescue-unit", Tasks: tc.tasks}, sink); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got := sink.byName(t, tc.tasks, "u1").GetStatus(); got == keeperv1.TaskStatus_TASK_STATUS_CANCELLED {
				t.Fatalf("u1 = CANCELLED, want it finished before the Cancel")
			}
			if got := sink.result.GetStatus(); got != tc.run {
				t.Errorf("RunResult = %v, want %v", got, tc.run)
			}
		})
	}
}

// An async task that a Cancel stops leaves its flow a cancelled verdict, not a
// failed one.
func TestRescueUnits_AsyncFlowRecordsCancelled(t *testing.T) {
	r := NewApplyRunner(scriptRegistry(nil), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	flows := newAsyncFlows(0, r.registerRefs)
	fl := r.launchAsync(ctx, flows, &lockedSink{sink: &eventSink{}}, "rescue-unit", 0, 0, st("u1", 1), newRegisterIndex(1), nil)
	flows.wait()
	if !fl.cancelled || fl.failed {
		t.Errorf("flow cancelled=%v failed=%v, want cancelled and not failed", fl.cancelled, fl.failed)
	}
}

// The same unit, decided: its one task was launched and ended CANCELLED, which
// is no success, so the source stays held.
func TestRescueUnits_CancelledTaskHandlesNothing(t *testing.T) {
	x := newRescueUnits([]*keeperv1.RenderedTask{st("src", 0), asAsync(st("u1", 1, 0))})
	x.fail(0, &keeperv1.TaskError{Message: "x"})
	fl := &asyncFlow{idx: 1, done: make(chan struct{}), cancelled: true}
	close(fl.done)
	x.launched(1, fl)
	x.settle(2)
	if !x.held(0) {
		t.Error("src handled by a unit whose only task was cancelled")
	}
}

// A skipped unit task is counted by why: requisite when its unit does not fire,
// read before its when:; failed_run once its unit has failed; when for a when:
// false in a unit that fires.
func TestRescueUnits_SkipReasons(t *testing.T) {
	cases := []struct {
		name  string
		fail  map[string]string
		tasks []*keeperv1.RenderedTask
		want  map[string]int
	}{
		{
			name:  "a unit that does not fire",
			tasks: []*keeperv1.RenderedTask{st("src", 0), withWhen(st("u1", 1, 0), whenErrors), st("after", 0)},
			want:  map[string]int{skipReasonRequisite: 1},
		},
		{
			// u1b skips with its failed unit, `after` with the held source.
			name: "a failed unit's later task, and a when: false in a unit that fires",
			fail: map[string]string{"src": "x", "u1a": "y"},
			tasks: []*keeperv1.RenderedTask{
				st("src", 0), st("u1a", 1, 0), st("u1b", 1, 0), withWhen(st("u2", 2, 0), "false"), st("after", 0),
			},
			want: map[string]int{skipReasonFailedRun: 2, skipReasonWhen: 1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := obs.NewRegistry()
			runWith(t, NewApplyRunner(scriptRegistry(tc.fail), RegisterApplyMetrics(reg)), tc.tasks)
			body := obstest.Scrape(t, reg.Gatherer())
			for _, reason := range []string{skipReasonWhen, skipReasonRequisite, skipReasonFailedRun} {
				n := tc.want[reason]
				if n == 0 {
					if strings.Contains(body, fmt.Sprintf("reason=%q", reason)) {
						t.Errorf("reason %q counted, want none; got=\n%s", reason, body)
					}
					continue
				}
				if line := fmt.Sprintf("soul_apply_task_skipped_total{reason=%q} %d\n", reason, n); !strings.Contains(body, line) {
					t.Errorf("missing %q; got=\n%s", line, body)
				}
			}
		})
	}
}
