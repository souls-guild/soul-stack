package rbac

import "fmt"

// Read-side visibility of the role catalog (NIM-202).
//
// `role.list` used to answer with the whole catalog for every caller, and a role
// carries far more than its name: its permission set, its scope and the AIDs
// holding it. Read together that is the cluster's privilege map — which covens
// and services exist, who administers what, and which operator to attack to
// reach `*`. A scoped operator could read all of it.
//
// The rule: a caller sees a role exactly when the caller could GRANT what that
// role grants — the least-privilege containment of subset.go, evaluated against
// the role's EFFECTIVE form. Reusing the write-side predicate instead of
// inventing a visibility one is the point:
//
//   - it is already a boundary the caller cannot cross, so it leaks nothing. A
//     role you may see is a role you could have created, been granted, or
//     derived from;
//   - one definition of "⊆" for the subsystem (ADR-078(c)). A second, read-only
//     notion of "close enough to show" would be free to disagree with the
//     decision layer, and in this direction a disagreement is a leak;
//   - a bare `*` covers everything, so the cluster-admin surface is unchanged
//     and nothing has to maintain a list of "roles that are safe to show".
//
// Comparison runs on the EFFECTIVE permissions and scope, never the stored rows:
// the effective form is what the role actually grants (ADR-078(c)/(d)), so it is
// what "could the caller grant this" has to be asked about. A derived role
// carrying a row its parent stopped covering grants nothing through that row and
// must not be hidden on account of it.
//
// An auditor has to see the whole catalog while holding nothing, which no
// coverage rule can express — that is [callerHoldsFullCatalog] and the explicit
// `role.list-all` right (NIM-203).

// callerHoldsFullCatalog reports whether the caller may read the WHOLE role
// catalog — the `role.list-all` right of NIM-203, for the reader who must see
// roles they hold nothing of: an auditor, a security review, first-line support
// identifying who holds what.
//
// It is a BREADTH modifier, not a route: `role.list` still gates the endpoint,
// this decides how much comes back. Granting it alone opens nothing.
//
// Checked with the ordinary containment against the caller set the filter has
// already loaded — no extra query, and `*` / `role.*` cover it for free. The
// required permission is deliberately BARE, so [callerHolds] demands the caller
// be UNRESTRICTED on it: a scoped `role.list-all on coven=X` grants no full
// catalog, which is the only sound reading — the scope grammar has no `role=`
// dimension, so there is nothing for such a scope to select.
func callerHoldsFullCatalog(callerPerms []Permission) bool {
	return callerHolds(callerPerms, Permission{Resource: "role", Action: "list-all"})
}

// visibleRoleViews keeps the roles callerPerms cover, preserving input order
// (LoadRoleViews sorts by name; filtering must not reshuffle it).
func visibleRoleViews(callerPerms []Permission, views []RoleView) ([]RoleView, error) {
	out := make([]RoleView, 0, len(views))
	for _, v := range views {
		visible, err := callerMaySeeRole(callerPerms, v)
		if err != nil {
			return nil, err
		}
		if visible {
			out = append(out, v)
		}
	}
	return out, nil
}

// callerMaySeeRole reports whether callerPerms cover everything role v grants.
//
// Bare permissions are expanded under the role's effective scope — the same
// normalisation [effectiveRoleRights] performs for a role being written
// (ADR-047 S1). Comparing the raw strings instead would read a bare
// `incarnation.get` on a role scoped to `coven=prod` as unrestricted, and hide
// the role from the very operator who owns that coven.
//
// The parent is nil because [RoleView.EffectivePermissions] is ALREADY resolved
// against the chain ([resolveRoleViews]); passing one would attenuate a form
// that has been attenuated once and narrow the role out of its owner's view.
//
// A role that grants nothing is visible to everyone: it exposes no privilege,
// and any caller could create the same empty role themselves.
func callerMaySeeRole(callerPerms []Permission, v RoleView) (bool, error) {
	required, err := effectiveRoleRights(nil, v.EffectivePermissions, effectiveScopePtr(v))
	if err != nil {
		// Unreachable: the effective form is rendered from already-parsed
		// permissions ([resolveRoleViews]). Fail the read rather than publish a
		// role whose rights could not be compared.
		return false, fmt.Errorf("rbac: role %q: %w", v.Name, err)
	}
	return assertCallerCovers(callerPerms, required) == nil, nil
}

// effectiveScopePtr adapts [RoleView]'s scope string ("" = unrestricted) to the
// nullable form [effectiveRoleRights] takes.
func effectiveScopePtr(v RoleView) *string {
	if v.EffectiveScope == "" {
		return nil
	}
	return &v.EffectiveScope
}
