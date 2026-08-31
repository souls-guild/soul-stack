package render

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/tmpl"
)

// tracer for the render-pipeline in-process span (ADR-024 §4). Uses the
// global TracerProvider set up by [obs.SetupOTel] in cmd/keeper; when OTel is
// disabled the provider is no-op — span is free, no branching needed.
var tracer = otel.Tracer("keeper/render")

// KVReader is the narrow subset of keeper/internal/vault.Client needed by
// the pipeline's vault-resolve phase (`vault:` refs in params). *vault.Client
// satisfies it as-is; the narrow interface lets the Trial runner ([ADR-023])
// run hermetically against a fixture-backed reader without a live Vault.
// Mirrors keeper/internal/coremod/vault.VaultReader.
type KVReader interface {
	ReadKV(ctx context.Context, path string) (map[string]any, error)
}

// Pipeline orchestrates the Keeper-side scenario render phases ([ADR-010]).
// Thread-safe: cel.Engine and KVReader hold their own internal locks/pools,
// Pipeline itself carries no mutable state.
type Pipeline struct {
	vault   KVReader
	cel     *cel.Engine
	logger  *slog.Logger
	metrics *RenderMetrics
}

// NewPipeline constructs a Pipeline. engine is required. vc may be nil (a
// scenario with no vault-refs makes vault-resolve a no-op; a ref against a
// nil reader errors during vault-resolve). logger may be nil (diagnostics
// suppressed). metrics may be nil (keeper_render_* metrics disabled — nil-safe
// [RenderMetrics] methods no-op; used by unit tests, dev builds, Trial).
//
// The destiny resolver (apply:destiny) is passed per-Render via
// [RenderInput.Destiny], not as a Pipeline field — Pipeline is immutable and
// shared across concurrent runs, while the resolver is per-run (carries a
// specific service snapshot's destiny[] refs). RenderInput.Destiny=nil →
// apply:destiny fails with [ErrUnsupportedDSL].
func NewPipeline(vc KVReader, engine *cel.Engine, logger *slog.Logger, metrics *RenderMetrics) *Pipeline {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Pipeline{vault: vc, cel: engine, logger: logger, metrics: metrics}
}

// Render runs a scenario through vault-resolve → CEL-render → `on:`/`where:`
// resolution and returns the flat list of rendered tasks plus the dispatch
// plan (task → hosts).
//
// Pilot DSL scope: module tasks (including `core.file.rendered`), `apply:
// destiny` (isolated render pass, V2 ADR-009), serial:/run_once: (slice D),
// loop: (E1) and block: (C1) — all expand via render-time fan-out into the
// flat layer — plus `on: keeper` (renderKeeperTask) and `async:` (ADR-0075,
// threaded into RenderedTask). Outside pilot scope: loop: on an apply: task and
// loop:/async: on a keeper-side task → [ErrUnsupportedDSL]; unexpanded include:
// → [ErrUnexpandedInclude].
//
// Index/TaskIndex is a cross-cutting index over the final plan: scenario
// tasks and spliced-in destiny tasks share one monotonic counter (links
// RenderedTask↔DispatchPlan↔TaskEvent.task_idx). Without apply:destiny the
// index matches the position in scenario.tasks[].
//
// CEL-rendered params are per-host (soulprint.self of the host). In pilot,
// params must be host-invariant: a task producing different params on
// different targeted hosts is host-dependent render, which the "one
// RenderedTask per task" contract can't express (per-host ApplyRequest is an
// orchestrator-layer concern) → error.
func (p *Pipeline) Render(ctx context.Context, in RenderInput) (_ []*RenderedTask, _ []DispatchPlan, err error) {
	if in.Scenario == nil {
		return nil, nil, fmt.Errorf("render: scenario manifest is nil")
	}

	// keeper_render_* metrics (ADR-024): full-pass duration + error counter,
	// observed in defer via named-return err (mirrors the span below) — one
	// measurement per pass. nil metrics → no-op. nil-scenario is rejected above
	// before the timer starts: that's a caller error, not "render ran".
	start := time.Now()
	defer func() { p.metrics.ObserveRender(time.Since(start), err) }()

	// In-process span for the render pipeline (vault-resolve → CEL →
	// on/where), child of scenario.run (ADR-024 §4): the heaviest Keeper-side
	// phase of a run, previously indistinguishable inside the scenario.run
	// span. incarnation/scenario name are domain identifiers for trace
	// filtering (forbidden in metric labels, §2.2); secrets (params/vault
	// values) are never put in attributes. Tracer is no-op when OTel is
	// disabled — Start/End are free.
	ctx, span := tracer.Start(ctx, "render.pipeline",
		trace.WithAttributes(
			attribute.String("incarnation", in.Incarnation.Name),
			attribute.String("scenario", in.Scenario.Name),
			attribute.Int("tasks", len(in.Scenario.Tasks)),
			attribute.Int("hosts", len(in.Hosts)),
		),
	)
	defer func() {
		if err != nil {
			span.SetStatus(codes.Error, "render_failed")
		}
		span.End()
	}()

	// per-render-pass vault() memo: repeated vault(same-path) calls in this
	// pass (per-host × operational redis ACL/sentinel scenarios — dozens of
	// identical reads) hit the cache instead of re-querying Vault. Scoped to
	// exactly this Render call (one incarnation): the cache lives in ctx, not
	// on Engine (which is shared across runs).
	ctx = WithVaultFence(ctx, in.Incarnation.Service)
	in.Ctx = ctx // propagate to CEL vault() (ReadKV cancel/timeout + memo + fence)

	// The own-namespace fence ([ADR-0083] §7), runtime half. The load-time half
	// (artifact.LoadScenarioManifestResolved, soul-lint) sees the MAIN FILE only —
	// an `include:` body is parsed inside config.ExpandIncludes, after that load
	// returns. in.Scenario.Tasks here is the expanded list, and every dispatch path
	// (run / preflight / render_host) passes through this function, so a path
	// smuggled in through an include is caught before a single Vault read happens.
	// A caller with no service identity (push, unit eval) scans nothing — the same
	// condition resolveRegisterSecrets applies, for the same reason. Trial is NOT
	// such a caller: since NIM-726 the L0 harness derives the name from the service
	// directory (`fixtures.service` overrides), and neither source can be empty, so
	// the fence is never switched off there. Whether it MATCHES is a separate question
	// the harness cannot answer offline — see trial.trialServiceName. The property to
	// re-check if this is ever revisited is non-emptiness, not a `name:` key.
	if fdiags := config.ScanOwnNamespaceVault(in.Scenario.Name, in.Incarnation.Service, in.Scenario, in.Scenario.Tasks); len(fdiags) > 0 {
		return nil, nil, fmt.Errorf("render: %s: %s at %s (%d in this scenario)",
			fdiags[0].Code, fdiags[0].Message, fdiags[0].YAMLPath, len(fdiags))
	}

	// A declared secret written by a keeper-side task travels in the register as a
	// `vault:` reference, not as a value ([ADR-0083] §6). It is resolved here, once
	// per pass and through the same memo as every other read, before any CEL root
	// is built from it.
	keeperReg, sealedReg, kerr := p.resolveRegisterSecrets(ctx, in)
	if kerr != nil {
		return nil, nil, kerr
	}
	in.KeeperRegister = keeperReg
	// Two seal sources, resolved provenance and declared provenance — see
	// secretOutputRegisters for why neither substitutes for the other.
	in.sealedRegisters = mergeSealedRegisters(sealedReg,
		secretOutputRegisters(in.Scenario.Tasks, in.Modules))

	// compute: resolved ONCE per run (run-level context, no soulprint — a
	// host-invariance barrier) before task rendering — the `compute.<name>`
	// result is visible in apply.input/where/params of every host via hostVars
	// (ADR-009).
	computed, cerr := p.resolveCompute(in)
	if cerr != nil {
		return nil, nil, cerr
	}
	in.Compute = computed

	tasks := make([]*RenderedTask, 0, len(in.Scenario.Tasks))
	plans := make([]DispatchPlan, 0, len(in.Scenario.Tasks))
	idx := 0

	// includeGroupKeep caches the conditional-include (group-drop) decision for
	// this pass — see [Pipeline.keepIncludeGroup]. Threaded into
	// renderBlockTask so a within-block include group is decided once for the
	// whole pass, at both nesting levels.
	includeGroupKeep := includeGroupCache{}

	// passageStart marks where the current top-level task's RenderedTask
	// output begins, so its whole output (including apply:destiny/loop
	// descendants) can be stamped with the originating task's passage index
	// (staged-render, ADR-056). Stamping happens once at the end of each
	// iteration (stampPassage) rather than at each branch — one pass covers
	// every expansion path automatically.
	for i := range in.Scenario.Tasks {
		task := in.Scenario.Tasks[i]

		passage := taskPassageAt(in.TaskPassage, i)
		passageStart := len(tasks)

		// Conditional-include group-drop (ADR-009 amendment) — before
		// emitStaticWhenSkip. A false include-when is a real drop: continue
		// without emitting a RenderedTask and without idx++ (index isn't
		// reserved, the task disappears from the plan entirely) — unlike
		// emitStaticWhenSkip's placeholder-with-idx++. Safe: cross-file register
		// of a dropped group is already lint-forbidden (per-file
		// validateTaskRefs), so an external onchanges can't reference it →
		// resolveOnChanges never hits ErrOnChangesUnknownRegister.
		if keep, kerr := p.keepIncludeGroup(in, task, includeGroupKeep); kerr != nil {
			return nil, nil, kerr
		} else if !keep {
			continue
		}

		// assert task (ADR-009 amendment 2026-06-23) — keeper-side render-time
		// precondition. Handled before emitStaticWhenSkip/guardPilotDSL: an
		// assert never emits a RenderedTask (it's a check, not a task), so
		// emitStaticWhenSkip must not emit a placeholder for it. evalAssertTask
		// itself honors the `when:` gate (static-when-false → assert not
		// evaluated) and returns ErrAssertFailed on predicate failure (render
		// aborts, idx doesn't advance). idx/tasks/plans are untouched — tasks
		// after the assert shift to its position.
		//
		// RUN-LEVEL "once": in staged-render, Render is called per-Passage with
		// a growing ActivePassage; the assert is evaluated only when its own
		// Passage is active (otherwise it'd repeat every Passage). Non-staged
		// (TaskPassage==nil: Trial/Acolyte) → passage is always 0 ==
		// ActivePassage 0 → single pass, bit-for-bit unchanged.
		if IsAssertTask(task) {
			if in.TaskPassage == nil || passage == in.ActivePassage {
				if err := p.evalAssertTask(in, task); err != nil {
					return nil, nil, err
				}
			}
			continue
		}

		// Static-when precedes guardPilotDSL (ADR-012(d), extending the
		// static-when invariant): a statically-false `when:` gates the task
		// off and skips it before any eager processing, including the DSL
		// guard. An inactive branch with unsupported DSL (`loop:` on an
		// `apply:` task) doesn't block the active one — its DSL is rejected
		// only on activation (per-action validation). Not masking a bug: the
		// task is physically never executed, so the guard is never reached.
		// isStaticWhen/staticWhenSkips are register-/soulprint-independent and
		// build flow_context from input/vars/incarnation/self, not DSL
		// fields, so calling them before the guard is safe.
		if skipped, serr := p.emitStaticWhenSkip(ctx, in, task, &tasks, &plans, &idx); serr != nil {
			return nil, nil, serr
		} else if skipped {
			stampPassage(tasks, passageStart, passage)
			continue
		}

		if err := guardPilotDSL(task, i); err != nil {
			return nil, nil, err
		}

		// Future passage (staged-render, ADR-056 §c.1): register isn't
		// collected yet, so register-dependent where:/params: aren't resolved
		// (they'd fail on an empty register — that's the drift this guards
		// against). Emit a placeholder to keep the index contiguous; the
		// orchestrator won't dispatch it in the active passage. Once its
		// passage becomes active, a repeat Render resolves it fully. Gated
		// strictly to staged mode (TaskPassage set) — non-staged callers never
		// reach this branch.
		if in.TaskPassage != nil && passage > in.ActivePassage {
			rt := &RenderedTask{Index: idx, Name: task.Name, Register: task.Register, ID: task.ID, Passage: passage}
			if task.Module != nil {
				rt.Module = task.Module.Module
			}
			tasks = append(tasks, rt)
			plans = append(plans, DispatchPlan{TaskIndex: idx})
			idx++
			continue
		}

		// keeper-side task (`on: keeper`, docs/keeper/modules.md): no hosts —
		// render params in the keeper context (no per-host soulprint) and emit
		// a single keeper target. Executes locally on the keeper instance
		// (scenario-runner), never dispatched to a Soul. apply:/loop: on a
		// keeper task aren't supported in pilot (guardPilotDSL lets apply
		// through, so check explicitly here).
		if IsKeeperTask(task) {
			rt, derr := p.renderKeeperTask(ctx, in, task, idx)
			if derr != nil {
				return nil, nil, derr
			}
			tasks = append(tasks, rt)
			plans = append(plans, DispatchPlan{
				TaskIndex:  idx,
				TargetSIDs: []string{KeeperTargetSID},
				Keeper:     true,
			})
			idx++
			stampPassage(tasks, passageStart, passage)
			continue
		}

		targeted, err := resolveTargets(p.cel, in, task)
		if err != nil {
			return nil, nil, err
		}

		// run_once: trims the target to one host (first by SID) before
		// rendering params and building the plan — orchestration.md §2.2.2.
		targeted = applyRunOnce(targeted, task.RunOnce)

		// apply: destiny — isolated destiny render pass (V2). Its tasks are
		// spliced into the overall plan with contiguous indices; one apply
		// task expands into N destiny tasks. The parent's run_once is already
		// applied to targeted; serial: on the apply task propagates to its
		// destiny tasks.
		if task.Apply != nil {
			width := serialWidth(task.Serial, len(targeted))
			dt, dp, derr := p.renderApplyDestiny(ctx, in, task, idx, targeted, width)
			if derr != nil {
				return nil, nil, derr
			}
			tasks = append(tasks, dt...)
			plans = append(plans, dp...)
			idx += len(dt)
			stampPassage(tasks, passageStart, passage)
			continue
		}

		// loop: on a module task (slice E1) — render-time fan-out: one task
		// expands into N RenderedTask entries over items, with contiguous
		// indices (mirrors apply:destiny). Loop expansion happens after target
		// resolution (on→where→run_once), within each targeted host; serial:
		// is inherited by every iteration (orthogonal axes, orchestration.md
		// §2.2).
		if task.Loop != nil {
			lt, lp, lerr := p.renderLoopTask(ctx, in, task, idx, targeted)
			if lerr != nil {
				return nil, nil, lerr
			}
			tasks = append(tasks, lt...)
			plans = append(plans, lp...)
			idx += len(lt)
			stampPassage(tasks, passageStart, passage)
			continue
		}

		// block: (pilot C1) — render-time fan-out into the flat RenderedTask
		// layer, like loop/apply:destiny. targeted is already resolved against
		// block.on/block.where + run_once (above) — descendants inherit
		// on/where/run_once for free. width from block.serial is handed to
		// every descendant. stampPassage stamps the whole fan-out with one
		// Passage (block is atomic per Passage, ADR-056). A static-when-false
		// block isn't gated by emitStaticWhenSkip (it skips block tasks) — it
		// falls through here instead: walkBlockChildren ANDs block.when into
		// every descendant, and each child emits its own skip placeholder with
		// register/requisites (keeps flat-register-scope intact on skip —
		// otherwise descendant registers would be lost).
		if task.Block != nil {
			bt, bp, berr := p.renderBlockTask(ctx, in, task, idx, targeted, includeGroupKeep)
			if berr != nil {
				return nil, nil, berr
			}
			tasks = append(tasks, bt...)
			plans = append(plans, bp...)
			idx += len(bt)
			stampPassage(tasks, passageStart, passage)
			continue
		}

		rt, err := p.renderTask(ctx, in, task, idx, targeted)
		if err != nil {
			return nil, nil, err
		}

		tasks = append(tasks, rt)
		plans = append(plans, DispatchPlan{
			TaskIndex:   idx,
			TargetSIDs:  sidsOf(targeted),
			SerialWidth: serialWidth(task.Serial, len(targeted)),
		})
		idx++
		stampPassage(tasks, passageStart, passage)
	}

	// Resolve `onchanges:`/`onfail:` register names to task indices (Variant
	// A) as a final pass once the whole plan is built: with apply:destiny/loop
	// the Index is contiguous but unknown earlier (renderTaskIter renders
	// before later source tasks exist).
	if err := resolveOnChanges(tasks); err != nil {
		return nil, nil, err
	}
	if err := resolveOnFail(tasks); err != nil {
		return nil, nil, err
	}
	// `require:` (ADR-0075) resolves in the same pass and by the same Variant A,
	// but additionally checks the Passage invariant — which needs every task's
	// Passage stamped, i.e. the end of the walk.
	if err := resolveRequire(tasks); err != nil {
		return nil, nil, err
	}

	return tasks, plans, nil
}

