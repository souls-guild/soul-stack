package render

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
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
	// unvalidated (vars are untyped). CEL expressions `${ … }` in values, at any
	// depth, are evaluated inside the destiny pass when a cell reads them, over
	// input+soulprint.self+incarnation, isolated from scenario scope
	// ([Pipeline.destinyBaseViews]). They are the base `vars.*` rung; task-level
	// `vars:` overrides same-named keys on top (Option A, vars.md
	// "file-vars/task-vars merge"). nil means the destiny has no locals.
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

// applierRegisterModule is the module of the terminal task that folds an applier's
// register out of its destiny tasks (renderApplyDestiny).
const applierRegisterModule = "core.noop.run"

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
// run_once: resolve). The destiny inherits this roster, narrowed per task by
// a top-level destiny task's own on:/where: (resolveTargets below); a block in a
// destiny refuses on:/where: on itself and its children.
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
//
// Each child without a `timeout:` of its own takes the applier's effective
// `task_timeout` (settings.md §3 rule 6), through [mergeApplierInheritance]. The
// applier's own `timeout:` is refused at parse, as is a block's, so what reaches
// here is the `settings:` chain's.
func (p *Pipeline) renderApplyDestiny(
	ctx context.Context,
	parentIn RenderInput,
	applier config.Task,
	startIndex int,
	targeted []*topology.HostFacts,
	serialWidth int,
) (_ []*RenderedTask, _ []DispatchPlan, err error) {
	// Set once the pass exists: from there an error is the destiny's, withheld by
	// the destiny's own secret inputs and addressed by the destiny task last set.
	var (
		pass       *RenderInput
		failing    config.Task
		failingIdx int
		inTask     bool
	)
	defer func() {
		if err == nil || pass == nil {
			return
		}
		if inTask {
			err = p.taskFailure(*pass, failing, frameIndex(failing, failingIdx), err)
		}
		err = p.passFailure(*pass, err)
	}()

	applier.Timeout = wireTimeout(parentIn, applier)
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
	//
	// PER HOST since NIM-908: inputByHost is what a host-bound context reads,
	// destinyInput carries only the names that are the same for everyone.
	variantInputs := p.hostVariantInputNames(parentIn, applier)
	// A unit nothing was routed to lays out its destiny over the host-invariant
	// inputs alone: the per-host ones are left unrendered, and every contract rule
	// that reads one is skipped (ADR-0091 amendment NIM-993, rule 1).
	layout := parentIn.layoutOnly
	inputApplier := applier
	var contract []config.InputContractOption
	if layout {
		variantVars := hostVariantVarNames(p.cel, p.taskVarLayers(parentIn, applier), keeperRegisterNames(parentIn.Scenario), nil)
		inputApplier = hostInvariantApplier(applier, variantVars, variantInputs)
		contract = append(contract, config.WithoutInputs(variantInputs))
	}
	inputByHost, destinyInput, extraVariant, err := p.resolveApplyInput(parentIn, inputApplier, resolved, targeted, variantInputs, contract...)
	if err != nil {
		// A destiny `validate:` rule reads the destiny's inputs, so the destiny's
		// schema says which of them are secret; the apply.input expressions above
		// it are the scenario's, and the scenario level decides those.
		var ruleErr *config.ValidateRuleError
		if errors.As(err, &ruleErr) {
			err = p.passFailure(RenderInput{
				Scenario:          &config.ScenarioManifest{Name: resolved.Name, Input: resolved.Input},
				SecretStateFields: parentIn.SecretStateFields,
				destinyIsolated:   true,
			}, err)
		}
		return nil, nil, err
	}
	// A name the TEXT read as invariant whose VALUES turned out to differ (a
	// `required_when:`/default keyed on a per-host input). Merged so the projection
	// and the compile-time guard agree on one set: a name missing from Input must be
	// refused BY NAME in a host-free context, not answered with a no-such-key.
	if len(extraVariant) > 0 {
		variantInputs = append(append([]string(nil), variantInputs...), extraVariant...)
		sort.Strings(variantInputs)
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
		Scenario: &config.ScenarioManifest{Name: resolved.Name, Tasks: resolved.Tasks, Input: resolved.Input},
		// Input holds the HOST-INVARIANT names only; InputByHost holds every name,
		// per SID. Which one a context gets is [inputForHost]'s decision and it turns
		// on whether that context binds a host — the split is the fix, not a cache.
		Input:             destinyInput,
		InputByHost:       inputByHost,
		hostVariantInputs: variantInputs,
		Incarnation:       parentIn.Incarnation,
		Hosts:             targeted,
		Templates:         resolved.Templates, // .tmpl from THIS destiny's own snapshot
		Ctx:               ctx,                // vault() in destiny params: cancel/timeout for ReadKV
		destinyIsolated:   true,
		// Modules is carried over, unlike the scenario scope below: it is not
		// scope, it is the plugin-manifest resolver that says which of a module's
		// `output:` fields are declared secret ([ADR-0083] §8). A destiny task
		// runs the same modules as a scenario task, so dropping it would leave
		// §8 redaction dead inside a destiny — a module output masked on the
		// scenario path and printed in full on the destiny path.
		// Guarded by TestRender_ApplyDestiny_SecretOutputStillDerived.
		Modules: parentIn.Modules,
		// ServiceVars stays nil ON PURPOSE, and it is load-bearing rather than an
		// omission: a destiny task's view stacks on its host's `vars.yml` view, and
		// a cell with no task view of its own — the loop axis, a template path —
		// would read this one. Copying parentIn's value — the shape a
		// `destinyIn := parentIn` refactor would produce — opens the whole service
		// layer to every destiny cell. Guarded by
		// TestRender_ApplyDestiny_ServiceVarsNotLeaked.
		ServiceVars: nil,
		StrictVars:  parentIn.StrictVars,
		// Carried over although State is NOT (a destiny reads no
		// `incarnation.state` — the key is absent from its activation, so the
		// address set is dead here and costs a map of names). It is here so that
		// forwarding State one day is one edit and not two: a seal that has to be
		// remembered separately from the value it covers is the shape NIM-811 and
		// NIM-826 both had.
		SecretStateFields: parentIn.SecretStateFields,
		// seal (ADR-010 §7.4): same run-wide accumulator — destiny params with
		// `${ vault(...) }` get marked sealed just like scenario ones, and since
		// NIM-812 so does `${ input.<secret> }`, off the destiny's own schema
		// carried on Scenario above. The accumulator is shared with the parent
		// deliberately: its paths are matched against each task's own params root.
		Sealed:     parentIn.Sealed,
		sealMemo:   parentIn.sealMemo,
		units:      destinyUnits(parentIn, resolved.Tasks),
		layoutOnly: layout,
	}
	pass = &destinyIn

	// destiny locals from vars.yml (Option A, vars.md): one lazy view per host for
	// the pass, evaluated in the destiny env (destiny input + soulprint.self +
	// incarnation) and isolated from scenario scope — `register.*` and
	// `soulprint.hosts` in a vars.yml value are isolation errors. A definition is
	// evaluated when a cell reads it, and a destiny task's own `vars:` stack on it
	// (ADR-0082 amendment 2026-10-15, point 5). Laid out, the per-host ones stay in the
	// view and are never read: an include `when:` refuses them by name, and a laid-out
	// flow_context leaves them out ([RenderInput.leftOut]).
	destinyIn.destinyVars = p.destinyBaseViews(destinyIn, resolved.Vars, targeted)
	// Which of those locals actually resolve per host — the mirror of
	// hostVariantInputs one layer down, read from the raw vars.yml text, so the
	// two travel together into every host-free context of this pass. A context
	// deciding a task reads its view's set instead ([Pipeline.hostVariantTaskVars]).
	destinyIn.hostVariantVars = hostVariantDestinyVars(p.cel, p.taskVarLayers(destinyIn, config.Task{}), variantInputs)

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
		failing, failingIdx, inTask = task, idx, true
		taskStart, path := len(tasks), config.StepPath("", i)

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

		// Laid out, a module task stands where it stands whatever its `when:`; only a
		// `loop:`'s static `when:` decides how many places it takes. The guard still
		// holds, save for a task whose static `when:` gates it off, and a static
		// `when:` over a per-host name is refused by name, as routed.
		if layout && task.Block == nil && task.Loop == nil {
			if gerr := p.layoutGuard(destinyIn, task, guardDestinyTask(task, i, resolved.Name)); gerr != nil {
				return nil, nil, gerr
			}
			tasks = append(tasks, unitSkipPlaceholder(task, idx, 0, destinyIn.Modules))
			plans = append(plans, DispatchPlan{TaskIndex: idx, SerialWidth: serialWidth})
			idx++
			stampRescueUnit(destinyIn, tasks, taskStart, path)
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
		// pass. Laid out, a `loop:`'s is decided host-free, the guard's refusal standing
		// unless that gates it off.
		var skipped bool
		var serr error
		if layout {
			skipped, serr = p.layoutStaticWhen(ctx, destinyIn, task, destinyIn.Hosts, guardDestinyTask(task, i, resolved.Name), &tasks, &plans, &idx)
		} else {
			skipped, serr = p.emitStaticWhenSkip(ctx, destinyIn, task, destinyIn.Hosts, &tasks, &plans, &idx)
		}
		if serr != nil {
			return nil, nil, serr
		} else if skipped {
			stampRescueUnit(destinyIn, tasks, taskStart, path)
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
			bin := destinyIn
			bin.path = path
			bt, bp, berr := p.renderDestinyBlock(ctx, bin, task, idx, targeted, serialWidth, includeGroupKeep)
			if berr != nil {
				return nil, nil, berr
			}
			tasks = append(tasks, bt...)
			plans = append(plans, bp...)
			idx += len(bt)
			continue
		}

		var destinyTargeted []*topology.HostFacts
		if !layout {
			var terr error
			if destinyTargeted, terr = p.resolveTargets(destinyIn, task); terr != nil {
				return nil, nil, terr
			}
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
			stampRescueUnit(destinyIn, tasks, taskStart, path)
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
		stampRescueUnit(destinyIn, tasks, taskStart, path)
	}
	inTask = false
	withSkipTargets(tasks, plans, parentIn.unitSkip)

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
			Module:      applierRegisterModule,
			Register:    applierRegister,
			AggregateOf: aggregateOf,
		})
		// A live host the unit is skipped on is sent the terminal: the Soul folds
		// the register from children it was not sent.
		plans = append(plans, DispatchPlan{
			TaskIndex:   idx,
			TargetSIDs:  sidsOf(mergeHosts(targeted, parentIn.unitSkip)),
			SerialWidth: serialWidth,
		})
		idx++
	}

	return tasks, plans, nil
}

