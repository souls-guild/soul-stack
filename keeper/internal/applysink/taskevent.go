package applysink

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

// TaskEvent records one task's terminal event for host sid across all four
// channels: the `task.executed` audit event, the failure reason on the host's
// `apply_runs` row, its advisory notices, the `register:` accumulator, and the
// operator SSE frame.
//
// A nil event is ignored (the caller has already logged the protocol violation
// it represents; the two transports word that differently).
//
// Order is load-bearing only in one place: audit is written before the PG
// writes so a run whose PG write fails still leaves the changed/failed fact the
// rollup and the cross-passage gating read (ADR-056 R3).
func (s *Sink) TaskEvent(ctx context.Context, sid string, ev *keeperv1.TaskEvent) {
	if ev == nil {
		return
	}
	s.writeTaskExecutedAudit(ctx, sid, ev)
	s.recordTaskFailure(ctx, sid, ev)
	s.recordRunNotices(ctx, sid, ev)
	s.accumulateRegister(ctx, sid, ev)
	s.publishTaskExecuted(sid, ev)
}

// writeTaskExecutedAudit writes the `task.executed` audit event.
//
// Declared-secret output ([ADR-0083] §8): the fields TaskEvent.secret_output names
// (echoing RenderedTask.secret_output, apply.proto) are replaced with
// [audit.MaskedValue] INSIDE register_data before the payload is built — the rest of
// the task's output, and its error.message, are written as before. The decision is
// strictly by the echoed list, without touching []RenderedTask: on multi-Keeper
// (ADR-002) this TaskEvent may have arrived at a different instance than the one
// holding the run-goroutine. register_data itself then passes the shared
// [audit.MaskSecrets] on the write path (auditpg).
//
// This replaces the per-task `no_log:` flag, which dropped register_data and
// error.message wholesale. Per-field is both narrower and stricter: it is the MODULE
// that declares which of its outputs is a credential, so a task no longer has to be
// silenced entirely, and no longer depends on an author remembering to silence it.
//
// The TaskStatus enum (including `TASK_STATUS_CANCELLED`) is serialized via
// `Status().String()` as a single `status` field — extending the enum is handled
// without separate branches.
func (s *Sink) writeTaskExecutedAudit(ctx context.Context, sid string, ev *keeperv1.TaskEvent) {
	if s.deps.Audit == nil {
		return
	}
	in := audit.TaskExecutedInput{
		SID:     sid,
		ApplyID: ev.GetApplyId(),
		TaskIdx: int(ev.GetTaskIdx()),
		// plan_index (ADR-056 §S1 fix Variant B): the GLOBAL cross-plan index across the
		// whole plan (= RenderedTask.Index) — the correlation key linking a CHANGED task
		// to the plan in auditpg.SelectChangedTaskKeys. The local TaskIdx under
		// staged/per-host-where ≠ the global one. N=1 → plan_index==task_idx.
		PlanIndex: int(ev.GetPlanIndex()),
		Status:    ev.GetStatus().String(),
		Passage:   int(ev.GetPassage()),
	}
	if e := ev.GetError(); e != nil {
		in.Error = &audit.TaskExecutedError{
			Code:    e.GetCode(),
			Module:  e.GetModule(),
			Message: e.GetMessage(),
		}
	}
	for _, n := range ev.GetNotices() {
		in.Notices = append(in.Notices, audit.TaskExecutedNotice{
			Code:    n.GetCode(),
			Module:  n.GetModule(),
			Param:   n.GetParam(),
			Message: n.GetMessage(),
		})
	}
	if rd := ev.GetRegisterData(); rd != nil {
		// google.protobuf.Struct → JSON via protojson is the only way to correctly
		// serialize NullValue / NumberValue / nested-Struct.
		rd = redactSecretOutput(rd, ev.GetSecretOutput())
		if b, err := protojson.Marshal(rd); err != nil {
			s.deps.Logger.Warn("applysink: register_data marshal failed",
				slog.String("sid", sid),
				slog.String("apply_id", ev.GetApplyId()),
				slog.Any("error", err))
		} else {
			in.RegisterData = string(b)
		}
	}

	if err := s.deps.Audit.Write(ctx, &audit.Event{
		EventType:     audit.EventTaskExecuted,
		Source:        s.deps.Source,
		CorrelationID: ev.GetApplyId(),
		Payload:       audit.BuildTaskExecutedPayload(in),
		CreatedAt:     time.Now().UTC(),
	}); err != nil {
		s.deps.Logger.Warn("applysink: audit write task.executed failed",
			slog.String("sid", sid),
			slog.String("apply_id", ev.GetApplyId()),
			slog.Any("error", err))
	}
}