// renderTask renders params for a single module task (after vault-resolve +
// CEL) and builds a RenderedTask. Thin wrapper over renderTaskIter with no
// loop variables.
func (p *Pipeline) renderTask(ctx context.Context, in RenderInput, task config.Task, idx int, targeted []*topology.HostFacts) (*RenderedTask, error) {
	return p.renderTaskIter(ctx, in, task, idx, targeted, nil)
}

// renderTaskIter renders params for a single module task (or a single
// `loop:` iteration) per host (after vault-resolve + CEL) and builds a
// RenderedTask. params are rendered per host and checked for host-invariance
// (pilot restriction, see Render).
//
// loopVars holds the current iteration's variables (`<as>`/`<index_as>`);
// nil for a task without loop:. Host-invariance is checked per-iteration: for
// fixed loopVars, params must match across all targeted hosts. Across the
// iteration axis, loop legitimately produces different params (caller
// renderLoopTask calls renderTaskIter with different loopVars per iteration)
// — that's not an invariant violation.
//
// Empty targeted (where: filtered everyone out) still produces a task in the
// list (with an empty DispatchPlan); params render in a context without
// soulprint so the RenderedTask is complete and the orchestrator simply
// skips dispatch.
func (p *Pipeline) renderTaskIter(ctx context.Context, in RenderInput, task config.Task, idx int, targeted []*topology.HostFacts, loopVars map[string]any) (*RenderedTask, error) {
	// Fail-closed guard: a host-variant flow-control predicate
	// (soulprint.self) on a multi-host target would silently resolve using
	// the first host's facts for everyone (dispatch hands out one
	// RenderedTask carrying the first host's flow_context). Reject before
	// building flow_context — mirrors reLoopWhenSoulprint (loop.go).
	if err := guardFlowControlHostInvariant(task, targeted); err != nil {
		return nil, err
	}

	rt := &RenderedTask{
		Index:    idx,
		Name:     task.Name,
		Module:   task.Module.Module,
		Register: task.Register,
		ID:       task.ID,
		Timeout:  task.Timeout,
		// [ADR-0083] §8: derived from the module manifest, never authored.
		SecretOutput: config.SecretOutputFields(task.Module.Module, in.Modules),
		// flow-control CEL strings (ADR-012(d)) pass through as-is — Keeper
		// never evaluates them (they depend on register.* from prior tasks,
		// known only to Soul). Host-invariant (one predicate text per task);
		// Soul evaluates per host.
		When:           task.When,
		ChangedWhen:    task.ChangedWhen,
		FailedWhen:     task.FailedWhen,
		onChangesNames: task.OnChanges,
		onFailNames:    task.OnFail,
	}
	applyConcurrency(rt, task)

	// retry: (destiny/tasks.md §9) is enforced Soul-side; Keeper just passes
	// the fields through. nil Retry → one attempt (zero-value RetryCount=0,
	// until/delay empty).
	if task.Retry != nil {
		rt.RetryCount = task.Retry.Count
		rt.RetryDelay = task.Retry.Delay
		rt.Until = task.Retry.Until
	}

	// Hosts for CEL render: targeted, or — if where: filtered everyone out —
	// one synthetic empty context (params with no soulprint dependency).
	renderHosts := targeted
	if len(renderHosts) == 0 {
		renderHosts = []*topology.HostFacts{{}}
	}

	// Static-when placeholder-skip (ADR-012(d), Variant b): when a register-/
	// soulprint-independent when: evaluates false on Keeper, params aren't
	// rendered — the task still ends up SKIPPED on Soul (it evaluates the
	// same when against the same flow_context). This fixes multi-action
	// destinies: tasks on an inactive branch (`when: input.action ==
	// 'apply'` under a different action) that read an optional input which
	// isn't present would otherwise hit no-such-key → render_failed during
	// eager render. The skip collects only flow_context (Soul reads it for
	// evalWhen — built from input/vars/incarnation/self, never from
	// the failing params — so it's safe) and leaves a complete RenderedTask
	// (Index/Passage/Register/When/requisites kept, params empty). The
	// decision is deterministic (static-when is host-invariant) — taken on
	// the first host; fc for the rest is still built to keep their snapshot
	// valid.
	if skip, serr := p.staticWhenSkips(in, task, renderHosts, len(targeted), loopVars); serr != nil {
		return nil, serr
	} else if skip != nil {
		rt.FlowContext = skip
		return rt, nil
	}

	resolved, err := resolveVaultRefs(ctx, p.vault, task.Module.Params)
	if err != nil {
		return nil, fmt.Errorf("render: task %q: %w", task.Name, err)
	}

	// seal / sealed-paths ([ADR-010] §7.4): mark params cell paths whose raw
	// `${ … }` value reads a secret source (secret-input/vault()). Walking
	// raw params (task.Module.Params, before resolveVaultRefs+CEL) is the
	// only place the original expressions are visible. Per-task
	// (host-invariant), nil Sealed → no-op.
	collectSealed(p.cel, in.Sealed, task.Module.Params, scenarioSealSources(in), "")

	isRendered := task.Module.Module == moduleFileRendered

	// core.file.rendered: before the per-host loop, read the template
	// content once and detect whether it references the root `.input.*`
	// (tmpl.UsesRootField via AST, not string search — mentioning `.input`
	// inside a body comment doesn't count). The template path is
	// host-invariant (pilot contract), so content and the flag are too.
	// content is reused by injectTemplateContent below (no double read).
	var templateContent string
	var injectInput bool
	var fileVarKeys map[string]bool
	var wholeVars bool
	if isRendered {
		content, uses, terr := p.resolveTemplateUsesInput(in, resolved)
		if terr != nil {
			return nil, fmt.Errorf("render: task %q: %w", task.Name, terr)
		}
		templateContent = content
		injectInput = uses

		// Targeted file-vars (vars.yml) injection: which `.vars.<key>` the
		// template actually reads (AST) — see buildRenderContext/
		// referencedFileVars. Host-invariant (one template path), computed
		// once before the per-host loop.
		keys, kerr := templateVarSubKeys(templateContent)
		if kerr != nil {
			return nil, fmt.Errorf("render: task %q: %w", task.Name, kerr)
		}
		fileVarKeys = keys

		// A whole-map read gets the whole map: scoping by subkeys cannot serve a
		// template that names none.
		whole, werr := templateReadsWholeVars(templateContent)
		if werr != nil {
			return nil, fmt.Errorf("render: task %q: %w", task.Name, werr)
		}
		wholeVars = whole

		// seal S-1 (ADR-010 §7.4, Variant B): mark sealed paths of
		// render_context.input.<secret> per schema, gated the same as the
		// input injection itself (see sealRenderContextInput/
		// buildRenderContext §Security).
		if injectInput {
			sealRenderContextInput(in.Sealed, in)
		}
	}

	var firstSID string
	for hi, h := range renderHosts {
		vars := hostLoopVars(in, h, len(targeted), loopVars)
		vars, err = resolveTaskVars(p.cel, fileVarsForHost(in, h), task.Vars, vars)
		if err != nil {
			return nil, fmt.Errorf("render: task %q (host %s): %w", task.Name, h.SID, err)
		}
		st, err := renderParams(p.cel, resolved, vars)
		if err != nil {
			return nil, fmt.Errorf("render: task %q (host %s): %w", task.Name, h.SID, err)
		}
		// core.file.rendered: build the per-host render_context
		// (buildRenderContext) and place it in params alongside
		// template_content. The flat params.vars key is removed — Soul reads
		// the root only from render_context (§3.2/§6). render_context is
		// host-variant (self per host) — excluded from the host-invariance
		// check below; rt.Params carries the first host's value.
		//
		// Partial fix for open Q #25 (render_context.self only): each host's
		// render_context is materialized into rt.RenderContextBySID[SID].
		// Without this, every host would get the first host's render_context
		// (one *RenderedTask is dispatched to all — groupByHost/claim),
		// silently rendering a self-variant template (`{{
		// .self.network.primary_ip }}`) with the first host's facts.
		// ToProtoTasksForHost overlays the per-host variant onto Params when
		// building a given SID's ApplyRequest. The map is only populated for
		// multi-host (N=1: first host's render_context == the only one, no
		// overlay needed, behavior unchanged). Full per-host dispatch
		// (Variant B) is a separate ADR.
		if isRendered {
			paramsVars := extractParamsVars(st)
			delete(st.Fields, paramVars)
			available := mergeVars(in.ServiceVars, fileVarsForHost(in, h))
			fileVars := referencedFileVars(available, fileVarKeys)
			if wholeVars {
				// A whole-map read (`index .vars "x"`, `range … := .vars`,
				// `toYaml .vars`) names no key the AST can see, so the
				// statically-referenced filter cannot be applied to the SERVICE
				// layer — holding it back is what made such a template render an
				// empty section into a config file and restart the service onto it.
				//
				// The FILE layer is deliberately not widened with it. In a destiny
				// pass ServiceVars is nil by construction (destiny.go), so
				// `available` there is the destiny's entire vars.yml — every
				// internal plumbing var it uses to build paths and versions. Passing
				// that whole set would put it in render_context, which crosses the
				// wire to the host, and would buy nothing: a destiny's own vars.yml
				// is already fully in reach through the same filter for every key
				// the template names, and the keys a dynamic index reaches for are
				// the ones the task put there.
				fileVars = mergeVars(in.ServiceVars, fileVars)
			}
			if err := setRenderContext(st, buildRenderContext(in, h, fileVars, paramsVars, injectInput)); err != nil {
				return nil, fmt.Errorf("render: task %q (host %s): %w", task.Name, h.SID, err)
			}
			if len(renderHosts) > 1 {
				if rt.RenderContextBySID == nil {
					rt.RenderContextBySID = make(map[string]*structpb.Struct, len(renderHosts))
				}
				rt.RenderContextBySID[h.SID] = st.Fields[paramRenderContext].GetStructValue()
			}
		}
		// flow_context (ADR-012(d)): per-host snapshot {input,vars,
		// incarnation,self} for Soul-side flow-control predicates. Built from
		// the same vars as params (minus soulprint.hosts/loop, see
		// buildFlowContext). Host-variant (self per host) — like
		// render_context, excluded from the host-invariance check; rt carries
		// the first host's value (golden path).
		//
		// For hi>0, fc is rebuilt only to surface build errors (validating
		// this host's snapshot); the wire value rt.FlowContext comes from the
		// first host (hi==0). Not a forgotten per-host dispatch — host-variant
		// flow-control on multi-host is already rejected by
		// guardFlowControlHostInvariant.
		fc, err := buildFlowContext(in, h, vars, len(targeted))
		if err != nil {
			return nil, fmt.Errorf("render: task %q (host %s): %w", task.Name, h.SID, err)
		}

		if hi == 0 {
			rt.Params = st
			rt.FlowContext = fc
			firstSID = h.SID
			continue
		}
		if !paramsHostInvariant(rt.Params, st) {
			return nil, fmt.Errorf(
				"render: task %q gives host-dependent params (%s vs %s) - host variance of params is outside pilot scope (per-host ApplyRequest is an orchestrator-layer concern)",
				task.Name, firstSID, h.SID)
		}
		// Second fail-closed layer: host-variant flow_context (vars derived
		// from soulprint.self, leaking into flow_context.vars). The predicate
		// text then contains no "soulprint" (e.g. `when: vars.is_debian`), so
		// the regex guard (guardFlowControlHostInvariant) misses it — a
		// task-level vars bypass. Here we diff the collected flow_context
		// MINUS self across hosts (self is host-variant by nature and already
		// covered by the text guard).
		//
		// GATE: this check only runs when at least one flow-control predicate
		// is non-empty. Without a predicate, Soul never reads flow_context —
		// its variance doesn't matter; a legitimate task with host-variant
		// vars-in-params (no when) should fail on paramsHostInvariant above,
		// not here.
		//
		// Both layers (text regex + snapshot diff) are a temporary
		// fail-closed measure until per-host dispatch (open Q #25) lands;
		// they'll be removed together when it does.
		if hasFlowControl(task) && !flowContextHostInvariant(rt.FlowContext, fc) {
			return nil, fmt.Errorf(
				"render: task %q: host-variant flow_context (vars derived from soulprint.self) on a multi-host target (%s vs %s) - fail-closed; per-host dispatch is deferred (separate ADR)",
				task.Name, firstSID, h.SID)
		}
	}

	// core.file.rendered: after the CEL phase, replace params.template (a
	// path) with the literal template_content (Keeper reads the .tmpl,
	// A1/ADR-012(d)). text/template is not executed here — rendering happens
	// on Soul. Content was already read before the per-host loop
	// (resolveTemplateUsesInput) — reused, not read again.
	if err := injectTemplateContent(rt, in.Templates, templateContent); err != nil {
		return nil, err
	}

	return rt, nil
}

