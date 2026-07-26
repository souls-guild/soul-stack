package rbac

import (
	"context"
	"errors"
	"fmt"
)

// Write-time attenuation for derived roles (ADR-078(c)/(h), NIM-180).
//
// The enforcer re-resolves every chain on every snapshot build, so "a child never
// exceeds its parent" holds at the DECISION layer whatever is stored — a row the
// parent stops covering simply stops granting. What this file adds is the write
// gate: an operator who asks for a right their chosen parent does not hold gets a
// refusal now, instead of a role that silently grants less than it reads.
//
// Two conditions, both required (ADR-078(h)):
//
//	child ⊆ parent        structural — the child's rows, resolved the way the
//	                      enforcer will resolve them, are covered by the parent's
//	                      effective rights.
//	caller holds parent   least-privilege — the EXISTING subset.go floor, applied
//	                      to the parent rather than only to the child. Without it,
//	                      an operator with `role.create` could derive from a role
//	                      far above their own rights: the child would be legal at
//	                      creation, and the cascade would then carry every later
//	                      widening of that parent straight into it.
//
// The parent's chain is read inside the mutation's transaction but NOT locked.
// Deliberate: locking it would add a second lock order over `rbac_roles` on top
// of the self-lockout probes for no security gain, because a concurrent narrowing
// of the parent is absorbed by re-resolution at the next snapshot build. The gate
// is here for the error message, not for the boundary.

// ErrRoleExceedsParent — a derived role would carry a right its parent does not
// hold: a permission outside the parent's effective set, or a scope the parent's
// ceiling does not cover. The structural half of ADR-078(h). Transport maps it to
// 403, alongside [ErrPermissionNotHeld] — both are "you asked for a right that is
// not yours to give", they only differ in which ceiling refused it.
var ErrRoleExceedsParent = errors.New("rbac: derived role exceeds its parent role")

// selectRoleChainSQL — a role and its ancestors, with each one's default_scope and
// permission rows, in a single round trip. The recursive term walks UP the
// parent edges from $1.
//
// The `lvl` bound is not an optimization: a chain corrupted out-of-band (a row
// written before migration 102, a restore) would otherwise make the CTE spin. It
// reaches one hop PAST the cap so such a chain still trips [validateRoleGraph] —
// as too deep, or as a parent outside the collected set — instead of being
// silently truncated into a shallower, and therefore WIDER, ceiling.
//
// LEFT JOIN so a role with no permission rows still appears — it is a legal role
// that grants nothing, and as a parent it is the tightest ceiling there is.
const selectRoleChainSQL = `
WITH RECURSIVE chain(name, parent_role, default_scope, lvl) AS (
    SELECT r.name, r.parent_role, r.default_scope, 1
      FROM rbac_roles r
     WHERE r.name = $1
    UNION ALL
    SELECT r.name, r.parent_role, r.default_scope, c.lvl + 1
      FROM rbac_roles r
      JOIN chain c ON r.name = c.parent_role
     WHERE c.lvl <= $2
)
SELECT c.name, c.parent_role, c.default_scope, rp.permission
FROM chain c
LEFT JOIN rbac_role_permissions rp ON rp.role_name = c.name
`

