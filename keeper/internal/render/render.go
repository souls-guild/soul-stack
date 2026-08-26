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
// [ErrUnexpandedInclude]. `async:` (ADR-0075) is honoured too — threaded into
// RenderedTask for the Soul runner. What is left outside pilot scope is a key
// on a node that cannot carry it (loop:/async: on an apply:/keeper task,
// scenario orchestration inside a destiny) → [ErrUnsupportedDSL] (explicit
// error, not a silent skip).
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
// scenario layer — `loop:` on an apply: task, `loop:`/`async:` on a keeper-side
// task). Not a scenario-author error but a pilot
// implementation boundary: the caller distinguishes "unsupported in pilot" from
// "scenario broken" (symmetric to cel.ErrUnsupported). serial:/run_once: are no
// longer in scope — implemented (slice D); block: (C1), loop: (E1) and async:
// (ADR-0075) are also implemented and excluded (renderBlockTask /
// renderLoopTask / RenderedTask.Async). include: is
// excluded — it's expanded before render (config.ExpandIncludes), see
// ErrUnexpandedInclude. on: keeper is excluded — keeper-side tasks render in
// the keeper context (see resolveOn / renderKeeperTask).
var ErrUnsupportedDSL = errors.New("render: DSL construct outside the pilot scope")

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
// topology_failed / service_vars_failed / input_invalid / render_failed /
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
var ErrUnexpandedInclude = errors.New("render: include task reached render unexpanded")

