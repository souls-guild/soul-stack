package rbac

import (
	"context"
	"fmt"
)

// ErrPermissionNotHeld signals a least-privilege violation (subset check):
// the caller is trying to grant, via a role, a permission it does NOT hold
// itself (create a role with that permission / add it to a role / bind a
// role that has it).
//
// Guards against vertical escalation: an operator with `role.create` +
// `role.grant-operator` but without `*` must not be able to create a role
// with `permissions: ["*"]` and become an effective cluster-admin. Invariant:
// "you cannot grant a permission you don't hold yourself" (rbac.md → §
// least-privilege invariant).
var ErrPermissionNotHeld = fmt.Errorf("rbac: caller may not grant a permission it does not hold (least-privilege)")

// selectAIDPermissionsSQL selects the active operator's permission strings
// (revoked_at IS NULL) across all their roles, TOGETHER WITH each role's
// default_scope (ADR-047 S1): the caller's bare permission inherits its
// role's scope; otherwise least-privilege would compare raw values and a
// bare perm with scope=prod would falsely cover any coven (privilege
// escalation).
//
// Synod (ADR-049(f)): the caller's effective rights are direct ∪ via Synod.
const selectAIDPermissionsSQL = `
SELECT rp.permission, r.default_scope
FROM rbac_role_permissions rp
JOIN rbac_role_operators ro ON ro.role_name = rp.role_name
JOIN rbac_roles r ON r.name = rp.role_name
JOIN operators o ON o.aid = ro.aid
WHERE ro.aid = $1 AND o.revoked_at IS NULL
UNION
SELECT rp.permission, r.default_scope
FROM synod_operators so
JOIN synod_roles sr ON sr.synod_name = so.synod_name
JOIN rbac_role_permissions rp ON rp.role_name = sr.role_name
JOIN rbac_roles r ON r.name = sr.role_name
JOIN operators o ON o.aid = so.aid
WHERE so.aid = $1 AND o.revoked_at IS NULL
`

// assertCallerMayGrant is the least-privilege gate for Service mutations: it
// checks that the caller may grant EVERY permission in required (see
// [assertCallerCovers]). Called INSIDE the mutation tx.
func (s *Service) assertCallerMayGrant(ctx context.Context, db ExecQueryRower, callerAID string, required []Permission) error {
	if len(required) == 0 {
		return nil
	}
	if callerAID == "" {
		return fmt.Errorf("%w: missing caller", ErrPermissionNotHeld)
	}
	callerPerms, err := callerPermissions(ctx, db, callerAID)
	if err != nil {
		return err
	}
	return assertCallerCovers(callerPerms, required)
}

// requiredPermissions expands the granted permission strings into EFFECTIVE
// permissions under the role's default_scope (ADR-047 S1): a bare permission
// on the role being granted inherits its scope; otherwise least-privilege
// would compare raw values.
//
// rawScope is the RAW default_scope of the role being granted (nil = NULL =
// role has no scope, bare stays unrestricted).
func requiredPermissions(rawPerms []string, rawScope *string) ([]Permission, error) {
	if len(rawPerms) == 0 {
		return nil, nil
	}
	var scope *ScopeExpr
	if rawScope != nil {
		var err error
		scope, err = ParseDefaultScope(*rawScope)
		if err != nil {
			return nil, fmt.Errorf("rbac: granted role default_scope %q: %w", *rawScope, err)
		}
	}
	perms := make([]Permission, 0, len(rawPerms))
	for _, raw := range rawPerms {
		p, err := ParsePermission(raw)
		if err != nil {
			return nil, fmt.Errorf("rbac: invalid permission %q: %w", raw, err)
		}
		perms = append(perms, p)
	}
	return effectivePermissions(perms, scope), nil
}

