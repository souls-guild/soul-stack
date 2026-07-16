// Per-host operation history (`GET /v1/souls/{sid}/history`).
//
// Aggregates two tables with per-host rows for the target SID into a single timeline:
//
//   - `apply_runs` (PK (apply_id, sid)) - scenario tasks that reached the host
//     within a scenario run of an incarnation.
//   - `errands` (PK errand_id, FK sid) - ad-hoc pull-exec of a single module
//     (ADR-033).
//
// push_runs NOT included: per-host data there lives only inside jsonb
// `summary` (sid → {...}) and `inventory_sids` array - extracting per-host
// row requires jsonb unpacking / UNNEST, which exceeds UNION form of this
// endpoint. Follow-up: separate SELECT from push_runs WHERE $sid = ANY(
// inventory_sids) with summary->$sid unpacking.
//
// Implementation - UNION ALL of two SELECTs with common projection (type discriminator
// + nullable columns for mismatched fields), ORDER BY started_at DESC,
// LIMIT/OFFSET outside. total - separate SUM(COUNT) under same filter without
// LIMIT/OFFSET (for UI pagination, parity with errand.Store.List / soul.SelectAll).
package soul

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// HistoryType is the discriminator for the source of per-host timeline entries.
type HistoryType string

const (
	// HistoryTypeScenario is a row from `apply_runs` (scenario task to host).
	HistoryTypeScenario HistoryType = "scenario"
	// HistoryTypeErrand is a row from `errands` (ad-hoc exec).
	HistoryTypeErrand HistoryType = "errand"
)

// ValidHistoryType checks if type is known (for validation of query filter `type`).
func ValidHistoryType(t HistoryType) bool {
	switch t {
	case HistoryTypeScenario, HistoryTypeErrand:
		return true
	default:
		return false
	}
}

// HistoryItem is a single per-host timeline entry. Fields specific to one
// source are nil/"" for the other (Incarnation/Scenario - scenario only;
// Module - errand only).
type HistoryItem struct {
	Type        HistoryType
	ID          string // apply_id (scenario) | errand_id (errand).
	Incarnation string // scenario-only.
	Scenario    string // scenario-only.
	Module      string // errand-only.
	Status      string
	StartedAt   time.Time
	FinishedAt  *time.Time
	// VoyageID is a back-link to Voyage (ADR-043), resolved via voyage_targets
	// (scenario → vt.apply_id, errand → vt.errand_id). nil for direct
	// incarnation.run / standalone errand / historical entries (normal).
	VoyageID *string
}

// HistoryFilter are parameters for `GET /v1/souls/{sid}/history`. Empty fields = "no
// filter". SID is required (caller validates SIDPattern). Types is multi-value
// OR by source (empty = both). Since is in UTC (started_at > value).
type HistoryFilter struct {
	SID   string
	Types []HistoryType
	Since time.Time
}

// includeScenario / includeErrand determine which sources participate in UNION
// under Types filter. Empty Types → both.
func (f HistoryFilter) includeScenario() bool {
	if len(f.Types) == 0 {
		return true
	}
	for _, t := range f.Types {
		if t == HistoryTypeScenario {
			return true
		}
	}
	return false
}

func (f HistoryFilter) includeErrand() bool {
	if len(f.Types) == 0 {
		return true
	}
	for _, t := range f.Types {
		if t == HistoryTypeErrand {
			return true
		}
	}
	return false
}

