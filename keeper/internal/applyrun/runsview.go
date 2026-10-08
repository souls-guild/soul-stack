package applyrun

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Read-view of incarnation runs for the Operator API (GET /v1/incarnations/{id}/runs
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
	// RunStatusPartialFailed indicates the run finished without some of its hosts:
	// they failed under `on_failure: host` and the rest ran to the end. The
	// incarnation ends error_locked and a Voyage counts the run as failed.
	//
	// The host rows of such a run are the rows of a failed run; `apply_runs.dropped`
	// on the failed row of each dropped host (migration 122, [MarkDropped]) is what
	// tells the two apart.
	RunStatusPartialFailed RunStatus = "partial_failed"
	// RunStatusCancelled indicates all host rows are terminal with cancelled,
	// but no failed/orphaned rows.
	RunStatusCancelled RunStatus = "cancelled"
)

// RunStatuses returns the closed set of [RunStatus] values, in the order the API
// lists them. The filter, the stats buckets, the wire enums and the Voyage outcome
// are each checked against it, so a value added here and missed there goes red.
func RunStatuses() []RunStatus {
	return []RunStatus{
		RunStatusApplying, RunStatusSuccess, RunStatusFailed, RunStatusPartialFailed, RunStatusCancelled,
	}
}

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

// RunHostStatus represents one host row of a run (GET .../runs/{apply_id}). A host
// has one row per unit it was sent, keyed by Passage: the ADR-056 Passage today, the
// step once linear dispatch (NIM-907) is on. The projection carries each row as-is,
// so the failed task's address stays on the unit it failed in.
//
// FailedTaskIdx is the LOCAL index of the failed task in the ApplyRequest this row was
// dispatched with (nil on success/still-running/dispatch-level failure); for a host the
// Keeper failed before sending it anything (linear dispatch, NIM-943), its position in
// the slice the host was grouped with.
// FailedPlanIndex is the GLOBAL end-to-end plan_index of the same task across the
// entire plan (correlation key with the scenario plan; nil under the same
// conditions); both hold the row's FIRST failure (recordTaskFailureSQL). ErrorSummary
// is the operator-facing reason (`task <idx> <module>: <message>`, secret-masked on
// the write path).
type RunHostStatus struct {
	SID             string
	Status          Status
	Passage         int
	FailedTaskIdx   *int
	FailedPlanIndex *int
	ErrorSummary    *string
	Attempt         int32
	CancelRequested bool
	// Dropped — this failed row dropped its host from a run that finished without it
	// ([MarkDropped]).
	Dropped bool
	// Notices — advisory findings this host reported during the run (NIM-237),
	// deduplicated by (code, module, param): the same deprecated param used by
	// twenty tasks is one thing to migrate. Per-host rather than per-run because
	// the contract a param is checked against is the manifest compiled into THAT
	// agent — a park mid-upgrade legitimately answers differently host to host,
	// and flattening that would report a fleet-wide truth nobody established.
	// Stored per row, so a host with several rows (one per Passage, one per step
	// under linear dispatch) spreads its notices over them and the dedup above
	// holds within a row only: a reader wanting the host's set groups the rows by
	// SID. Empty for a run that had nothing to say.
	Notices []RunNotice
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
	// Input — masked snapshot of the operator input for the run (apply_runs.input,
	// migration 101; masked on the write path). Run-invariant across host rows; the
	// projection takes the first non-null. nil for old runs / input-less paths.
	Input map[string]any
}

// AggregateRunStatus collapses a run into [RunStatus]. dropped is how many of its
// rows are a dropped host's ([MarkDropped]), each of them failed or orphaned.
// Priority order (corresponds to barrier classification, dispatch.go classify): the
// run holds its incarnation's applying lock (lockHeld, [runLockHeldSQL]) or any row
// is non-terminal → applying; else a failed/orphaned row that is not a drop →
// failed; else cancelled → cancelled; else a drop → partial_failed; else (only
// success/no_match) → success. Empty slice → applying (rows not yet dispatched —
// run not complete).
//
// The rows alone cannot say a run is over: between one step's barrier and the
// next step's rows every row is terminal (NIM-972).
func AggregateRunStatus(statuses []Status, dropped int, lockHeld bool) RunStatus {
	if lockHeld || len(statuses) == 0 {
		return RunStatusApplying
	}
	var failures int
	var hasCancelled bool
	for _, s := range statuses {
		switch s {
		case StatusSuccess, StatusNoMatch:
			// benign terminal.
		case StatusFailed, StatusOrphaned:
			failures++
		case StatusCancelled:
			hasCancelled = true
		default:
			// planned/claimed/running/dispatched are non-terminal: run is in progress.
			return RunStatusApplying
		}
	}
	switch {
	case failures > dropped:
		return RunStatusFailed
	case hasCancelled:
		return RunStatusCancelled
	case dropped > 0:
		return RunStatusPartialFailed
	default:
		return RunStatusSuccess
	}
}

