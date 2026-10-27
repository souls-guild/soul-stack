package config

// Step numbering for linear dispatch (ADR-0091,
// docs/adr/0091-scenario-linear-barrier.md, decisions 1-2, §`passage`, fork 3).
// Under it `passage` is the 0-based index of a STEP in the plan after
// ExpandIncludes, computed from the plan alone — a block's mode is written in the
// file — so nothing the data does moves a number: a task dropped at render (a false
// `include:` when, a static `when:`) leaves a hole, and the next task keeps its
// index.
//
// A step is a top-level task other than a default block — an applier, a `loop:`
// fan-out, a keeper-side task, an `assert:` and a block rolled as one group each
// count as one — or a child of a default block, at any depth, unless it sits inside
// a group.

import (
	"slices"
	"strconv"
	"strings"
)

// StepNode is one task's place in the step order.
type StepNode struct {
	// Step is the task's step; for a default block, its first child's.
	Step int
	// Group marks a block rolled as one group: one step for its whole subtree, at
	// the block's width.
	Group bool
	// Children — a default block only: one node per child, in plan order. nil for
	// every task that is one step.
	Children []StepNode
}

// LinearPassage numbers a run's plan in steps. TaskPassage[i] is top-level task i's
// step (a default block's first) and Steps[i] its node, from which render numbers a
// default block's children. Count is the number of steps. A default block with no
// children takes a step of its own, so every TaskPassage entry stays below Count.
//
// scenario is the scenario level of `settings:`, where `serial_scope` may be set for
// every block at once.
func LinearPassage(tasks []Task, scenario *ScenarioSettings) (Passage, []StepNode) {
	next := 0
	nodes := make([]StepNode, len(tasks))
	taskPassage := make([]int, len(tasks))
	for i := range tasks {
		nodes[i] = linearStepNode(tasks[i], scenario, &next)
		taskPassage[i] = nodes[i].Step
	}
	if next == 0 {
		next = 1
	}
	return Passage{TaskPassage: taskPassage, Count: next}, nodes
}

func linearStepNode(t Task, scenario *ScenarioSettings, next *int) StepNode {
	group := BlockRollsAsGroup(scenario, t)
	if t.Block == nil || group || len(t.Block.Block) == 0 {
		n := StepNode{Step: *next, Group: group}
		*next++
		return n
	}
	n := StepNode{Step: *next, Children: make([]StepNode, len(t.Block.Block))}
	for j := range t.Block.Block {
		n.Children[j] = linearStepNode(LinearBlockChild(t, t.Block.Block[j]), scenario, next)
	}
	return n
}

// LinearBlockChild is what a default block hands a child under linear dispatch,
// beyond the shared block inheritance: its `settings:` level and its `serial:`. The
// child is a step of its own and inherits by the ordinary rule, the lower level
// winning (fork 3) — so a block's `serial:` becomes its children's default, where
// staged dispatch stamps the block's width on every child instead. Render applies
// the same two fields, so the numbering and the rendering cannot disagree on which
// nested block is a group.
func LinearBlockChild(block, child Task) Task {
	out := child
	out.Settings = InheritSettings(block.Settings, child)
	if out.Serial == nil {
		out.Serial = block.Serial
	}
	return out
}

// BlockRollsAsGroup reports whether a block rolls as one group under linear
// dispatch: its effective `serial_scope` is `block` and it has a `serial:`, its own
// or inherited (settings.md §2, §3.1). Anywhere else `serial_scope` is unused.
// block must carry its enclosing blocks' settings and serial already merged in
// ([LinearBlockChild]); scenario is the scenario level beneath them.
func BlockRollsAsGroup(scenario *ScenarioSettings, block Task) bool {
	if block.Block == nil || block.Serial == nil {
		return false
	}
	eff := DefaultScenarioSettings()
	if scenario != nil {
		eff = MergeSettings(eff, *scenario)
	}
	if block.Settings != nil {
		eff = MergeSettings(eff, *block.Settings)
	}
	return eff.SerialScope != nil && *eff.SerialScope == SerialScopeBlock
}

// StepPath is the key of a nested block's plan position: the top-level index, then
// each child position below it. Stable across every pass of a run.
func StepPath(parent string, pos int) string {
	if parent == "" {
		return strconv.Itoa(pos)
	}
	return parent + "/" + strconv.Itoa(pos)
}

