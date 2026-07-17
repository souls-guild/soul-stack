package scenario

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// ConvergeScenarioName is the specialized scenario kind describing a service's
// DESIRED end state (ADR-031, Slice B). A service supporting drift detection
// MUST ship `scenario/converge/main.yml`: the Scry check runs it in `dry_run`
// mode (Soul calls `mod.Plan` instead of `mod.Apply`) and collects per-task
// `changed` into [DriftReport].
//
// Discovered by name (auto-discover, symmetric with other scenarios) — no new
// YAML field needed. Missing scenario → check-drift returns 422
// "drift check unsupported for this service" (informational, not an error).
const ConvergeScenarioName = "converge"

// ErrConvergeMissing — the incarnation's service has no
// `scenario/converge/main.yml` in its current git snapshot: drift check
// unsupported (ADR-031). Caller (REST/MCP handler) maps to 422
// `drift-unsupported`; incarnation untouched.
var ErrConvergeMissing = errors.New("scenario: converge scenario is missing — drift check unsupported")

// ErrDriftInputMissing — a converge parameter resolved to no value in either
// `incarnation.state.<param>` (auto-from-state by name convention) or the
// operator override; required with no default. Caller maps to 422.
var ErrDriftInputMissing = errors.New("scenario: converge input parameter cannot be resolved from state or override")

// DriftStatus is a host's terminal state in DriftReport (per-host aggregate of
// [DriftTaskResult]s).
type DriftStatus string

const (
	// DriftStatusClean — all host tasks returned `changed=false` (no drift).
	DriftStatusClean DriftStatus = "clean"
	// DriftStatusDrifted — at least one host task returned `changed=true`.
	DriftStatusDrifted DriftStatus = "drifted"
	// DriftStatusUnsupported — at least one host task is a community module
	// without `PlanReadSafe` capability: Soul returned `TASK_STATUS_FAILED`
	// with code `plan.unsupported` (default-deny, ADR-031). Set only when the
	// run has no drifted or failed tasks (failed takes priority — a real
	// error outranks "unsupported").
	DriftStatusUnsupported DriftStatus = "unsupported"
	// DriftStatusFailed — host ended in a non-success terminal
	// (failed/cancelled/orphaned/no_match) OR at least one task returned
	// FAILED with a code other than `plan.unsupported` (a real error). Drift
	// is undetermined, needs investigation.
	DriftStatusFailed DriftStatus = "failed"
)

// DriftTaskResult is the drift result of one task on one host. Filled after
// the barrier from [applyrun.SelectTaskRegistersByApplyID] (register_data with
// `changed`/`failed` per task_idx) + [applyrun.SelectByApplyID] (error_summary
// of the first failed task).
type DriftTaskResult struct {
	// Idx is the GLOBAL plan_index of the task (RenderedTask.Index, threaded
	// through the whole run plan across all Passages, ADR-056 §S1 fix Variant
	// B). Stable between Keeper-side render and Soul-side TaskEvent
	// (ADR-012(d)); unlike the local TaskEvent.task_idx it doesn't depend on
	// per-host where:.
	Idx int `json:"idx"`
	// Module is `<namespace>.<module>.<state>` from RenderedTask.Module (e.g.
	// `core.pkg.installed`).
	Module string `json:"module"`
	// Action is the task name from the scenario (RenderedTask.Name); empty if
	// the task has no `name:`.
	Action string `json:"action,omitempty"`
	// Changed is the final `changed` from register_data. true → drift on this
	// task. False for unsupported/failed tasks (Soul never reached the read).
	Changed bool `json:"changed"`
	// Message is an operator-facing description (empty for clean tasks).
	// Filled only for failed/unsupported from error_summary (`task <idx>
	// <module>: <message>`, already masked); empty for drifted (machine-
	// readable changed is enough for MVP — aggregating module output is
	// harder).
	Message string `json:"message,omitempty"`
}

