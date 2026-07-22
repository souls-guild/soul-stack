package incarnation

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// ScryCandidate — minimal incarnation representation for the iterator of the
// background rule `scry_background` (ADR-031 Slice C, Reaper). Carries
// exactly what a reaper tick needs to build scenario.CheckDriftSpec
// (Service resolve + roster + dispatch) and to check the min-interval
// throttle on read.
type ScryCandidate struct {
	Name             string
	Service          string
	ServiceVersion   string
	Covens           []string
	LastDriftCheckAt *time.Time
}

// SelectScryCandidates returns a batch of incarnations eligible for a
// background Scry scan (ADR-031 Slice C, Reaper rule `scry_background`).
// Iterator predicate:
//
//   - status ready or drift (drift is informational, doesn't block a
//     repeat scan; see ADR-031);
//   - no active apply run (NOT IN apply_runs WHERE finished_at IS
//     NULL — excludes all live claimed/dispatched/running runs
//     regardless of status, not just applying);
//   - order by `last_drift_check_at NULLS FIRST` — natural round-robin:
//     never-scanned incarnations go first, then by the date of the last
//     scan.
//
// The min-interval throttle (PM config `min_interval_per_incarnation`) is
// applied at the iterator level: if set > 0, exclude incarnations with
// `last_drift_check_at + min_interval > NOW()`. A zero (or negative)
// duration → throttle off, ORDER BY NULLS FIRST gives natural fairness.
// batchSize<=0 → returns an empty list without hitting PG (defensive no-op).
func SelectScryCandidates(ctx context.Context, db ExecQueryRower, minInterval time.Duration, batchSize int) ([]ScryCandidate, error) {
	if batchSize <= 0 {
		return nil, nil
	}
	const baseSQL = `
SELECT name, service, service_version, covens, last_drift_check_at
FROM incarnation
WHERE status IN ('ready', 'drift')
  AND name NOT IN (
      SELECT incarnation_name FROM apply_runs WHERE finished_at IS NULL
  )
`
	// $1 — min_interval (interval literal), $2 — limit. The min-interval
	// predicate is appended conditionally: when minInterval<=0 we don't pass
	// interval, so PG doesn't need to infer the type of an unused parameter.
	var (
		sql  string
		args []any
	)
	if minInterval > 0 {
		sql = baseSQL + `
  AND (last_drift_check_at IS NULL OR last_drift_check_at + $1::interval <= NOW())
ORDER BY last_drift_check_at NULLS FIRST, name
LIMIT $2
`
		args = []any{fmt.Sprintf("%d seconds", int64(minInterval.Seconds())), batchSize}
	} else {
		sql = baseSQL + `
ORDER BY last_drift_check_at NULLS FIRST, name
LIMIT $1
`
		args = []any{batchSize}
	}
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("incarnation: scry candidates query: %w", err)
	}
	defer rows.Close()
	var out []ScryCandidate
	for rows.Next() {
		var c ScryCandidate
		if err := rows.Scan(&c.Name, &c.Service, &c.ServiceVersion, &c.Covens, &c.LastDriftCheckAt); err != nil {
			return nil, fmt.Errorf("incarnation: scry candidates scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("incarnation: scry candidates iter: %w", err)
	}
	return out, nil
}

// CountActiveDryRuns returns the number of in-flight background dry_run runs
// (`apply_runs` with `recipe->>'dry_run'='true'` and not finished). Used by
// the Reaper rule `scry_background` for the `max_concurrent_in_flight` throttle cap.
//
// The query doesn't use an index (predicate on a jsonb field), but a partial
// index isn't worth adding: dry_run run cardinality in production is
// negligible compared to regular applies, and a full scan over the active
// pool is cheap.
func CountActiveDryRuns(ctx context.Context, db ExecQueryRower) (int, error) {
	const sql = `
SELECT count(*) FROM apply_runs
WHERE recipe->>'dry_run' = 'true'
  AND finished_at IS NULL
`
	var n int
	if err := db.QueryRow(ctx, sql).Scan(&n); err != nil {
		return 0, fmt.Errorf("incarnation: count active dry-runs: %w", err)
	}
	return n, nil
}

// DriftScanSummary — a counts aggregate of one Scry check (ADR-031 Slice C),
// stored in the `incarnation.last_drift_summary` column. Symmetric to
// scenario.DriftSummary, plus `TotalHosts` and `ScannedAt` to discriminate
// stale scan info.
type DriftScanSummary struct {
	HostsDrifted     int       `json:"hosts_drifted"`
	HostsClean       int       `json:"hosts_clean"`
	HostsUnsupported int       `json:"hosts_unsupported"`
	HostsFailed      int       `json:"hosts_failed"`
	TotalHosts       int       `json:"total_hosts"`
	ScannedAt        time.Time `json:"scanned_at"`
}

// UpdateDriftScanResult atomically sets `last_drift_check_at` and
// `last_drift_summary` after a converge dry_run finishes — background
// (Reaper rule `scry_background`) or on-demand (REST/MCP CheckDrift,
// Slice B). Doesn't touch incarnation status: Slice B does that separately
// via `MarkDriftStatus`; the caller must coordinate ordering.
//
// `summary.ScannedAt` is set by the caller (usually `time.Now().UTC()` after
// assembling the DriftReport). UPDATE has no status WHERE guard: the
// incarnation may have moved to applying/destroying during the scan — that
// doesn't prevent recording the fact of the check (informational,
// non-blocking fields).
func UpdateDriftScanResult(ctx context.Context, db ExecQueryRower, name string, summary DriftScanSummary) error {
	if !ValidName(name) {
		return fmt.Errorf("incarnation: invalid name %q", name)
	}
	if summary.ScannedAt.IsZero() {
		summary.ScannedAt = time.Now().UTC()
	}
	summaryBytes, err := json.Marshal(summary)
	if err != nil {
		return fmt.Errorf("incarnation: marshal drift summary: %w", err)
	}
	const sql = `
UPDATE incarnation
SET last_drift_check_at = $2,
    last_drift_summary  = $3,
    updated_at          = NOW()
WHERE name = $1
`
	if _, err := db.Exec(ctx, sql, name, summary.ScannedAt, summaryBytes); err != nil {
		return fmt.Errorf("incarnation: update drift scan result: %w", err)
	}
	return nil
}