// addedPermissions returns the permissions in newPerms that aren't in
// oldPerms (the set being added). UpdateRolePermissions restricts
// least-privilege to just these: removing permissions isn't escalation.
func addedPermissions(oldPerms, newPerms []string) []string {
	old := make(map[string]struct{}, len(oldPerms))
	for _, p := range oldPerms {
		old[p] = struct{}{}
	}
	seen := make(map[string]struct{}, len(newPerms))
	var added []string
	for _, p := range newPerms {
		if _, inOld := old[p]; inOld {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		added = append(added, p)
	}
	return added
}

// callerPermissions reads the caller's EFFECTIVE permissions: each string is
// parsed via [ParsePermission], then a bare permission (Scope==nil) inherits
// its role's default_scope via [effectivePermissions] (ADR-047 S1).
func callerPermissions(ctx context.Context, db ExecQueryRower, callerAID string) ([]Permission, error) {
	rows, err := db.Query(ctx, selectAIDPermissionsSQL, callerAID)
	if err != nil {
		return nil, fmt.Errorf("rbac: read caller permissions: %w", wrapPgErr(err))
	}
	defer rows.Close()
	var out []Permission
	for rows.Next() {
		var raw string
		var rawScope *string
		if err := rows.Scan(&raw, &rawScope); err != nil {
			return nil, fmt.Errorf("rbac: scan caller permission: %w", err)
		}
		p, err := ParsePermission(raw)
		if err != nil {
			return nil, fmt.Errorf("rbac: caller permission %q: %w", raw, err)
		}
		var scope *ScopeExpr
		if rawScope != nil {
			scope, err = ParseDefaultScope(*rawScope)
			if err != nil {
				return nil, fmt.Errorf("rbac: caller role default_scope %q: %w", *rawScope, err)
			}
		}
		if !p.IsWildcard && p.Scope == nil && scope != nil {
			p.Scope = scope
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rbac: iter caller permissions: %w", err)
	}
	return out, nil
}

// effectivePermissions expands bare permissions (Scope==nil) under a role's
// default_scope (ADR-047 S1). scope==nil → bare stays unrestricted. `*` and
// per-permission scopes are left untouched. Returns a new slice.
func effectivePermissions(perms []Permission, scope *ScopeExpr) []Permission {
	if scope == nil {
		return perms
	}
	out := make([]Permission, len(perms))
	for i, p := range perms {
		if !p.IsWildcard && p.Scope == nil {
			p.Scope = scope
		}
		out[i] = p
	}
	return out
}

// assertCallerCovers checks that EVERY required permission is covered by the
// caller's effective set (least-privilege subset check, NIM-128 boolean
// containment). Any uncovered one → [ErrPermissionNotHeld].
func assertCallerCovers(callerPerms, required []Permission) error {
	for _, req := range required {
		if !callerHolds(callerPerms, req) {
			return fmt.Errorf("%w: %s", ErrPermissionNotHeld, permString(req))
		}
	}
	return nil
}

// permString is a diagnostic rendering of a permission for subset error
// messages.
func permString(p Permission) string {
	if p.IsWildcard {
		return "*"
	}
	s := p.Resource + "." + p.Action
	if p.Scope != nil {
		s += " on " + p.Scope.String()
	}
	return s
}

// callerHolds reports whether the caller's effective set covers granting req
// (NIM-128 boolean least-privilege containment, fail-closed).
//
//   - required `*`: covered ONLY by the caller's own `*`.
//   - required bare (Scope==nil): covered only if the caller is UNRESTRICTED
//     on (resource, action) — has `*` or a bare permission there.
//   - required scoped: normalize the required scope to DNF; EVERY disjunct must
//     be covered ([disjunctCovered]). If not provably covered → deny.
func callerHolds(callerPerms []Permission, req Permission) bool {
	if req.IsWildcard {
		// The caller's BARE `*` covers any wildcard grant (full or scoped).
		for _, cp := range callerPerms {
			if cp.IsWildcard && cp.Scope == nil {
				return true
			}
		}
		// A bare `*` grant needs the caller's own bare `*` (a scoped caller
		// cannot mint a full cluster-admin).
		if req.Scope == nil {
			return false
		}
		// A scoped `* on X` grant is covered iff the caller holds `* on X'`
		// with X ⊆ X' — least-privilege over the wildcard scope (NIM-128).
		callerWild := callerWildcardConjuncts(callerPerms)
		if len(callerWild) == 0 {
			return false
		}
		dnf, err := toDNF(req.Scope)
		if err != nil {
			return false
		}
		for _, ri := range dnf {
			if !disjunctCovered(ri, callerWild) {
				return false
			}
		}
		return true
	}
	if req.Scope == nil {
		return callerUnrestrictedOn(callerPerms, req.Resource, req.Action)
	}
	dnf, err := toDNF(req.Scope)
	if err != nil {
		return false // too complex → fail-closed
	}
	callerConjs := callerScopeConjuncts(callerPerms, req.Resource, req.Action)
	if callerConjs == nil {
		// A nil sentinel means the caller is unrestricted on (resource, action)
		// (has `*` or a bare permission) — it covers every disjunct.
		return true
	}
	for _, ri := range dnf {
		if !disjunctCovered(ri, callerConjs) {
			return false
		}
	}
	return true
}

// callerUnrestrictedOn reports whether the caller has `*` or a bare permission
// (Scope==nil) for (resource, action) — i.e. no scope restriction there.
func callerUnrestrictedOn(callerPerms []Permission, resource, action string) bool {
	for _, cp := range callerPerms {
		// Only a BARE `*` is unrestricted; a scoped `* on X` is bounded and
		// does NOT let the caller grant an unrestricted permission (NIM-128).
		if cp.IsWildcard && cp.Scope == nil {
			return true
		}
		if cp.IsWildcard {
			continue
		}
		if cp.Resource != resource {
			continue
		}
		if cp.Action != "*" && cp.Action != action {
			continue
		}
		if cp.Scope == nil {
			return true
		}
	}
	return false
}

// callerScopeConjuncts returns the caller's scope predicates for (resource,
// action) as a flat list of DNF conjuncts (the OR-set the caller may grant
// within). Returns nil (a sentinel) when the caller is UNRESTRICTED on
// (resource, action) — `*` or a bare permission — meaning "covers everything".
// An empty non-nil slice means the caller holds only differently-scoped or
// no permissions on (resource, action) — covers nothing.
func callerScopeConjuncts(callerPerms []Permission, resource, action string) [][]*ScopeCond {
	var conjs [][]*ScopeCond
	for _, cp := range callerPerms {
		if cp.IsWildcard {
			if cp.Scope == nil {
				return nil // bare `*` → unrestricted
			}
			// A scoped `* on X` covers EVERY (resource, action) within X, so
			// its scope contributes to the caller's coverage here too (NIM-128).
			dnf, err := toDNF(cp.Scope)
			if err != nil {
				continue
			}
			conjs = append(conjs, dnf...)
			continue
		}
		if cp.Resource != resource {
			continue
		}
		if cp.Action != "*" && cp.Action != action {
			continue
		}
		if cp.Scope == nil {
			return nil // bare → unrestricted
		}
		dnf, err := toDNF(cp.Scope)
		if err != nil {
			continue // caller scope too complex → contributes nothing (fail-closed)
		}
		conjs = append(conjs, dnf...)
	}
	if conjs == nil {
		return [][]*ScopeCond{} // non-nil empty: matched perms exist but none unrestricted
	}
	return conjs
}

// callerWildcardConjuncts gathers the DNF conjuncts of the caller's SCOPED
// wildcard permissions (`* on <expr>`). A bare `*` is handled by the caller
// (it covers everything). Used to bound granting a scoped `* on X` to the
// caller's own wildcard scope (least-privilege).
func callerWildcardConjuncts(callerPerms []Permission) [][]*ScopeCond {
	var conjs [][]*ScopeCond
	for _, cp := range callerPerms {
		if !cp.IsWildcard || cp.Scope == nil {
			continue
		}
		dnf, err := toDNF(cp.Scope)
		if err != nil {
			continue
		}
		conjs = append(conjs, dnf...)
	}
	return conjs
}

// disjunctCovered reports whether a required disjunct ri (a conjunction of
// conditions) is within the caller's granted scope.
//
//   - single-conjunct subsumption: ri is covered if ri ⇒ some caller conjunct
//     cj ([impliesConj]); ri may add extra narrowing conditions.
//   - exact-value fast-path (backward compat): when ri is a SINGLE in-list
//     atom (one dimension, no glob), it is covered if EACH value is implied by
//     some caller conjunct — so a caller holding `coven=a` and `coven=b`
//     separately covers granting `coven in (a,b)`, matching the pre-NIM-128
//     per-value union semantics.
func disjunctCovered(ri []*ScopeCond, callerConjs [][]*ScopeCond) bool {
	for _, cj := range callerConjs {
		if impliesConj(ri, cj) {
			return true
		}
	}
	if len(ri) == 1 && ri[0].Match == MatchIn && len(ri[0].Values) > 1 {
		atom := ri[0]
		for _, v := range atom.Values {
			point := []*ScopeCond{{Dim: atom.Dim, Key: atom.Key, Match: MatchIn, Values: []string{v}}}
			covered := false
			for _, cj := range callerConjs {
				if impliesConj(point, cj) {
					covered = true
					break
				}
			}
			if !covered {
				return false
			}
		}
		return true
	}
	return false
}

// impliesConj reports whether the required conjunct ri implies the caller
// conjunct cj (ri ⇒ cj): for EVERY condition of cj, ri must constrain the same
// (dimension, trait-key) at least as tightly ([atomSubset]). A dimension
// constrained by cj but left free by ri → not implied (ri would admit contexts
// outside cj). Dimensions ri constrains but cj does not are fine (ri is
// narrower). Fail-closed: anything not provably subset → false.
func impliesConj(ri, cj []*ScopeCond) bool {
	for _, ac := range cj {
		matched := false
		for _, ar := range ri {
			if ar.Dim != ac.Dim || ar.Key != ac.Key {
				continue
			}
			if atomSubset(ar, ac) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// atomSubset reports whether ar (required) ⊆ ac (caller) on the same
// dimension/key — every context satisfying ar also satisfies ac.
//
//   - in-list ⊆ in-list: set(ar.Values) ⊆ set(ac.Values).
//   - exact host ⊆ host glob: every ar value matches ac's glob.
//   - host glob ⊆ host glob: only equal globs (glob subsumption is deferred;
//     conservative fail-closed, NIM-128 §C.5).
//   - host glob ⊆ in-list, or in-list ⊆ glob for a multi-wildcard glob: a glob
//     matches an unbounded set → not ⊆ a finite set → false.
func atomSubset(ar, ac *ScopeCond) bool {
	switch {
	case ar.Match == MatchIn && ac.Match == MatchIn:
		set := make(map[string]struct{}, len(ac.Values))
		for _, v := range ac.Values {
			set[v] = struct{}{}
		}
		for _, v := range ar.Values {
			if _, ok := set[v]; !ok {
				return false
			}
		}
		return true
	case ar.Match == MatchIn && ac.Match == MatchGlob:
		// ar is an exact host set; each value must match ac's glob.
		for _, v := range ar.Values {
			if !globMatch(ac.Values[0], v) {
				return false
			}
		}
		return true
	case ar.Match == MatchGlob && ac.Match == MatchGlob:
		return ar.Values[0] == ac.Values[0] // equal globs only (conservative)
	default:
		return false
	}
}
