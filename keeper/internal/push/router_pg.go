package push

// router_pg.go — production implementation of [PGRouterReader] over
// pgxpool.Pool (ADR-032 amendment 2026-05-27, P2 W-3 Multi-provider routing).
//
// A separate type wrapping pgPoolTargetReader (see target_pg.go) adds reading
// `souls.coven[]` for the Level 2 resolve. Kept isolated from the
// target-reader: the latter is used broadly in PGFallbackTargetResolver (the
// SendApply hot path, where coven doesn't need to be read), so merging them
// into one type isn't worth it.

import (
	"context"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// pgPoolRouterReader implements [PGRouterReader] over soul.ExecQueryRower.
type pgPoolRouterReader struct {
	db soul.ExecQueryRower
}

// NewPGRouterReader adapts a pgxpool.Pool (or any soul.ExecQueryRower) to
// [PGRouterReader]. Used by setupPushDispatchers in daemon wire-up.
func NewPGRouterReader(db soul.ExecQueryRower) PGRouterReader {
	return &pgPoolRouterReader{db: db}
}

func (r *pgPoolRouterReader) SelectSshTarget(ctx context.Context, sid string) (*soul.SSHTarget, error) {
	return soul.SelectSshTarget(ctx, r.db, sid)
}

func (r *pgPoolRouterReader) SelectCovens(ctx context.Context, sid string) ([]string, error) {
	// SelectBySID is the only CRUD method that returns a full Soul along with
	// coven[]. The router only needs the labels, but adding SQL dedicated to
	// the router would complicate schema invalidation; the cost of 5 extra
	// fields in the Soul row is negligible next to a separate round trip or a
	// separate SELECT.
	s, err := soul.SelectBySID(ctx, r.db, sid)
	if err != nil {
		return nil, err
	}
	return s.Coven, nil
}
