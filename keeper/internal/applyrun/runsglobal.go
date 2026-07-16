package applyrun

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
)

// Global read-view of runs ACROSS ALL incarnations (GET /v1/runs + /v1/runs/stats,
// "All Runs" UI page). Same data boundary as per-incarnation view (runsview.go):
// run = group of apply_runs host rows by apply_id, NOT a host row.
// Purview-scope (ADR-047) comes from the handler as [incarnation.ListScope] and
// is passed to SQL as a subquery over the incarnation table — a single source of
// scope semantics with incarnation.SelectAll ([incarnation.ScopeCondition]).

// RunsFilter represents user filters for the global run list (what the operator
// requested to see; scope is what they are permitted to see, AND-intersection in WHERE).
type RunsFilter struct {
	// Status is a filter by aggregate run status ("" = all). Validity of
	// the value is checked at the handler level ([ValidRunStatus] → 422).
	Status RunStatus
	// Incarnation is a filter by incarnation name ("" = all).
	Incarnation string
	// Service is a filter by the owning incarnation's service ("" = all); exact
	// match via incarnation.service (index-safe, incarnation_service_idx).
	Service string
	// Q is a free-text search (ILIKE-substring) over incarnation/scenario/service/
	// started_by ("" = no search). LIKE metacharacters are escaped (literal).
	Q string
	// StartedAfter/StartedBefore are inclusive boundaries on the AGGREGATE started_at
	// (MIN(started_at)); nil = not set. Aggregate filter goes in outerWhere (after
	// GROUP BY): started_at is not constant within apply_id, in sub it would split the group.
	StartedAfter  *time.Time
	StartedBefore *time.Time
	// Sort is the sort field (whitelist [runsSortableColumns], "" = default started_at).
	// SortDir is asc|desc ("" = desc). Invalid values surface from [ListRuns] as sentinel
	// errors ([ErrInvalidRunsSortField]/[ErrInvalidRunsSortDir] → 422 at the handler).
	// ADR-068 §B1.
	Sort    string
	SortDir string
}

// Sentinel errors for sort validation in [ListRuns] (handler maps to 422). ADR-068 §B1.
var (
	ErrInvalidRunsSortField = errors.New("applyrun: invalid sort field")
	ErrInvalidRunsSortDir   = errors.New("applyrun: invalid sort direction")
)

// runsSortableColumns is a closed whitelist of sortable fields for GET /v1/runs →
// alias column from the aggregation subquery (runs). Columns are taken from here, not
// from raw input — otherwise SQL injection via ORDER BY. ADR-068 §B1.
var runsSortableColumns = map[string]string{
	"started_at":  "started_at",
	"finished_at": "finished_at",
	"status":      "status",
	"incarnation": "incarnation",
	"service":     "service",
	"scenario":    "scenario",
}

// ValidRunStatus is the closed set of aggregate run statuses (filter for /v1/runs).
func ValidRunStatus(s RunStatus) bool {
	switch s {
	case RunStatusApplying, RunStatusSuccess, RunStatusFailed, RunStatusCancelled:
		return true
	}
	return false
}

// RunsStatusCounts represents counters for runs by aggregate status. Total = sum of all.
type RunsStatusCounts struct {
	Total     int
	Applying  int
	Success   int
	Failed    int
	Cancelled int
}

// RunsStats represents a summary of the global run read-view: all-time and last
// 24 hours (window based on MIN(started_at) — same axis as list order).
type RunsStats struct {
	All     RunsStatusCounts
	Last24h RunsStatusCounts
}

// runsAggregateSelect aggregates apply_runs by apply_id: the common subquery base
// for both list and stats. The status column is the SQL form of [AggregateRunStatus];
// literals are gathered from Go constants (init below) — a single source of priority
// applying > failed > cancelled > success. Unknown status is treated as non-terminal
// (parity with AggregateRunStatus default branch).
//
// apply_runs is aliased as `ar`, incarnation as `i`: incarnation has its own status →
// bare columns would be ambiguous. LEFT JOIN is defensive (FK ON DELETE CASCADE doesn't
// leave orphans, but JOIN doesn't drop runs on desync); service via COALESCE to empty
// string — NULL-safe scan into string.
var runsAggregateSelect = fmt.Sprintf(`
SELECT ar.apply_id,
       MIN(ar.incarnation_name)                        AS incarnation,
       COALESCE(MIN(i.service), '')                    AS service,
       MIN(ar.scenario)                                AS scenario,
       MIN(ar.started_at)                              AS started_at,
       CASE WHEN bool_and(ar.finished_at IS NOT NULL)
            THEN MAX(ar.finished_at) END               AS finished_at,
       MIN(ar.started_by_aid)                          AS started_by_aid,
       CASE
         WHEN bool_or(ar.status NOT IN (%[1]s))        THEN '%[2]s'
         WHEN bool_or(ar.status IN ('%[3]s','%[4]s'))  THEN '%[5]s'
         WHEN bool_or(ar.status = '%[6]s')             THEN '%[7]s'
         ELSE '%[8]s'
       END                                             AS status
FROM apply_runs ar
LEFT JOIN incarnation i ON ar.incarnation_name = i.name`,
	terminalStatusesSQL(), RunStatusApplying,
	StatusFailed, StatusOrphaned, RunStatusFailed,
	StatusCancelled, RunStatusCancelled,
	RunStatusSuccess,
)

