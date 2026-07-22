package rbac

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// ErrPermissionDenied is the sentinel deny result. All other Check errors
// (e.g. an unknown action in the catalog) are returned wrapped, but NOT
// through this sentinel — middleware maps them the same way (403), but
// tests need to distinguish "explicit deny" from "misconfigured call".
var ErrPermissionDenied = errors.New("rbac: permission denied")

// ErrOperatorRevoked is the sentinel deny result for an Archon whose
// `operators` registry row has `revoked_at` set (ADR-014 Amendment
// 2026-05-27, JWT immediate revoke). The transport maps it to 401 (parity
// with an expired JWT), NOT to 403 like [ErrPermissionDenied] — the token is
// formally valid but no longer trusted.
var ErrOperatorRevoked = errors.New("rbac: operator revoked")

// Role is the runtime form of a role from the DB snapshot (after parsing
// permissions). Not exported via API (handlers only see Enforcer); external
// code learns an operator's "roles" through [Enforcer.RolesOf].
type Role struct {
	Name        string
	Permissions []Permission

	// DefaultScope is the parsed role default_scope (ADR-047 S1, NIM-128
	// boolean scope), inherited by the role's permissions that have no scope of
	// their own. nil = NULL = the dimension is NOT introduced (bare-permission
	// roles → unrestricted, backcompat).
	DefaultScope *ScopeExpr
}

// Enforcer is an in-memory snapshot of the RBAC catalog. Safe for concurrent
// reads after construction (immutable; ADR-021 hot-reload will swap the
// weak-ref pointer wholesale via [config.Store] — this type doesn't reload
// itself).
type Enforcer struct {
	// rolesByAID resolves "AID → []*Role". Pointers so we don't copy
	// permission lists on every Check (a role can hold dozens of
	// permissions).
	rolesByAID map[string][]*Role

	// roles holds all roles in declaration order. Used by RolesOf and
	// diagnostics.
	roles []*Role

	// revoked is a copy of Snapshot.Revoked. Stored on the enforcer (rather
	// than as a separate projection) so Check is one cheap map lookup with no
	// sync against an external structure (immutable after construction).
	revoked map[string]time.Time
}

// NewEnforcerFromSnapshot builds an Enforcer from a DB snapshot (ADR-028(d))
// — the sole source of the RBAC catalog (config-RBAC was removed by the
// ADR-028(g) hard cut). Permission strings are parsed via [ParsePermission];
// matching and surface — Check / RolesOf / ClusterAdmins / HasWildcard.
//
// nil snapshot → empty enforcer (default deny). An invalid permission in the
// DB (e.g. a name outside the catalog after a version desync) → error; the
// caller ([Holder]) keeps the previous snapshot + warns on a TTL-refresh
// failure, same as it did on a config-reload failure.
//
// Membership entries referencing a role outside snapshot.Roles are ignored
// (desync protection; in practice the rbac_role_operators.role_name FK rules
// this out).
func NewEnforcerFromSnapshot(snap *Snapshot) (*Enforcer, error) {
	e := &Enforcer{
		rolesByAID: make(map[string][]*Role),
	}
	if snap == nil {
		return e, nil
	}
	// Revoked projection (ADR-014 Amendment 2026-05-27): no copy needed —
	// Snapshot.Revoked is never mutated after the enforcer is constructed
	// (the caller, Holder, always builds a fresh enforcer after Refresh).
	e.revoked = snap.Revoked

	byName := make(map[string]*Role, len(snap.Roles))
	for name, rawPerms := range snap.Roles {
		role := &Role{Name: name}
		for _, raw := range rawPerms {
			p, err := ParsePermission(raw)
			if err != nil {
				return nil, fmt.Errorf("rbac: role %q permission %q: %w", name, raw, err)
			}
			role.Permissions = append(role.Permissions, p)
		}
		// Role default_scope (ADR-047 S1): parsed with the same
		// [parseSelector] as a per-permission selector. A missing key in
		// RoleScopes = NULL = nil scope (role has no scope restriction).
		if rawScope, ok := snap.RoleScopes[name]; ok {
			scope, err := ParseDefaultScope(rawScope)
			if err != nil {
				return nil, fmt.Errorf("rbac: role %q default_scope %q: %w", name, rawScope, err)
			}
			role.DefaultScope = scope
		}
		byName[name] = role
		e.roles = append(e.roles, role)
	}

	for aid, roleNames := range snap.Membership {
		for _, name := range roleNames {
			role, ok := byName[name]
			if !ok {
				// Role from membership is missing in the catalog (desync) —
				// skip: a binding to a nonexistent role grants nothing.
				continue
			}
			e.rolesByAID[aid] = append(e.rolesByAID[aid], role)
		}
	}

	return e, nil
}

