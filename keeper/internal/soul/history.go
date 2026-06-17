// Per-host operation history (`GET /v1/souls/{sid}/history`).
//
// Агрегирует в единый timeline две таблицы, у которых есть per-host строка
// под целевым SID:
//
//   - `apply_runs` (PK (apply_id, sid)) — scenario-задачи, доехавшие до хоста
//     в рамках scenario-прогона инкарнации.
//   - `errands` (PK errand_id, FK sid) — ad-hoc pull-exec одиночного модуля
//     (ADR-033).
//
// push_runs НЕ включён: per-host данные там живут только внутри jsonb
// `summary` (sid → {...}) и массива `inventory_sids` — извлечение per-host
// строки требует jsonb-распаковки / UNNEST, что выходит за UNION-форму этого
// эндпоинта. Follow-up: отдельный SELECT по push_runs WHERE $sid = ANY(
// inventory_sids) с распаковкой summary->$sid.
//
// Реализация — UNION ALL двух SELECT-ов с общей проекцией (type-дискриминатор
// + nullable-колонки несовпадающих полей), ORDER BY started_at DESC,
// LIMIT/OFFSET снаружи. total — отдельный SUM(COUNT) под тем же фильтром без
// LIMIT/OFFSET (для UI-пагинации, паритет errand.Store.List / soul.SelectAll).
package soul

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// HistoryType — дискриминатор источника записи в per-host timeline.
type HistoryType string

const (
	// HistoryTypeScenario — строка из `apply_runs` (scenario-задача на хост).
	HistoryTypeScenario HistoryType = "scenario"
	// HistoryTypeErrand — строка из `errands` (ad-hoc exec).
	HistoryTypeErrand HistoryType = "errand"
)

// ValidHistoryType — известен ли тип (для валидации query-фильтра `type`).
func ValidHistoryType(t HistoryType) bool {
	switch t {
	case HistoryTypeScenario, HistoryTypeErrand:
		return true
	default:
		return false
	}
}

// HistoryItem — одна запись per-host timeline. Поля, специфичные для одного
// источника, nil/"" для другого (Incarnation/Scenario — только scenario;
// Module — только errand).
type HistoryItem struct {
	Type        HistoryType
	ID          string // apply_id (scenario) | errand_id (errand).
	Incarnation string // scenario-only.
	Scenario    string // scenario-only.
	Module      string // errand-only.
	Status      string
	StartedAt   time.Time
	FinishedAt  *time.Time
	// VoyageID — back-link на Voyage (ADR-043), резолвится через voyage_targets
	// (scenario → vt.apply_id, errand → vt.errand_id). nil для прямого
	// incarnation.run / standalone errand / исторических записей (норма).
	VoyageID *string
}

// HistoryFilter — параметры `GET /v1/souls/{sid}/history`. Пустые поля = «без
// фильтра». SID обязателен (caller валидирует SIDPattern). Types — multi-value
// OR по источнику (пусто = оба). Since в UTC (started_at > value).
type HistoryFilter struct {
	SID   string
	Types []HistoryType
	Since time.Time
}

// includeScenario / includeErrand — какие источники участвуют в UNION под
// фильтром Types. Пустой Types → оба.
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

// SelectHistory возвращает страницу per-host timeline под фильтром,
// отсортированную по started_at DESC, и total под тем же фильтром без
// LIMIT/OFFSET.
//
// SID обязателен — пустой возвращает ошибку (caller-bug). Если фильтр Types
// исключает оба источника (теоретически невозможно — валидатор это режет),
// возвращаются пустой список и total=0 без обращения к БД.
func SelectHistory(ctx context.Context, db ExecQueryRower, f HistoryFilter, offset, limit int) ([]HistoryItem, int, error) {
	if f.SID == "" {
		return nil, 0, fmt.Errorf("soul: history requires sid")
	}

	scen := f.includeScenario()
	err := f.includeErrand()
	if !scen && !err {
		return nil, 0, nil
	}

	// $1=sid всегда; $2=since (опц.). Оба SELECT-а используют те же плейсхолдеры
	// (UNION ALL делит args), чтобы не плодить позиционные сдвиги.
	args := []any{f.SID}
	var sinceCond string
	if !f.Since.IsZero() {
		args = append(args, f.Since.UTC())
		sinceCond = " AND started_at > $2"
	}

	// Per-source SELECT-ы с общей 9-колоночной проекцией:
	//   type, id, incarnation, scenario, module, status, started_at,
	//   finished_at, voyage_id
	// Несовпадающие поля — NULL-литералы нужного типа. voyage_id берётся из
	// voyage_targets LEFT JOIN-ом (NULL для не-Voyage прогонов).
	//
	// LEFT JOIN не множит строки истории: инвариант «один apply_id/errand_id →
	// максимум одна строка voyage_targets» гарантируется partial-UNIQUE индексом
	// (миграция 063), так что строка apply_runs/errands матчится максимум с одним
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

	// total — COUNT над объединением (без LIMIT/OFFSET).
	countSQL := "SELECT COUNT(*) FROM (" + union + "\n) AS h"
	var total int
	if cerr := db.QueryRow(ctx, countSQL, args...).Scan(&total); cerr != nil {
		return nil, 0, fmt.Errorf("soul: history count: %w", cerr)
	}

	// Страница: ORDER BY started_at DESC, LIMIT/OFFSET. id вторичным ключом —
	// стабильный порядок при равных started_at (детерминизм пагинации).
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
