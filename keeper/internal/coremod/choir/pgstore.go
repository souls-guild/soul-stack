package choir

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	keeperchoir "github.com/souls-guild/soul-stack/keeper/internal/choir"
)

// PGStore — тонкий adapter поверх choir-CRUD функций (S-T2), нужный модулю
// `core.choir`. Существует, чтобы модуль зависел от узкого интерфейса
// [Store], а не от свободных функций пакета (тестирование + явный контракт).
// Симметрично keeper/internal/coremod/soul.PGStore.
//
// AddVoice требует TxBeginner (FOR UPDATE на строке Choir-а), RemoveVoice —
// ExecQueryRower; *pgxpool.Pool удовлетворяет оба, потому держим один Pool.
type PGStore struct {
	Pool *pgxpool.Pool
}

// NewPGStore — wire-helper для daemon-а: соединяет модуль с реальным pgxpool.Pool.
func NewPGStore(pool *pgxpool.Pool) *PGStore {
	return &PGStore{Pool: pool}
}

func (s *PGStore) AddVoice(ctx context.Context, v *keeperchoir.Voice) error {
	return keeperchoir.AddVoice(ctx, s.Pool, v)
}

func (s *PGStore) RemoveVoice(ctx context.Context, incarnation, choirName, sid string) error {
	return keeperchoir.RemoveVoice(ctx, s.Pool, incarnation, choirName, sid)
}

const incarnationExistsSQL = `SELECT 1 FROM incarnation WHERE name = $1`

// IncarnationExists — лёгкая проверка существования инкарнации (SELECT 1, без
// десериализации spec/state). Используется absent-веткой модуля как substitute
// жёсткого cross-incarnation guard (S-T5; см. member.go).
func (s *PGStore) IncarnationExists(ctx context.Context, incarnation string) (bool, error) {
	var dummy int
	err := s.Pool.QueryRow(ctx, incarnationExistsSQL, incarnation).Scan(&dummy)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("choir: incarnation exists probe: %w", err)
	}
	return true, nil
}
