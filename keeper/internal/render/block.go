package render

import (
	"context"
	"fmt"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// renderBlockTask expands a block-task (pilot C1, destiny/tasks.md §6.5,
// orchestration.md §2.2.1) into a flat layer of RenderedTask at the render phase —
// following the renderApplyDestiny/renderLoopTask pattern. block is NOT a wire
// entity: the contract (proto/DispatchPlan/Soul) doesn't change. "The whole block
// as one wave" is emergent: all descendants carry the same SerialWidth+TargetSIDs
// (inherited from block) and one Passage (stampPassage stamps the entire fan-out —
// block is atomic per Passage, ADR-056; Stratify accounts for this).
//
// Inheritance (mergeBlockInheritance) — for each descendant:
//   - when: AND-merge `(<block.when>) && (<child.when>)` (if one is empty → take
//     the other). Predicates are carried as CEL strings (Soul evaluates, ADR-012(d)).
//   - where: AND-merge with child.where (same rule; resolved Keeper-side on the descendant).
//   - vars: block.vars is the base, child.vars overrides (child wins on same names).
//   - onchanges/onfail: union of block+child names → descendant.
//
// width is computed ONCE from block.Serial against the number of targeted hosts
// and inherited by all descendants (SerialWidth in every DispatchPlan). targeted
// is the block-task's hosts (after the parent's on:/where:/run_once:, resolved in
// Render before branching). idx is threaded through: descendants get monotonic
// Index values with no gaps.
//
// Descendant kinds:
//   - nested block: → recursive renderBlockTask (cascading inheritance);
//   - apply: → renderApplyDestiny (inherited width);
//   - module: → renderTask;
//   - loop: on a descendant → REJECTED (outside pilot C1 scope, clear error);
//   - parallel:/include: on a descendant → REJECTED (guardPilotBlockChild).
//
// Static-when-false descendant: emitStaticWhenSkip at the top of the loop emits
// placeholder(s) BEFORE guard/render — symmetric with the top-level Render loop
// (an inactive branch with unsupported DSL doesn't block the active one).
// Block-level static-when:false is NOT swallowed by emitStaticWhenSkip (it skips
// the block-task itself): a static-false block reaches here, mergeBlockInheritance
// merges block.when into EVERY descendant via AND, each descendant's static-when
// becomes false, and each child emits ITS OWN skip-placeholder with
// register/requisites via walkBlockChildren. This keeps register of a static-false
// block's descendants visible from outside (flat-register-scope is invariant under
// static-skip — the loopStaticSkip pattern).
func (p *Pipeline) renderBlockTask(
	ctx context.Context,
	in RenderInput,
	blockTask config.Task,
	startIndex int,
	targeted []*topology.HostFacts,
) ([]*RenderedTask, []DispatchPlan, error) {
	width := serialWidth(blockTask.Serial, len(targeted))

	// childTarget: a descendant resolves its own on:/where:/run_once: against the
	// block's hosts (scenario orchestration inside a block is allowed —
	// orchestration.md §2.2.2); the block NARROWS the descendant's roster.
	childTarget := func(child config.Task) ([]*topology.HostFacts, error) {
		return p.blockChildTargets(in, child, targeted)
	}
	// childRecurse: a nested block → recursion into the same renderBlockTask.
	childRecurse := func(child config.Task, idx int, childTargeted []*topology.HostFacts) ([]*RenderedTask, []DispatchPlan, error) {
		return p.renderBlockTask(ctx, in, child, idx, childTargeted)
	}
	return p.walkBlockChildren(ctx, in, blockTask, startIndex, width, guardPilotBlockChild, childTarget, childRecurse)
}

// blockChildGuard — checks a block descendant's kind before rendering. The
// scenario layer (renderBlockTask) supplies guardPilotBlockChild (allows
// apply/scenario orchestration on the descendant); the destiny layer
// (renderDestinyBlock) supplies guardDestinyBlockChild (rejects scenario keys).
// idx is the descendant's position for diagnostics, blockName is the container's
// name.
type blockChildGuard func(child config.Task, idx int, blockName string) error

// blockChildTargeter resolves a block descendant's roster. The scenario layer
// narrows the roster by the descendant's on:/where:/run_once:; the destiny layer
// inherits the block's roster WHOLESALE (block in destiny carries no orchestration
// — rejected by the guard).
type blockChildTargeter func(child config.Task) ([]*topology.HostFacts, error)

// blockChildRecurser renders a NESTED block descendant (child.Block != nil). Each
// layer supplies its own recursion (renderBlockTask / renderDestinyBlock) so
// cascading inheritance doesn't drift between layers.
type blockChildRecurser func(child config.Task, idx int, childTargeted []*topology.HostFacts) ([]*RenderedTask, []DispatchPlan, error)

// walkBlockChildren — the SINGLE traversal of a block's descendants for both
// layers (scenario and destiny). Source of truth for descendant processing order,
// so block inheritance doesn't drift between renderBlockTask and
// renderDestinyBlock (major drift risk):
//
//	mergeBlockInheritance(blockTask, child)   — merge in when/where/vars/requisites
//	→ emitStaticWhenSkip(child)               — the AND-when may have become static-false
//	→ guard(child)                            — layer's key boundary (callback)
//	→ render: nested block (recurse) / apply / module
//
// The per-layer difference is factored into three callbacks:
//   - guard: which descendant kinds/keys are allowed (scenario vs destiny boundary);
//   - target: how the descendant's roster is resolved (on/where narrowing vs inheritance);
//   - recurse: how a nested block is rendered (recursion within the same layer).
//
// width — the container's wave width (serial: of the block for scenario / serial:
// of the apply-task for destiny): threaded into every descendant's DispatchPlan
// (block doesn't carry its own serial for descendants — it inherits the
// container's width). idx is threaded through.
//
// apply-descendant: renderApplyDestiny is only ever reachable once the guard has
// let it through (the destiny guard rejects apply on a descendant — nested apply
// in destiny is forbidden, guardDestinyTask); so the child.Apply branch is only
// reachable in the scenario layer.
func (p *Pipeline) walkBlockChildren(
	ctx context.Context,
	in RenderInput,
	blockTask config.Task,
	startIndex int,
	width int,
	guard blockChildGuard,
	target blockChildTargeter,
	recurse blockChildRecurser,
) ([]*RenderedTask, []DispatchPlan, error) {
	tasks := make([]*RenderedTask, 0, len(blockTask.Block.Block))
	plans := make([]DispatchPlan, 0, len(blockTask.Block.Block))
	idx := startIndex
	for i := range blockTask.Block.Block {
		child := mergeBlockInheritance(blockTask, blockTask.Block.Block[i])

		// Static-when-false descendant: placeholder(s) BEFORE guard/render (like the
		// top-level Render loop). idx advances by the number of emitted placeholders.
		// The inherited when's static-when is static exactly when both operands
		// (block.when + child.when) are static — isStaticWhen checks the whole
		// AND-string.
		if skipped, serr := p.emitStaticWhenSkip(ctx, in, child, &tasks, &plans, &idx); serr != nil {
			return nil, nil, serr
		} else if skipped {
			continue
		}

		if gerr := guard(child, i, blockTask.Name); gerr != nil {
			return nil, nil, gerr
		}

		// Nested block: recursion — cascading inheritance (the outer predicate is
		// already merged into child via mergeBlockInheritance, now child passes it
		// down to its own descendants). serial is NOT inherited into child
		// (mergeBlockInheritance doesn't touch it): a nested block without serial:
		// rides its own width (0 = one wave).
		if child.Block != nil {
			childTargeted, terr := target(child)
			if terr != nil {
				return nil, nil, terr
			}
			bt, bp, berr := recurse(child, idx, childTargeted)
			if berr != nil {
				return nil, nil, berr
			}
			tasks = append(tasks, bt...)
			plans = append(plans, bp...)
			idx += len(bt)
			continue
		}

		childTargeted, terr := target(child)
		if terr != nil {
			return nil, nil, terr
		}

		// apply-descendant: an isolated destiny render pass with inherited width.
		// Only reachable in the scenario layer (the destiny guard rejects apply on a
		// descendant). child.Register materializes via the aggregate's terminal
		// core.noop.run (Variant B, renderApplyDestiny) — a block-descendant's
		// applier-register is addressable from outside as register.<child>.*
		// (orchestration.md §2.1.1).
		if child.Apply != nil {
			dt, dp, derr := p.renderApplyDestiny(ctx, in, child.Apply, idx, childTargeted, width, child.Register)
			if derr != nil {
				return nil, nil, derr
			}
			tasks = append(tasks, dt...)
			plans = append(plans, dp...)
			idx += len(dt)
			continue
		}

		// module-descendant.
		rt, rerr := p.renderTask(ctx, in, child, idx, childTargeted)
		if rerr != nil {
			return nil, nil, rerr
		}
		tasks = append(tasks, rt)
		plans = append(plans, DispatchPlan{
			TaskIndex:   idx,
			TargetSIDs:  sidsOf(childTargeted),
			SerialWidth: width,
		})
		idx++
	}
	return tasks, plans, nil
}

// blockChildTargets resolves a block descendant's target: child's on:/where:
// (already carrying inherited where via mergeBlockInheritance) against the
// block's hosts. on: on a descendant inside a block isn't typically used in the
// pilot, but resolves through the same resolveTargets (as in the scenario loop);
// then the descendant's run_once: (an uncommon case, but the grammar allows it —
// orchestration.md §2.2.2).
//
// CRITICAL (target isolation): resolution runs against the BLOCK's hosts
// (targeted — the result of the block-task's on:/where:/run_once: in Render), NOT
// against the whole roster: the descendant's where: narrows the set already
// selected by the block, symmetric to the two-phase on:→where: resolution
// (orchestration.md §4).
func (p *Pipeline) blockChildTargets(in RenderInput, child config.Task, targeted []*topology.HostFacts) ([]*topology.HostFacts, error) {
	scoped := in
	scoped.Hosts = targeted
	childTargeted, err := resolveTargets(p.cel, scoped, child)
	if err != nil {
		return nil, err
	}
	return applyRunOnce(childTargeted, child.RunOnce), nil
}

// mergeBlockInheritance builds a block descendant with inheritance from the
// container merged in (destiny/tasks.md §6.5). Does NOT mutate the source
// structs — returns a copy of child with When/Where/Vars/OnChanges/OnFail
// rewritten. Other fields (Module/Apply/Block/Loop/Register/params/serial/run_once/…)
// stay as in the source descendant: serial is NOT inherited by the descendant
// (wave width is distributed via DispatchPlan in renderBlockTask, not through a
// task field).
func mergeBlockInheritance(blockTask config.Task, child config.Task) config.Task {
	out := child
	out.When = andMergePredicate(blockTask.When, child.When)
	out.Where = andMergePredicate(blockTask.Where, child.Where)
	out.Vars = mergeVars(blockTask.Vars, child.Vars)
	out.OnChanges = unionNames(blockTask.OnChanges, child.OnChanges)
	out.OnFail = unionNames(blockTask.OnFail, child.OnFail)
	return out
}

// andMergePredicate joins two CEL predicates (when:/where:) by AND, preserving
// priority via parens: `(<outer>) && (<inner>)`. If one is empty → the other is
// taken as-is (unwrapped); both empty → "". This is the destiny/tasks.md §6.5 rule
// "inner when combines with outer by AND".
func andMergePredicate(outer, inner string) string {
	switch {
	case outer == "" && inner == "":
		return ""
	case outer == "":
		return inner
	case inner == "":
		return outer
	default:
		return "(" + outer + ") && (" + inner + ")"
	}
}

// mergeVars merges block.vars (base) and child.vars (overlay): child overrides
// same-named keys (destiny/tasks.md §9, the more local scope wins). Both empty →
// nil. Doesn't mutate the input maps.
func mergeVars(base, override map[string]any) map[string]any {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}
	out := make(map[string]any, len(base)+len(override))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range override {
		out[k] = v
	}
	return out
}

