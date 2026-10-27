package render

import (
	"context"
	"fmt"
	"regexp"
	"sort"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// defaultLoopVar is the default loop variable name (destiny/tasks.md §7,
// when `as:` is omitted).
const defaultLoopVar = "item"

// renderLoopTask expands `loop:` on a module task in the render phase (slice
// E1): one task → N RenderedTasks over the items elements, with contiguous
// indexes (symmetric with renderApplyDestiny). Unlike config-splice include
// (expanded BEFORE render), loop is expanded HERE — items is a CEL/template
// expression (`${ input.users }` / `${ vars.x }`), and CEL only runs in the
// render phase.
//
// Order (orchestration.md §2.2):
//  1. items is resolved once per run (host-invariant context of input/vars;
//     no soulprint — items must not depend on the host).
//  2. array → as=element, index_as=0-based index; object → as=value,
//     index_as=key, iteration order is alphabetical by key
//     (destiny/tasks.md §7).
//  3. when: is a per-item truthy filter in the same host-invariant context
//     (filters by element content, no soulprint); a filtered-out iteration
//     produces no RenderedTask. A soulprint reference in when: is an error
//     (host-variant when is deferred along with per-host loop filtering).
//  4. each remaining iteration is rendered by renderTaskIter with loop
//     variables; host-invariance is checked PER-ITERATION (see
//     renderTaskIter).
//
// targeted is the task's hosts (after parent on:/where:/run_once:); loop runs
// on every targeted host (serial waves are orthogonal to the iteration axis —
// the whole loop runs on each host of a wave). SerialWidth is inherited by
// all iterations. Empty result (items empty / when: filtered everything out)
// → 0 tasks (valid no-op).
func (p *Pipeline) renderLoopTask(
	ctx context.Context,
	in RenderInput,
	task config.Task,
	startIndex int,
	targeted []*topology.HostFacts,
) ([]*RenderedTask, []DispatchPlan, error) {
	asName := task.Loop.As
	if asName == "" {
		asName = defaultLoopVar
	}

	// seal (NIM-822, NIM-823): a loop over a secret list binds the secret to
	// `asName`, and every `${ <as>.<field> }` in this task's params reads it.
	// Decided here, once, from the RAW `items:` text — before the iteration loop,
	// because the taint is iteration-invariant for the reason sealedLoopItems
	// states, and because renderTaskIter takes `in` by value, so marking it here
	// reaches exactly this loop's iterations.
	src := p.taskSealSources(in, task)
	if task.Loop.Items != nil && sealedLoopItems(p.cel, task.Loop.Items, src) {
		in = withSealedLoopBind(in, asName)
		src = p.taskSealSources(in, task)
	}
	in.loopSeal = &loopSeal{name: task.Name, source: task.Source, src: src}

	// A static-when-false loop task never reaches here: both call sites
	// (scenario loop in pipeline.go, destiny.go) call emitStaticWhenSkip
	// first, which emits an N/1 skip-placeholder via loopStaticSkip for a
	// statically-false loop and continues BEFORE renderLoopTask. So by here
	// static-when is guaranteed active (true) or absent — a repeated
	// static-when gate would be unreachable and just duplicate per-host
	// staticWhenSkips. renderLoopTask only sees the fan-out of the active
	// branch.
	axis, err := p.loopAxisView(in)
	if err != nil {
		return nil, nil, p.taskFailure(in, task, -1, fmt.Errorf("render: task %q loop: %w", task.Name, err))
	}
	iters, err := resolveLoopItems(p.cel, in, task.Loop, asName, axis)
	if err != nil {
		return nil, nil, p.taskFailure(in, task, -1, fmt.Errorf("render: task %q loop: %w", task.Name, err))
	}

	width := serialWidth(task.Serial, len(targeted)+len(in.unitSkip))
	tasks := make([]*RenderedTask, 0, len(iters))
	plans := make([]DispatchPlan, 0, len(iters))
	idx := startIndex
	for _, it := range iters {
		keep, werr := evalLoopWhen(p.cel, in, task.Loop.When, it, axis)
		if werr != nil {
			// Here, with `in` carrying the secret loop binding: above this function
			// nothing knows `<as>` is secret.
			return nil, nil, p.taskFailure(in, task, idx, fmt.Errorf("render: task %q: %w", task.Name, werr))
		}
		if !keep {
			continue
		}

		var rt *RenderedTask
		if in.layoutOnly {
			rt = unitSkipPlaceholder(task, idx, 0, in.Modules)
		} else {
			var rerr error
			if rt, rerr = p.renderTaskIter(ctx, in, task, idx, targeted, it); rerr != nil {
				return nil, nil, rerr
			}
		}
		tasks = append(tasks, rt)
		plans = append(plans, DispatchPlan{
			TaskIndex:   idx,
			TargetSIDs:  sidsOf(targeted),
			SerialWidth: width,
		})
		idx++
	}
	withSkipTargets(tasks, plans, in.unitSkip)
	return tasks, plans, nil
}

// loopStaticSkip emits skip-placeholder(s) for a loop task with a
// statically-false when: (architect decision "resolves→N / doesn't
// resolve→1"):
//
//   - items RESOLVES (host-invariant context) → N skip-placeholders over the
//     iterations with contiguous Index — PARITY with the per-iter skip from
//     renderTaskIter that used to happen naturally (when items resolved, the
//     loop ran renderTaskIter, each one skipping params). We don't collapse
//     N→1 so the plan/Index matches the active branch (Passage determinism).
//   - items does NOT resolve (no-such-key/non-collection — typically an
//     absent optional input on an inactive branch) → don't fail (that was
//     the original bug), emit ONE skip-placeholder for the whole task. Soul
//     computes the same when:false from flow_context → SKIPPED; loop doesn't
//     expand on a skipped task.
//
// All placeholders carry the first host's flow_context (skip), Params=nil,
// When/ID/Register/Passage are carried through — same as the scenario
// static-when placeholder-skip.
func (p *Pipeline) loopStaticSkip(
	in RenderInput,
	task config.Task,
	startIndex int,
	targeted []*topology.HostFacts,
	asName string,
	skip *structpb.Struct,
) ([]*RenderedTask, []DispatchPlan, error) {
	axis, err := p.loopAxisView(in)
	var iters []map[string]any
	if err == nil {
		iters, err = resolveLoopItems(p.cel, in, task.Loop, asName, axis)
	}
	if err != nil {
		// Unresolvable items in a skipped task is NOT an error: emit one placeholder.
		rt := p.loopSkipPlaceholder(task, startIndex, skip, in.Modules)
		return []*RenderedTask{rt}, []DispatchPlan{{TaskIndex: startIndex}}, nil
	}

	tasks := make([]*RenderedTask, 0, len(iters))
	plans := make([]DispatchPlan, 0, len(iters))
	idx := startIndex
	for range iters {
		tasks = append(tasks, p.loopSkipPlaceholder(task, idx, skip, in.Modules))
		plans = append(plans, DispatchPlan{TaskIndex: idx})
		idx++
	}
	return tasks, plans, nil
}

// loopSkipPlaceholder builds one skip-placeholder for a loop iteration:
// Params=nil (render skipped), first host's flow_context, When/ID/Register +
// onchanges/onfail names carried through directly, bypassing renderTaskIter
// (single source of Index/Passage; Passage is stamped by stampPassage in
// Render's caller).
//
// onChangesNames/onFailNames are carried through symmetrically with
// staticSkipPlaceholder (pipeline.go): without this, requisite names of a
// static-false loop task would be lost on the skip-placeholder, and the
// final resolveOnChanges/resolveOnFail wouldn't find their sources — a
// latent requisite loss for a loop task with onchanges:/onfail:.
func (p *Pipeline) loopSkipPlaceholder(task config.Task, idx int, skip *structpb.Struct, modules config.ModuleManifestResolver) *RenderedTask {
	rt := &RenderedTask{
		Index:          idx,
		Name:           task.Name,
		Register:       task.Register,
		ID:             task.ID,
		Timeout:        task.Timeout,
		When:           task.When,
		ChangedWhen:    task.ChangedWhen,
		FailedWhen:     task.FailedWhen,
		onChangesNames: task.OnChanges,
		onFailNames:    task.OnFail,
		FlowContext:    skip,
	}
	if task.Module != nil {
		rt.Module = task.Module.Module
		rt.SecretOutput = config.SecretOutputFields(rt.Module, modules)
	}
	applyConcurrency(rt, task)
	return rt
}

// resolveLoopItems evaluates items and lays it out as an ordered list of loop
// contexts (`map[<as>]=element` + optionally `map[<index_as>]=index/key`).
// items is resolved once per run in a host-invariant context (input/register/
// incarnation; no soulprint — items must not depend on the host), reading `vars`
// through axis ([Pipeline.loopAxisView]).
//
// array → as=element, index_as=0-based; object → as=value, index_as=key,
// alphabetical order by key (destiny/tasks.md §7). Scalar/string (e.g. items
// that didn't resolve to a collection) → error: loop requires array/object.
func resolveLoopItems(engine *cel.Engine, in RenderInput, loop *config.LoopSpec, asName string, axis *cel.VarView) ([]map[string]any, error) {
	resolved, err := renderValue(engine, loop.Items, loopInvariantVars(in, nil, axis), "loop.items")
	if err != nil {
		return nil, err
	}

	indexAs := loop.IndexAs
	switch coll := resolved.(type) {
	case []any:
		out := make([]map[string]any, len(coll))
		for i, el := range coll {
			ctx := map[string]any{asName: el}
			if indexAs != "" {
				ctx[indexAs] = i
			}
			out[i] = ctx
		}
		return out, nil
	case map[string]any:
		keys := make([]string, 0, len(coll))
		for k := range coll {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]map[string]any, len(keys))
		for i, k := range keys {
			ctx := map[string]any{asName: coll[k]}
			if indexAs != "" {
				ctx[indexAs] = k
			}
			out[i] = ctx
		}
		return out, nil
	default:
		return nil, fmt.Errorf("loop.items evaluated to %T, expected array or object", resolved)
	}
}