// WithinGroupRegisterDependency is [WithinBlockRegisterDependency] under linear
// dispatch, narrowed to blocks rolled as one group (ADR item 12). Only a group
// still travels as one message, so only there can a child read a sibling's
// register before it exists. A default block's children are steps, and a later
// child reads an earlier one's register the way any later step does.
func WithinGroupRegisterDependency(tasks []Task, scenario *ScenarioSettings) (WithinBlockInfo, bool) {
	for i := range tasks {
		if info, bad := groupPeerRegisterRead(tasks[i], scenario); bad {
			return info, true
		}
	}
	return WithinBlockInfo{}, false
}

// CrossStepRequisite is [CrossPassageRequisite] under linear dispatch: an
// `onchanges:`/`onfail:` whose source runs in another step than the task that
// reads it. The Keeper resolves every such link from the audit log
// (keeper/internal/scenario/crosspassage.go), so a Keeper without an audit reader
// refuses the run (ADR-0091 item 9). It sees what the staged detector
// does not: a default block's children are steps of their own, and a link
// between two of them crosses a step. A block rolled as one group is one step,
// and links inside it are resolved inside its message. A requisite written on a
// block reaches every task in it, as render merges it. steps is [LinearPassage]'s
// for the same tasks.
func CrossStepRequisite(tasks []Task, steps []StepNode) (CrossPassageInfo, bool) {
	if len(steps) != len(tasks) {
		return CrossPassageInfo{}, false
	}
	emitted := RegisterSteps(tasks, steps)
	for _, l := range planStepLinks(tasks, steps) {
		for _, req := range l.reqs {
			for _, src := range emitted[req.name] {
				if src != l.step {
					return CrossPassageInfo{
						ConsumerName:    taskDisplayName(l.task),
						RequisiteName:   req.name,
						Kind:            req.kind,
						ConsumerPassage: l.step,
						SourcePassage:   src,
					}, true
				}
			}
		}
	}
	return CrossPassageInfo{}, false
}

// RescueSteps are the steps of a linear plan holding an `onfail:` task whose source
// runs at an earlier step, each with the source registers its rescues name — the
// steps a run that a failure ended still walks to (docs/scenario/rescues.md). A
// keeper-side `onfail:` task is no rescue: the Keeper evaluates no requisite of its
// own tasks. steps is [LinearPassage]'s for the same tasks.
func RescueSteps(tasks []Task, steps []StepNode) map[int][]string {
	if len(steps) != len(tasks) {
		return nil
	}
	links := planStepLinks(tasks, steps)
	emitted := RegisterSteps(tasks, steps)
	out := map[int][]string{}
	for _, l := range links {
		if IsKeeperSideTask(*l.task) {
			continue
		}
		for _, req := range l.reqs {
			if req.kind != "onfail" || slices.Contains(out[l.step], req.name) {
				continue
			}
			for _, src := range emitted[req.name] {
				if src < l.step {
					out[l.step] = append(out[l.step], req.name)
					break
				}
			}
		}
	}
	return out
}

// RegisterSteps is, for each register a linear plan sets, the steps of the tasks
// that set it: a group's child at the group's step, an applier and a `loop:` at
// their one step. steps is [LinearPassage]'s for the same tasks; nil when it was
// numbered from another plan.
func RegisterSteps(tasks []Task, steps []StepNode) map[string][]int {
	if len(steps) != len(tasks) {
		return nil
	}
	out := map[string][]int{}
	for _, l := range planStepLinks(tasks, steps) {
		if l.task.Register != "" {
			out[l.task.Register] = append(out[l.task.Register], l.step)
		}
	}
	return out
}

// StepSpan is the first and the last step of the construct at plan position path
// ([StepPath] form): a default block runs from its first child's step to its last
// descendant's, any other task — a group included — is one step, and so is every
// position below a group, which is not checked against the group's children. ok is
// false for a path outside steps: past a default block's last child, or below a task
// that is neither a group nor a block with children.
func StepSpan(steps []StepNode, path string) (first, last int, ok bool) {
	if path == "" {
		return 0, 0, false
	}
	nodes := steps
	var node StepNode
	parts := strings.Split(path, "/")
	for i, part := range parts {
		pos, err := strconv.Atoi(part)
		if err != nil || pos < 0 || pos >= len(nodes) {
			return 0, 0, false
		}
		node = nodes[pos]
		if node.Children == nil {
			if i < len(parts)-1 && !node.Group {
				return 0, 0, false
			}
			return node.Step, node.Step, true
		}
		nodes = node.Children
	}
	tail := node
	for len(tail.Children) > 0 {
		tail = tail.Children[len(tail.Children)-1]
	}
	return node.Step, tail.Step, true
}

