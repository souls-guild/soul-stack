package grpc

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// handleTaskEvent — handler for the [keeperv1.TaskEvent] payload (M2.4).
//
// Per PM-decision (3): we write a single audit-event `task.executed` with
// `source: soul_grpc`, `correlation_id = apply_id`, and a payload of status,
// task_idx, error (if any), and register_data (if any). register_data itself
// is masked by the shared [audit.MaskSecrets] on the write path (auditpg).
//
// no_log suppression: for a task with TaskEvent.no_log=true (echoing RenderedTask.no_log,
// apply.proto), register_data and error.message are NOT written to audit — this is the root of a
// leak of an arbitrary secret that MaskSecrets can't catch by vault-ref. The payload carries a
// suppressed:"no_log" marker. Suppression is strictly by the echoed flag, without touching
// []RenderedTask: on multi-Keeper (ADR-002) this TaskEvent may have arrived at a different
// instance than the one holding the run-goroutine.
//
// The TaskStatus enum (including `TASK_STATUS_CANCELLED`) is serialized into the payload
// via `Status().String()` as a single `status` field — extending the enum
// (CANCELLED, SKIPPED, …) is handled without separate branches. A separate
// `task.cancelled` audit-event is post-MVP (filtering by `payload->>'status'`
// in `audit_log` covers the practical use case without duplicating the enum).
//
// Persisting into `incarnation.run_state` (PM-decision 3 "optional") is post-MVP:
// in MVP, run-time state is not materialized as a separate table — the outcome is recorded
// in one motion via `RunResult` (see [handleRunResult]).
func (h *eventStreamHandler) handleTaskEvent(ctx context.Context, sid, sessionID string, ev *keeperv1.TaskEvent) {
	if ev == nil {
		h.logger.Warn("eventstream: TaskEvent payload is nil",
			slog.String("sid", sid), slog.String("session_id", sessionID))
		return
	}

	// no_log task: register_data (params/output) and error.message (= stderr) are the
	// root of a leak of an arbitrary secret that MaskSecrets can't catch by
	// vault-ref. We suppress them in the long-lived audit. The decision is strictly by the echoed
	// TaskEvent.no_log flag (apply.proto): []RenderedTask is held by the run-goroutine, and this
	// TaskEvent may have arrived at a different instance on multi-Keeper (ADR-002). The
	// suppressed:"no_log" marker is set by the helper itself.
	noLog := ev.GetNoLog()
	in := audit.TaskExecutedInput{
		SID:     sid,
		ApplyID: ev.GetApplyId(),
		TaskIdx: int(ev.GetTaskIdx()),
		// plan_index (ADR-056 §S1 fix Variant B): the GLOBAL cross-plan index across the
		// whole plan (= RenderedTask.Index) — the correlation key linking a CHANGED task to the plan in
		// auditpg.SelectChangedTaskKeys (state_changes whitelist + audit). The local
		// TaskIdx under staged/per-host-where ≠ the global one. N=1 → plan_index==task_idx.
		PlanIndex: int(ev.GetPlanIndex()),
		Status:    ev.GetStatus().String(),
		NoLog:     noLog,
		Passage:   int(ev.GetPassage()),
	}
	if e := ev.GetError(); e != nil {
		in.Error = &audit.TaskExecutedError{
			Code:    e.GetCode(),
			Module:  e.GetModule(),
			Message: e.GetMessage(),
		}
	}
	if rd := ev.GetRegisterData(); rd != nil && !noLog {
		// google.protobuf.Struct → JSON via protojson is the only way to
		// correctly serialize NullValue / NumberValue / nested-Struct.
		// Bytes go straight into the payload — the auditpg-writer will route them through MaskSecrets.
		// no_log → we don't write register_data at all (arbitrary secret in output).
		if b, err := protojson.Marshal(rd); err != nil {
			h.logger.Warn("eventstream: register_data marshal failed",
				slog.String("sid", sid),
				slog.String("apply_id", ev.GetApplyId()),
				slog.Any("error", err),
			)
		} else {
			in.RegisterData = string(b)
		}
	}
	payload := audit.BuildTaskExecutedPayload(in)

	if err := h.deps.AuditWriter.Write(ctx, &audit.Event{
		EventType:     audit.EventTaskExecuted,
		Source:        audit.SourceSoulGRPC,
		CorrelationID: ev.GetApplyId(),
		Payload:       payload,
		CreatedAt:     time.Now().UTC(),
	}); err != nil {
		h.logger.Warn("eventstream: audit write task.executed failed",
			slog.String("sid", sid),
			slog.String("apply_id", ev.GetApplyId()),
			slog.Any("error", err),
		)
	}

	h.recordTaskFailure(ctx, sid, ev)
	h.accumulateRegister(ctx, sid, ev)
	h.publishTaskExecuted(sid, ev)
}

