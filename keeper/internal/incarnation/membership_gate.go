package incarnation

// Screening of the hosts an OPERATOR bind targets (ADR-008 amendment 2026-07-28,
// NIM-209). Lives in the domain, not in a handler, because BOTH operator surfaces
// go through it — REST `POST /v1/incarnations/{name}/members` and the MCP mirror
// `keeper.incarnation.bind-member`. MCP has no chi middleware, so a check that
// existed only on the REST side would be a hole rather than a gate.
//
// The keeper-internal bind act (`core.soul.registered` inside a scenario run) does
// NOT screen: it binds hosts it has itself just created, still `pending`, and it
// acts as the keeper rather than as an operator.

import (
	"context"
	"errors"
	"sort"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/soulpurview"
)

// isSoulNotFound isolates the registry's not-found sentinel so the membership
// paths read as "the host is gone" rather than as error plumbing.
func isSoulNotFound(err error) bool { return errors.Is(err, soul.ErrSoulNotFound) }

// BindRejection lists why a bind may not proceed, one bucket per reason, each
// sorted. The buckets are filled in one pass and reported together so the
// operator fixes everything at once instead of discovering the next problem on
// the next call.
//
// ALL-OR-NOTHING: any non-empty bucket rejects the WHOLE request. Binding the
// acceptable subset and staying quiet about the rest would read as "roster
// complete" while leaving it short a host — the run would then fail further away,
// with a message about something else.
type BindRejection struct {
	// UnknownSIDs — not in the soul registry at all (FK `sid → souls` would
	// reject them anyway; catching it here yields a 422 that names them instead
	// of a constraint violation).
	UnknownSIDs []string
	// OutOfScope — outside the caller's soul visibility. THE security bucket:
	// without it a holder of `bind-member on incarnation=X` could pull any host
	// into X and reach it through `incarnation.run`.
	OutOfScope []string
	// NotConnected — known and visible, but not `connected`; entries carry the
	// actual status in parentheses ("web1.example.com (disconnected)").
	NotConnected []string
}

// Empty reports whether the screening found nothing to reject.
func (r *BindRejection) Empty() bool {
	return r == nil || (len(r.UnknownSIDs) == 0 && len(r.OutOfScope) == 0 && len(r.NotConnected) == 0)
}

// ScreenBindCandidates checks the SIDs an operator wants to bind: they must
// exist, be inside the operator's soul scope, and be `connected`. It returns nil
// when every SID passes.
//
// Bucket order matters for what the caller reports first: unknown → out-of-scope
// → not-connected. An operator who may not see a host is told "forbidden", never
// "that host is disconnected" — the scope boundary must not leak the state of
// hosts behind it.
func ScreenBindCandidates(ctx context.Context, db ExecQueryRower, sids []string, scope soulpurview.Scope) (*BindRejection, error) {
	if len(sids) == 0 {
		return nil, nil
	}
	hosts, err := soul.SelectBySIDs(ctx, db, sids)
	if err != nil {
		return nil, err
	}
	bySID := make(map[string]*soul.Soul, len(hosts))
	for _, s := range hosts {
		bySID[s.SID] = s
	}

	rej := &BindRejection{}
	for _, sid := range sids {
		s, known := bySID[sid]
		if !known {
			rej.UnknownSIDs = append(rej.UnknownSIDs, sid)
			continue
		}
		if !hostInScope(scope, s) {
			rej.OutOfScope = append(rej.OutOfScope, sid)
			continue
		}
		if s.Status != soul.StatusConnected {
			rej.NotConnected = append(rej.NotConnected, sid+" ("+string(s.Status)+")")
		}
	}
	sort.Strings(rej.UnknownSIDs)
	sort.Strings(rej.OutOfScope)
	sort.Strings(rej.NotConnected)

	if rej.Empty() {
		return nil, nil
	}
	return rej, nil
}

// HostInScope reports whether ONE host is inside the operator's soul scope,
// resolving it from the registry. A host absent from the registry is reported as
// (false, false): the caller decides what that means — on the unbind path it is
// an idempotent no-op, since FK `sid → souls ON DELETE CASCADE` has already taken
// its memberships with it.
func HostInScope(ctx context.Context, db ExecQueryRower, sid string, scope soulpurview.Scope) (inScope, known bool, err error) {
	s, err := soul.SelectBySID(ctx, db, sid)
	if err != nil {
		if isSoulNotFound(err) {
			return false, false, nil
		}
		return false, false, err
	}
	return hostInScope(scope, s), true, nil
}

// hostInScope judges ONE host on the labels it carries ITSELF — `souls.coven` and
// `souls.traits`, nothing else (NIM-281). This is the same answer the souls list
// pushes down in SQL and the same one the single-object read gate gives, so an
// operator cannot be shown a host in the list and refused the bind on it.
//
// A bind attaches no label to the host: membership is a relation, not a tag. That
// is what makes the screening safe to run before the write — there is no way for
// binding to mint the label that would authorize the bind.
func hostInScope(scope soulpurview.Scope, s *soul.Soul) bool {
	return soulpurview.InScope(scope, s.SID, s.Coven, soulpurview.TraitsInput(s.Traits))
}
