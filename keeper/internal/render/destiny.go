package render

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// ResolvedDestiny is a materialized destiny for an apply task: parsed tasks
// plus the `input:` contract. Returned by [DestinyResolver]. The isolated
// render pass (V2, ADR-009) renders Tasks in its own CEL env, seeing only
// apply.input against the Input contract — no scenario scope (vars/register/
// soulprint).
type ResolvedDestiny struct {
	// Name is the destiny name (diagnostics only).
	Name string
	// Tasks is the flat task list from the destiny's `tasks/main.yml`.
	Tasks []config.Task
	// Input is the `destiny.yml` input: schema, for a defense-in-depth check
	// of apply.input against the contract.
	Input config.InputSchemaMap
	// Vars holds raw destiny locals from `vars.yml` (docs/destiny/vars.md),
	// unvalidated (vars are untyped). CEL expressions `${ … }` in values
	// resolve inside the destiny pass (renderApplyDestiny) over
	// input+soulprint.self+incarnation, isolated from scenario scope. The
	// result is the base `vars.*` layer; task-level `vars:` overrides
	// same-named keys on top (Option A, vars.md "file-vars/task-vars merge").
	// nil means the destiny has no locals.
	Vars map[string]any
	// Templates reads the `.tmpl` snapshot of THIS destiny (its templates
	// live in their own snapshot, not the service's — single-level resolve,
	// destiny has no scenario-local layer). nil means core.file.rendered
	// inside the destiny fails with a handoff error (TemplateReader not
	// configured); DestinyResolver must populate it (prod: snapshot-backed,
	// Trial: fixture-backed).
	Templates TemplateReader
}

// DestinyResolver resolves a destiny name from an apply task into a parsed
// artifact. In prod it's a loader-backed adapter (git snapshot of the
// destiny, scenario-runner); in hermetic Trial L0, a fixture resolver
// (destiny next to case.yml). A nil resolver rejects apply:destiny with
// ErrUnsupportedDSL.
type DestinyResolver interface {
	Resolve(ctx context.Context, name string) (*ResolvedDestiny, error)
}

