package applyrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TaskRegister represents a row in the `apply_task_register` accumulator
// (migration 022, key moved to plan_index by migration 079): the register result
// of a single probe task on one host within a run.
//
// register_name is not present here: the handler at the time of TaskEvent only knows
// indices (the proto register name is not carried, ADR-012(d)). Resolution of
// plan_index → register_name is done by scenario-runner when reading (it holds
// []RenderedTask with a Register field).
type TaskRegister struct {
	ApplyID string
	SID     string

	// PlanIndex is the GLOBAL end-to-end task index across the entire run plan
	// (across all Passages), echoing TaskEvent.plan_index (ADR-056 §S1 fix Variant B).
	// It is the register correlation key (PK component, migration 079): unique across
	// both all Passages and all hosts — eliminates the task_idx collision (probe
	// passage0 / action passage1 shared local idx=0, ON CONFLICT overwrote probe-register).
	// scenario-runner maps it against RenderedTask.Index. N=1 → ==TaskIdx.
	PlanIndex int

	// TaskIdx is the LOCAL position of the task within its Passage's ApplyRequest.tasks[]
	// (echoing TaskEvent.task_idx). Stored for informational purposes (triage);
	// NOT the correlation key — it is not unique across Passages or across hosts within
	// one Passage (different where:). Register resolution goes by PlanIndex.
	TaskIdx int

	RegisterData map[string]any

	// Passage is the Passage index for staged-render (ADR-056, migration 078):
	// a component of the FK to apply_runs(apply_id, sid, passage). passage is written
	// as row data and is needed for the FK target. N=1 → 0 (the host's only Passage).
	Passage int
}

const upsertTaskRegisterSQL = `
INSERT INTO apply_task_register (apply_id, sid, plan_index, task_idx, register_data, passage)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (apply_id, sid, plan_index)
DO UPDATE SET task_idx = EXCLUDED.task_idx, register_data = EXCLUDED.register_data, passage = EXCLUDED.passage, created_at = NOW()
`

// UpsertTaskRegister writes (or overwrites) a task's register result.
// Overwrite handles retries of the same task on the Soul side: the latest result wins
// (ON CONFLICT on PK (apply_id, sid, plan_index) — PK changed in migration 079 from
// task_idx to plan_index).
//
// Pre-conditions: non-empty ApplyID / SID; non-negative TaskIdx; non-empty
// RegisterData (nil/empty → no-op: no data to accumulate for register).
//
// register_data is serialized to jsonb via encoding/json. FK-violation
// (no apply_runs row with (apply_id, sid)) → wrapped fmt.Errorf: a programming
// order error (Insert of apply_run must precede the TaskEvent).
func UpsertTaskRegister(ctx context.Context, db ExecQueryRower, tr *TaskRegister) error {
	if tr == nil {
		return fmt.Errorf("applyrun: nil task register")
	}
	if tr.ApplyID == "" {
		return fmt.Errorf("applyrun: empty apply_id")
	}
	if tr.SID == "" {
		return fmt.Errorf("applyrun: empty sid")
	}
	if tr.PlanIndex < 0 {
		return fmt.Errorf("applyrun: negative plan_index %d", tr.PlanIndex)
	}
	if tr.TaskIdx < 0 {
		return fmt.Errorf("applyrun: negative task_idx %d", tr.TaskIdx)
	}
	if len(tr.RegisterData) == 0 {
		return nil
	}

	raw, err := json.Marshal(tr.RegisterData)
	if err != nil {
		return fmt.Errorf("applyrun: marshal register_data: %w", err)
	}

	if _, err := db.Exec(ctx, upsertTaskRegisterSQL, tr.ApplyID, tr.SID, tr.PlanIndex, tr.TaskIdx, raw, tr.Passage); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgErrCodeForeignKeyViolation {
			return fmt.Errorf("applyrun: task register FK violation on %s: %w", pgErr.ConstraintName, err)
		}
		return fmt.Errorf("applyrun: upsert task register: %w", err)
	}
	return nil
}

const selectTaskRegistersByApplyIDSQL = `
SELECT sid, plan_index, task_idx, register_data, passage
FROM apply_task_register
WHERE apply_id = $1
ORDER BY sid ASC, plan_index ASC
`

// SelectTaskRegistersByApplyID returns all register rows for a run
// (single apply_id, multiple sid/plan_index pairs), sorted by (sid, plan_index).
// Used by scenario-runner after the barrier: it groups rows per-host,
// resolves plan_index → register_name from its []RenderedTask (by
// RenderedTask.Index = global index) and builds RenderInput.Register for
// rendering state_changes.sets. Sorting by global plan_index preserves
// the "later task wins" semantics when register names collide.
//
// Returns registers for ALL Passages in the run (staged-render, ADR-056):
// the final state_changes.sets render aggregates registers from all passages
// (after the last barrier). Rendering the next Passage in the stage-loop reads
// registers from previous passages via [SelectTaskRegistersByApplyIDUpToPassage].
//
// An empty result means a run with no registers: no tasks to accumulate; the caller
// treats this as an empty register context.
func SelectTaskRegistersByApplyID(ctx context.Context, db ExecQueryRower, applyID string) ([]TaskRegister, error) {
	rows, err := db.Query(ctx, selectTaskRegistersByApplyIDSQL, applyID)
	if err != nil {
		return nil, fmt.Errorf("applyrun: task registers query: %w", err)
	}
	return scanTaskRegisters(rows, applyID)
}

const selectTaskRegistersUpToPassageSQL = `
SELECT sid, plan_index, task_idx, register_data, passage
FROM apply_task_register
WHERE apply_id = $1 AND passage < $2
ORDER BY sid ASC, plan_index ASC
`

// SelectTaskRegistersByApplyIDUpToPassage returns register rows for a run accumulated
// in Passages STRICTLY LESS THAN upToPassage (staged-render, ADR-056 §S.1):
// rendering Passage N passes in registers from all previous Passages (per-host map,
// gathered by their barriers). upToPassage=0 (first Passage) → empty (register not yet
// gathered — behavior matches up-front render).
func SelectTaskRegistersByApplyIDUpToPassage(ctx context.Context, db ExecQueryRower, applyID string, upToPassage int) ([]TaskRegister, error) {
	rows, err := db.Query(ctx, selectTaskRegistersUpToPassageSQL, applyID, upToPassage)
	if err != nil {
		return nil, fmt.Errorf("applyrun: task registers query (up to passage %d): %w", upToPassage, err)
	}
	return scanTaskRegisters(rows, applyID)
}

// scanTaskRegisters reads rows into []TaskRegister (common part of both selects).
// Closes rows.
func scanTaskRegisters(rows pgx.Rows, applyID string) ([]TaskRegister, error) {
	defer rows.Close()

	var out []TaskRegister
	for rows.Next() {
		var (
			tr  TaskRegister
			raw []byte
		)
		if err := rows.Scan(&tr.SID, &tr.PlanIndex, &tr.TaskIdx, &raw, &tr.Passage); err != nil {
			return nil, fmt.Errorf("applyrun: task registers scan: %w", err)
		}
		if err := json.Unmarshal(raw, &tr.RegisterData); err != nil {
			return nil, fmt.Errorf("applyrun: task registers unmarshal (sid=%s plan_index=%d): %w", tr.SID, tr.PlanIndex, err)
		}
		tr.ApplyID = applyID
		out = append(out, tr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("applyrun: task registers iter: %w", err)
	}
	return out, nil
}
