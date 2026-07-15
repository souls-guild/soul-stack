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
//
// A SEPARATE sentinel from [ErrPermissionDenied]: that one means "no
// permission for the operation itself" (checked by middleware/tool BEFORE
// Service); this one means "the operation is allowed, but its content
// exceeds the caller's rights" (checked inside Service). Transport maps both
// to forbidden/403, but the distinction matters for logs and tests.
var ErrPermissionNotHeld = fmt.Errorf("rbac: caller may not grant a permission it does not hold (least-privilege)")

// selectAIDPermissionsSQL selects the active operator's permission strings
// (revoked_at IS NULL) across all their roles, TOGETHER WITH each role's
// default_scope (ADR-047 S1): the caller's bare permission inherits its
// role's scope; otherwise least-privilege would compare raw values and a
// bare perm with scope=prod would falsely cover any coven (privilege
// escalation). Read-only (no FOR UPDATE): a subset check is an
// authorization gate, not a consistency invariant like self-lockout;
// same-tx read under READ COMMITTED sees committed state that's strictly
// fresher than the enforcer snapshot ([PermissionChecker.Check]). Without
// FOR UPDATE the gate adds no new row locks and doesn't touch the
// deterministic lock order of the self-lockout core (role → permissions →
// operators) — no deadlock risk against concurrent mutations.
//
// The `SELECT rp.permission` marker is unique among RBAC queries (the
// self-lockout core and lockRole select `ro.aid`/`builtin`) — test fake
// pools classify the caller-perms query by it without collision.
//
// Synod (ADR-049(f)): the caller's effective rights are direct ∪ via Synod.
// The second UNION branch expands roles through all of the caller's Synods
// (synod_operators ⋈ synod_roles) — without it, least-privilege would
// undercount rights that arrive via a group: an operator whose right X is
// held ONLY through a Synod would falsely be denied when granting X
// (escalation-via-group's flip side: failing to see one's own rights), and
// its effective scope wouldn't inherit from the group's role. UNION (not
// UNION ALL) collapses the duplicate `(permission, default_scope)` pair for
// a role reached both directly and via Synod — a set union, same as in
// snapshot assembly. Both branches filter on `o.revoked_at IS NULL` — a
// revoked caller holds no rights through either path.
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
// [assertCallerCovers]). Called INSIDE the mutation tx (CreateRole /
// UpdateRolePermissions / GrantOperator), before the write.
//
// Empty required → no-op, no DB round-trip (nothing to check — a role with
// no new permissions, or none at all). callerAID == "" is not allowed when
// required is non-empty: transport always carries a caller (claims.Subject);
// an empty caller with grant rights is a misconfiguration — we deny
// ([ErrPermissionNotHeld]) instead of silently allowing it.
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
// would compare raw values (a bare perm covers any coven → a caller with
// scope=prod could grant a role with scope=staging).
//
// rawScope is the RAW default_scope of the role being granted (nil = NULL =
// role has no scope, bare stays unrestricted). The strings are already
// validated by [ParsePermission] in the Service method BEFORE the tx;
// re-parsing here shouldn't fail, but we propagate the error anyway
// (defensive against drift) rather than masking it as a subset failure.
func requiredPermissions(rawPerms []string, rawScope *string) ([]Permission, error) {
	if len(rawPerms) == 0 {
		return nil, nil
	}
	var scope map[string][]string
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
// least-privilege to just these: removing permissions isn't escalation (see
// [Service.UpdateRolePermissions]). Preserves newPerms order; duplicates in
// newPerms are collapsed.
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
// parsed via [ParsePermission], then a bare permission (Selector==nil)
// inherits its role's default_scope via [effectivePermissions] (ADR-047 S1)
// — exactly like [Enforcer.ResolvePurview]. Without this, least-privilege
// would compare raw values: a bare perm with scope=prod would cover any
// coven (privilege escalation).
//
// An invalid permission/default_scope in the DB (version drift) is an
// error: a subset check must not silently swallow a malformed permission
// and accidentally allow a grant. An active operator with no roles → empty
// set (default deny). default_scope NULL (scope==nil) → bare stays
// unrestricted (backcompat, exception #2).
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
		var scope map[string][]string
		if rawScope != nil {
			scope, err = ParseDefaultScope(*rawScope)
			if err != nil {
				return nil, fmt.Errorf("rbac: caller role default_scope %q: %w", *rawScope, err)
			}
		}
		if !p.IsWildcard && p.Selector == nil && scope != nil {
			p.Selector = scope
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rbac: iter caller permissions: %w", err)
	}
	return out, nil
}

