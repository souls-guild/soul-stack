package soul

import (
	"context"

	keepersoul "github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// PGStore — тонкий adapter поверх keeper/internal/soul функций, нужный модулю
// `core.soul.registered`. Существует, чтобы модуль зависел от узкого
// интерфейса [Store], а не от свободных функций пакета (тестирование +
// явный контракт).
//
// DB-поле — любой ExecQueryRower (pgxpool.Pool / pgx.Conn / pgx.Tx).
type PGStore struct {
	DB keepersoul.ExecQueryRower
}

// NewPGStore — wire-helper для main.go: соединяет модуль с реальным
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