// recordRunNotices appends a task's advisory findings to the host's `apply_runs`
// row (NIM-237, migration 107), so a deprecation outlives the live SSE stream
// and the operator who was not watching can still find it on the run.
//
// Storage is Postgres for the same reason as the failure reason: on a
// multi-Keeper cluster (ADR-002) this TaskEvent may land on a different instance
// than the one holding the run-goroutine, and a shared table survives that. The
// write is a blind append (see applyrun.AppendRunNotices); duplicates across
// tasks are collapsed on read.
//
// Never redacted: a notice carries manifest metadata (param name, versions, the
// replacement's name) and never a param VALUE, so a credential cannot travel this
// way. Suppressing it would blind the operator exactly on the tasks that handle
// secrets — the ones where a silent contract change is least affordable.
//
// DB=nil (unit build without PG / ad-hoc push) → no-op. Errors are logged and
// swallowed: a notice is advisory, and losing one must never fail the apply
// stream that carries the actual work.
func (s *Sink) recordRunNotices(ctx context.Context, sid string, ev *keeperv1.TaskEvent) {
	if s.deps.DB == nil || len(ev.GetNotices()) == 0 {
		return
	}
	notices := make([]applyrun.RunNotice, 0, len(ev.GetNotices()))
	for _, n := range ev.GetNotices() {
		notices = append(notices, applyrun.RunNotice{
			Code:    n.GetCode(),
			Module:  n.GetModule(),
			Param:   n.GetParam(),
			Message: n.GetMessage(),
		})
	}
	if err := applyrun.AppendRunNotices(ctx, s.deps.DB, ev.GetApplyId(), sid, int(ev.GetPassage()), notices); err != nil {
		s.deps.Logger.Warn("applysink: append run notices failed",
			slog.String("sid", sid),
			slog.String("apply_id", ev.GetApplyId()),
			slog.Any("error", err))
	}
}

