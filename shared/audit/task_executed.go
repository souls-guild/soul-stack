package audit

// TaskExecutedError — the error part of the task.executed payload (populated only on
// terminals FAILED/TIMED_OUT). Code — the module error code (Soul-side TaskError.code;
// keeper-side stays empty — keeper modules carry no code). Message — the error text
// (= module stderr / message): for a no_log task it is NOT set
// (BuildTaskExecutedPayload suppresses it), otherwise it goes into the payload as is,
// masking happens on the write path (MaskSecrets in auditpg).
type TaskExecutedError struct {
	Code    string
	Module  string
	Message string
}

// TaskExecutedInput — extracted primitives for building the task.executed event
// payload. Populated by both emit points (Soul-side TaskEvent handler and keeper-side
// dispatchKeeperTasks) from their own proto/render structs — the single payload shape
// lives in [BuildTaskExecutedPayload] so the changed_tasks rollup (auditpg, by
// payload->>'sid'/'task_idx'/'status') sees both sides' tasks identically.
type TaskExecutedInput struct {
	SID     string
	ApplyID string
	// TaskIdx — the LOCAL position of the task in its Passage's ApplyRequest (echo of
	// TaskEvent.task_idx). Under staged/per-host-where ≠ the global RenderedTask.Index.
	// Stays in the payload for observability/triage, NOT the correlation key of CHANGED
	// tasks with the plan — see PlanIndex.
	TaskIdx int
	// PlanIndex — the GLOBAL end-to-end task index across the whole run plan (across
	// all Passages), = RenderedTask.Index (echo of TaskEvent.plan_index, ADR-056 §S1
	// fix Variant B). The correlation key of a CHANGED task with the plan in
	// auditpg.SelectChangedTaskKeys (state_changes whitelist + audit changed_tasks).
	// The local TaskIdx under staged/per-host-where would point at a neighboring task
	// (the same defect the register channel fixed with migration 079). keeper-side tasks
	// (`on: keeper`) run before host fan-out → PlanIndex==TaskIdx==Index. N=1 / an old
	// Soul (payload without plan_index) → correlation falls back to TaskIdx, which for
	// N=1 equals the global one.
	PlanIndex int
	// Status — the string name of the task's terminal status (keeperv1.TaskStatus.String(),
	// e.g. "TASK_STATUS_CHANGED"). The changed rollup filters by the literal
	// "TASK_STATUS_CHANGED" — both sides put the same enum name.
	Status string
	// NoLog — echo of RenderedTask.no_log: suppresses error.message and register_data
	// (the root of an arbitrary-secret leak that MaskSecrets by vault-ref doesn't catch).
	// In the payload a suppressed:"no_log" marker goes instead.
	NoLog bool
	// Error — populated only on FAILED/TIMED_OUT (nil otherwise).
	Error *TaskExecutedError
	// RegisterData — the serialized register result (Soul-side: protojson of
	// google.protobuf.Struct). Empty → the key is not set. Suppressed for no_log.
	// keeper-side puts no register_data in audit at all (secret hygiene) — leaves it empty.
	RegisterData string
	// Passage — the staged-render Passage index (ADR-056) the task belongs to (echo of
	// TaskEvent.passage). 0 = the only Passage (behavior as before staged-render). Always
	// in the payload (including 0) for per-Passage triage; keeper-side tasks (`on: keeper`)
	// run before host fan-out → passage=0.
	Passage int
}

// BuildTaskExecutedPayload assembles the task.executed audit-event payload from the
// extracted primitives — a single shape for both emit points (Soul-side
// events_taskevent.go and keeper-side keeper_dispatch.go). Keeping the assembly in one
// place is critical: the changed_tasks rollup (auditpg.SelectChangedTaskKeys) reads the
// payload by keys sid/task_idx/status, and a shape mismatch between sides would silently
// zero it out for one of them.
//
// no_log suppression (symmetric on both sides): for a no_log task error.message and
// register_data are NOT set (the root of an arbitrary-secret leak), a suppressed:"no_log"
// marker goes instead. Secret masking by vault-ref/sensitive-key happens on the write
// path (MaskSecrets in auditpg); here the payload is assembled "as is" (symmetric to the
// previous inline handler assembly).
func BuildTaskExecutedPayload(in TaskExecutedInput) map[string]any {
	payload := map[string]any{
		"sid":      in.SID,
		"apply_id": in.ApplyID,
		"task_idx": in.TaskIdx,
		// plan_index (ADR-056 §S1 fix Variant B): the GLOBAL end-to-end task index across
		// the whole plan (= RenderedTask.Index). The correlation key of a CHANGED task with
		// the plan in auditpg.SelectChangedTaskKeys; task_idx (the local position in its
		// Passage) under staged/per-host-where would point at a neighboring task. Always set
		// (additive to task_idx) — old audit rows without it are read by correlation with a
		// fallback to task_idx (N=1 → they coincide).
		"plan_index": in.PlanIndex,
		"status":     in.Status,
		"passage":    in.Passage,
	}
	if in.NoLog {
		payload["suppressed"] = "no_log"
	}
	if in.Error != nil {
		errPayload := map[string]any{
			"code":   in.Error.Code,
			"module": in.Error.Module,
		}
		// message (stderr) — only for non-no_log: for no_log it may carry a plaintext
		// secret that MaskSecrets by vault-ref doesn't catch.
		if !in.NoLog {
			errPayload["message"] = in.Error.Message
		}
		payload["error"] = errPayload
	}
	if in.RegisterData != "" && !in.NoLog {
		payload["register_data"] = in.RegisterData
	}
	return payload
}