// Check returns nil if AID has permission for (resource, action) in the
// given context; otherwise a wrapped [ErrPermissionDenied].
//
// Algorithm per rbac.md § Conflict semantics (OR among allows):
//  1. Find the AID's roles.
//  2. For each permission across the roles — Matches(resource, action, context).
//  3. At least one true → allow (return nil).
//  4. Otherwise → ErrPermissionDenied.
//
// context is a runtime filter passed by middleware from the request (e.g.
// `{"service": ..., "incarnation": ...}` for incarnation endpoints). An
// empty map is valid — it means "no context"; in that case selector
// permissions won't match, only bare permissions and full wildcard.
func (e *Enforcer) Check(aid, resource, action string, context map[string]string) error {
	if resource == "" || action == "" {
		return fmt.Errorf("rbac: Check called with empty resource/action")
	}
	// Revoked shortcut (ADR-014 Amendment 2026-05-27): a revoked Archon gets
	// deny regardless of roles. This check runs BEFORE any permission logic —
	// otherwise a bare `*` role would let a revoked AID through.
	if revokedAt, ok := e.revoked[aid]; ok {
		return fmt.Errorf("%w: AID %q revoked at %s",
			ErrOperatorRevoked, aid, revokedAt.UTC().Format(time.RFC3339))
	}
	roles, ok := e.rolesByAID[aid]
	if !ok || len(roles) == 0 {
		return fmt.Errorf("%w: AID %q has no roles, resource=%q action=%q",
			ErrPermissionDenied, aid, resource, action)
	}
	for _, role := range roles {
		for _, p := range role.Permissions {
			if p.Matches(resource, action, context) {
				return nil
			}
		}
	}
	return fmt.Errorf("%w: AID %q lacks %s.%s (roles: %s)",
		ErrPermissionDenied, aid, resource, action, joinRoleNames(roles))
}

// IsRevoked — whether the snapshot holds the AID in the revoked projection (ADR-014 Amendment).
// A cheap map lookup without role/permission logic; the cookie→Bearer exchange
// (POST /auth/token, NIM-77) rejects a revoked Archon in-memory, not via SQL.
func (e *Enforcer) IsRevoked(aid string) bool {
	_, ok := e.revoked[aid]
	return ok
}

// HasWildcard — true if the AID has at least one `*`-permission
// (through any of its roles). Used by the self-lockout invariant —
// "cannot revoke the last cluster-admin" (rbac.md → Self-lockout invariant).
func (e *Enforcer) HasWildcard(aid string) bool {
	for _, role := range e.rolesByAID[aid] {
		for _, p := range role.Permissions {
			// Only a BARE `*` is cluster-admin for self-lockout. A scoped
			// `* on X` (NIM-128) is bounded and does NOT count — revoking the
			// last such operator can't lock the cluster out of RBAC.
			if p.IsWildcard && p.Scope == nil {
				return true
			}
		}
	}
	return false
}

