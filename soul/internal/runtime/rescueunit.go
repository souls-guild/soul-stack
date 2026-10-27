package runtime

import (
	"sort"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/shared/config"
	coretask "github.com/souls-guild/soul-stack/soul/internal/coremod/task"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// rescueUnits drives one message's [config.RescueRule] (ADR-0091 amendment
// NIM-993, rule 6) in message order. Run holds a nil one for a message in which
// no task carries a `rescue_unit` mark, and then keeps fail-stop exactly as
// before the amendment — that is how a message from a Keeper that sets no marks
// keeps its meaning.
//
// Only the main flow touches it. An async flow reports through its [asyncFlow],
// and the main flow folds that in where it awaits the flow, which is also where
// ADR-0075(c) says a failure is observed.
type rescueUnits struct {
	rule  *config.RescueRule
	units map[int]*rescueUnit
	// order lists the units by their last position; next is the first one not
	// decided yet. Units nest as whole subtrees and a nested one carries the outer
	// mark, so the positions of two units never interleave.
	order []*rescueUnit
	next  int
}

type rescueUnit struct {
	id   int
	last int32
	// flows are its launched async tasks, awaited at its last position.
	flows []*asyncFlow
}

// newRescueUnits returns nil when no task in the message carries a mark.
func newRescueUnits(tasks []*keeperv1.RenderedTask) *rescueUnits {
	var x *rescueUnits
	for i, t := range tasks {
		id := int(t.GetRescueUnit())
		if id == 0 {
			continue
		}
		if x == nil {
			x = &rescueUnits{rule: config.NewRescueRule(), units: make(map[int]*rescueUnit)}
		}
		u := x.units[id]
		if u == nil {
			u = &rescueUnit{id: id}
			x.units[id] = u
			x.order = append(x.order, u)
		}
		u.last = int32(i)
	}
	if x == nil {
		return nil
	}
	for i, t := range tasks {
		agg := t.GetAggregateOf()
		x.rule.Task(i, int(t.GetRescueUnit()), positions(t.GetOnfailIdx()), len(agg) > 0, positions(agg))
	}
	sort.Slice(x.order, func(a, b int) bool { return x.order[a].last < x.order[b].last })
	return x
}

func positions(idx []int32) []int {
	if len(idx) == 0 {
		return nil
	}
	out := make([]int, len(idx))
	for i, v := range idx {
		out[i] = int(v)
	}
	return out
}

func (x *rescueUnits) unitAt(idx int32) *rescueUnit {
	return x.units[x.rule.UnitOf(int(idx))]
}

func (x *rescueUnits) held(idx int32) bool { return x.rule.Held(int(idx)) }

func (x *rescueUnits) anyHeld() bool { return x.rule.AnyHeld() }

// fail records a failed task the main flow observed. An async flow can be
// awaited by several barriers; the rule's Fail is idempotent for that.
func (x *rescueUnits) fail(idx int32, err *keeperv1.TaskError) {
	x.rule.Fail(int(idx), err.GetMessage())
}

// observe folds in the awaited flows that failed.
func (x *rescueUnits) observe(flows []*asyncFlow) {
	for _, fl := range flows {
		if fl.failed {
			x.fail(fl.idx, fl.err)
		}
	}
}

// ran records the outcome of a task the main flow executed.
func (x *rescueUnits) ran(idx int32, ev *keeperv1.TaskEvent) {
	x.rule.Ran(int(idx), ev.GetStatus() != keeperv1.TaskStatus_TASK_STATUS_SKIPPED)
	if failedStatus(ev.GetStatus()) {
		x.fail(idx, ev.GetError())
	}
}

// launched records an async task: it executes whatever its outcome, and its
// unit, if any, waits for it before being decided.
func (x *rescueUnits) launched(idx int32, fl *asyncFlow) {
	if u := x.unitAt(idx); u != nil {
		x.rule.Ran(int(idx), true)
		u.flows = append(u.flows, fl)
	}
}

// settle decides every unit whose last position is before pos, in position
// order, which is rule 3's plan order. A unit's async tasks are awaited first
// (rule 1), and one a Cancel stopped leaves its unit handling nothing: no rescue
// runs on Cancel (rule 9).
func (x *rescueUnits) settle(pos int32) {
	for x.next < len(x.order) && x.order[x.next].last < pos {
		u := x.order[x.next]
		x.next++
		waitFlows(u.flows)
		x.observe(u.flows)
		x.rule.Decide(u.id, anyCancelled(u.flows))
	}
}

// skipUnitTask reports whether a unit task is settled SKIPPED, and the metric
// reason; false for a task of no unit. The requisite is read before `when:` for
// a unit task only (rule 1, a unit task with a source in its own message): a unit
// that does not fire is not evaluated, so a `when:` reading an output of its own
// unit does not fail a host on which nothing failed.
func (x *rescueUnits) skipUnitTask(idx int32, task *keeperv1.RenderedTask) (string, bool) {
	u := x.rule.UnitOf(int(idx))
	switch {
	case u == 0:
		return "", false
	case x.rule.UnitFailed(u):
		return skipReasonFailedRun, true
	case !x.rule.Fires(u) || x.onfailUnmet(idx, task, nil):
		return skipReasonRequisite, true
	}
	return "", false
}

// onfailUnmet is [skipOnFail] for this message. Nil-safe: without marks it is
// skipOnFail itself. With them, a source a unit handled no longer satisfies an
// `onfail:` — the same reading the Keeper's requisite gate makes across steps
// (rule 6, what the run goroutine keeps) — and a unit task's own failure never
// does, because it is not a source.
func (x *rescueUnits) onfailUnmet(idx int32, task *keeperv1.RenderedTask, state *registerIndex) bool {
	if x == nil {
		return skipOnFail(task.GetOnfailIdx(), state)
	}
	return len(task.GetOnfailIdx()) > 0 && !x.rule.NamesHeld(int(idx))
}

// failureCounts reports whether a child's failure counts toward its applier's
// register ([config.RescueRule.Counts]). Nil-safe: without marks every failure
// counts.
func (x *rescueUnits) failureCounts(idx int32) bool {
	return x == nil || x.rule.Counts(int(idx))
}

// withDefaultMessage gives a `core.task.failed` unit task with no `message:` the
// original error: what the sources its unit holds recorded, joined in plan order
// (rule 8). The request's own task is never mutated.
func (x *rescueUnits) withDefaultMessage(idx int32, task *keeperv1.RenderedTask) *keeperv1.RenderedTask {
	u := x.rule.UnitOf(int(idx))
	if u == 0 {
		return task
	}
	mod, state, ok := config.SplitModuleAddr(task.GetModule())
	if !ok || mod != coretask.Name || state != "failed" || hasMessage(task.GetParams()) {
		return task
	}
	msg := x.rule.HeldErrors(u)
	if msg == "" {
		return task
	}
	out := proto.Clone(task).(*keeperv1.RenderedTask)
	if out.Params == nil {
		out.Params = &structpb.Struct{}
	}
	if out.Params.Fields == nil {
		out.Params.Fields = map[string]*structpb.Value{}
	}
	out.Params.Fields["message"] = structpb.NewStringValue(msg)
	return out
}

// hasMessage: an absent, null or empty `message:` is no message. Any other value
// is the author's, and the module's own validation judges it.
func hasMessage(params *structpb.Struct) bool {
	v, ok := params.GetFields()["message"]
	if !ok {
		return false
	}
	switch k := v.GetKind().(type) {
	case nil, *structpb.Value_NullValue:
		return false
	case *structpb.Value_StringValue:
		return k.StringValue != ""
	}
	return true
}
