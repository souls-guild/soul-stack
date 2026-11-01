package applyrun

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// RunTaskTime is one row of apply_run_plan_host_times (migration 137, NIM-1170):
// when one host started and finished one task of a run, by the database clock.
// A row exists only for a task the Keeper ran itself or sent to the host. nil
// StartedAt is a task sent and not started yet, nil FinishedAt one not finished.
type RunTaskTime struct {
	PlanIndex  int
	SID        string
	StartedAt  *time.Time
	FinishedAt *time.Time
}

// The first task (lowest plan_index) starts now, the rest wait for the TaskEvent of
// the task before them. A repeat send — a re-claimed step, a send retried once the
// host has a stream — restarts the host's rows, since the host runs them anew.
const startHostTasksSQL = `
WITH sent AS (
    SELECT u.plan_index FROM unnest($3::int[]) AS u(plan_index)
)
INSERT INTO apply_run_plan_host_times (apply_id, plan_index, sid, started_at, finished_at)
SELECT $1, s.plan_index, $2,
       CASE WHEN s.plan_index = (SELECT min(plan_index) FROM sent) THEN now() END,
       NULL
FROM sent s
ON CONFLICT (apply_id, plan_index, sid)
DO UPDATE SET started_at = EXCLUDED.started_at, finished_at = NULL
`

// StartHostTasks records that sid was sent (or, for a Keeper task, began) the tasks
// planIndexes of run applyID: one row per task, the lowest started now. The plan
// rows must already exist: the table references apply_run_plan. Empty → no-op.
func StartHostTasks(ctx context.Context, db ExecQueryRower, applyID, sid string, planIndexes []int) error {
	if applyID == "" {
		return fmt.Errorf("applyrun: empty apply_id")
	}
	if len(planIndexes) == 0 {
		return nil
	}
	// An upsert that meets one row twice in a statement fails the whole statement.
	idx := slices.Clone(planIndexes)
	slices.Sort(idx)
	idx = slices.Compact(idx)
	if _, err := db.Exec(ctx, startHostTasksSQL, applyID, sid, idx); err != nil {
		return fmt.Errorf("applyrun: start host tasks: %w", err)
	}
	return nil
}

// Finishes the row, then starts the host's next unstarted row of the same step when
// the host's own flow is at this task: only when this statement finished the row —
// a duplicate TaskEvent, or the record the Keeper writes for a task it never sent
// (no row), must not start a task the host has not reached — and only when no later
// row of the step has started or finished. An `async:` task runs beside the tasks
// after it and reports when it ends, often after they have; starting the next row
// then would start a task the host has not reached, and every later start would
// come one task early. The step bounds both lookups because a step's plan indexes
// need not all follow an earlier step's (a staged Passage). All parts read the same
// snapshot, so the subqueries still see the finished row as it was; plan_index > $3
// keeps it out anyway.
const finishHostTaskSQL = `
WITH done AS (
    UPDATE apply_run_plan_host_times
    SET finished_at = now()
    WHERE apply_id = $1 AND sid = $2 AND plan_index = $3 AND finished_at IS NULL
    RETURNING plan_index
),
step AS (
    SELECT plan_index FROM apply_run_plan
    WHERE apply_id = $1
      AND passage = (SELECT passage FROM apply_run_plan WHERE apply_id = $1 AND plan_index = $3)
)
UPDATE apply_run_plan_host_times t
SET started_at = now()
FROM done
WHERE $4::boolean
  AND t.apply_id = $1 AND t.sid = $2
  AND t.plan_index = (
      SELECT min(n.plan_index) FROM apply_run_plan_host_times n
      WHERE n.apply_id = $1 AND n.sid = $2 AND n.plan_index > $3
        AND n.plan_index IN (SELECT plan_index FROM step)
        AND n.started_at IS NULL AND n.finished_at IS NULL
  )
  AND NOT EXISTS (
      SELECT 1 FROM apply_run_plan_host_times a
      WHERE a.apply_id = $1 AND a.sid = $2 AND a.plan_index > $3
        AND a.plan_index IN (SELECT plan_index FROM step)
        AND (a.started_at IS NOT NULL OR a.finished_at IS NOT NULL)
  )
`

// FinishHostTask records that sid finished task planIndex of run applyID and, with
// startNext, starts the host's next task it was sent. startNext is false for a task
// that ended its host's run — a cancelled one: the host runs nothing after it. A
// task with no row is left alone.
func FinishHostTask(ctx context.Context, db ExecQueryRower, applyID, sid string, planIndex int, startNext bool) error {
	if _, err := db.Exec(ctx, finishHostTaskSQL, applyID, sid, planIndex, startNext); err != nil {
		return fmt.Errorf("applyrun: finish host task: %w", err)
	}
	return nil
}

const forgetHostTasksSQL = `
DELETE FROM apply_run_plan_host_times
WHERE apply_id = $1 AND sid = $2 AND plan_index = ANY($3::int[])
`

// ForgetHostTasks drops the rows [StartHostTasks] wrote for a send that failed, so
// none of its tasks reads as started. A TaskEvent arriving for them after all
// finds no row and is left alone.
func ForgetHostTasks(ctx context.Context, db ExecQueryRower, applyID, sid string, planIndexes []int) error {
	if len(planIndexes) == 0 {
		return nil
	}
	if _, err := db.Exec(ctx, forgetHostTasksSQL, applyID, sid, planIndexes); err != nil {
		return fmt.Errorf("applyrun: forget host tasks: %w", err)
	}
	return nil
}

// A host whose run ended with tasks it never reported — its stream broke, the
// Keeper gave up waiting — leaves rows no TaskEvent will close: they are read
// against the host's apply_runs row for the task's step. Once that row is
// terminal, a task that started finished when the host's run did, and one that
// never started is not returned. A host still running is read as stamped.
var selectRunTaskTimesSQL = `
SELECT t.plan_index, t.sid, t.started_at,
       CASE WHEN t.finished_at IS NULL AND r.status IN (` + terminalStatusesSQL() + `)
            THEN r.finished_at ELSE t.finished_at END
FROM apply_run_plan_host_times t
JOIN apply_run_plan p ON p.apply_id = t.apply_id AND p.plan_index = t.plan_index
LEFT JOIN apply_runs r ON r.apply_id = t.apply_id AND r.sid = t.sid AND r.passage = p.passage
WHERE t.apply_id = $1
  AND NOT (t.started_at IS NULL AND t.finished_at IS NULL
           AND COALESCE(r.status IN (` + terminalStatusesSQL() + `), false))
ORDER BY t.plan_index ASC, t.sid ASC
`

// SelectRunTaskTimes returns the run's task time rows, ordered by plan_index then
// SID, settled against the hosts' runs as selectRunTaskTimesSQL says. A run that
// predates NIM-1170 has none.
func SelectRunTaskTimes(ctx context.Context, db ExecQueryRower, applyID string) ([]RunTaskTime, error) {
	rows, err := db.Query(ctx, selectRunTaskTimesSQL, applyID)
	if err != nil {
		return nil, fmt.Errorf("applyrun: run task times query: %w", err)
	}
	defer rows.Close()

	var out []RunTaskTime
	for rows.Next() {
		var r RunTaskTime
		if err := rows.Scan(&r.PlanIndex, &r.SID, &r.StartedAt, &r.FinishedAt); err != nil {
			return nil, fmt.Errorf("applyrun: run task times scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("applyrun: run task times iter: %w", err)
	}
	return out, nil
}