// recordTaskFailure records the failure reason of the host's first failed task in
// the `apply_runs` row (BUG-3): task index, module name, and the text of
// `TaskError.Message`. This way an operator hitting `GET /v1/incarnations/<name>` sees
// the specific step and reason (`task 0 core.pkg.installed: E: Version '7.2.4' not
// found`), instead of a bare `RUN_STATUS_FAILED`.
//
// Storage is Postgres (NOT in-memory): a TaskEvent may have arrived at a different
// Keeper instance than the one holding the run-goroutine (ADR-002); a shared table
// survives cross-Keeper routing. first-failure-wins is guaranteed by
// [applyrun.RecordTaskFailure] (COALESCE) — there's no race when several tasks fail.
//
// Masking: error_summary is read externally via barrier/status_details (GET
// incarnation, unmasked on that channel), so MaskSecrets is applied here, on the
// write path. Since [ADR-0083] §8 removed the per-task `no_log:`, that masking is
// the whole barrier on this channel.
//
// Triggers only on FAILED/TIMED_OUT (TaskError is populated only there, see
// apply.proto). DB=nil → no-op. ErrApplyRunNotFound (a push without a
// scenario-runner, or a TaskEvent that raced ahead of Insert) → log+skip: the
// reason is lost, but we don't fail the apply stream.
func (s *Sink) recordTaskFailure(ctx context.Context, sid string, ev *keeperv1.TaskEvent) {
	if s.deps.DB == nil {
		return
	}
	if !isFailedStatus(ev.GetStatus()) {
		return
	}
	taskIdx := int(ev.GetTaskIdx())
	if taskIdx < 0 {
		return
	}

	summary := composeTaskErrorSummary(taskIdx, ev.GetError())
	// passage (ADR-056): the failure reason is written into the (apply_id, sid, passage)
	// row of this Passage; N=1 → 0. The Soul echoes passage from ApplyRequest.
	//
	// plan_index (ADR-056 §S1 fix Variant B): the GLOBAL cross-plan index of the failed
	// task, written into apply_runs.failed_plan_index — the correlation key linking the
	// failure to the plan in the barrier (scenario.failureReason).
	if err := applyrun.RecordTaskFailure(ctx, s.deps.DB, ev.GetApplyId(), sid, int(ev.GetPassage()), taskIdx, int(ev.GetPlanIndex()), summary); err != nil {
		s.deps.Logger.Warn("applysink: record task failure failed",
			slog.String("sid", sid),
			slog.String("apply_id", ev.GetApplyId()),
			slog.Int("task_idx", taskIdx),
			slog.Int64("plan_index", int64(ev.GetPlanIndex())),
			slog.Any("error", err))
	}
}

// isFailedStatus — true for terminal task statuses where TaskError is populated
// (FAILED / TIMED_OUT, see apply.proto). TIMED_OUT is a special case of failed.
func isFailedStatus(st keeperv1.TaskStatus) bool {
	return st == keeperv1.TaskStatus_TASK_STATUS_FAILED || st == keeperv1.TaskStatus_TASK_STATUS_TIMED_OUT
}

// composeTaskErrorSummary builds an operator-facing string of the task failure
// reason: `task <idx> <module>: <message>`. message is passed through
// [audit.MaskSecrets] (vault-ref / secret-shaped values from stderr don't leak into
// the observable channel). Empty module/message are omitted, so we don't produce
// `task 3 : `.
//
// This is what the barrier reads back out of `apply_runs.error_summary` and
// shows the operator verbatim (scenario.failureReason), so the format is a
// contract with a reader, not an internal detail.
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
// secret-shaped substring → ***MASKED***). audit only exposes masking for a map
// payload, so we wrap the string in a map and pull it back out.
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
// (migration 022): after the barrier, the scenario-runner reads what's accumulated
// and builds RenderInput.Register per-host for the next Passage's render
// (staged-render, ADR-056).
//
// ★ This is the write NIM-880 is about. It has a foreign key to
// `apply_runs(apply_id, sid, passage)`, so it only lands for a transport that
// minted that row — which the scenario dispatcher does for BOTH of its branches,
// and a bare `POST /v1/push/apply` run does for neither (it writes `push_runs`
// and has no `register:` consumer).
//
// register_name isn't known here (the proto only carries indices, ADR-012(d)) — we
// store by plan_index; the name is resolved by the scenario-runner from its own
// []RenderedTask. Storage is Postgres (NOT in-memory): on multi-Keeper (ADR-002)
// this TaskEvent may have arrived at a different instance than the one holding the
// run-goroutine.
//
// DB=nil → no-op. Empty register_data (a task without register:) → no-op. A write
// error is only logged: the scenario-runner treats a missing row as an absent
// register value, and a failure of this write must not fail the apply stream.
func (s *Sink) accumulateRegister(ctx context.Context, sid string, ev *keeperv1.TaskEvent) {
	if s.deps.DB == nil {
		return
	}
	rd := ev.GetRegisterData()
	if rd == nil {
		return
	}
	if err := applyrun.UpsertTaskRegister(ctx, s.deps.DB, &applyrun.TaskRegister{
		ApplyID: ev.GetApplyId(),
		SID:     sid,
		// plan_index (ADR-056 §S1 fix Variant B): the GLOBAL cross-plan index of the
		// task across the whole plan — the register correlation key (migration 079).
		// task_idx (the local position in its Passage) isn't unique across Passages or
		// across hosts, so it can't serve as the key. N=1 / an old Soul → 0 = task_idx.
		PlanIndex:    int(ev.GetPlanIndex()),
		TaskIdx:      int(ev.GetTaskIdx()),
		RegisterData: rd.AsMap(),
		// passage (ADR-056): the FK on apply_runs requires passage to match the task
		// row of this Passage.
		Passage: int(ev.GetPassage()),
	}); err != nil {
		s.deps.Logger.Warn("applysink: accumulate register_data failed",
			slog.String("sid", sid),
			slog.String("apply_id", ev.GetApplyId()),
			slog.Int64("task_idx", int64(ev.GetTaskIdx())),
			slog.Any("error", err))
	}
}

