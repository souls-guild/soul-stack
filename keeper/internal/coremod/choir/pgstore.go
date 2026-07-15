package choir

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	keeperchoir "github.com/souls-guild/soul-stack/keeper/internal/choir"
)

// PGStore is a thin adapter over the choir CRUD functions (S-T2) needed by
// the `core.choir` module. It exists so the module depends on the narrow interface
// [Store] rather than free package functions (testing + explicit contract).
// Mirrors keeper/internal/coremod/soul.PGStore.
//
// AddVoice requires a TxBeginner (FOR UPDATE on the Choir row), RemoveVoice —
// requires an ExecQueryRower; *pgxpool.Pool satisfies both, so we keep one Pool.
type PGStore struct {
	Pool *pgxpool.Pool
}

// NewPGStore is a wire helper for the daemon: connects the module to a real pgxpool.Pool.
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

// IncarnationExists is a lightweight existence check for the incarnation (SELECT 1,
// no spec/state deserialization). Used by the module's absent branch as a substitute
// for a hard cross-incarnation guard (S-T5; see member.go).
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
