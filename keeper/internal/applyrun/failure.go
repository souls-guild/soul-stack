package applyrun

import (
	"context"
	"encoding/json"
	"fmt"
)

// RunFailure is why and where a run that did not succeed stopped (NIM-1000), read
// from its terminal `state_history` row: Summary is its `error_summary`
// (migration 111), everything else its `error_detail` (migration 126). Both are
// masked on the write path.
//
// A row written before migration 126 has a Summary and nothing else. A run that
// succeeded has no RunFailure, and neither has one whose terminal was never
// recorded (its lock released as an orphan, its terminal write failed).
type RunFailure struct {
	Summary string `json:"-"`

	// Code is the abort reason (`render_failed`, `dispatch_failed`, …).
	Code string `json:"code,omitempty"`
	// Step is the 0-based step the run stopped at; nil when it stopped outside a
	// step — before its first or after its last.
	Step *int `json:"step,omitempty"`
	// Tasks is the task chain of a render failure, outermost first: a block and
	// its child, an applier and the destiny task it expanded into.
	Tasks []RunFailureTask `json:"tasks,omitempty"`
	// Host is the SID the failing evaluation ran for.
	Host string `json:"host,omitempty"`
	// Cell is the params cell path (`args[0]`).
	Cell string `json:"cell,omitempty"`
	// Expression is the CEL expression as the author wrote it.
	Expression string `json:"expression,omitempty"`
	// Cause is the innermost reason; a fixed sentence when CauseWithheld.
	Cause string `json:"cause,omitempty"`
	// CauseWithheld — the failing expression reads a secret, so the evaluator's
	// own message, which can quote a value, was not stored.
	CauseWithheld bool `json:"cause_withheld,omitempty"`
}

// RunFailureTask is one task of [RunFailure.Tasks]. Index is its plan index — the
// `plan_index` of GET …/runs/{apply_id}/tasks — and nil for a task that expands
// (a block, an applier), an assert, which renders into no task, and a loop whose
// failing iteration is not known. Destiny is
// what an applier applies. File and Line are where the task is written, relative
// to its artifact: the service for a scenario task, the destiny the applier above
// it names for a destiny task. IncludedFrom are the `include:` tasks that spliced
// it in, outermost first.
type RunFailureTask struct {
	Index        *int         `json:"index,omitempty"`
	Name         string       `json:"name,omitempty"`
	Module       string       `json:"module,omitempty"`
	Destiny      string       `json:"destiny,omitempty"`
	File         string       `json:"file,omitempty"`
	Line         int          `json:"line,omitempty"`
	IncludedFrom []SourceLine `json:"included_from,omitempty"`
}

// SourceLine is a line of a file of an artifact.
type SourceLine struct {
	File string `json:"file"`
	Line int    `json:"line"`
}

// selectRunFailuresSQL reads each run's terminal history row — the one carrying
// run_status; a run's `core.state.*` captures write rows of their own, without it.
const selectRunFailuresSQL = `
SELECT DISTINCT ON (apply_id) apply_id, error_summary, error_detail
FROM state_history
WHERE apply_id = ANY($1) AND run_status IS NOT NULL
ORDER BY apply_id, at DESC, history_id DESC
`

// SelectRunFailures returns the [RunFailure] of every run in applyIDs that has
// one, keyed by apply_id.
func SelectRunFailures(ctx context.Context, db ExecQueryRower, applyIDs []string) (map[string]*RunFailure, error) {
	if len(applyIDs) == 0 {
		return nil, nil
	}
	rows, err := db.Query(ctx, selectRunFailuresSQL, applyIDs)
	if err != nil {
		return nil, fmt.Errorf("applyrun: run failures query: %w", err)
	}
	defer rows.Close()

	out := map[string]*RunFailure{}
	for rows.Next() {
		var (
			applyID string
			summary *string
			detail  []byte
		)
		if err := rows.Scan(&applyID, &summary, &detail); err != nil {
			return nil, fmt.Errorf("applyrun: scan run failure: %w", err)
		}
		if summary == nil && len(detail) == 0 {
			continue
		}
		f := &RunFailure{}
		if len(detail) > 0 {
			if err := json.Unmarshal(detail, f); err != nil {
				return nil, fmt.Errorf("applyrun: decode error_detail of %s: %w", applyID, err)
			}
		}
		if summary != nil {
			f.Summary = *summary
		}
		out[applyID] = f
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("applyrun: iterate run failures: %w", err)
	}
	return out, nil
}

// attachFailures sets each summary's Failure from one [SelectRunFailures] over
// the page.
func attachFailures(ctx context.Context, db ExecQueryRower, runs []RunSummary) error {
	ids := make([]string, len(runs))
	for i := range runs {
		ids[i] = runs[i].ApplyID
	}
	failures, err := SelectRunFailures(ctx, db, ids)
	if err != nil {
		return err
	}
	for i := range runs {
		runs[i].Failure = failures[runs[i].ApplyID]
	}
	return nil
}