// runLockHeldSQL is true on an `apply_runs ar` row joined to `incarnation i` when
// the row's run holds the incarnation's applying lock (ADR-027 amend (r)). A
// `destroying` lock carries no epoch, so a destroy run is judged by its rows only.
const runLockHeldSQL = `COALESCE(i.status = 'applying' AND i.applying_apply_id = ar.apply_id, false)`

const listRunsByIncarnationSQL = `
SELECT ar.apply_id,
       MIN(ar.scenario)                                AS scenario,
       MIN(ar.started_at)                              AS started_at,
       CASE WHEN bool_and(ar.finished_at IS NOT NULL) AND NOT bool_or(` + runLockHeldSQL + `)
            THEN MAX(ar.finished_at) END               AS finished_at,
       MIN(ar.started_by_aid)                          AS started_by_aid,
       array_agg(ar.status)                            AS statuses,
       bool_or(` + runLockHeldSQL + `)                 AS lock_held,
       COUNT(*) FILTER (WHERE ar.dropped)              AS dropped
FROM apply_runs ar
LEFT JOIN incarnation i ON ar.incarnation_name = i.id
WHERE ar.incarnation_name = $1
GROUP BY ar.apply_id
ORDER BY MIN(ar.started_at) DESC, ar.apply_id DESC
LIMIT $2 OFFSET $3
`

const countRunsByIncarnationSQL = `
SELECT COUNT(DISTINCT apply_id) FROM apply_runs WHERE incarnation_name = $1
`

// ListRunsByIncarnation returns a page of incarnation runs (aggregated by apply_id),
// with newest first (MIN(started_at) DESC), and the total run count. scenario/
// started_by_aid are identical across apply_id — we take MIN as a deterministic
// representative. finished_at is MAX across rows, but only when ALL have finished
// and the run no longer holds its lock (otherwise NULL: run still applying).
// statuses is the set of host statuses for aggregation by [AggregateRunStatus].
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
			lockHeld   bool
			dropped    int
		)
		if err := rows.Scan(&rs.ApplyID, &rs.Scenario, &rs.StartedAt, &rs.FinishedAt, &rs.StartedByAID, &statusStrs, &lockHeld, &dropped); err != nil {
			return nil, 0, fmt.Errorf("applyrun: scan run summary: %w", err)
		}
		rs.Incarnation = incarnationName
		rs.Status = AggregateRunStatus(toStatuses(statusStrs), dropped, lockHeld)
		out = append(out, rs)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("applyrun: iterate runs: %w", err)
	}
	return out, total, nil
}

const selectRunHostsSQL = `
SELECT ar.sid, ar.status, ar.passage, ar.task_idx, ar.failed_plan_index, ar.error_summary,
       ar.attempt, ar.cancel_requested, ar.scenario, ar.started_at, ar.finished_at, ar.started_by_aid,
       ar.input, ar.notices, ` + runLockHeldSQL + `, ar.dropped
FROM apply_runs ar
LEFT JOIN incarnation i ON ar.incarnation_name = i.id
WHERE ar.apply_id = $1 AND ar.incarnation_name = $2
ORDER BY ar.sid ASC, ar.passage ASC
`