// staticWhenSkips decides whether to skip rendering a task's params based on
// a static when: (ADR-012(d), Variant b placeholder-skip). Returns:
//   - (fc, nil) — SKIP the task: when is static (register-/soulprint-
//     independent) and evaluated false. fc is the first host's flow_context
//     (Soul reads it for its own evalWhen → confirms when:false → SKIPPED,
//     as today);
//   - (nil, nil) — don't skip: when is non-static (register/soulprint/empty)
//     or static-but-true. Normal path, params get rendered.
//   - (nil, err) — error building flow_context or evaluating the static
//     predicate (a broken when — Keeper fails the same way Soul would; see
//     evalStaticWhen).
//
// The decision is deterministic: static-when is host-invariant by
// construction (doesn't depend on soulprint.self/register, the only
// host-variant layers), so it's evaluated on the first host. flow_context
// for the remaining hosts is still built (to validate each host's snapshot,
// as in renderTaskIter's main loop), but doesn't affect the static-when
// outcome. This keeps the skip consistent across hosts and across Passages:
// one input/state snapshot of the run yields the same false on every host
// and on a repeat render of the next Passage.
func (p *Pipeline) staticWhenSkips(
	in RenderInput,
	task config.Task,
	renderHosts []*topology.HostFacts,
	targetCount int,
	loopVars map[string]any,
) (*structpb.Struct, error) {
	if !isStaticWhen(task.When) {
		return nil, nil
	}

	var firstFC *structpb.Struct
	for hi, h := range renderHosts {
		vars := hostLoopVars(in, h, targetCount, loopVars)
		vars, err := resolveTaskVars(p.cel, fileVarsForHost(in, h), task.Vars, vars)
		if err != nil {
			return nil, fmt.Errorf("render: task %q (host %s): %w", task.Name, h.SID, err)
		}
		fc, err := buildFlowContext(in, h, vars, targetCount)
		if err != nil {
			return nil, fmt.Errorf("render: task %q (host %s): %w", task.Name, h.SID, err)
		}
		if hi == 0 {
			firstFC = fc
		}
	}

	pass, err := evalStaticWhen(task.When, firstFC)
	if err != nil {
		return nil, fmt.Errorf("render: task %q: static-when %q: %w", task.Name, task.When, err)
	}
	if pass {
		return nil, nil // when:true — task is active, render params normally.
	}
	return firstFC, nil
}

