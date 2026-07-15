package handlers

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/soulpurview"
)

// VoyageScenarioResolver — resolves a Voyage `kind=scenario` target → a snapshot of
// incarnation NAMES (ADR-043 §4, B1: the batch unit is an incarnation). Out-of-incarnation
// resolve: the set is pinned at start (snapshot scope, parity with Tide
// `target_resolved_souls`).
//
// Intake (any non-empty → OR-merge, then dedup):
//   - explicit incarnations[] (exact-match by name, existence check);
//   - service= / coven= filter → resolved to names (parity with GET /v1/incarnations
//     with a filter, [incarnation.SelectAll] / ListFilter).
//
// An empty result is not an error (the handler decides: 422 voyage_empty_target).
type VoyageScenarioResolver interface {
	ResolveIncarnations(ctx context.Context, filter VoyageScenarioFilter) ([]string, error)
}

// VoyageScenarioFilter — a resolved target for [VoyageScenarioResolver].
// All fields are optional; non-empty ones are OR-merged (like list /v1/incarnations:
// explicit names ∪ filter result). AND semantics would be a narrowing, incompatible
// with the "take these + everything from this environment" use case.
type VoyageScenarioFilter struct {
	Incarnations []string // exact-match names; invalid/nonexistent → error.
	Service      string   // incarnation.service filter (exact).
	Coven        string   // env tag incarnation.covens[] (any-of, ADR-008 amendment a).
}

// VoyageCommandResolver — resolves a Voyage `kind=command` target → a snapshot of SIDs
// (ADR-043 §4: the batch unit is a host). AND-merge sids/coven/where (security
// invariant ADR-040 → ADR-043 §5): an invocation narrows the scope, never widens it.
//
// ResolveSIDsInScope (ADR-047 S4) additionally intersects the resolved target with
// the operator Purview (upper bound, like the list visibility of `GET /v1/souls`):
// the command path must not reveal hosts outside the Archon's scope. Preview and other
// future consumers inherit the intersection via the same resolver.
type VoyageCommandResolver interface {
	ResolveSIDs(ctx context.Context, filter VoyageCommandFilter) ([]string, error)
	ResolveSIDsInScope(ctx context.Context, filter VoyageCommandFilter, scope soulpurview.Scope) (ScopedSIDs, error)
}

// ScopedSIDs — the result of [VoyageCommandResolver.ResolveSIDsInScope]: the resolved
// target ∩ operator Purview + explicitly-named foreign hosts (ADR-047 S4).
//
// Hybrid semantics (user's choice 2026-06-09, ADR-047 §S4): the handler decides
// by the fields —
//   - DeniedExplicit non-empty → 403 (an explicit foreign host in sids[] = an escalation
//     attempt, parity with the per-incarnation scope check of the scenario path);
//   - else SIDs empty → 422 voyage_empty_target (a broad target trimmed to zero);
//   - else SIDs → run snapshot (a trimmed subset, without refusal).
type ScopedSIDs struct {
	// SIDs — the resolved target ∩ Purview, sorted/deduplicated (run
	// snapshot). For Unrestricted = the full resolve (backcompat cluster-admin).
	SIDs []string
	// DeniedExplicit — hosts explicitly listed in filter.SIDs that exist
	// (passed SQL-presence) but fell out of the operator Purview. A non-empty list →
	// 403 (anti-escalation): the operator named a specific foreign SID. Sorted.
	DeniedExplicit []string
}

// VoyageCommandFilter — a resolved target for [VoyageCommandResolver]
// (kind=command). All fields are optional; AND-merge on the resolver side
// (security invariant ADR-040: an invocation narrows the scope, never widens it). where —
// for now stored in target_origin, NOT evaluated in the MVP.
type VoyageCommandFilter struct {
	SIDs   []string
	Covens []string
	Where  string
	// RequireAlive — presence filter (ADR-043 amendment §5): when true the resolve
	// additionally drops Souls without a live presence-lease (SoulPresence).
	// The post-filter snapshot is pinned in target_resolved as usual (the snapshot does
	// not "jitter" — the filter is at resolve time). Applied on top of SQL-presence (status IN
	// connected/dormant).
	RequireAlive bool
}

