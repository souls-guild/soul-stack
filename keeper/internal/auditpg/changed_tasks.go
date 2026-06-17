package auditpg

import (
	"context"
	"fmt"
	"strconv"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// ChangedTaskKey — идентификатор «эта задача на этом хосте изменилась»:
// (sid, task_idx) одного прогона. Источник — журнал аудита (event_type
// `task.executed` с `payload.status == TASK_STATUS_CHANGED`), НЕ отдельная
// таблица: changed-факт уже фиксируется handler-ом TaskEvent (M2.4,
// events_taskevent.go) на каждую (apply_id, sid, task_idx).
//
// Секрет-гигиена (T3): из audit_log берутся ТОЛЬКО (sid, task_idx) — адрес
// факта, без payload-значений register_data/params/error. Метаданные задачи
// (name/register/id/module) scenario-runner добирает из in-memory []RenderedTask,
// не из журнала.
type ChangedTaskKey struct {
	SID     string
	TaskIdx int
}

// taskStatusChanged — строковое значение `payload.status` для CHANGED-задачи.
// Handler TaskEvent (events_taskevent.go) кладёт статус как `Status().String()`,
// то есть имя enum-константы keeperv1.TaskStatus_TASK_STATUS_CHANGED. Фильтруем
// по этой строке (не по числу) — она в JSONB-payload как текст.
const taskStatusChanged = "TASK_STATUS_CHANGED"

// selectChangedTaskKeysSQL — выборка адресов CHANGED-задач прогона из журнала
// аудита. Фильтр строго по индексируемым колонкам (correlation_id, event_type)
// + JSONB-предикат на status; sid/task_idx достаются из payload как текст и
// число (task_idx handler кладёт числом, JSONB ->> возвращает его текстовую
// форму — парсим в caller-е). Параметры — позиционные плейсхолдеры, без
// конкатенации значений в SQL.
//
// $1 = apply_id (correlation_id task.executed), $2 = event_type, $3 = CHANGED-
// статус. NULLIF на sid отбрасывает строки без sid (их быть не должно — handler
// всегда кладёт sid; защита от мусора). DISTINCT не нужен: пара (sid, task_idx)
// в task.executed уникальна на финальный статус задачи, но retry мог дать
// несколько строк — дедуп делает caller (множество ключей).
const selectChangedTaskKeysSQL = `
SELECT payload->>'sid' AS sid, payload->>'task_idx' AS task_idx
FROM audit_log
WHERE correlation_id = $1
  AND event_type = $2
  AND payload->>'status' = $3
  AND payload->>'sid' IS NOT NULL
  AND payload->>'task_idx' IS NOT NULL
`

// SelectChangedTaskKeys возвращает множество (sid, task_idx) задач прогона
// `applyID`, терминал-ивших со статусом CHANGED (по журналу аудита). Источник —
// `task.executed`-события с `payload.status == TASK_STATUS_CHANGED`; берутся
// ТОЛЬКО адресные поля (sid, task_idx), payload-значения НЕ читаются (секрет-
// гигиена T3).
//
// Результат — set: дубль (apply_id, sid, task_idx) (retry перезаписал статус,
// дав вторую task.executed-строку) схлопывается. Пустой результат — ни одна
// задача не изменилась (или прогон без task.executed-следа). task_idx, не
// распарсившийся в int (мусор в payload), пропускается без ошибки — это
// деградация наблюдаемости, не повод валить финал прогона.
func (r *Reader) SelectChangedTaskKeys(ctx context.Context, applyID string) (map[ChangedTaskKey]struct{}, error) {
	rows, err := r.pool.Query(ctx, selectChangedTaskKeysSQL,
		applyID, string(audit.EventTaskExecuted), taskStatusChanged)
	if err != nil {
		return nil, fmt.Errorf("audit: changed task keys query: %w", err)
	}
	defer rows.Close()

	out := make(map[ChangedTaskKey]struct{})
	for rows.Next() {
		var (
			sid        string
			taskIdxStr string
		)
		if err := rows.Scan(&sid, &taskIdxStr); err != nil {
			return nil, fmt.Errorf("audit: changed task keys scan: %w", err)
		}
		taskIdx, perr := strconv.Atoi(taskIdxStr)
		if perr != nil {
			// Нечисловой task_idx в payload — не должен встречаться (handler кладёт
			// proto-int). Пропускаем: финал прогона важнее одной мусорной строки.
			continue
		}
		out[ChangedTaskKey{SID: sid, TaskIdx: taskIdx}] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("audit: changed task keys iter: %w", err)
	}
	return out, nil
}
