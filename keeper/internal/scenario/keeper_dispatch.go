package scenario

import (
	"context"
	"fmt"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// dispatchKeeperTasks executes this Passage's keeper-side tasks (`on: keeper`,
// docs/keeper/modules.md) LOCALLY on this keeper instance — no work-queue/Soul
// involved. Execution contract mirrors the Soul-side path (events_taskevent.go /
// events_runresult.go) but runs in-process: the module executes through the
// keeper-side core Registry, its ApplyEvents are collected by an in-proc stream
// and folded into the same run tables:
//   - apply_runs (apply_id, sid=[render.KeeperTargetSID]) — terminal success/failed;
//   - apply_task_register — task register result (read by loadRegisterByHost);
//   - error_summary — failure reason (RecordTaskFailure), same as Soul-side tasks.
//
// All keeper tasks of a Passage share ONE apply_runs row (apply_id, sid=keeper,
// passage): task_idx distinguishes them in apply_task_register, and the row
// goes success only once every keeper task of THIS Passage passed. First
// failing keeper task → row failed + error returned (run() aborts). A staged
// run with keeper tasks across several Passages writes N keeper rows, one per
// (apply_id, keeper, passage) (triple PK, migration 078). N=1 (or all keeper
// tasks in Passage 0) → single passage-0 row, bit-for-bit as before Slice 2.
//
// Runs per-Passage INSIDE the stage loop (run.go), on tasks re-rendered for
// ActivePassage=passage, STRICTLY BEFORE this Passage's host-dispatch: a
// keeper-task failure on Passage>0 aborts before this Passage's host fan-out
// starts, same as Passage 0. keeper→keeper register chaining: a keeper task on
// Passage N sees `register.<prev>.*` from keeper tasks of earlier Passages via
// renderIn.KeeperRegister (stage-loop carry-over, see keeperRegisterBucket).
// Ordering matters at the refresh boundary: keeper dispatch of Passage P runs
// before P+1's re-resolve reads its effect (e.g. core.soul.registered{refresh_soulprint}
// writes souls+coven).
//
// No keeper tasks for this Passage → no-op (host-only Passage, or a run with no
// keeper-side tasks at all — ordinary Soul-side path).
func (r *Runner) dispatchKeeperTasks(ctx context.Context, spec RunSpec, log *slog.Logger, passage int, tasks []*render.RenderedTask, plans []render.DispatchPlan) error {
	keeperTasks := keeperTasksOf(tasks, plans, passage)
	if len(keeperTasks) == 0 {
		return nil
	}
	if r.keeperModules == nil {
		return ErrKeeperModulesNotConfigured
	}

	// One apply_runs row per keeper-target per Passage: inserted running before
	// the first task, terminal after all keeper tasks of the Passage ran.
	// Passage is part of the PK (apply_id, sid, passage) — N keeper rows for a
	// staged run with keeper tasks in different Passages.
	if err := applyrun.Insert(ctx, r.deps.DB, &applyrun.ApplyRun{
		ApplyID:         spec.ApplyID,
		SID:             render.KeeperTargetSID,
		IncarnationName: spec.IncarnationName,
		Scenario:        spec.ScenarioName,
		Status:          applyrun.StatusRunning,
		StartedByAID:    startedByPtr(spec.StartedByAID),
		Passage:         passage,
	}); err != nil {
		return fmt.Errorf("scenario: insert keeper apply_run (passage %d): %w", passage, err)
	}

	for _, rt := range keeperTasks {
		changed, failed, output, msg := r.applyKeeperTask(ctx, rt)
		log.Info("scenario: keeper-side task executed",
			slog.String("module", rt.Module),
			slog.Int("task_idx", rt.Index),
			slog.Bool("changed", changed),
			slog.Bool("failed", failed))

		// task.executed for EVERY keeper task — mirrors the Soul-side handler
		// (events_taskevent.go). Without it, changed keeper tasks (on: keeper) never
		// reach the changed_tasks fold (auditpg) and the task: Tiding subscription
		// (ADR-052 §k) silently misses them. Emitted BEFORE the failed-return so a
		// failing keeper task is logged too (status FAILED, not CHANGED).
		r.emitKeeperTaskExecuted(ctx, spec.ApplyID, passage, rt, changed, failed, msg, log)
		// applybus publish (ADR-068 §A2) — ALONGSIDE audit, not instead: keeper-side
		// `on: keeper` progress is visible on operator SSE just like Soul-side.
		r.publishKeeperTaskExecuted(spec.ApplyID, passage, rt, changed, failed)

		if failed {
			summary := composeKeeperFailure(rt, msg)
			r.recordKeeperFailure(ctx, spec.ApplyID, passage, rt, summary, log)
			return fmt.Errorf("scenario: keeper-side task %q (%s) failed: %s", rt.Name, rt.Module, msg)
		}

		// Bind membership BEFORE the trait sync-hook (ADR-008 amendment
		// 2026-07-17/NIM-124): core.soul.registered is the bind act — it writes
		// incarnation membership (incarnation_membership) for its SIDs from the
		// current run's incarnation. Authoritative: a membership write failure fails
		// the run (a registered-but-not-a-member host would be invisible to the
		// roster). Must precede syncTraitsOnRegistered, which now resolves members
		// via incarnation_membership.
		if berr := r.bindMembershipOnRegistered(ctx, spec, rt, output); berr != nil {
			summary := composeKeeperFailure(rt, berr.Error())
			r.recordKeeperFailure(ctx, spec.ApplyID, passage, rt, summary, log)
			return fmt.Errorf("scenario: keeper-side task %q (%s) membership bind failed: %w", rt.Name, rt.Module, berr)
		}

		// register: of a keeper task accumulates under KeeperTargetSID — same path
		// as Soul-side accumulateRegister. No register: (rt.Register=="") / no_log →
		// not written (loadRegisterByHost wouldn't resolve it anyway). passage is
		// REQUIRED (Slice 2): FK apply_task_register→apply_runs is the triple
		// (apply_id, sid, passage) (migration 078) — a Passage P task's register
		// must reference the keeper apply_runs row for exactly passage P (inserted
		// above); otherwise the FK would target (apply_id, keeper, 0), which for
		// P>0 doesn't exist, and the register would be lost.
		r.accumulateKeeperRegister(ctx, spec.ApplyID, passage, rt, changed, failed, output, log)

		// Sync-hook for the bind path (ADR-060 amend, R1): once membership is
		// written above, project incarnation.traits onto member souls.traits so the
		// newly bound host picks up its incarnation's traits. Gated on the registered
		// module specifically; other keeper tasks (cloud/vault) don't bind hosts.
		r.syncTraitsOnRegistered(ctx, spec.IncarnationName, rt, log)
	}

	if err := applyrun.UpdateStatus(ctx, r.deps.DB, spec.ApplyID, render.KeeperTargetSID, passage, applyrun.StatusSuccess, nil); err != nil {
		return fmt.Errorf("scenario: transitioning keeper apply_run (passage %d) to success: %w", passage, err)
	}
	return nil
}

// emitKeeperTaskExecuted writes the task.executed audit event for a keeper-side
// task — mirrors the Soul-side handler (events_taskevent.go). It's the only
// source through which the changed_tasks fold (auditpg) and the task: Tiding
// subscription (ADR-052 §k) see keeper tasks (on: keeper): without it, a
// changed keeper-target task silently drops out of run_completed.changed_tasks.
//
// Payload shape is the shared [audit.BuildTaskExecutedPayload] (same as
// Soul-side) so the fold (payload->>'sid'/'task_idx'/'status') sees both sides
// uniformly. sid = render.KeeperTargetSID, correlation_id = apply_id (matches
// the SelectChangedTaskKeys filter).
//
// Secret hygiene: register_data/output are NOT put in the payload (keeper tasks
// may carry vault-resolved output); message only on failure and only for
// non-no_log tasks (suppressed by the helper for no_log). This is the audit
// path (changed_tasks/Tiding fold); SSE visibility of keeper-side progress is a
// separate channel, [Runner.publishKeeperTaskExecuted] (ADR-068 §A2).
//
// Audit=nil (unit build without audit) → no-op. A write error is only logged:
// losing the event degrades observability but doesn't fail the run.
func (r *Runner) emitKeeperTaskExecuted(ctx context.Context, applyID string, passage int, rt *render.RenderedTask, changed, failed bool, message string, log *slog.Logger) {
	if r.deps.Audit == nil {
		return
	}
	in := audit.TaskExecutedInput{
		SID:     render.KeeperTargetSID,
		ApplyID: applyID,
		TaskIdx: rt.Index,
		// keeper-side tasks (`on: keeper`) execute as ONE row per (apply_id, keeper,
		// passage) — no per-host where:, local position always equals the global
		// RenderedTask.Index. plan_index == task_idx == rt.Index (plan-wide across
		// all Passages → correlation key unique between Passages).
		PlanIndex: rt.Index,
		Status:    keeperTaskStatus(changed, failed).String(),
		NoLog:     rt.NoLog,
		// This task's passage (Slice 2: keeper tasks are stratified by Passage). In
		// the payload for per-Passage triage; doesn't affect changed_tasks
		// correlation (that goes by sid/plan_index). 0 for N=1 / Passage-0 tasks.
		Passage: passage,
	}
	if failed {
		in.Error = &audit.TaskExecutedError{
			Module:  rt.Module,
			Message: message,
		}
	}
	ev := &audit.Event{
		EventType:     audit.EventTaskExecuted,
		Source:        audit.SourceKeeperInternal,
		CorrelationID: applyID,
		Payload:       audit.BuildTaskExecutedPayload(in),
	}
	if err := r.deps.Audit.Write(ctx, ev); err != nil {
		log.Warn("scenario: writing audit task.executed for the keeper task failed",
			slog.Int("task_idx", rt.Index), slog.String("module", rt.Module), slog.Any("error", err))
	}
}

// publishKeeperTaskExecuted relays keeper-side task.executed onto the SSE
// channel via applybus (ADR-068 §A2) — MIRRORS Soul-side [publishTaskExecuted]
// (grpc/events_taskevent.go): same payload shape (snake_case keys, same field
// set), sid = [render.KeeperTargetSID]. Without this, `on: keeper` steps
// (cloud/vault/registered) are silent in the live run view.
//
// ★SECRET HYGIENE (same as Soul-side): SSE does NOT carry output/register_data/
// message (keeper tasks may carry vault-resolved output); on failed — only
// error{code,module}, no message. The final [audit.MaskSecrets] on the SSE
// write path is a second barrier. task_idx/passage are int32 (type parity with
// Soul-side proto getters).
//
// ApplyBus=nil (single-Keeper dev / unit without SSE) → no-op, same as Soul-side.
func (r *Runner) publishKeeperTaskExecuted(applyID string, passage int, rt *render.RenderedTask, changed, failed bool) {
	if r.deps.ApplyBus == nil {
		return
	}
	payload := map[string]any{
		"apply_id":    applyID,
		"kind":        string(applybus.KindTaskExecuted),
		"sid":         render.KeeperTargetSID,
		"task_idx":    int32(rt.Index),
		"task_status": keeperTaskStatus(changed, failed).String(),
		"passage":     int32(passage),
	}
	if rt.NoLog {
		payload["suppressed"] = "no_log"
	}
	if failed {
		// keeper-side carries no structured error.code (module returned a
		// message/gRPC error); code="" is honest, keys code+module mirror
		// Soul-side. message (stderr) is NOT put in SSE — floor for all failed
		// tasks (BUG-3, same as Soul-side).
		payload["error"] = map[string]any{
			"code":   "",
			"module": rt.Module,
		}
	}
	r.deps.ApplyBus.Publish(applybus.Event{
		ApplyID: applyID,
		Kind:    applybus.KindTaskExecuted,
		Payload: payload,
	})
}

// keeperTaskStatus maps a keeper task's outcome (changed/failed) to the
// keeperv1 enum so the task.executed payload carries the same status string as
// Soul-side (Status().String()). The changed fold filters on the literal
// "TASK_STATUS_CHANGED" (auditpg). keeper-side doesn't distinguish timed_out
// (the module either returns a failed event or a gRPC error) — failed suffices.
func keeperTaskStatus(changed, failed bool) keeperv1.TaskStatus {
	switch {
	case failed:
		return keeperv1.TaskStatus_TASK_STATUS_FAILED
	case changed:
		return keeperv1.TaskStatus_TASK_STATUS_CHANGED
	default:
		return keeperv1.TaskStatus_TASK_STATUS_OK
	}
}

// applyKeeperTask calls a keeper-side core module in-process and folds its
// ApplyEvent stream into a final result (changed/failed/output/message),
// mirroring Soul-side runTask (selfRegisterData). Module address splits into
// (base, state) via the same config.SplitModuleAddr as Soul-side plantask/
// applyrunner: Registry indexes modules by base (`core.cloud`), state
// (`created`) goes into ApplyRequest.state. A malformed address or a module
// not found in the Registry → failed (like Soul on an unknown module). Apply
// returning a gRPC error (not a failed event) → failed with the error text.
func (r *Runner) applyKeeperTask(ctx context.Context, rt *render.RenderedTask) (changed, failed bool, output map[string]any, message string) {
	base, state, ok := config.SplitModuleAddr(rt.Module)
	if !ok {
		return false, true, nil, fmt.Sprintf("invalid keeper-side module address %q (want <namespace>.<module>.<state>)", rt.Module)
	}
	mod, ok := r.keeperModules.Lookup(base)
	if !ok {
		return false, true, nil, fmt.Sprintf("unknown keeper-side module %q", rt.Module)
	}

	req := &pluginv1.ApplyRequest{
		State:  state,
		Params: rt.Params,
	}
	sink := newKeeperApplyStream(ctx)
	if err := mod.Apply(req, sink); err != nil {
		return false, true, nil, err.Error()
	}

	last := sink.last()
	if last == nil {
		// No final event from the module — treated as a contract error (Soul-side
		// also considers a missing final event anomalous).
		return false, true, nil, "keeper-side module produced no final event"
	}
	if last.GetFailed() {
		return false, true, nil, last.GetMessage()
	}
	var out map[string]any
	if o := last.GetOutput(); o != nil {
		out = o.AsMap()
	}
	return last.GetChanged(), false, out, last.GetMessage()
}

// accumulateKeeperRegister writes a keeper task's register result into
// apply_task_register under KeeperTargetSID — same path as Soul-side
// accumulateRegister (events_taskevent.go). Payload is {changed, failed,
// timed_out, skipped} + the module's output fields (mirrors selfRegisterData
// in applyrunner.go). A task without register: or no_log → no-op
// (loadRegisterByHost wouldn't resolve it into state_changes anyway). Errors
// are only logged (best-effort, like Soul-side accumulateRegister).
//
// passage (Slice 2): FK apply_task_register→apply_runs is the triple
// (apply_id, sid, passage) (migration 078). A Passage P keeper task's register
// MUST reference the keeper apply_runs row for exactly passage P (inserted in
// dispatchKeeperTasks before the loop over this Passage's keeper tasks).
// Otherwise the FK targets (apply_id, keeper, 0), which for P>0 doesn't exist,
// losing the register and breaking the keeper→keeper Passage P→P+1 chain.
func (r *Runner) accumulateKeeperRegister(ctx context.Context, applyID string, passage int, rt *render.RenderedTask, changed, failed bool, output map[string]any, log *slog.Logger) {
	if rt.Register == "" || rt.NoLog {
		return
	}
	data := map[string]any{
		"changed":   changed,
		"failed":    failed,
		"timed_out": false,
		"skipped":   false,
	}
	for k, v := range output {
		data[k] = v
	}
	if err := applyrun.UpsertTaskRegister(ctx, r.deps.DB, &applyrun.TaskRegister{
		ApplyID: applyID,
		SID:     render.KeeperTargetSID,
		// keeper task executes locally with the global rt.Index — both the
		// correlation key (PlanIndex) and the informational TaskIdx: keeper-side has
		// no per-Passage ApplyRequest slice, so local==global index (ADR-056 §S1 fix
		// Variant B). buildRegisterByHost resolves the name by PlanIndex.
		PlanIndex:    rt.Index,
		TaskIdx:      rt.Index,
		RegisterData: data,
		// passage of the keeper task — part of the FK to apply_runs(apply_id, sid,
		// passage) + the accumulation filter loadRegisterByHostUpToPassage (register Passage<P).
		Passage: passage,
	}); err != nil {
		log.Warn("scenario: accumulating register for the keeper task failed",
			slog.Int("passage", passage), slog.Int("task_idx", rt.Index), slog.Any("error", err))
	}
}

// registeredModuleBase is the base address of the keeper-side core module
// core.soul.registered (coremod/soul.Name). Kept as a local constant: scenario
// does NOT import coremod/soul (it pulls PG-store/presence deps) — the bind
// boundary is recognized by task address, not module type.
const (
	registeredModuleBase  = "core.soul"
	registeredModuleState = "registered"
)

// syncTraitsOnRegistered is the sync-hook for the Trait relocation bind path
// (ADR-060 amend, R1). After core.soul.registered SUCCEEDS and membership is
// written (bindMembershipOnRegistered), project incarnation.traits onto member
// souls.traits so the newly bound host picks up its incarnation's traits.
// Gated specifically on the registered module — other keeper tasks (cloud/vault)
// don't bind hosts.
//
// Best-effort: empty incName (direct keeper test without an incarnation) /
// incarnation without traits / load failure → logged, run not failed (traits
// are an organizational label, not an apply blocker). Idempotent: a repeat
// bind re-projects the same source.
func (r *Runner) syncTraitsOnRegistered(ctx context.Context, incName string, rt *render.RenderedTask, log *slog.Logger) {
	base, state, ok := config.SplitModuleAddr(rt.Module)
	if !ok || base != registeredModuleBase || state != registeredModuleState {
		return
	}
	if incName == "" || r.deps.DB == nil {
		return
	}
	inc, err := incarnation.SelectByName(ctx, r.deps.DB, incName)
	if err != nil {
		log.Warn("scenario: bind-sync traits - loading the incarnation failed (best-effort)",
			slog.String("incarnation", incName), slog.Any("error", err))
		return
	}
	if len(inc.Traits) == 0 {
		return
	}
	if serr := incarnation.SyncTraitsToHosts(ctx, r.deps.DB, incName, inc.Traits); serr != nil {
		log.Warn("scenario: bind-sync traits -> souls failed (best-effort)",
			slog.String("incarnation", incName), slog.Any("error", serr))
	}
}

// recordKeeperFailure records a keeper-task failure (RecordTaskFailure +
// apply_run → failed) for the triple (apply_id, keeper, passage). rt.Index is
// the global plan-wide index; for keeper-target local==global (no per-host
// where:), so task_idx and plan_index both equal rt.Index. Write errors are
// only logged (losing the reason degrades triage but the run still aborts).
func (r *Runner) recordKeeperFailure(ctx context.Context, applyID string, passage int, rt *render.RenderedTask, summary string, log *slog.Logger) {
	if rerr := applyrun.RecordTaskFailure(ctx, r.deps.DB, applyID, render.KeeperTargetSID, passage, rt.Index, rt.Index, summary); rerr != nil {
		log.Warn("scenario: recording the keeper task failure reason failed",
			slog.Int("passage", passage), slog.Int("task_idx", rt.Index), slog.Any("error", rerr))
	}
	if uerr := applyrun.UpdateStatus(ctx, r.deps.DB, applyID, render.KeeperTargetSID, passage, applyrun.StatusFailed, &summary); uerr != nil {
		log.Warn("scenario: transitioning keeper apply_run to failed failed",
			slog.Int("passage", passage), slog.Any("error", uerr))
	}
}

// bindMembershipOnRegistered writes incarnation membership for a successful
// core.soul.registered task (ADR-008 amendment 2026-07-17/NIM-124: membership is
// a first-class relation, no longer `incarnation.name ∈ souls.coven[]`). The
// current run's incarnation (spec.IncarnationName) is the unambiguous target —
// cross-incarnation binding is forbidden by the grammar. Idempotent (ON CONFLICT
// DO NOTHING). Gated on the registered module specifically; other keeper tasks
// (cloud/vault) don't bind hosts.
//
// Empty incarnation name (direct keeper test without an incarnation) / nil DB /
// no SIDs in the module output → no-op, nil. Any DB error is returned so the
// caller fails the run (the module already created the souls rows, so the FK
// sid → souls is satisfied; an error here is a real DB fault, not a missing row).
func (r *Runner) bindMembershipOnRegistered(ctx context.Context, spec RunSpec, rt *render.RenderedTask, output map[string]any) error {
	base, state, ok := config.SplitModuleAddr(rt.Module)
	if !ok || base != registeredModuleBase || state != registeredModuleState {
		return nil
	}
	if spec.IncarnationName == "" || r.deps.DB == nil {
		return nil
	}
	sids := sidsFromRegisteredOutput(output)
	if len(sids) == 0 {
		return nil
	}
	return incarnation.AddMembers(ctx, r.deps.DB, spec.IncarnationName, sids, startedByPtr(spec.StartedByAID))
}

// sidsFromRegisteredOutput extracts the registered SIDs from a
// core.soul.registered module output: `sid` is a string (single) or a list of
// strings (batch), matching buildOutput in coremod/soul. Empty/other → nil.
func sidsFromRegisteredOutput(output map[string]any) []string {
	raw, ok := output["sid"]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case string:
		if v == "" {
			return nil
		}
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// keeperRegisterBucket extracts the flat keeper-task register bucket from the
// run's per-host register table: entries under the synthetic host
// KeeperTargetSID ("keeper"), where accumulateKeeperRegister writes keeper-side
// task register results. Returns register-name → payload (same shape as flat
// RenderInput.Register), or nil if the keeper bucket is empty/absent.
//
// Purpose (staged render, keeper→keeper register chaining): the stage loop in
// run.go carries the result into the ISOLATED renderIn.KeeperRegister before
// per-passage render of the active Passage's keeper tasks. keeperVars
// (render/dispatch.go) reads KeeperRegister specifically — so a keeper task on
// Passage N sees `register.<prev>.*` from keeper tasks of earlier Passages,
// while the host fallback (hostRegister) does NOT get keeper-register (the
// channel is separate from flat Register). registerByHost is whatever
// loadRegisterByHostUpToPassage returned (register for Passage < active), so
// the bucket only carries ALREADY completed keeper tasks (forward-only).
func keeperRegisterBucket(registerByHost map[string]map[string]any) map[string]any {
	bucket := registerByHost[render.KeeperTargetSID]
	if len(bucket) == 0 {
		return nil
	}
	return bucket
}

// keeperTasksOf selects RenderedTasks whose DispatchPlan is marked Keeper=true
// (render.IsKeeperTask) AND whose RenderedTask.Passage == passage, in Index
// order (= scenario.tasks[] order).
//
// Passage filter (staged render, ADR-056, Slice 2): keeper tasks are
// stratified by register dependency like host tasks (core.bootstrap.delivered
// reads register.provision.* → Passage strictly AFTER core.cloud.created).
// dispatchKeeperTasks is called per-Passage on tasks re-rendered for
// ActivePassage=p; only this Passage's keeper tasks are selected here. A
// future Passage's (>p) keeper task on this render is a placeholder without
// Params (pipeline.go placeholder gate) AND without Keeper=true in the plan,
// so the Keeper filter alone would already exclude it; the Passage filter is a
// second boundary (on a fresh render of Passage p, its staged keeper task
// carries Keeper=true). N=1 run: single Passage 0, all keeper tasks have
// passage==0 → selected as before this epic (bit-for-bit).
func keeperTasksOf(tasks []*render.RenderedTask, plans []render.DispatchPlan, passage int) []*render.RenderedTask {
	byIndex := make(map[int]*render.RenderedTask, len(tasks))
	for _, t := range tasks {
		byIndex[t.Index] = t
	}
	out := make([]*render.RenderedTask, 0)
	for _, p := range plans {
		if !p.Keeper {
			continue
		}
		t := byIndex[p.TaskIndex]
		if t == nil || t.Passage != passage {
			continue
		}
		out = append(out, t)
	}
	return out
}

// composeKeeperFailure builds the operator-facing failure reason for a keeper
// task, for apply_runs.error_summary (format `task <idx> <module>: <message>`,
// mirrors composeTaskErrorSummary on Soul-side). A no_log keeper task →
// message suppressed (like failureReason).
func composeKeeperFailure(rt *render.RenderedTask, message string) string {
	if rt.NoLog {
		return fmt.Sprintf("task %d %s: (no_log task failed)", rt.Index, rt.Module)
	}
	head := fmt.Sprintf("task %d %s", rt.Index, rt.Module)
	if message == "" {
		return head
	}
	// This run's sealed paths aren't passed to a keeper-task summary (narrow
	// point building the task's error text) → nil: vault+regex layers +
	// regex-alarm (ADR-010 §7.4) still apply. A resolved keeper-task message is
	// the module's message, not a sealed-path value; vault-ref/sensitive-by-name
	// are covered by those layers regardless.
	return head + ": " + maskErrText(fmt.Errorf("%s", message), nil)
}

// keeperApplyStream is an in-proc grpc.ServerStreamingServer[ApplyEvent]
// implementation for calling a keeper-side core module locally (mirrors
// inProcApplyStream in the Soul-side runtime). Buffers ApplyEvents; the caller
// looks at the last one.
type keeperApplyStream struct {
	grpc.ServerStream
	ctx    context.Context
	events []*pluginv1.ApplyEvent
}

func newKeeperApplyStream(ctx context.Context) *keeperApplyStream {
	return &keeperApplyStream{ctx: ctx}
}

func (s *keeperApplyStream) Context() context.Context { return s.ctx }

func (s *keeperApplyStream) Send(ev *pluginv1.ApplyEvent) error {
	s.events = append(s.events, ev)
	return nil
}

func (s *keeperApplyStream) SetHeader(metadata.MD) error  { return nil }
func (s *keeperApplyStream) SendHeader(metadata.MD) error { return nil }
func (s *keeperApplyStream) SetTrailer(metadata.MD)       {}

func (s *keeperApplyStream) SendMsg(m any) error {
	ev, ok := m.(*pluginv1.ApplyEvent)
	if !ok {
		return fmt.Errorf("keeper apply stream: SendMsg got %T, want *pluginv1.ApplyEvent", m)
	}
	return s.Send(ev)
}

func (s *keeperApplyStream) RecvMsg(any) error {
	return fmt.Errorf("keeper apply stream: RecvMsg not supported")
}

func (s *keeperApplyStream) last() *pluginv1.ApplyEvent {
	if len(s.events) == 0 {
		return nil
	}
	return s.events[len(s.events)-1]
}
