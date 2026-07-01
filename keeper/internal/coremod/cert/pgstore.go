package cert

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	keepercert "github.com/souls-guild/soul-stack/keeper/internal/cert"
)

// PGStore — тонкий adapter поверх cert-CRUD функций (keeper/internal/cert),
// нужный модулю `core.cert.registered`. Существует, чтобы модуль зависел от
// узкого интерфейса [Store], а не от свободных функций пакета (тестирование +
// явный контракт). Симметрично coremod/choir.PGStore / coremod/soul.PGStore.
type PGStore struct {
	Pool *pgxpool.Pool
}

// NewPGStore — wire-helper для daemon-а.
func NewPGStore(pool *pgxpool.Pool) *PGStore {
	return &PGStore{Pool: pool}
}

func (s *PGStore) SelectActive(ctx context.Context, incarnationID string, kind keepercert.Kind) (*keepercert.Warrant, error) {
	return keepercert.SelectActive(ctx, s.Pool, incarnationID, kind)
}

func (s *PGStore) RegisterActive(ctx context.Context, w *keepercert.Warrant) error {
	return keepercert.RegisterActive(ctx, s.Pool, w)
}
