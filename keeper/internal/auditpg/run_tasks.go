package auditpg

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// TaskExecution — per-host outcome of one run's task, reconstructed from the
// audit log (`task.executed`, NIM-37). Source — the same event already
// written by events_taskevent.go for each (apply_id, sid, plan_index):
// status, register_data (output), and error. The /tasks endpoint joins this
// with the plan (apply_run_plan) via plan_index → host address within the
// task.
//
// Secret hygiene: register_data/error.message already went through
// MaskSecrets on the write path (auditpg-writer), and no_log tasks don't
// carry them at all (BuildTaskExecutedPayload suppresses them) — the reader
// returns what was written, with no additional masking.
type TaskExecution struct {
	SID string

	// PlanIndex — the GLOBAL cross-run task index (= RenderedTask.Index); the
	// join key with the plan. COALESCE(plan_index, task_idx): old rows without
	// plan_index → fall back to task_idx (N=1 matches).
	PlanIndex int

	// Status — the terminal status string name (keeperv1.TaskStatus.String(),
	// e.g. "TASK_STATUS_CHANGED").
	Status string

	// Output — parsed register_data (JSON object). nil if register_data is
	// absent (task without register:), suppressed (no_log), or failed to parse.
	Output map[string]any

	// Error — set only on FAILED/TIMED_OUT (nil otherwise). Message is empty
	// for a no_log task (suppressed on the write path).
	Error *TaskExecutionError
}

// TaskExecutionError — the error part of task.executed (code/module/message).
type TaskExecutionError struct {
	Code    string
	Module  string
	Message string
}

// selectTaskExecutionsSQL — all task.executed of a run with address fields
// (sid, plan_index), status, register_data, and error. Filter on indexed
// columns (correlation_id, event_type); JSONB fields are extracted as text.
// ORDER BY created_at ASC: on retry (several rows per task-host), the later
// one overwrites the earlier in the caller's aggregation (last result wins).
//
// $1 = apply_id (correlation_id of task.executed), $2 = event_type.
const selectTaskExecutionsSQL = `
SELECT payload->>'sid'                                      AS sid,
       COALESCE(payload->>'plan_index', payload->>'task_idx') AS plan_index,
       payload->>'status'                                   AS status,
       payload->>'register_data'                            AS register_data,
       payload->'error'->>'code'                            AS err_code,
       payload->'error'->>'module'                          AS err_module,
       payload->'error'->>'message'                         AS err_message,
       (payload ? 'error')                                  AS has_error
FROM audit_log
WHERE correlation_id = $1
  AND event_type = $2
  AND payload->>'sid' IS NOT NULL
  AND COALESCE(payload->>'plan_index', payload->>'task_idx') IS NOT NULL
ORDER BY created_at ASC
`

// SelectTaskExecutions returns per-host outcomes for run `applyID`'s tasks
// from the audit log (`task.executed`): status, output (register_data), and
// error for each (sid, plan_index). Order is by time (retry rows come
// later); dedup/picking the last result is done by the caller (the /tasks
// endpoint) when grouping by (plan_index, sid). An empty result means the run
// has no task.executed trace.
func (r *Reader) SelectTaskExecutions(ctx context.Context, applyID string) ([]TaskExecution, error) {
	rows, err := r.pool.Query(ctx, selectTaskExecutionsSQL, applyID, string(audit.EventTaskExecuted))
	if err != nil {
		return nil, fmt.Errorf("audit: task executions query: %w", err)
	}
	defer rows.Close()

	var out []TaskExecution
	for rows.Next() {
		var (
			sid         string
			planIdxStr  string
			status      *string
			registerRaw *string
			errCode     *string
			errModule   *string
			errMessage  *string
			hasError    bool
		)
		if err := rows.Scan(&sid, &planIdxStr, &status, &registerRaw, &errCode, &errModule, &errMessage, &hasError); err != nil {
			return nil, fmt.Errorf("audit: task executions scan: %w", err)
		}
		planIdx, perr := strconv.Atoi(planIdxStr)
		if perr != nil {
			// Non-numeric plan_index/task_idx (garbage in payload) — skip: one bad
			// row shouldn't fail the whole /tasks response.
			continue
		}
		te := TaskExecution{SID: sid, PlanIndex: planIdx}
		if status != nil {
			te.Status = *status
		}
		if registerRaw != nil {
			var m map[string]any
			if json.Unmarshal([]byte(*registerRaw), &m) == nil {
				te.Output = m
			}
		}
		if hasError {
			te.Error = &TaskExecutionError{}
			if errCode != nil {
				te.Error.Code = *errCode
			}
			if errModule != nil {
				te.Error.Module = *errModule
			}
			if errMessage != nil {
				te.Error.Message = *errMessage
			}
		}
		out = append(out, te)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("audit: task executions iter: %w", err)
	}
	return out, nil
}
