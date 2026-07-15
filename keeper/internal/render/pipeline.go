package render

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
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
// flat layer — plus `on: keeper` (renderKeeperTask). Outside pilot scope:
// parallel: → [ErrUnsupportedDSL]; unexpanded include: → [ErrUnexpandedInclude].
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
	ctx = cel.WithVaultMemo(ctx)
	in.Ctx = ctx // propagate to CEL vault() (ReadKV cancel/timeout + memo)

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

	// includeGroupKeep caches the conditional-include (group-drop) decision:
	// group id (Task.IncludeGroupID, set by ExpandIncludes) → keep/drop.
	// Computed once per group (include-when is host-invariant — static
	// input./essence./incarnation./vars.), all tasks in the group share the
	// outcome. Unconditional tasks (IncludeGroupID==0) never hit this map.
	includeGroupKeep := map[int]bool{}

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
		// emitStaticWhenSkip. IncludeGroupID!=0 (set by config.ExpandIncludes)
		// means this task's include was expanded under a static `when:`.
		// include-when is computed once per group (cached by IncludeGroupID) via
		// the same isStaticWhen/evalStaticWhen as static-when-skip. false → real
		// drop: continue without emitting a RenderedTask and without idx++
		// (index isn't reserved, the task disappears from the plan entirely) —
		// unlike emitStaticWhenSkip's placeholder-with-idx++. true → renders
		// normally. Safe: cross-file register of a dropped group is already
		// lint-forbidden (per-file validateTaskRefs + ErrUnexpandedInclude), so
		// an external onchanges can't reference it → resolveOnChanges never hits
		// ErrOnChangesUnknownRegister.
		if task.IncludeGroupID != 0 {
			keep, ok := includeGroupKeep[task.IncludeGroupID]
			if !ok {
				k, derr := p.evalIncludeWhen(in, task.IncludeWhen)
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
		// (TaskPassage==nil: Trial/Acolyte/CheckDrift) → passage is always 0 ==
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
		// guard. An inactive branch with unsupported DSL (`parallel:`/`block:`)
		// doesn't block the active one — its DSL is rejected only on
		// activation (per-action validation). Not masking a bug: the task is
		// physically never executed (parallel: is never reached).
		// isStaticWhen/staticWhenSkips are register-/soulprint-independent and
		// build flow_context from input/vars/essence/incarnation/self, not DSL
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
			dt, dp, derr := p.renderApplyDestiny(ctx, in, task.Apply, idx, targeted, width, task.Register)
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
			bt, bp, berr := p.renderBlockTask(ctx, in, task, idx, targeted)
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
		NoLog:    task.NoLog,
		Timeout:  task.Timeout,
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
	// evalWhen — built from input/vars/essence/incarnation/self, never from
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
			fileVars := referencedFileVars(fileVarsForHost(in, h), fileVarKeys)
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
		// flow_context (ADR-012(d)): per-host snapshot {input,vars,essence,
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
				"render: task %q даёт host-зависимые params (%s vs %s) — host-вариативность params вне pilot-объёма (per-host ApplyRequest — слой orchestrator-а)",
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
				"render: task %q: host-вариативный flow_context (vars, производный от soulprint.self) на multi-host таргете (%s vs %s) — fail-closed; per-host dispatch отложен (отдельный ADR)",
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
		return false, fmt.Errorf("render: include-when %q не статичен (register/soulprint) — group-drop требует статического предиката (ADR-009 amendment)", when)
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

// emitStaticWhenSkip is an early static-when placeholder-skip, run at the
// START of the task-iteration loop, before guardPilotDSL/guardDestinyTask
// (ADR-012(d), extending the static-when invariant). If `when:` is static
// (register-/soulprint-independent) and evaluates false, the task is gated
// off: emit skip placeholder(s), mutating tasks/plans/idx through pointers,
// and return skipped=true. The caller does `continue` without the guard and
// without rendering — so unsupported DSL (`parallel:`/`block:`) on an
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

	*tasks = append(*tasks, p.staticSkipPlaceholder(task, *idx, skip))
	*plans = append(*plans, DispatchPlan{TaskIndex: *idx})
	*idx++
	return true, nil
}

// staticSkipPlaceholder builds one skip placeholder for a task with a
// statically-false when: (Params=nil — render skipped; first host's
// flow_context; When/ID/Register/requisites passed through). Module is set
// when a module task is present (block/parallel-without-module → empty
// Module — placeholder is still valid, just never executed).
func (p *Pipeline) staticSkipPlaceholder(task config.Task, idx int, skip *structpb.Struct) *RenderedTask {
	rt := &RenderedTask{
		Index:          idx,
		Name:           task.Name,
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
	if task.Module != nil {
		rt.Module = task.Module.Module
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
	ctx = cel.WithVaultMemo(ctx) // per-pass vault() memo (assert pre-flight is its own pass)
	in.Ctx = ctx                 // assert.that[] may call vault() — propagate cancel/timeout + memo
	// compute: available in assert.that[] the same as in params/where (one
	// resolve, run-level context, no soulprint). Idempotent with
	// Render/RenderStateOps.
	computed, cerr := p.resolveCompute(in)
	if cerr != nil {
		return cerr
	}
	in.Compute = computed

	// includeGroupKeep — conditional-include (group-drop) decision cache,
	// mirroring [Render]: group id (Task.IncludeGroupID, set by
	// ExpandIncludes) → keep/drop, computed once per group. Without it, an
	// assert from a conditionally-included file (cluster.yml under `when:
	// input.redis_type=='cluster'`) would evaluate even under a mismatched
	// mode (a sentinel run → CEL no-such-key: shards), whereas Render/Trial
	// drop that group before the assert. This restores a single source of
	// truth: pre-flight applies include-when to asserts the same way
	// run-render does.
	includeGroupKeep := map[int]bool{}
	for i := range in.Scenario.Tasks {
		task := in.Scenario.Tasks[i]

		// Conditional-include group-drop — before IsAssertTask, as in
		// [Render]: a false group include-when physically excludes the task
		// from the plan, its assert is never evaluated.
		if task.IncludeGroupID != 0 {
			keep, ok := includeGroupKeep[task.IncludeGroupID]
			if !ok {
				k, derr := p.evalIncludeWhen(in, task.IncludeWhen)
				if derr != nil {
					return derr
				}
				keep = k
				includeGroupKeep[task.IncludeGroupID] = keep
			}
			if !keep {
				continue
			}
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
			return fmt.Errorf("%w: %s (предикат that[%d] %q вычислился в false)", ErrAssertFailed, assertMessage(task), i, pred)
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
		return fmt.Sprintf("assert %q не прошёл", task.Name)
	}
	return "assert-предикат не прошёл"
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
// executor ignores them (like Soul did before integrating them). register:
// is passed through — the keeper executor accumulates this task's register
// under KeeperTargetSID.
func (p *Pipeline) renderKeeperTask(ctx context.Context, in RenderInput, task config.Task, idx int) (*RenderedTask, error) {
	if task.Apply != nil {
		return nil, fmt.Errorf("%w: apply: на keeper-side задаче (task[%d] %q)", ErrUnsupportedDSL, idx, task.Name)
	}
	if task.Loop != nil {
		return nil, fmt.Errorf("%w: loop: на keeper-side задаче (task[%d] %q)", ErrUnsupportedDSL, idx, task.Name)
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
		NoLog:          task.NoLog,
		Timeout:        task.Timeout,
		When:           task.When,
		ChangedWhen:    task.ChangedWhen,
		FailedWhen:     task.FailedWhen,
		onChangesNames: task.OnChanges,
		onFailNames:    task.OnFail,
	}
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
// (input/vars/essence/incarnation), and its false outcome is the same on
// every host of the run.
//
// Reuses the canonical parsers (no regex duplication):
//   - config.ExtractRegisterRefs (shared/config/task_refs.go) — register
//     refs; any register.<name> (except register.self, which has no gating
//     semantics in when) makes when register-dependent → not static;
//   - reFlowControlSoulprint (guardFlowControlHostInvariant) — soulprint is
//     host-variant (soulprint.self), excluded from "static".
//
// Empty when → false (no predicate — nothing for Keeper to evaluate; the
// task is unconditional, goes through the normal params-render path). A
// mixed when (register+input) → not static (has a register ref) — stays
// Soul-side.
func isStaticWhen(when string) bool {
	// Assumption: register dependence is only detected via dot form
	// register.<name> (ExtractRegisterRefs). Bracket form register["x"] in
	// when is unsupported — mirrors checkPredicateRefs in the config
	// validator. Latent for when in practice (probe-register is always dot
	// form).
	if when == "" {
		return false
	}
	if len(config.ExtractRegisterRefs(when)) != 0 {
		return false
	}
	if reFlowControlSoulprint.MatchString(when) {
		return false
	}
	return true
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
		return false, fmt.Errorf("static-when: сборка flow-control-движка: %w", err)
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
		Essence:       flowSection("essence"),
		Incarnation:   flowSection("incarnation"),
		SoulprintSelf: flowSection(flowContextSelfKey),
		Register:      register,
		// AllowHosts intentionally false: NewFlowControl enforces soulprint.hosts isolation.
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
// Multi-host with a host-INVARIANT predicate (register.*/input.*/essence.*/
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
				"render: task %q: %s %q — host-вариативный flow-control-предикат (soulprint.self) на multi-host таргете не поддержан в pilot — per-host dispatch отложен (отдельный ADR)",
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
// flow_context = {input, vars, essence, incarnation, self}
// (buildFlowContext). input/essence/incarnation are host-INVARIANT by
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
		st, err := renderParams(p.cel, map[string]any{paramTemplate: tv}, keeperVars(in))
		if err != nil {
			return "", false, fmt.Errorf("резолв пути шаблона: %w", err)
		}
		rel = st.GetFields()[paramTemplate].GetStringValue()
		if rel == "" {
			return "", false, fmt.Errorf("%q должен резолвиться в непустую строку-путь, получено %v", paramTemplate, tv)
		}
	}

	if in.Templates == nil {
		return "", false, fmt.Errorf("TemplateReader не сконфигурирован — Keeper не может доставить содержимое шаблона %q (RenderInput.Templates=nil)", rel)
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
		return nil, fmt.Errorf("сборка tmpl-движка: %w", err)
	}
	return engine.RootFieldSubKeys(content, templateVarField)
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
		return false, fmt.Errorf("сборка tmpl-движка: %w", err)
	}
	return engine.UsesRootField(content, templateInputField)
}

// setRenderContext places the assembled render-context into params under
// the render_context key (structpb conversion of {vars,self,role,essence};
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

// RenderStateChanges renders a scenario's `state_changes` into a
// field→value map for committing set operations (orchestration.md §7.1).
// Compatibility: returns the same flat map as before — a projection of ONLY
// the set operations (both the old map form `sets:` and the new list form
// `- set:`). add operations aren't in this projection: they need ordered
// application against intermediate state (see [Pipeline.RenderStateOps] /
// scenario.mergeStateChanges). Kept for the trial assertion
// `assert.state_changes` (field→value) and existing state-merge unit tests.
//
// Implemented on top of RenderStateOps: renders the whole ordered list, then
// projects the set operations into a map (later entries overwrite earlier
// ones on the same field).
func (p *Pipeline) RenderStateChanges(in RenderInput) (map[string]any, error) {
	ops, err := p.RenderStateOps(in)
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, len(ops))
	for i := range ops {
		if ops[i].Verb == config.VerbSet {
			out[ops[i].Field] = ops[i].Value
		}
	}
	return out, nil
}

// RenderStateOps renders a scenario's ordered `state_changes` operation
// list (orchestration.md §7, the new list form) into []RenderedOp with
// Keeper-side values already computed. Called after the barrier (run.go),
// separately from Render.
//
// CEL context: input/incarnation/soulprint.self + this host's register
// (slice 2: register from the run's probe tasks, in.RegisterByHost[sid]);
// vars/essence/state/soulprint.hosts belong to the future full grammar, not
// available in pilot.
//
// Cross-host folding is last-wins by SID order: each operation's Value
// (and, for map-add, Key) is evaluated on every host in SID order, the
// later one overwrites the earlier (determinism, output.md). The
// list-dedup match predicate is NOT evaluated here — it's passed through as
// a string and applied at merge time per existing element (depends on each
// state element, not on the host).
//
// Two StateChanges forms:
//   - list form (IsList): operations from sc.Ops in declaration order;
//   - map form (DEPRECATED): sc.Sets → set operations (order is
//     nondeterministic, but set field-overwrite semantics don't depend on
//     order — last-wins per field).
//
// nil/empty block → nil. Empty in.Hosts → nil (nothing to evaluate against;
// caller run.go already rejects a run with no hosts).
func (p *Pipeline) RenderStateOps(in RenderInput) ([]RenderedOp, error) {
	if in.Scenario == nil {
		return nil, fmt.Errorf("render: scenario manifest is nil")
	}
	sc := in.Scenario.StateChanges
	if sc == nil {
		return nil, nil
	}

	hosts := sortedHostsBySID(in.Hosts)
	if len(hosts) == 0 {
		return nil, nil
	}

	// compute: resolved the same way as in Render (once, run-level context,
	// no soulprint) — RenderStateOps is called separately after the barrier
	// (run.go) with the same RenderInput but no preceding Render pass.
	// Idempotent: if the caller already populated in.Compute, resolveCompute
	// returns it unchanged. So `compute.<name>` in state_changes gets the
	// SAME value as in apply.input (compute removes that drift risk).
	computed, cerr := p.resolveCompute(in)
	if cerr != nil {
		return nil, cerr
	}
	in.Compute = computed

	if sc.IsList {
		return p.renderStateOpsList(in, hosts, sc.Ops)
	}
	return p.renderStateOpsLegacy(in, hosts, sc.Sets)
}

// renderStateOpsLegacy renders the old map form `sets:` into set operations.
// Each field is a CEL expression, last-wins across hosts. Field names are
// sorted deterministically for a stable operation order (set semantics
// don't depend on order, but determinism matters for logs/diffing).
func (p *Pipeline) renderStateOpsLegacy(in RenderInput, hosts []*topology.HostFacts, sets map[string]string) ([]RenderedOp, error) {
	if len(sets) == 0 {
		return nil, nil
	}
	fields := make([]string, 0, len(sets))
	for f := range sets {
		fields = append(fields, f)
	}
	sort.Strings(fields)

	out := make([]RenderedOp, 0, len(fields))
	for _, field := range fields {
		var val any
		for _, h := range hosts {
			v, err := p.cel.EvalInterpolation(sets[field], stateChangesVars(in, h))
			if err != nil {
				return nil, fmt.Errorf("render: state_changes.sets.%s (host %s): %w", field, h.SID, err)
			}
			val = v // last-wins by SID
		}
		out = append(out, RenderedOp{Verb: config.VerbSet, Field: field, Value: val})
	}
	return out, nil
}

// renderStateOpsList renders the new list form: each operation in order.
//
//   - set/add: Value (arbitrary YAML with CEL cells) and map-add Key render
//     per-host last-wins; the list-dedup Match is passed through as a
//     string (merge-time);
//   - modify/remove: Match/Patch pass through AS-IS (evaluated merge-time
//     per element — they depend on each state element). A per-run snapshot
//     of the scenario context (Context, last-wins by SID) is attached to
//     the RenderedOp — needed merge-time because match `key ==
//     input.username` / patch `${ input.acl }` need the full sets context
//     (ADR-057 §b);
//   - foreach: render-time fan-out — iterate the CEL collection's elements,
//     render each nested do operation with the `as` name bound. Expands
//     into N RenderedOp entries (element count × do-operation count).
func (p *Pipeline) renderStateOpsList(in RenderInput, hosts []*topology.HostFacts, ops []config.StateChange) ([]RenderedOp, error) {
	out := make([]RenderedOp, 0, len(ops))
	for i := range ops {
		op := ops[i]
		if op.Verb == config.VerbForeach {
			expanded, err := p.renderForeach(in, hosts, op, i, nil)
			if err != nil {
				return nil, err
			}
			out = append(out, expanded...)
			continue
		}
		ro, err := p.renderOneStateOp(in, hosts, op, i, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, ro)
	}
	return out, nil
}

// renderOneStateOp renders ONE non-foreach operation (set/add/modify/remove).
// loopBind is the current foreach iteration's binding (`as` name → element),
// nil outside foreach; it's mixed into the CEL context of every
// value/key/patch/match cell.
func (p *Pipeline) renderOneStateOp(in RenderInput, hosts []*topology.HostFacts, op config.StateChange, idx int, loopBind map[string]any) (RenderedOp, error) {
	ro := RenderedOp{Verb: op.Verb, Field: op.Field, Match: op.Match, OnConflict: op.OnConflict, Expect: op.Expect}

	// modify/remove: Match/Patch are evaluated merge-time per element. Here
	// we only take a per-run scenario-context snapshot (last-wins by SID) —
	// the foreach binding substitution into Patch strings is render-time,
	// see renderForeach.
	if op.Verb == config.VerbModify || op.Verb == config.VerbRemove {
		ctx := p.stateContextSnapshot(in, hosts, loopBind)
		ro.Context = ctx
		if op.Verb == config.VerbModify {
			patch, err := patchMapFromAny(op.Patch)
			if err != nil {
				return RenderedOp{}, fmt.Errorf("render: state_changes[%d] modify %q: %w", idx, op.Field, err)
			}
			ro.Patch = patch
		}
		// foreach binding in match/patch is resolved merge-time via Context
		// (loopBind is folded into the snapshot), match/patch stay strings. A
		// match using an `as` name (e.g. `elem.sid == replica`) sees replica
		// from Context.
		return ro, nil
	}

	// set/add: Value (recursive CEL) + map-add Key — per-host last-wins.
	var val any
	var key string
	for _, h := range hosts {
		vars := stateChangesVars(in, h)
		vars.Loop = mergeLoop(vars.Loop, loopBind)
		v, err := renderValue(p.cel, op.Value, vars, fmt.Sprintf("state_changes[%d].value", idx))
		if err != nil {
			return RenderedOp{}, fmt.Errorf("render: state_changes[%d] %s %q (host %s): %w", idx, op.Verb, op.Field, h.SID, err)
		}
		val = v // last-wins by SID
		if op.Key != "" {
			kv, kerr := p.cel.EvalInterpolation(op.Key, vars)
			if kerr != nil {
				return RenderedOp{}, fmt.Errorf("render: state_changes[%d].key %q (host %s): %w", idx, op.Key, h.SID, kerr)
			}
			key = fmt.Sprint(kv)
		}
	}
	ro.Value = val
	ro.Key = key
	// add inside foreach: the list-dedup match predicate may reference the
	// `as` name (ADR-057 add_replicas example: `match: "elem == sid"`). A
	// pure add-match (EvalStateMatch) only sees elem/value; to resolve `sid`
	// merge-time, attach the foreach binding as Context — merge picks the
	// context-aware evaluator when Context != nil (findListMatch). Outside
	// foreach, Context=nil → the original pure add-match elem/value.
	if op.Verb == config.VerbAdd && len(loopBind) > 0 {
		ro.Context = loopBind
	}
	return ro, nil
}

// renderForeach expands a foreach at render time: evaluates the CEL
// collection op.In, iterates its elements (list → as=element; map →
// as=key/value record), and renders every do operation for each element
// with the as-name bound. Expands into N×M RenderedOp (N elements × M do
// operations).
//
// Binding shape (ADR-057 §3, FIXED):
//   - foreach over a LIST → `as`=the element as-is. For a list of scalars,
//     `${replica}` yields the scalar; for a list of objects,
//     `${replica.sid}` reads an object field.
//   - foreach over a MAP → `as`={key, value} record: `${change.key}` is the
//     record's key, `${change.value.acl}` a field of the value. Mirrors the
//     register map (sid→payload) and the migration-DSL foreach over a map.
//
// The op.In collection is evaluated ONCE (host-invariant in pilot: foreach
// over input.*/vars.* is shared run context; foreach over
// soulprint.self.* would be host-variant and isn't supported here — falls
// back to the last host's snapshot by SID, same as the rest of the
// last-wins folding). nestedBind is the outer foreach's binding (nesting is
// grammar-forbidden, but the parameter exists for symmetry).
func (p *Pipeline) renderForeach(in RenderInput, hosts []*topology.HostFacts, op config.StateChange, idx int, nestedBind map[string]any) ([]RenderedOp, error) {
	// The collection is evaluated in the last host's context by SID (last-wins).
	last := hosts[len(hosts)-1]
	vars := stateChangesVars(in, last)
	vars.Loop = mergeLoop(vars.Loop, nestedBind)
	collVal, err := p.cel.EvalInterpolation(op.In, vars)
	if err != nil {
		return nil, fmt.Errorf("render: state_changes[%d].foreach %q: %w", idx, op.In, err)
	}

	binds, err := foreachBindings(op.As, collVal)
	if err != nil {
		return nil, fmt.Errorf("render: state_changes[%d].foreach %q: %w", idx, op.In, err)
	}

	out := make([]RenderedOp, 0, len(binds)*len(op.Do))
	for _, bind := range binds {
		merged := mergeLoop(nestedBind, bind)
		for di := range op.Do {
			sub := op.Do[di]
			ro, derr := p.renderOneStateOp(in, hosts, sub, idx, merged)
			if derr != nil {
				return nil, fmt.Errorf("render: state_changes[%d].foreach.do[%d]: %w", idx, di, derr)
			}
			out = append(out, ro)
		}
	}
	return out, nil
}

// foreachBindings builds per-iteration `as`-name bindings from the
// evaluated collection. list → as=element; map → as={key, value} record
// (ADR-057 §3). Map iteration order is deterministic (sorted keys) for
// reproducible state commits. Not list/not map (scalar/nil) → error:
// foreach requires a collection.
func foreachBindings(asName string, coll any) ([]map[string]any, error) {
	switch c := coll.(type) {
	case []any:
		out := make([]map[string]any, 0, len(c))
		for _, elem := range c {
			out = append(out, map[string]any{asName: elem})
		}
		return out, nil
	case map[string]any:
		keys := make([]string, 0, len(c))
		for k := range c {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]map[string]any, 0, len(c))
		for _, k := range keys {
			// as=key/value record: .key (string) + .value (the entry's value).
			out = append(out, map[string]any{asName: map[string]any{"key": k, "value": c[k]}})
		}
		return out, nil
	default:
		return nil, fmt.Errorf("foreach: выражение дало %T, ожидался list или map", coll)
	}
}

// patchMapFromAny coerces an arbitrary YAML patch value into map[string]any
// (element path → CEL/literal). nil → empty map (no-op merge). Non-map →
// error (the config validator already rejects this, defense in depth).
func patchMapFromAny(v any) (map[string]any, error) {
	if v == nil {
		return map[string]any{}, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("patch должен быть map путь→значение, получено %T", v)
	}
	return m, nil
}

// stateContextSnapshot builds a per-run scenario-context snapshot
// (last-wins by SID) as a flat map for merge-time evaluation of
// modify/remove match/patch. Contains input/register/incarnation/self/
// essence/vars plus the folded-in foreach binding (loopBind). Uses the last
// host's context by SID — register/self are host-variant, last-wins
// (output.md, mirrors set/add).
func (p *Pipeline) stateContextSnapshot(in RenderInput, hosts []*topology.HostFacts, loopBind map[string]any) map[string]any {
	last := hosts[len(hosts)-1]
	vars := stateChangesVars(in, last)
	ctx := map[string]any{}
	putIfSet := func(name string, m map[string]any) {
		if m != nil {
			ctx[name] = m
		}
	}
	putIfSet("input", vars.Input)
	putIfSet("register", vars.Register)
	putIfSet("incarnation", vars.Incarnation)
	putIfSet("essence", vars.Essence)
	putIfSet("vars", vars.Vars)
	putIfSet("compute", vars.Compute)
	if vars.SoulprintSelf != nil {
		ctx["soulprint"] = map[string]any{"self": vars.SoulprintSelf}
	}
	for k, v := range loopBind {
		ctx[k] = v
	}
	return ctx
}

// mergeLoop merges two loop bindings (outer + current) into a new map. The
// current one (b) wins over the outer (a) on name collisions. nil arguments
// are safe.
func mergeLoop(a, b map[string]any) map[string]any {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(map[string]any, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// EvalStateMatch evaluates the identity match predicate for a list element
// of an `add` operation (orchestration.md §7, new list form). Bindings:
// `elem` is the existing collection element, `value` the one being added
// (already rendered). Both are top-level CEL names (via Vars.Loop, the same
// mechanism as loop variables). No other scenario context
// (input/register/…) is available in match: identity is a pure function of
// elem+value (like migration-CEL is a pure function of state, ADR-019). An
// empty predicate never reaches here (merge with an empty match compares
// elements deep-equal, no CEL).
//
// The result must be bool (evalBoolExpr). Called by merge per existing
// element — stateless with respect to Pipeline (cel.Engine is thread-safe).
func (p *Pipeline) EvalStateMatch(predicate string, elem, value any) (bool, error) {
	vars := cel.Vars{Loop: map[string]any{"elem": elem, "value": value}}
	return evalBoolExpr(p.cel, "state_changes.add.match", predicate, vars)
}

// EvalStateOpExpr is the merge-time CEL evaluator for modify/remove (see
// [StateOpEvalFunc]). Unlike EvalStateMatch (isolated elem/value for
// add-dedup), expr here sees the FULL scenario run context (ctx — a
// snapshot of input/register/incarnation/soulprint.self/essence/vars, built
// by stateContextSnapshot) PLUS the current element's bindings (binds —
// elem/key/value). boolOut=true → match predicate
// (EvalExpression→bool); boolOut=false → patch value
// (EvalInterpolation→native). So a modify-match `key == input.username`
// sees both key (the element) and input.* (context).
//
// Called by merge (scenario/trial) per matched element; stateless with
// respect to Pipeline (cel.Engine is thread-safe).
func (p *Pipeline) EvalStateOpExpr(expr string, ctx, binds map[string]any, boolOut bool) (any, error) {
	vars := stateOpVars(ctx)
	vars.Loop = mergeLoop(vars.Loop, binds)
	if boolOut {
		return evalBoolExpr(p.cel, "state_changes.match", expr, vars)
	}
	return p.cel.EvalInterpolation(expr, vars)
}

// stateOpVars unpacks a flat ctx snapshot (stateContextSnapshot) back into
// cel.Vars for merge-time evaluation of modify/remove. Mirrors
// stateChangesVars, but the source is an already-built per-run snapshot
// (host resolution happened render-side), not topology. soulprint.self
// comes from the nested soulprint map.
func stateOpVars(ctx map[string]any) cel.Vars {
	asMap := func(k string) map[string]any {
		m, _ := ctx[k].(map[string]any)
		return m
	}
	v := cel.Vars{
		Input:       asMap("input"),
		Register:    asMap("register"),
		Incarnation: asMap("incarnation"),
		Essence:     asMap("essence"),
		Vars:        asMap("vars"),
		Compute:     asMap("compute"),
	}
	if sp, ok := ctx["soulprint"].(map[string]any); ok {
		if self, ok := sp["self"].(map[string]any); ok {
			v.SoulprintSelf = self
		}
	}
	// foreach binding (as-name) sits in ctx as a top-level key — move it into Loop.
	loop := map[string]any{}
	for k, val := range ctx {
		switch k {
		case "input", "register", "incarnation", "essence", "vars", "compute", "soulprint":
			continue
		}
		loop[k] = val
	}
	if len(loop) > 0 {
		v.Loop = loop
	}
	return v
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
// fan-out (renderBlockTask, like loop/apply:destiny). parallel: on block is
// still out of pilot scope (the task.Parallel case above catches a block
// with parallel:true before the block-accept case). The guard remains for
// parallel:, unexpanded include:, and empty tasks.
func guardPilotDSL(task config.Task, idx int) error {
	switch {
	case task.Apply != nil:
		// module == nil is fine for an apply task (discriminator is apply).
		// loop: on apply is deferred (slice E.later) — reject explicitly.
		if task.Loop != nil {
			return fmt.Errorf("%w: loop: на apply-задаче (task[%d] %q)", ErrUnsupportedDSL, idx, task.Name)
		}
		return nil
	case task.Include != nil:
		return fmt.Errorf("%w: (task[%d] %q)", ErrUnexpandedInclude, idx, task.Name)
	case task.Parallel:
		return fmt.Errorf("%w: parallel: (task[%d] %q)", ErrUnsupportedDSL, idx, task.Name)
	case task.Block != nil:
		// A block task is valid (pilot C1); module == nil is fine
		// (discriminator is block). loop: on block is still out of pilot
		// scope — the config validator already rejects loop on a non-module
		// task earlier (no defense-in-depth needed here, renderBlockTask
		// never looks at block.Loop).
		return nil
	case task.Module == nil:
		return fmt.Errorf("%w: task[%d] %q не является module-задачей", ErrUnsupportedDSL, idx, task.Name)
	}
	return nil
}

// taskPassageAt returns top-level task i's passage index from the
// stratification plan (RenderInput.TaskPassage). nil plan or i out of range
// → 0 (N=1 / non-staged caller: Trial / Acolyte RenderForHost / CheckDrift)
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