// --- production implementations over pgxpool.Pool ---

// voyageResolverDB — a narrow surface over PG for the production resolvers.
// A real *pgxpool.Pool satisfies it.
type voyageResolverDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// VoyageScenarioPGResolver — production implementation of [VoyageScenarioResolver].
type VoyageScenarioPGResolver struct {
	db voyageResolverDB
}

// NewVoyageScenarioPGResolver constructs the resolver. db is required.
func NewVoyageScenarioPGResolver(db voyageResolverDB) *VoyageScenarioPGResolver {
	return &VoyageScenarioPGResolver{db: db}
}

// ResolveIncarnations resolves a target → a sorted, deduplicated slice of
// incarnation names (deterministic for audit/snapshot).
//
// Algorithm:
//  1. Explicit incarnations[] — each is checked via [incarnation.SelectByName]
//     (nonexistent → ErrIncarnationNotFound, the handler maps to 422:
//     a run cannot start on a missing target).
//  2. service= / coven= filter → [incarnation.SelectAll] with ListFilter (the same
//     resolve as GET /v1/incarnations). Drains all pages via an unbounded
//     limit loop over offset.
//  3. Union (set), sort.
//
// An empty result is not an error.
func (r *VoyageScenarioPGResolver) ResolveIncarnations(ctx context.Context, filter VoyageScenarioFilter) ([]string, error) {
	set := make(map[string]struct{})

	for _, name := range filter.Incarnations {
		if !incarnation.ValidName(name) {
			return nil, fmt.Errorf("voyage resolver: invalid incarnation name %q", name)
		}
		if _, err := incarnation.SelectByName(ctx, r.db, name); err != nil {
			if errors.Is(err, incarnation.ErrIncarnationNotFound) {
				return nil, fmt.Errorf("%w: %s", incarnation.ErrIncarnationNotFound, name)
			}
			return nil, fmt.Errorf("voyage resolver: select incarnation %q: %w", name, err)
		}
		set[name] = struct{}{}
	}

	if filter.Service != "" || filter.Coven != "" {
		lf := incarnation.ListFilter{Service: filter.Service, Coven: filter.Coven}
		const pageSize = 1000
		for offset := 0; ; offset += pageSize {
			// scope Unrestricted: voyage-target resolve operates over the full
			// set of incarnations (RBAC is checked at the voyage-permission
			// level, not by the scoped List visibility). Behavior unchanged.
			items, total, err := incarnation.SelectAll(ctx, r.db, lf, incarnation.ListScope{Unrestricted: true}, offset, pageSize)
			if err != nil {
				return nil, fmt.Errorf("voyage resolver: list incarnations: %w", err)
			}
			for _, inc := range items {
				set[inc.Name] = struct{}{}
			}
			if offset+pageSize >= total || len(items) == 0 {
				break
			}
		}
	}

	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// VoyageCommandPGResolver — production implementation of [VoyageCommandResolver] over
// the `souls` table (cluster-wide resolve target → SID[] snapshot, parity with
// [ErrandRunSoulsPGResolver]). AND-merge sids/coven; where in the MVP is stored
// in target_origin, not evaluated (the CEL evaluator is a separate slice).
//
// presence — an optional presence-lease checker (ADR-006(a), [SoulPresence]):
// needed only when filter.RequireAlive=true. nil → require_alive degrades to
// SQL-presence (status IN connected/dormant), without a separate lease check
// (single-instance dev without Redis), symmetric to the topology resolver and `GET /v1/souls`.
type VoyageCommandPGResolver struct {
	db       voyageResolverDB
	presence SoulPresence
}

// NewVoyageCommandPGResolver constructs the resolver without a presence checker
// (require_alive degrades to SQL-presence). db is required.
func NewVoyageCommandPGResolver(db voyageResolverDB) *VoyageCommandPGResolver {
	return &VoyageCommandPGResolver{db: db}
}

// NewVoyageCommandPGResolverWithPresence constructs the resolver with a presence-lease
// checker (ADR-043 amendment §5): when filter.RequireAlive=true the resolve drops
// Souls without a live presence-lease. db is required; presence nil → degradation to
// SQL-presence (like [NewVoyageCommandPGResolver]).
func NewVoyageCommandPGResolverWithPresence(db voyageResolverDB, presence SoulPresence) *VoyageCommandPGResolver {
	return &VoyageCommandPGResolver{db: db, presence: presence}
}

// ResolveSIDs resolves a target → a sorted slice of SIDs. AND-merge: all non-empty
// filters intersect (security invariant: narrowing, not widening). When
// filter.RequireAlive — additionally a presence filter for live hosts via
// [SoulPresence] (ADR-043 amendment §5).
//
// Cluster-wide (without Purview intersection): used by keeper-side consumers for whom
// the operator scope boundary does not apply (e.g. Cadence spawn, where RBAC is
// checked at the recipe level). The HTTP path createCommand calls [ResolveSIDsInScope].
func (r *VoyageCommandPGResolver) ResolveSIDs(ctx context.Context, filter VoyageCommandFilter) ([]string, error) {
	pairs, err := r.resolvePairs(ctx, filter)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(pairs))
	for i := range pairs {
		out[i] = pairs[i].sid
	}
	return out, nil
}