// DriftHostReport is a per-host aggregate of drift results.
type DriftHostReport struct {
	SID    string            `json:"sid"`
	Status DriftStatus       `json:"status"`
	Tasks  []DriftTaskResult `json:"tasks"`
}

// DriftSummary aggregates per-host terminal states across the run. Either
// hosts_drifted/hosts_failed > 0 → incarnation.status moves to `drift` (or
// stays put, see CheckDrift).
type DriftSummary struct {
	HostsDrifted     int `json:"hosts_drifted"`
	HostsClean       int `json:"hosts_clean"`
	HostsUnsupported int `json:"hosts_unsupported"`
	HostsFailed      int `json:"hosts_failed"`
}

// DriftReport is the final Scry-check report (ADR-031, Slice B). Not the
// Keeper<->Soul proto — that wire form carries RunResult+PlanEvent.changed at
// the single-task level. This type is the keeper-internal aggregated view +
// API/MCP response.
type DriftReport struct {
	CheckedAt       time.Time         `json:"checked_at"`
	IncarnationName string            `json:"incarnation"`
	ScenarioRef     string            `json:"scenario_ref"`
	Hosts           []DriftHostReport `json:"hosts"`
	Summary         DriftSummary      `json:"summary"`
}

// CheckDriftSpec is the parameters of one check-drift run.
//
// InputOverride is operator-supplied converge parameter values (optional):
// overrides auto-from-state (`incarnation.state.<param>`) by name convention.
// nil → state only.
type CheckDriftSpec struct {
	ApplyID         string
	IncarnationName string
	ServiceRef      artifact.ServiceRef
	InputOverride   map[string]any
	StartedByAID    string
}