// renderApplyDestiny runs the isolated destiny render pass (V2, ADR-009) for
// the parent apply task and returns its rendered tasks plus dispatch plans.
//
// Isolation (CRITICAL): the destiny sees ONLY its own input: (resolved
// apply.input), not scenario input/vars/register/soulprint. This is a
// structural boundary — a separate RenderInput with empty Register/Essence.
// SoulprintSelf of the host is preserved (per-host facts are a stable layer
// available to any step), but the parent's scenario scope (input, register,
// vars) never reaches the destiny env.
//
// startIndex is the running index of the first destiny task in the parent's
// final plan (RenderedTask.Index/DispatchPlan.TaskIndex increase
// monotonically across the whole plan). loop: on a destiny task expands into
// N RenderedTask (renderLoopTask), idx advances by the iteration count —
// indices stay contiguous (mirrors scenario).
//
// targeted is the apply task's host set (after the parent's on:/where:/
// run_once: resolve). The destiny inherits this roster; per-task on:/where:
// inside a destiny isn't supported in the pilot (guardDestinyTask rejects
// them).
//
// serialWidth is the parent apply task's `serial:` wave width
// (orchestration.md §2.2.1): inherited by all destiny tasks (the whole
// destiny rolls as one rolling wave over hosts). 0 means serial isn't set.
//
// applierRegister is the applier task's own register: (parent.Register). If
// non-empty, renderApplyDestiny emits a synthetic terminal `core.noop.run`
// after the child tasks, with Register=applierRegister and AggregateOf=the
// global indices of all child destiny tasks (orchestration.md §2.1.1,
// applier-register materialization, Option B): Soul builds its register_data
// as an aggregate (`changed=OR(child.changed)`, similarly for
// failed/timed_out) so an external `onchanges:[<applier>]` /
// `when: register.<applier>.changed` resolves. "" means no terminal is
// emitted (applier without register: — no index reserved, bit-for-bit
// unchanged behavior).
func (p *Pipeline) renderApplyDestiny(
	ctx context.Context,
	parentIn RenderInput,
	apply *config.ApplyTask,
	startIndex int,
	targeted []*topology.HostFacts,
	serialWidth int,
	applierRegister string,
) ([]*RenderedTask, []DispatchPlan, error) {
	if parentIn.Destiny == nil {
		return nil, nil, fmt.Errorf("%w: apply: destiny %q - DestinyResolver not configured (RenderInput.Destiny=nil)", ErrUnsupportedDSL, apply.Destiny)
	}

	resolved, err := parentIn.Destiny.Resolve(ctx, apply.Destiny)
	if err != nil {
		return nil, nil, fmt.Errorf("render: apply destiny %q: %w", apply.Destiny, err)
	}

	// Resolve apply.input into destiny input values + defense-in-depth check
	// against the destiny's input: contract (required params present,
	// defaults applied). apply.input renders in scenario env (the parent
	// resolves what to pass); the destiny itself sees only the result.
	destinyInput, err := p.resolveApplyInput(parentIn, apply, resolved, targeted)
	if err != nil {
		return nil, nil, err
	}

	// Isolated destiny RenderInput: only input + roster + incarnation meta.
	// Register/Essence/RegisterByHost are empty — destiny doesn't see scenario
	// scope. destinyIsolated=true: soulprint.hosts/soulprint.where inside a
	// destiny is an isolation error (orchestration.md §4.1); the host
	// projection isn't passed into a destiny.
	destinyIn := RenderInput{
		Scenario:        &config.ScenarioManifest{Name: resolved.Name, Tasks: resolved.Tasks},
		Input:           destinyInput,
		Incarnation:     parentIn.Incarnation,
		Hosts:           targeted,
		Templates:       resolved.Templates, // .tmpl from THIS destiny's own snapshot
		Ctx:             ctx,                // vault() in destiny params: cancel/timeout for ReadKV
		destinyIsolated: true,
		// seal (ADR-010 §7.4): same run-wide accumulator — destiny params with
		// `${ vault(...) }` get marked sealed just like scenario ones. The
		// destiny-input secret flag is only detected when ResolvedDestiny
		// carries an Input schema (not wired in the pilot — vault() provenance
		// is caught without the schema; transiting a destiny secret input is a
		// separate slice, see observations).
		Sealed: parentIn.Sealed,
	}

	// destiny locals from vars.yml (Option A, vars.md): resolved ONCE per
	// pass, per host, over the destiny env (destiny input + soulprint.self +
	// incarnation), isolated from scenario scope. resolveDestinyVars builds
	// its own base env with empty Register/Essence/Vars and AllowHosts=false —
	// `vars.<other>`/`register.*`/`essence.*`/`soulprint.hosts` in a vars.yml
	// value is an isolation error.
	destinyVars, verr := p.resolveDestinyVars(destinyIn, resolved.Vars, targeted)
	if verr != nil {
		return nil, nil, verr
	}
	destinyIn.DestinyVarsResolved = destinyVars

	tasks := make([]*RenderedTask, 0, len(resolved.Tasks))
	plans := make([]DispatchPlan, 0, len(resolved.Tasks))
	idx := startIndex

	// includeGroupKeep caches the conditional-include decision inside a
	// destiny (group-drop, ADR-009 amendment): group id (Task.IncludeGroupID,
	// set by ExpandIncludes) → keep/drop. Separate from the scenario cache
	// (pipeline.go) — the destiny pass is isolated, its own env. include-when
	// is evaluated ONCE per group over destinyIn (isolated destiny input),
	// host-invariant. Unconditional tasks (IncludeGroupID==0) never land here.
	includeGroupKeep := map[int]bool{}

	for i := range resolved.Tasks {
		task := resolved.Tasks[i]

		// Conditional-include group-drop (ADR-009 amendment) — mirrors the
		// scenario loop (pipeline.go), runs BEFORE emitStaticWhenSkip and
		// block handling. A task with IncludeGroupID!=0 had its include
		// expanded under a static `when:` by config.ExpandIncludes.
		// include-when evaluates ONCE per group in the ISOLATED destiny env
		// (destinyIn: input = resolved apply.input + schema defaults, not
		// scenario scope) — never parentIn. include-when false is a REAL
		// drop: continue without emitting a RenderedTask and without idx++
		// (no index reserved, the task physically disappears).
		// IncludeGroupID is orthogonal to block: group-drop sits ABOVE the
		// block branch, so keep=false drops the whole group (including a
		// block task and its children) before renderDestinyBlock runs.
		// includeGroupKeep is a separate cache from scenario's; register
		// isolation matches scenario (cross-file register on a dropped group
		// is lint-forbidden offline, so onchanges can't break).
		if task.IncludeGroupID != 0 {
			keep, ok := includeGroupKeep[task.IncludeGroupID]
			if !ok {
				k, derr := p.evalIncludeWhen(destinyIn, task.IncludeWhen)
				if derr != nil {
					return nil, nil, derr
				}
				keep = k
				includeGroupKeep[task.IncludeGroupID] = keep
			}
			if !keep {
				continue // group-drop: no RenderedTask, no idx — a real exclusion.
			}
		}

		// Static-when PRECEDES guardDestinyTask (ADR-012(d), same invariant as
		// the scenario loop in pipeline.go): a statically-false `when:` gates
		// the task off before the DSL guard, so unsupported DSL (`parallel:`)
		// in an inactive destiny branch doesn't block the active one. Fixes
		// the multi-action redis destiny: diagnostic.yml carries
		// `parallel: true` + `when: input.action=='diagnose'`, inactive at
		// action=update_acls — previously guardDestinyTask rejected it with
		// ErrUnsupportedDSL before static-when ran, failing the whole destiny
		// pass.
		if skipped, serr := p.emitStaticWhenSkip(ctx, destinyIn, task, &tasks, &plans, &idx); serr != nil {
			return nil, nil, serr
		} else if skipped {
			continue
		}

		if gerr := guardDestinyTask(task, i, resolved.Name); gerr != nil {
			return nil, nil, gerr
		}

		// block: inside a destiny pass (ADR-009 amendment 2026-06-24) —
		// render-time fan-out into the flat layer, like scenario
		// (renderBlockTask), but with destiny semantics: env-agnostic
		// inheritance (mergeBlockInheritance: when/vars/requisites), the
		// roster is inherited WHOLESALE (block does NOT narrow hosts —
		// where/on on a destiny block are rejected by
		// guardDestinyBlockChild), and the destiny parent's serialWidth
		// propagates into each child's DispatchPlan. A static-when-false
		// block isn't caught by emitStaticWhenSkip (which skips block tasks)
		// — it falls through here instead: walkBlockChildren ANDs block.when
		// into each child, and each child emits its OWN skip placeholder
		// with register/requisites (flat register scope stays intact on
		// skip — children's register is visible outside via
		// resolveOnChanges).
		if task.Block != nil {
			bt, bp, berr := p.renderDestinyBlock(ctx, destinyIn, task, idx, targeted, serialWidth)
			if berr != nil {
				return nil, nil, berr
			}
			tasks = append(tasks, bt...)
			plans = append(plans, bp...)
			idx += len(bt)
			continue
		}

		destinyTargeted, terr := resolveTargets(p.cel, destinyIn, task)
		if terr != nil {
			return nil, nil, terr
		}

		// loop: on a destiny task (slice E lifted) — render-time fan-out, like
		// the scenario loop (pipeline.go). renderLoopTask is path-agnostic:
		// items/when resolve via loopInvariantVars over destinyIn, so
		// AllowHosts=false and empty Register inherit destiny isolation
		// (soulprint.hosts/register in items is an isolation error). idx
		// advances by the number of expanded iterations.
		if task.Loop != nil {
			lt, lp, lerr := p.renderLoopTask(ctx, destinyIn, task, idx, destinyTargeted)
			if lerr != nil {
				return nil, nil, lerr
			}
			tasks = append(tasks, lt...)
			plans = append(plans, lp...)
			idx += len(lt)
			continue
		}

		rt, rerr := p.renderTask(ctx, destinyIn, task, idx, destinyTargeted)
		if rerr != nil {
			return nil, nil, rerr
		}

		tasks = append(tasks, rt)
		plans = append(plans, DispatchPlan{
			TaskIndex:   idx,
			TargetSIDs:  sidsOf(destinyTargeted),
			SerialWidth: serialWidth,
		})
		idx++
	}

	// applier-register materialization (orchestration.md §2.1.1, Option B): if
	// the applier task carries register:, the destiny run's summary MUST be
	// addressable as register.<applier>.* (external onchanges:[<applier>] /
	// when: register.<applier>.changed). We emit a synthetic TERMINAL
	// `core.noop.run` task (last in the group, so all children are already in
	// registerByIdx by the time it runs on Soul) with Register=applierRegister
	// and AggregateOf=the GLOBAL Index of every child destiny task of this
	// applier. Soul builds its register_data not from the ApplyEvent (noop is
	// trivially changed=false) but as an aggregate (aggregateRegisterData:
	// changed=OR(child.changed), likewise for failed/timed_out). The terminal
	// gets its Passage from the parent's stampPassage (pipeline.go) — not set
	// here. There may be no child tasks (the whole destiny dropped via
	// include-when, or where: filtered everything out) — AggregateOf is then
	// empty and the aggregate collapses to changed/failed/timed_out=false
	// (no-op applier).
	if applierRegister != "" {
		aggregateOf := make([]int, 0, len(tasks))
		for _, t := range tasks {
			aggregateOf = append(aggregateOf, t.Index)
		}
		tasks = append(tasks, &RenderedTask{
			Index: idx,
			Name:  "applier-register " + applierRegister,
			// core.noop ignores params (docs/module/core/noop) — the empty
			// Struct only exists so the proto field isn't nil (ApplyRequest
			// assembly).
			Params:      &structpb.Struct{Fields: map[string]*structpb.Value{}},
			Module:      "core.noop.run",
			Register:    applierRegister,
			AggregateOf: aggregateOf,
		})
		plans = append(plans, DispatchPlan{
			TaskIndex:   idx,
			TargetSIDs:  sidsOf(targeted),
			SerialWidth: serialWidth,
		})
		idx++
	}

	return tasks, plans, nil
}

