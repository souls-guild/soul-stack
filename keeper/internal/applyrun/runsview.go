package applyrun

import (
	"context"
	"fmt"
	"time"
)

// Read-view of incarnation runs for the Operator API (GET /v1/incarnations/{name}/runs
// and .../runs/{apply_id}). Separated from write-CRUD (crud.go): here only
// aggregating SELECTs for the UI "execution status / current job" display.
//
// DATA BOUNDARY. apply_runs stores status PER HOST-ROW (planned…orphaned),
// NOT per-task changed/ok/skipped: TaskEvent is aggregated on the Soul side without
// per-task progress in MVP (ADR-012). The only per-task detail in PG is a failed task
// (task_idx / failed_plan_index / error_summary on failed rows). Therefore,
// "run details" are a slice by hosts (which hosts are running/failed/succeeded +
// the address of the failed task), not a full list of tasks with their statuses.

// RunStatus is the aggregate status of the ENTIRE run (aggregation of host rows
// from apply_runs). NOT to be confused with [Status] (status of a single host row)
// or incarnation.Status (error_locked is incarnation state, not run state).
type RunStatus string

const (
	// RunStatusApplying indicates at least one host row is not yet terminal
	// (planned/claimed/running/dispatched). The run is in progress — "current job".
	RunStatusApplying RunStatus = "applying"
	// RunStatusSuccess indicates all host rows are terminal and benign (success/no_match).
	RunStatusSuccess RunStatus = "success"
	// RunStatusFailed indicates all host rows are terminal with at least one
	// failed/orphaned (non-success). Takes priority over cancelled.
	RunStatusFailed RunStatus = "failed"
	// RunStatusCancelled indicates all host rows are terminal with cancelled,
	// but no failed/orphaned rows.
	RunStatusCancelled RunStatus = "cancelled"
)

// RunSummary represents a single row in a run list (GET .../runs and global
// GET /v1/runs). Aggregates all host×passage rows for one apply_id: aggregate status,
// time boundaries, initiator. Incarnation is the run's owner (in per-incarnation
// queries it matches the argument, in global queries it comes from apply_id rows).
type RunSummary struct {
	ApplyID     string
	Incarnation string
	// Service is the service of the owning incarnation (JOIN incarnation; "" if
	// incarnation is not accessible). Populated only by global ListRuns.
	Service   string
	Scenario  string
	Status    RunStatus
	StartedAt time.Time
	// FinishedAt is NULL while at least one host row has not finished (run is
	// applying); otherwise MAX(finished_at) across rows.
	FinishedAt   *time.Time
	StartedByAID *string
}

// RunHostStatus represents the status of a single host within a run
// (GET .../runs/{apply_id}). One host corresponds to N rows (by Passage from staged-render);
// the projection carries the per-passage row as-is — the UI sees the failed task address
// per passage.
//
// FailedTaskIdx is the LOCAL index of the failed task in its Passage's ApplyRequest
// (nil on success/still-running/dispatch-level failure). FailedPlanIndex is the
// GLOBAL end-to-end plan_index of the same task across the entire plan (correlation key
// with the scenario plan; nil under the same conditions). ErrorSummary is the
// operator-facing reason (`task <idx> <module>: <message>`, secret-masked on the write path).
type RunHostStatus struct {
	SID             string
	Status          Status
	Passage         int
	FailedTaskIdx   *int
	FailedPlanIndex *int
	ErrorSummary    *string
	Attempt         int32
	CancelRequested bool
}

// RunDetail represents the details of a single run (GET .../runs/{apply_id}):
// header + slice by hosts. Scenario/StartedAt/StartedByAID are taken from any host row
// (identical across apply_id); Status is the aggregate of Hosts.
type RunDetail struct {
	ApplyID      string
	Scenario     string
	Status       RunStatus
	StartedAt    time.Time
	FinishedAt   *time.Time
	StartedByAID *string
	Hosts        []RunHostStatus
}

// AggregateRunStatus collapses the statuses of host rows for a run into [RunStatus].
// Priority order (corresponds to barrier classification, dispatch.go classify):
// any non-terminal → applying; else failed/orphaned → failed; else cancelled
// → cancelled; else (only success/no_match) → success. Empty slice → applying
// (rows not yet dispatched — run not complete).
func AggregateRunStatus(statuses []Status) RunStatus {
	if len(statuses) == 0 {
		return RunStatusApplying
	}
	var hasFailure, hasCancelled bool
	for _, s := range statuses {
		switch s {
		case StatusSuccess, StatusNoMatch:
			// benign terminal.
		case StatusFailed, StatusOrphaned:
			hasFailure = true
		case StatusCancelled:
			hasCancelled = true
		default:
			// planned/claimed/running/dispatched are non-terminal: run is in progress.
			return RunStatusApplying
		}
	}
	switch {
	case hasFailure:
		return RunStatusFailed
	case hasCancelled:
		return RunStatusCancelled
	default:
		return RunStatusSuccess
	}
}

