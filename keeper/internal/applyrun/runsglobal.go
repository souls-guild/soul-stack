package applyrun

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
)

// Глобальный read-view прогонов ЧЕРЕЗ ВСЕ инкарнации (GET /v1/runs + /v1/runs/stats,
// страница «All Runs» UI). Та же граница данных, что у per-incarnation view
// (runsview.go): прогон = группа host-строк apply_runs по apply_id, НЕ хост-строка.
// Purview-scope (ADR-047) приходит из handler-а как [incarnation.ListScope] и
// уходит в SQL подзапросом по таблице incarnation — единый источник семантики
// scope с incarnation.SelectAll ([incarnation.ScopeCondition]).

// RunsFilter — пользовательские фильтры глобального списка прогонов (что оператор
// попросил показать; scope — что ему положено видеть, AND-пересечение в WHERE).
type RunsFilter struct {
	// Status — фильтр по агрегатному статусу прогона ("" = все). Валидность
	// значения — на handler-е ([ValidRunStatus] → 422).
	Status RunStatus
	// Incarnation — фильтр по имени инкарнации ("" = все).
	Incarnation string
	// Sort — поле сортировки (whitelist [runsSortableColumns], "" = дефолт
	// started_at). SortDir — asc|desc ("" = desc). Невалидное значение всплывает
	// из [ListRuns] sentinel-ошибкой ([ErrInvalidRunsSortField]/[ErrInvalidRunsSortDir]
	// → 422 на handler-е). ADR-068 §B1.
	Sort    string
	SortDir string
}

// Sentinel-ошибки валидации сортировки [ListRuns] (handler маппит в 422). ADR-068 §B1.
var (
	ErrInvalidRunsSortField = errors.New("applyrun: invalid sort field")
	ErrInvalidRunsSortDir   = errors.New("applyrun: invalid sort direction")
)

// runsSortableColumns — closed whitelist полей сортировки GET /v1/runs →
// alias-колонка свёртки (подзапрос runs). Колонка берётся отсюда, не из сырой
// строки — иначе SQL-инъекция через ORDER BY. ADR-068 §B1.
var runsSortableColumns = map[string]string{
	"started_at":  "started_at",
	"finished_at": "finished_at",
	"status":      "status",
	"incarnation": "incarnation",
	"scenario":    "scenario",
}

// ValidRunStatus — закрытый набор агрегатных статусов прогона (фильтр /v1/runs).
func ValidRunStatus(s RunStatus) bool {
	switch s {
	case RunStatusApplying, RunStatusSuccess, RunStatusFailed, RunStatusCancelled:
		return true
	}
	return false
}

// RunsStatusCounts — счётчики прогонов по агрегатному статусу. Total = сумма всех.
type RunsStatusCounts struct {
	Total     int
	Applying  int
	Success   int
	Failed    int
	Cancelled int
}

// RunsStats — сводка глобального read-view прогонов: за всё время + за последние
// 24 часа (окно по MIN(started_at) прогона — та же ось, что порядок списка).
type RunsStats struct {
	All     RunsStatusCounts
	Last24h RunsStatusCounts
}

// runsAggregateSelect — свёртка apply_runs по apply_id: общая подзапрос-основа
// списка и статистики. Колонка status — SQL-форма [AggregateRunStatus]; литералы
// собраны из Go-констант (init ниже) — единый источник приоритетов applying >
// failed > cancelled > success. Неизвестный статус трактуется как non-terminal
// (parity default-ветки AggregateRunStatus).
var runsAggregateSelect = fmt.Sprintf(`
SELECT apply_id,
       MIN(incarnation_name)                           AS incarnation,
       MIN(scenario)                                   AS scenario,
       MIN(started_at)                                 AS started_at,
       CASE WHEN bool_and(finished_at IS NOT NULL)
            THEN MAX(finished_at) END                  AS finished_at,
       MIN(started_by_aid)                             AS started_by_aid,
       CASE
         WHEN bool_or(status NOT IN (%[1]s))           THEN '%[2]s'
         WHEN bool_or(status IN ('%[3]s','%[4]s'))     THEN '%[5]s'
         WHEN bool_or(status = '%[6]s')                THEN '%[7]s'
         ELSE '%[8]s'
       END                                             AS status
FROM apply_runs`,
	terminalStatusesSQL(), RunStatusApplying,
	StatusFailed, StatusOrphaned, RunStatusFailed,
	StatusCancelled, RunStatusCancelled,
	RunStatusSuccess,
)