// evalIncludeWhen evaluates the include-when of a conditional include
// (conditional-include group-drop, ADR-009 amendment) — keeper-side, once
// per group. include-when is static by contract (config.ExpandIncludes
// would reject a dynamic one as include_when_dynamic_unsupported,
// isStaticWhen reconfirms it here as defense-in-depth), so it's evaluated
// with the same flow-control engine and flow_context as static-when-skip —
// host-invariant, on the roster's first host.
//
// Returns keep: true → the group stays (tasks render normally); false → the
// group is dropped (caller continues without emitting/idx++). An empty
// include-when never reaches here (IncludeGroupID!=0 ⇔ non-empty when, per
// ExpandIncludes). An eval error (broken predicate / no-such-key on a
// missing input) propagates to the caller — Keeper fails with render_failed
// the same way Soul would on a static-when.
func (p *Pipeline) evalIncludeWhen(in RenderInput, when string) (bool, error) {
	// defense-in-depth: ExpandIncludes already guaranteed staticity; if a
	// non-static include-when reaches here (an expansion bug) — fail-closed,
	// don't silently keep.
	if !isStaticWhen(when) {
		return false, fmt.Errorf("render: include-when %q is not static (register/soulprint) - group-drop requires a static predicate (ADR-009 amendment)", when)
	}
	host := &topology.HostFacts{}
	if len(in.Hosts) > 0 {
		host = in.Hosts[0]
	}
	vars := hostVars(in, host, len(in.Hosts))
	vars, err := resolveTaskVars(p.cel, fileVarsForHost(in, host), nil, vars)
	if err != nil {
		return false, fmt.Errorf("render: include-when %q: %w", when, err)
	}
	fc, err := buildFlowContext(in, host, vars, len(in.Hosts))
	if err != nil {
		return false, fmt.Errorf("render: include-when %q: flow_context: %w", when, err)
	}
	keep, err := evalStaticWhen(when, fc)
	if err != nil {
		return false, fmt.Errorf("render: include-when %q: %w", when, err)
	}
	return keep, nil
}

// includeGroupCache memoizes the conditional-include group-drop decision for ONE
// render pass: group id (config.Task.IncludeGroupID, stamped by
// config.ExpandIncludes) → keep/drop. include-when is static and therefore
// host-invariant, so one evaluation per group covers every task carrying that
// id. Each pass keeps its OWN cache — a destiny pass evaluates in the isolated
// destiny env, never the scenario one.
type includeGroupCache map[int]bool

// keepIncludeGroup reports whether a task's conditional-include group stays in
// the plan (ADR-009 amendment, conditional-include). The single source of truth
// for group-drop, shared by every task walk: the scenario loop, the destiny
// loop, the assert pre-flight and block descendants (a within-block include
// splices its group INTO a block, so walkBlockChildren gates children with the
// same cache).
//
// An unconditional task (IncludeGroupID==0 — the normal path) is always kept and
// never touches the cache. keep=false is a REAL drop: the caller continues
// without emitting a RenderedTask and without advancing idx (no index reserved),
// unlike a static-when placeholder.
func (p *Pipeline) keepIncludeGroup(in RenderInput, task config.Task, cache includeGroupCache) (bool, error) {
	if task.IncludeGroupID == 0 {
		return true, nil
	}
	if keep, ok := cache[task.IncludeGroupID]; ok {
		return keep, nil
	}
	keep, err := p.evalIncludeWhen(in, task.IncludeWhen)
	if err != nil {
		return false, err
	}
	cache[task.IncludeGroupID] = keep
	return keep, nil
}

// emitStaticWhenSkip is an early static-when placeholder-skip, run at the
// START of the task-iteration loop, before guardPilotDSL/guardDestinyTask
// (ADR-012(d), extending the static-when invariant). If `when:` is static
// (register-/soulprint-independent) and evaluates false, the task is gated
// off: emit skip placeholder(s), mutating tasks/plans/idx through pointers,
// and return skipped=true. The caller does `continue` without the guard and
// without rendering — so unsupported DSL (`loop:` on an `apply:` task) on an
// inactive branch is never rejected (it's unreachable — the task never
// executes).
//
// Returns:
//   - (false, nil) — not a static-skip: when is non-static or
//     static-but-true. Caller takes the normal path (guard →
//     resolveTargets → render);
//   - (true, nil) — task skipped, placeholder(s) already appended;
//   - (false, err) — error building flow_context / evaluating the static
//     predicate.
//
// flow_context is built from in.Hosts (a synthetic empty host when the
// roster is empty): static-when is host-invariant (doesn't depend on
// soulprint.self), the outcome is the same on every host; Soul reads `self`
// in the placeholder flow_context only as data for its own evalWhen, which
// evaluates the same predicate to false too → SKIPPED.
//
// loop task (task.Loop != nil): N/1 skip placeholders via loopStaticSkip —
// keeps Index parity with the active branch (resolvable items → N,
// unresolvable → 1). This catches loop before renderLoopTask — the only
// static-skip path for loop (renderLoopTask itself no longer has a
// static-when gate). Non-loop task → one placeholder.
func (p *Pipeline) emitStaticWhenSkip(
	ctx context.Context,
	in RenderInput,
	task config.Task,
	tasks *[]*RenderedTask,
	plans *[]DispatchPlan,
	idx *int,
) (bool, error) {
	if !isStaticWhen(task.When) {
		return false, nil
	}

	// A block task with a static-false when: is NOT gated here with a single
	// placeholder (otherwise the renderBlockTask/renderDestinyBlock branch
	// never runs, descendants never materialize, and their register is lost
	// — resolveOnChanges fails downstream with ErrOnChangesUnknownRegister).
	// Instead defer to the block: branch — mergeBlockInheritance ANDs
	// block.when into every descendant, each descendant's static-when
	// becomes false, and each child goes through emitStaticWhenSkip inside
	// walkBlockChildren, emitting its own placeholder with its own
	// Register/requisites/ID (the loopStaticSkip pattern — block expands
	// per-descendant, not as one placeholder). flat-register-scope stays
	// intact on skip. The block node itself carries no register (forbidden
	// by the validator) — nothing to lose there.
	if task.Block != nil {
		return false, nil
	}

	renderHosts := in.Hosts
	if len(renderHosts) == 0 {
		renderHosts = []*topology.HostFacts{{}}
	}
	skip, err := p.staticWhenSkips(in, task, renderHosts, len(in.Hosts), nil)
	if err != nil {
		return false, err
	}
	if skip == nil {
		return false, nil // static-true → active, normal path.
	}

	if task.Loop != nil {
		asName := task.Loop.As
		if asName == "" {
			asName = defaultLoopVar
		}
		lt, lp, lerr := p.loopStaticSkip(in, task, *idx, in.Hosts, asName, skip)
		if lerr != nil {
			return false, lerr
		}
		*tasks = append(*tasks, lt...)
		*plans = append(*plans, lp...)
		*idx += len(lt)
		return true, nil
	}

	*tasks = append(*tasks, p.staticSkipPlaceholder(task, *idx, skip, in.Modules))
	*plans = append(*plans, DispatchPlan{TaskIndex: *idx})
	*idx++
	return true, nil
}

// staticSkipPlaceholder builds one skip placeholder for a task with a
// statically-false when: (Params=nil — render skipped; first host's
// flow_context; When/ID/Register/requisites passed through). Module is set
// when a module task is present (a block node has none → empty
// Module — placeholder is still valid, just never executed).
func (p *Pipeline) staticSkipPlaceholder(task config.Task, idx int, skip *structpb.Struct, modules config.ModuleManifestResolver) *RenderedTask {
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
	applyConcurrency(rt, task)
	if task.Module != nil {
		rt.Module = task.Module.Module
		rt.SecretOutput = config.SecretOutputFields(rt.Module, modules)
	}
	return rt
}