// effectivePermissions expands bare permissions (Selector==nil) under a
// role's default_scope — exactly how [Enforcer.ResolvePurview] resolves the
// effective selector (ADR-047 S1): `eff = p.Selector; if nil → scope`. The
// single source of the "bare inherits default_scope" rule; the subset check
// compares EFFECTIVE rights on both sides, not raw ones (otherwise a bare
// perm with scope=prod would falsely cover any coven → privilege
// escalation).
//
// scope==nil (role has no default_scope, NULL) — bare stays bare
// (unrestricted): a caller with no scope may grant any selector (backcompat,
// exception #2 from ADR-047). `*` permissions and per-permission selectors
// are left untouched (scope doesn't override them — symmetric with
// ResolvePurview).
//
// Returns a new slice (input isn't mutated): a bare perm with a non-empty
// scope is replaced by a copy with Selector=scope. Cold path (subset check
// on mutations) — the allocation is acceptable.
func effectivePermissions(perms []Permission, scope map[string][]string) []Permission {
	if scope == nil {
		return perms
	}
	out := make([]Permission, len(perms))
	for i, p := range perms {
		if !p.IsWildcard && p.Selector == nil {
			p.Selector = scope
		}
		out[i] = p
	}
	return out
}

// assertCallerCovers checks that EVERY required permission is covered by the
// caller's effective set (least-privilege subset check). Any uncovered one →
// [ErrPermissionNotHeld] (caller is granting a permission it doesn't hold).
//
// Both sides must ALREADY be effective (bare expanded under its role's
// default_scope via [effectivePermissions]) — otherwise a bare perm with
// scope=prod would falsely cover any coven (privilege escalation, ADR-047
// S1).
//
// Coverage semantics reuse the existing implication from
// [Permission.Matches] (we don't invent a new one): the caller "holds"
// required if at least one of its permissions Matches (resource, action)
// with the required selector as context. A caller's `*` covers everything
// (IsWildcard → Matches=true).
//
// A required `*` (caller grants a full wildcard) is covered ONLY by the
// caller's own `*`: a bare `*` as (resource, action) doesn't match any
// non-wildcard caller permission — so a suboperator can't grant `*`.
//
// callerPerms is resolved once by the caller, not per required permission.
func assertCallerCovers(callerPerms, required []Permission) error {
	for _, req := range required {
		if !callerHolds(callerPerms, req) {
			return fmt.Errorf("%w: %s", ErrPermissionNotHeld, permString(req))
		}
	}
	return nil
}

// permString is a diagnostic rendering of a permission for subset error
// messages (not a canonical serializer; only for logs/tests of
// least-privilege denials).
func permString(p Permission) string {
	if p.IsWildcard {
		return "*"
	}
	s := p.Resource + "." + p.Action
	for key, vals := range p.Selector {
		s += " on " + key + "="
		for i, v := range vals {
			if i > 0 {
				s += ","
			}
			s += v
		}
	}
	return s
}