// ResolveSIDsInScope resolves target ∩ operator Purview (ADR-047 S4). The same
// AND-merge sids/coven + require_alive as [ResolveSIDs], then intersection with
// scope (the upper bound of operator visibility):
//   - Unrestricted → full resolve (backcompat cluster-admin, SIDs field without
//     trimming, DeniedExplicit empty);
//   - Empty (fail-closed: the operator is entitled to no host) → SIDs empty,
//     DeniedExplicit = all explicitly-named existing SIDs (→ 403 if they were
//     named; otherwise 422 on empty SIDs);
//   - coven/regex scope → host visibility = covenMatch OR regexMatch
//     ([soulpurview.CompiledScope.Visible]); the resolved set is trimmed to the
//     visible one. An explicitly-named (filter.SIDs) invisible host goes into
//     DeniedExplicit (anti-escalation), a broad one (coven/where) is silently trimmed.
//
// soulprint/state dimensions (scope.Partial) are NOT computed (S3b-2b deferred) —
// under-display (fail-closed: the operator would rather miss their own host reachable
// ONLY via soulprint than see a foreign one). coven/regex work fully.
//
// A broken/too-long regex in Purview ([soulpurview.CompileScope] error) →
// fail-closed: empty set + all explicit SIDs in DeniedExplicit (hide, not 500).
func (r *VoyageCommandPGResolver) ResolveSIDsInScope(ctx context.Context, filter VoyageCommandFilter, scope soulpurview.Scope) (ScopedSIDs, error) {
	pairs, err := r.resolvePairs(ctx, filter)
	if err != nil {
		return ScopedSIDs{}, err
	}

	if scope.Unrestricted {
		out := make([]string, len(pairs))
		for i := range pairs {
			out[i] = pairs[i].sid
		}
		return ScopedSIDs{SIDs: out}, nil
	}

	explicit := make(map[string]struct{}, len(filter.SIDs))
	for _, sid := range filter.SIDs {
		explicit[sid] = struct{}{}
	}

	// Empty (fail-closed) or a scope-eval error → no visible host. Explicitly-named
	// existing SIDs become DeniedExplicit (handler → 403).
	if scope.Empty {
		return scopedFromPairs(pairs, explicit, func(string, []string) bool { return false }), nil
	}
	compiled, cerr := soulpurview.CompileScope(scope)
	if cerr != nil {
		return scopedFromPairs(pairs, explicit, func(string, []string) bool { return false }), nil
	}
	return scopedFromPairs(pairs, explicit, compiled.Visible), nil
}

