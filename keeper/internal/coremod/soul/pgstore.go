package soul

import (
	"context"

	keepersoul "github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// PGStore is a thin adapter over the keeper/internal/soul functions needed by
// the `core.soul.registered` module. It exists so the module depends on the narrow
// [Store] interface rather than free package functions (testing +
// explicit contract).
//
// DB is any ExecQueryRower (pgxpool.Pool / pgx.Conn / pgx.Tx).
type PGStore struct {
	DB keepersoul.ExecQueryRower
}

// NewPGStore is a wire helper for main.go: connects the module to a real
// pgxpool.Pool.
func NewPGStore(db keepersoul.ExecQueryRower) *PGStore {
	return &PGStore{DB: db}
}

func (s *PGStore) SelectBySID(ctx context.Context, sid string) (*keepersoul.Soul, error) {
	return keepersoul.SelectBySID(ctx, s.DB, sid)
}

func (s *PGStore) Insert(ctx context.Context, soul *keepersoul.Soul) error {
	return keepersoul.Insert(ctx, s.DB, soul)
}

func (s *PGStore) UpdateCoven(ctx context.Context, sid string, coven []string) ([]string, error) {
	return keepersoul.UpdateCoven(ctx, s.DB, sid, coven)
}

func (s *PGStore) SoulsWithSoulprint(ctx context.Context, sids []string) (map[string]struct{}, error) {
	return keepersoul.SelectSoulsWithSoulprint(ctx, s.DB, sids)
}
