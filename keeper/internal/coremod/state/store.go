package state

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	keeperincarnation "github.com/souls-guild/soul-stack/keeper/internal/incarnation"
)

// Store is the narrow write surface `core.state.*` needs: apply a mutation to
// the incarnation's CURRENT state and commit it, mid-run, with a `state_history`
// row naming the run that caused it ([ADR-0084]).
//
// Narrow rather than a pgxpool, for the same reason as choir.Store and
// soul.Store: a fake implements one method and the contract stays readable.
type Store interface {
	CaptureState(ctx context.Context, spec keeperincarnation.CaptureSpec, mutate func(map[string]any) (map[string]any, error)) (map[string]any, error)
	// ReadState reads the incarnation's current state WITHOUT locking it. Only
	// `core.state.set` uses it, and only to answer "does this field already
	// have a value?" BEFORE minting a secret the write would then discard —
	// minting inside the mutation would hold the row's write lock across a Vault
	// round-trip. The merge re-decides under the lock, so a stale read cannot
	// produce a wrong write; and within a run nothing else writes the row, which
	// the `applying` status enforces.
	ReadState(ctx context.Context, name string) (map[string]any, error)
}

// PGStore is the Postgres adapter, wired in the daemon. Mirrors
// keeper/internal/coremod/choir.PGStore.
type PGStore struct {
	Pool *pgxpool.Pool
}

// NewPGStore is the wire helper for the daemon.
func NewPGStore(pool *pgxpool.Pool) *PGStore {
	return &PGStore{Pool: pool}
}

func (s *PGStore) CaptureState(ctx context.Context, spec keeperincarnation.CaptureSpec, mutate func(map[string]any) (map[string]any, error)) (map[string]any, error) {
	return keeperincarnation.CaptureState(ctx, s.Pool, spec, mutate)
}

func (s *PGStore) ReadState(ctx context.Context, name string) (map[string]any, error) {
	return keeperincarnation.SelectState(ctx, s.Pool, name)
}

var _ Store = (*PGStore)(nil)