// resolveRoleChain reads a role and its ancestors from the DB and returns the
// role in EFFECTIVE form — the exact resolution [NewEnforcerFromSnapshot]
// performs, run against live rows inside the mutation's transaction.
//
// It deliberately goes through [validateRoleGraph] and [flattenRoleGraph] rather
// than resolving inline: the write gate and the decision layer must not be able
// to disagree about what a chain means, and the only way to guarantee that is to
// run the same code.
//
// Returns [ErrRoleNotFound] when the role does not exist.
func resolveRoleChain(ctx context.Context, db ExecQueryRower, name string) (*Role, error) {
	rows, err := db.Query(ctx, selectRoleChainSQL, name, maxRoleChainDepth)
	if err != nil {
		return nil, fmt.Errorf("rbac: read chain of role %q: %w", name, wrapPgErr(err))
	}
	defer rows.Close()

	byName := make(map[string]*Role)
	rawScopes := make(map[string]string)
	parents := make(map[string]string)
	for rows.Next() {
		var (
			roleName     string
			parentRole   *string
			defaultScope *string
			permission   *string
		)
		if err := rows.Scan(&roleName, &parentRole, &defaultScope, &permission); err != nil {
			return nil, fmt.Errorf("rbac: scan chain of role %q: %w", name, err)
		}
		role, seen := byName[roleName]
		if !seen {
			role = &Role{Name: roleName}
			byName[roleName] = role
			if parentRole != nil && *parentRole != "" {
				role.ParentRole = *parentRole
				parents[roleName] = *parentRole
			}
			if defaultScope != nil && *defaultScope != "" {
				rawScopes[roleName] = *defaultScope
			}
		}
		if permission == nil {
			continue // LEFT JOIN miss: the role holds no permissions
		}
		p, err := ParsePermission(*permission)
		if err != nil {
			return nil, fmt.Errorf("rbac: role %q permission %q: %w", roleName, *permission, err)
		}
		role.Permissions = append(role.Permissions, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rbac: iter chain of role %q: %w", name, err)
	}
	if _, ok := byName[name]; !ok {
		return nil, ErrRoleNotFound
	}

	for roleName, raw := range rawScopes {
		scope, err := ParseDefaultScope(raw)
		if err != nil {
			return nil, fmt.Errorf("rbac: role %q default_scope %q: %w", roleName, raw, err)
		}
		byName[roleName].DefaultScope = scope
	}

	names := make(map[string]struct{}, len(byName))
	for roleName := range byName {
		names[roleName] = struct{}{}
	}
	if err := validateRoleGraph(names, parents); err != nil {
		return nil, err
	}
	if err := flattenRoleGraph(byName); err != nil {
		return nil, err
	}
	return byName[name], nil
}

// assertDerivedWithinParent is the write gate for a role that names parentName
// (ADR-078(h)). childPerms / childScope are the child's rows as they would be
// stored; callerAID is the operator performing the mutation.
//
// Order of refusals is deliberate — the parent's existence first (a 404 beats a
// 403 about a role that isn't there), then caller-holds-parent, then the
// structural check. Reporting "you may not derive from this role at all" before
// "and by the way this permission is outside it" is the more actionable of the
// two.
func (s *Service) assertDerivedWithinParent(ctx context.Context, db ExecQueryRower, childName, parentName string, childPerms []string, childScope *string, callerAID string) error {
	if childName == parentName {
		// The DB says the same thing (CHECK + trigger), but reaching it would
		// mean resolving a chain that closes on itself first.
		return fmt.Errorf("%w: role %q cannot derive from itself", ErrRoleParentCycle, childName)
	}

	parent, err := resolveRoleChain(ctx, db, parentName)
	if err != nil {
		if errors.Is(err, ErrRoleNotFound) {
			return fmt.Errorf("%w: parent role %q", ErrRoleNotFound, parentName)
		}
		return err
	}

	// (h) caller holds the parent. The parent's effective rights are what the
	// child's ceiling tracks from now on, so that is what the caller must cover —
	// not merely what the child asks for today.
	parentEff := effectivePermissions(parent.Permissions, parent.DefaultScope)
	if err := s.assertCallerMayGrant(ctx, db, callerAID, parentEff); err != nil {
		return err
	}

	// (c) child ⊆ parent, resolved exactly as the enforcer will resolve it.
	own, err := parsePermissions(childPerms)
	if err != nil {
		return err
	}
	var delta *ScopeExpr
	if childScope != nil {
		if delta, err = ParseDefaultScope(*childScope); err != nil {
			return fmt.Errorf("rbac: invalid default_scope %q: %w", *childScope, err)
		}
	}
	att, err := attenuate(parent, own, delta)
	if err != nil {
		return fmt.Errorf("rbac: role %q: %w", childName, err)
	}
	if len(att.Rejected) > 0 {
		return fmt.Errorf("%w: %q does not cover %s",
			ErrRoleExceedsParent, parentName, permString(att.Rejected[0]))
	}
	return nil
}

// parsePermissions parses a role's raw permission strings. The service validates
// them before opening its transaction; this exists so the attenuation path works
// on parsed values without duplicating the loop.
func parsePermissions(raw []string) ([]Permission, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]Permission, 0, len(raw))
	for _, s := range raw {
		p, err := ParsePermission(s)
		if err != nil {
			return nil, fmt.Errorf("rbac: invalid permission %q: %w", s, err)
		}
		out = append(out, p)
	}
	return out, nil
}