// SelectRunDetail returns the details of a single run: all host×passage rows for an
// apply_id belonging to the specified incarnation (WHERE on both — enforcement of
// "apply_id of this incarnation"; foreign apply_id returns 0 rows → [ErrApplyRunNotFound]).
// Header (scenario/started_at/started_by_aid) comes from the first row (identical
// across apply_id); finished_at detail is MAX across rows, but nil until all have
// finished and while the run holds its lock; Status is the aggregate of Hosts and
// the lock.
func SelectRunDetail(ctx context.Context, db ExecQueryRower, applyID, incarnationName string) (*RunDetail, error) {
	rows, err := db.Query(ctx, selectRunHostsSQL, applyID, incarnationName)
	if err != nil {
		return nil, fmt.Errorf("applyrun: run detail query: %w", err)
	}
	defer rows.Close()

	var (
		detail   RunDetail
		statuses []Status
		dropped  int
		// Run finality: expose FinishedAt only when every row has finished (max of
		// non-nil; nil if at least one row is still running).
		allFinished = true
		maxFinished time.Time
		lockHeld    bool
	)
	for rows.Next() {
		var (
			hs           RunHostStatus
			statusStr    string
			rowScenario  string
			rowStarted   time.Time
			rowFinished  *time.Time
			rowStartedBy *string
			rowInput     []byte
			rowNotices   []byte
		)
		if err := rows.Scan(
			&hs.SID, &statusStr, &hs.Passage, &hs.FailedTaskIdx, &hs.FailedPlanIndex,
			&hs.ErrorSummary, &hs.Attempt, &hs.CancelRequested,
			&rowScenario, &rowStarted, &rowFinished, &rowStartedBy,
			&rowInput, &rowNotices, &lockHeld, &hs.Dropped,
		); err != nil {
			return nil, fmt.Errorf("applyrun: scan run host: %w", err)
		}
		hs.Status = Status(statusStr)
		if hs.Dropped {
			dropped++
		}
		// notices is an append-only log (one entry per reporting task), collapsed
		// here into the set an operator acts on. Unparseable jsonb is dropped
		// rather than failing the read: a run's outcome must stay visible even if
		// its advisory column is somehow malformed.
		if len(rowNotices) > 0 {
			var raw []RunNotice
			if err := json.Unmarshal(rowNotices, &raw); err == nil {
				hs.Notices = DedupeNotices(raw)
			}
		}
		if detail.ApplyID == "" {
			detail.ApplyID = applyID
			detail.Scenario = rowScenario
			detail.StartedAt = rowStarted
			detail.StartedByAID = rowStartedBy
		}
		// input is run-invariant (written identically on every row); take the
		// first non-null (old rows / keeper-side/sentinel rows carry NULL).
		if detail.Input == nil && len(rowInput) > 0 {
			var in map[string]any
			if err := json.Unmarshal(rowInput, &in); err == nil {
				detail.Input = in
			}
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
	detail.Status = AggregateRunStatus(statuses, dropped, lockHeld)
	if allFinished && !lockHeld {
		detail.FinishedAt = &maxFinished
	}
	return &detail, nil
}

const selectRunStatusSQL = `
SELECT ar.status, ` + runLockHeldSQL + `, ar.dropped
FROM apply_runs ar
LEFT JOIN incarnation i ON ar.incarnation_name = i.id
WHERE ar.apply_id = $1
`

// SelectRunStatus is the status of run applyID as the runs API folds it, from its
// rows and its incarnation's applying lock read in one statement.
func SelectRunStatus(ctx context.Context, db ExecQueryRower, applyID string) (RunStatus, error) {
	rows, err := db.Query(ctx, selectRunStatusSQL, applyID)
	if err != nil {
		return "", fmt.Errorf("applyrun: run status query: %w", err)
	}
	defer rows.Close()
	var (
		statuses []Status
		lockHeld bool
		dropped  int
	)
	for rows.Next() {
		var (
			s          string
			held, drop bool
		)
		if err := rows.Scan(&s, &held, &drop); err != nil {
			return "", fmt.Errorf("applyrun: scan run status: %w", err)
		}
		statuses = append(statuses, Status(s))
		lockHeld = lockHeld || held
		if drop {
			dropped++
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("applyrun: iterate run status: %w", err)
	}
	return AggregateRunStatus(statuses, dropped, lockHeld), nil
}

// toStatuses converts a slice of status strings from array_agg into []Status.
func toStatuses(ss []string) []Status {
	out := make([]Status, len(ss))
	for i, s := range ss {
		out[i] = Status(s)
	}
	return out
}