// EvalAsserts evaluates ONLY the scenario's assert tasks (ADR-009 amendment
// 2026-06-23, two-point eval) — no RenderedTask emission, no vault-resolve/
// dispatch/on-where. Reused by the create-run pre-flight gate (request path,
// before the incarnation is committed —
// keeper/internal/scenario.PreflightAssert): same source of truth for
// predicate evaluation as the render branch ([Render] → [evalAssertTask]),
// no separate dialect.
//
// The contract matches Render's assert branch bit-for-bit: walks
// scenario.Tasks in order, applies conditional-include group-drop
// (Task.IncludeGroupID/IncludeWhen, set by config.ExpandIncludes) before the
// assert check — like [Render]: an assert from a dropped include group is
// never evaluated (a cluster.yml assert on a sentinel run is excluded from
// the plan rather than failing on CEL no-such-key). For each remaining
// [IsAssertTask] task it calls the shared [evalAssertTask] (same when: gate,
// same run-level CEL context with soulprint.hosts). First false →
// [ErrAssertFailed] (abort, text = message + failing predicate index/text).
// Non-assert tasks are skipped (pre-flight doesn't render them — Render does
// that at run start). All asserts true / scenario with no asserts → nil
// (most scenarios are a no-op here, as pilot requires).
//
// NOT staged: pre-flight is always non-staged (single pass, TaskPassage=nil);
// assert is run-level "once per run" by construction, so Render's
// passage-filter isn't needed here (it guards against re-running the assert
// on every Passage of a staged pass, which doesn't apply to pre-flight).
// nil Scenario → error (a caller error, as in Render).
func (p *Pipeline) EvalAsserts(ctx context.Context, in RenderInput) error {
	if in.Scenario == nil {
		return fmt.Errorf("render: scenario manifest is nil")
	}
	// assert pre-flight is its own pass; assert.that[] may call vault(), so the
	// context carries cancel/timeout, the memo and the fence.
	ctx = WithVaultFence(ctx, in.Incarnation.Service)
	in.Ctx = ctx
	// compute: available in assert.that[] the same as in params/where (one
	// resolve, run-level context, no soulprint). Idempotent with
	// Render.
	computed, cerr := p.resolveCompute(in)
	if cerr != nil {
		return cerr
	}
	in.Compute = computed

	// includeGroupKeep — conditional-include (group-drop) decision cache,
	// mirroring [Render] (see [Pipeline.keepIncludeGroup]). Without it, an
	// assert from a conditionally-included file (cluster.yml under `when:
	// input.redis_type=='cluster'`) would evaluate even under a mismatched
	// mode (a sentinel run → CEL no-such-key: shards), whereas Render/Trial
	// drop that group before the assert. This restores a single source of
	// truth: pre-flight applies include-when to asserts the same way
	// run-render does.
	includeGroupKeep := includeGroupCache{}
	for i := range in.Scenario.Tasks {
		task := in.Scenario.Tasks[i]

		// Conditional-include group-drop — before IsAssertTask, as in
		// [Render]: a false group include-when physically excludes the task
		// from the plan, its assert is never evaluated.
		if keep, kerr := p.keepIncludeGroup(in, task, includeGroupKeep); kerr != nil {
			return kerr
		} else if !keep {
			continue
		}

		if !IsAssertTask(task) {
			continue
		}
		if err := p.evalAssertTask(in, task); err != nil {
			return err
		}
	}
	return nil
}

// evalAssertTask evaluates an assert task (ADR-009 amendment 2026-06-23) — a
// keeper-side render-time precondition of the run. RUN-LEVEL (once, not per
// host): checks a topology invariant of the run, not a per-host predicate.
//
// The `when:` gate is honored: if when is static (register-/soulprint-
// independent, isStaticWhen) and evaluates false, the assert isn't evaluated
// (inactive-branch placeholder-skip, same as a regular task: a cluster
// assert stays quiet on a standalone run). Empty or statically-true when →
// assert evaluates. A non-static when (register-/soulprint-dependent) on an
// assert is outside pilot scope — assert is run-level and the register map
// is incomplete; such a when is treated as "active" (predicates evaluate
// anyway) — we don't fail this degenerate case, it's just unused in pilot.
//
// `that[]` predicates evaluate in the FULL scenario CEL context, including
// soulprint.hosts (AllowHosts=!destinyIsolated, as in
// evalWhere/resolveTargets): the run-level context is built by hostVars over
// the roster's first host (self isn't used by topology predicates here;
// size(soulprint.hosts) is host-invariant). First false →
// ErrAssertFailed (render aborts before dispatch): text = message (or
// default) + failing predicate's index/text. All true → nil (the assert
// "disappears" from the plan, no RenderedTask emitted — caller does that via
// continue).
func (p *Pipeline) evalAssertTask(in RenderInput, task config.Task) error {
	// when: gate — statically-false → assert not evaluated (inactive mode).
	if isStaticWhen(task.When) {
		renderHosts := in.Hosts
		if len(renderHosts) == 0 {
			renderHosts = []*topology.HostFacts{{}}
		}
		fc, err := buildFlowContext(in, renderHosts[0], hostVars(in, renderHosts[0], len(in.Hosts)), len(in.Hosts))
		if err != nil {
			return fmt.Errorf("render: assert %q: when flow_context: %w", task.Name, err)
		}
		pass, err := evalStaticWhen(task.When, fc)
		if err != nil {
			return fmt.Errorf("render: assert %q: static-when %q: %w", task.Name, task.When, err)
		}
		if !pass {
			return nil // when:false — assert inactive (placeholder-skip semantics).
		}
	}

	// Run-level context: roster's first host (or a synthetic empty one when
	// the roster is empty). soulprint.hosts projects from in.Hosts
	// (AllowHosts=true in the scenario pass); size(soulprint.hosts) is
	// host-invariant — the choice of first host for self doesn't affect
	// topology predicate results.
	host := &topology.HostFacts{}
	if len(in.Hosts) > 0 {
		host = in.Hosts[0]
	}
	vars := hostVars(in, host, len(in.Hosts))
	vars, err := resolveTaskVars(p.cel, fileVarsForHost(in, host), task.Vars, vars)
	if err != nil {
		return fmt.Errorf("render: assert %q: %w", task.Name, err)
	}

	for i, pred := range task.Assert.That {
		ok, err := evalBoolExpr(p.cel, "assert.that", pred, vars)
		if err != nil {
			return fmt.Errorf("render: assert %q: %w", task.Name, err)
		}
		if !ok {
			return fmt.Errorf("%w: %s (predicate that[%d] %q evaluated to false)", ErrAssertFailed, assertMessage(task), i, pred)
		}
	}
	return nil
}

// assertMessage builds a human-readable assert-failure message: the
// author's message, or a name-based default when message is omitted.
func assertMessage(task config.Task) string {
	if task.Assert != nil && task.Assert.Message != "" {
		return task.Assert.Message
	}
	if task.Name != "" {
		return fmt.Sprintf("assert %q failed", task.Name)
	}
	return "assert predicate failed"
}

// renderKeeperTask renders a keeper-side task (`on: keeper`, docs/keeper/
// modules.md): params are computed once in the keeper context (keeperVars —
// no per-host soulprint), since there are no hosts — the step runs on the
// keeper instance itself. No host-invariance check (single keeper target).
//
// Pilot: a keeper task is module-only (apply:/loop:/block: on it are
// rejected above by guardPilotDSL/here). core.file.rendered never appears
// keeper-side (it's a Soul-side module), so render_context/template_content
// aren't collected. flow_context isn't built either: a keeper task runs
// locally in the scenario-runner, which doesn't evaluate flow-control
// predicates (when/changed_when/failed_when) yet — the fields are passed
// through as CEL strings for RenderedTask symmetry, but the MVP keeper
// executor ignores them (like Soul did before integrating them). The one
// spelling NOT passed through silently is a register-/soulprint-reading
// `when:` — [guardKeeperWhen] rejects it ([ADR-0084] F-D). register:
// is passed through — the keeper executor accumulates this task's register
// under KeeperTargetSID.
func (p *Pipeline) renderKeeperTask(ctx context.Context, in RenderInput, task config.Task, idx int) (*RenderedTask, error) {
	if task.Apply != nil {
		return nil, fmt.Errorf("%w: apply: on a keeper-side task (task[%d] %q)", ErrUnsupportedDSL, idx, task.Name)
	}
	if task.Loop != nil {
		return nil, fmt.Errorf("%w: loop: on a keeper-side task (task[%d] %q)", ErrUnsupportedDSL, idx, task.Name)
	}
	// block: on a keeper task. The keeper branch of the Render loop runs BEFORE
	// the block branch, so a block carrying `on: keeper` never reaches
	// renderBlockTask - it lands here with Module == nil. Without this check the
	// next line dereferences it and the render panics.
	//
	// Moving the key down to the block's children is not a workaround: renderBlockTask
	// fans a child out through resolveTargets, which refuses `on: keeper` outright.
	// Blocks and keeper tasks are disjoint at both levels - the keeper tasks go flat.
	if task.Block != nil {
		return nil, fmt.Errorf("%w: block: on a keeper-side task (task[%d] %q) - a keeper task cannot live in a block; write it flat, in the scenario's own task list", ErrUnsupportedDSL, idx, task.Name)
	}
	// async: is Soul-side task concurrency (ADR-0075): the flag rides
	// RenderedTask to a Soul runner, and a keeper task never reaches one. It
	// would be a silent no-op — reject, like apply:/loop: above. `require:` is
	// NOT rejected: the keeper executor runs its tasks in plan order, so the
	// barrier is simply already satisfied (the same redundancy a linear
	// Soul-side flow has), and threading it keeps its names under the
	// unknown-register check.
	if task.Async {
		return nil, fmt.Errorf("%w: async: on a keeper-side task (task[%d] %q)", ErrUnsupportedDSL, idx, task.Name)
	}
	if err := guardKeeperWhen(task, idx); err != nil {
		return nil, err
	}

	resolved, err := resolveVaultRefs(ctx, p.vault, task.Module.Params)
	if err != nil {
		return nil, fmt.Errorf("render: keeper task %q: %w", task.Name, err)
	}

	// seal / sealed-paths ([ADR-010] §7.4): a keeper-side task
	// (core.vault.kv-read and similar) can also carry `${ vault(...) }`/`${
	// input.<secret> }` in params.
	collectSealed(p.cel, in.Sealed, task.Module.Params, scenarioSealSources(in), "")

	vars := keeperVars(in)
	// keeper-side task — not a destiny pass (destiny tasks are all Soul-side
	// in pilot); no file-vars base (DestinyVarsResolved is nil outside
	// renderApplyDestiny).
	vars, err = resolveTaskVars(p.cel, nil, task.Vars, vars)
	if err != nil {
		return nil, fmt.Errorf("render: keeper task %q: %w", task.Name, err)
	}
	st, err := renderParams(p.cel, resolved, vars)
	if err != nil {
		return nil, fmt.Errorf("render: keeper task %q: %w", task.Name, err)
	}

	rt := &RenderedTask{
		Index:          idx,
		Name:           task.Name,
		Module:         task.Module.Module,
		Params:         st,
		Register:       task.Register,
		ID:             task.ID,
		Timeout:        task.Timeout,
		When:           task.When,
		ChangedWhen:    task.ChangedWhen,
		FailedWhen:     task.FailedWhen,
		onChangesNames: task.OnChanges,
		onFailNames:    task.OnFail,
		SecretOutput:   config.SecretOutputFields(task.Module.Module, in.Modules),
	}
	applyConcurrency(rt, task)
	if task.Retry != nil {
		rt.RetryCount = task.Retry.Count
		rt.RetryDelay = task.Retry.Delay
		rt.Until = task.Retry.Until
	}
	return rt, nil
}