// terminalStatusesSQL returns terminal host statuses as a list of SQL literals
// ('success','no_match',...). Statuses are trusted package constants, not input.
func terminalStatusesSQL() string {
	terminal := []Status{StatusSuccess, StatusNoMatch, StatusFailed, StatusOrphaned, StatusCancelled}
	quoted := make([]string, len(terminal))
	for i, s := range terminal {
		quoted[i] = "'" + string(s) + "'"
	}
	return strings.Join(quoted, ",")
}

// buildRunsQuery builds an aggregation subquery with filters/scope and an outer WHERE
// for aggregate status. Returns the body `(<aggregation>) runs` + outer WHERE + args.
func buildRunsQuery(filter RunsFilter, scope incarnation.ListScope) (sub, outerWhere string, args []any) {
	var clauses []string
	if filter.Incarnation != "" {
		args = append(args, filter.Incarnation)
		clauses = append(clauses, fmt.Sprintf("ar.incarnation_name = $%d", len(args)))
	}
	if filter.Service != "" {
		args = append(args, filter.Service)
		clauses = append(clauses, fmt.Sprintf("i.service = $%d", len(args)))
	}
	if filter.Q != "" {
		args = append(args, "%"+escapeLike(filter.Q)+"%")
		n := len(args)
		clauses = append(clauses, fmt.Sprintf(
			"(ar.incarnation_name ILIKE $%[1]d OR ar.scenario ILIKE $%[1]d OR i.service ILIKE $%[1]d OR ar.started_by_aid ILIKE $%[1]d)",
			n))
	}
	// Purview-scope via subquery on incarnation (fail-closed: empty scope gives
	// WHERE FALSE in the subquery → no runs).
	cond, args := incarnation.ScopeCondition(args, scope)
	if cond != "" {
		clauses = append(clauses, "ar.incarnation_name IN (SELECT name FROM incarnation WHERE "+cond+")")
	}

	sub = runsAggregateSelect
	if len(clauses) > 0 {
		sub += "\nWHERE " + strings.Join(clauses, " AND ")
	}
	sub += "\nGROUP BY ar.apply_id"

	// Outer WHERE filters by AGGREGATE columns from the aggregation (status, started_at):
	// filtered AFTER GROUP BY (started_at = MIN per apply_id, not constant within
	// the group — would split it in sub). Both countSQL and listSQL are built from
	// sub+outerWhere → total/page are consistent. All values are bind parameters.
	var outer []string
	if filter.Status != "" {
		args = append(args, string(filter.Status))
		outer = append(outer, fmt.Sprintf("status = $%d", len(args)))
	}
	if filter.StartedAfter != nil {
		args = append(args, *filter.StartedAfter)
		outer = append(outer, fmt.Sprintf("started_at >= $%d", len(args)))
	}
	if filter.StartedBefore != nil {
		args = append(args, *filter.StartedBefore)
		outer = append(outer, fmt.Sprintf("started_at <= $%d", len(args)))
	}
	if len(outer) > 0 {
		outerWhere = "\nWHERE " + strings.Join(outer, " AND ")
	}
	return sub, outerWhere, args
}

// likeEscaper escapes LIKE/ILIKE metacharacters (backslash-escape by default in PG):
// literal %/_/\ in free search don't act as wildcards. Single pass NewReplacer
// (backslash first in the pairs list avoids double-escaping).
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// escapeLike prepares Q for substitution into a `%…%` ILIKE pattern.
func escapeLike(s string) string { return likeEscaper.Replace(s) }

// buildRunsOrderBy builds the ORDER BY clause from a whitelist field + direction.
// Default field is started_at, direction is desc (byte-exact to previous
// `started_at DESC, apply_id DESC`). Tie-break `apply_id DESC` is always added
// (stability for equal sort-column values). finished_at → NULLS LAST (applying runs
// without finished_at go last regardless of direction). ADR-068 §B1.
func buildRunsOrderBy(sort, sortDir string) (string, error) {
	if sort == "" {
		sort = "started_at"
	}
	col, ok := runsSortableColumns[sort]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrInvalidRunsSortField, sort)
	}
	dir, err := runsSortDirSQL(sortDir)
	if err != nil {
		return "", err
	}
	var nulls string
	if sort == "finished_at" {
		nulls = " NULLS LAST"
	}
	return fmt.Sprintf("%s %s%s, apply_id DESC", col, dir, nulls), nil
}

