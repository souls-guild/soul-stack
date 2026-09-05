package render

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// ErrDestinyInputInvalid — the values an apply: task passed to a destiny violate
// that destiny's `input:` contract: a required (or required_when) param is
// missing with no default, or a value does not match its declared
// type/enum/pattern/format/length. The destiny render is rejected before any
// task reaches a host (ADR-009 amendment 2026-07-26, NIM-167).
//
// Symmetric with scenario.ErrInputInvalid, but the gate sits at RENDER, not on
// the API request path: destiny input is computed by the scenario, so this
// catches a scenario handing its destiny bad values — the operator never typed
// them.
var ErrDestinyInputInvalid = errors.New("render: destiny input invalid")

// ErrDestinyValidateFailed — a rule of the destiny's top-level `validate:`
// section evaluated to false for the passed input (ADR-009 amendment
// 2026-07-26). Separate from [ErrDestinyInputInvalid] for the same reason
// scenario splits ErrValidateFailed from ErrInputInvalid: "the values do not
// match the schema" and "the values violate a declared invariant" are different
// diagnoses. The wrapped [config.ValidateRuleFailure] carries the rule's
// `message:`.
var ErrDestinyValidateFailed = errors.New("render: destiny validate rule failed")

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
	// Validate is the `destiny.yml` validate: section — declarative invariants
	// over Input (ADR-009 amendment 2026-07-26). Evaluated by resolveApplyInput
	// after the values themselves check out. nil means the destiny declares no
	// invariants.
	Validate []config.ValidateRule
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
// structural boundary — a separate RenderInput with empty Register/ServiceVars.
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
// applier is the whole parent task, not just its `apply:` block — the
// function that renders a task's fan-out has to see the task, or keys land
// nowhere and no one is told (NIM-245). Three of its fields are read here:
//
// applier.Register — if non-empty, renderApplyDestiny emits a synthetic
// terminal `core.noop.run` after the child tasks, with Register=applier.Register
// and AggregateOf=the global indices of all child destiny tasks
// (orchestration.md §2.1.1, applier-register materialization, Option B): Soul
// builds its register_data as an aggregate (`changed=OR(child.changed)`,
// similarly for failed/timed_out) so an external `onchanges:[<applier>]` /
// `when: register.<applier>.changed` resolves. "" means no terminal is emitted
// (applier without register: — no index reserved, bit-for-bit unchanged
// behavior).
//
// applier.Vars — task-level `vars:` (its own, plus any a `block:` above merged
// in), resolved by [Pipeline.resolveApplyInput] into the env that renders
// `apply.input`. NOT inherited by the children: they are answered on the
// CALLER's side, which is where the text was written, and only the resulting
// values cross the boundary (NIM-336).
//
// applier's requisites (`onchanges:`/`onfail:`/`require:`) — merged into every
// child by [mergeApplierInheritance], the same way a block passes its own down
// (destiny/tasks.md §6.5). `when:`/`where:` are deliberately NOT inherited:
// those are resolved in the SCENARIO env, while a child's flow context is built
// in the isolated destiny env, so the same text would mean a different thing on
// the other side of the boundary. The applier's `when:` is decided before this
// call instead (static-when at the scenario level), and a `when:` that cannot be
// decided there is refused by [guardApplierWhen].
func (p *Pipeline) renderApplyDestiny(
	ctx context.Context,
	parentIn RenderInput,
	applier config.Task,
	startIndex int,
	targeted []*topology.HostFacts,
	serialWidth int,
) ([]*RenderedTask, []DispatchPlan, error) {
	apply := applier.Apply
	applierRegister := applier.Register
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
	destinyInput, err := p.resolveApplyInput(parentIn, applier, resolved, targeted)
	if err != nil {
		return nil, nil, err
	}

	// Isolated destiny RenderInput: only input + roster + incarnation meta.
	// Register/ServiceVars/RegisterByHost are empty — destiny doesn't see scenario
	// scope. destinyIsolated=true: soulprint.hosts/soulprint.where inside a
	// destiny is an isolation error (orchestration.md §4.1); the host
	// projection isn't passed into a destiny.
	//
	// The synthetic manifest carries Input as well as Tasks (NIM-812). It is the
	// destiny's OWN `input:` contract, not the caller's, so it grants the pass
	// nothing it could not already see — apply.input was checked against this very
	// schema a few lines up. What it buys is the one reader of `Scenario.Input` in
	// this package, [secretInputNames]: without it `${ input.<secret> }` written
	// INSIDE a destiny is sealed by nothing, and `apply: input:` is the only
	// channel into a destiny (ADR-009 V2), so that is the shape the DSL steers a
	// credential through.
	destinyIn := RenderInput{
		Scenario:        &config.ScenarioManifest{Name: resolved.Name, Tasks: resolved.Tasks, Input: resolved.Input},
		Input:           destinyInput,
		Incarnation:     parentIn.Incarnation,
		Hosts:           targeted,
		Templates:       resolved.Templates, // .tmpl from THIS destiny's own snapshot
		Ctx:             ctx,                // vault() in destiny params: cancel/timeout for ReadKV
		destinyIsolated: true,
		// Modules is carried over, unlike the scenario scope below: it is not
		// scope, it is the plugin-manifest resolver that says which of a module's
		// `output:` fields are declared secret ([ADR-0083] §8). A destiny task
		// runs the same modules as a scenario task, so dropping it would leave
		// §8 redaction dead inside a destiny — a module output masked on the
		// scenario path and printed in full on the destiny path.
		// Guarded by TestRender_ApplyDestiny_SecretOutputStillDerived.
		Modules: parentIn.Modules,
		// ServiceVars stays nil ON PURPOSE, and it is load-bearing rather than an
		// omission: hostVars seeds cel.Vars.Vars from it, resolveTaskVars takes the
		// task layer's `lower` from that, so a nil here is what stops a destiny's
		// task vars reaching the caller's service layer at all. Copying parentIn's
		// value — the shape a `destinyIn := parentIn` refactor would produce —
		// opens the whole service layer to every destiny task var.
		// Guarded by TestRender_ApplyDestiny_ServiceVarsNotLeaked.
		ServiceVars: nil,
		// seal (ADR-010 §7.4): same run-wide accumulator — destiny params with
		// `${ vault(...) }` get marked sealed just like scenario ones, and since
		// NIM-812 so does `${ input.<secret> }`, off the destiny's own schema
		// carried on Scenario above. The accumulator is shared with the parent
		// deliberately: its paths are matched against each task's own params root.
		Sealed: parentIn.Sealed,
	}

	// destiny locals from vars.yml (Option A, vars.md): resolved ONCE per
	// pass, per host, over the destiny env (destiny input + soulprint.self +
	// incarnation), isolated from scenario scope. resolveDestinyVars builds
	// its own base env with empty Register/Vars and AllowHosts=false, so in a
	// vars.yml value `register.*` and `soulprint.hosts` are isolation errors and
	// `vars.<other>` reaches only a file-var of the SAME layer.
	destinyVars, verr := p.resolveDestinyVars(destinyIn, resolved.Vars, targeted)
	if verr != nil {
		return nil, nil, verr
	}
	destinyIn.DestinyVarsResolved = destinyVars

	// The file layer's taint (NIM-811), from the RAW vars.yml text and once for
	// the pass — the values above are per-host, the provenance is not. This is the
	// bottom of the `vars.*` taint every task of this pass stacks its own `vars:`
	// on; a destiny local written as `${ input.<secret> }` is the same hop a task
	// var is, and with the destiny schema now carried it is finally detectable.
	destinyIn.sealedFileVars = sealedVarNames(p.cel, resolved.Vars, scenarioSealSources(destinyIn))

	tasks := make([]*RenderedTask, 0, len(resolved.Tasks))
	plans := make([]DispatchPlan, 0, len(resolved.Tasks))
	idx := startIndex

	// includeGroupKeep caches the conditional-include decision inside a
	// destiny (group-drop, see [Pipeline.keepIncludeGroup]). Separate from the
	// scenario cache (pipeline.go) — the destiny pass is isolated, its own env:
	// include-when is evaluated over destinyIn (isolated destiny input).
	// Threaded into renderDestinyBlock so a within-block include group is
	// decided once for the whole pass.
	includeGroupKeep := includeGroupCache{}

	for i := range resolved.Tasks {
		task := mergeApplierInheritance(applier, resolved.Tasks[i])

		// Conditional-include group-drop (ADR-009 amendment) — mirrors the
		// scenario loop (pipeline.go), runs BEFORE emitStaticWhenSkip and
		// block handling. include-when evaluates ONCE per group in the
		// ISOLATED destiny env (destinyIn: input = resolved apply.input +
		// schema defaults, not scenario scope) — never parentIn. false is a
		// REAL drop: no RenderedTask, no idx++ (the task physically
		// disappears). IncludeGroupID is orthogonal to block: group-drop sits
		// ABOVE the block branch, so keep=false drops the whole group
		// (including a block task and its children) before renderDestinyBlock
		// runs; a group spliced INSIDE a block is gated by walkBlockChildren
		// with this same cache.
		if keep, kerr := p.keepIncludeGroup(destinyIn, task, includeGroupKeep); kerr != nil {
			return nil, nil, kerr
		} else if !keep {
			continue
		}

		// Static-when PRECEDES guardDestinyTask (ADR-012(d), same invariant as
		// the scenario loop in pipeline.go): a statically-false `when:` gates
		// the task off before the DSL guard, so unsupported DSL (`run_once:`,
		// a nested `apply:`) in an inactive destiny branch doesn't block the
		// active one. Fixes the multi-action redis destiny, where a diagnostic
		// branch gated by `when: input.action=='diagnose'` is inactive at
		// action=update_acls — previously guardDestinyTask rejected its DSL with
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
			bt, bp, berr := p.renderDestinyBlock(ctx, destinyIn, task, idx, targeted, serialWidth, includeGroupKeep)
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
// the isolated destiny RenderInput (Register/ServiceVars empty,
// destinyIsolated=true → AllowHosts=false). Available: input.* (destiny
// input, not scenario), soulprint.self.*, incarnation.*; `register.*` and
// `soulprint.hosts` are isolation errors. The file layer sits on NOTHING
// (resolveVarLayer is called with a nil lower layer) and its accumulator starts
// empty, so a `vars.<other>` reference resolves only against a file-var of the
// SAME layer (var→var allowed, eager-topological) — never against the caller's
// service vars, and never against a task-var.
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
		resolved, err := resolveVarLayer(p.cel, raw, nil, base)
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
// then run the result through the destiny's FULL input contract (defense in
// depth, ADR-009): defaults, required/required_when, value validation
// (type/enum/pattern/format/length) and the `validate:` invariants.
//
// apply.input is host-invariant in the pilot: values are computed once (on
// the first targeted host), same as module-task params (host variance is
// out of pilot scope).
//
// The applier's task-level `vars:` are resolved into that env first, exactly
// as the module path does it (dispatch.go, renderModuleTask) — NIM-336. This
// is the one applier key that CAN be answered on this side of the boundary:
// `apply.input` renders in the SCENARIO env, where `vars.<name>` means what
// its author meant, and only the resulting VALUES cross into the destiny — the
// same thing every other apply.input value does. Isolation is untouched; the
// destiny still sees no `vars.*` of its caller, only its own vars.yml.
//
// ★ Two entrances, one fix. The applier's own `vars:` is the visible one; the
// second is a `block:` above it, whose vars mergeBlockInheritance merges into
// every descendant INCLUDING an apply: one. That second path is invisible to
// the offline validator — the key is written on the block, where §6.5 allows
// it, and the descendant carries no key of its own — so refusing it there was
// never an option.
//
// ★ The loss was LOUD, not silent: the scenario pass has no file-vars base, so
// `${ vars.x }` in apply.input failed with "no such key" for every applier.
// What was missing is a capability the DSL documents — a block passes `vars:`
// to its descendants (destiny/tasks.md §6.5) — and did not deliver to one kind
// of descendant, while every module sibling in the same block got it.
//
// fileVarsForHost is the base layer (Variant A, vars.md): empty on the scenario
// pass, and the destiny's own vars.yml if an applier is ever rendered inside a
// destiny pass — so the base is always the right one for wherever this applier
// physically sits.
func (p *Pipeline) resolveApplyInput(
	parentIn RenderInput,
	applier config.Task,
	resolved *ResolvedDestiny,
	targeted []*topology.HostFacts,
) (map[string]any, error) {
	apply := applier.Apply
	var host *topology.HostFacts
	if len(targeted) > 0 {
		host = targeted[0]
	} else {
		host = &topology.HostFacts{}
	}
	vars := hostVars(parentIn, host, len(targeted))
	vars, err := resolveTaskVars(p.cel, fileVarsForHost(parentIn, host), applier.Vars, vars)
	if err != nil {
		return nil, fmt.Errorf("render: apply destiny %q (task %q): %w", apply.Destiny, applier.Name, err)
	}

	rendered := make(map[string]any, len(apply.Input))
	for name, raw := range apply.Input {
		val, err := renderValue(p.cel, raw, vars, "apply.input."+name)
		if err != nil {
			return nil, fmt.Errorf("render: apply destiny %q input %q: %w", apply.Destiny, name, err)
		}
		rendered[name] = val
	}

	merged, err := config.ResolveInputContract(resolved.Input, resolved.Validate, rendered)
	if err != nil {
		return nil, destinyInputError(apply.Destiny, err)
	}
	return merged, nil
}

// destinyInputError classifies a destiny input-contract failure into the render
// layer's sentinels, keeping the underlying message (which names the offending
// field or carries the failing rule's `message:`) verbatim — an operator must
// not have to guess which input broke the contract.
func destinyInputError(destiny string, err error) error {
	var fail *config.ValidateRuleFailure
	switch {
	case errors.As(err, &fail):
		return fmt.Errorf("%w: destiny %q: %w", ErrDestinyValidateFailed, destiny, err)
	case errors.Is(err, config.ErrValidateRuleEval):
		return fmt.Errorf("render: apply destiny %q: %w", destiny, err)
	default:
		return fmt.Errorf("%w: destiny %q: %w", ErrDestinyInputInvalid, destiny, err)
	}
}

// guardDestinyTask rejects nested DSL constructs outside pilot scope
// (a nested apply:) and scenario-only keys on a destiny task
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
//     (where/serial/run_once/on/async/loop/include/apply) on the block or
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
	includeGroups includeGroupCache,
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
		return p.renderDestinyBlock(ctx, destinyIn, child, idx, childTargeted, width, includeGroups)
	}
	return p.walkBlockChildren(ctx, destinyIn, blockTask, startIndex, width, includeGroups, guardDestinyBlockChild, childTarget, childRecurse)
}