// ClusterAdmins returns the list of AIDs with an active wildcard permission.
// Used for the self-lockout check: "if the revoke target is the sole active
// cluster-admin → 409 would-lock-out-cluster".
//
// The returned list is a snapshot of enforcer state and doesn't account for
// revoked_at in the DB — that's a layer up, the caller filters out revoked.
func (e *Enforcer) ClusterAdmins() []string {
	// Set, to dedupe AIDs that have wildcard through multiple roles.
	seen := make(map[string]struct{})
	for aid := range e.rolesByAID {
		if e.HasWildcard(aid) {
			seen[aid] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for aid := range seen {
		out = append(out, aid)
	}
	return out
}

// CovenScope returns the operator's coven scope for a specific (resource,
// action) — the set of Coven labels their right covers, plus an unrestricted
// flag.
//
// Semantics — the dual of [Permission.Matches] on the `coven` key:
//   - unrestricted=true if AID has at least one matching (resource, action)
//     permission with NO selector at all (nil), or `*` (full wildcard), or a
//     `coven` key with wildcard value `*`. Such an operator is not
//     coven-restricted — bulk WHERE adds no coven filter, and the assigned
//     label passes without a scope check.
//   - unrestricted=false → covens = union of all concrete `coven=` values
//     across matching permissions (deduped, sorted).
//   - a permission with a non-empty selector but WITHOUT a `coven` key (only
//     `host=`/`incarnation=`/`service=`) does NOT make the operator
//     unrestricted on coven: its contribution is covens=nil,
//     unrestricted=false. This mirrors [Permission.Matches] — there, a
//     host-only permission won't match a request with no `host` in context,
//     meaning it restricts along a different dimension, not "allows any
//     coven". Latent escalation footgun if CovenScope is called without a
//     route gate.
//
// Union across multiple matching roles: if ANY of them is unrestricted → the
// result is unrestricted=true; otherwise covens is the union of concrete
// coven values from roles with a `coven` key. An empty covens with
// unrestricted=false means "the operator has no right to touch any coven for
// this action" (e.g. a right with only a `host=` selector).
//
// Used by the bulk-API scope intersection (selector ∩ scope, pilot spec
// `POST /v1/souls/coven`): target hosts ⊆ scope + the assigned label ∈ scope.
// Mirrors the least-privilege subset check in [Service] (that one caps
// permission GRANTS, this one caps the SCOPE of a bulk mutation).
//
// S0 (ADR-047): CovenScope is a thin projection of [Enforcer.ResolvePurview]
// onto the `coven` dimension. All the logic (the `coven`-keyed dual of
// [Permission.Matches], the union across roles, the bare/`*`/`coven=*` →
// unrestricted semantics) lives in ResolvePurview; this just unpacks Purview
// into the legacy `(covens, unrestricted)` shape so the sole consumer
// (soul.coven-assign) doesn't need to change. Full normative semantics for
// the `coven=*` wildcard branch and host-only selectors live in
// [Enforcer.ResolvePurview] and [Permission.Matches].
func (e *Enforcer) CovenScope(aid, resource, action string) (covens []string, unrestricted bool) {
	p := e.ResolvePurview(aid, resource, action)
	if p.Unrestricted {
		return nil, true
	}
	return covensFromPurview(p), false
}

// covensFromPurview conservatively extracts the coven labels an operator may
// bulk-assign/target (NIM-128). A coven value counts ONLY when it appears in a
// DNF disjunct that constrains coven ALONE — a disjunct mixing coven with
// another dimension (`coven=a AND host matches x`) narrows below "any host in
// coven a", so projecting it to just `a` would OVER-permit a bulk coven
// mutation. Such mixed disjuncts are dropped (fail-closed). Order-stable.
func covensFromPurview(p Purview) []string {
	set := make(map[string]struct{})
	for _, expr := range p.Exprs {
		dnf, err := toDNF(expr)
		if err != nil {
			continue // too complex → contribute nothing (fail-closed)
		}
		for _, conj := range dnf {
			pureCoven := true
			for _, c := range conj {
				if c.Dim != dimCoven || c.Match != MatchIn {
					pureCoven = false
					break
				}
			}
			if !pureCoven {
				continue
			}
			for _, c := range conj {
				for _, v := range c.Values {
					set[v] = struct{}{}
				}
			}
		}
	}
	return sortedKeys(set)
}

// HoldsAction is the existence gate for read endpoints (ADR-047 §d amendment
// 2026-06-04): does the operator hold (resource, action) in ANY scope at
// all, ignoring the request context. This is a different question than
// scope-aware [Enforcer.Check]: Check answers "does the permission apply in
// this scope context", and for a scoped permission with an empty context it
// gives a false deny (the selector key isn't in a nil context) — which would
// cut a scoped operator off from their own list BEFORE the handler runs. The
// gate only asks about the EXISTENCE of the right; scope narrowing is done by
// the handler after fetching rows (per-resource soulpurview/statepredicate
// resolvers).
//
// Semantics on top of [Enforcer.ResolvePurview] (no new matching logic):
//   - bare permission / `*` / any populated dimension (coven/regex/
//     soulprint/state) → true (existence holds);
//   - no permission (Purview{} — no matching role / unknown AID) → false;
//   - Deny → false (forward-compat S2+: "an introduced empty dimension" =
//     explicit scope deny; in the coven MVP, Deny is never set — this branch
//     is a placeholder).
func (e *Enforcer) HoldsAction(aid, resource, action string) bool {
	return holdsFromPurview(e.ResolvePurview(aid, resource, action))
}

// holdsFromPurview is the existence-gate predicate over an already-resolved
// [Purview] (single source of truth for both [Enforcer.HoldsAction] and its
// test). Factored out of the method body so the guard test for the
// forward-compat `Deny` branch (ResolvePurview never sets Deny in the coven
// MVP) checks the same formula rather than a duplicate — otherwise the test
// and the method could silently drift apart when the formula changes.
//
//   - Deny → false (forward-compat S2+: "an introduced empty dimension" =
//     explicit scope deny);
//   - otherwise bare/`*` (Unrestricted) OR any populated dimension
//     (coven/regex/soulprint/state/trait) → true.
func holdsFromPurview(p Purview) bool {
	return p.Holds()
}

// RolesOf returns the names of the roles bound to AID. Used by `IssueToken`
// (PM decision M0.6b #5) — issue a JWT with current roles from keeper.yml,
// not the roles from an old JWT.
func (e *Enforcer) RolesOf(aid string) []string {
	roles := e.rolesByAID[aid]
	if len(roles) == 0 {
		return nil
	}
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		out = append(out, r.Name)
	}
	return out
}

// EffectivePermission is one effective operator right with its scope broken
// out. Returned by [Enforcer.PermissionsOf] for the self-describing
// `GET /v1/me/permissions` endpoint (permission-aware UI: "can I
// resource.action, and in what scope").
//
// Wildcard=true means full-`*` (cluster-admin): Resource/Action are empty,
// Scope is zero-valued (the right isn't restricted by anything; the UI
// treats this as "can do everything"). In this case PermissionsOf returns
// EXACTLY one marker element and nothing else.
type EffectivePermission struct {
	// Wildcard means the operator has full-`*` (cluster-admin). When true,
	// Resource/Action are empty and Scope is ignored.
	Wildcard bool

	// Resource/Action is the concrete permission pair (`incarnation`/`run`).
	// Action can be `*` (resource wildcard, e.g. `incarnation.*`).
	Resource string
	Action   string

	// Scope is the allowed scope of this right across dimensions
	// ([ResolvePurview]): covens / regex / soulprint + the Unrestricted flag.
	// The UI decides whether to render a scope summary. For a resource
	// wildcard (`incarnation.*`), scope is resolved against the wildcard
	// action `*` itself — the upper bound for the resource.
	Scope Purview
}

// PermissionsOf returns all of AID's effective rights — an unpacking of
// `rolesByAID[aid][*].Permissions` deduped by (resource, action), with scope
// from [Enforcer.ResolvePurview] on each pair. Deterministic order
// (resource, then action) for UI/test stability.
//
// Semantics:
//   - full-`*` in even one role → EXACTLY one
//     [EffectivePermission]{Wildcard:true} (cluster-admin is unrestricted;
//     enumerating the whole catalog would be pointless).
//   - otherwise one entry per unique (resource, action); Scope is resolved by
//     [ResolvePurview], which itself unions across roles and inherits
//     default_scope (ADR-047). This does NOT duplicate Matches/subset logic —
//     it only reads already-parsed Permissions and the existing resolver.
//   - unknown AID / AID with no roles → nil.
//
// A resource wildcard (`incarnation.*`) is returned as-is (Action="*"); the
// UI treats `*` as "any action on this resource".
func (e *Enforcer) PermissionsOf(aid string) []EffectivePermission {
	// Revoked shortcut (ADR-047 G1, mirrors [Enforcer.Check]/[Enforcer.ResolvePurview]):
	// a revoked Archon gets an empty rights list regardless of roles — BEFORE
	// the IsWildcard branch, otherwise a revoked cluster-admin (`*`) would
	// still see their former wildcard marker on `GET /v1/me/permissions`.
	// revoked = "no rights" (wildcard and scoped alike); the result shape
	// matches the unknown-AID branch (nil).
	if _, ok := e.revoked[aid]; ok {
		return nil
	}
	roles, ok := e.rolesByAID[aid]
	if !ok || len(roles) == 0 {
		return nil
	}

	// Dedup by (resource, action). A BARE `*` is the unrestricted cluster-admin
	// marker (dominates everything). A scoped `* on X` (NIM-128) is surfaced as
	// a wildcard entry carrying its scope — "all actions, capped to X" — the
	// ceiling for any (resource, action) not otherwise listed.
	type pair struct{ resource, action string }
	seen := make(map[pair]struct{})
	seenWild := make(map[string]struct{})
	var wildExprs []*ScopeExpr
	for _, role := range roles {
		for _, p := range role.Permissions {
			if p.IsWildcard {
				if p.Scope == nil {
					return []EffectivePermission{{Wildcard: true}}
				}
				if k := p.Scope.String(); k != "" {
					if _, dup := seenWild[k]; !dup {
						seenWild[k] = struct{}{}
						wildExprs = append(wildExprs, p.Scope)
					}
				}
				continue
			}
			seen[pair{p.Resource, p.Action}] = struct{}{}
		}
	}

	out := make([]EffectivePermission, 0, len(seen)+1)
	if len(wildExprs) > 0 {
		sort.Slice(wildExprs, func(i, j int) bool { return wildExprs[i].String() < wildExprs[j].String() })
		out = append(out, EffectivePermission{Wildcard: true, Scope: Purview{Exprs: wildExprs}})
	}
	for pr := range seen {
		out = append(out, EffectivePermission{
			Resource: pr.resource,
			Action:   pr.action,
			Scope:    e.ResolvePurview(aid, pr.resource, pr.action),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Resource != out[j].Resource {
			return out[i].Resource < out[j].Resource
		}
		return out[i].Action < out[j].Action
	})
	return out
}

// RoleCount is the number of roles in the snapshot. Source for
// keeper_rbac_snapshot_roles: Enforcer is immutable and doesn't hold onto
// the Snapshot after construction, so the metric reads it from here instead
// of [Snapshot].
func (e *Enforcer) RoleCount() int {
	return len(e.roles)
}

// OperatorCount is the number of operators with >=1 role binding in the
// snapshot. Source for keeper_rbac_snapshot_operators: rolesByAID only holds
// AIDs bound to existing roles (an AID with no bindings is default-deny and
// never enters the map).
func (e *Enforcer) OperatorCount() int {
	return len(e.rolesByAID)
}

// joinRoleNames returns comma-separated role names for diagnostic messages.
func joinRoleNames(roles []*Role) string {
	if len(roles) == 0 {
		return "<none>"
	}
	out := roles[0].Name
	for _, r := range roles[1:] {
		out += "," + r.Name
	}
	return out
}
