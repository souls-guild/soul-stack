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

	// A static-when-false loop task never reaches here: both call sites
	// (scenario loop in pipeline.go, destiny.go) call emitStaticWhenSkip
	// first, which emits an N/1 skip-placeholder via loopStaticSkip for a
	// statically-false loop and continues BEFORE renderLoopTask. So by here
	// static-when is guaranteed active (true) or absent — a repeated
	// static-when gate would be unreachable and just duplicate per-host
	// staticWhenSkips. renderLoopTask only sees the fan-out of the active
	// branch.
	iters, err := resolveLoopItems(p.cel, in, task.Loop, asName)
	if err != nil {
		return nil, nil, fmt.Errorf("render: task %q loop: %w", task.Name, err)
	}

	width := serialWidth(task.Serial, len(targeted))
	tasks := make([]*RenderedTask, 0, len(iters))
	plans := make([]DispatchPlan, 0, len(iters))
	idx := startIndex
	for _, it := range iters {
		keep, werr := evalLoopWhen(p.cel, in, task.Loop.When, it)
		if werr != nil {
			return nil, nil, fmt.Errorf("render: task %q: %w", task.Name, werr)
		}
		if !keep {
			continue
		}

		rt, rerr := p.renderTaskIter(ctx, in, task, idx, targeted, it)
		if rerr != nil {
			return nil, nil, rerr
		}
		tasks = append(tasks, rt)
		plans = append(plans, DispatchPlan{
			TaskIndex:   idx,
			TargetSIDs:  sidsOf(targeted),
			SerialWidth: width,
		})
		idx++
	}
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
	iters, err := resolveLoopItems(p.cel, in, task.Loop, asName)
	if err != nil {
		// Unresolvable items in a skipped task is NOT an error: emit one placeholder.
		rt := p.loopSkipPlaceholder(task, startIndex, skip)
		return []*RenderedTask{rt}, []DispatchPlan{{TaskIndex: startIndex}}, nil
	}

	tasks := make([]*RenderedTask, 0, len(iters))
	plans := make([]DispatchPlan, 0, len(iters))
	idx := startIndex
	for range iters {
		tasks = append(tasks, p.loopSkipPlaceholder(task, idx, skip))
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
func (p *Pipeline) loopSkipPlaceholder(task config.Task, idx int, skip *structpb.Struct) *RenderedTask {
	return &RenderedTask{
		Index:          idx,
		Name:           task.Name,
		Module:         task.Module.Module,
		Register:       task.Register,
		ID:             task.ID,
		NoLog:          task.NoLog,
		Timeout:        task.Timeout,
		When:           task.When,
		ChangedWhen:    task.ChangedWhen,
		FailedWhen:     task.FailedWhen,
		onChangesNames: task.OnChanges,
		onFailNames:    task.OnFail,
		FlowContext:    skip,
	}
}

// resolveLoopItems evaluates items and lays it out as an ordered list of loop
// contexts (`map[<as>]=element` + optionally `map[<index_as>]=index/key`).
// items is resolved once per run in a host-invariant context (input/register/
// incarnation; no soulprint — items must not depend on the host).
//
// array → as=element, index_as=0-based; object → as=value, index_as=key,
// alphabetical order by key (destiny/tasks.md §7). Scalar/string (e.g. items
// that didn't resolve to a collection) → error: loop requires array/object.
func resolveLoopItems(engine *cel.Engine, in RenderInput, loop *config.LoopSpec, asName string) ([]map[string]any, error) {
	resolved, err := renderValue(engine, loop.Items, loopInvariantVars(in, nil), "loop.items")
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
		return nil, fmt.Errorf("loop.items вычислился в %T, ожидался array или object", resolved)
	}
}

// loopInvariantVars is the host-invariant context for the loop axis: input/
// register/incarnation/essence + current iteration variables (`<as>`/
// `<index_as>`), WITHOUT soulprint.self. items and when: resolve in exactly
// this context — all host-invariant in the pilot (symmetric with
// resolveCovenList: on: also resolves not per-host). essence is
// host-invariant, so it's available in items/when (`items:
// ${ essence.users }`). loopVars=nil for items itself (no loop variables yet).
//
// soulprint.hosts (+ .where) IS available: it's the run's host-invariant
// roster (not per-host facts), a legitimate items source (`items:
// ${ soulprint.hosts.where("role == 'replica'") }`). soulprint.self is still
// unavailable in loop — per-host loop is deferred; a soulprint reference in
// loop.when is rejected by a separate guard (reLoopWhenSoulprint) BEFORE this
// context is built.
func loopInvariantVars(in RenderInput, loopVars map[string]any) cel.Vars {
	return cel.Vars{
		Input:          in.Input,
		Register:       in.Register,
		Incarnation:    incarnationVars(in, len(in.Hosts)),
		SoulprintHosts: soulprintHosts(in),
		Essence:        in.Essence,
		Loop:           loopVars,
		AllowHosts:     !in.destinyIsolated,
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
func evalLoopWhen(engine *cel.Engine, in RenderInput, when string, loopVars map[string]any) (bool, error) {
	if when == "" {
		return true, nil
	}
	if reLoopWhenSoulprint.MatchString(when) {
		return false, fmt.Errorf(
			"loop.when %q ссылается на soulprint — host-вариативный when в loop не поддержан в пилоте (loop host-инвариантен; per-host loop-фильтрация отложена)",
			when)
	}
	return evalBoolExpr(engine, "loop.when", when, loopInvariantVars(in, loopVars))
}