// terminalStatusesSQL — терминальные host-статусы списком SQL-литералов
// ('success','no_match',...). Статусы — доверенные константы пакета, не ввод.
func terminalStatusesSQL() string {
	terminal := []Status{StatusSuccess, StatusNoMatch, StatusFailed, StatusOrphaned, StatusCancelled}
	quoted := make([]string, len(terminal))
	for i, s := range terminal {
		quoted[i] = "'" + string(s) + "'"
	}
	return strings.Join(quoted, ",")
}

// buildRunsQuery собирает подзапрос-свёртку с фильтрами/scope и внешний WHERE по
// агрегатному статусу. Возвращает тело `(<свёртка>) runs` + внешний WHERE + args.
func buildRunsQuery(filter RunsFilter, scope incarnation.ListScope) (sub, outerWhere string, args []any) {
	var clauses []string
	if filter.Incarnation != "" {
		args = append(args, filter.Incarnation)
		clauses = append(clauses, fmt.Sprintf("incarnation_name = $%d", len(args)))
	}
	// Purview-scope — подзапросом по incarnation (fail-closed: пустой scope даёт
	// WHERE FALSE внутри подзапроса → ни одного прогона).
	cond, args := incarnation.ScopeCondition(args, scope)
	if cond != "" {
		clauses = append(clauses, "incarnation_name IN (SELECT name FROM incarnation WHERE "+cond+")")
	}

	sub = runsAggregateSelect
	if len(clauses) > 0 {
		sub += "\nWHERE " + strings.Join(clauses, " AND ")
	}
	sub += "\nGROUP BY apply_id"

	if filter.Status != "" {
		args = append(args, string(filter.Status))
		outerWhere = fmt.Sprintf("\nWHERE status = $%d", len(args))
	}
	return sub, outerWhere, args
}

// buildRunsOrderBy — тело ORDER BY из whitelist-поля + направления. Дефолт поля
// started_at, направления desc (byte-exact прежнему `started_at DESC, apply_id DESC`).
// Tie-break `apply_id DESC` добавляется всегда (стабильность при равных значениях
// сорт-колонки). finished_at → NULLS LAST (applying-прогоны без finished_at — в
// конец при любом направлении). ADR-068 §B1.
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

// runsSortDirSQL валидирует направление сортировки. Пустое → DESC (дефолт
// /v1/runs — новейшие сверху, в отличие от asc-дефолта incarnation).
func runsSortDirSQL(d string) (string, error) {
	switch d {
	case "", "desc":
		return "DESC", nil
	case "asc":
		return "ASC", nil
	}
	return "", fmt.Errorf("%w: %q", ErrInvalidRunsSortDir, d)
}

// ListRuns отдаёт страницу прогонов через все инкарнации (свёртка по apply_id),
// в порядке filter.Sort/SortDir (дефолт — новейшие сверху), и общее число прогонов
// под теми же фильтрами/scope. Фильтр по агрегатному статусу — на SQL-уровне (иначе
// total/offset разъехались бы с Go-постфильтром).
func ListRuns(ctx context.Context, db ExecQueryRower, filter RunsFilter, scope incarnation.ListScope, offset, limit int) ([]RunSummary, int, error) {
	// Валидация sort — до БД: невалидное поле/направление → sentinel-ошибка (422),
	// без лишнего count-запроса.
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

	// ORDER BY — из whitelist-выражения (не из сырого ввода); countSQL выше НЕ
	// затрагивается — total от сортировки не зависит (ADR-068 §B1).
	listSQL := "SELECT apply_id, incarnation, scenario, started_at, finished_at, started_by_aid, status FROM (" +
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
		if err := rows.Scan(&rs.ApplyID, &rs.Incarnation, &rs.Scenario, &rs.StartedAt,
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

// SelectRunsStats отдаёт сводные счётчики прогонов (по агрегатному статусу, за всё
// время и за 24 часа) одним запросом по свёртке в границах scope.
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
