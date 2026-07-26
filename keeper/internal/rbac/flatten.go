package rbac

import (
	"errors"
	"fmt"
	"sort"
)

// Chain resolution for derived roles (ADR-078(c)/(d), NIM-180). A derived role
// stores a reference to its parent plus a delta; this file turns that chain into
// the EFFECTIVE form a permission check reads.
//
// Resolution runs ONCE, when the enforcer snapshot is built — never per request.
// After [flattenRoleGraph] every [Role] in the catalog carries its own effective
// permissions and its own effective scope, so Check / ResolvePurview / HoldsAction
// are unchanged and cost exactly what they cost before derivation existed.
//
// Two independent mechanisms, both narrowing, neither sufficient alone:
//
//	scope ceiling   effective_scope(r) = effective_scope(parent) AND default_scope(r)
//	                The parent's effective scope is a HARD bound on every permission
//	                of the child, including a per-permission `on <expr>` and `*`.
//	                This is what makes the cascade work: move the parent and the
//	                child follows, with no role rewritten by hand.
//
//	set intersection  effective_perms(r) = own_perms(r) ∩ effective_perms(parent)
//	                A child permission the parent does not hold is DROPPED. The
//	                ceiling cannot catch this one: a parent whose narrowing lives in
//	                per-permission scopes has no default_scope to conjoin.
//
// `∩` is [callerHolds] — the same containment predicate the least-privilege check
// uses, reused rather than reimplemented (ADR-078(c): one definition of "⊆").
//
// Everything here is fail-closed. A permission that is not PROVABLY within the
// parent is dropped, not kept; a chain that cannot be resolved (a parent missing
// from the catalog, a depth past the cap, a conjunction whose DNF explodes) fails
// the whole snapshot build rather than yielding a partial enforcer — the ADR-078(f)
// call, matching what an unparseable permission already does.

// ErrRoleScopeTooComplex — the conjunction of a chain's scopes cannot be
// normalized within the size caps of [maxDNFConjuncts]. Refused rather than
// approximated: the subset check works over DNF, and a scope it cannot normalize
// is a scope it cannot bound. Transport maps it to 422.
var ErrRoleScopeTooComplex = errors.New("rbac: role scope too complex to resolve")

// flattenRoleGraph resolves every derived role in a built catalog into its
// effective form, in place. byName is the full catalog keyed by role name; the
// parent graph must already have passed [validateRoleGraph] (the caller runs it
// first, so cycles and dangling parents are rejected before any resolution).
//
// Roles are visited in name order and each resolves its parent first, so the
// result does not depend on map iteration order — the same catalog always yields
// the same enforcer.
func flattenRoleGraph(byName map[string]*Role) error {
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)

	done := make(map[string]struct{}, len(byName))
	for _, name := range names {
		if err := flattenRole(byName, byName[name], done, 1); err != nil {
			return err
		}
	}
	return nil
}

// flattenRole resolves one role, resolving its ancestors first (memoized through
// done, so a shared parent is flattened once no matter how many children it has).
//
// depth is a belt-and-braces bound on the recursion: [validateRoleGraph] has
// already rejected cycles, but a stack overflow is a worse failure mode than an
// error, and the cap costs one comparison per role.
func flattenRole(byName map[string]*Role, r *Role, done map[string]struct{}, depth int) error {
	if _, resolved := done[r.Name]; resolved {
		return nil
	}
	if depth > maxRoleChainDepth {
		return fmt.Errorf("%w: resolving role %q", ErrRoleChainTooDeep, r.Name)
	}
	if r.ParentRole == "" {
		// A plain role IS its own effective form (ADR-078(b): its parent side is
		// the unrestricted top).
		done[r.Name] = struct{}{}
		return nil
	}

	parent, known := byName[r.ParentRole]
	if !known {
		// Unreachable through validateRoleGraph, kept because a child whose
		// ceiling cannot be read must be denied, never treated as unbounded.
		return fmt.Errorf("%w: role %q names parent %q", ErrRoleParentUnknown, r.Name, r.ParentRole)
	}
	if err := flattenRole(byName, parent, done, depth+1); err != nil {
		return err
	}

	att, err := attenuate(parent, r.Permissions, r.DefaultScope)
	if err != nil {
		return fmt.Errorf("rbac: role %q: %w", r.Name, err)
	}
	r.DefaultScope = att.Scope
	r.Permissions = att.Kept
	done[r.Name] = struct{}{}
	return nil
}