// hostInvariantApplier is applier with only what a host-free render of its
// `apply: input:` can read: the inputs outside variantInputs and the `vars:` outside
// variantVars.
func hostInvariantApplier(applier config.Task, variantVars map[string]bool, variantInputs []string) config.Task {
	apply := *applier.Apply
	apply.Input = withoutNames(apply.Input, variantInputs)
	applier.Vars = withoutNames(applier.Vars, slices.Collect(maps.Keys(variantVars)))
	applier.Apply = &apply
	return applier
}

// withoutNames is m without the keys in names; m itself when none is in it.
func withoutNames(m map[string]any, names []string) map[string]any {
	drop := false
	for _, name := range names {
		if _, ok := m[name]; ok {
			drop = true
			break
		}
	}
	if !drop {
		return m
	}
	out := make(map[string]any, len(m))
	for name, val := range m {
		if !slices.Contains(names, name) {
			out[name] = val
		}
	}
	return out
}

// resolveApplyInput computes the destiny input from apply.input, PER HOST.
//
// apply.input is literals/CEL in scenario env (the parent decides what to
// pass to the destiny). We resolve it in the parent's context
// (input/incarnation/soulprint.self/register of each targeted host, or empty),
// then run each host's result through the destiny's FULL input contract (defense
// in depth, ADR-009): defaults, required/required_when, value validation
// (type/enum/pattern/format/length) and the `validate:` invariants.
//
// ★ Per host since NIM-908, and it used to be once on `targeted[0]`. The context
// this renders in offers `soulprint.self.*` and `register.*` — per-host roots, not
// refused — so a single render meant nine hosts received the first host's address,
// silently and green. The two returns are how the fix stays honest past the render:
//
//   - byHost (sid → values) is what every host-bound context reads, through
//     [inputForHost]: a destiny's vars.yml, a task's params:/where:, and the
//     render_context of `core.file.rendered`, which is the one channel that
//     dispatches per host (RenderContextBySID);
//   - invariant carries ONLY the names that are the same for every host, and is
//     what a context with no host gets. A per-host name is absent from it and
//     refused by name there ([cel.HostScope]) rather than answered from host zero.
//
// ★ And it reaches an ordinary module param too, because the same change dispatches
// params per host ([RenderedTask.ParamsBySID], selected by [ToProtoTasksForHost]) —
// open Q #25 closed for params. What still refuses on a multi-host target is FLOW
// CONTROL, which Soul evaluates against one snapshot per task, and any context with
// no roster at all ([cel.HostScope]).
//
// The contract runs per host rather than once on the merged result: `required_when`
// and `validate:` are predicates OVER the input, so a rule that holds for one host's
// values can fail for another's, and checking a single host would let the other
// eight through unvalidated. Errors name the host.
//
// Cost is N renders of apply.input instead of one, and it is re-measurable rather
// than quoted: [BenchmarkApplyInput_ResolveByRoster] is this call with the roster as
// its axis. On 2026-09-28, four inputs: ~0.17 ms at one host against ~0.69 ms at nine,
// i.e. ~0.065 ms per extra host. `vault()` does NOT scale with it — the resolution memo
// ([WithVaultFence]) spans at least the whole Render this loop runs inside, so a sealed
// input is one ReadKV whether the roster is one host or nine, which
// TestApplyInput_VaultReadsDoNotScaleWithTheRoster asserts and a mutation kills.
//
// The applier's task-level `vars:` are read through its task view in that env,
// exactly as the module path does it (renderTaskIter) — NIM-336. This
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
// ★ The loss was LOUD, not silent: `${ vars.x }` in apply.input failed with "no
// such key" for every applier whose `vars:` did not reach it. What was missing is
// a capability the DSL documents — a block passes `vars:` to its descendants
// (destiny/tasks.md §6.5) — and did not deliver to one kind of descendant, while
// every module sibling in the same block got it.
func (p *Pipeline) resolveApplyInput(
	parentIn RenderInput,
	applier config.Task,
	resolved *ResolvedDestiny,
	targeted []*topology.HostFacts,
	variantInputs []string,
	contract ...config.InputContractOption,
) (byHost map[string]map[string]any, invariant map[string]any, extraVariant []string, err error) {
	apply := applier.Apply
	hosts := targeted
	if len(hosts) == 0 {
		// where: filtered everyone out — one synthetic host under key "", the same
		// shape destinyBaseViews uses. The destiny still renders (its tasks land in
		// the plan with an empty DispatchPlan), so its input still has to exist.
		hosts = []*topology.HostFacts{{}}
	}

	byHost = make(map[string]map[string]any, len(hosts))
	// Strict mode's views: the applier is a host task, materialized per targeted host
	// as renderTaskIter does it.
	var strictViews []*cel.VarView
	for _, host := range hosts {
		vars, verr := p.withTaskView(parentIn, applier, host, hostVars(parentIn, host, len(targeted)))
		if verr != nil {
			return nil, nil, nil, atHost(host.SID, fmt.Errorf("render: apply destiny %q (task %q, host %s): %w", apply.Destiny, applier.Name, host.SID, verr))
		}
		if parentIn.StrictVars && len(targeted) > 0 {
			strictViews = append(strictViews, vars.View)
		}

		rendered := make(map[string]any, len(apply.Input))
		for _, name := range slices.Sorted(maps.Keys(apply.Input)) {
			val, rerr := renderValue(p.cel, apply.Input[name], vars, "apply.input."+name)
			if rerr != nil {
				return nil, nil, nil, atHost(host.SID, fmt.Errorf("render: apply destiny %q input %q (host %s): %w", apply.Destiny, name, host.SID, rerr))
			}
			rendered[name] = val
		}

		merged, cerr := config.ResolveInputContract(resolved.Input, resolved.Validate, rendered,
			config.OutOfScopeIncarnation(config.IncarnationOutOfScopeDestiny), contract...)
		if cerr != nil {
			return nil, nil, nil, atHost(host.SID, destinyInputError(apply.Destiny, host.SID, cerr))
		}
		byHost[host.SID] = merged
	}
	for i, view := range strictViews {
		if err := materialize(view); err != nil {
			return nil, nil, nil, atHost(hosts[i].SID, fmt.Errorf("render: apply destiny %q (task %q, host %s): %w", apply.Destiny, applier.Name, hosts[i].SID, err))
		}
	}

	invariant, extra := hostInvariantInput(byHost, variantInputs)
	return byHost, invariant, extra, nil
}