// CrossStepUnits are the plan positions ([StepPath]) of a linear plan's cross-step
// rescue units: the outermost units of [RescueUnits] with no source before them in
// their own message (ADR-0091 amendment NIM-993, rule 1). Such a unit never fires on a
// live host.
//
// A unit's sources are every source its tasks name, nested units included, and each is
// set outside the unit (onfail_names_rescue). So a source set at a step of the unit is a
// sibling in the group the unit sits in, travelling in the unit's message. It can fire
// the unit on a live host only if it comes before the unit: the Soul runs a message in
// plan order, so a sibling after the unit (a forward `onfail:`) has not run when the
// unit does. A source at any other step cannot: one at an earlier step fires the unit
// only on a held or a judged host, neither of them live, and one at a later step cannot
// have failed when the unit runs. A destiny's own units are another plan's, decided in
// the destiny's message, and a source no task sets keeps its unit out.
//
// nil when the plan holds none. steps is [LinearPassage]'s for the same tasks.
func CrossStepUnits(tasks []Task, steps []StepNode) map[string]bool {
	if len(steps) != len(tasks) {
		return nil
	}
	units, visits := walkRescueUnits(tasks)
	if len(units) == 0 {
		return nil
	}
	// Pre-order: every unit nested in an outermost one follows it, before any unit
	// outside it.
	var outer []string
	sources := map[string][]string{}
	for _, u := range units {
		if n := len(outer); n == 0 || !(RescueUnit{Path: outer[n-1]}).Holds(u.Path) {
			outer = append(outer, u.Path)
		}
		key := outer[len(outer)-1]
		sources[key] = append(sources[key], u.Source)
	}
	order := make(map[string]int, len(visits))
	setters := map[string][]registerSetter{}
	for i, v := range visits {
		order[v.path] = i
		if v.task.Register == "" {
			continue
		}
		if step, _, ok := StepSpan(steps, v.path); ok {
			setters[v.task.Register] = append(setters[v.task.Register], registerSetter{step: step, order: i})
		}
	}
	var out map[string]bool
	for _, key := range outer {
		first, last, ok := StepSpan(steps, key)
		if !ok || !sourcesOutside(sources[key], first, last, order[key], setters) {
			continue
		}
		if out == nil {
			out = map[string]bool{}
		}
		out[key] = true
	}
	return out
}

// registerSetter is a task setting a register: its step, and its pre-order position
// in the plan.
type registerSetter struct{ step, order int }

// sourcesOutside reports whether every one of sources is set, and none of them by a
// task at a step from first to last that comes before the unit, whose pre-order
// position is unit.
func sourcesOutside(sources []string, first, last, unit int, setters map[string][]registerSetter) bool {
	for _, name := range sources {
		at := setters[name]
		if len(at) == 0 || slices.ContainsFunc(at, func(s registerSetter) bool { return s.step >= first && s.step <= last && s.order < unit }) {
			return false
		}
	}
	return true
}

// planStepLinks is every task of a plan with its step and requisites, in plan order.
func planStepLinks(tasks []Task, steps []StepNode) []stepLink {
	var links []stepLink
	for i := range tasks {
		collectStepLinks(&tasks[i], steps[i], nil, &links)
	}
	return links
}

// stepLink is one task of the plan with the step it runs in and every requisite
// it reads: its own and those of the blocks around it.
type stepLink struct {
	task *Task
	step int
	reqs []taskRequisite
}

func collectStepLinks(t *Task, node StepNode, inherited []taskRequisite, out *[]stepLink) {
	reqs := append([]taskRequisite(nil), inherited...)
	for _, name := range t.OnChanges {
		reqs = append(reqs, taskRequisite{name: name, kind: "onchanges"})
	}
	for _, name := range t.OnFail {
		reqs = append(reqs, taskRequisite{name: name, kind: "onfail"})
	}
	if node.Children == nil {
		*out = append(*out, stepLink{task: t, step: node.Step, reqs: reqs})
	}
	if t.Block == nil {
		return
	}
	for j := range t.Block.Block {
		child := StepNode{Step: node.Step}
		if node.Children != nil {
			// A shape numbered from another plan: render refuses it at the block.
			if j >= len(node.Children) {
				return
			}
			child = node.Children[j]
		}
		collectStepLinks(&t.Block.Block[j], child, reqs, out)
	}
}

func groupPeerRegisterRead(t Task, scenario *ScenarioSettings) (WithinBlockInfo, bool) {
	if t.Block == nil {
		return WithinBlockInfo{}, false
	}
	if BlockRollsAsGroup(scenario, t) {
		return blockPeerRegisterRead(&t)
	}
	for j := range t.Block.Block {
		if info, bad := groupPeerRegisterRead(LinearBlockChild(t, t.Block.Block[j]), scenario); bad {
			return info, true
		}
	}
	return WithinBlockInfo{}, false
}