// resolveDestinyVars resolves raw destiny locals from `vars.yml`, per host,
// in the destiny env (Option A, vars.md). Returns sid → name → resolved
// value.
//
// Isolation (CRITICAL): the base env is built by hostVars over destinyIn —
// the isolated destiny RenderInput (Register/Essence empty,
// destinyIsolated=true → AllowHosts=false). Available: input.* (destiny
// input, not scenario), soulprint.self.*, incarnation.*;
// `register.*`/`essence.*`/`soulprint.hosts` are isolation errors. base.Vars
// is empty at the START of the layer (resolveVarLayer accumulates it) — a
// `vars.<other>` reference only resolves against a file-var of the SAME
// layer (var→var allowed, eager-topological); a reference to another
// layer/register/soulprint.hosts is an isolation error.
//
// var→var (vars.md, ADR-009/ADR-010 amendment 2026-06-24): a file-var can
// reference another file-var via `${ vars.<other> }`; resolveVarLayer builds
// a graph from VarRefs and resolves in topological order. Key order in
// vars.yml doesn't matter. A cycle gives ErrVarCycle with a trace; a
// reference to an unknown var gives ErrVarUnknownRef (eager, even if the
// referencing var is unused). Isolation isn't weakened: var→var stays
// strictly within the file layer.
//
// Per-host resolve: values may reference soulprint.self (host-variant), so
// each host gets its own map. nil raw → nil (destiny has no locals). Empty
// targeted (where: filtered everyone out) → one synthetic host under key "".
func (p *Pipeline) resolveDestinyVars(destinyIn RenderInput, raw map[string]any, targeted []*topology.HostFacts) (map[string]map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	hosts := targeted
	if len(hosts) == 0 {
		hosts = []*topology.HostFacts{{}}
	}
	out := make(map[string]map[string]any, len(hosts))
	for _, host := range hosts {
		base := hostVars(destinyIn, host, len(targeted)) // base.Vars empty — start of layer
		resolved, err := resolveVarLayer(p.cel, raw, base)
		if err != nil {
			return nil, fmt.Errorf("render: destiny %q (vars.yml, host %s): %w", destinyIn.Scenario.Name, host.SID, err)
		}
		out[host.SID] = resolved
	}
	return out, nil
}

