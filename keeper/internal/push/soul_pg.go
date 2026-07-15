package push

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// PGSoulLookup is the production implementation of [SoulLookup] over the
// shared pgxpool.Pool. A thin wrapper over [soul.SelectBySID]; kept separate
// from crud.go for call-site clarity in daemon wire-up and symmetry with
// PGStore adapters in other subsystems (sigil, cloud, ...).
type PGSoulLookup struct {
	pool *pgxpool.Pool
}

// NewPGSoulLookup assembles the adapter. Pool is required (a nil pool is a
// caller programming error, and it will panic immediately on the first
// SelectBySID).
func NewPGSoulLookup(pool *pgxpool.Pool) *PGSoulLookup { return &PGSoulLookup{pool: pool} }

// SelectBySID implements [SoulLookup]: it proxies to [soul.SelectBySID].
func (l *PGSoulLookup) SelectBySID(ctx context.Context, sid string) (*soul.Soul, error) {
	return soul.SelectBySID(ctx, l.pool, sid)
}