// loopInvariantVars is the host-invariant context for the loop axis: input/
// register/incarnation/vars + current iteration variables (`<as>`/
// `<index_as>`), WITHOUT soulprint.self. items and when: resolve in exactly
// this context — all host-invariant in the pilot (symmetric with
// resolveCovenList: on: also resolves not per-host). `vars` is axis, the
// service and scenario rungs and never the task's own (`items: ${ vars.users }`,
// [Pipeline.loopAxisView]). loopVars=nil for items itself (no loop variables
// yet).
//
// soulprint.hosts (+ .where) IS available: it's the run's host-invariant
// roster (not per-host facts), a legitimate items source (`items:
// ${ soulprint.hosts.where("role == 'replica'") }`). soulprint.self is still
// unavailable in loop — per-host loop is deferred; a soulprint reference in
// loop.when is rejected by a separate guard (reLoopWhenSoulprint) BEFORE this
// context is built.
func loopInvariantVars(in RenderInput, loopVars map[string]any, axis *cel.VarView) cel.Vars {
	return cel.Vars{
		Ctx:            in.Ctx, // vault() in items/when: cancel/timeout + memo + §7 fence
		Input:          in.Input,
		Register:       in.Register,
		Incarnation:    incarnationVars(in, len(in.Hosts)),
		SoulprintHosts: soulprintHosts(in),
		View:           axis,
		Loop:           loopVars,
		AllowHosts:     !in.destinyIsolated,
		// compute is not in the loop axis (NIM-619): declared, so a reference names
		// the namespace and this context instead of a key that isn't the problem.
		ComputeScope: cel.ComputeOutOfScopeLoopAxis,
		// No host is bound here, and since NIM-908 that is said rather than left to
		// an empty soulprint map: `soulprint.self` in items/when is refused by name,
		// and so is an `input.<name>` the destiny renders per host — the loop expands
		// into tasks every targeted host runs, so an iteration set built from one
		// host's value would be that host's set imposed on all of them.
		HostScope:         cel.HostFreeLoopAxis,
		HostVariantInputs: in.hostVariantInputs,
		HostVariantVars:   in.hostVariantVars,
	}
}