const listRunsByIncarnationSQL = `
SELECT apply_id,
       MIN(scenario)                                   AS scenario,
       MIN(started_at)                                 AS started_at,
       CASE WHEN bool_and(finished_at IS NOT NULL)
            THEN MAX(finished_at) END                  AS finished_at,
       MIN(started_by_aid)                             AS started_by_aid,
       array_agg(status)                               AS statuses
FROM apply_runs
WHERE incarnation_name = $1
GROUP BY apply_id
ORDER BY MIN(started_at) DESC, apply_id DESC
LIMIT $2 OFFSET $3
`

const countRunsByIncarnationSQL = `
SELECT COUNT(DISTINCT apply_id) FROM apply_runs WHERE incarnation_name = $1
`

// ListRunsByIncarnation returns a page of incarnation runs (aggregated by apply_id),
// with newest first (MIN(started_at) DESC), and the total run count. scenario/
// started_by_aid are identical across apply_id — we take MIN as a deterministic
// representative. finished_at is MAX across rows, but only when ALL have finished
// (otherwise NULL: run still applying). statuses is the set of host statuses for
// aggregation by [AggregateRunStatus].
//
// scope-gate is at the handler level (incarnation ownership is checked before
// calling: WHERE by incarnation_name already narrows the query to one incarnation).
func ListRunsByIncarnation(ctx context.Context, db ExecQueryRower, incarnationName string, offset, limit int) ([]RunSummary, int, error) {
	var total int
	if err := db.QueryRow(ctx, countRunsByIncarnationSQL, incarnationName).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("applyrun: count runs: %w", err)
	}

	rows, err := db.Query(ctx, listRunsByIncarnationSQL, incarnationName, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("applyrun: list runs: %w", err)
	}
	defer rows.Close()

	out := make([]RunSummary, 0, limit)
	for rows.Next() {
		var (
			rs         RunSummary
			statusStrs []string
		)
		if err := rows.Scan(&rs.ApplyID, &rs.Scenario, &rs.StartedAt, &rs.FinishedAt, &rs.StartedByAID, &statusStrs); err != nil {
			return nil, 0, fmt.Errorf("applyrun: scan run summary: %w", err)
		}
		rs.Incarnation = incarnationName
		rs.Status = AggregateRunStatus(toStatuses(statusStrs))
		out = append(out, rs)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("applyrun: iterate runs: %w", err)
	}
	return out, total, nil
}

const selectRunHostsSQL = `
SELECT sid, status, passage, task_idx, failed_plan_index, error_summary,
       attempt, cancel_requested, scenario, started_at, finished_at, started_by_aid
FROM apply_runs
WHERE apply_id = $1 AND incarnation_name = $2
ORDER BY sid ASC, passage ASC
`

// SelectRunDetail returns the details of a single run: all host×passage rows for an
// apply_id belonging to the specified incarnation (WHERE on both — enforcement of
// "apply_id of this incarnation"; foreign apply_id returns 0 rows → [ErrApplyRunNotFound]).
// Header (scenario/started_at/started_by_aid) comes from the first row (identical
// across apply_id); finished_at detail is MAX across rows, but nil until all have
// finished; Status is the aggregate of Hosts.
func SelectRunDetail(ctx context.Context, db ExecQueryRower, applyID, incarnationName string) (*RunDetail, error) {
	rows, err := db.Query(ctx, selectRunHostsSQL, applyID, incarnationName)
	if err != nil {
		return nil, fmt.Errorf("applyrun: run detail query: %w", err)
	}
	defer rows.Close()

	var (
		detail   RunDetail
		statuses []Status
		// Run finality: expose FinishedAt only when every row has finished (max of
		// non-nil; nil if at least one row is still running).
		allFinished = true
		maxFinished time.Time
	)
	for rows.Next() {
		var (
			hs           RunHostStatus
			statusStr    string
			rowScenario  string
			rowStarted   time.Time
			rowFinished  *time.Time
			rowStartedBy *string
		)
		if err := rows.Scan(
			&hs.SID, &statusStr, &hs.Passage, &hs.FailedTaskIdx, &hs.FailedPlanIndex,
			&hs.ErrorSummary, &hs.Attempt, &hs.CancelRequested,
			&rowScenario, &rowStarted, &rowFinished, &rowStartedBy,
		); err != nil {
			return nil, fmt.Errorf("applyrun: scan run host: %w", err)
		}
		hs.Status = Status(statusStr)
		if detail.ApplyID == "" {
			detail.ApplyID = applyID
			detail.Scenario = rowScenario
			detail.StartedAt = rowStarted
			detail.StartedByAID = rowStartedBy
		}
		if rowStarted.Before(detail.StartedAt) {
			detail.StartedAt = rowStarted
		}
		if rowFinished == nil {
			allFinished = false
		} else if rowFinished.After(maxFinished) {
			maxFinished = *rowFinished
		}
		statuses = append(statuses, hs.Status)
		detail.Hosts = append(detail.Hosts, hs)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("applyrun: iterate run hosts: %w", err)
	}
	if detail.ApplyID == "" {
		return nil, ErrApplyRunNotFound
	}
	detail.Status = AggregateRunStatus(statuses)
	if allFinished {
		detail.FinishedAt = &maxFinished
	}
	return &detail, nil
}

// toStatuses converts a slice of status strings from array_agg into []Status.
func toStatuses(ss []string) []Status {
	out := make([]Status, len(ss))
	for i, s := range ss {
		out[i] = Status(s)
	}
	return out
}