// resolveApplyInput computes the destiny input from apply.input.
//
// apply.input is literals/CEL in scenario env (the parent decides what to
// pass to the destiny). We resolve it in the parent's context
// (input/incarnation/soulprint.self of the first targeted host, or empty),
// then check the result against the destiny's input: schema (defense in
// depth, ADR-009): required params present, missing ones with a default get
// filled in.
//
// apply.input is host-invariant in the pilot: values are computed once (on
// the first targeted host), same as module-task params (host variance is
// out of pilot scope).
func (p *Pipeline) resolveApplyInput(
	parentIn RenderInput,
	apply *config.ApplyTask,
	resolved *ResolvedDestiny,
	targeted []*topology.HostFacts,
) (map[string]any, error) {
	var host *topology.HostFacts
	if len(targeted) > 0 {
		host = targeted[0]
	} else {
		host = &topology.HostFacts{}
	}
	vars := hostVars(parentIn, host, len(targeted))

	rendered := make(map[string]any, len(apply.Input))
	for name, raw := range apply.Input {
		val, err := renderValue(p.cel, raw, vars, "apply.input."+name)
		if err != nil {
			return nil, fmt.Errorf("render: apply destiny %q input %q: %w", apply.Destiny, name, err)
		}
		rendered[name] = val
	}

	if err := applyInputContract(rendered, resolved.Input, apply.Destiny); err != nil {
		return nil, err
	}
	return rendered, nil
}

