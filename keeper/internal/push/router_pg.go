package push

// router_pg.go — production implementation of [PGRouterReader] over
// pgxpool.Pool (ADR-032 amendment 2026-05-27, P2 W-3 Multi-provider routing).
//
// A separate type wrapping pgPoolTargetReader (see target_pg.go) adds reading
// a host's effective coven labels for the Level 2 resolve. Kept isolated from
// the target-reader: the latter is used broadly in PGFallbackTargetResolver (the
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

// SelectCovens reads the host's own `souls.coven[]` and, separately, the labels
// it inherits from the incarnations it belongs to (ADR-080, NIM-251). Returning
// only the column left `coven_default_providers: {redis-prod: bastion-eu}` dead
// for the hosts OF incarnation `redis-prod` — the label an operator would most
// naturally reach for.
func (r *pgPoolRouterReader) SelectCovens(ctx context.Context, sid string) (own, inherited []string, err error) {
	// SelectBySID is the only CRUD method that returns a full Soul along with
	// coven[]. The router only needs the labels, but adding SQL dedicated to
	// the router would complicate schema invalidation; the cost of 5 extra
	// fields in the Soul row is negligible next to a separate round trip or a
	// separate SELECT.
	s, err := soul.SelectBySID(ctx, r.db, sid)
	if err != nil {
		return nil, nil, err
	}
	// The second round trip is the shared inherited-labels resolver, not a
	// third mechanism: the RBAC predicate, the topology roster and the reactor
	// subject all read the same one, so they cannot disagree about one host.
	//
	// This is also why the router does NOT call [soul.EffectiveCovens], which
	// every other consumer of the axis uses (NIM-249): that collapses the two
	// halves into one set, and the Level 2 tiebreak below needs them apart to
	// try own tags before inherited ones. Consolidating the two calls "for
	// consistency" would silently change which provider a routed host lands on.
	labels, err := soul.LoadInheritedLabels(ctx, r.db, sid)
	if err != nil {
		return nil, nil, err
	}
	return s.Coven, labels.Covens, nil
}
