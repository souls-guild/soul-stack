// Package soulpurview translates an operator's [rbac.Purview] into souls
// visibility (ADR-047, NIM-128 boolean scope). Two consumers:
//
//   - list (`GET /v1/souls`, `/stats`) — [Scope.WhereSQL] renders the purview
//     into a parameterized SQL boolean pushed into the souls query. Every
//     dimension is evaluated in the database, so offset pagination and total
//     stay exact (no Go post-filter, no keyset window).
//   - single-object reads (`GET /v1/souls/{sid}`, `/soulprint`, `/history`) —
//     [InScope] checks one Soul against the purview boundary in Go.
//
// Souls carry three scope dimensions: coven (TEXT[] `coven`), host (`sid`) and
// trait (jsonb `traits`). They have no service/incarnation column, so a
// condition on those dimensions renders FALSE (fail-closed).
//
// fail-closed (ADR-047): uncertainty means "hide", NOT "show the whole fleet".
// This is OPPOSITE of the presence-overlay (`GET /v1/souls` on a Redis error
// fails SAFE by returning the PG snapshot): scope hides on doubt, presence
// shows on doubt. These two layers MUST NOT borrow strategy from each other.
package soulpurview

import (
	"fmt"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

// Columns maps the souls table onto scope dimensions for [rbac.PurviewSQL].
// A Soul carries coven / host / trait; it has no service or incarnation column,
// so those stay empty — any condition on them renders FALSE (fail-closed).
var Columns = rbac.ScopeColumns{
	Coven:  "coven",
	Host:   "sid",
	Traits: "traits",
}

// Scope is a thin wrapper over the operator's [rbac.Purview] for souls
// visibility. It hides the boolean-scope plumbing behind the two operations the
// souls layer needs: a SQL predicate for the list, and an in-Go membership test
// for single-object reads.
type Scope struct {
	p rbac.Purview
}

// Resolve wraps a resolved [rbac.Purview] into a souls [Scope].
func Resolve(p rbac.Purview) Scope { return Scope{p: p} }

// Unrestricted reports whether the operator has no scope restriction (the whole
// registry is visible).
func (s Scope) Unrestricted() bool { return s.p.Unrestricted }

// Empty reports the fail-closed case: the operator is entitled to NO hosts (no
// access at all). Consumers return an empty list / 404 without touching PG.
func (s Scope) Empty() bool { return s.p.IsEmpty() }

// WhereSQL renders the purview into a parameterized SQL boolean over cols, with
// positional placeholders starting at startIdx. It returns the SQL fragment
// (always atomic and safe to AND into a WHERE), the args in placeholder order,
// and the next free placeholder index.
//
//   - Unrestricted → "TRUE" (no narrowing);
//   - empty / Deny → "FALSE" (fail-closed: zero hosts, NOT the whole registry);
//   - otherwise → the OR of the purview predicates over coven/host/trait.
func (s Scope) WhereSQL(cols rbac.ScopeColumns, startIdx int) (string, []any, int) {
	return rbac.PurviewSQL(s.p, cols, startIdx)
}

// InScope reports whether ONE Soul (its sid, covens and traits) is inside the
// operator's purview boundary — the single-object gate for GET /v1/souls/{sid},
// /soulprint and /history. Fail-closed (symmetric to [Scope.WhereSQL]):
//
//   - Unrestricted → true (any host, including one without covens);
//   - empty / Deny → false (no visible host; the operator has no rights);
//   - otherwise → the host satisfies ANY purview predicate (OR across Exprs,
//     via [rbac.Purview.Match]).
//
// traits is the Soul's traits already projected onto the [rbac.ScopeInput] shape
// (key → []string), or nil (a trait condition then fails closed). Use
// [TraitsInput] to project a Soul's raw jsonb traits.
func InScope(s Scope, sid string, soulCovens []string, traits map[string][]string) bool {
	return s.p.Match(rbac.ScopeInput{
		Covens: soulCovens,
		Hosts:  []string{sid},
		Traits: traits,
	})
}

// TraitsInput projects a Soul's traits (jsonb: key → scalar | list) onto the
// [rbac.ScopeInput] shape (key → []string), stringifying values the way PG's
// `->>` does so the in-Go check matches the SQL pushdown. A nil/empty map yields
// nil (a trait condition then fails closed).
func TraitsInput(traits map[string]any) map[string][]string {
	if len(traits) == 0 {
		return nil
	}
	out := make(map[string][]string, len(traits))
	for k, v := range traits {
		if list, ok := v.([]any); ok {
			vals := make([]string, 0, len(list))
			for _, e := range list {
				vals = append(vals, scalarToString(e))
			}
			out[k] = vals
			continue
		}
		out[k] = []string{scalarToString(v)}
	}
	return out
}

// scalarToString renders a scalar trait value as text (string as-is; other JSON
// scalars via %v, matching PG jsonb `->>`).
func scalarToString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}