// ErrAssertFailed — an assert task (ADR-009 amendment 2026-06-23) failed: at
// least one `that[]` predicate evaluated to false during the render phase.
// Render aborts BEFORE dispatch — no task reaches a Soul, the run never starts
// ("fail at model stage"). Not a render-author bug and not a pilot boundary but
// declared DSL semantics: the caller (scenario.run / trial) distinguishes an
// invariant failure from an internal error and reports the operator the
// message + predicate text.
var ErrAssertFailed = errors.New("render: assert failed")

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
// ServiceVars — the service's own vars (`<service>/vars/`, host-invariant,
// ADR-0082). They are the BOTTOM of the flat `vars.*` namespace: render layers
// the destiny's `vars.yml`, a `block:`'s and the task's own `vars:` over them,
// outermost first, so a task reads `vars.<key>` without knowing which layer
// supplied it. NOT forwarded in the destiny pass (renderApplyDestiny) — a
// destiny's `vars.*` is its own alone, and it sees the caller's values only via
// apply: input: (isolation, slice A). Register —
// register-context from already-executed tasks (register-name → payload),
// supplied by the orchestrator (.g) during per-task render; empty in pilot
// (cross-task chaining within Render is future work).
//
// RegisterByHost — per-host register-context accumulated from the run's
// TaskEvents AFTER the barrier (sid → register-name → payload). Read by the
// render of the NEXT Passage (staged-render, ADR-056), which is how
// `${ register.<task>.<field> }` reaches a later task's params. Empty/nil — a run
// whose earlier Passages registered nothing.
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
	ServiceVars    map[string]any
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
	// available in params/where/apply-input, including the params of a
	// `core.state.<verb>` capture; eval does NOT mutate it (CEL reads, doesn't
	// write — Variant A, ADR-009/010). INVARIANT WITHIN a Passage. A plan that
	// captures state has run.go re-read it at each Passage boundary ([ADR-0084]),
	// so `incarnation.state.*` on P+1 sees what a capture on P wrote; a plan with
	// no capture keeps the pre-run stateBefore for every Passage. nil → the
	// `state` key isn't declared (push/trial without State:
	// `incarnation.state.x` = no-such-key, backward-compat).
	State map[string]any

	// Ctx — request-scoped run context, threaded into the CEL vault() function
	// ([ADR-017]) for ReadKV cancellation/timeout. Set by [Pipeline.Render] from
	// its ctx argument (run.go sets it on RenderInput); inherited by the child
	// destiny pass (renderApplyDestiny). nil ⇒ vault()
	// reads with context.Background() (cel.Vars.Ctx semantics).
	Ctx context.Context

	// DestinyVarsResolved — resolved destiny-local `vars.yml` values (Variant A,
	// docs/destiny/vars.md), per host: sid → name→value. Filled ONCE per destiny
	// pass (renderApplyDestiny resolves vars.yml over destiny-env
	// input+soulprint.self+incarnation, isolated from scenario register/vars
	// and without visibility between vars), then used as the BASE `vars.*` layer
	// when rendering each destiny task (resolveTaskVars merges task-level
	// `vars:` on top — task overrides same-named file-vars). Invariant across
	// the pass's tasks (resolved not per-task). nil/empty for a host → base
	// `vars.*` is empty (scenario pass: file-vars don't participate). Key is the
	// host SID; a synthetic empty host (where: filtered out everyone) → key "".
	DestinyVarsResolved map[string]map[string]any

	// Compute — resolved scenario-level `compute:` variables (ADR-009 amendment
	// 2026-06-23): name→value, computed ONCE per run in the run-level context
	// (input/register/incarnation/vars — WITHOUT soulprint, a structural
	// host-invariance barrier). Filled by [Pipeline.resolveCompute] at the start
	// of [Pipeline.Render]; placed into every per-host context (hostVars) and into
	// the keeper-side context (keeperVars) as `compute.<name>`. NOT forwarded in the isolated
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
	// callers: Trial, Acolyte RenderForHost) — bit-for-bit behavior.
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
	// (push/trial/Acolyte — seal not needed, bit-for-bit behavior).
	// The pointer is shared across staged-render passages: paths accumulate
	// across all Passages of one run.
	Sealed *SealedSet

	// KeeperRegister — flat register bucket for keeper-side tasks of PREVIOUS
	// Passages (keeper→keeper register-chaining, staged-render, ADR-056). A
	// keeper task in the active Passage sees `register.<prev>.*` of keeper tasks
	// from past Passages (e.g. core.bootstrap.delivered reads the register of
	// core.cloud.created). The stage-loop (run.go) pours
	// keeperRegisterBucket(RegisterByHost) in here before the per-passage render
	// of the active Passage. nil/empty (P0, N=1, non-staged, host-only Passage) →
	// keeperVars degrades to the flat Register (backward-compat: trial/push/
	// others that only set Register see register the same way, bit-for-bit).
	//
	// ★ A HOST task reads this bucket too, as a union where its own per-host
	// bucket wins ([hostRegister], [ADR-0083] §5) — a declared secret is written
	// by a keeper-side task and consumed by a Soul-side one, so the isolation
	// ADR-056 Slice 2 introduced would cut that path. What Slice 2 guarded
	// against was the wholesale FALLBACK (an empty per-host bucket REPLACED by
	// this one, so `register.<name>` on a host silently resolved to keeper data);
	// a union with host precedence does not bring that back. Values here may be
	// `vault:` refs rather than plaintext ([ADR-0083] §6) — [Pipeline.Render]
	// resolves the run's own namespace before any CEL root is built.
	KeeperRegister map[string]any

	// Modules — plugin-manifest resolver for this render (NIM-228), used to
	// derive [RenderedTask.SecretOutput] for a non-core module ([ADR-0083] §8).
	// Core modules resolve without it. nil is tolerated and is not a silent
	// pass: the same resolver drives `plugin_params_unchecked` at load, which is
	// where an author is told the manifest did not resolve. Callers that never
	// see plugins (trial, push, pre-flight) leave it nil.
	Modules config.ModuleManifestResolver

	// sealedRegisters — DERIVED inside [Pipeline.Render], not supplied by the
	// caller (hence unexported): the names of keeper registers whose payload held
	// a declared secret resolved at the §6 boundary. Feeds
	// [cel.SealSources.SealedRegisters] so a cell reading `register.<name>.…` is
	// sealed and masked on the way out.
	sealedRegisters map[string]bool
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
	ID string

	// SecretOutput — names of the module's OUTPUT fields declared `secret: true`
	// in its manifest ([ADR-0083] §8), sorted. Derived at render from the
	// manifest by [config.SecretOutputFields] — the task author declares
	// nothing. Soul echoes the list onto the TaskEvent, and both sides mask
	// exactly these fields wherever the task's output is observable; the
	// register payload itself is left intact, or the next task could not read
	// what this one produced.
	//
	// Replaces the removed per-task `no_log:`, which was all-or-nothing and set
	// by the author rather than by the module that knows its own output shape.
	// Params are not covered here and do not need to be: their secret
	// provenance is tracked per cell by the seal ([ADR-010] §7.4, [Sealed]).
	SecretOutput []string
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
	// flow-control predicates' CEL context: { input, vars, incarnation,
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

	// Async — the DSL core `async:` (destiny/tasks.md §6, ADR-0075): the task is
	// fire-and-forget, Soul starts it in its own flow and the main loop moves on
	// without waiting. Keeper only threads the flag through — every barrier is
	// Soul-side, inside one ApplyRequest (an async flow never outlives its
	// Passage). false = an ordinary sequential task. config.Task.Async → proto
	// keeperv1.RenderedTask.Async.
	Async bool

	// RequireIdx / RequireAll — the DSL core `require:` (destiny/tasks.md §8),
	// the EXPLICIT barrier and the preferred way to depend on an async task.
	// RequireIdx holds the GLOBAL Index of each named source after resolveRequire
	// (Variant A, mirroring OnChangesIdx); RequireAll is the `require: all` form,
	// which names no source and waits for every async task started earlier in the
	// ApplyRequest. The two forms are mutually exclusive (the config validator
	// rejects a mixed list), so at most one is ever set.
	//
	// ★ RequireIdx is remapped global→local for the wire like OnChangesIdx, but
	// the sentinel means the OPPOSITE thing: for a requisite, an absent source
	// contributes false to a gate; for a barrier, an absent source is "nothing to
	// wait for" (it was filtered out by where: on this host, or lives in an
	// earlier Passage already closed on every host). A source in a LATER Passage
	// is not encodable at all — resolveRequire rejects that plan rather than
	// shipping a barrier Soul cannot honor.
	RequireIdx []int
	RequireAll bool

	// requireNames — register names of the task's `require:` list form, mirroring
	// onChangesNames/onFailNames: threaded through to the resolveRequire pass,
	// unused after turning into RequireIdx.
	requireNames []string

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

