package push

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// PGSoulLookup — production-реализация [SoulLookup] над общим pgxpool.Pool.
// Тонкая обёртка над [soul.SelectBySID]; вынесена отдельно от crud.go ради
// явности call-site в daemon-wire-up и симметрии с PGStore-адаптерами других
// подсистем (sigil, cloud, …).
type PGSoulLookup struct {
	pool *pgxpool.Pool
}

// NewPGSoulLookup собирает адаптер. Pool обязателен (nil-pool — программная
// ошибка caller-а, валится сразу на первом SelectBySID).
func NewPGSoulLookup(pool *pgxpool.Pool) *PGSoulLookup { return &PGSoulLookup{pool: pool} }

// SelectBySID реализует [SoulLookup]: проксирует на [soul.SelectBySID].
func (l *PGSoulLookup) SelectBySID(ctx context.Context, sid string) (*soul.Soul, error) {
	return soul.SelectBySID(ctx, l.pool, sid)
}