// hostVariantInputNames is the classification NIM-908 turns on: which `apply: input:`
// entries render differently per host. Sorted, deduplicated.
//
// Read off the RAW EXPRESSION TEXT, never off what the values turned out to be. A
// value-derived set would depend on the roster — nine hosts that happen to share an
// address would classify as invariant and the tenth would flip the whole scenario
// from accepted to refused, with nothing in the file to explain it. Same principle
// as taskFlowContextReads, and the same reason: a rule an author cannot read off
// their own file is a rule they cannot follow.
//
// A name is per-host iff its value REACHES a per-host root — directly, or through
// a name of the applier's view of `vars`, which [hostVariantVarNames] closes
// transitively. The indirect route is the one that is easy to leave out and was: see
// that function.
//
// `soulprint.self` is per-host by definition. `register` is per-host by
// construction ([hostRegister] keys the run's buckets by SID) — per NAME, not a name-by-name verdict, even though the KEEPER bucket is unioned into
// it and is genuinely the same for everyone. Deliberate over-approximation: which
// bucket a name comes from depends on what has been registered by the time this
// runs, so no reading of the text can decide it, and the host's own bucket wins on
// a collision. The cost is narrow — a keeper-register read in apply.input is still
// available to every context that binds a host, and only a host-FREE reference to
// it is refused.
//
// `soulprint.hosts` is deliberately not counted: it is the run's roster, identical
// for every host.
func (p *Pipeline) hostVariantInputNames(in RenderInput, applier config.Task) []string {
	keeperRegisters := keeperRegisterNames(in.Scenario)
	variantVars := hostVariantVarNames(p.cel, p.taskVarLayers(in, applier), keeperRegisters, nil)
	var names []string
	for name, val := range applier.Apply.Input {
		if readsHostRoots(rawReads(p.cel, val, hostVariantRoots), variantVars, keeperRegisters, nil) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// keeperRegisterNames is the set of register names a KEEPER-side task of this
// scenario produces. A keeper task runs once per run with no roster
// ([keeperVars]), so its register carries one value that every host reads
// identically — it is in the per-host `register` root only because [hostRegister]
// unions the keeper bucket into it ([ADR-0083] §5).
//
// ★ Without this the classification is too coarse to ship. Measured on
// `wb/service/redis` (2026-09-29): its `apply: input:` builds `config` and `users`
// from `register.system_acl_users.effective`, written by a `core.state.present`
// step — keeper-side, identical everywhere — and the redis destiny gates a task on
// `when: … has(input.config.unixsocket)`, a static predicate. Treating the whole
// `register` root as per-host classified `config` as host-variant and turned that
// working `when:` into a refusal. The price of "fail immediately" has to be zero on
// a correct service, and the whole-root rule made it one.
//
// Static and roster-independent: the answer comes from the scenario's own task
// list, not from what has been registered by the time the applier runs. Recurses
// through `block:`, whose children are ordinary tasks with their own `register:`.
// A name this does not find is absent from the set and therefore treated as
// per-host — fail-closed, which is the right direction for a name whose producer
// the manifest does not show.
func keeperRegisterNames(scn *config.ScenarioManifest) map[string]bool {
	if scn == nil {
		return nil
	}
	out := map[string]bool{}
	var walk func(tasks []config.Task)
	walk = func(tasks []config.Task) {
		for i := range tasks {
			t := &tasks[i]
			if t.Register != "" && config.IsKeeperSideTask(*t) {
				out[t.Register] = true
			}
			if t.Block != nil {
				walk(t.Block.Block)
			}
		}
	}
	walk(scn.Tasks)
	return out
}

// hostVariantVarNames closes a view of `vars` over the per-host roots: which of its
// names carry a per-host value, directly or through another name ([viewClosure]).
//
// ★ Without this the fix reproduces the defect it fixes, one hop over. An applier
// writing
//
//	vars: {addr: "${ soulprint.self.network.primary_ip }"}
//	apply: {input: {master_addr: "${ vars.addr }"}}
//
// has a genuinely per-host `master_addr` — the applier's task view evaluates that
// layer per host in resolveApplyInput — while the input's own text names no per-host root, so a
// scan of apply.input alone reads it as invariant and hands the first host's address to
// every host-free context. Measured on this tree before the closure existed: two hosts,
// one `loop.items: ${ input.master_addr }`, `echo 10.0.0.1` dispatched to both, green.
//
// The block: a `block:` above the applier merges its own `vars:` down
// (mergeBlockInheritance), and that happens before renderApplyDestiny is called, so
// applier.Vars already carries them and they close here with the rest.
//
// ★ The applier's WHOLE view is closed, its service and scenario rungs included
// ([ADR-0082] amendment 2026-10-15, point 16). Neither rung's text can name a per-host
// root (point 6), but under late binding a definition there reads the applier's
// override: a service `x: ${ vars.y }` under the applier's `y: ${ soulprint.self… }`
// is per-host in that view. A rung no override reaches stays invariant, so the
// service vars `apply: input:` exists to forward are not misclassified.
func hostVariantVarNames(engine *cel.Engine, layers []cel.VarLayer, keeperRegisters, variantInputs map[string]bool) map[string]bool {
	return viewClosure(engine, layers, hostVariantRoots, func(reads map[string]cel.RootReads, closed map[string]bool) bool {
		return readsHostRoots(reads, closed, keeperRegisters, variantInputs)
	})
}

// readsHostRoots reports whether reads — of an apply.input value or a definition, every
// string of it at any depth ([rawReads]) — reach a per-host root. At any depth because
// `input:` values are not flat: a list of addresses or a map of per-host overrides
// carries its expressions one level down, and a scan that stopped at the top would
// classify exactly the structured cases as invariant.
func readsHostRoots(reads map[string]cel.RootReads, variantVars, keeperRegisters, variantInputs map[string]bool) bool {
	// soulprint: only `self` counts. `soulprint.hosts` is the run's roster — the same
	// list on every host — and counting it would classify the standard topology
	// expression (`soulprint.hosts.where(...)`) as per-host and refuse it in exactly
	// the contexts it was designed for.
	sp := reads[soulprintRootName]
	if sp.Whole || sp.Fields[soulprintSelfFieldName] {
		return true
	}
	// register: per NAME. A keeper-side register is the same value on every host
	// ([keeperRegisterNames]); every other name is the host's own bucket, and so is
	// `register.self`. A whole-root read (`size(register)`, `register[k]`) names
	// nothing the walk can check — fail-closed.
	reg := reads[registerRootName]
	if reg.Whole {
		return true
	}
	for name := range reg.Fields {
		if !keeperRegisters[name] {
			return true
		}
	}
	// vars: only the names already closed as per-host (hostVariantVarNames). The root
	// as a whole is NOT per-host — a name no per-host override reaches is invariant —
	// so counting it would misclassify every input forwarding a service var. A
	// whole-root read (`size(vars)`, `vars[k]`) is fail-closed only when there IS a
	// per-host name to reach.
	//
	// The loop goes over the names read, never over the closed set: viewClosure calls
	// this for every definition on every pass while that set grows.
	if readsAny(reads[varsRootName], variantVars) {
		return true
	}
	// input: only in the DESTINY pass, where variantInputs is the set of names
	// `apply: input:` renders per host. On the applier side it is empty — the
	// scenario's own `input:` is the operator's run input and has no host axis.
	return readsAny(reads[inputRootName], variantInputs)
}

const (
	soulprintRootName      = "soulprint"
	soulprintSelfFieldName = "self"
	registerRootName       = "register"
	varsRootName           = "vars"
	inputRootName          = "input"
)

// hostVariantRoots are the CEL roots this classification reads. The first two are
// per-host by construction; `vars` is per-NAME and is judged against the closure
// [hostVariantVarNames] builds, because a name no per-host override reaches is
// host-invariant.
var hostVariantRoots = []string{soulprintRootName, registerRootName, varsRootName, inputRootName}

// hostVariantDestinyVars closes a view of a DESTINY pass over the per-host roots it
// can see: `soulprint.self` (available in the destiny pass), a per-host
// `input.<name>` from [Pipeline.hostVariantInputNames], and another per-host name.
// Sorted.
//
// ★ The MIRROR of the applier-vars hop, one layer down, and it was open until it was
// measured. A destiny writing
//
//	vars.yml:  addr: "${ input.master_addr }"
//	tasks:     - when: "vars.addr == '10.0.0.2'"
//
// over a per-host `master_addr` decides that `when:` on the roster's FIRST host and
// applies the verdict to everyone. Measured on this tree: true on the second host,
// false on the first, task skipped for BOTH — silently, green. The reference that
// reaches the decision says only `vars.addr`, which is why nothing downstream could
// see it.
func hostVariantDestinyVars(engine *cel.Engine, layers []cel.VarLayer, variantInputs []string) []string {
	if len(layers) == 0 {
		return nil
	}
	inputs := make(map[string]bool, len(variantInputs))
	for _, name := range variantInputs {
		inputs[name] = true
	}
	set := hostVariantVarNames(engine, layers, nil, inputs)
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// hostVariantTaskVars is the per-host `vars.*` set of task's view, for a context that
// decides the task on one host for the whole roster — a static `when:`, an `assert:`
// gate. In a destiny pass that view is the pass's `vars.yml` with the task's own block
// + task rung on it, closed together (point 16), so a task var over `soulprint.self`
// and a `vars.yml` name derived from one are both refused there by name ([ADR-0082]
// amendment 2026-10-15, point 7). A context with no task rung — an include `when:`,
// the loop axis — reads the pass's own set, [RenderInput.hostVariantVars].
//
// On a scenario pass it is that set too, empty: a scenario static `when:` over a
// host-dependent task var is NIM-1020, not this.
func (p *Pipeline) hostVariantTaskVars(in RenderInput, task config.Task) []string {
	if !in.destinyIsolated || len(task.Vars) == 0 {
		return in.hostVariantVars
	}
	return hostVariantDestinyVars(p.cel, p.taskVarLayers(in, task), in.hostVariantInputs)
}

// hostInvariantInput is what a context with no host gets: the entries whose name is
// not in variant, taken from any one host (they are equal on all of them by
// construction — that is what "not variant" means).
//
// A per-host name is OMITTED rather than filled from host zero. That is the whole
// difference between this and the defect: a missing key is a refusal one layer up
// ([cel.HostScope], which names it) or an ordinary no-such-key, and either beats a
// value that is real and belongs to somebody else.
func hostInvariantInput(byHost map[string]map[string]any, variant []string) (map[string]any, []string) {
	variantSet := make(map[string]bool, len(variant))
	for _, name := range variant {
		variantSet[name] = true
	}
	sids := sortedSIDs(byHost)
	if len(sids) == 0 {
		return map[string]any{}, nil
	}
	first := byHost[sids[0]]

	// ★ The equality is CHECKED, not assumed. "Not variant" is a property of the
	// apply.input TEXT, and the values here have been through
	// config.ResolveInputContract since: a `required_when:` or a default keyed on a
	// per-host input can make a textually-invariant name differ between hosts after
	// all. Without this check that name would sit in the invariant projection with
	// the first host's value and be readable, unrefused, from every host-free
	// context — the defect shape, arrived at by a route the text classification
	// cannot see.
	//
	// Values may only ADD to the variant set, never remove from it. The text decides
	// what is per-host; this is a safety net under it, and a net that could subtract
	// would put the classification back at the mercy of which hosts a run happened
	// to have.
	var extra []string
	out := map[string]any{}
	for name, val := range first {
		if variantSet[name] {
			continue
		}
		if !equalAcrossHosts(byHost, sids, name, val) {
			extra = append(extra, name)
			continue
		}
		out[name] = val
	}
	sort.Strings(extra)
	return out, extra
}

// equalAcrossHosts reports whether one input name holds the same value on every
// host. Compared through the YAML/JSON shapes the render produces (maps, slices,
// scalars), so reflect.DeepEqual is the right instrument — these are plain Go data
// out of CEL, not proto messages.
func equalAcrossHosts(byHost map[string]map[string]any, sids []string, name string, want any) bool {
	for _, sid := range sids[1:] {
		if !reflect.DeepEqual(byHost[sid][name], want) {
			return false
		}
	}
	return true
}

func sortedSIDs(byHost map[string]map[string]any) []string {
	sids := make([]string, 0, len(byHost))
	for sid := range byHost {
		sids = append(sids, sid)
	}
	sort.Strings(sids)
	return sids
}

// destinyInputError classifies a destiny input-contract failure into the render
// layer's sentinels, keeping the underlying message (which names the offending
// field or carries the failing rule's `message:`) verbatim — an operator must
// not have to guess which input broke the contract.
//
// sid names the host whose values failed. The contract runs per host since NIM-908,
// and `required_when`/`validate:` are predicates over the values, so the same
// scenario can pass on eight hosts and fail on the ninth — without the SID that
// reads as an intermittent failure of the whole run. Empty for the synthetic host
// of an empty roster, where there is no host to name.
func destinyInputError(destiny, sid string, err error) error {
	where := fmt.Sprintf("destiny %q", destiny)
	if sid != "" {
		where = fmt.Sprintf("destiny %q (host %s)", destiny, sid)
	}
	var fail *config.ValidateRuleFailure
	switch {
	case errors.As(err, &fail):
		return fmt.Errorf("%w: %s: %w", ErrDestinyValidateFailed, where, err)
	case errors.Is(err, config.ErrValidateRuleEval):
		return fmt.Errorf("render: apply %s: %w", where, err)
	default:
		return fmt.Errorf("%w: %s: %w", ErrDestinyInputInvalid, where, err)
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
	case task.Transport != nil:
		// A destiny is rendered per host and shipped whole to ONE transport that
		// the scenario task chose; a destiny task naming a second one has nothing
		// to act on. Refused rather than ignored — the whole point of NIM-870 is
		// that the key is not decorative, and a silently dropped `transport: ssh`
		// is a file that says one thing while the run dials another.
		return fmt.Errorf("%w: transport: in destiny %q (task[%d] %q) - the transport is chosen by the scenario task that applies this destiny", ErrUnsupportedDSL, destiny, idx, task.Name)
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
	childTarget := func(_ RenderInput, _ int, _ config.Task) ([]*topology.HostFacts, error) {
		return targeted, nil
	}
	// nested block → recurse into the same destiny layer (cascading inheritance).
	childRecurse := func(in RenderInput, _ int, child config.Task, idx int, childTargeted []*topology.HostFacts) ([]*RenderedTask, []DispatchPlan, error) {
		return p.renderDestinyBlock(ctx, in, child, idx, childTargeted, width, includeGroups)
	}
	// No steps: a destiny is one step whole (ADR-0091, decision 6), so its
	// blocks render in the applier's pass at the applier's width under either model.
	return p.walkBlockChildren(ctx, destinyIn, blockTask, startIndex, width, includeGroups, guardDestinyBlockChild, childTarget, childRecurse, nil)
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
	case child.Transport != nil:
		return fmt.Errorf("%w: transport: on a destiny-block child %q (task[%d] %q) - the transport is chosen by the scenario task that applies this destiny", ErrUnsupportedDSL, blockName, idx, child.Name)
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
