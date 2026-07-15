package cert

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	keepercert "github.com/souls-guild/soul-stack/keeper/internal/cert"
)

// PGStore is a thin adapter over the cert CRUD functions (keeper/internal/cert),
// needed by the `core.cert.registered` module. It exists so the module depends on
// the narrow [Store] interface rather than free package functions (testing +
// explicit contract). Mirrors coremod/choir.PGStore / coremod/soul.PGStore.
type PGStore struct {
	Pool *pgxpool.Pool
}

// NewPGStore is a wire helper for the daemon.
func NewPGStore(pool *pgxpool.Pool) *PGStore {
	return &PGStore{Pool: pool}
}

func (s *PGStore) SelectActive(ctx context.Context, incarnationID string, kind keepercert.Kind) (*keepercert.Warrant, error) {
	return keepercert.SelectActive(ctx, s.Pool, incarnationID, kind)
}

func (s *PGStore) RegisterActive(ctx context.Context, w *keepercert.Warrant) error {
	return keepercert.RegisterActive(ctx, s.Pool, w)
}