// CheckDrift runs a synchronous Scry drift check for an incarnation (ADR-031,
// on-demand pilot). Runs `scenario/converge/main.yml` from the service's
// current git snapshot in dry_run mode (Soul calls `mod.Plan` instead of
// `mod.Apply`), collects per-host per-task `changed`, and returns
// [DriftReport]. Does NOT change incarnation.status — the caller (REST/MCP
// handler) does that from the report summary.
//
// Flow (parity with [Runner.run] up to dispatch):
//  1. SelectByName + render pipeline: ServiceLoader → parseScenario(converge) →
//     ExpandIncludes → topology.LoadIncarnationHosts → essence.Resolve →
//     resolveDriftInput (state ∪ override merge before vault resolve) →
//     ResolveInputValuesVault → render.Pipeline.Render.
//  2. dispatch (work queue, ADR-027): InsertPlanned for EVERY roster host with
//     Recipe{DryRun:true} + Summons. Acolyte renders per-host and sends
//     `ApplyRequest{dry_run:true}` to Soul (claim.go passthrough).
//  3. driftBarrier: waits for terminal state on ALL planned hosts (any
//     terminal, including failed — unlike waitBarrier, drift mode doesn't
//     return err on failure). After the barrier, reads apply_task_register +
//     apply_runs and builds DriftReport via assembleDriftReport.
//
// Missing scenario.converge → [ErrConvergeMissing] (incarnation untouched,
// dispatch never starts — "drift check unavailable", not a failure).
// incarnation state is never changed by this method on any branch — the
// caller makes the transition to `drift`.
//
// Requires Acolyte mode: DryRun passthrough relies on persisted Recipe.DryRun,
// which the inline path (acolytes=0) doesn't forward into the proto. Explicit
// refusal when acolyteEnabled=false.
func (r *Runner) CheckDrift(ctx context.Context, spec CheckDriftSpec) (*DriftReport, error) {
	log := r.logger.With(
		slog.String("apply_id", spec.ApplyID),
		slog.String("incarnation", spec.IncarnationName),
		slog.String("scenario", ConvergeScenarioName),
	)

	ctx, span := tracer.Start(ctx, "scenario.check_drift",
		trace.WithAttributes(
			attribute.String("incarnation", spec.IncarnationName),
			attribute.String("scenario", ConvergeScenarioName),
			attribute.String("apply_id", spec.ApplyID),
		),
	)
	defer span.End()

	if !r.acolyteEnabled {
		// Inline path (acolytes=0) doesn't forward DryRun into ApplyRequest —
		// for check-drift that means a silent real apply with an actual
		// mutation. Fail-closed: explicit config refusal, not implicit
		// mutation. check-drift needs the work queue (Acolyte).
		span.SetStatus(codes.Error, "acolyte_required")
		return nil, fmt.Errorf("scenario: check-drift requires a work queue (keeper.acolytes>0, ADR-027); the inline path does not propagate dry_run")
	}

	inc, err := incarnation.SelectByName(ctx, r.deps.DB, spec.IncarnationName)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}

	// 1. Load service artifact + parse converge scenario. A missing file means
	//    "drift check unsupported" (ErrConvergeMissing), not a parse error:
	//    try ReadFile before LoadScenarioManifestFromBytes.
	art, err := r.deps.Loader.Load(ctx, spec.ServiceRef)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("scenario: check-drift load service: %w", err)
	}
	relMain := fmt.Sprintf(scenarioMainFile, ConvergeScenarioName)
	data, err := r.deps.Loader.ReadFile(art, relMain)
	if err != nil {
		// ReadFile returns a generic error (no typed sentinel like a wrapped
		// os.ErrNotExist), so any read failure is treated as converge
		// missing. A real IO error (bad perms/corrupt snapshot) also
		// collapses into ErrConvergeMissing, hence log.Warn — operators
		// should see a possible IO issue in logs, not just a silent
		// "converge undefined".
		log.Warn("scenario: check-drift - converge not read, the check is considered unavailable (possible IO issue)",
			slog.String("ref", spec.ServiceRef.Ref), slog.Any("error", err))
		return nil, ErrConvergeMissing
	}
	scn, _, diags, err := artifact.LoadScenarioManifestResolved(art, relMain, data)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("scenario: check-drift parse %s: %w", relMain, err)
	}
	if diag.HasErrors(diags) {
		err := fmt.Errorf("scenario: check-drift %s is invalid: %s", relMain, firstError(diags))
		span.RecordError(err)
		return nil, err
	}

	expanded, idiags := config.ExpandIncludes(scn.Tasks, scenarioIncludeResolver(r.deps.Loader, art, ConvergeScenarioName))
	if diag.HasErrors(idiags) {
		err := fmt.Errorf("scenario: check-drift include expansion in %s/%s: %s", ConvergeScenarioName, scenarioMainFile, firstError(idiags))
		span.RecordError(err)
		return nil, err
	}
	scn.Tasks = expanded

	// Synthesize install steps from modules[] (ADR-065), symmetric with run():
	// the drift plan must match the apply plan, or the synthesis step itself
	// would show up as permanent drift.
	if synthed, names := config.SynthesizeModuleInstalls(scn.Tasks, art.Manifest.Modules); len(names) > 0 {
		scn.Tasks = synthed
		log.Info("scenario: check-drift - synthesized module install steps from manifest.modules[] (ADR-065)",
			slog.Any("modules", names))
	}

	// 2. Roster + essence (same as run.go).
	hosts, err := r.deps.Topology.LoadIncarnationHosts(ctx, spec.IncarnationName)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("scenario: check-drift topology: %w", err)
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("scenario: check-drift incarnation %q has no connected hosts", spec.IncarnationName)
	}
	essenceMap, err := r.deps.Essence.Resolve(essenceInput(art.LocalDir, inc, hosts[0]))
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("scenario: check-drift essence: %w", err)
	}

	// 3. Resolve drift input (auto-from-state + override + vault). By name
	//    convention: for each converge schema parameter missing from override,
	//    check incarnation.state[<name>]; if absent there too, leave default/
	//    required resolution to the schema, which raises ErrDriftInputMissing.
	provided := resolveDriftInput(scn.Input, spec.InputOverride, inc.State)
	resolver := r.newInputVaultResolver(ctx, inputVaultAuditCtx{
		aid:         spec.StartedByAID,
		incarnation: spec.IncarnationName,
		scenario:    ConvergeScenarioName,
	}, r.deps.InputDenyPaths)
	effectiveInput, err := config.ResolveInputValuesVault(scn.Input, provided, resolver)
	if err != nil {
		// Required parameter didn't resolve — wrap in ErrDriftInputMissing so
		// the caller returns a clear 422 (distinct from other schema input
		// errors).
		if isInputRequiredErr(err) {
			return nil, fmt.Errorf("%w: %s", ErrDriftInputMissing, err.Error())
		}
		span.RecordError(err)
		return nil, fmt.Errorf("scenario: check-drift input %s/%s: %w", spec.IncarnationName, ConvergeScenarioName, err)
	}

	// 4. Render the full roster (same as run-goroutine).
	renderIn := render.RenderInput{
		Scenario: scn,
		Essence:  essenceMap,
		Input:    effectiveInput,
		Incarnation: render.IncarnationMeta{
			Name:           inc.Name,
			Service:        inc.Service,
			ServiceVersion: inc.ServiceVersion,
		},
		Hosts: hosts,
		// State is the incarnation.state snapshot for `incarnation.state.<path>`
		// (ADR-009/010). Symmetric with the run goroutine: a converge scenario
		// may read pre-run state; check-drift compares desired-vs-actual
		// through the same render pipeline. Read-only.
		State: inc.State,
		Ctx:   ctx,
		Templates: render.NewSnapshotTemplateReader(
			func(rel string) ([]byte, error) { return artifact.ReadSnapshotFile(art.LocalDir, rel) },
			scenarioTemplatePrefix(ConvergeScenarioName),
		),
	}
	if r.deps.Destiny != nil {
		renderIn.Destiny = r.deps.Destiny.resolverFor(art.Manifest)
	}
	tasks, _, err := r.deps.Render.Render(ctx, renderIn)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("scenario: check-drift render: %w", err)
	}

	// 5. Dispatch planned for EVERY roster host with DryRun=true. Recipe.Input
	//    carries vault refs AS-IS (invariant A); Acolyte re-resolves on claim
	//    the same way a normal run does.
	startedBy := startedByPtr(spec.StartedByAID)
	recipe := &applyrun.Recipe{
		ServiceRef:   spec.ServiceRef,
		ScenarioName: ConvergeScenarioName,
		Input:        spec.InputOverride, // override as given; no state-merge needed here (Acolyte re-reads state in RenderForHost)
		StartedByAID: startedBy,
		DryRun:       true,
		FromUpgrade:  false, // converge is an operational scenario/, never upgrade/ (ADR-0068)
	}
	for _, h := range hosts {
		if err := applyrun.InsertPlanned(ctx, r.deps.DB, &applyrun.ApplyRun{
			ApplyID:         spec.ApplyID,
			SID:             h.SID,
			IncarnationName: spec.IncarnationName,
			Scenario:        ConvergeScenarioName,
			StartedByAID:    startedBy,
			Recipe:          recipe,
		}); err != nil {
			span.RecordError(err)
			return nil, fmt.Errorf("scenario: check-drift insert planned (%s): %w", h.SID, err)
		}
	}
	r.publishSummons(ctx, log)

	// 6. driftBarrier: waits for terminal state on ALL planned hosts (any
	//    status — failed/no_match are terminal too). Drift mode doesn't abort
	//    on the first failure: a failed task isn't a run failure, just a host
	//    status in the report.
	if err := r.driftBarrier(ctx, spec.ApplyID, len(hosts)); err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("scenario: check-drift barrier: %w", err)
	}

	// 7. Assemble the report from persisted data: per-host status from
	//    apply_runs, per-task changed from apply_task_register, error_summary
	//    for failed hosts.
	report, err := r.assembleDriftReport(ctx, spec, tasks)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("scenario: check-drift assemble: %w", err)
	}
	log.Info("scenario: check-drift completed",
		slog.Int("hosts", len(report.Hosts)),
		slog.Int("hosts_drifted", report.Summary.HostsDrifted),
		slog.Int("hosts_failed", report.Summary.HostsFailed))
	return report, nil
}

