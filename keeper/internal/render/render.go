// Package render — Keeper-side render pipeline for the scenario runner
// (architect-recon slice .f). Orchestrates ADR-010 phases over one scenario run:
//
//	vault-resolve → input-validation → CEL-render → produces []RenderedTask + []DispatchPlan
//
// text/template-render for `.tmpl` files is NOT done here: per ADR-012(d) it
// happens Soul-side in `core.file.rendered`. Pipeline only carries literal
// template-content + CEL-rendered vars into task params (RawTemplate field);
// the actual text/template pass runs on the host.
//
// Pipeline reuses pilot packages: vault-resolve via
// `keeper/internal/vault.Client`, the CEL phase via `shared/cel.Engine`, host
// resolution (`on:`/`where:`) via the roster from `keeper/internal/topology`.
//
// Pilot DSL scope: sequential tasks + per-host fan-out + `apply: destiny`
// (isolated destiny render pass, V2 ADR-009 — destiny renders with its own
// input-scope, tasks merge into the shared plan) + `serial:`/`run_once:`
// (orchestration.md §2.2: run_once trims the target to the first host by SID,
// serial computes wave width in DispatchPlan — wave dispatch is done by the
// scenario-orchestrator). `block:` (C1) and `loop:` (E1) are implemented —
// expanded in the render phase into a flat RenderedTask list (renderBlockTask /
// renderLoopTask). `include:` is expanded BEFORE render (config.ExpandIncludes
// at the loader layer), render gets a flat list; an unexpanded include: →
// [ErrUnexpandedInclude]. Of the original trio, only `parallel:` remains
// outside pilot scope → [ErrUnsupportedDSL] (explicit error, not a silent
// skip).
//
// [ADR-010]: docs/adr/0010-templating.md
// [ADR-012]: docs/adr/0012-keeper-soul-grpc.md
package render

