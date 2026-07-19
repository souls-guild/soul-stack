package rbac

import "sort"

// Purview is the typed result of [Enforcer.ResolvePurview]: the upper bound of
// an operator's scope-visibility/targeting for a given (resource, action)
// (ADR-047, NIM-128 boolean scope).
//
// With the NIM-128 boolean scope, a Purview no longer decomposes into a flat
// union of per-dimension value lists. Instead it carries the operator's scope
// PREDICATES for (resource, action): the scope boundary is the OR of Exprs — a
// node is in scope if it satisfies ANY of them. Terminal flags:
//   - Unrestricted=true — no scope restriction (bare permission, `*`, or a
//     matching permission whose role has no default_scope). Exprs is ignored.
//   - Deny=true — access denied (revoked operator). Exprs ignored.
//   - otherwise — Exprs is the OR-set; empty Exprs (and not Unrestricted) means
//     no access (fail-closed).
type Purview struct {
	// Unrestricted — the operator has no scope restriction for (resource, action).
	Unrestricted bool

	// Deny — access denied by scope (revoked operator). Placeholder for future
	// explicit-deny dimensions; set on the revoked shortcut.
	Deny bool

	// Exprs — boolean scope predicates for (resource, action); the boundary is
	// their OR. Deduped by canonical string, order-stable (sorted). Empty when
	// Unrestricted or Deny, or when the operator holds no matching permission.
	Exprs []*ScopeExpr
}

// Holds reports whether the operator holds (resource, action) in ANY scope —
// the existence gate for read endpoints (ADR-047 §d). Deny → false; otherwise
// Unrestricted or a non-empty Exprs → true.
func (p Purview) Holds() bool {
	if p.Deny {
		return false
	}
	return p.Unrestricted || len(p.Exprs) > 0
}

// IsEmpty reports the fail-closed case: no access at all (not Unrestricted, not
// Deny, no predicates). Consumers treat this as "hide everything".
func (p Purview) IsEmpty() bool {
	return !p.Unrestricted && !p.Deny && len(p.Exprs) == 0
}

// Match reports whether a node (described by a [ScopeInput]) is within the
// operator's Purview boundary. Unrestricted → always true; Deny/empty → false;
// otherwise true iff ANY predicate matches (OR across Exprs). This is the
// single in-Go scope-membership check used by the unified resolver where SQL
// pushdown is not applicable.
func (p Purview) Match(in ScopeInput) bool {
	if p.Deny {
		return false
	}
	if p.Unrestricted {
		return true
	}
	for _, e := range p.Exprs {
		if evalScope(e, in) {
			return true
		}
	}
	return false
}

// ResolvePurview resolves an operator's [Purview] for (resource, action) — the
// allowed scope, as an OR of boolean predicates (ADR-047 S1 + NIM-128).
//
// For each matching (resource, action) permission, the effective scope is:
//   - a `*` permission → ALWAYS Unrestricted (cluster-admin, ADR-047(b) #1);
//   - a per-perm scope (`on <expr>`) → FULLY OVERRIDES the role default_scope;
//   - a bare permission (Scope==nil):
//     · role WITHOUT default_scope → Unrestricted (backcompat, #2);
//     · role WITH default_scope → INHERITS it.
//
// Union across roles: if ANY matching permission yields Unrestricted → the
// result is Unrestricted; otherwise Exprs is the deduped set of effective
// predicates (their OR is the boundary).
func (e *Enforcer) ResolvePurview(aid, resource, action string) Purview {
	// Revoked shortcut (ADR-047 G1, mirrors [Enforcer.Check]).
	if _, ok := e.revoked[aid]; ok {
		return Purview{Deny: true}
	}
	roles, ok := e.rolesByAID[aid]
	if !ok {
		return Purview{}
	}
	seen := make(map[string]struct{})
	var exprs []*ScopeExpr
	for _, role := range roles {
		for _, p := range role.Permissions {
			if p.IsWildcard {
				// Bare `*` → unrestricted (cluster-admin). A scoped `* on <expr>`
				// (NIM-128) applies its scope to EVERY (resource, action), so it
				// contributes its predicate here like a matching permission —
				// it does NOT lift the operator to unrestricted. Role
				// default_scope does not touch `*` (ADR-047(b) #1).
				if p.Scope == nil {
					return Purview{Unrestricted: true}
				}
				key := p.Scope.String()
				if _, dup := seen[key]; !dup {
					seen[key] = struct{}{}
					exprs = append(exprs, p.Scope)
				}
				continue
			}
			if p.Resource != resource {
				continue
			}
			if p.Action != "*" && p.Action != action {
				continue
			}
			// Effective scope: per-perm overrides default_scope; bare inherits
			// the role's default_scope (nil → unrestricted).
			eff := p.Scope
			if eff == nil {
				eff = role.DefaultScope
			}
			if eff == nil {
				return Purview{Unrestricted: true}
			}
			key := eff.String()
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			exprs = append(exprs, eff)
		}
	}
	if len(exprs) == 0 {
		return Purview{}
	}
	sort.Slice(exprs, func(i, j int) bool { return exprs[i].String() < exprs[j].String() })
	return Purview{Exprs: exprs}
}

// sortedKeys returns a sorted slice of a set's keys (nil for an empty set).
func sortedKeys(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