// callerHolds reports whether the caller's effective set covers req.
//
// required `*` (IsWildcard): covered ONLY by the caller's own `*`. Going
// through Matches directly doesn't work here — Matches(resource, action)
// needs concrete resource/action, which `*` doesn't have. So a wildcard
// required is handled by an explicit branch: look for `*` in the caller's
// set.
//
// non-wildcard required: check EVERY (key, value) combination of the
// selector separately. `x on coven=prod,stage` means "prod OR stage" —
// granting such a permission confers BOTH values, so the caller must cover
// each one. If the caller only has `x on coven=prod`, it can't grant
// `coven=prod,stage` (escalation onto stage). For each such (resource,
// action, {key: value}) point, we ask the caller's permissions via their
// native [Permission.Matches]; required is covered only if every point is
// covered.
func callerHolds(callerPerms []Permission, req Permission) bool {
	if req.IsWildcard {
		for _, cp := range callerPerms {
			if cp.IsWildcard {
				return true
			}
		}
		return false
	}

	// No selector — a single point with nil context.
	if len(req.Selector) == 0 {
		return matchesAny(callerPerms, req.Resource, req.Action, nil)
	}

	// With a selector — every (key, value) point must be covered.
	for key, values := range req.Selector {
		if key == "regex" {
			// ADR-047 S2a: regex subset = string-equality, fail-closed. Whether
			// one regex covers another ("^web- ⊇ ^web-prod-?") is statically
			// undecidable in general → MVP: the caller may grant ONLY an
			// identical pattern (present in its effective regex set) OR holds a
			// broader right (`*` / bare with no regex selector on this
			// resource.action). Going through Matches is NOT an option: a regex
			// string used as a host context would falsely match the caller's
			// regex (^web-prod- matches ^web-).
			for _, pat := range values {
				if !callerHoldsRegex(callerPerms, req.Resource, req.Action, pat) {
					return false
				}
			}
			continue
		}
		if key == "soulprint" {
			// ADR-047 S2b: soulprint subset = string-equality, fail-closed.
			// Whether one CEL predicate covers another (logical containment) is
			// statically undecidable → MVP: the caller may grant ONLY an
			// identical predicate (present in its effective soulprint set) OR
			// holds a broader right (`*` / bare with no soulprint selector on
			// this resource.action). Symmetric with the regex branch.
			for _, expr := range values {
				if !callerHoldsSoulprint(callerPerms, req.Resource, req.Action, expr) {
					return false
				}
			}
			continue
		}
		if key == "state" {
			// ADR-047 S2c: state subset = string-equality, fail-closed. Whether
			// one state CEL predicate covers another (logical containment) is
			// statically undecidable → MVP: the caller may grant ONLY an
			// identical predicate (present in its effective state set) OR holds
			// a broader right (`*` / bare with no state selector on this
			// resource.action). Symmetric with the soulprint/regex branches.
			for _, expr := range values {
				if !callerHoldsState(callerPerms, req.Resource, req.Action, expr) {
					return false
				}
			}
			continue
		}
		if key == "trait" {
			// ADR-047 amendment (ADR-060 item 7, slice 1): trait subset =
			// string-equality, fail-closed. A trait is an exact `key:value` pair
			// (not a predicate), but the coverage logic matches state/soulprint:
			// the caller may grant ONLY an identical pair (present in its
			// effective trait set) OR holds a broader right (`*` / bare with no
			// trait selector on this resource.action). Going through Matches is
			// NOT an option (trait is fail-closed without traits in context) —
			// direct pair comparison, symmetric with the state/soulprint
			// branches.
			for _, pair := range values {
				if !callerHoldsTrait(callerPerms, req.Resource, req.Action, pair) {
					return false
				}
			}
			continue
		}
		for _, v := range values {
			if !matchesAny(callerPerms, req.Resource, req.Action, map[string]string{key: v}) {
				return false
			}
		}
	}
	return true
}

// callerHoldsRegex reports whether the caller covers granting regex pattern
// pat for (resource, action). MVP semantics (ADR-047 S2a, fail-closed):
// covered if the caller has
//   - a `*` permission (covers everything), OR
//   - a matching (resource, action) BARE permission (Selector==nil — caller
//     is unrestricted, may grant any regex), OR
//   - a matching permission with an IDENTICAL regex pattern
//     (string-equality).
//
// A caller RESTRICTED IN A DIFFERENT dimension (`coven=prod` with no regex)
// does NOT cover a regex grant — symmetric with exact keys
// ([Permission.Matches]: key-not-in-context → deny). Narrowing a regex
// (caller `^web-` grants `^web-prod-`) is statically undecidable → DENY.
// Regex containment is NOT implemented (see the S2a spec's warning).
func callerHoldsRegex(callerPerms []Permission, resource, action, pat string) bool {
	for _, cp := range callerPerms {
		if cp.IsWildcard {
			return true
		}
		if cp.Resource != resource {
			continue
		}
		if cp.Action != "*" && cp.Action != action {
			continue
		}
		if cp.Selector == nil {
			// Caller's bare permission — unrestricted by regex, covers any.
			return true
		}
		for _, cpat := range cp.Selector["regex"] {
			if cpat == pat {
				return true
			}
		}
	}
	return false
}

