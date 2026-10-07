package config

// Step numbering for linear dispatch (draft ADR
// docs/adr/draft-scenario-linear-barrier.md, decisions 1-2, §`passage`, fork 3).
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

import "strconv"

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
	out.Settings = InheritSettings(block, child)
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
