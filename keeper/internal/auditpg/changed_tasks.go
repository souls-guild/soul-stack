package auditpg

import (
	"context"
	"fmt"
	"strconv"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// ChangedTaskKey — идентификатор «эта задача на этом хосте изменилась»:
// (sid, plan_index) одного прогона. Источник — журнал аудита (event_type
// `task.executed` с `payload.status == TASK_STATUS_CHANGED`), НЕ отдельная
// таблица: changed-факт уже фиксируется handler-ом TaskEvent (M2.4,
// events_taskevent.go) на каждую (apply_id, sid, plan_index).
//
// PlanIndex — ГЛОБАЛЬНЫЙ сквозной индекс задачи по всему плану прогона (по всем
// Passage), = RenderedTask.Index (ADR-056 §S1 fix Variant B, T3-канал). Ключ
// корреляции с планом в buildChangedTasks (scenario.state) идёт по
// RenderedTask.Index — поэтому из payload берётся глобальный `plan_index`, а НЕ
// локальный `task_idx` (под staged/per-host-where ≠ глобальному, указывал бы на
// соседнюю задачу → mismatch в state_changes-whitelist (секрет-гигиена) + audit).
// Старые audit-строки без `plan_index` → fallback на `task_idx` (N=1 совпадает).
//
// Секрет-гигиена (T3): из audit_log берутся ТОЛЬКО (sid, plan_index) — адрес
// факта, без payload-значений register_data/params/error. Метаданные задачи
// (name/register/id/module) scenario-runner добирает из in-memory []RenderedTask,
// не из журнала.
type ChangedTaskKey struct {
	SID       string
	PlanIndex int
}

// taskStatusChanged — строковое значение `payload.status` для CHANGED-задачи.
// Handler TaskEvent (events_taskevent.go) кладёт статус как `Status().String()`,
// то есть имя enum-константы keeperv1.TaskStatus_TASK_STATUS_CHANGED. Фильтруем
// по этой строке (не по числу) — она в JSONB-payload как текст.
const taskStatusChanged = "TASK_STATUS_CHANGED"

// taskStatusFailed / taskStatusTimedOut — строковые значения `payload.status`
// для FAILED-задачи (FAILED ∪ TIMED_OUT). TIMED_OUT — частный случай failed
// (apply.proto: «является частным случаем failed»), поэтому onfail-gating
// обязан считать оба «источник упал». Имена enum-констант keeperv1.TaskStatus.
const (
	taskStatusFailed   = "TASK_STATUS_FAILED"
	taskStatusTimedOut = "TASK_STATUS_TIMED_OUT"
)

// selectChangedTaskKeysSQL — выборка адресов CHANGED-задач прогона из журнала
// аудита. Фильтр строго по индексируемым колонкам (correlation_id, event_type)
// + JSONB-предикат на status; sid/plan_index достаются из payload как текст и
// число (handler кладёт оба числами, JSONB ->> возвращает текстовую форму —
// парсим в caller-е). Параметры — позиционные плейсхолдеры, без конкатенации
// значений в SQL.
//
// plan_index (ADR-056 §S1 fix Variant B, T3): COALESCE(plan_index, task_idx) —
// приоритет глобальному сквозному индексу (= RenderedTask.Index, ключ корреляции
// с планом). Старые audit-строки без `plan_index` (прогон до T3-фикса / старый
// Soul) → fallback на локальный `task_idx`, для N=1 совпадающий с глобальным
// (поведение БИТ-В-БИТ). COALESCE на JSONB-text-извлечениях: `payload->>'key'`
// возвращает NULL при отсутствии ключа — берётся следующий аргумент.
//
// $1 = apply_id (correlation_id task.executed), $2 = event_type, $3 = набор
// status-имён (`= ANY($3)`, текстовый массив). Фильтр требует наличие хотя бы
// одного из (plan_index, task_idx): COALESCE обоих IS NOT NULL. NULLIF на sid не
// нужен — handler всегда кладёт sid. DISTINCT не нужен: пара (sid, plan_index) в
// task.executed уникальна на финальный статус задачи, но retry мог дать несколько
// строк — дедуп делает caller (set). Один SQL обслуживает CHANGED- и FAILED-
// выборки — различие лишь в наборе status-имён ($3).
const selectTaskKeysByStatusSQL = `
SELECT payload->>'sid' AS sid,
       COALESCE(payload->>'plan_index', payload->>'task_idx') AS plan_index
FROM audit_log
WHERE correlation_id = $1
  AND event_type = $2
  AND payload->>'status' = ANY($3)
  AND payload->>'sid' IS NOT NULL
  AND COALESCE(payload->>'plan_index', payload->>'task_idx') IS NOT NULL
`

// SelectChangedTaskKeys возвращает множество (sid, plan_index) задач прогона
// `applyID`, терминал-ивших со статусом CHANGED (по журналу аудита). Источник —
// `task.executed`-события с `payload.status == TASK_STATUS_CHANGED`; берутся
// ТОЛЬКО адресные поля (sid, plan_index), payload-значения НЕ читаются (секрет-
// гигиена T3).
//
// plan_index — ГЛОБАЛЬНЫЙ сквозной индекс задачи (= RenderedTask.Index); под
// staged/per-host-where он, а НЕ локальный task_idx, корелирует с планом в
// scenario.buildChangedTasks (ключ ChangedTaskKey{sid, t.Index}). Старые
// audit-строки без `plan_index` читаются с fallback на `task_idx` (COALESCE в
// SQL), для N=1 совпадающий с глобальным.
//
// Результат — set: дубль (apply_id, sid, plan_index) (retry перезаписал статус,
// дав вторую task.executed-строку) схлопывается. Пустой результат — ни одна
// задача не изменилась (или прогон без task.executed-следа). plan_index, не
// распарсившийся в int (мусор в payload), пропускается без ошибки — это
// деградация наблюдаемости, не повод валить финал прогона.
func (r *Reader) SelectChangedTaskKeys(ctx context.Context, applyID string) (map[ChangedTaskKey]struct{}, error) {
	return r.selectTaskKeysByStatus(ctx, applyID, []string{taskStatusChanged})
}

// SelectFailedTaskKeys возвращает множество (sid, plan_index) задач прогона
// `applyID`, терминал-ивших со статусом FAILED либо TIMED_OUT (по журналу
// аудита). Зеркало [SelectChangedTaskKeys] для onfail-rescue-gating (ADR-056 R3,
// cross-passage): keeper резолвит `onfail:[A]` cross-passage по тому, упал ли A
// на хосте. TIMED_OUT включён — apply.proto объявляет его частным случаем failed,
// поэтому rescue обязан срабатывать и по timeout-источнику. Секрет-гигиена та же:
// читаются ТОЛЬКО адресные поля (sid, plan_index).
func (r *Reader) SelectFailedTaskKeys(ctx context.Context, applyID string) (map[ChangedTaskKey]struct{}, error) {
	return r.selectTaskKeysByStatus(ctx, applyID, []string{taskStatusFailed, taskStatusTimedOut})
}

// selectTaskKeysByStatus — общая выборка (sid, plan_index) задач прогона по
// набору terminal-статусов. statuses — имена enum-констант keeperv1.TaskStatus
// (текст в payload). Дедуп через set; нечисловой plan_index пропускается.
func (r *Reader) selectTaskKeysByStatus(ctx context.Context, applyID string, statuses []string) (map[ChangedTaskKey]struct{}, error) {
	rows, err := r.pool.Query(ctx, selectTaskKeysByStatusSQL,
		applyID, string(audit.EventTaskExecuted), statuses)
	if err != nil {
		return nil, fmt.Errorf("audit: task keys query: %w", err)
	}
	defer rows.Close()

	out := make(map[ChangedTaskKey]struct{})
	for rows.Next() {
		var (
			sid        string
			planIdxStr string
		)
		if err := rows.Scan(&sid, &planIdxStr); err != nil {
			return nil, fmt.Errorf("audit: task keys scan: %w", err)
		}
		planIdx, perr := strconv.Atoi(planIdxStr)
		if perr != nil {
			// Нечисловой plan_index/task_idx в payload — не должен встречаться (handler
			// кладёт proto-int). Пропускаем: финал прогона важнее одной мусорной строки.
			continue
		}
		out[ChangedTaskKey{SID: sid, PlanIndex: planIdx}] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("audit: task keys iter: %w", err)
	}
	return out, nil
}