// resolveDriftInput builds the provided-map for converge input by the
// "auto-from-state" convention: for each converge schema parameter,
//
//   - key in override → use override (operator takes priority);
//   - else key in state[<name>] → use state (typical case: converge reads
//     state rendered by a previous apply);
//   - else omit from provided (default/required merge is left to
//     ResolveInputValuesVault: default → fills it in, required → rejects).
//
// Override params absent from the schema pass through as-is (MVP grammar
// doesn't forbid unknown input keys, symmetric with ResolveInputValues).
func resolveDriftInput(schema config.InputSchemaMap, override, state map[string]any) map[string]any {
	out := make(map[string]any, len(override)+len(schema))
	for k, v := range override {
		out[k] = v
	}
	for name := range schema {
		if _, ok := out[name]; ok {
			continue
		}
		if state == nil {
			continue
		}
		if v, ok := state[name]; ok {
			out[name] = v
		}
	}
	return out
}

// isInputRequiredErr detects a "required parameter not provided" error from
// [config.ResolveInputValuesVault]. shared/config has no typed sentinel for
// this (see requireInputValues in shared/config/input_value.go); matches the
// exact message shape requireInputValues produces: `input "<name>" is
// required but was not provided and has no default`. That substring doesn't
// overlap other resolve errors (type/enum/pattern/min_length/max_length/
// object-required). Used by CheckDrift to distinguish a missing converge
// parameter (422 drift-input-missing) from other input errors.
func isInputRequiredErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "is required but was not provided and has no default")
}

