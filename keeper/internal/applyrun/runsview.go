package applyrun

import (
	"context"
	"fmt"
	"time"
)

// Read-view прогонов инкарнации для Operator API (GET /v1/incarnations/{name}/runs
// и .../runs/{apply_id}). Отделён от write-CRUD (crud.go): здесь только
// агрегирующие SELECT-ы под UI «статус выполнения / текущая джоба».
//
// ГРАНИЦА ДАННЫХ. apply_runs хранит статус НА ХОСТ-СТРОКУ (planned…orphaned),
// НЕ per-task changed/ok/skipped: TaskEvent агрегируется на Soul-е без per-task-
// прогресса в MVP (ADR-012). Единственная per-task деталь в PG — упавшая задача
// (task_idx / failed_plan_index / error_summary на failed-строке). Поэтому
// «детали прогона» — это срез по хостам (какие хосты идут/упали/успели + адрес
// упавшей задачи), а не полный список задач с их статусами.

// RunStatus — агрегатный статус ВСЕГО прогона (свёртка host-строк apply_runs).
// НЕ путать с [Status] (статус одной host-строки) и incarnation.Status
// (error_locked — состояние инкарнации, не прогона).
type RunStatus string

const (
	// RunStatusApplying — хотя бы одна host-строка ещё не терминальна
	// (planned/claimed/running/dispatched). Прогон в процессе — «текущая джоба».
	RunStatusApplying RunStatus = "applying"
	// RunStatusSuccess — все host-строки терминальны и benign (success/no_match).
	RunStatusSuccess RunStatus = "success"
	// RunStatusFailed — все host-строки терминальны, есть хотя бы одна
	// failed/orphaned (не-успех). Приоритетнее cancelled.
	RunStatusFailed RunStatus = "failed"
	// RunStatusCancelled — все host-строки терминальны, есть cancelled и нет
	// failed/orphaned.
	RunStatusCancelled RunStatus = "cancelled"
)

// RunSummary — одна строка списка прогонов (GET .../runs и глобальный GET
// /v1/runs). Свёртка всех host×passage-строк одного apply_id: агрегатный статус,
// границы времени, инициатор. Incarnation — владелец прогона (в per-incarnation
// выборке совпадает с аргументом, в глобальной — из строк apply_id).
type RunSummary struct {
	ApplyID     string
	Incarnation string
	Scenario    string
	Status      RunStatus
	StartedAt   time.Time
	// FinishedAt — NULL, пока хотя бы одна host-строка не финишировала (прогон
	// applying); иначе MAX(finished_at) по строкам.
	FinishedAt   *time.Time
	StartedByAID *string
}

// RunHostStatus — статус одного хоста в рамках прогона (GET .../runs/{apply_id}).
// На один хост приходится N строк (по Passage staged-render); проекция несёт
// per-passage строку как есть — UI видит адрес упавшей задачи per-passage.
//
// FailedTaskIdx — ЛОКАЛЬНЫЙ индекс упавшей задачи в ApplyRequest своего Passage
// (nil на success/ещё-running/dispatch-level фейле). FailedPlanIndex — ГЛОБАЛЬНЫЙ
// сквозной plan_index той же задачи по всему плану (ключ корреляции с планом
// сценария; nil на тех же условиях). ErrorSummary — operator-facing причина
// (`task <idx> <module>: <message>`, secret-masked на write-path).
type RunHostStatus struct {
	SID             string
	Status          Status
	Passage         int
	FailedTaskIdx   *int
	FailedPlanIndex *int
	ErrorSummary    *string
	Attempt         int32
	CancelRequested bool
}

// RunDetail — детали одного прогона (GET .../runs/{apply_id}): шапка + срез по
// хостам. Scenario/StartedAt/StartedByAID берутся из любой host-строки (одинаковы
// в пределах apply_id); Status — агрегат Hosts.
type RunDetail struct {
	ApplyID      string
	Scenario     string
	Status       RunStatus
	StartedAt    time.Time
	FinishedAt   *time.Time
	StartedByAID *string
	Hosts        []RunHostStatus
}

// AggregateRunStatus сворачивает статусы host-строк прогона в [RunStatus].
// Порядок приоритетов (соответствует барьер-классификации, dispatch.go classify):
// любой non-terminal → applying; иначе failed/orphaned → failed; иначе cancelled
// → cancelled; иначе (только success/no_match) → success. Пустой срез → applying
// (строки ещё не диспатчены — прогон не завершён).
func AggregateRunStatus(statuses []Status) RunStatus {
	if len(statuses) == 0 {
		return RunStatusApplying
	}
	var hasFailure, hasCancelled bool
	for _, s := range statuses {
		switch s {
		case StatusSuccess, StatusNoMatch:
			// benign-терминал.
		case StatusFailed, StatusOrphaned:
			hasFailure = true
		case StatusCancelled:
			hasCancelled = true
		default:
			// planned/claimed/running/dispatched — не терминал: прогон идёт.
			return RunStatusApplying
		}
	}
	switch {
	case hasFailure:
		return RunStatusFailed
	case hasCancelled:
		return RunStatusCancelled
	default:
		return RunStatusSuccess
	}
}

