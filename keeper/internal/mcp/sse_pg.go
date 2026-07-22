package mcp

import (
	"context"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
)

// ApplyAccessPG is the production [applyAccessStore] implementation over
// pgxpool.Pool (via the narrow [applyrun.ExecQueryRower]). Resolves
// apply_id → owner + incarnation for SSE-RBAC (M1). Exported for wire-up
// from cmd/keeper.
type ApplyAccessPG struct {
	db applyrun.ExecQueryRower
}

// NewApplyAccessPG builds the PG adapter. db is usually *pgxpool.Pool.
func NewApplyAccessPG(db applyrun.ExecQueryRower) *ApplyAccessPG {
	return &ApplyAccessPG{db: db}
}

// Access resolves apply_id → owner + incarnation. Returns
// applyrun.ErrApplyRunNotFound if the run does not exist.
func (s *ApplyAccessPG) Access(ctx context.Context, applyID string) (*applyrun.Access, error) {
	return applyrun.SelectAccessByApplyID(ctx, s.db, applyID)
}