// applyInputContract checks the resolved apply.input against the destiny's
// input: schema (defense in depth, ADR-009): fills in defaults for missing
// params, rejects a missing required param with no default.
//
// Full type/pattern/enum validation of values against the schema is a
// separate validator (doesn't exist yet for either scenario-input or
// destiny-input in this project); this is the minimal required+default
// contract needed for a correct destiny CEL render.
func applyInputContract(values map[string]any, schema config.InputSchemaMap, destiny string) error {
	for name, sc := range schema {
		if sc == nil {
			continue
		}
		if _, ok := values[name]; ok {
			continue
		}
		if sc.Default != nil {
			values[name] = sc.Default
			continue
		}
		if sc.Required {
			return fmt.Errorf("render: apply destiny %q: required input %q not passed and has no default", destiny, name)
		}
	}
	return nil
}

// guardDestinyTask rejects nested DSL constructs outside pilot scope
// (parallel:/nested apply:) and scenario-only keys on a destiny task
// (serial:/run_once: — not allowed in a destiny, docs/destiny/tasks.md §3;
// scenario-level serial: is inherited by the destiny through
// renderApplyDestiny's parameter, not a per-task field). The pilot supports
// a flat destiny: module tasks with on:/where: + loop: (slice E lifted —
// fan-out inherits destiny isolation via loopInvariantVars:
// AllowHosts=false, Register empty) + block: (ADR-009 amendment 2026-06-24 —
// render-time fan-out, renderDestinyBlock). include: inside a destiny
// expands BEFORE render (within-destiny, in DestinyLoader.parseTasks / the
// fixture resolver); an include that reaches render is ErrUnexpandedInclude
// (an expansion bug), not "outside pilot".
//
// ★ A block task (task.Block != nil) PASSES guardDestinyTask: in
// renderApplyDestiny, guardDestinyTask runs BEFORE the block branch
// (guardDestinyTask :145 → `if task.Block != nil` renderDestinyBlock :157).
// So the `case task.Block != nil` below is LOAD-BEARING on the live path: it
// deliberately SKIPS the block (return nil, not treating it as a module
// task), after which renderApplyDestiny branches into renderDestinyBlock.
// Don't remove it as "dead code" — without it, block would fall into
// `case task.Module == nil` (not a module task). The key boundary INSIDE a
// destiny block is guardDestinyBlockChild.
func guardDestinyTask(task config.Task, idx int, destiny string) error {
	switch {
	case task.Apply != nil:
		return fmt.Errorf("%w: nested apply: in destiny %q (task[%d] %q)", ErrUnsupportedDSL, destiny, idx, task.Name)
	case task.Include != nil:
		return fmt.Errorf("%w: in destiny %q (task[%d] %q)", ErrUnexpandedInclude, destiny, idx, task.Name)
	case task.Parallel:
		return fmt.Errorf("%w: parallel: in destiny %q (task[%d] %q)", ErrUnsupportedDSL, destiny, idx, task.Name)
	case task.RunOnce:
		return fmt.Errorf("%w: run_once: in destiny %q (task[%d] %q)", ErrUnsupportedDSL, destiny, idx, task.Name)
	case task.Serial != nil:
		return fmt.Errorf("%w: serial: in destiny %q (task[%d] %q)", ErrUnsupportedDSL, destiny, idx, task.Name)
	case task.Block != nil:
		// LOAD-BEARING (not dead code) — see doc comment above: block passes
		// this guard first (return nil), renderDestinyBlock handles it next.
		return nil
	case task.Module == nil:
		return fmt.Errorf("%w: task[%d] %q in destiny %q is not a module task", ErrUnsupportedDSL, idx, task.Name, destiny)
	}
	return nil
}