// guardDestinyBlockChild is the key boundary for a destiny block (render
// layer — the config layer is shared between both layers and block keys are
// valid there). Rejects scenario orchestration on a destiny block child with
// an explicit [ErrUnsupportedDSL]:
//
//	where / serial / run_once / on / async / loop / apply
//
// — all meaningless in a destiny (no per-child roster resolve, no nested
// destiny). VALID (env-agnostic inheritance + flat core): when (AND-merge),
// name, vars, onchanges/onfail/require (union), nested block:; a child is
// module: or a nested block:.
//
// include: on a child is NOT a boundary — within-block include is supported and
// expands before render (config.ExpandIncludes). The case stays as
// defense-in-depth with [ErrUnexpandedInclude]: an include child reaching here
// means the expander was skipped or is broken.
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
	case child.Async:
		return fmt.Errorf("%w: async: on a destiny-block child %q (task[%d] %q) - async inside a block is a deferred slice (ADR-0075)", ErrUnsupportedDSL, blockName, idx, child.Name)
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
// async:/loop: on it are caught by neither guardDestinyTask (block
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
	case blockTask.Async:
		return fmt.Errorf("%w: async: on destiny-block %q - async on a block is a deferred slice (ADR-0075)", ErrUnsupportedDSL, blockTask.Name)
	case blockTask.Loop != nil:
		return fmt.Errorf("%w: loop: on destiny-block %q - outside destiny block scope", ErrUnsupportedDSL, blockTask.Name)
	}
	return nil
}
