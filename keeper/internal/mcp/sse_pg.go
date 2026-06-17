package mcp

import (
	"context"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
)

// ApplyAccessPG — прод-реализация [applyAccessStore] поверх pgxpool.Pool
// (через узкий [applyrun.ExecQueryRower]). Резолвит apply_id → владелец +
// incarnation для SSE-RBAC (M1). Экспортирован для wire-up из cmd/keeper.
type ApplyAccessPG struct {
	db applyrun.ExecQueryRower
}

// NewApplyAccessPG собирает PG-адаптер. db — обычно *pgxpool.Pool.
func NewApplyAccessPG(db applyrun.ExecQueryRower) *ApplyAccessPG {
	return &ApplyAccessPG{db: db}
}

// Access резолвит apply_id → владелец + incarnation. Возвращает
// applyrun.ErrApplyRunNotFound, если прогона нет.
func (s *ApplyAccessPG) Access(ctx context.Context, applyID string) (*applyrun.Access, error) {
	return applyrun.SelectAccessByApplyID(ctx, s.db, applyID)
}
