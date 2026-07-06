package auditpg

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// TaskExecution — per-host итог одной задачи прогона, восстановленный из журнала
// аудита (`task.executed`, NIM-37). Источник — то же событие, что уже пишет
// events_taskevent.go на каждую (apply_id, sid, plan_index): статус, register_data
// (output) и error. Эндпоинт /tasks джойнит это с планом (apply_run_plan) по
// plan_index → адрес хоста внутри задачи.
//
// Секрет-гигиена: register_data/error.message уже прошли MaskSecrets на write-path-е
// (auditpg-writer), а no_log-задачи их вовсе не несут (BuildTaskExecutedPayload
// подавляет) — reader отдаёт то, что записано, без дополнительного маскинга.
type TaskExecution struct {
	SID string

	// PlanIndex — ГЛОБАЛЬНЫЙ сквозной индекс задачи (= RenderedTask.Index); ключ
	// джойна с планом. COALESCE(plan_index, task_idx): старые строки без
	// plan_index → fallback на task_idx (N=1 совпадает).
	PlanIndex int

	// Status — строковое имя terminal-статуса (keeperv1.TaskStatus.String(),
	// например "TASK_STATUS_CHANGED").
	Status string

	// Output — распарсенный register_data (JSON-объект). nil, если register_data
	// отсутствует (задача без register:), подавлен (no_log) или не распарсился.
	Output map[string]any

	// Error — заполнен только на FAILED/TIMED_OUT (nil иначе). Message для
	// no_log-задачи пуст (подавлен на write-path-е).
	Error *TaskExecutionError
}

// TaskExecutionError — error-часть task.executed (code/module/message).
type TaskExecutionError struct {
	Code    string
	Module  string
	Message string
}

// selectTaskExecutionsSQL — все task.executed прогона с адресными полями (sid,
// plan_index), статусом, register_data и error. Фильтр по индексируемым колонкам
// (correlation_id, event_type); JSONB-поля извлекаются как текст. ORDER BY
// created_at ASC: при retry (несколько строк на задачу-хост) более поздняя
// перезаписывает раннюю в свёртке caller-а (последний результат побеждает).
//
// $1 = apply_id (correlation_id task.executed), $2 = event_type.
const selectTaskExecutionsSQL = `
SELECT payload->>'sid'                                      AS sid,
       COALESCE(payload->>'plan_index', payload->>'task_idx') AS plan_index,
       payload->>'status'                                   AS status,
       payload->>'register_data'                            AS register_data,
       payload->'error'->>'code'                            AS err_code,
       payload->'error'->>'module'                          AS err_module,
       payload->'error'->>'message'                         AS err_message,
       (payload ? 'error')                                  AS has_error
FROM audit_log
WHERE correlation_id = $1
  AND event_type = $2
  AND payload->>'sid' IS NOT NULL
  AND COALESCE(payload->>'plan_index', payload->>'task_idx') IS NOT NULL
ORDER BY created_at ASC
`

// SelectTaskExecutions возвращает per-host итоги задач прогона `applyID` из журнала
// аудита (`task.executed`): статус, output (register_data) и error на каждую
// (sid, plan_index). Порядок — по времени (retry-строки идут позже); дедуп/выбор
// последнего результата делает caller (эндпоинт /tasks) при группировке по
// (plan_index, sid). Пустой результат — прогон без task.executed-следа.
func (r *Reader) SelectTaskExecutions(ctx context.Context, applyID string) ([]TaskExecution, error) {
	rows, err := r.pool.Query(ctx, selectTaskExecutionsSQL, applyID, string(audit.EventTaskExecuted))
	if err != nil {
		return nil, fmt.Errorf("audit: task executions query: %w", err)
	}
	defer rows.Close()

	var out []TaskExecution
	for rows.Next() {
		var (
			sid         string
			planIdxStr  string
			status      *string
			registerRaw *string
			errCode     *string
			errModule   *string
			errMessage  *string
			hasError    bool
		)
		if err := rows.Scan(&sid, &planIdxStr, &status, &registerRaw, &errCode, &errModule, &errMessage, &hasError); err != nil {
			return nil, fmt.Errorf("audit: task executions scan: %w", err)
		}
		planIdx, perr := strconv.Atoi(planIdxStr)
		if perr != nil {
			// Нечисловой plan_index/task_idx (мусор в payload) — пропускаем: одна
			// битая строка не должна ронять весь ответ /tasks.
			continue
		}
		te := TaskExecution{SID: sid, PlanIndex: planIdx}
		if status != nil {
			te.Status = *status
		}
		if registerRaw != nil {
			var m map[string]any
			if json.Unmarshal([]byte(*registerRaw), &m) == nil {
				te.Output = m
			}
		}
		if hasError {
			te.Error = &TaskExecutionError{}
			if errCode != nil {
				te.Error.Code = *errCode
			}
			if errModule != nil {
				te.Error.Module = *errModule
			}
			if errMessage != nil {
				te.Error.Message = *errMessage
			}
		}
		out = append(out, te)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("audit: task executions iter: %w", err)
	}
	return out, nil
}