const listRunsByIncarnationSQL = `
SELECT apply_id,
       MIN(scenario)                                   AS scenario,
       MIN(started_at)                                 AS started_at,
       CASE WHEN bool_and(finished_at IS NOT NULL)
            THEN MAX(finished_at) END                  AS finished_at,
       MIN(started_by_aid)                             AS started_by_aid,
       array_agg(status)                               AS statuses
FROM apply_runs
WHERE incarnation_name = $1
GROUP BY apply_id
ORDER BY MIN(started_at) DESC, apply_id DESC
LIMIT $2 OFFSET $3
`

const countRunsByIncarnationSQL = `
SELECT COUNT(DISTINCT apply_id) FROM apply_runs WHERE incarnation_name = $1
`

// ListRunsByIncarnation отдаёт страницу прогонов инкарнации (свёртка по apply_id),
// новейшие сверху (MIN(started_at) DESC), и общее число прогонов. scenario/
// started_by_aid одинаковы в пределах apply_id — берём MIN как детерминированный
// представитель. finished_at — MAX по строкам, но только когда ВСЕ финишировали
// (иначе NULL: прогон ещё applying). statuses — набор host-статусов для свёртки
// [AggregateRunStatus].
//
// scope-гейт — на уровне handler-а (принадлежность incarnation проверяется до
// вызова: WHERE по incarnation_name уже сужает выборку одной инкарнацией).
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
		)
		if err := rows.Scan(&rs.ApplyID, &rs.Scenario, &rs.StartedAt, &rs.FinishedAt, &rs.StartedByAID, &statusStrs); err != nil {
			return nil, 0, fmt.Errorf("applyrun: scan run summary: %w", err)
		}
		rs.Incarnation = incarnationName
		rs.Status = AggregateRunStatus(toStatuses(statusStrs))
		out = append(out, rs)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("applyrun: iterate runs: %w", err)
	}
	return out, total, nil
}

const selectRunHostsSQL = `
SELECT sid, status, passage, task_idx, failed_plan_index, error_summary,
       attempt, cancel_requested, scenario, started_at, finished_at, started_by_aid
FROM apply_runs
WHERE apply_id = $1 AND incarnation_name = $2
ORDER BY sid ASC, passage ASC
`

// SelectRunDetail отдаёт детали одного прогона: все host×passage-строки apply_id,
// принадлежащего указанной инкарнации (WHERE по обоим — фильтр enforcement-а
// «apply_id этой инкарнации»; чужой apply_id вернёт 0 строк → [ErrApplyRunNotFound]).
// Шапка (scenario/started_at/started_by_aid) — из первой строки (одинакова в
// пределах apply_id); finished_at детали — MAX по строкам, но nil пока не все
// финишировали; Status — агрегат Hosts.
func SelectRunDetail(ctx context.Context, db ExecQueryRower, applyID, incarnationName string) (*RunDetail, error) {
	rows, err := db.Query(ctx, selectRunHostsSQL, applyID, incarnationName)
	if err != nil {
		return nil, fmt.Errorf("applyrun: run detail query: %w", err)
	}
	defer rows.Close()

	var (
		detail   RunDetail
		statuses []Status
		// финальность прогона: FinishedAt отдаём только когда каждая строка
		// финишировала (max по non-nil; nil если хоть одна ещё running).
		allFinished = true
		maxFinished time.Time
	)
	for rows.Next() {
		var (
			hs           RunHostStatus
			statusStr    string
			rowScenario  string
			rowStarted   time.Time
			rowFinished  *time.Time
			rowStartedBy *string
		)
		if err := rows.Scan(
			&hs.SID, &statusStr, &hs.Passage, &hs.FailedTaskIdx, &hs.FailedPlanIndex,
			&hs.ErrorSummary, &hs.Attempt, &hs.CancelRequested,
			&rowScenario, &rowStarted, &rowFinished, &rowStartedBy,
		); err != nil {
			return nil, fmt.Errorf("applyrun: scan run host: %w", err)
		}
		hs.Status = Status(statusStr)
		if detail.ApplyID == "" {
			detail.ApplyID = applyID
			detail.Scenario = rowScenario
			detail.StartedAt = rowStarted
			detail.StartedByAID = rowStartedBy
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
	detail.Status = AggregateRunStatus(statuses)
	if allFinished {
		detail.FinishedAt = &maxFinished
	}
	return &detail, nil
}

// toStatuses конвертирует срез строк-статусов из array_agg в []Status.
func toStatuses(ss []string) []Status {
	out := make([]Status, len(ss))
	for i, s := range ss {
		out[i] = Status(s)
	}
	return out
}