// unionNames merges two lists of requisite names (onchanges/onfail) without
// duplicates, preserving "block names, then child names" order. Both empty → nil.
func unionNames(blockNames, childNames []string) []string {
	if len(blockNames) == 0 && len(childNames) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(blockNames)+len(childNames))
	out := make([]string, 0, len(blockNames)+len(childNames))
	for _, n := range append(append([]string{}, blockNames...), childNames...) {
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// guardPilotBlockChild rejects block descendant kinds outside pilot C1 scope with
// an explicit [ErrUnsupportedDSL]. Supported: module:, apply:, nested block:
// (renderBlockTask's recursion checks these earlier — a block descendant never
// reaches here). Outside the pilot:
//   - loop: on a descendant (render-time fan-out inside a block is deferred);
//   - parallel: on a descendant (parallel in a block — a later slice);
//   - include: (must expand BEFORE render — ErrUnexpandedInclude);
//   - an empty task (no discriminator).
//
// A block descendant (child.Block != nil) never reaches here — renderBlockTask
// branches into recursion BEFORE the guard. parallel/loop on the block ITSELF
// (not the descendant) is rejected by the config validator
// (parallel_on_block_invalid / loop validation) and by guardPilotDSL (parallel) at
// the top level.
func guardPilotBlockChild(child config.Task, idx int, blockName string) error {
	switch {
	case child.Loop != nil:
		return fmt.Errorf("%w: loop: на потомке block %q (task[%d] %q) — вне pilot-объёма block", ErrUnsupportedDSL, blockName, idx, child.Name)
	case child.Parallel:
		return fmt.Errorf("%w: parallel: на потомке block %q (task[%d] %q) — вне pilot-объёма block", ErrUnsupportedDSL, blockName, idx, child.Name)
	case child.Include != nil:
		return fmt.Errorf("%w: include: на потомке block %q (task[%d] %q)", ErrUnexpandedInclude, blockName, idx, child.Name)
	case child.Module == nil && child.Apply == nil && child.Block == nil:
		return fmt.Errorf("%w: task[%d] %q в block %q не является module/apply/block-задачей", ErrUnsupportedDSL, idx, child.Name, blockName)
	}
	return nil
}