// renderDestinyBlock expands a block task INSIDE a destiny pass (ADR-009
// amendment 2026-06-24) into the flat RenderedTask layer — mirrors
// renderBlockTask (block.go) with destiny semantics. Reuses the same
// walkBlockChildren traversal (single source of truth for inheritance:
// mergeBlockInheritance → emitStaticWhenSkip → guard → render), differing in
// three layer-specific invariants:
//
//   - child guard is guardDestinyBlockChild: rejects scenario orchestration
//     (where/serial/run_once/on/parallel/loop/include/apply) on the block or
//     its children — these keys are meaningless in a destiny (no per-child
//     roster resolve).
//   - the roster is inherited WHOLESALE (block does NOT narrow hosts): the
//     target callback always returns the block's targeted, unlike scenario
//     where a child's where: narrows it.
//   - the destiny parent's serialWidth propagates into every child's
//     DispatchPlan (a block carries no serial of its own — rejected by the
//     guard; width comes from the parent apply task's serial: via
//     renderApplyDestiny).
//
// width=0 for an apply child is unused — apply on a child is rejected by
// guardDestinyBlockChild earlier, so the child.Apply branch in
// walkBlockChildren is unreachable.
//
// flat register scope (case #10): a block child's register is visible
// OUTSIDE the block — children merge into the destiny pass's shared flat
// tasks[] with contiguous idx, and resolveOnChanges/resolveOnFail at Render
// output resolve against the flat list (collectFlatAddresses in the config
// layer is already recursive through block:).
func (p *Pipeline) renderDestinyBlock(
	ctx context.Context,
	destinyIn RenderInput,
	blockTask config.Task,
	startIndex int,
	targeted []*topology.HostFacts,
	width int,
) ([]*RenderedTask, []DispatchPlan, error) {
	// Key boundary on the block node ITSELF. A top-level block PASSES
	// guardDestinyTask (which skips it via `case task.Block`, see above) but
	// not its module-specific key checks, so guardDestinyBlock here checks
	// them on the block itself. A nested block is caught by
	// guardDestinyBlockChild as a block child — same error text for both
	// paths.
	if gerr := guardDestinyBlock(blockTask); gerr != nil {
		return nil, nil, gerr
	}
	// The roster is inherited by the block WHOLESALE: a destiny block carries
	// no on/where (rejected by the guard) — children apply to the same hosts
	// as the block.
	childTarget := func(_ config.Task) ([]*topology.HostFacts, error) {
		return targeted, nil
	}
	// nested block → recurse into the same destiny layer (cascading inheritance).
	childRecurse := func(child config.Task, idx int, childTargeted []*topology.HostFacts) ([]*RenderedTask, []DispatchPlan, error) {
		return p.renderDestinyBlock(ctx, destinyIn, child, idx, childTargeted, width)
	}
	return p.walkBlockChildren(ctx, destinyIn, blockTask, startIndex, width, guardDestinyBlockChild, childTarget, childRecurse)
}