// runsSortDirSQL validates the sort direction. Empty → DESC (default for
// /v1/runs — newest first, unlike the asc-default for incarnation).
func runsSortDirSQL(d string) (string, error) {
	switch d {
	case "", "desc":
		return "DESC", nil
	case "asc":
		return "ASC", nil
	}
	return "", fmt.Errorf("%w: %q", ErrInvalidRunsSortDir, d)
}

// ListRuns returns a page of runs across all incarnations (aggregated by apply_id),
// in the order of filter.Sort/SortDir (default — newest first), and the total run count
// under the same filters/scope. Aggregate status filtering is at the SQL level (otherwise
// total/offset would diverge from Go postfiltering).
func ListRuns(ctx context.Context, db ExecQueryRower, filter RunsFilter, scope incarnation.ListScope, offset, limit int) ([]RunSummary, int, error) {
	// Validate sort before DB: invalid field/direction → sentinel error (422),
	// without unnecessary count query.
	orderBy, err := buildRunsOrderBy(filter.Sort, filter.SortDir)
	if err != nil {
		return nil, 0, err
	}
	sub, outerWhere, args := buildRunsQuery(filter, scope)

	countSQL := "SELECT COUNT(*) FROM (" + sub + ") runs" + outerWhere
	var total int
	if err := db.QueryRow(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("applyrun: count global runs: %w", err)
	}

	// ORDER BY comes from a whitelist expression (not raw input); countSQL above is
	// unaffected — total is independent of sort (ADR-068 §B1).
	listSQL := "SELECT apply_id, incarnation, scenario, service, started_at, finished_at, started_by_aid, status FROM (" +
		sub + ") runs" + outerWhere +
		fmt.Sprintf("\nORDER BY %s OFFSET $%d LIMIT $%d", orderBy, len(args)+1, len(args)+2)
	listArgs := append(append([]any{}, args...), offset, limit)

	rows, err := db.Query(ctx, listSQL, listArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("applyrun: list global runs: %w", err)
	}
	defer rows.Close()

	out := make([]RunSummary, 0, limit)
	for rows.Next() {
		var (
			rs        RunSummary
			statusStr string
		)
		if err := rows.Scan(&rs.ApplyID, &rs.Incarnation, &rs.Scenario, &rs.Service, &rs.StartedAt,
			&rs.FinishedAt, &rs.StartedByAID, &statusStr); err != nil {
			return nil, 0, fmt.Errorf("applyrun: scan global run: %w", err)
		}
		rs.Status = RunStatus(statusStr)
		out = append(out, rs)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("applyrun: iterate global runs: %w", err)
	}
	return out, total, nil
}

// SelectRunsStats returns summary counters of runs (by aggregate status, all-time and
// last 24 hours) in a single query over the aggregation within scope.
func SelectRunsStats(ctx context.Context, db ExecQueryRower, scope incarnation.ListScope) (RunsStats, error) {
	sub, _, args := buildRunsQuery(RunsFilter{}, scope)
	statsSQL := `SELECT status, COUNT(*),
       COUNT(*) FILTER (WHERE started_at >= now() - interval '24 hours')
FROM (` + sub + `) runs
GROUP BY status`

	rows, err := db.Query(ctx, statsSQL, args...)
	if err != nil {
		return RunsStats{}, fmt.Errorf("applyrun: runs stats: %w", err)
	}
	defer rows.Close()

	var stats RunsStats
	for rows.Next() {
		var (
			status      string
			total, last int
		)
		if err := rows.Scan(&status, &total, &last); err != nil {
			return RunsStats{}, fmt.Errorf("applyrun: scan runs stats: %w", err)
		}
		stats.All.Total += total
		stats.Last24h.Total += last
		switch RunStatus(status) {
		case RunStatusApplying:
			stats.All.Applying, stats.Last24h.Applying = total, last
		case RunStatusSuccess:
			stats.All.Success, stats.Last24h.Success = total, last
		case RunStatusFailed:
			stats.All.Failed, stats.Last24h.Failed = total, last
		case RunStatusCancelled:
			stats.All.Cancelled, stats.Last24h.Cancelled = total, last
		}
	}
	if err := rows.Err(); err != nil {
		return RunsStats{}, fmt.Errorf("applyrun: iterate runs stats: %w", err)
	}
	return stats, nil
}
