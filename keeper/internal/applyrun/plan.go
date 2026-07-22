package applyrun

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// RunPlanTask is a row of the "run task plan" (apply_run_plan, migration 096, NIM-37):
// the host-invariant metadata of one rendered task keyed by its global
// plan_index. name/module/no_log/passage are the same across all hosts of the
// run, so they are stored ONCE per (apply_id, plan_index); the per-host
// status/output is pulled by the /tasks endpoint from audit_log (task.executed).
type RunPlanTask struct {
	ApplyID string

	// PlanIndex is the GLOBAL cross-cutting task index over the whole run plan
	// (all Passages) = RenderedTask.Index (ADR-056 §S1 Variant B). The
	// correlation key with the per-host result in audit_log (payload.plan_index).
	PlanIndex int

	Name   string
	Module string
	NoLog  bool

	// Passage is the Passage index of the staged render (ADR-056); N=1 → 0.
	Passage int

	// Params is the JSON of the task's operator input parameters (NIM-37 S1b),
	// already masked by the seal-aware mechanism on the write path
	// (scenario.persistRunPlan). nil/empty → jsonb NULL: either a no_log task
	// (params suppressed) or a task without params. Transport-only keys
	// (template_content/render_context) are filtered out before the write. The
	// store layer does NOT mask values — it only persists/reads them as-is.
	Params []byte
}

// insertRunPlanSQL is a bulk-upsert of the plan in a single query via unnest
// arrays (apply_id shared, the remaining columns are parallel arrays). ON
// CONFLICT DO UPDATE is idempotent: a staged run re-invokes Render per
// Passage, but the plan (Index/name/module/no_log/passage) is stable — a
// repeat write is harmless.
const insertRunPlanSQL = `
INSERT INTO apply_run_plan (apply_id, plan_index, name, module, no_log, passage, params)
SELECT $1, u.plan_index, u.name, u.module, u.no_log, u.passage, u.params::jsonb
FROM unnest($2::int[], $3::text[], $4::text[], $5::bool[], $6::int[], $7::text[])
     AS u(plan_index, name, module, no_log, passage, params)
ON CONFLICT (apply_id, plan_index)
DO UPDATE SET name = EXCLUDED.name, module = EXCLUDED.module, no_log = EXCLUDED.no_log, passage = EXCLUDED.passage, params = EXCLUDED.params
`

// InsertRunPlan writes the run's task plan (apply_run_plan) in a single
// bulk-upsert. Empty tasks → no-op (nothing to write). A non-empty ApplyID is
// required. Called once at dispatch (scenario-runner) — the plan is
// host-invariant.
func InsertRunPlan(ctx context.Context, db ExecQueryRower, applyID string, tasks []RunPlanTask) error {
	if applyID == "" {
		return fmt.Errorf("applyrun: empty apply_id")
	}
	if len(tasks) == 0 {
		return nil
	}

	planIdx := make([]int, len(tasks))
	names := make([]string, len(tasks))
	modules := make([]string, len(tasks))
	noLogs := make([]bool, len(tasks))
	passages := make([]int, len(tasks))
	// params is a parallel text[] array (each element is JSON or NULL), cast
	// to jsonb in SQL. A nil element (no_log / no params) → jsonb NULL.
	params := make([]*string, len(tasks))
	for i, t := range tasks {
		planIdx[i] = t.PlanIndex
		names[i] = t.Name
		modules[i] = t.Module
		noLogs[i] = t.NoLog
		passages[i] = t.Passage
		if len(t.Params) > 0 {
			s := string(t.Params)
			params[i] = &s
		}
	}

	if _, err := db.Exec(ctx, insertRunPlanSQL, applyID, planIdx, names, modules, noLogs, passages, params); err != nil {
		return fmt.Errorf("applyrun: insert run plan: %w", err)
	}
	return nil
}

const selectRunPlanByApplyIDSQL = `
SELECT plan_index, name, module, no_log, passage, params
FROM apply_run_plan
WHERE apply_id = $1
ORDER BY plan_index ASC
`

// SelectRunPlanByApplyID returns the run's task plan, sorted by the global
// plan_index. An empty result means a run with no persisted plan (it failed
// before render, or predates NIM-37): the caller treats this as an empty plan.
func SelectRunPlanByApplyID(ctx context.Context, db ExecQueryRower, applyID string) ([]RunPlanTask, error) {
	rows, err := db.Query(ctx, selectRunPlanByApplyIDSQL, applyID)
	if err != nil {
		return nil, fmt.Errorf("applyrun: run plan query: %w", err)
	}
	defer rows.Close()

	var out []RunPlanTask
	for rows.Next() {
		t := RunPlanTask{ApplyID: applyID}
		if err := rows.Scan(&t.PlanIndex, &t.Name, &t.Module, &t.NoLog, &t.Passage, &t.Params); err != nil {
			return nil, fmt.Errorf("applyrun: run plan scan: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("applyrun: run plan iter: %w", err)
	}
	return out, nil
}

const runExistsForIncarnationSQL = `
SELECT EXISTS(SELECT 1 FROM apply_runs WHERE apply_id = $1 AND incarnation_name = $2)
`

// RunExistsForIncarnation reports whether incarnation `name` has a run
// `applyID`. The scope guard for the /tasks endpoint: apply_run_plan carries
// no incarnation_name, so the run's ownership by the incarnation is checked
// against apply_runs (otherwise a foreign apply_id would return a foreign
// plan). Absence → the caller returns 404 (parity with SelectRunDetail).
func RunExistsForIncarnation(ctx context.Context, db ExecQueryRower, applyID, name string) (bool, error) {
	var exists bool
	if err := db.QueryRow(ctx, runExistsForIncarnationSQL, applyID, name).Scan(&exists); err != nil {
		if err == pgx.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf("applyrun: run exists probe: %w", err)
	}
	return exists, nil
}