// reFlowControlSoulprint catches a soulprint reference in any flow-control
// predicate (when/changed_when/failed_when). Style mirrors
// reLoopWhenSoulprint (loop.go): one word-boundary regex, fail-closed on
// multi-host.
var reFlowControlSoulprint = regexp.MustCompile(`\bsoulprint\b`)

// flowControlEngine is the shared Soul-side flow-control engine
// ([cel.NewFlowControl], ADR-012(d)) used for Keeper-side static-when
// placeholder-skip. CRITICAL: this is the same sandbox Soul uses for
// evalWhen (applyrunner.go), not the full Keeper env — guarantees
// static-when-false on Keeper is bit-for-bit equivalent to when-false on
// Soul (same env, same flow_context). Thread-safe (compile-cache under
// RWMutex) and shared across all runs; built lazily once (rbac/soulprint,
// statepredicate pattern) — the constructor doesn't depend on runtime
// state, but building it in init() would cost every import.
var (
	flowControlEngineOnce sync.Once
	flowControlEngineInst *cel.Engine
	flowControlEngineErr  error
)

func flowControlEngine() (*cel.Engine, error) {
	flowControlEngineOnce.Do(func() {
		flowControlEngineInst, flowControlEngineErr = cel.NewFlowControl()
	})
	return flowControlEngineInst, flowControlEngineErr
}

// isStaticWhen reports whether a when: predicate can be evaluated
// Keeper-side before rendering params (placeholder-skip, ADR-012(d), Variant
// b). Static means a non-empty when that depends on neither register.*
// (prior tasks' results, known only to Soul) nor soulprint (the host-variant
// layer). Such a predicate is deterministic on Keeper from flow_context
// (input/vars/incarnation), and its false outcome is the same on
// every host of the run.
//
// The rule itself lives in [config.IsStaticPredicate] and this is a thin alias
// over it, deliberately not a second copy: the same line is drawn offline by
// soul-lint (a conditional include's `when:`, an applier's `when:`, and
// `when_on_keeper_dynamic_unsupported`), and a render that decided "static" by
// its own reimplementation could accept what the linter refuses, or refuse
// mid-render what the linter accepted. Empty when → false (no predicate —
// nothing for Keeper to evaluate; the task is unconditional, goes through the
// normal params-render path). A mixed when (register+input) → not static (it
// has a register ref) — stays Soul-side.
//
// Bracket form register["x"] is not detected (ExtractRegisterRefs is dot-form
// only, mirroring checkPredicateRefs in the config validator); latent in
// practice, a probe register is always written in dot form.
func isStaticWhen(when string) bool {
	return config.IsStaticPredicate(when)
}

// evalStaticWhen evaluates a static when: Keeper-side, through the same
// flow-control engine and the same flow_context that would go to Soul
// (evalWhen). Returns the predicate result. Called ONLY for when that
// passed isStaticWhen (register-/soulprint-independent) — register is empty
// in the activation, and its emptiness doesn't affect the outcome.
// Bit-for-bit equivalent to Soul-side evalWhen (same env, same
// flow_context).
//
// An eval error (e.g. no-such-key on a missing input) propagates to the
// caller: Keeper fails with render_failed on a broken static predicate the
// same way Soul would fail in evalWhen — no behavior divergence, the
// author's error just surfaces earlier.
func evalStaticWhen(when string, fc *structpb.Struct) (bool, error) {
	engine, err := flowControlEngine()
	if err != nil {
		return false, fmt.Errorf("static-when: building the flow-control engine: %w", err)
	}
	// Activation uses the Soul-side flowControlVars shape (flow_context +
	// empty register). The register map is empty: isStaticWhen already
	// guaranteed no register.* in when, so register's emptiness doesn't
	// affect the outcome (mirrors Soul, where register isn't read for a
	// register-independent when either).
	return engine.EvalPredicate(when, flowControlVarsFromStruct(fc, nil))
}

// flowControlVarsFromStruct unpacks a flow_context snapshot into cel.Vars in
// the Soul-side shape — an exact mirror of
// soul/internal/runtime.flowControlVars (applyrunner.go), so static-when on
// Keeper binds the SAME names as evalWhen on Soul. register is passed
// separately (nil for static-when — a register-independent predicate).
// nil/missing sections → empty maps (a normal CEL no-such-key, not a panic).
func flowControlVarsFromStruct(flowCtx *structpb.Struct, register map[string]any) cel.Vars {
	fc := map[string]any{}
	if flowCtx != nil {
		fc = flowCtx.AsMap()
	}
	flowSection := func(key string) map[string]any {
		if sec, ok := fc[key].(map[string]any); ok {
			return sec
		}
		return map[string]any{}
	}
	return cel.Vars{
		Input:         flowSection("input"),
		Vars:          flowSection("vars"),
		Incarnation:   flowSection("incarnation"),
		SoulprintSelf: flowSection(flowContextSelfKey),
		Register:      register,
		// AllowHosts intentionally false: NewFlowControl enforces soulprint.hosts isolation.
		//
		// ComputeScope stays ComputeAvailable, and that is not a claim that compute
		// is readable here: the flow-control env does not DECLARE the name
		// (cel.flowControlVars), so `compute.x` in a when: is an undeclared-reference
		// compile error from cel-go itself — already unambiguous, already naming the
		// namespace. Overriding it would replace that with our message for no gain.
		ComputeScope: cel.ComputeAvailable,
	}
}

// hasFlowControl reports whether the task has at least one non-empty
// flow-control predicate (when/changed_when/failed_when). Gates the second
// fail-closed layer (flowContextHostInvariant): without a predicate, Soul
// never reads flow_context, so its host-variance doesn't matter.
func hasFlowControl(task config.Task) bool {
	return task.When != "" || task.ChangedWhen != "" || task.FailedWhen != ""
}

// guardFlowControlHostInvariant rejects a host-variant flow-control
// predicate (when/changed_when/failed_when referencing soulprint.self) on a
// multi-host target. The pilot dispatch model hands out ONE RenderedTask
// (carrying the first host's flow_context) to the whole targeted group —
// such a predicate would silently evaluate against the first host's facts
// for everyone. Fail-closed: an explicit error about the pilot boundary
// instead of a silently wrong result.
//
// Single-host (len==1): flow_context.self is correct for the one host →
// soulprint.self in the predicate is fine (golden-path redis single-host).
// Multi-host with a host-INVARIANT predicate (register.*/input.*/vars.*/
// incarnation.*) → OK, one predicate for the whole group is correct.
//
// Generalized to all three fields at once: changed_when/failed_when will
// follow the same pattern next slice, this bug shouldn't get to repeat.
func guardFlowControlHostInvariant(task config.Task, targeted []*topology.HostFacts) error {
	if len(targeted) <= 1 {
		return nil
	}
	for _, p := range []struct{ kind, expr string }{
		{"when", task.When},
		{"changed_when", task.ChangedWhen},
		{"failed_when", task.FailedWhen},
	} {
		if reFlowControlSoulprint.MatchString(p.expr) {
			return fmt.Errorf(
				"render: task %q: %s %q - a host-variant flow-control predicate (soulprint.self) on a multi-host target is unsupported in the pilot - per-host dispatch is deferred (separate ADR)",
				task.Name, p.kind, p.expr)
		}
	}
	return nil
}

// paramsHostInvariant diffs two hosts' params for host-invariance,
// EXCLUDING the per-host-by-design core.file.rendered keys: template_content
// (injected once after the loop by injectTemplateContent) and
// render_context (per-host by construction — carries a specific host's
// self, templating.md §3.2). For every other key it's an exact proto diff
// (pilot restriction "one RenderedTask per task", see Render): a
// self-dependent TEMPLATE is legitimate (its context goes into per-host
// render_context, materialized in RenderedTask.RenderContextBySID and
// overlaid per-SID by ToProtoTasksForHost) — self-dependent OTHER params are
// not.
func paramsHostInvariant(a, b *structpb.Struct) bool {
	return proto.Equal(stripPerHostKeys(a), stripPerHostKeys(b))
}

