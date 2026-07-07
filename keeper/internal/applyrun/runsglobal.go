package applyrun

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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
	// Service — фильтр по сервису инкарнации-владельца ("" = все); точное
	// совпадение через incarnation.service (index-safe, incarnation_service_idx).
	Service string
	// Q — свободный поиск (ILIKE-substring) по incarnation/scenario/service/
	// started_by ("" = без поиска). LIKE-метасимволы экранируются (литеральные).
	Q string
	// StartedAfter/StartedBefore — inclusive-границы по АГРЕГАТУ started_at прогона
	// (MIN(started_at)); nil = не задано. Фильтр по агрегату → в outerWhere (после
	// GROUP BY): started_at не константен внутри apply_id, в sub расщепил бы группу.
	StartedAfter  *time.Time
	StartedBefore *time.Time
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
	"service":     "service",
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
//
// apply_runs заалиашена `ar`, incarnation — `i`: у incarnation свой status →
// bare-колонки стали бы ambiguous. LEFT JOIN защитно (FK ON DELETE CASCADE не
// оставляет orphan, но JOIN не роняет прогон при рассинхроне); service через
// COALESCE к пустой строке — NULL-safe скан в string.
var runsAggregateSelect = fmt.Sprintf(`
SELECT ar.apply_id,
       MIN(ar.incarnation_name)                        AS incarnation,
       COALESCE(MIN(i.service), '')                    AS service,
       MIN(ar.scenario)                                AS scenario,
       MIN(ar.started_at)                              AS started_at,
       CASE WHEN bool_and(ar.finished_at IS NOT NULL)
            THEN MAX(ar.finished_at) END               AS finished_at,
       MIN(ar.started_by_aid)                          AS started_by_aid,
       CASE
         WHEN bool_or(ar.status NOT IN (%[1]s))        THEN '%[2]s'
         WHEN bool_or(ar.status IN ('%[3]s','%[4]s'))  THEN '%[5]s'
         WHEN bool_or(ar.status = '%[6]s')             THEN '%[7]s'
         ELSE '%[8]s'
       END                                             AS status
FROM apply_runs ar
LEFT JOIN incarnation i ON ar.incarnation_name = i.name`,
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
		clauses = append(clauses, fmt.Sprintf("ar.incarnation_name = $%d", len(args)))
	}
	if filter.Service != "" {
		args = append(args, filter.Service)
		clauses = append(clauses, fmt.Sprintf("i.service = $%d", len(args)))
	}
	if filter.Q != "" {
		args = append(args, "%"+escapeLike(filter.Q)+"%")
		n := len(args)
		clauses = append(clauses, fmt.Sprintf(
			"(ar.incarnation_name ILIKE $%[1]d OR ar.scenario ILIKE $%[1]d OR i.service ILIKE $%[1]d OR ar.started_by_aid ILIKE $%[1]d)",
			n))
	}
	// Purview-scope — подзапросом по incarnation (fail-closed: пустой scope даёт
	// WHERE FALSE внутри подзапроса → ни одного прогона).
	cond, args := incarnation.ScopeCondition(args, scope)
	if cond != "" {
		clauses = append(clauses, "ar.incarnation_name IN (SELECT name FROM incarnation WHERE "+cond+")")
	}

	sub = runsAggregateSelect
	if len(clauses) > 0 {
		sub += "\nWHERE " + strings.Join(clauses, " AND ")
	}
	sub += "\nGROUP BY ar.apply_id"

	// Внешний WHERE — по АГРЕГАТНЫМ колонкам свёртки (status, started_at):
	// фильтруются ПОСЛЕ GROUP BY (started_at = MIN по apply_id, не константен внутри
	// группы — в sub расщепил бы её). И countSQL, и listSQL строятся из sub+outerWhere
	// → total/страница консистентны. Все значения — bind-параметры.
	var outer []string
	if filter.Status != "" {
		args = append(args, string(filter.Status))
		outer = append(outer, fmt.Sprintf("status = $%d", len(args)))
	}
	if filter.StartedAfter != nil {
		args = append(args, *filter.StartedAfter)
		outer = append(outer, fmt.Sprintf("started_at >= $%d", len(args)))
	}
	if filter.StartedBefore != nil {
		args = append(args, *filter.StartedBefore)
		outer = append(outer, fmt.Sprintf("started_at <= $%d", len(args)))
	}
	if len(outer) > 0 {
		outerWhere = "\nWHERE " + strings.Join(outer, " AND ")
	}
	return sub, outerWhere, args
}

// likeEscaper экранирует LIKE/ILIKE-метасимволы (backslash-escape по умолчанию в
// PG): литеральные %/_/\ в свободном поиске не работают как wildcard. Один проход
// NewReplacer (backslash первым в списке пар не даёт двойного экранирования).
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// escapeLike готовит Q к подстановке в `%…%` ILIKE-паттерн.
func escapeLike(s string) string { return likeEscaper.Replace(s) }

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
	listSQL := "SELECT apply_id, incarnation, scenario, service, started_at, finished_at, started_by_aid, status FROM (" +
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
		if err := rows.Scan(&rs.ApplyID, &rs.Incarnation, &rs.Scenario, &rs.Service, &rs.StartedAt,
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
