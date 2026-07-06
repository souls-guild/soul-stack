package applyrun

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// RunPlanTask — строка «плана задач прогона» (apply_run_plan, миграция 096, NIM-37):
// host-инвариантные метаданные одной отрендеренной задачи по её глобальному
// plan_index. name/module/no_log/passage одинаковы на всех хостах прогона, поэтому
// хранятся РАЗ на (apply_id, plan_index), а per-host статус/output эндпоинт /tasks
// добирает из audit_log (task.executed).
type RunPlanTask struct {
	ApplyID string

	// PlanIndex — ГЛОБАЛЬНЫЙ сквозной индекс задачи по всему плану прогона (все
	// Passage) = RenderedTask.Index (ADR-056 §S1 Variant B). Ключ корреляции с
	// per-host результатом в audit_log (payload.plan_index).
	PlanIndex int

	Name   string
	Module string
	NoLog  bool

	// Passage — индекс Passage staged-render (ADR-056); N=1 → 0.
	Passage int

	// Params — JSON операторских input-параметров задачи (NIM-37 S1b), уже
	// masked seal-aware механизмом на write-path-е (scenario.persistRunPlan).
	// nil/пусто → jsonb NULL: no_log-задача (params подавлены) либо задача без
	// params. Транспортные ключи (template_content/render_context) отфильтрованы
	// до записи. Store слой значения НЕ маскирует — только персистит/читает как есть.
	Params []byte
}

// insertRunPlanSQL — bulk-upsert плана одним запросом через unnest-массивы
// (apply_id общий, остальные колонки — параллельные массивы). ON CONFLICT DO
// UPDATE идемпотентен: staged-прогон переспрашивает Render per-Passage, но план
// (Index/name/module/no_log/passage) стабилен — повторная запись безвредна.
const insertRunPlanSQL = `
INSERT INTO apply_run_plan (apply_id, plan_index, name, module, no_log, passage, params)
SELECT $1, u.plan_index, u.name, u.module, u.no_log, u.passage, u.params::jsonb
FROM unnest($2::int[], $3::text[], $4::text[], $5::bool[], $6::int[], $7::text[])
     AS u(plan_index, name, module, no_log, passage, params)
ON CONFLICT (apply_id, plan_index)
DO UPDATE SET name = EXCLUDED.name, module = EXCLUDED.module, no_log = EXCLUDED.no_log, passage = EXCLUDED.passage, params = EXCLUDED.params
`

// InsertRunPlan пишет план задач прогона (apply_run_plan) одним bulk-upsert-ом.
// Пустой tasks → no-op (нечего писать). Непустой ApplyID обязателен. Вызывается
// один раз при dispatch (scenario-runner) — план host-инвариантен.
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
	// params — параллельный text[]-массив (каждый элемент — JSON или NULL),
	// кастуется в jsonb в SQL. nil-элемент (no_log / нет params) → jsonb NULL.
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

// SelectRunPlanByApplyID возвращает план задач прогона, отсортированный по
// глобальному plan_index. Пустой результат — прогон без сохранённого плана
// (упал до render, либо прогон до NIM-37): caller трактует как пустой план.
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

// RunExistsForIncarnation сообщает, есть ли у инкарнации `name` прогон `applyID`.
// Scope-guard эндпоинта /tasks: apply_run_plan не несёт incarnation_name, поэтому
// принадлежность прогона инкарнации проверяется по apply_runs (иначе чужой apply_id
// вернул бы чужой план). Отсутствие → caller отдаёт 404 (parity SelectRunDetail).
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
