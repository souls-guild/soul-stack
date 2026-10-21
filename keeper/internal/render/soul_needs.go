package render

import (
	"context"
	"math"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// What a linear run needs from its Souls, known before its first step (ADR-0091
// item 10, NIM-946). A future step renders as a placeholder whose only input to
// the capability gate is its module, and gating each step as it comes would refuse
// step k after the steps before it changed the hosts. So the first pass also walks the
// whole plan and states, per task, what it will send to which hosts, as far as that is
// known before the run.
//
// Known before the run: the hosts `on:`/`where:`/`run_once:` resolve to when `on:`
// reads no register and `where:` — with the `vars:` it is resolved against — reads no
// register and no state a capture at an earlier step changes; the tasks the render
// drops up front (an include group, a static `when:`, unless it reads such state); and
// every feature, which the text decides. State is read directly, or through
// `compute:` or a name of the task's view of `vars` — the service, scenario and task
// rungs, which every pass resolves again (ADR-0082 amendment 2026-10-15, point 16).
// What depends on the
// run is counted as sent: a `when:` the Keeper decides at the step, an include or
// static `when:` reading state an earlier capture changes, a requisite that drops the
// task on a host, and a loop's items — the hosts a task's targeting resolves to, as
// the ADR puts it; a host that leaves the roster later was still asked. Left to the
// step's own gate:
//   - a task whose targeting reads a register, or state an earlier capture changes;
//   - a host or a task a change of the roster brings in — a refresh boundary
//     re-resolves it as a fresh snapshot, and a host dropped under `on_failure: host`
//     leaves it — so hosts join and change facts, `run_once:` may pick another host,
//     and a `when:` or `where:` over `incarnation.host_count` or `soulprint.hosts` may
//     switch a task on;
//   - a destiny task a static `when:`, a conditional include or its own `on:`/`where:`
//     could switch off on a host — the survey does not render the destiny;
//   - the `when: false` a grouped child is sent where the Keeper skips it;
//   - a task the survey cannot judge — an evaluation error, a transient Vault failure
//     included.

// SoulNeed is what one task of a linear run sends to the Soul of each host in
// TargetSIDs.
type SoulNeed struct {
	Step int
	Task string
	// Module — the module address; empty for a task that has none.
	Module string
	// FlowControl — a predicate travels: `changed_when:`/`failed_when:`, a grouped
	// child's `when:` reading a sibling's register, a destiny task's own predicates.
	FlowControl bool
	// Retry — `retry.until:` or more than one attempt.
	Retry bool
	// RenderedPredicate — the Keeper substitutes a value into a predicate it sends.
	RenderedPredicate bool
	TargetSIDs        []string
}

// SoulNeeds collects the plan's [SoulNeed]s. Filled by the first pass that reaches
// it, so a run sets it once and every later pass leaves it alone.
type SoulNeeds struct {
	needs  []SoulNeed
	filled bool
}

// NewSoulNeeds returns an empty collector for one run.
func NewSoulNeeds() *SoulNeeds {
	return &SoulNeeds{}
}

// List returns what the run's first pass collected. Nil-safe.
func (n *SoulNeeds) List() []SoulNeed {
	if n == nil {
		return nil
	}
	return n.needs
}

// fillSoulNeeds runs the survey once, on the prepared input of a pass that settles
// flow control on the Keeper.
func (p *Pipeline) fillSoulNeeds(ctx context.Context, in RenderInput) {
	if in.SoulNeeds == nil || in.SoulNeeds.filled || !in.keeperFlow() {
		return
	}
	s := &needSurvey{
		p:            p,
		ctx:          ctx,
		in:           in,
		includes:     includeGroupCache{},
		firstCapture: math.MaxInt,
	}
	// A capture sits at the top level (a block and a destiny refuse one), and one the
	// render drops before its step writes nothing.
	for i := range in.Scenario.Tasks {
		if config.HasStateCapture(in.Scenario.Tasks[i:i+1]) && !s.off(in.Scenario.Tasks[i], in.Steps[i].Step) {
			s.firstCapture = in.Steps[i].Step
			break
		}
	}
	s.movingCompute = map[string]bool{}
	for _, cv := range in.Scenario.Compute {
		if s.computeMoves(cv.Value, s.movingCompute) {
			s.movingCompute[cv.Name] = true
		}
	}
	s.runMoving = s.movingIn(p.runLevelVarLayers(in))
	for i, task := range in.Scenario.Tasks {
		s.task(task, in.Steps[i], in.Hosts, nil, guardTopLevel(i))
	}
	in.SoulNeeds.needs = s.out
	in.SoulNeeds.filled = true
}

// needSurvey walks the plan as Render does, without rendering. It never fails the
// run: a task it cannot judge is left to its step, whose render meets the error again
// or whose gate judges the task.
type needSurvey struct {
	p        *Pipeline
	ctx      context.Context
	in       RenderInput
	includes includeGroupCache
	// firstCapture — the first step that writes incarnation.state (math.MaxInt when
	// none does). The run re-reads the state before every later step, so a predicate
	// there reads what the capture wrote, not the snapshot this pass has.
	firstCapture int
	// movingCompute — the compute entries that read the state, directly or through an
	// earlier entry: every pass recomputes the block against the state it was given.
	movingCompute map[string]bool
	// runMoving — the names of the run-level view (the service and scenario rungs)
	// that read the state, the view of a task without `vars:` and of an include's
	// `when:`.
	runMoving map[string]bool
	out       []SoulNeed
}

// surveyGroup is the block rolled as one group a task sits in: its registers stay
// open in the predicates its children are sent.
type surveyGroup struct {
	registers []string
}

func guardTopLevel(i int) func(config.Task) error {
	return func(task config.Task) error { return guardPilotDSL(task, i) }
}

// task surveys one scenario task resolved against scope, in the order of the render
// loops (Render, walkBlockChildren).
func (s *needSurvey) task(task config.Task, node config.StepNode, scope []*topology.HostFacts, group *surveyGroup, guard func(config.Task) error) {
	if IsAssertTask(task) || s.off(task, node.Step) || guard(task) != nil || IsKeeperTask(task) || !s.targetingKnown(task, node.Step) {
		return
	}
	scoped := s.in
	scoped.Hosts = scope
	targeted, err := s.p.resolveTargets(scoped, task)
	if err != nil {
		return
	}
	targeted = applyRunOnce(targeted, task.RunOnce)
	if len(targeted) == 0 {
		return
	}
	switch {
	case task.Apply != nil:
		s.applier(task, node.Step, targeted)
	case task.Block != nil:
		s.block(task, node, targeted, group)
	case task.Module != nil:
		s.add(task, node.Step, targeted, group)
	}
}

// block surveys a block's children: a group's at the block's step with its registers
// open, a default block's each at its own step with its own `where:` (the block holds
// its hosts).
func (s *needSurvey) block(blockTask config.Task, node config.StepNode, targeted []*topology.HostFacts, group *surveyGroup) {
	overlay := group == nil && !node.Group
	if overlay && len(node.Children) != len(blockTask.Block.Block) {
		return
	}
	if group == nil && node.Group {
		group = &surveyGroup{registers: groupRegisters(blockTask)}
	}
	for j, raw := range blockTask.Block.Block {
		child := mergeBlockInheritance(blockTask, raw)
		childNode := config.StepNode{Step: node.Step, Group: true}
		if overlay {
			child = linearOverlayChild(blockTask, raw, child)
			childNode = node.Children[j]
		}
		guard := func(c config.Task) error { return guardPilotBlockChild(c, j, blockTask.Name) }
		s.task(child, childNode, targeted, group, guard)
	}
}

// off reports whether the render drops a task before its step: its include group is
// dropped, or its static `when:` is false or cannot be evaluated (the task then
// fails at its step and is sent nothing). A block's `when:` reaches its children. A
// predicate reading state an earlier capture changes is decided against that state at
// the step, so here it drops nothing.
//
// The flow context these are evaluated in carries no compute (a read of it fails
// every time), and an include's `when:` none of the task's `vars:`: it reads the
// service and scenario rungs, the view of a task without `vars:`.
func (s *needSurvey) off(task config.Task, step int) bool {
	if task.IncludeGroupID != 0 && !s.movesWithState(task.IncludeWhen, step, nil, s.movingVars(config.Task{}, step)) {
		if keep, err := s.p.keepIncludeGroup(s.in, task, s.includes); err != nil || !keep {
			return true
		}
	}
	if task.Block != nil || !isStaticWhen(task.When) || s.movesWithState(task.When, step, nil, s.movingVars(task, step)) {
		return false
	}
	hosts := s.in.Hosts
	if len(hosts) == 0 {
		hosts = []*topology.HostFacts{{}}
	}
	skip, err := s.p.staticWhenSkips(s.in, task, hosts, len(s.in.Hosts), nil)
	return err != nil || skip != nil
}

// targetingKnown reports whether the hosts a task resolves to are known before the
// run: its `on:` reads no register, and its `where:` — with the `vars:` it is resolved
// against, and inside a `soulprint.hosts.where("…")` — no register and no state a
// capture before its step changes, directly or through `compute:` or a name of the
// task's view. `on:` resolves against neither the state, compute nor vars, and `vars:`
// is resolved for `where:` alone.
//
// The register test reads every string of the task's own `vars:`, at any depth,
// whether or not the `where:` names it. No lower rung can name `register` (ADR-0082
// amendment 2026-10-15, point 6), so a name of the view reaches one only through the
// task rung.
func (s *needSurvey) targetingKnown(task config.Task, step int) bool {
	if readsRegister(task.On) {
		return false
	}
	if task.Where == "" {
		return true
	}
	reads := s.p.cel.HostPredicateReads(task.Where, targetingRoots)
	if r := reads["register"]; r.Whole || len(r.Fields) > 0 || len(config.ExtractRegisterRefs(task.Where)) > 0 {
		return false
	}
	for _, v := range task.Vars {
		if s.varReadsRegister(v) {
			return false
		}
	}
	return !(s.firstCapture < step && readsMoving(reads, s.movingCompute, s.movingVars(task, step)))
}

// varReadsRegister reports whether any string of a `vars:` value, at any depth, reads
// a register: by its parse tree, or by the text reading.
func (s *needSurvey) varReadsRegister(v any) bool {
	switch t := v.(type) {
	case string:
		r := s.p.cel.InterpolationReads(t, registerRoot)["register"]
		return r.Whole || len(r.Fields) > 0 || len(config.ExtractRegisterRefs(t)) > 0
	case []any:
		for _, el := range t {
			if s.varReadsRegister(el) {
				return true
			}
		}
	case map[string]any:
		for _, el := range t {
			if s.varReadsRegister(el) {
				return true
			}
		}
	}
	return false
}

var (
	targetingRoots = []string{"incarnation", "compute", "vars", "register"}
	registerRoot   = []string{"register"}
)

func readsRegister(v any) bool {
	switch t := v.(type) {
	case string:
		return len(config.ExtractRegisterRefs(t)) > 0
	case []any:
		for _, sub := range t {
			if readsRegister(sub) {
				return true
			}
		}
	}
	return false
}

var movingRoots = []string{"incarnation", "compute", "vars"}

// readsMoving reports whether reads touch incarnation.state, a compute entry of
// computes or a var of vars.
func readsMoving(reads map[string]cel.RootReads, computes, vars map[string]bool) bool {
	if r := reads["incarnation"]; r.Whole || r.Fields["state"] {
		return true
	}
	return readsAny(reads["compute"], computes) || readsAny(reads["vars"], vars)
}

func readsAny(r cel.RootReads, names map[string]bool) bool {
	if len(names) == 0 {
		return false
	}
	if r.Whole {
		return true
	}
	for f := range r.Fields {
		if names[f] {
			return true
		}
	}
	return false
}

// computeMoves reports whether a `compute:` value reads moving state, directly or
// through an entry of computes. Only a string is interpolated; any other value passes
// through as a literal. Its `vars.*` is the service view alone, which reads no state.
func (s *needSurvey) computeMoves(v any, computes map[string]bool) bool {
	str, ok := v.(string)
	return ok && readsMoving(s.p.cel.InterpolationReads(str, movingRoots), computes, nil)
}

// movingVars names the vars of task's view that read moving state at step: the
// service, scenario and block + task rungs, directly or through another name, every
// definition and every string of it at any depth ([viewClosure]); nil before the first
// capture.
func (s *needSurvey) movingVars(task config.Task, step int) map[string]bool {
	if s.firstCapture >= step {
		return nil
	}
	if len(task.Vars) == 0 {
		return s.runMoving
	}
	return s.movingIn(s.p.taskVarLayers(s.in, task))
}

// movingIn closes a view's raw layers over moving state.
func (s *needSurvey) movingIn(layers []cel.VarLayer) map[string]bool {
	return viewClosure(s.p.cel, layers, movingRoots, func(reads map[string]cel.RootReads, closed map[string]bool) bool {
		return readsMoving(reads, s.movingCompute, closed)
	})
}

// movesWithState reports whether a bare predicate at step reads state a capture at an
// earlier step changes: incarnation.state, a compute entry of computes, or a var of vars.
func (s *needSurvey) movesWithState(expr string, step int, computes, vars map[string]bool) bool {
	return s.firstCapture < step && readsMoving(s.p.cel.PredicateReads(expr, movingRoots), computes, vars)
}

// add records a scenario task. Its `when:` is decided on the Keeper and travels blank,
// unless it is a grouped child's reading a sibling's register; its post-module
// predicates travel with `register.self` and the group's registers open.
func (s *needSurvey) add(task config.Task, step int, targeted []*topology.HostFacts, group *surveyGroup) {
	need := SoulNeed{Step: step, Task: task.Name, Module: task.Module.Module, TargetSIDs: sidsOf(targeted)}
	open := []string{cel.RegisterSelf}
	if group != nil {
		open = append(open, group.registers...)
		if readsAnyRegister(task.When, group.registers) {
			need.FlowControl = true
			need.RenderedPredicate = s.resolves(task.When, group.registers)
		}
	}
	for _, expr := range []string{task.ChangedWhen, task.FailedWhen} {
		if expr != "" {
			need.FlowControl = true
			need.RenderedPredicate = need.RenderedPredicate || s.resolves(expr, open)
		}
	}
	if task.Retry != nil {
		need.Retry = task.Retry.Until != "" || task.Retry.Count > 1
		if task.Retry.Until != "" {
			need.RenderedPredicate = need.RenderedPredicate || s.resolves(task.Retry.Until, open)
		}
	}
	s.out = append(s.out, need)
}

// resolves is [cel.Engine.PredicateResolves] with no loop variables: settleHostFlow
// renders against the flow context, which binds none, so a loop variable in a
// predicate fails its task at the step. A predicate that does not parse fails there
// too, before anything is sent.
func (s *needSurvey) resolves(expr string, open []string) bool {
	ok, err := s.p.cel.PredicateResolves(expr, open, nil)
	return err == nil && ok
}

// applier records the destiny's tasks on the applier's hosts (user decision
// 2026-10-08, option A). A destiny travels whole and the Soul evaluates its
// predicates (decision 6), so none is rendered. A task a static `when:`, a
// conditional include or its own `on:`/`where:` could switch off on a host is left
// to the step, which renders the destiny against its input; the survey does not. A
// destiny that does not resolve here is left to the step too, which reports the same
// error as it does today.
func (s *needSurvey) applier(task config.Task, step int, targeted []*topology.HostFacts) {
	if s.in.Destiny == nil {
		return
	}
	resolved, err := s.in.Destiny.Resolve(s.ctx, task.Apply.Destiny)
	if err != nil {
		return
	}
	sids := sidsOf(targeted)
	// walk follows renderApplyDestiny and walkBlockChildren: include, static `when:`,
	// guard, then a block's children with its `when:` AND-ed in.
	var walk func(t config.Task, guard func(config.Task) error)
	walk = func(t config.Task, guard func(config.Task) error) {
		if t.IncludeGroupID != 0 || isStaticWhen(t.When) || t.On != nil || t.Where != "" || guard(t) != nil {
			return
		}
		if t.Block != nil {
			if guardDestinyBlock(t) != nil {
				return
			}
			for j, child := range t.Block.Block {
				walk(mergeBlockInheritance(t, child), func(c config.Task) error { return guardDestinyBlockChild(c, j, t.Name) })
			}
			return
		}
		if t.Module == nil {
			return
		}
		s.out = append(s.out, SoulNeed{
			Step:        step,
			Task:        task.Name + "/" + t.Name,
			Module:      t.Module.Module,
			FlowControl: t.When != "" || t.ChangedWhen != "" || t.FailedWhen != "",
			Retry:       t.Retry != nil && (t.Retry.Until != "" || t.Retry.Count > 1),
			TargetSIDs:  sids,
		})
	}
	for i, t := range resolved.Tasks {
		walk(mergeApplierInheritance(task, t), func(c config.Task) error { return guardDestinyTask(c, i, resolved.Name) })
	}
	if task.Register != "" {
		s.out = append(s.out, SoulNeed{Step: step, Task: task.Name, Module: applierRegisterModule, TargetSIDs: sids})
	}
}