// scopedFromPairs splits the resolved (sid, covens) pairs by the visibility predicate
// visible: visible → SIDs (sorted — pairs are already ORDER BY sid ASC), invisible
// and explicitly-named (∈ explicit) → DeniedExplicit. Invisible broad ones (coven/where,
// ∉ explicit) are silently dropped (trimming without refusal, ADR-047 S4 branch 2).
func scopedFromPairs(pairs []soulPair, explicit map[string]struct{}, visible func(sid string, covens []string) bool) ScopedSIDs {
	var res ScopedSIDs
	for i := range pairs {
		p := &pairs[i]
		if visible(p.sid, p.covens) {
			res.SIDs = append(res.SIDs, p.sid)
			continue
		}
		if _, ok := explicit[p.sid]; ok {
			res.DeniedExplicit = append(res.DeniedExplicit, p.sid)
		}
	}
	return res
}

// soulPair — (sid, covens) of a single host for scope-eval. covens are needed only in
// the regex/coven mode [soulpurview.CompiledScope.Visible].
type soulPair struct {
	sid    string
	covens []string
}

// resolvePairs — the shared resolve target → sorted (sid, covens) pairs
// (AND-merge sids/coven + require_alive). The base for both cluster-wide [ResolveSIDs]
// (drops covens) and [ResolveSIDsInScope] (scope-eval by covens).
func (r *VoyageCommandPGResolver) resolvePairs(ctx context.Context, filter VoyageCommandFilter) ([]soulPair, error) {
	for _, sid := range filter.SIDs {
		if !soul.ValidSID(sid) {
			return nil, fmt.Errorf("voyage resolver: invalid SID %q", sid)
		}
	}
	for _, c := range filter.Covens {
		if !soul.ValidCoven(c) {
			return nil, fmt.Errorf("voyage resolver: invalid coven label %q", c)
		}
	}

	const baseSQL = `SELECT sid, coven FROM souls
WHERE status IN ('connected','dormant')
  AND ($1::text[] IS NULL OR cardinality($1::text[]) = 0 OR sid = ANY($1::text[]))
  AND ($2::text[] IS NULL OR cardinality($2::text[]) = 0 OR coven @> $2::text[])
ORDER BY sid ASC
`
	var sidsArg, covensArg any
	if len(filter.SIDs) > 0 {
		sidsArg = filter.SIDs
	}
	if len(filter.Covens) > 0 {
		covensArg = filter.Covens
	}

	rows, err := r.db.Query(ctx, baseSQL, sidsArg, covensArg)
	if err != nil {
		return nil, fmt.Errorf("voyage resolver: query souls: %w", err)
	}
	defer rows.Close()
	var out []soulPair
	for rows.Next() {
		var sid string
		var covens []string
		if err := rows.Scan(&sid, &covens); err != nil {
			return nil, fmt.Errorf("voyage resolver: scan: %w", err)
		}
		out = append(out, soulPair{sid: sid, covens: covens})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("voyage resolver: iter: %w", err)
	}

	// require_alive (ADR-043 amendment §5): drop hosts without a live presence-lease.
	// presence=nil → degradation to the already-applied SQL-presence (status IN
	// connected/dormant) — without extra filtering. SID order is preserved
	// (a filtered subset of the sorted slice).
	if filter.RequireAlive && r.presence != nil && len(out) > 0 {
		sids := make([]string, len(out))
		for i := range out {
			sids[i] = out[i].sid
		}
		alive, perr := r.presence.SoulsStreamAlive(ctx, sids)
		if perr != nil {
			return nil, fmt.Errorf("voyage resolver: presence-фильтр (require_alive): %w", perr)
		}
		filtered := out[:0]
		for i := range out {
			if _, ok := alive[out[i].sid]; ok {
				filtered = append(filtered, out[i])
			}
		}
		out = filtered
	}

	return out, nil
}