// reLoopWhenSoulprint catches a soulprint reference in loop.when. when: is
// host-invariant in the pilot (evaluated once per run, like items), so a
// host-variant predicate over a specific host's soulprint isn't supported —
// per-host loop filtering is deferred (soulprint.hosts / E3).
var reLoopWhenSoulprint = regexp.MustCompile(`\bsoulprint\b`)

// evalLoopWhen evaluates the per-item loop.when filter in the same
// host-invariant context as items (input/register/incarnation + loop
// variables, no soulprint). Empty when → true (no filter).
//
// when: is meant as a filter over the element's CONTENT (`item.enabled`),
// not a per-host predicate. A soulprint reference → a clear error
// (host-variant when in loop isn't supported in the pilot), rather than
// silently deciding based on the first host: symmetric with the
// host-invariance guard for params (see renderTaskIter). Non-bool result →
// error (when: must return a boolean, like where:).
func evalLoopWhen(engine *cel.Engine, in RenderInput, when string, loopVars map[string]any, axis *cel.VarView) (bool, error) {
	if when == "" {
		return true, nil
	}
	if reLoopWhenSoulprint.MatchString(when) {
		return false, fmt.Errorf(
			"loop.when %q references soulprint - host-variant when in loop isn't supported in the pilot (loop is host-invariant; per-host loop filtering is deferred)",
			when)
	}
	return evalBoolExpr(engine, "loop.when", when, loopInvariantVars(in, loopVars, axis))
}

// loopAxisView is the view `loop.items:` and `loop.when:` read (ADR-0082 amendment
// 2026-10-15, point 5): the service and scenario rungs, never the task's own — a
// task-rung definition is evaluated per iteration and may read the loop binding,
// so the set being iterated cannot depend on it. The scenario rung is evaluated
// with the loop axis's compute scope, so a scenario definition reading `compute.*`
// is refused here (point 6). Built once per loop task: no rung of it reads the
// iteration. nil in a destiny pass, as before: the loop axis there sees no vars.
func (p *Pipeline) loopAxisView(in RenderInput) (*cel.VarView, error) {
	return p.runLevelView(in, loopInvariantVars(in, nil, nil))
}