import (
	"context"
	"errors"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// ErrUnsupportedDSL — scenario uses a DSL construct outside pilot scope (in the
// scenario layer — `parallel:`). Not a scenario-author error but a pilot
// implementation boundary: the caller distinguishes "unsupported in pilot" from
// "scenario broken" (symmetric to cel.ErrUnsupported). serial:/run_once: are no
// longer in scope — implemented (slice D); block: (C1) and loop: (E1) are also
// implemented and excluded (renderBlockTask / renderLoopTask). include: is
// excluded — it's expanded before render (config.ExpandIncludes), see
// ErrUnexpandedInclude. on: keeper is excluded — keeper-side tasks render in
// the keeper context (see resolveOn / renderKeeperTask).
var ErrUnsupportedDSL = errors.New("render: DSL-конструкция вне pilot-объёма")

// KeeperTargetSID — synthetic "host" target for a keeper-side task (`on: keeper`,
// docs/keeper/modules.md). A keeper-side step has no hosts: it runs on the
// keeper instance itself. To fit the shared "one apply_runs row per
// (apply_id, sid)" model and the cross-host barrier (orchestration.md §7), a
// keeper task gets a single stable target-SID. Matches the `on: keeper` literal
// (docs/naming-rules.md): an apply_runs row with sid="keeper" is keeper-local
// execution, not a Soul. souls.sid = FQDN, so there's no collision with a real
// host (composite PK (apply_id, sid)).
const KeeperTargetSID = "keeper"

// RunSentinelSID — run-level terminal marker for apply_runs when a scenario run
// aborts BEFORE the dispatch phase and there are no real hosts (BAG-1,
// ADR-043/027/009). An early abort (no_hosts / scenario_load_failed /
// topology_failed / essence_failed / input_invalid / render_failed /
// keeper_dispatch_failed) never manages to insert a single apply_runs row:
// dispatch hasn't started yet. The Voyage awaiter
// (PgIncarnationAwaiter.pollOutcome) polls until all run rows reach terminal
// state and waits forever on an empty set → Voyage hangs. To guarantee every
// run has a terminal row even with an empty roster, the abort path inserts one
// sentinel apply_runs row with this SID and status=failed. Not to be confused
// with [KeeperTargetSID] (`on: keeper`, keeper-side execution of a real task):
// RunSentinelSID is NOT an executor, just a placeholder for "run closed without
// a single host". The non-FQDN form (`__run__`) guarantees no collision with a
// real souls.sid=FQDN (composite PK (apply_id, sid)).
const RunSentinelSID = "__run__"

// ErrUnexpandedInclude — render encountered an include task. include is
// expanded into a flat list BEFORE render (config.ExpandIncludes at the loader
// layer); its presence here is a programming error (expansion wasn't called),
// not a pilot boundary. A separate sentinel from ErrUnsupportedDSL: include IS
// supported, it just must arrive already expanded.
var ErrUnexpandedInclude = errors.New("render: include-задача дошла до render нераскрытой")

// ErrAssertFailed — an assert task (ADR-009 amendment 2026-06-23) failed: at
// least one `that[]` predicate evaluated to false during the render phase.
// Render aborts BEFORE dispatch — no task reaches a Soul, the run never starts
// ("fail at model stage"). Not a render-author bug and not a pilot boundary but
// declared DSL semantics: the caller (scenario.run / trial) distinguishes an
// invariant failure from an internal error and reports the operator the
// message + predicate text.
var ErrAssertFailed = errors.New("render: assert не прошёл")

// IncarnationMeta — factual incarnation fields available in CEL as
// `incarnation.<path>` ([ADR-010]). Pipeline expands them into a map for
// cel.Vars.Incarnation; host_count is auto-filled from the number of targeted
// hosts (used in scenario predicates like
// `size(register.x) < incarnation.host_count`, see add_user/main.yml).
type IncarnationMeta struct {
	Name           string
	Service        string
	ServiceVersion string
}

// RenderInput — input for one render pipeline run.
//
// Essence — the effective essence layer of the incarnation (host-invariant),
// available in CEL as `essence.<path>` in all scenario vars (params/where/when/
// loop items). NOT forwarded in the destiny pass (renderApplyDestiny) — destiny
// sees essence only via apply: input: (isolation, slice A). Register —
// register-context from already-executed tasks (register-name → payload),
// supplied by the orchestrator (.g) during per-task render; empty in pilot
// (cross-task chaining within Render is future work).
//
// RegisterByHost — per-host register-context accumulated from the run's
// TaskEvents AFTER the barrier (sid → register-name → payload). Used only by
// [Pipeline.RenderStateChanges] (`sets: ${ register.<task>.<field> }`, slice 2);
// the Render phase doesn't read it (there register is cross-task chaining,
// future work). Empty/nil — a run with no register: tasks (sets with no
// register references).
//
// Hosts — the run's roster (resolved by topology.Resolver): connected souls of
// the incarnation with last-reported soulprint. Pipeline applies per-task
// `on:`/`where:` over this roster itself (see dispatch.go).
//
// Destiny — destiny resolver for apply:destiny tasks (per-run: carries the
// destiny[] refs of the current service snapshot). nil → apply:destiny →
// [ErrUnsupportedDSL]. NOT forwarded in the destiny pass (renderApplyDestiny) —
// destiny doesn't do a nested apply:destiny in the pilot (guardDestinyTask
// rejects it).
//
// Templates — reader for the service snapshot's `.tmpl` files for the
// `core.file.rendered` step (two-level resolve scenario-local→service-level,
// ADR-009). Pipeline reads the literal template content through it and puts it
// into `params.template_content` (text/template is NOT executed here — that's
// Soul-side). nil → a core.file.rendered task referencing a template → error
// (handoff not configured). In the destiny pass (renderApplyDestiny), swapped
// for the destiny snapshot reader — its `.tmpl` files live in its own snapshot,
// not the service snapshot.
type RenderInput struct {
	Scenario       *config.ScenarioManifest
	Essence        map[string]any
	Input          map[string]any
	Register       map[string]any
	RegisterByHost map[string]map[string]any
	Incarnation    IncarnationMeta
	Hosts          []*topology.HostFacts
	Destiny        DestinyResolver
	Templates      TemplateReader

	// State — snapshot of incarnation.state at the moment the run's row-lock is
	// acquired (run.go: stateBefore = inc.State under FOR UPDATE). Read-only:
	// projected into CEL as `incarnation.state.<path>` (incarnationVars),
	// available in params/where/apply-input AND in the state_changes context;
	// eval does NOT mutate it (CEL reads, doesn't write — Variant A,
	// ADR-009/010). INVARIANT across all staged-render passages: renderIn is
	// reused on P>0 with the same State, so `incarnation.state.*` is identical
	// on P0 and P1+ (= pre-run stateBefore, NOT an intermediate state_changes
	// result). nil → the `state` key isn't declared (push/trial without State:
	// `incarnation.state.x` = no-such-key, backward-compat).
	State map[string]any

	// Ctx — request-scoped run context, threaded into the CEL vault() function
	// ([ADR-017]) for ReadKV cancellation/timeout. Set by [Pipeline.Render] from
	// its ctx argument and by the [Pipeline.RenderStateChanges] caller (run.go);
	// inherited by the child destiny pass (renderApplyDestiny). nil ⇒ vault()
	// reads with context.Background() (cel.Vars.Ctx semantics).
	Ctx context.Context

	// DestinyVarsResolved — resolved destiny-local `vars.yml` values (Variant A,
	// docs/destiny/vars.md), per host: sid → name→value. Filled ONCE per destiny
	// pass (renderApplyDestiny resolves vars.yml over destiny-env
	// input+soulprint.self+incarnation, isolated from scenario register/essence
	// and without visibility between vars), then used as the BASE `vars.*` layer
	// when rendering each destiny task (resolveTaskVars merges task-level
	// `vars:` on top — task overrides same-named file-vars). Invariant across
	// the pass's tasks (resolved not per-task). nil/empty for a host → base
	// `vars.*` is empty (scenario pass: file-vars don't participate). Key is the
	// host SID; a synthetic empty host (where: filtered out everyone) → key "".
	DestinyVarsResolved map[string]map[string]any

	// Compute — resolved scenario-level `compute:` variables (ADR-009 amendment
	// 2026-06-23): name→value, computed ONCE per run in the run-level context
	// (input/register/incarnation/essence — WITHOUT soulprint, a structural
	// host-invariance barrier). Filled by [Pipeline.resolveCompute] at the start
	// of [Pipeline.Render] and [Pipeline.RenderStateOps]; placed into every
	// per-host context (hostVars) and into the state_changes context
	// (stateChangesVars) as `compute.<name>`. NOT forwarded in the isolated
	// destiny pass (renderApplyDestiny) — destiny sees the compute result only
	// via apply.input (ADR-009 V2). nil ⇒ `compute.<name>` is a plain
	// no-such-key (scenario without compute:, bit-for-bit backward-compat).
	Compute map[string]any

	// destinyIsolated marks the isolated destiny pass (renderApplyDestiny).
	// Unexported: external callers (scenario-runner, trial) always run a
	// scenario pass (zero-value false → soulprint.hosts is available and
	// projected from Hosts). In the destiny pass, renderApplyDestiny sets true:
	// soulprint.hosts in destiny is an isolation violation (orchestration.md
	// §4.1), the projection isn't forwarded.
	destinyIsolated bool

	// TaskPassage — passage index (0-based) of each top-level task in the run
	// plan (staged-render, ADR-056; result of [Stratify]). Render stamps it onto
	// every emitted [RenderedTask] (and its apply:destiny/loop descendants) from
	// the originating task — the orchestrator (run.go) filters dispatch/barrier
	// by RenderedTask.Passage. nil → all tasks in Passage 0 (N=1 / non-staged
	// callers: Trial, Acolyte RenderForHost, CheckDrift) — bit-for-bit behavior.
	// Length must match the number of top-level tasks after ExpandIncludes
	// (caller guarantees Stratify runs over the same list).
	TaskPassage []int

	// ActivePassage — the Passage index the stage-loop is rendering and
	// dispatching RIGHT NOW (staged-render, ADR-056 §c.1). Tasks in future
	// Passages (TaskPassage[i] > ActivePassage) don't have an accumulated
	// register yet — their `where:`/`params:` that read register are NOT
	// resolved: Render emits a placeholder RenderedTask for them (correct
	// Index/Passage, params/target not computed) purely to keep index numbering
	// contiguous; the orchestrator doesn't dispatch them in this Passage
	// (filtered by Passage). Once their Passage becomes active, a repeat Render
	// with the accumulated register resolves them fully. nil TaskPassage →
	// ActivePassage is ignored (non-staged: all tasks render in Passage 0 as
	// before, bit-for-bit).
	ActivePassage int

	// Sealed — accumulator of sealed paths for the render run (seal /
	// sealed-paths, [ADR-010] §7.4). Render marks the path of any params cell
	// whose RAW `${ … }` value reads a secret source (secret-input/vault()/
	// transitively vars). The caller (scenario.run) creates [NewSealedSet], puts
	// it here, and after Render uses Sealed.Paths() for seal-aware masking of
	// observable channels (audit.MaskSecretsSealed). nil ⇒ collection is off
	// (push/trial/Acolyte/CheckDrift — seal not needed, bit-for-bit behavior).
	// The pointer is shared across staged-render passages: paths accumulate
	// across all Passages of one run.
	Sealed *SealedSet

	// KeeperRegister — flat register bucket for keeper-side tasks of PREVIOUS
	// Passages (keeper→keeper register-chaining, staged-render, ADR-056).
	// Deliberately ISOLATED from host-register: a keeper task in the active
	// Passage sees `register.<prev>.*` of keeper tasks from past Passages (e.g.
	// core.bootstrap.delivered reads register from core.cloud.created) — read
	// ONLY by [keeperVars]. Host tasks don't get register through it: the
	// host-fallback ([hostRegister]) stays on the flat [RenderInput.Register] so
	// a host doesn't accidentally read keeper-register when its per-host bucket
	// is empty in a mixed Passage. The stage-loop (run.go) pours
	// keeperRegisterBucket(RegisterByHost) in here before per-passage render of
	// the active Passage. nil/empty (P0, N=1, non-staged, host-only Passage) →
	// keeperVars degrades to the flat Register (backward-compat: trial/push/
	// others that only set Register see register the same way, bit-for-bit).
	KeeperRegister map[string]any
}

// RenderedTask — a task after the Keeper-side CEL render, an intermediate
// representation before assembling `proto/keeper/v1.ApplyRequest`.
//
// Differs from the proto type `keeperv1.RenderedTask`: this one has Index
// (position in scenario.tasks[], links to DispatchPlan and TaskEvent.task_idx)
// and Register (register-result name for chaining), which aren't in the wire
// contract — the orchestrator (.g) uses them for per-host dispatch and doesn't
// put them in proto.
//
// Params — CEL-rendered, already in `*structpb.Struct` form (direct fit for
// proto). For the `core.file.rendered` step, params carry the literal
// `template_content` (the read `.tmpl` content, ADR-012(d) variant A1: no proto
// changes); the `template` key (path) is removed from params — the Soul
// doesn't need it.
//
// RawTemplate — literal `.tmpl` content for `core.file.rendered` after the CEL
// phase, before the text/template pass (Soul-side); "" for other modules.
// Duplicates `params.template_content` as a typed field for
// orchestrator/diagnostics; the wire authority is `params.template_content`
// (A1).
type RenderedTask struct {
	Index    int
	Name     string
	Module   string
	Params   *structpb.Struct
	Register string

	// Passage — passage index (0-based) for staged-render (ADR-056), inherited
	// from the originating top-level task via RenderInput.TaskPassage. The
	// orchestrator (run.go stage-loop) dispatches and barriers tasks strictly by
	// Passage: an ApplyRequest carries only tasks of one Passage, its barrier
	// waits for terminal rows (apply_id, sid, passage=N). 0 = the only Passage
	// (N=1 / non-staged) — bit-for-bit as before staged-render.
	// apply:destiny/loop descendants inherit the parent's Passage (block is an
	// atomic Passage unit, ADR-056).
	Passage int
	// ID — stable task address from the DSL core `id:` (config.Task.ID, T1): an
	// alternative to register for addressing a task without capturing a
	// register result (register∪id, T1 forbids both at once). Orchestrator-only,
	// like Index — NOT part of the wire contract (keeperv1.RenderedTask): Soul
	// addresses tasks by task_idx, id is only needed for the Keeper-side
	// per-task result fold (changed_tasks, T3). Threaded from config.Task.ID
	// alongside Register.
	ID    string
	NoLog bool
	// Timeout — per-task hard limit for one Apply attempt (DSL core timeout:,
	// destiny/tasks.md §9), Soul Stack `duration` convention (Go duration "30s"
	// OR `<N>d` suffix); "" = no per-task limit. Format is validated at
	// destiny/scenario PARSE time (config validator); render only threads
	// config.Task.Timeout into proto keeperv1.RenderedTask.Timeout unchanged and
	// unvalidated (string-consistent with Task.Timeout/RetrySpec.Delay).
	Timeout     string
	RawTemplate string

	// When/ChangedWhen/FailedWhen — flow-control CEL predicates (ADR-012(d)),
	// threaded through AS CEL STRINGS (not evaluated by Keeper): they depend on
	// register.* — results of previous tasks, known only to the Soul during the
	// run. Soul evaluates them with the sandboxed shared/cel.NewFlowControl
	// engine. When gates BEFORE Apply (empty = unconditional); ChangedWhen/
	// FailedWhen override changed/failed AFTER Apply (Soul evaluates in
	// applyrunner.runTask). Source — config.Task.When/ChangedWhen/FailedWhen (CEL
	// strings as-is from the parsed task). → proto keeperv1.RenderedTask.
	When        string
	ChangedWhen string
	FailedWhen  string

	// Until/RetryCount/RetryDelay — DSL core retry: (destiny/tasks.md §9),
	// threaded through by Keeper AS-IS — the retry loop is enforced Soul-side
	// (applyrunner.runTaskWithRetry). Until — CEL predicate for exiting the loop
	// (same sandboxed engine as failed_when; evaluated on the Soul after each
	// attempt). RetryCount — max attempts including the first (0/1 = one
	// attempt). RetryDelay — pause between attempts, `duration` convention
	// (string like Timeout, not revalidated — format validated by
	// validateRetryField at parse time). Source — config.Task.Retry (nil → all
	// zero-value = one attempt).
	Until      string
	RetryCount int
	RetryDelay string

	// FlowContext — a literal per-host snapshot of the non-register part of the
	// flow-control predicates' CEL context: { input, vars, essence, incarnation,
	// self }. Same as what's built for rendering params (hostVars), MINUS
	// soulprint.hosts and loop. Soul reads it as DATA (binds soulprint.self ←
	// flow_context.self), doesn't do external lookups. Host-variant (self
	// per-host) — excluded from the per-host params host-invariance check (see
	// paramsHostInvariant). → proto keeperv1.RenderedTask.FlowContext.
	FlowContext *structpb.Struct

	// OnChangesIdx — indices of source tasks for the DSL core `onchanges:`
	// (destiny/tasks.md §8) after resolving register names to Index across the
	// whole run plan (resolveOnChanges, Variant A). nil/empty = unconditional
	// run; otherwise the task executes on the Soul only if at least one source
	// has register.changed == true. config.Task.OnChanges (names) → this slice
	// (indices) → proto keeperv1.RenderedTask.OnchangesIdx.
	OnChangesIdx []int

	// onChangesNames — register names of the source task's `onchanges:`,
	// threaded by renderTaskIter through to the final resolve pass
	// [Pipeline.Render] → resolveOnChanges. Unexported: names live only within
	// one render run before turning into OnChangesIdx; orchestrator/dispatch
	// only see indices (wire form). Unused after resolveOnChanges.
	onChangesNames []string

	// OnFailIdx — indices of source tasks for the DSL core `onfail:`
	// (destiny/tasks.md §8) after resolving register names to Index across the
	// whole run plan (resolveOnFail, Variant A — mirrors OnChangesIdx). nil/empty
	// = not an onfail task (no gating applied); otherwise the task executes on
	// the Soul only if at least one source has register.failed == true (rescue
	// semantics). config.Task.OnFail (names) → this slice (indices) → proto
	// keeperv1.RenderedTask.OnfailIdx.
	OnFailIdx []int

	// onFailNames — register names of the source task's `onfail:`, mirrors
	// onChangesNames: threaded by renderTaskIter through to the resolveOnFail
	// pass, unused after turning into OnFailIdx.
	onFailNames []string

	// AggregateOf — GLOBAL cross-cutting Index of ALL child destiny tasks of one
	// applier task (`apply:`+`register:`), whose rolled-up result THIS synthetic
	// terminal `core.noop.run` carries (orchestration.md §2.1.1, applier-register
	// materialization, Variant B). Emitted by renderApplyDestiny AFTER the child
	// tasks, only if the applier had a non-empty register: this task's Register
	// = the applier's register, so an external `onchanges:[<applier>]` /
	// `when: register.<applier>.changed` resolves to its Index (registerIndex
	// picks it up automatically — onchanges.go isn't touched).
	//
	// Soul (applyrunner.aggregateRegisterData) builds this task's register_data
	// NOT from its own ApplyEvent (noop trivially changed=false) but as
	// `changed=OR(registerByIdx[i].changed)`, likewise failed/timed_out over
	// these indices. Indices are REMAPPED global→local when assembling proto
	// (ToProtoTasks/remapRequisites), like OnChangesIdx — they address the local
	// position in the ApplyRequest.tasks[] slice. nil/empty = task doesn't
	// aggregate. → proto keeperv1.RenderedTask.AggregateOf. Stored as []int
	// (global Index, mirrors OnChangesIdx) — remapRequisites converts to
	// []int32-local when assembling proto.
	AggregateOf []int

	// RenderContextBySID — per-host variant of params.render_context for a
	// self-variant core.file.rendered task: host SID → its assembled
	// render_context (buildRenderContext, templating.md §3.2). Filled ONLY for
	// core.file.rendered and ONLY when render_context actually differs between
	// target hosts (self per-host, e.g. `{{ .self.network.primary_ip }}`).
	//
	// Why a separate field instead of one render_context in Params: the
	// per-host renderTaskIter loop builds the correct render_context for each
	// host, but only the first by SID goes into Params (golden-path / N=1
	// bit-for-bit). One `*RenderedTask` (pointer) is dispatched to EVERY host
	// (groupByHost/dispatchWave, claim Acolyte), so without per-host
	// materialization every host would get the first host's render_context — a
	// self-variant template would silently render with the first host's facts
	// (CORE bug).
	//
	// ToProtoTasksForHost(tasks, sid), when assembling the ApplyRequest for a
	// specific SID, overlays its variant on top of Params (single-key overlay of
	// render_context). nil / empty map / missing SID key → render_context comes
	// from Params (golden-path). ALL OTHER params stay under the host-invariance
	// check (paramsHostInvariant) — fail-closed for ordinary host-variant params
	// targets.
	//
	// Partial closure of open Q #25 (render_context.self ONLY); full per-host
	// dispatch of arbitrary params (Variant B) is deferred to a separate ADR.
	RenderContextBySID map[string]*structpb.Struct
}

// RenderedOp — one `state_changes` operation after the Keeper-side CEL render
// (value/key/match already computed, cross-host last-wins fold applied). An
// ordered list of these operations is returned by [Pipeline.RenderStateOps];
// scenario.mergeStateChanges / the trial mirror apply them to incarnation.state
// (orchestration.md §7, the new list-form state_changes grammar).
//
// Verb distinguishes how it applies:
//   - VerbSet — overwrite Field with Value;
//   - VerbAdd — idempotently add Value into the Field collection (map: by Key;
//     list: dedup by the Match predicate). OnConflict (skip|replace|error) —
//     policy for an identity collision;
//   - VerbModify — patch ALL elements of Field matching Match. Patch — a map of
//     path-in-element → CEL/literal, threaded through AS A TEMPLATE (not
//     evaluated): merge computes it per matched element via [StateOpEvalFunc]
//     (elem/key/value bindings + the scenario Context);
//   - VerbRemove — delete ALL elements of Field matching Match.
//
// foreach never reaches RenderedOp — it's expanded in the render phase into N
// RenderedOp (per collection element, with the as-name binding already
// substituted into Value/Patch/Match).
//
// Match/Patch are threaded through AS A STRING/TEMPLATE: merge evaluates them
// per element (Keeper doesn't evaluate them ahead of time — they depend on each
// state element). Value (and, for map, Key) are already cross-host folded
// last-wins by SID.
//
// Context — a per-RUN snapshot of the scenario context (input/register/
// incarnation/soulprint.self/essence/vars), last-wins by SID (output.md).
// Needed at merge time to evaluate modify-Match/Patch and remove-Match, which
// see the full sets context on top of the element bindings (ADR-057 §b). nil
// for set/add (their Value/Key are already computed render-side; add-Match is a
// pure function of elem+value, see [StateMatchFunc]).
//
// Expect — optional match-cardinality assert for modify/remove (ADR-057 §c).
// ""/any = no assert.
type RenderedOp struct {
	Verb       config.StateVerb
	Field      string
	Value      any
	Key        string
	Match      string
	OnConflict config.OnConflict

	Patch   map[string]any
	Expect  config.Expect
	Context map[string]any
}

// StateMatchFunc — evaluator for the identity match predicate of a list
// element in an add operation (see [Pipeline.EvalStateMatch]). Passed into
// merge (scenario/trial) so it doesn't hold its own cel.Engine: the predicate
// `elem.sid == value.sid` is evaluated per existing element against elem
// (existing) / value (being added) bindings. Returns a bool "are identical".
type StateMatchFunc func(predicate string, elem, value any) (bool, error)

// StateOpEvalFunc — CEL evaluator for modify/remove at merge time (see
// [Pipeline.EvalStateOpExpr]). Unlike [StateMatchFunc] (isolated elem/value for
// add dedup), here the predicate/value sees the FULL run-scenario context (ctx
// — a snapshot of input/register/incarnation/soulprint.self/essence/vars) PLUS
// the current collection element's bindings (binds — elem/key/value). Used per
// matched element: match predicate → bool (boolOut=true), patch value → any
// (boolOut=false). This way a modify-match `key == input.username` sees both
// key (the element) and input.* (the context).
type StateOpEvalFunc func(expr string, ctx, binds map[string]any, boolOut bool) (any, error)

// DispatchPlan — which hosts a task targets after resolving `on:`+`where:`.
// TaskIndex refers to RenderedTask.Index. TargetSIDs — a slice sorted by SID
// (run determinism, scenario/orchestration.md). Empty TargetSIDs — the task
// targets no hosts (where: filtered out everyone); not an error, the
// orchestrator skips such a task.
//
// SerialWidth — the `serial:` wave width (orchestration.md §2.2.1): number of
// hosts per wave (≤N), already computed from `serial: N | "<N>%"` against the
// target count (percent rounds up, minimum 1). 0 = `serial:` not set (whole
// target width in one wave). RunOnce is already applied to TargetSIDs (trimmed
// to one host, see resolveTargets), so there's no separate run_once: flag in
// the plan — it's expressed as a single-element TargetSIDs. serial: and
// run_once: are mutually exclusive (config validator), so SerialWidth>0 and
// len(TargetSIDs)==1-from-run_once never overlap.
type DispatchPlan struct {
	TaskIndex   int
	TargetSIDs  []string
	SerialWidth int

	// Keeper marks a keeper-side task (`on: keeper`, docs/keeper/modules.md):
	// executed LOCALLY on the keeper instance via the keeper-side core Registry,
	// NOT dispatched to a Soul. TargetSIDs for such a plan = [KeeperTargetSID]
	// (single synthetic target — the keeper instance). scenario-runner branches
	// execution on this flag (run.go::dispatchKeeperTasks). false → ordinary
	// Soul-side task.
	Keeper bool
}