// publishTaskExecuted translates a TaskEvent into the SSE channel via applybus.
// Pure best-effort: Bus=nil (single-Keeper dev without SSE) → no-op.
//
// Payload is the SSE contract: snake_case keys, fixed in
// docs/keeper/mcp-tools.md → § SSE event payloads.
//
// Suppressing raw stderr on operator-SSE (BUG-3 floor): for a FAILED task,
// `error.message` (= stderr, which may carry a plaintext credential that
// MaskSecrets can't catch by vault-ref) is NOT placed into SSE at all — the frame
// only carries code/module for triage. The operator gets the detailed reason via
// `status_details`/GET, which passes a second MaskSecrets pass (see
// scenario.failureReason). This is a floor for ALL failed tasks, and since
// [ADR-0083] §8 removed the per-task `no_log:` it is the only stderr barrier on
// this channel.
//
// For NON-failed tasks (ok/changed), `error` is absent (TaskError is populated only
// on FAILED/TIMED_OUT, see apply.proto). The final MaskSecrets pass on the SSE write
// path (writeSSEEvent) remains as a second barrier for register secrets.
func (s *Sink) publishTaskExecuted(sid string, ev *keeperv1.TaskEvent) {
	if s.deps.Bus == nil {
		return
	}
	payload := map[string]any{
		"apply_id":    ev.GetApplyId(),
		"kind":        string(applybus.KindTaskExecuted),
		"sid":         sid,
		"task_idx":    ev.GetTaskIdx(),
		"task_status": ev.GetStatus().String(),
		// passage (ADR-056): the staged-render Passage index. 0 = the only Passage.
		"passage": ev.GetPassage(),
	}
	if e := ev.GetError(); e != nil {
		// message (stderr) is intentionally not forwarded to SSE: see the doc-comment.
		payload["error"] = map[string]any{
			"code":   e.GetCode(),
			"module": e.GetModule(),
		}
	}
	// notices (NIM-237) carry their message, unlike error: the text is rendered from
	// the manifest (param name, versions, replacement), never from task output, so the
	// stderr hazard that keeps error.message off this channel does not apply. Without
	// the sentence the frame would say "something is deprecated" and send the operator
	// hunting for what.
	if len(ev.GetNotices()) > 0 {
		out := make([]map[string]any, 0, len(ev.GetNotices()))
		for _, n := range ev.GetNotices() {
			out = append(out, map[string]any{
				"code":    n.GetCode(),
				"module":  n.GetModule(),
				"param":   n.GetParam(),
				"message": n.GetMessage(),
			})
		}
		payload["notices"] = out
	}
	s.deps.Bus.Publish(applybus.Event{
		ApplyID: ev.GetApplyId(),
		Kind:    applybus.KindTaskExecuted,
		Payload: payload,
	})
}