// driftBarrier is the drift variant of [Runner.waitBarrier]: waits for
// terminal state on ALL planned hosts. Unlike the regular fail-stop
// waitBarrier, failed/cancelled/orphaned is NOT a run failure for drift (it's
// a per-host status reflected in DriftReport), so this never returns err on a
// non-success terminal. Returns only:
//   - nil — all wantHosts reached a terminal state (any);
//   - error — ctx cancelled (timeout/Cancel/Shutdown) or the poll query failed.
func (r *Runner) driftBarrier(ctx context.Context, applyID string, wantHosts int) error {
	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()
	for {
		statuses, err := applyrun.SelectStatusesByApplyID(ctx, r.deps.DB, applyID)
		if err != nil {
			return fmt.Errorf("poll: %w", err)
		}
		if isAllTerminal(statuses, wantHosts) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("interrupted: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// isAllTerminal reports whether all wantHosts run rows are in a terminal
// status (success/failed/cancelled/orphaned/no_match); planned/claimed/
// dispatched/running are not. Same terminal set as the regular barrier, minus
// fail-stop: failed doesn't end a drift check.
//
// InsertPlanned writes exactly one row per host: strict `==` here, since `>=`
// would mask bugs producing extra rows (e.g. a duplicate InsertPlanned).
func isAllTerminal(statuses []applyrun.HostStatus, wantHosts int) bool {
	terminal := 0
	for _, hs := range statuses {
		switch hs.Status {
		case applyrun.StatusSuccess, applyrun.StatusNoMatch,
			applyrun.StatusFailed, applyrun.StatusCancelled, applyrun.StatusOrphaned:
			terminal++
		}
	}
	return terminal == wantHosts
}

// assembleDriftReport builds a DriftReport from the run's persisted data after
// the barrier: per-host apply_runs (status + error_summary + failed_plan_index)
// and apply_task_register (changed/failed per plan_index).
//
// plan_index → module/action mapping comes from renderedTasks (Keeper-side
// render keeps RenderedTask with Module/Name/Index); the host list itself
// comes from apply_runs rows.
func (r *Runner) assembleDriftReport(ctx context.Context, spec CheckDriftSpec, tasks []*render.RenderedTask) (*DriftReport, error) {
	statuses, err := applyrun.SelectStatusesByApplyID(ctx, r.deps.DB, spec.ApplyID)
	if err != nil {
		return nil, fmt.Errorf("statuses: %w", err)
	}
	registers, err := applyrun.SelectTaskRegistersByApplyID(ctx, r.deps.DB, spec.ApplyID)
	if err != nil {
		return nil, fmt.Errorf("task registers: %w", err)
	}

	taskMeta := buildTaskMetaIndex(tasks)
	registerBySID := groupRegistersBySID(registers)
	taskFailures := buildTaskFailureMap(statuses)

	hostReports := make([]DriftHostReport, 0, len(statuses))
	for _, hs := range statuses {
		hr := buildHostReport(hs, taskMeta, registerBySID[hs.SID], taskFailures[hs.SID])
		hostReports = append(hostReports, hr)
	}
	sort.Slice(hostReports, func(i, j int) bool {
		return hostReports[i].SID < hostReports[j].SID
	})

	return &DriftReport{
		CheckedAt:       time.Now().UTC(),
		IncarnationName: spec.IncarnationName,
		ScenarioRef:     ConvergeScenarioName,
		Hosts:           hostReports,
		Summary:         summarize(hostReports),
	}, nil
}

// taskMetaIndex maps plan_index → {module, action} from RenderedTask (keyed by
// RenderedTask.Index, the GLOBAL index threaded through the whole plan,
// ADR-056 §S1 fix Variant B). Stable after expand-include (RenderedTask.Index
// = position in the full run plan).
type taskMetaIndex map[int]taskMeta

type taskMeta struct {
	module string
	action string
}

func buildTaskMetaIndex(tasks []*render.RenderedTask) taskMetaIndex {
	out := make(taskMetaIndex, len(tasks))
	for _, t := range tasks {
		if t == nil {
			continue
		}
		out[t.Index] = taskMeta{module: t.Module, action: t.Name}
	}
	return out
}

func groupRegistersBySID(rs []applyrun.TaskRegister) map[string][]applyrun.TaskRegister {
	out := make(map[string][]applyrun.TaskRegister)
	for _, r := range rs {
		out[r.SID] = append(out[r.SID], r)
	}
	return out
}

// hostTaskFailure describes a host's failed task (for unsupported/failed
// differentiation). Sourced from apply_runs.failed_plan_index + error_summary
// (written by recordTaskFailure on a FAILED TaskEvent). error_summary is
// formatted as `task <idx> <module>: <message>`, carrying the machine-readable
// `plan.unsupported` identifier in the message text.
//
// planIndex is the GLOBAL plan_index of the failed task (apply_runs.
// failed_plan_index, migration 081, ADR-056 §S1 fix Variant B): the key used
// to resolve module/action against taskMeta (built from RenderedTask.Index).
// NOT the local task_idx, which under staged/per-host where: would point at
// the wrong task.
type hostTaskFailure struct {
	planIndex int
	summary   string
}

func buildTaskFailureMap(statuses []applyrun.HostStatus) map[string]hostTaskFailure {
	out := make(map[string]hostTaskFailure, len(statuses))
	for _, hs := range statuses {
		// failed_plan_index is the global key for module/action resolution.
		// An old Soul without plan_index / a pre-migration-081 run falls back
		// to the local task_idx (identical for N=1, bit-for-bit behavior).
		// Neither present (dispatch-level failure with no TaskEvent) → no
		// failure entry built.
		if hs.ErrorSummary == nil {
			continue
		}
		idx, ok := failedPlanIndex(hs)
		if !ok {
			continue
		}
		out[hs.SID] = hostTaskFailure{planIndex: idx, summary: *hs.ErrorSummary}
	}
	return out
}

// failedPlanIndex picks the GLOBAL plan_index of a host's failed task:
// failed_plan_index (migration 081) takes priority; falls back to the local
// task_idx (identical to global for N=1) when absent (old Soul without echoed
// plan_index, or a pre-081 run). (false, _) means no failed task was recorded.
func failedPlanIndex(hs applyrun.HostStatus) (int, bool) {
	if hs.FailedPlanIndex != nil {
		return *hs.FailedPlanIndex, true
	}
	if hs.TaskIdx != nil {
		return *hs.TaskIdx, true
	}
	return 0, false
}

// buildHostReport assembles a per-host aggregate: per-task results + overall
// DriftStatus, fail-closed priority:
//
//   - failed/cancelled/orphaned, TaskError != plan.unsupported → DriftStatusFailed;
//   - failed/cancelled/orphaned, TaskError = plan.unsupported → DriftStatusUnsupported;
//   - success with any register changed=true → DriftStatusDrifted;
//   - success with all register changed=false → DriftStatusClean;
//   - no_match → DriftStatusClean (host out of scope, symmetric with a regular run).
func buildHostReport(hs applyrun.HostStatus, taskMeta taskMetaIndex, registers []applyrun.TaskRegister, failure hostTaskFailure) DriftHostReport {
	results := make([]DriftTaskResult, 0, len(registers)+1)
	hasDrifted := false
	for _, reg := range registers {
		// Correlate by GLOBAL plan_index (ADR-056 §S1 fix Variant B): taskMeta
		// is built from RenderedTask.Index (global, threaded through the whole
		// plan), while reg.TaskIdx is a LOCAL position within the host's
		// ApplyRequest Passage (not unique across Passages or across hosts of
		// the same Passage under per-host where:). Resolve and set Idx from
		// PlanIndex (the same global index), or module/action end up
		// mislabeled.
		meta := taskMeta[reg.PlanIndex]
		changed, _ := boolField(reg.RegisterData, "changed")
		if changed {
			hasDrifted = true
		}
		results = append(results, DriftTaskResult{
			Idx:     reg.PlanIndex,
			Module:  meta.module,
			Action:  meta.action,
			Changed: changed,
		})
	}

	// Failed task (host in failed/cancelled/orphaned): add an explicit Tasks
	// entry with Changed=false and Message from error_summary — gives the
	// operator a diagnostic point (`tasks[*].message`) instead of a bare
	// `status: failed` at the host level.
	if hs.Status == applyrun.StatusFailed || hs.Status == applyrun.StatusCancelled || hs.Status == applyrun.StatusOrphaned {
		if failure.summary != "" {
			// Resolve module/action by GLOBAL plan_index (ADR-056 §S1 fix
			// Variant B): taskMeta is keyed by RenderedTask.Index (global),
			// failure.planIndex is apply_runs.failed_plan_index (echoed
			// TaskEvent.plan_index). The local task_idx would mislabel under
			// staged/per-host where — symmetric with the register branch
			// above (reg.PlanIndex), fixed by migration 079.
			meta := taskMeta[failure.planIndex]
			results = append(results, DriftTaskResult{
				Idx:     failure.planIndex,
				Module:  meta.module,
				Action:  meta.action,
				Changed: false,
				Message: failure.summary,
			})
		}
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Idx < results[j].Idx })

	status := classifyHostStatus(hs.Status, failure.summary, hasDrifted)
	return DriftHostReport{
		SID:    hs.SID,
		Status: status,
		Tasks:  results,
	}
}

// classifyHostStatus derives DriftStatus from the per-host apply_runs status +
// the "plan.unsupported" signal in the first failed task's error_summary.
// `plan.unsupported` is the stable TaskError.Code Soul-side planTask sets for
// a module without PlanReadSafe capability (see
// soul/internal/runtime/plantask.go).
func classifyHostStatus(applyStatus applyrun.Status, failureSummary string, hasDrifted bool) DriftStatus {
	switch applyStatus {
	case applyrun.StatusSuccess:
		if hasDrifted {
			return DriftStatusDrifted
		}
		return DriftStatusClean
	case applyrun.StatusNoMatch:
		// FINDING-01(b): host out of scope (on/where filtered out all tasks)
		// → drift undetermined but not a failure either. Semantically clean
		// (nothing to drift).
		return DriftStatusClean
	case applyrun.StatusFailed, applyrun.StatusCancelled, applyrun.StatusOrphaned:
		// failed can mean two things: plan.unsupported is a community module
		// without read-safe capability (Scry default-deny, not a real error);
		// anything else is a genuine FAILED (Plan error/no_result/timeout).
		if strings.Contains(failureSummary, "plan.unsupported") {
			return DriftStatusUnsupported
		}
		return DriftStatusFailed
	}
	// running/planned/claimed/dispatched never reach here: assembleDriftReport
	// runs after driftBarrier (all hosts terminal). Any unexpected status is
	// fail-closed as failed.
	return DriftStatusFailed
}

func summarize(hosts []DriftHostReport) DriftSummary {
	var s DriftSummary
	for _, h := range hosts {
		switch h.Status {
		case DriftStatusClean:
			s.HostsClean++
		case DriftStatusDrifted:
			s.HostsDrifted++
		case DriftStatusUnsupported:
			s.HostsUnsupported++
		case DriftStatusFailed:
			s.HostsFailed++
		}
	}
	return s
}

// boolField reads a bool value from register_data (jsonb `map[string]any`).
// Returns false on missing key or non-bool type (fail-closed: "change not
// confirmed" = clean).
func boolField(m map[string]any, key string) (bool, bool) {
	if m == nil {
		return false, false
	}
	v, ok := m[key]
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	if !ok {
		return false, false
	}
	return b, true
}

// MarkDriftStatus transitions the incarnation to `drift` (or back to `ready`
// if no drift was found, preserving the informational semantics). Called by
// the check-drift handler after assembling the DriftReport.
//
// Safe update: the transition is allowed only from ready or drift (guarded by
// a FOR UPDATE status-machine check). If the incarnation moved to
// applying/error_locked/destroying in the meantime, the UPDATE silently
// no-ops — we never clobber someone else's transition. ready→ready /
// drift→drift are no-ops too.
//
// The caller (REST/MCP) writes the audit event, not this method: the payload
// is assembled at the handler level from summary + archon.
func (r *Runner) MarkDriftStatus(ctx context.Context, name string, hasDrift bool) error {
	target := incarnation.StatusReady
	if hasDrift {
		target = incarnation.StatusDrift
	}
	return updateDriftStatus(ctx, r.deps.DB, name, target)
}

// updateDriftStatus is a WHERE-guarded UPDATE: the status transition is
// allowed only from ready and drift (informational, non-blocking).
// applying/error_locked/migration_failed/destroying/destroy_failed → no-op (no
// rows affected; we don't return ErrAlreadyFinalized — it's a safe no-op).
//
// 5s detached ctx guards against caller-ctx cancellation (HTTP client
// disconnect): the drift mark must commit even if the connection drops — it's
// informational and blocks no one.
func updateDriftStatus(ctx context.Context, db applyrun.ExecQueryRower, name string, target incarnation.Status) error {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	const sql = `UPDATE incarnation
SET status = $2, updated_at = NOW()
WHERE name = $1 AND status IN ('ready', 'drift')`
	if _, err := db.Exec(wctx, sql, name, string(target)); err != nil {
		return fmt.Errorf("incarnation: drift-mark %s: %w", target, err)
	}
	return nil
}