// attenuation is the resolved form of a derived role against its (already
// flattened) parent: the effective scope, the permissions the parent covers, and
// the ones it does not.
//
// The enforcer drops Rejected silently — a stored row the parent no longer covers
// simply stops granting, which is the cascade working. The write path reports it
// instead ([ErrRoleExceedsParent]): refusing at write time is a better error than
// storing a row that will never grant anything.
type attenuation struct {
	// Scope is effective_scope(child) = effective_scope(parent) AND own delta.
	Scope *ScopeExpr
	// Kept are the child's permissions in effective form (per-permission scopes
	// already capped by the parent's ceiling), in input order.
	Kept []Permission
	// Rejected are the child's permissions the parent does not cover, in input
	// order, in the same effective form — so an error message names the resolved
	// right the operator actually asked for.
	Rejected []Permission
}

// attenuate resolves a child's own rows against a flattened parent (ADR-078(c)).
// parent must already be in effective form; ownPerms / ownScope are the child's
// stored rows exactly as written.
//
// Scope handling per permission, mirroring ADR-047's default/override split with
// the ceiling layered on top:
//
//   - own `on <expr>` — overrides the child's delta (ADR-047), but is still
//     conjoined with the parent's ceiling: a per-permission scope may narrow the
//     inherited area, never leave it.
//   - `*` — the role's own default_scope does not touch a wildcard (ADR-047(b) #1),
//     but the parent's ceiling does. A `*` derived from a scoped parent is a
//     scoped super-admin, not an unrestricted one.
//   - bare — left bare, inheriting the role's effective scope through
//     [Enforcer.ResolvePurview] exactly as it does on a plain role. Writing the
//     ceiling onto it instead would DROP the child's own delta, which sits in
//     that same effective scope — a widening.
func attenuate(parent *Role, ownPerms []Permission, ownScope *ScopeExpr) (attenuation, error) {
	ceiling := parent.DefaultScope
	out := attenuation{Scope: andScopes(ceiling, ownScope)}
	if err := checkScopeResolvable(out.Scope); err != nil {
		return attenuation{}, err
	}

	// The parent's rows in the form a containment check can read: its bare
	// permissions carry its effective scope (ADR-047 S1), its `*` stays as it is.
	parentEff := effectivePermissions(parent.Permissions, parent.DefaultScope)

	for _, p := range ownPerms {
		switch {
		case p.Scope != nil:
			p.Scope = andScopes(ceiling, p.Scope)
		case p.IsWildcard:
			p.Scope = ceiling
		}
		if err := checkScopeResolvable(p.Scope); err != nil {
			return attenuation{}, err
		}
		// What the child would actually grant, with a bare permission resolved
		// under the role's effective scope — the same expansion ResolvePurview
		// performs at check time.
		req := p
		if !req.IsWildcard && req.Scope == nil {
			req.Scope = out.Scope
		}
		if callerHolds(parentEff, req) {
			out.Kept = append(out.Kept, p)
			continue
		}
		out.Rejected = append(out.Rejected, req)
	}
	return out, nil
}

// checkScopeResolvable rejects a conjoined scope whose DNF exceeds the size caps.
// The containment predicate normalizes to DNF, so a scope past the cap is one
// nothing downstream can bound — fail-closed rather than approximated. nil (the
// unrestricted top) is always fine.
func checkScopeResolvable(e *ScopeExpr) error {
	if e == nil {
		return nil
	}
	if _, err := toDNF(e); err != nil {
		return fmt.Errorf("%w: %w", ErrRoleScopeTooComplex, err)
	}
	return nil
}

// andScopes conjoins two scope predicates. A nil operand is the unrestricted top,
// so `nil AND x` is x — which is what makes one formula cover both a plain role
// (no parent, no ceiling) and a derived one (ADR-078(b)).
//
// Conjunction is the whole safety argument: the grammar has no NOT, so adding a
// term can only ever narrow. Nested AND nodes are flattened and duplicate terms
// dropped, so a child that restates its parent's predicate produces
// `coven=dba`, not `coven=dba AND coven=dba` — same meaning, smaller DNF, stable
// canonical string.
func andScopes(outer, inner *ScopeExpr) *ScopeExpr {
	if outer == nil {
		return inner
	}
	if inner == nil {
		return outer
	}
	terms := appendConjuncts(appendConjuncts(nil, outer), inner)

	seen := make(map[string]struct{}, len(terms))
	uniq := terms[:0]
	for _, t := range terms {
		key := t.String()
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		uniq = append(uniq, t)
	}
	if len(uniq) == 1 {
		return uniq[0]
	}
	return &ScopeExpr{Op: OpAnd, Children: uniq}
}

// appendConjuncts appends e's top-level AND terms to dst (e itself when it is not
// an AND node), so nesting never grows with the chain.
func appendConjuncts(dst []*ScopeExpr, e *ScopeExpr) []*ScopeExpr {
	if e.Op == OpAnd {
		return append(dst, e.Children...)
	}
	return append(dst, e)
}