// stripPerHostKeys returns a shallow copy of struct without the per-host
// keys (template_content/render_context). The source struct isn't mutated
// (the Fields map shares values read-only — sufficient for proto.Equal).
func stripPerHostKeys(s *structpb.Struct) *structpb.Struct {
	if s == nil || s.Fields == nil {
		return s
	}
	out := &structpb.Struct{Fields: make(map[string]*structpb.Value, len(s.Fields))}
	for k, v := range s.Fields {
		if k == paramTemplateContent || k == paramRenderContext {
			continue
		}
		out.Fields[k] = v
	}
	return out
}

// flowContextHostInvariant diffs two hosts' flow_context as the SECOND
// fail-closed layer (the first is guardFlowControlHostInvariant, on
// predicate text). A proto diff of the snapshots with only the `self` key
// subtracted.
//
// flow_context = {input, vars, incarnation, self}
// (buildFlowContext). input/vars/incarnation are host-INVARIANT by
// construction (shared run context); self is ALWAYS host-VARIANT (per-host
// facts) and already covered by the predicate-text regex guard, so it's
// excluded here. That leaves vars: task-level `vars:` CAN be host-variant
// (when a value derives from soulprint.self), and then the predicate text
// `vars.<key>` contains no "soulprint" — the regex guard misses it. This
// layer catches exactly that vars-laundering case.
//
// Invariant: register is never placed in flow_context (Soul builds it
// itself from prior tasks' results, see buildFlowContext); if that changes,
// it should also be excluded here (host-variant by nature, like self).
func flowContextHostInvariant(a, b *structpb.Struct) bool {
	return proto.Equal(stripSelfKey(a), stripSelfKey(b))
}

// stripSelfKey returns a shallow copy of struct without the `self` key
// (mirrors stripPerHostKeys, but cuts exactly one key). Source struct isn't
// mutated.
func stripSelfKey(s *structpb.Struct) *structpb.Struct {
	if s == nil || s.Fields == nil {
		return s
	}
	out := &structpb.Struct{Fields: make(map[string]*structpb.Value, len(s.Fields))}
	for k, v := range s.Fields {
		if k == flowContextSelfKey {
			continue
		}
		out.Fields[k] = v
	}
	return out
}

// extractParamsVars pulls the CEL-rendered params.vars value as
// map[string]any for render_context.vars (templating.md §3.2/§6).
// Missing/non-object → nil (buildRenderContext substitutes an empty map).
// Source has already been through renderParams, so this is just a field
// read.
func extractParamsVars(st *structpb.Struct) map[string]any {
	if st == nil || st.Fields == nil {
		return nil
	}
	v, ok := st.Fields[paramVars]
	if !ok {
		return nil
	}
	sv, ok := v.GetKind().(*structpb.Value_StructValue)
	if !ok {
		return nil
	}
	return sv.StructValue.AsMap()
}

// templateInputField is the root render_context field whose injection is
// conditional (Variant B, ADR-010 §3.2): `input` is placed only for
// templates that actually read `.input.*`.
const templateInputField = "input"

// usesFieldEngine is a lazily-built text/template Engine for detecting
// whether a template references a root field (tmpl.UsesRootField). Same
// allowlisted FuncMap as the Soul-side renderer (rendered.go) — the parser
// knows the legal functions, doesn't choke on calls to them.
// Stateless/thread-safe, built once (flowControlEngine pattern): the
// constructor doesn't depend on runtime state, but rebuilding the FuncMap
// per rendered task would be wasted work in the render hot path.
var (
	usesFieldEngineOnce sync.Once
	usesFieldEngineInst *tmpl.Engine
	usesFieldEngineErr  error
)

func usesFieldEngine() (*tmpl.Engine, error) {
	usesFieldEngineOnce.Do(func() {
		usesFieldEngineInst, usesFieldEngineErr = tmpl.New()
	})
	return usesFieldEngineInst, usesFieldEngineErr
}

// resolveTemplateUsesInput reads a core.file.rendered step's .tmpl content
// once (host-invariant path) and reports whether to inject the root `input`
// into render_context (Variant B, ADR-010 §3.2): true iff the template
// actually reads `.input.*` (AST detection via tmpl.UsesRootField —
// mentioning `.input` in literal text or a body comment doesn't count).
// Content is returned to the caller so injectTemplateContent doesn't read
// the file again.
//
// The path comes from resolved params (after vault-resolve, before CEL). In
// pilot it's a string literal; for a `${ … }` expression it's resolved via
// CEL in the keeper context (no soulprint) — the template path is
// host-invariant per the pilot contract, so the keeper context suffices. An
// inline template with no file (params.template_content set directly,
// params.template absent) → nothing to read: content="", detection falls
// back to the existing template_content.
//
// reader=nil while params.template is set is a handoff error (as in
// injectTemplateContent): Keeper isn't configured to deliver the content.
func (p *Pipeline) resolveTemplateUsesInput(in RenderInput, resolved map[string]any) (string, bool, error) {
	tv, hasPath := resolved[paramTemplate]
	if !hasPath {
		// inline template_content (no file): detect directly on it.
		cv, hasContent := resolved[paramTemplateContent]
		content, _ := cv.(string)
		if !hasContent || content == "" {
			return "", false, nil
		}
		uses, err := usesInputField(content)
		return content, uses, err
	}

	rel, ok := tv.(string)
	if !ok || rel == "" {
		// non-string/`${}` path: resolve via CEL in the keeper context.
		// The keeper context, but with register.hosts explicitly closed: this
		// resolves a HOST task's param, and the accessor is keeper-only (NIM-711).
		// The per-host pass would reject it a moment later anyway; closing it here
		// keeps the isolation from depending on that ordering, and stops a
		// cross-host value from choosing which template file gets read.
		kv := keeperVars(in)
		kv.RegisterHosts, kv.AllowRegisterHosts = nil, false
		st, err := renderParams(p.cel, map[string]any{paramTemplate: tv}, kv)
		if err != nil {
			return "", false, fmt.Errorf("resolving template path: %w", err)
		}
		rel = st.GetFields()[paramTemplate].GetStringValue()
		if rel == "" {
			return "", false, fmt.Errorf("%q must resolve to a non-empty path string, got %v", paramTemplate, tv)
		}
	}

	if in.Templates == nil {
		return "", false, fmt.Errorf("TemplateReader not configured - Keeper cannot deliver template content %q (RenderInput.Templates=nil)", rel)
	}
	data, err := in.Templates.Read(rel)
	if err != nil {
		return "", false, err
	}
	content := string(data)
	uses, derr := usesInputField(content)
	return content, uses, derr
}

// templateVarField is the root render_context field carrying
// template-derived values (`.vars.<name>`): the file-vars from vars.yml
// whose keys the template actually reads are placed here selectively.
const templateVarField = "vars"

// templateVarSubKeys returns the set of `.vars.<key>` subkeys the template
// actually reads (AST, tmpl.RootFieldSubKeys) — the basis for selective
// file-vars injection into render_context.vars. Empty content (inline with
// no file / not core.file.rendered) → empty set. A broken template → error
// (caller fails render_failed like Soul would on render).
func templateVarSubKeys(content string) (map[string]bool, error) {
	if content == "" {
		return nil, nil
	}
	engine, err := usesFieldEngine()
	if err != nil {
		return nil, fmt.Errorf("building the tmpl engine: %w", err)
	}
	return engine.RootFieldSubKeys(content, templateVarField)
}

// templateReadsWholeVars reports whether the template reads `.vars` AS A MAP
// (`index .vars "x"`, `range .vars`, `toYaml .vars`, `with .vars`) rather than
// through named subkeys. Targeted injection is keyed on the subkeys the AST can
// see, so such a template would otherwise render an empty map against a map we
// hold — a config file written with a section missing, and no error anywhere.
func templateReadsWholeVars(content string) (bool, error) {
	if content == "" {
		return false, nil
	}
	engine, err := usesFieldEngine()
	if err != nil {
		return false, fmt.Errorf("building the tmpl engine: %w", err)
	}
	return engine.UsesWholeRootField(content, templateVarField)
}

// referencedFileVars filters the resolved destiny locals (vars.yml) down to
// just the keys the template actually reads as `.vars.<key>` (keys). This
// way render_context.vars gets EXACTLY the needed file-vars, not the whole
// vars.yml: node-exporter gets bin_path, redis (reads task-var keys, not
// file-vars) gets nothing extra. Empty fileVars/keys → nil
// (buildRenderContext substitutes an empty `.vars` layer). Doesn't mutate
// its input.
func referencedFileVars(fileVars map[string]any, keys map[string]bool) map[string]any {
	if len(fileVars) == 0 || len(keys) == 0 {
		return nil
	}
	out := make(map[string]any, len(keys))
	for k := range keys {
		if v, ok := fileVars[k]; ok {
			out[k] = v
		}
	}
	return out
}

// usesInputField detects whether a template references the root `.input`
// via AST (tmpl.UsesRootField). A broken template → error (caller fails
// render_failed the same way Soul would on render).
func usesInputField(content string) (bool, error) {
	engine, err := usesFieldEngine()
	if err != nil {
		return false, fmt.Errorf("building the tmpl engine: %w", err)
	}
	return engine.UsesRootField(content, templateInputField)
}

// setRenderContext places the assembled render-context into params under
// the render_context key (structpb conversion of {vars,self,role};
// input is conditional).
func setRenderContext(st *structpb.Struct, rc map[string]any) error {
	rcStruct, err := structpb.NewStruct(rc)
	if err != nil {
		return fmt.Errorf("render_context → structpb: %w", err)
	}
	if st.Fields == nil {
		st.Fields = map[string]*structpb.Value{}
	}
	st.Fields[paramRenderContext] = structpb.NewStructValue(rcStruct)
	return nil
}

