package render

import (
	"errors"
	"fmt"
	"slices"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Flow control settled on the Keeper (ADR-0091, decision 4;
// docs/scenario/predicate-rendering.md). Under linear dispatch no register crosses
// the wire: a step's `when:` is decided here, per host, against that host's
// accumulated registers and its soulprint.self, and `changed_when:`, `failed_when:`
// and `retry.until:` are rendered here with only `register.self` left open. The Soul
// receives text that reads nothing it does not have.
//
// Only the step being dispatched is settled: an earlier step's task is re-rendered
// on every pass, and re-evaluating its `when:` against registers written since could
// fail the run over a task that has already run.

// HostFlow is one task's flow control on one host, as the Keeper settled it.
type HostFlow struct {
	// Skip — `when:` is false on this host, so the task does not run there.
	Skip bool
	// Unrendered — with Skip: a cross-step rescue unit on a live host, where it
	// cannot fire (ADR-0091 amendment NIM-993, rule 1). Nothing of it was rendered
	// for this host, so a copy sent as `when: false` carries empty params.
	Unrendered bool
	// Failure — the task fails on this host before dispatch: its `when:` could not be
	// evaluated, or a predicate could not be rendered.
	Failure *FlowFailure
	// When, ChangedWhen, FailedWhen, Until — the text this host is sent. When is empty
	// for a `when:` the Keeper decided, and the rendered residual for a grouped child's
	// `when:` reading a sibling's register.
	When        string
	ChangedWhen string
	FailedWhen  string
	Until       string
	// Rendered — a value was substituted into at least one of them, so the text is
	// this host's alone and needs a Soul that reads a rendered predicate (NIM-946).
	Rendered bool
}

// FlowFailure is why a task fails on a host before dispatch. Message quotes the
// author's predicate and never the rendered text, which can hold a substituted secret.
type FlowFailure struct {
	// Predicate — which one failed: when, changed_when, failed_when or until. The
	// dispatcher orders by it as the Soul does: a `when:` failure stands whatever the
	// requisites say, a post-module one only on a host where the task would run.
	Predicate string
	Code      string
	Message   string
}

// predicateWhen names the `when:` in [FlowFailure.Predicate].
const predicateWhen = "when"

// WhenFailed reports whether the failure is the task's `when:`.
func (f *FlowFailure) WhenFailed() bool {
	return f != nil && f.Predicate == predicateWhen
}

// keeperFlow reports whether this pass settles flow control on the Keeper: a scenario
// pass under linear dispatch. A destiny's tasks travel whole and the Soul keeps
// evaluating their predicates there (decision 6).
func (in RenderInput) keeperFlow() bool {
	return in.Steps != nil && !in.destinyIsolated
}

// settleFlow fills [RenderedTask.FlowBySID] for every task of the step being
// dispatched, over the hosts its plan targets. A failure is captured per task and
// host rather than returned: it fails that host's task, judged by the run's
// on_failure, not the whole render.
func (p *Pipeline) settleFlow(in RenderInput, tasks []*RenderedTask, plans []DispatchPlan) error {
	if !in.keeperFlow() {
		return nil
	}
	soul, err := flowControlEngine()
	if err != nil {
		return fmt.Errorf("render: building the flow-control engine: %w", err)
	}
	keeper, err := keeperWhenEngine()
	if err != nil {
		return fmt.Errorf("render: building the flow-control engine: %w", err)
	}
	targets := make(map[int][]string, len(plans))
	for _, pl := range plans {
		if !pl.Keeper {
			targets[pl.TaskIndex] = pl.TargetSIDs
		}
	}
	for _, t := range tasks {
		contexts := t.flowContextBySID
		t.flowContextBySID = nil
		if (contexts == nil && t.flowFailure == nil && t.skipSIDs == nil) || (in.TaskPassage != nil && t.Passage != in.ActivePassage) {
			continue
		}
		sids := targets[t.Index]
		if len(sids) == 0 {
			continue
		}
		t.FlowBySID = make(map[string]HostFlow, len(sids))
		for _, sid := range sids {
			switch {
			case t.skipSIDs[sid]:
				t.FlowBySID[sid] = HostFlow{Skip: true, Unrendered: true}
			case t.flowFailure != nil:
				t.FlowBySID[sid] = HostFlow{Failure: t.flowFailure}
			case contexts != nil:
				host := &topology.HostFacts{SID: sid}
				full := flowControlVarsFromStruct(contexts[sid], hostRegister(in, host))
				full.InMaintenanceWindow = in.inMaintenanceWindow()
				t.FlowBySID[sid] = settleHostFlow(keeper, soul, t, full)
			}
		}
	}
	return nil
}

// staticWhenEvalError is a static `when:` that failed to evaluate, as opposed to
// failing to build the context it evaluates in.
type staticWhenEvalError struct {
	task, when string
	err        error
}

func (e *staticWhenEvalError) Error() string {
	return fmt.Sprintf("render: task %q: static-when %q: %v", e.task, e.when, e.err)
}

func (e *staticWhenEvalError) Unwrap() error { return e.err }

// staticWhenCause is the evaluation error under a [staticWhenEvalError].
func staticWhenCause(err error) error {
	var evalErr *staticWhenEvalError
	if errors.As(err, &evalErr) {
		return evalErr.err
	}
	return err
}

// settlesStaticWhenError reports whether a static `when:` that failed to evaluate
// fails the task per host instead of the render: under linear dispatch, for a module
// task rendered once by renderTaskIter (an applier has no module). An applier's and a
// keeper task's static `when:` is settled by the skip alone — nothing downstream
// reads it, and they would run — and a loop's `loop.when` can leave no iteration to
// carry the failure; for those the render still fails.
func settlesStaticWhenError(in RenderInput, task config.Task, err error) bool {
	var evalErr *staticWhenEvalError
	return in.keeperFlow() && task.Module != nil && task.Loop == nil && !IsKeeperTask(task) && errors.As(err, &evalErr)
}

// settleHostFlow settles one task on one host against that host's full context.
//
// `when:` first, as the Soul orders it: an error there fails the task whatever the
// requisites say, and a false one leaves nothing else to render. A grouped child's
// `when:` reading a sibling's register cannot be decided before the sibling runs, so
// it is rendered with the group's registers open and the Soul decides it — and a
// post-module predicate that cannot be rendered then fails the task although that
// `when:` might have skipped it.
//
// keeper decides a `when:` here and soul renders what a Soul evaluates
// ([keeperWhenEngine]): in_maintenance_window() is answered in the first and refused by
// name in the second.
func settleHostFlow(keeper, soul *cel.Engine, t *RenderedTask, full cel.Vars) HostFlow {
	var hf HostFlow
	if t.When != "" {
		if readsAnyRegister(t.When, t.groupRegisters) {
			rp, err := soul.RenderPredicate(t.When, withoutRegisters(full, t.groupRegisters), t.groupRegisters)
			if err != nil {
				return HostFlow{Failure: predicateFailure(predicateWhen, t.When, err)}
			}
			hf.When, hf.Rendered = rp.Text, rp.Resolved
		} else {
			pass, err := keeper.EvalPredicate(t.When, full)
			if err != nil {
				return HostFlow{Failure: whenFailure(keeper, t.When, t.flowSeal, err)}
			}
			if !pass {
				return HostFlow{Skip: true}
			}
		}
	}
	open := append([]string{cel.RegisterSelf}, t.groupRegisters...)
	closed := withoutRegisters(full, open)
	for _, pr := range []struct {
		kind, expr string
		out        *string
	}{
		{"changed_when", t.ChangedWhen, &hf.ChangedWhen},
		{"failed_when", t.FailedWhen, &hf.FailedWhen},
		{"until", t.Until, &hf.Until},
	} {
		if pr.expr == "" {
			continue
		}
		rp, err := soul.RenderPredicate(pr.expr, closed, open)
		if err != nil {
			return HostFlow{Failure: predicateFailure(pr.kind, pr.expr, err)}
		}
		*pr.out = rp.Text
		hf.Rendered = hf.Rendered || rp.Resolved
	}
	return hf
}

// whenFailure is the Soul's code and wording for a `when:` that failed to evaluate
// (applyrunner.go), except that the error is withheld when the predicate reads a
// secret source: CEL quotes a missing map key by its value (`no such key: <value>`),
// and this message reaches error_summary and the audit log.
func whenFailure(engine *cel.Engine, when string, seal cel.SealSources, err error) *FlowFailure {
	msg := fmt.Sprintf("when %q: %v", when, err)
	if engine.PredicateReadsSecret(when, seal) {
		msg = fmt.Sprintf("when %q: evaluation failed; the error is withheld because the predicate reads a secret", when)
	}
	return &FlowFailure{Predicate: predicateWhen, Code: "flowcontrol.when_error", Message: msg}
}

// predicateFailure names a predicate that could not be rendered: the rewrite's own
// code for a refusal, and otherwise — the author's predicate does not compile — the
// code the Soul reports for that predicate kind.
func predicateFailure(kind, expr string, err error) *FlowFailure {
	var refused *cel.ErrPredicateRender
	if errors.As(err, &refused) {
		return &FlowFailure{Predicate: kind, Code: refused.Code, Message: kind + ": " + err.Error()}
	}
	return &FlowFailure{Predicate: kind, Code: "flowcontrol." + kind + "_error", Message: fmt.Sprintf("%s %q: %v", kind, expr, err)}
}

// readsAnyRegister reports whether expr reads one of the named registers, in the dot
// form the rest of the plan reads (choosing a register by index is refused at
// compile, NIM-909).
func readsAnyRegister(expr string, names []string) bool {
	if len(names) == 0 {
		return false
	}
	for _, ref := range config.ExtractRegisterRefs(expr) {
		if slices.Contains(names, ref) {
			return true
		}
	}
	return false
}

// withoutRegisters is v without the named registers: RenderPredicate takes the host
// context WITHOUT the open set, so an open name that happens to hold a value must not
// be substituted.
func withoutRegisters(v cel.Vars, names []string) cel.Vars {
	drop := false
	for _, name := range names {
		if _, ok := v.Register[name]; ok {
			drop = true
			break
		}
	}
	if !drop {
		return v
	}
	reg := make(map[string]any, len(v.Register))
	for name, val := range v.Register {
		if !slices.Contains(names, name) {
			reg[name] = val
		}
	}
	v.Register = reg
	return v
}

// groupRegisters lists the register names of every descendant of a block rolled as
// one group: an applier child's register too, which the Soul folds from its children
// inside the same message.
func groupRegisters(block config.Task) []string {
	var out []string
	var walk func(config.Task)
	walk = func(task config.Task) {
		if task.Block == nil {
			return
		}
		for _, child := range task.Block.Block {
			if child.Register != "" && !slices.Contains(out, child.Register) {
				out = append(out, child.Register)
			}
			walk(child)
		}
	}
	walk(block)
	return out
}

// markGroup marks a group's rendered descendants. The outermost group is marked
// last, so a nested group's tasks end up with the whole group's registers.
func markGroup(tasks []*RenderedTask, registers []string) {
	for _, t := range tasks {
		t.Grouped = true
		t.groupRegisters = registers
	}
}