// recordTaskFailure records the failure reason of the host's first failed task in
// the `apply_runs` row (BUG-3): task index, module name, and the text of
// `TaskError.Message`. This way an operator hitting `GET /v1/incarnations/<name>` sees
// the specific step and reason (`task 0 core.pkg.installed: E: Version '7.2.4' not
// found`), instead of a bare `RUN_STATUS_FAILED`.
//
// Storage is Postgres (NOT in-memory): a TaskEvent may have arrived at a different
// Keeper instance than the one holding the run-goroutine (ADR-002); a shared table survives
// cross-Keeper routing. first-failure-wins is guaranteed by [applyrun.RecordTaskFailure]
// (COALESCE) — there's no race when several tasks fail.
//
// Masking: error_summary is read externally via barrier/status_details (GET
// incarnation, unmasked on that channel), so MaskSecrets is applied
// here, on the write path — vault-ref / secret-shaped values in a task's stderr
// won't leak. For a no_log task (echoing TaskEvent.no_log, apply.proto), message
// (= stderr) may carry an arbitrary plaintext secret that MaskSecrets doesn't
// catch — so the summary is entirely replaced with the neutral "(no_log task failed)"
// right here, on the write path. This is defense-in-depth: the run-goroutine
// (scenario.dispatch) does the same, holding []RenderedTask with NoLog, but on
// multi-Keeper (ADR-002) it may have ended up on a different instance; the floor in dispatch
// still holds.
//
// Triggers only on FAILED/TIMED_OUT (TaskError is populated only there, see
// apply.proto). ApplyRunDB=nil (unit build without PG / ad-hoc push) → no-op.
// ErrApplyRunNotFound (push without a scenario-runner / TaskEvent raced ahead of Insert)
// → log+skip: the reason is lost, but we don't fail the apply stream.
func (h *eventStreamHandler) recordTaskFailure(ctx context.Context, sid string, ev *keeperv1.TaskEvent) {
	if h.deps.ApplyRunDB == nil {
		return
	}
	if !isFailedStatus(ev.GetStatus()) {
		return
	}
	taskIdx := int(ev.GetTaskIdx())
	if taskIdx < 0 {
		return
	}

	// no_log task: error.message (= stderr) may carry an arbitrary plaintext
	// secret that MaskSecrets can't catch by vault-ref. On the write path (the source of
	// error_summary) we suppress it with neutral text — defense-in-depth, not
	// relying on the floor in the run-goroutine (scenario.dispatch), which holds
	// []RenderedTask and may have ended up on a different instance on multi-Keeper (ADR-002).
	var summary string
	if ev.GetNoLog() {
		summary = fmt.Sprintf("task %d %s: (no_log task failed)", taskIdx, ev.GetError().GetModule())
	} else {
		summary = composeTaskErrorSummary(taskIdx, ev.GetError())
	}
	// passage (ADR-056): the failure reason is written into the (apply_id, sid, passage)
	// row of this Passage; N=1 → 0. The Soul echoes passage from ApplyRequest.
	//
	// plan_index (ADR-056 §S1 fix Variant B): the GLOBAL cross-plan index of the failed
	// task across the whole plan (= RenderedTask.Index). Written into apply_runs.
	// failed_plan_index — the correlation key for the failed task's module/action when building
	// DriftReport (checkdrift.buildHostReport) and for no_log suppression in the barrier
	// (dispatch.failureReason). The local taskIdx (the task_idx field) under staged/
	// per-host-where ≠ the global one — it's not fit for correlating with the plan (the same
	// defect the register channel closed via migration 079). N=1 → plan_index==task_idx.
	if err := applyrun.RecordTaskFailure(ctx, h.deps.ApplyRunDB, ev.GetApplyId(), sid, int(ev.GetPassage()), taskIdx, int(ev.GetPlanIndex()), summary); err != nil {
		h.logger.Warn("eventstream: record task failure failed",
			slog.String("sid", sid),
			slog.String("apply_id", ev.GetApplyId()),
			slog.Int("task_idx", taskIdx),
			slog.Int64("plan_index", int64(ev.GetPlanIndex())),
			slog.Any("error", err),
		)
	}
}