// StateOpEvaluators returns the pair of merge-time CEL evaluators that
// stateop.Merge and the trial mirror need, both bound to ctx and to the §7
// own-namespace fence for service ([WithVaultFence]).
//
// The pair is handed out together, and only this way, because merge is the LAST
// place a scenario can put a value into incarnation.state: a `${ vault(...) }` in
// a modify patch or a match predicate that ran outside the fence would write the
// platform's own derived secret into state as plaintext — precisely what
// `type: secret` exists to prevent. A method on Pipeline evaluating these
// expressions without a ctx would be that hole with a shorter name, so there
// isn't one.
//
// merge is called once per run and evaluates per element, so the fence's memo is
// shared across every element of that merge — the same point-in-time view the
// render pass gets.
//
// match — the identity predicate of an `add` element ([ADR-0084] §"add").
// Bindings: `elem` (the existing element) and `value` (the one being added,
// already rendered), both top-level CEL names via Vars.Loop. No other scenario
// context: identity is a pure function of elem+value (as migration-CEL is a pure
// function of state, ADR-019). An empty predicate never reaches it (merge
// compares deep-equal without CEL).
//
// opEval — modify/remove. Same bindings plus `key`/`value` for a map element.
// The run context is deliberately NOT here: a capture step's `match:`/`patch:` is
// an ordinary module param, so whatever it needs from `input.*`/`register.*` was
// already interpolated render-side and reaches merge as a literal. What is left
// to evaluate per element depends only on the element.
// boolOut=true → predicate (bool); boolOut=false → patch value (native).
//
// Both closures are stateless with respect to Pipeline (cel.Engine is
// thread-safe).
func (p *Pipeline) StateOpEvaluators(ctx context.Context, service string) (StateMatchFunc, StateOpEvalFunc) {
	ctx = WithVaultFence(ctx, service)

	// ComputeScope is out of scope on both closures, and it is not the same claim
	// as "the map is empty": merge runs after the param render, so `${ compute.x }`
	// in a match:/patch: was already substituted — a `compute.x` still standing in
	// the text is one the author meant to be read HERE, per element, where no run
	// context exists. Saying so at compile beats the no-such-key it used to give
	// about a name that was spelled correctly (NIM-619).
	match := func(predicate string, elem, value any) (bool, error) {
		vars := cel.Vars{
			Ctx:          ctx,
			Loop:         map[string]any{"elem": elem, "value": value},
			ComputeScope: cel.ComputeOutOfScopeStateMatch,
		}
		return evalBoolExpr(p.cel, "core.state.add.match", predicate, vars)
	}

	opEval := func(expr string, binds map[string]any, boolOut bool) (any, error) {
		vars := cel.Vars{Ctx: ctx, Loop: binds, ComputeScope: cel.ComputeOutOfScopeStateMatch}
		if boolOut {
			return evalBoolExpr(p.cel, "core.state.match", expr, vars)
		}
		return p.cel.EvalInterpolation(expr, vars)
	}

	return match, opEval
}

// guardPilotDSL rejects task keys outside pilot scope with an explicit
// [ErrUnsupportedDSL] instead of a silent skip. The config validator
// already guarantees structural correctness; this is the pilot's
// implementation boundary.
//
// apply: destiny is NOT rejected here — it expands via an isolated render
// pass (renderApplyDestiny, V2). include: is also not rejected as
// "out of pilot scope" — it's expanded before render (config.ExpandIncludes);
// if it still reaches render unexpanded, that's ErrUnexpandedInclude (an
// expansion bug). serial:/run_once: aren't rejected either (slice D):
// run_once trims the target in resolveTargets, serial computes wave width
// in DispatchPlan.
//
// loop: on a MODULE task (slice E1) isn't rejected — it expands in the
// render phase (renderLoopTask: one task → N RenderedTask over items).
// loop: on include/apply/block remains outside pilot scope (the config
// validator already rejects loop on a non-module task earlier; this is
// defense in depth for apply+loop that reached render).
//
// block: (pilot C1) isn't rejected here — it expands via render-time
// fan-out (renderBlockTask, like loop/apply:destiny). The guard remains for
// unexpanded include: and empty tasks.
//
// async: (ADR-0075, NIM-150) is no longer rejected — it is honoured, threaded
// into RenderedTask.Async for the Soul runner. `async:` on a block: is still
// deferred, but it is caught EARLIER, by the config validator
// (async_on_block_invalid), so there is nothing left to check here.
func guardPilotDSL(task config.Task, idx int) error {
	switch {
	case task.Apply != nil:
		// module == nil is fine for an apply task (discriminator is apply).
		// loop: on apply is deferred (slice E.later) — reject explicitly.
		if task.Loop != nil {
			return fmt.Errorf("%w: loop: on an apply task (task[%d] %q)", ErrUnsupportedDSL, idx, task.Name)
		}
		return guardApplierWhen(task, idx)
	case task.Include != nil:
		return fmt.Errorf("%w: (task[%d] %q)", ErrUnexpandedInclude, idx, task.Name)
	case task.Block != nil:
		// A block task is valid (pilot C1); module == nil is fine
		// (discriminator is block). loop: on block is still out of pilot
		// scope — the config validator already rejects loop on a non-module
		// task earlier (no defense-in-depth needed here, renderBlockTask
		// never looks at block.Loop).
		return nil
	case task.Module == nil:
		return fmt.Errorf("%w: task[%d] %q is not a module task", ErrUnsupportedDSL, idx, task.Name)
	}
	return nil
}

// guardApplierWhen rejects a `when:` on an apply: task that cannot be decided
// Keeper-side (NIM-245). Fail-closed, [ErrUnsupportedDSL].
//
// A static `when:` never reaches here — emitStaticWhenSkip runs BEFORE the
// guard and settles it: false collapses the whole applier into one skip
// placeholder, true renders normally. That is the working form and stays
// bit-for-bit.
//
// What is refused is the other form — a `when:` reading `register.*` or
// `soulprint.*`. It cannot be decided at render, and it cannot be pushed onto
// the group either: a child's flow context is built in the ISOLATED destiny env,
// where `input.` and `vars.` name different things than in the scenario
// the predicate was written in, so inheriting the text would evaluate a
// different question. Until NIM-245 the key was simply dropped and the destiny
// applied everywhere — including on hosts the author had gated off, which is
// writing configuration where it was refused, not a missed optimisation.
//
// Both alternatives already work and the message names them: `where:` for a
// host-variant condition (Keeper-side targeting, register- and
// soulprint-capable through Passage stratification) and `onchanges:`/`onfail:`
// for "only if that source changed/failed", which this same slice makes reach
// the group. This is the boundary ADR-056 already draws for cross-Passage
// `when:` (cross_passage_when_unsupported), one level up.
func guardApplierWhen(task config.Task, idx int) error {
	if task.When == "" || isStaticWhen(task.When) {
		return nil
	}
	return fmt.Errorf("%w: when: %q on an apply task (task[%d] %q) reads register/soulprint - an applier's condition is decided Keeper-side, before its destiny is rendered; use where: for a host-variant condition, or onchanges:/onfail: to depend on a source's outcome",
		ErrUnsupportedDSL, task.When, idx, task.Name)
}

// guardKeeperWhen rejects a `when:` on an `on: keeper` task that reads
// `register.*` or `soulprint.*` ([ADR-0084] F-D). Fail-closed,
// [ErrUnsupportedDSL].
//
// A static `when:` never reaches here, for the same reason it never reaches
// [guardApplierWhen]: emitStaticWhenSkip runs at the top of the task loop,
// BEFORE the IsKeeperTask branch, and settles it — false gates the task off,
// true renders it normally. That form works and stays bit-for-bit.
//
// The other form was accepted and then dropped on the floor. `when:` is a
// Soul-side predicate: it rides RenderedTask to a Soul runner, which evaluates
// it in its own flow-control sandbox (soulcompat.go negotiates
// CapabilityFlowControl over exactly this field) — and a keeper task never
// reaches a Soul. Nothing in the keeper executor reads `When`. So the file said
// the step was conditional and the step ran regardless; since [ADR-0084] the
// step that runs regardless is the one that writes state, which is why review is
// not a sufficient backstop for it.
//
// The condition belongs inside the value instead: a `${ cond ? a : b }` is
// evaluated at render, in the keeper env, where a PREVIOUS keeper task's
// register is bound (keeperVars/KeeperRegister — a host task's register is
// not). A step that must not run at all belongs on the Soul side, where `when:`
// is evaluated. soul-lint raises the same finding offline
// (`when_on_keeper_dynamic_unsupported`), so an author normally sees it before
// a run exists.
func guardKeeperWhen(task config.Task, idx int) error {
	if task.When == "" || isStaticWhen(task.When) {
		return nil
	}
	return fmt.Errorf("%w: when: %q on a keeper-side task (task[%d] %q) reads register/soulprint - `when:` is evaluated Soul-side and a keeper task never reaches a Soul, so the predicate would be ignored and the task would run anyway; put the condition inside the value (${ cond ? a : b }, which sees a previous keeper task's register), or move the step to the Soul side",
		ErrUnsupportedDSL, task.When, idx, task.Name)
}

// taskPassageAt returns top-level task i's passage index from the
// stratification plan (RenderInput.TaskPassage). nil plan or i out of range
// → 0 (N=1 / non-staged caller: Trial / Acolyte RenderForHost)
// — behavior is bit-for-bit unchanged from before staged-render. Treating
// out-of-range as 0 is fail-safe: an extra Passage-0 is safer than
// panicking on a length mismatch.
func taskPassageAt(plan []int, i int) int {
	if i < 0 || i >= len(plan) {
		return 0
	}
	return plan[i]
}

// stampPassage sets passage on every RenderedTask added by the current
// top-level task (tasks[from:]). One call at the end of each Render
// iteration stamps apply:destiny/loop descendants too (block is an atomic
// Passage unit, ADR-056), instead of spreading the stamping across
// branches.
func stampPassage(tasks []*RenderedTask, from, passage int) {
	if passage == 0 {
		return // zero-value is already set — leave it (fast path N=1).
	}
	for i := from; i < len(tasks); i++ {
		tasks[i].Passage = passage
	}
}

// compile-time check that *structpb.Struct implements proto.Message (used
// by proto.Equal). If the type changes, this breaks here, not at runtime.
var _ proto.Message = (*structpb.Struct)(nil)