// callerHoldsSoulprint reports whether the caller covers granting soulprint
// predicate expr for (resource, action). MVP semantics (ADR-047 S2b,
// fail-closed, parallels [callerHoldsRegex]): covered if the caller has
//   - a `*` permission (covers everything), OR
//   - a matching (resource, action) BARE permission (Selector==nil — caller
//     is unrestricted, may grant any soulprint predicate), OR
//   - a matching permission with an IDENTICAL soulprint predicate
//     (string-equality).
//
// A caller RESTRICTED IN A DIFFERENT dimension (`coven=prod` / a different
// soulprint) does NOT cover a soulprint grant — symmetric with exact keys
// and regex. Logical containment of CEL predicates is statically undecidable
// → DENY (fail-closed).
func callerHoldsSoulprint(callerPerms []Permission, resource, action, expr string) bool {
	for _, cp := range callerPerms {
		if cp.IsWildcard {
			return true
		}
		if cp.Resource != resource {
			continue
		}
		if cp.Action != "*" && cp.Action != action {
			continue
		}
		if cp.Selector == nil {
			// Caller's bare permission — unrestricted by soulprint, covers any.
			return true
		}
		for _, cexpr := range cp.Selector["soulprint"] {
			if cexpr == expr {
				return true
			}
		}
	}
	return false
}

// callerHoldsState reports whether the caller covers granting state
// predicate expr for (resource, action). MVP semantics (ADR-047 S2c,
// fail-closed, parallels [callerHoldsSoulprint]): covered if the caller has
//   - a `*` permission (covers everything), OR
//   - a matching (resource, action) BARE permission (Selector==nil — caller
//     is unrestricted, may grant any state predicate), OR
//   - a matching permission with an IDENTICAL state predicate
//     (string-equality).
//
// A caller RESTRICTED IN A DIFFERENT dimension (`coven=prod` / a different
// state) does NOT cover a state grant — symmetric with exact keys, regex,
// and soulprint. Logical containment of CEL predicates is statically
// undecidable → DENY (fail-closed).
func callerHoldsState(callerPerms []Permission, resource, action, expr string) bool {
	for _, cp := range callerPerms {
		if cp.IsWildcard {
			return true
		}
		if cp.Resource != resource {
			continue
		}
		if cp.Action != "*" && cp.Action != action {
			continue
		}
		if cp.Selector == nil {
			// Caller's bare permission — unrestricted by state, covers any.
			return true
		}
		for _, cexpr := range cp.Selector["state"] {
			if cexpr == expr {
				return true
			}
		}
	}
	return false
}

// callerHoldsTrait reports whether the caller covers granting trait pair
// pair (`key:value`) for (resource, action). MVP semantics (ADR-047
// amendment / ADR-060 item 7, slice 1, fail-closed, parallels
// [callerHoldsState]): covered if the caller has
//   - a `*` permission (covers everything), OR
//   - a matching (resource, action) BARE permission (Selector==nil — caller
//     is unrestricted, may grant any trait pair), OR
//   - a matching permission with an IDENTICAL trait pair (string-equality).
//
// A caller RESTRICTED IN A DIFFERENT dimension (`coven=prod` / a different
// trait pair) does NOT cover a trait grant — symmetric with exact keys,
// regex, soulprint, and state.
func callerHoldsTrait(callerPerms []Permission, resource, action, pair string) bool {
	for _, cp := range callerPerms {
		if cp.IsWildcard {
			return true
		}
		if cp.Resource != resource {
			continue
		}
		if cp.Action != "*" && cp.Action != action {
			continue
		}
		if cp.Selector == nil {
			// Caller's bare permission — unrestricted by trait, covers any.
			return true
		}
		for _, cpair := range cp.Selector["trait"] {
			if cpair == pair {
				return true
			}
		}
	}
	return false
}

// matchesAny reports whether at least one caller permission matches the
// specific (resource, action, context) point. A thin wrapper over
// [Permission.Matches] for iterating the set.
func matchesAny(callerPerms []Permission, resource, action string, ctx map[string]string) bool {
	for _, cp := range callerPerms {
		if cp.Matches(resource, action, ctx) {
			return true
		}
	}
	return false
}