// SelectHistory returns a page of per-host timeline under the filter,
// sorted by started_at DESC, and total under the same filter without
// LIMIT/OFFSET.
//
// SID is required - empty returns error (caller-bug). If Types filter
// excludes both sources (theoretically impossible - validator rejects it),
// returns empty list and total=0 without database access.
func SelectHistory(ctx context.Context, db ExecQueryRower, f HistoryFilter, offset, limit int) ([]HistoryItem, int, error) {
	if f.SID == "" {
		return nil, 0, fmt.Errorf("soul: history requires sid")
	}

	scen := f.includeScenario()
	err := f.includeErrand()
	if !scen && !err {
		return nil, 0, nil
	}

	// $1=sid always; $2=since (opt). Both SELECTs use same placeholders
	// (UNION ALL splits args) to avoid positional shifts.
	args := []any{f.SID}
	var sinceCond string
	if !f.Since.IsZero() {
		args = append(args, f.Since.UTC())
		sinceCond = " AND started_at > $2"
	}

	// Per-source SELECTs with common 9-column projection:
	//   type, id, incarnation, scenario, module, status, started_at,
	//   finished_at, voyage_id
	// Mismatched fields are NULL literals of correct type. voyage_id comes from
	// voyage_targets via LEFT JOIN (NULL for non-Voyage runs).
	//
	// LEFT JOIN does not multiply history rows: invariant "one apply_id/errand_id →
	// at most one voyage_targets row" is guaranteed by partial-UNIQUE index
	// (migration 063), so apply_runs/errands row matches at most one
	// voyage_target.
	scenarioSQL := `
SELECT 'scenario' AS type, apply_runs.apply_id AS id,
       apply_runs.incarnation_name AS incarnation, apply_runs.scenario,
       NULL::text AS module, apply_runs.status, apply_runs.started_at,
       apply_runs.finished_at, vt.voyage_id AS voyage_id
FROM apply_runs
LEFT JOIN voyage_targets vt ON vt.apply_id = apply_runs.apply_id
WHERE apply_runs.sid = $1` + sinceCond

	errandSQL := `
SELECT 'errand' AS type, errands.errand_id AS id, NULL::text AS incarnation,
       NULL::text AS scenario, errands.module, errands.status,
       errands.started_at, errands.finished_at, vt.voyage_id AS voyage_id
FROM errands
LEFT JOIN voyage_targets vt ON vt.errand_id = errands.errand_id
WHERE errands.sid = $1` + sinceCond

	var union string
	switch {
	case scen && err:
		union = scenarioSQL + "\nUNION ALL" + errandSQL
	case scen:
		union = scenarioSQL
	default:
		union = errandSQL
	}

	// total is COUNT over the union (without LIMIT/OFFSET).
	countSQL := "SELECT COUNT(*) FROM (" + union + "\n) AS h"
	var total int
	if cerr := db.QueryRow(ctx, countSQL, args...).Scan(&total); cerr != nil {
		return nil, 0, fmt.Errorf("soul: history count: %w", cerr)
	}

	// Page: ORDER BY started_at DESC, LIMIT/OFFSET. id as secondary key -
	// stable order on equal started_at (pagination determinism).
	pageSQL := "SELECT * FROM (" + union + `
) AS h
ORDER BY started_at DESC, id DESC
LIMIT $` + strconv.Itoa(len(args)+1) + ` OFFSET $` + strconv.Itoa(len(args)+2)
	pageArgs := append(append([]any{}, args...), limit, offset)

	rows, qerr := db.Query(ctx, pageSQL, pageArgs...)
	if qerr != nil {
		return nil, 0, fmt.Errorf("soul: history select: %w", qerr)
	}
	defer rows.Close()

	out := make([]HistoryItem, 0, limit)
	for rows.Next() {
		var (
			it          HistoryItem
			typeStr     string
			incarnation *string
			scenario    *string
			module      *string
			finishedAt  *time.Time
			voyageID    *string
		)
		if serr := rows.Scan(
			&typeStr,
			&it.ID,
			&incarnation,
			&scenario,
			&module,
			&it.Status,
			&it.StartedAt,
			&finishedAt,
			&voyageID,
		); serr != nil {
			return nil, 0, fmt.Errorf("soul: history scan: %w", serr)
		}
		it.Type = HistoryType(typeStr)
		if incarnation != nil {
			it.Incarnation = *incarnation
		}
		if scenario != nil {
			it.Scenario = *scenario
		}
		if module != nil {
			it.Module = *module
		}
		it.FinishedAt = finishedAt
		it.VoyageID = voyageID
		out = append(out, it)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, 0, fmt.Errorf("soul: history iter: %w", rerr)
	}
	return out, total, nil
}
