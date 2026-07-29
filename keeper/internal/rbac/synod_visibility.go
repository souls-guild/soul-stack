package rbac

import "fmt"

// Read-side visibility of the Synod catalog (NIM-216).
//
// `synod.list` used to answer with every group in full for every caller, and a
// group carries the two things NIM-202 had just finished taking off `role.list`:
// WHO is in it, and WHICH roles it bundles. Membership is the readiest answer to
// "who do I attack to reach X", and the bundle is the delegation structure — which
// packages of rights exist and who was handed them. After NIM-202 this was the
// last way to read a piece of the privilege map out of a list.
//
// The rule needs no invention, because the write side already states it. By
// ADR-049(f) `synod.add-operator` demands the caller hold the effective rights of
// EVERY role the group bundles — a member receives the whole bundle. Reading that
// backwards:
//
//	a group is visible ⟺ the caller may see every role it bundles
//
// i.e. "I see the group exactly when I could have put someone in it", the same
// shape as NIM-202's "I see the role exactly when I could have granted it". It is
// literally [callerMaySeeRole] applied across the bundle, so there is no second
// notion of "close enough to show" that could drift away from the decision layer —
// one definition of "⊆" for the subsystem (ADR-078(c)).
//
// Two consequences fall out rather than being decided:
//
//   - a group that bundles NOTHING is visible to everyone. The quantifier is over
//     an empty set. It matches the role rule, where a role granting nothing is
//     visible to everyone, and for the same reason: it exposes no privilege.
//   - a visible group is returned WHOLE, members included. Visibility already
//     means the caller could add any of them; hiding the roster of a group they
//     may administer would be a half-truth with no boundary behind it.
//
// The auditor who must see every group while holding nothing is `synod.list-all`,
// the mirror of `role.list-all` (NIM-203) — no coverage rule can express that
// reader, and without the mirror an auditor granted the full role catalog would
// still be blind to the groups those roles are bundled into.

// callerHoldsFullSynodCatalog reports whether the caller may read the WHOLE group
// catalog — the `synod.list-all` right, mirroring [callerHoldsFullCatalog].
//
// A BREADTH modifier, not a route: `synod.list` still gates the endpoint, this
// decides how much comes back, and granting it alone opens nothing. Deliberately
// BARE, so [callerHolds] demands an UNRESTRICTED holder: the scope grammar has no
// `synod=` dimension, so a scoped form could not say which groups it covers.
func callerHoldsFullSynodCatalog(callerPerms []Permission) bool {
	return callerHolds(callerPerms, Permission{Resource: "synod", Action: "list-all"})
}

// visibleSynodViews keeps the groups callerPerms may see, preserving input order
// ([LoadSynodViews] sorts by name; filtering must not reshuffle it). byRole is the
// resolved role catalog keyed by name.
func visibleSynodViews(callerPerms []Permission, views []SynodView, byRole map[string]RoleView) ([]SynodView, error) {
	out := make([]SynodView, 0, len(views))
	for _, v := range views {
		visible, err := callerMaySeeSynod(callerPerms, v, byRole)
		if err != nil {
			return nil, err
		}
		if visible {
			out = append(out, v)
		}
	}
	return out, nil
}

// callerMaySeeSynod reports whether callerPerms cover every role the group
// bundles. Short-circuits on the first role they do not: a group is as sensitive
// as the widest thing in it.
//
// A bundled role missing from byRole is treated as NOT visible. It should be
// unreachable — `synod_roles.role_name` is an FK with ON DELETE CASCADE, and
// [LoadSynodViews] already drops orphans — but a bundle member whose rights cannot
// be read is a bundle whose sensitivity is unknown, and the fail-closed answer to
// that is to withhold the group.
func callerMaySeeSynod(callerPerms []Permission, v SynodView, byRole map[string]RoleView) (bool, error) {
	for _, name := range v.Roles {
		rv, known := byRole[name]
		if !known {
			return false, nil
		}
		visible, err := callerMaySeeRole(callerPerms, rv)
		if err != nil {
			return false, fmt.Errorf("rbac: synod %q: %w", v.Name, err)
		}
		if !visible {
			return false, nil
		}
	}
	return true, nil
}

// roleViewsByName indexes a resolved role catalog for bundle lookups.
func roleViewsByName(views []RoleView) map[string]RoleView {
	byName := make(map[string]RoleView, len(views))
	for _, v := range views {
		byName[v.Name] = v
	}
	return byName
}