// guardDestinyBlockChild is the key boundary for a destiny block (render
// layer — the config layer is shared between both layers and block keys are
// valid there). Rejects scenario orchestration on a destiny block child with
// an explicit [ErrUnsupportedDSL]:
//
//	where / serial / run_once / on / parallel / loop / include / apply
//
// — all meaningless in a destiny (no per-child roster resolve, no nested
// destiny). VALID (env-agnostic inheritance + flat core): when (AND-merge),
// name, vars, onchanges/onfail/require (union), nested block:; a child is
// module: or a nested block:.
//
// Mirrors guardPilotBlockChild (scenario layer) but stricter: scenario
// allows apply/serial/run_once/where/on on a child, destiny does not.
//
// The key boundary on the destiny block node ITSELF (not the child) is
// guardDestinyBlock, called from renderDestinyBlock (a block passes
// guardDestinyTask, whose `case task.Block` skips it, then the
// renderDestinyBlock branch calls guardDestinyBlock).
func guardDestinyBlockChild(child config.Task, idx int, blockName string) error {
	switch {
	case child.Where != "":
		return fmt.Errorf("%w: where: on a destiny-block child %q (task[%d] %q) - scenario orchestration in a destiny is forbidden", ErrUnsupportedDSL, blockName, idx, child.Name)
	case child.Serial != nil:
		return fmt.Errorf("%w: serial: on a destiny-block child %q (task[%d] %q) - scenario orchestration in a destiny is forbidden", ErrUnsupportedDSL, blockName, idx, child.Name)
	case child.RunOnce:
		return fmt.Errorf("%w: run_once: on a destiny-block child %q (task[%d] %q) - scenario orchestration in a destiny is forbidden", ErrUnsupportedDSL, blockName, idx, child.Name)
	case child.On != nil:
		return fmt.Errorf("%w: on: on a destiny-block child %q (task[%d] %q) - scenario orchestration in a destiny is forbidden", ErrUnsupportedDSL, blockName, idx, child.Name)
	case child.Parallel:
		return fmt.Errorf("%w: parallel: on a destiny-block child %q (task[%d] %q) - scenario orchestration in a destiny is forbidden", ErrUnsupportedDSL, blockName, idx, child.Name)
	case child.Loop != nil:
		return fmt.Errorf("%w: loop: on a destiny-block child %q (task[%d] %q) - outside destiny block scope", ErrUnsupportedDSL, blockName, idx, child.Name)
	case child.Include != nil:
		return fmt.Errorf("%w: include: on a destiny-block child %q (task[%d] %q)", ErrUnexpandedInclude, blockName, idx, child.Name)
	case child.Apply != nil:
		return fmt.Errorf("%w: apply: on a destiny-block child %q (task[%d] %q) - nested apply in a destiny is forbidden", ErrUnsupportedDSL, blockName, idx, child.Name)
	case child.Module == nil && child.Block == nil:
		return fmt.Errorf("%w: task[%d] %q in destiny-block %q is not a module/block task", ErrUnsupportedDSL, idx, child.Name, blockName)
	}
	return nil
}

// guardDestinyBlock is the key boundary on the destiny block node ITSELF
// (not its children). A top-level destiny block branches in
// renderApplyDestiny BEFORE guardDestinyTask, so serial:/on:/run_once:/
// parallel:/loop: on it are caught by neither guardDestinyTask (block
// bypasses it) nor mergeBlockInheritance (which doesn't inherit these keys
// to children — only where: is inherited, the rest stay on the block node).
// We reject them here.
//
// where: on the block node is inherited by children via
// mergeBlockInheritance and would be caught by guardDestinyBlockChild on the
// first child — but an empty block (no children) would leave it unchecked;
// caught here too for completeness.
func guardDestinyBlock(blockTask config.Task) error {
	switch {
	case blockTask.Where != "":
		return fmt.Errorf("%w: where: on destiny-block %q - scenario orchestration in a destiny is forbidden", ErrUnsupportedDSL, blockTask.Name)
	case blockTask.Serial != nil:
		return fmt.Errorf("%w: serial: on destiny-block %q - scenario orchestration in a destiny is forbidden", ErrUnsupportedDSL, blockTask.Name)
	case blockTask.RunOnce:
		return fmt.Errorf("%w: run_once: on destiny-block %q - scenario orchestration in a destiny is forbidden", ErrUnsupportedDSL, blockTask.Name)
	case blockTask.On != nil:
		return fmt.Errorf("%w: on: on destiny-block %q - scenario orchestration in a destiny is forbidden", ErrUnsupportedDSL, blockTask.Name)
	case blockTask.Parallel:
		return fmt.Errorf("%w: parallel: on destiny-block %q - scenario orchestration in a destiny is forbidden", ErrUnsupportedDSL, blockTask.Name)
	case blockTask.Loop != nil:
		return fmt.Errorf("%w: loop: on destiny-block %q - outside destiny block scope", ErrUnsupportedDSL, blockTask.Name)
	}
	return nil
}
