package herald

import (
	"context"
	"fmt"
)

// PGRuleSource is the PG implementation of [RuleSource]: it reads enabled Tiding
// rules for the dispatcher's snapshot. The query uses partial index
// tidings_enabled_idx (WHERE enabled=true, migration 071), so disabled
// subscriptions are not scanned.
//
// It reuses the narrow [ExecQueryRower] and package scan/collect helpers from
// crud.go: column layout and row parsing are shared with the CRUD layer.
type PGRuleSource struct {
	DB ExecQueryRower
}

// enabledTidingsSQL is a snapshot of enabled rules. No OFFSET/LIMIT: the rule set
// is small (tens), loaded as a whole, and cached by the dispatcher. Sorting by
// name keeps match/job order deterministic in logs.
const enabledTidingsSQL = `SELECT ` + tidingColumns + `
FROM tidings
WHERE enabled = true
ORDER BY name ASC`

// EnabledTidings returns the current snapshot of enabled Tiding rules.
func (s PGRuleSource) EnabledTidings(ctx context.Context) ([]*Tiding, error) {
	rows, err := s.DB.Query(ctx, enabledTidingsSQL)
	if err != nil {
		return nil, fmt.Errorf("herald: query enabled tidings: %w", err)
	}
	return collectTidings(rows)
}