// isFailedStatus — true for terminal task statuses where
// TaskError is populated (FAILED / TIMED_OUT, see apply.proto). TIMED_OUT is a special case of
// failed.
func isFailedStatus(s keeperv1.TaskStatus) bool {
	return s == keeperv1.TaskStatus_TASK_STATUS_FAILED || s == keeperv1.TaskStatus_TASK_STATUS_TIMED_OUT
}

// composeTaskErrorSummary builds an operator-facing string of the task failure reason:
// `task <idx> <module>: <message>`. message is passed through MaskSecrets
// (vault-ref / secret-shaped values from stderr don't leak into the observable
// channel). Empty module/message are omitted, so we don't produce `task 3 : `.
func composeTaskErrorSummary(taskIdx int, te *keeperv1.TaskError) string {
	module := ""
	message := ""
	if te != nil {
		module = te.GetModule()
		message = maskString(te.GetMessage())
	}

	head := fmt.Sprintf("task %d", taskIdx)
	if module != "" {
		head += " " + module
	}
	if message == "" {
		return head
	}
	return head + ": " + message
}

// maskString runs a single string through [audit.MaskSecrets] (vault-ref /
// secret-shaped substring → ***MASKED***). audit only exposes masking for
// a map payload, so we wrap the string in a map and pull it back out.
func maskString(s string) string {
	if s == "" {
		return ""
	}
	masked := audit.MaskSecrets(map[string]any{"v": s})
	if v, ok := masked["v"].(string); ok {
		return v
	}
	return s
}

// accumulateRegister accumulates a task's register_data into `apply_task_register`
// (migration 022): after the barrier, the scenario-runner reads what's accumulated and builds
// RenderInput.Register per-host to render state_changes.sets (slice 2,
// orchestration.md §7.1).
//
// register_name isn't known here (the proto only carries task_idx, ADR-012(d)) —
// we store by task_idx; the name is resolved by the scenario-runner when reading from its own
// []RenderedTask. Storage is Postgres (NOT in-memory): on multi-Keeper
// (ADR-002) this TaskEvent may have arrived at a different instance than the one holding
// the run-goroutine; a shared table survives cross-Keeper routing.
//
// ApplyRunDB=nil (unit build without PG / ad-hoc push) → no-op. Empty
// register_data (a task without register:) → no-op. A write error is only
// logged: the register channel is best-effort at the accumulation level; the scenario-runner
// treats a missing row as an absent register value; a failure of
// this write must not fail the apply stream.
func (h *eventStreamHandler) accumulateRegister(ctx context.Context, sid string, ev *keeperv1.TaskEvent) {
	if h.deps.ApplyRunDB == nil {
		return
	}
	rd := ev.GetRegisterData()
	if rd == nil {
		return
	}
	if err := applyrun.UpsertTaskRegister(ctx, h.deps.ApplyRunDB, &applyrun.TaskRegister{
		ApplyID: ev.GetApplyId(),
		SID:     sid,
		// plan_index (ADR-056 §S1 fix Variant B): the GLOBAL cross-plan index of the task
		// across the whole plan (all Passages) — the register correlation key. It keys
		// apply_task_register (migration 079); task_idx (the local position in
		// its Passage's ApplyRequest) isn't unique across Passages or across hosts,
		// so it can't serve as the key. N=1 / an old Soul → plan_index=0=task_idx.
		PlanIndex:    int(ev.GetPlanIndex()),
		TaskIdx:      int(ev.GetTaskIdx()),
		RegisterData: rd.AsMap(),
		// passage (ADR-056): register accumulates per-(apply_id, sid, passage). The FK on
		// apply_runs requires passage to match the task row of this Passage.
		Passage: int(ev.GetPassage()),
	}); err != nil {
		h.logger.Warn("eventstream: accumulate register_data failed",
			slog.String("sid", sid),
			slog.String("apply_id", ev.GetApplyId()),
			slog.Int64("task_idx", int64(ev.GetTaskIdx())),
			slog.Any("error", err),
		)
	}
}