// RenderedOp — one `core.state.<verb>` capture after the Keeper-side CEL render
// (params already interpolated). Built by stateop.BuildOp from the dispatched
// task's params; stateop.Merge / the trial mirror apply it to incarnation.state
// ([ADR-0084]).
//
// Verb distinguishes how it applies:
//   - VerbSet — overwrite Field with Value;
//   - VerbAdd — idempotently add Value into the Field collection (map: by Key;
//     list: dedup by the Match predicate). OnConflict (skip|replace|error) —
//     policy for an identity collision;
//   - VerbAppend — append Value to the Field list without a dedup check;
//   - VerbModify — patch ALL elements of Field matching Match. Patch — a map of
//     path-in-element → CEL/literal, threaded through AS A TEMPLATE (not
//     evaluated): merge computes it per matched element via [StateOpEvalFunc]
//     (elem/key/value bindings);
//   - VerbRemove — delete ALL elements of Field matching Match;
//   - VerbUnset — delete Field outright.
//
// Match/Patch are threaded through AS A STRING/TEMPLATE: merge evaluates them
// per element (Keeper cannot evaluate them ahead of time — they depend on each
// state element). Everything else was interpolated by the ordinary param render
// before the task was dispatched.
//
// Expect — optional match-cardinality assert for modify/remove ([ADR-0084]).
// ""/any = no assert.
type RenderedOp struct {
	Verb       config.StateVerb
	Field      string
	Value      any
	Key        string
	Match      string
	OnConflict config.OnConflict

	Patch  map[string]any
	Expect config.Expect
}

// StateMatchFunc — evaluator for the identity match predicate of a list
// element in an add operation (see [Pipeline.StateOpEvaluators]). Passed into
// merge (scenario/trial) so it doesn't hold its own cel.Engine: the predicate
// `elem.sid == value.sid` is evaluated per existing element against elem
// (existing) / value (being added) bindings. Returns a bool "are identical".
type StateMatchFunc func(predicate string, elem, value any) (bool, error)

// StateOpEvalFunc — CEL evaluator for modify/remove at merge time (see
// [Pipeline.StateOpEvaluators]). Like [StateMatchFunc] it sees only the current
// collection element's bindings (binds — elem, or key/value for a map element):
// a capture step's `match:`/`patch:` is an ordinary module param, so anything it
// needed from the run context was interpolated render-side. Used per matched
// element: match predicate → bool (boolOut=true), patch value → any
// (boolOut=false).
type StateOpEvalFunc func(expr string, binds map[string]any, boolOut bool) (any, error)

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
