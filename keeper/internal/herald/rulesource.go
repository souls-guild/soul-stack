package herald

import (
	"context"
	"fmt"
)

// PGRuleSource — PG-реализация [RuleSource]: читает ВКЛЮЧЁННЫЕ Tiding-правила
// для снимка dispatcher-а. Запрос идёт по partial-индексу tidings_enabled_idx
// (WHERE enabled=true, миграция 071) — выключенные подписки не сканируются.
//
// Реюзает узкий [ExecQueryRower] и пакетные хелперы scan/collect из crud.go
// (тот же пакет): схема колонок и парсинг строки общие с CRUD-слоем.
type PGRuleSource struct {
	DB ExecQueryRower
}

// enabledTidingsSQL — снимок включённых правил. Без OFFSET/LIMIT: набор
// правил мал (десятки), грузится целиком и кэшируется dispatcher-ом.
// Сортировка по name для детерминизма (порядок матча/jobs стабилен в логах).
const enabledTidingsSQL = `SELECT ` + tidingColumns + `
FROM tidings
WHERE enabled = true
ORDER BY name ASC`

// EnabledTidings возвращает текущий снимок включённых Tiding-правил.
func (s PGRuleSource) EnabledTidings(ctx context.Context) ([]*Tiding, error) {
	rows, err := s.DB.Query(ctx, enabledTidingsSQL)
	if err != nil {
		return nil, fmt.Errorf("herald: query enabled tidings: %w", err)
	}
	return collectTidings(rows)
}