// publishTaskExecuted translates a TaskEvent into the SSE channel via applybus.
// Pure best-effort: ApplyBus=nil (single-Keeper dev without SSE) → no-op.
//
// Payload is the SSE contract: snake_case keys, fixed in
// docs/keeper/mcp-tools.md → § SSE event payloads.
//
// Suppressing raw stderr on operator-SSE (BUG-3 floor): the `no_log` flag lives in
// []RenderedTask on the run-goroutine (scenario.dispatch), NOT in the proto TaskEvent
// (ADR-012(d)); on multi-Keeper (ADR-002) this TaskEvent may have arrived at a different
// instance than the one holding the run-goroutine — meaning the grpc layer here doesn't know a
// task's no_log status. So for a FAILED task, `error.message` (= stderr, which may
// carry a no_log task's plaintext password, which MaskSecrets can't catch by
// vault-ref) is NOT placed into SSE at all: the frame only carries code/module for triage.
// The operator gets the detailed, safe reason via `status_details`/GET
// (which has no_log suppression + a second MaskSecrets pass, see scenario.failureReason).
// Symmetric to "(no_log task failed)" in dispatch, but stricter — a floor for all
// failed tasks, without depending on cross-Keeper propagation of run state.
//
// For NON-failed tasks (ok/changed), `error` is absent (TaskError is populated
// only on FAILED/TIMED_OUT, see apply.proto); the useful status fields are preserved.
// The final MaskSecrets pass on the SSE write path (writeSSEEvent) remains as a
// second barrier for register/state_changes secrets by vault-ref/keys.
//
// A no_log task additionally carries a suppressed:"no_log" marker — so the client
// can see the reason for a "quiet" frame (error without message), instead of treating it as data loss.
func (h *eventStreamHandler) publishTaskExecuted(sid string, ev *keeperv1.TaskEvent) {
	if h.deps.ApplyBus == nil {
		return
	}
	idx := ev.GetTaskIdx()
	payload := map[string]any{
		"apply_id":    ev.GetApplyId(),
		"kind":        string(applybus.KindTaskExecuted),
		"sid":         sid,
		"task_idx":    idx,
		"task_status": ev.GetStatus().String(),
		// passage (ADR-056): the staged-render Passage index. 0 = the only Passage.
		"passage": ev.GetPassage(),
	}
	// A marker for intentional suppression, for UX (SSE already floors error.message and doesn't
	// carry register_data; the marker lets the client see the reason for a "quiet" frame).
	if ev.GetNoLog() {
		payload["suppressed"] = "no_log"
	}
	if e := ev.GetError(); e != nil {
		// message (stderr) is intentionally not forwarded to SSE: see the doc-comment.
		payload["error"] = map[string]any{
			"code":   e.GetCode(),
			"module": e.GetModule(),
		}
	}
	h.deps.ApplyBus.Publish(applybus.Event{
		ApplyID: ev.GetApplyId(),
		Kind:    applybus.KindTaskExecuted,
		Payload: payload,
	})
}
