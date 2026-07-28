package rbac

import "fmt"

// Resolution of the READ catalog (ADR-078, NIM-181). [LoadRoleViews] returns
// roles as they are stored — a derived role's rows are its delta, not its rights.
// This file fills the effective form next to the stored one, so the API can hand
// out both: what the operator wrote, and what it actually resolves to.
//
// It runs the SAME code the enforcer runs ([validateRoleGraph] + [flattenRoleGraph]),
// for the reason the write gate does (attenuate.go §resolveRoleChain): a second
// implementation of the attenuation rules is free to disagree with the decision
// layer, and a catalog that overstates a child's rights is exactly the mistake the
// UI is being spared.

// resolveRoleViews fills EffectivePermissions / EffectiveScope on every view, in
// place. views is the whole catalog — resolution needs each parent present, which
// is why it takes the catalog rather than one role.
//
// Fail-closed as a whole: a broken graph (a cycle, an over-deep chain, a dangling
// parent, an unparseable row) fails the READ instead of degrading it. Per ADR-078(f)
// such a catalog builds no enforcer either, so the Keeper is already refusing to
// serve on it; returning the stored-but-unresolved rows would mean publishing a
// derived role's rights as WIDER than they are.
func resolveRoleViews(views []RoleView) error {
	byName := make(map[string]*Role, len(views))
	names := make(map[string]struct{}, len(views))
	parents := make(map[string]string, len(views))

	for i := range views {
		v := &views[i]
		role := &Role{Name: v.Name, ParentRole: v.ParentRole}
		for _, raw := range v.Permissions {
			p, err := ParsePermission(raw)
			if err != nil {
				return fmt.Errorf("rbac: role %q permission %q: %w", v.Name, raw, err)
			}
			role.Permissions = append(role.Permissions, p)
		}
		scope, err := ParseDefaultScope(v.DefaultScope)
		if err != nil {
			return fmt.Errorf("rbac: role %q default_scope %q: %w", v.Name, v.DefaultScope, err)
		}
		role.DefaultScope = scope

		byName[v.Name] = role
		names[v.Name] = struct{}{}
		if v.ParentRole != "" {
			parents[v.Name] = v.ParentRole
		}
	}

	if err := validateRoleGraph(names, parents); err != nil {
		return err
	}
	if err := flattenRoleGraph(byName); err != nil {
		return err
	}

	for i := range views {
		resolved := byName[views[i].Name]
		views[i].EffectivePermissions = permStrings(resolved.Permissions)
		views[i].EffectiveScope = resolved.DefaultScope.String()
		views[i].InertPermissions = permStrings(resolved.InertPermissions)
	}
	return nil
}
