package auditpg

import (
	"context"
	"fmt"
	"strconv"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// ChangedTaskKey — identifier of "this task changed on this host":
// (sid, plan_index) for one run. Source — audit log (event_type
// `task.executed` with `payload.status == TASK_STATUS_CHANGED`), NOT a
// separate table: the changed-fact is already recorded by the TaskEvent handler
// (M2.4, events_taskevent.go) for each (apply_id, sid, plan_index).
//
// PlanIndex — the GLOBAL cross-run task index across the whole run plan (across
// all Passages), = RenderedTask.Index (ADR-056 §S1 fix Variant B, T3 channel).
// The correlation key with the plan in buildChangedTasks (scenario.state) goes
// by RenderedTask.Index — so the global `plan_index` is taken from payload, NOT
// the local `task_idx` (under staged/per-host-where it can differ from the
// global one, pointing at a neighboring task → mismatch in the state_changes
// whitelist (secret hygiene) + audit). Old audit rows without `plan_index` →
// fall back to `task_idx` (N=1 matches).
//
// Secret hygiene (T3): only (sid, plan_index) are read from audit_log — the
// address of the fact, without payload values register_data/params/error. Task
// metadata (name/register/id/module) is fetched by the scenario-runner from the
// in-memory []RenderedTask, not from the log.
type ChangedTaskKey struct {
	SID       string
	PlanIndex int
}

// taskStatusChanged — `payload.status` string value for a CHANGED task.
// The TaskEvent handler (events_taskevent.go) stores the status as
// `Status().String()`, i.e. the enum constant name
// keeperv1.TaskStatus_TASK_STATUS_CHANGED. We filter by this string (not by
// number) — it's stored as text in the JSONB payload.
const taskStatusChanged = "TASK_STATUS_CHANGED"

// taskStatusFailed / taskStatusTimedOut — `payload.status` string values for a
// FAILED task (FAILED ∪ TIMED_OUT). TIMED_OUT is a special case of failed
// (apply.proto: "is a special case of failed"), so onfail-gating must treat
// both as "source failed". Names match the keeperv1.TaskStatus enum constants.
const (
	taskStatusFailed   = "TASK_STATUS_FAILED"
	taskStatusTimedOut = "TASK_STATUS_TIMED_OUT"
)

// selectTaskKeysByStatusSQL — selects the addresses of a run's tasks in a given
// status set from the audit log. Filter strictly on indexed columns
// (correlation_id, event_type) + a JSONB predicate on status; sid/plan_index
// are extracted from payload as text and number (the handler stores both as
// numbers, JSONB ->> returns the text form — parsed by the caller). Parameters
// are positional placeholders, no value concatenation into SQL.
//
// plan_index (ADR-056 §S1 fix Variant B, T3): COALESCE(plan_index, task_idx) —
// prioritizes the global cross-run index (= RenderedTask.Index, the correlation
// key with the plan). Old audit rows without `plan_index` (run predating the T3
// fix / old Soul) → fall back to the local `task_idx`, which for N=1 matches
// the global one (bit-for-bit behavior). COALESCE over JSONB text extraction:
// `payload->>'key'` returns NULL when the key is absent — the next argument is
// used.
//
// $1 = apply_id (correlation_id of task.executed), $2 = event_type, $3 = the
// set of status names (`= ANY($3)`, text array). The filter requires at least
// one of (plan_index, task_idx) to be present: COALESCE of both IS NOT NULL.
// NULLIF on sid isn't needed — the handler always sets sid. DISTINCT isn't
// needed: the pair (sid, plan_index) is unique per task's final status in
// task.executed, but a retry may produce multiple rows — dedup is done by the
// caller (a set). One SQL query serves both the CHANGED and FAILED selections —
// they differ only in the set of status names ($3).
const selectTaskKeysByStatusSQL = `
SELECT payload->>'sid' AS sid,
       COALESCE(payload->>'plan_index', payload->>'task_idx') AS plan_index
FROM audit_log
WHERE correlation_id = $1
  AND event_type = $2
  AND payload->>'status' = ANY($3)
  AND payload->>'sid' IS NOT NULL
  AND COALESCE(payload->>'plan_index', payload->>'task_idx') IS NOT NULL
`

// SelectChangedTaskKeys returns the set of (sid, plan_index) for tasks of run
// `applyID` that terminated with status CHANGED (per the audit log). Source —
// `task.executed` events with `payload.status == TASK_STATUS_CHANGED`; ONLY
// address fields (sid, plan_index) are read, payload values are NOT read
// (secret hygiene T3).
//
// plan_index — the GLOBAL cross-run task index (= RenderedTask.Index); under
// staged/per-host-where it, not the local task_idx, correlates with the plan in
// scenario.buildChangedTasks (key ChangedTaskKey{sid, t.Index}). Old audit rows
// without `plan_index` are read with a fallback to `task_idx` (COALESCE in
// SQL), which for N=1 matches the global one.
//
// The result is a set: a duplicate (apply_id, sid, plan_index) (retry
// overwrote the status, producing a second task.executed row) collapses. An
// empty result means no task changed (or the run has no task.executed trace).
// A plan_index that fails to parse as int (garbage in payload) is skipped
// without error — that's an observability degradation, not a reason to fail
// the run's outcome.
func (r *Reader) SelectChangedTaskKeys(ctx context.Context, applyID string) (map[ChangedTaskKey]struct{}, error) {
	return r.selectTaskKeysByStatus(ctx, applyID, []string{taskStatusChanged})
}

// SelectFailedTaskKeys returns the set of (sid, plan_index) for tasks of run
// `applyID` that terminated with status FAILED or TIMED_OUT (per the audit
// log). Mirrors [SelectChangedTaskKeys] for onfail-rescue-gating (ADR-056 R3,
// cross-passage): keeper resolves cross-passage `onfail:[A]` by whether A
// failed on the host. TIMED_OUT is included — apply.proto declares it a
// special case of failed, so rescue must also trigger on a timeout source.
// Same secret hygiene: ONLY address fields (sid, plan_index) are read.
func (r *Reader) SelectFailedTaskKeys(ctx context.Context, applyID string) (map[ChangedTaskKey]struct{}, error) {
	return r.selectTaskKeysByStatus(ctx, applyID, []string{taskStatusFailed, taskStatusTimedOut})
}

// selectTaskKeysByStatus — shared (sid, plan_index) selection for a run's
// tasks by a set of terminal statuses. statuses are keeperv1.TaskStatus enum
// constant names (text in payload). Dedup via a set; non-numeric plan_index is
// skipped.
func (r *Reader) selectTaskKeysByStatus(ctx context.Context, applyID string, statuses []string) (map[ChangedTaskKey]struct{}, error) {
	rows, err := r.pool.Query(ctx, selectTaskKeysByStatusSQL,
		applyID, string(audit.EventTaskExecuted), statuses)
	if err != nil {
		return nil, fmt.Errorf("audit: task keys query: %w", err)
	}
	defer rows.Close()

	out := make(map[ChangedTaskKey]struct{})
	for rows.Next() {
		var (
			sid        string
			planIdxStr string
		)
		if err := rows.Scan(&sid, &planIdxStr); err != nil {
			return nil, fmt.Errorf("audit: task keys scan: %w", err)
		}
		planIdx, perr := strconv.Atoi(planIdxStr)
		if perr != nil {
			// Non-numeric plan_index/task_idx in payload shouldn't happen (the handler
			// stores a proto int). Skip: the run's outcome matters more than one bad row.
			continue
		}
		out[ChangedTaskKey{SID: sid, PlanIndex: planIdx}] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("audit: task keys iter: %w", err)
	}
	return out, nil
}
