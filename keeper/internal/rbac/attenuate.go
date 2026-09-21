package rbac

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
// The child is measured in the same currency (ADR-078(h) amendment, NIM-198):
// [effectiveRoleRights] resolves its rows against the ceiling before the floor
// compares them, so a delegator is asked to hold what the role GRANTS rather than
// what it stores. Reading the stored rows made the floor stricter than the
// authorization it protects, and the workaround for that — restating the parent's
// predicate in the delta — quietly cost the role its link to the parent.
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

// resolveParentCeiling reads the chain of parentName and enforces the
// least-privilege half of ADR-078(h): the caller must hold the PARENT's effective
// rights, not merely what the child asks for today, because the cascade carries
// every later widening of that parent into the child. Returns the parent in
// flattened form — the ceiling the child is then measured against.
//
// Order of refusals is deliberate — the parent's existence first (a 404 beats a
// 403 about a role that isn't there), then caller-holds-parent. Reporting "you
// may not derive from this role at all" before "and by the way this permission is
// outside it" is the more actionable of the two.
func (s *Service) resolveParentCeiling(ctx context.Context, db ExecQueryRower, childName, parentName, callerAID string) (*Role, error) {
	if childName == parentName {
		// The DB says the same thing (CHECK + trigger), but reaching it would
		// mean resolving a chain that closes on itself first.
		return nil, fmt.Errorf("%w: role %q cannot derive from itself", ErrRoleParentCycle, childName)
	}

	parent, err := resolveRoleChain(ctx, db, parentName)
	if err != nil {
		if errors.Is(err, ErrRoleNotFound) {
			return nil, fmt.Errorf("%w: parent role %q", ErrRoleNotFound, parentName)
		}
		return nil, err
	}

	parentEff := effectivePermissions(parent.Permissions, parent.DefaultScope)
	if err := s.assertCallerMayGrant(ctx, db, callerAID, parentEff); err != nil {
		return nil, err
	}
	return parent, nil
}

// assertWithinParent is the structural half of ADR-078(h): child ⊆ parent,
// resolved exactly as the enforcer will resolve it. parent comes from
// [Service.resolveParentCeiling]; childPerms / childScope are the child's rows as
// they would be stored.
//
// The error names EVERY row the parent fails to cover, not just the first. A
// child left inert by a narrowed parent (NIM-200) has to be repaired by rewriting
// its permission set, and an operator who is told about one offending row at a
// time cannot do that in one PATCH.
// Returns the resolved child, so a caller that needs the role's after-state — the
// cascade report of [Service.assertCascadeConfirmed] does — does not resolve it a
// second time and risk a different answer.
func assertWithinParent(parent *Role, childName string, childPerms []string, childScope *string) (*Role, error) {
	att, err := attenuateRaw(parent, childName, childPerms, childScope)
	if err != nil {
		return nil, err
	}
	if len(att.Rejected) > 0 {
		return nil, fmt.Errorf("%w: %q does not cover %s",
			ErrRoleExceedsParent, parent.Name, strings.Join(permStrings(att.Rejected), ", "))
	}
	return &Role{Name: childName, ParentRole: parent.Name, DefaultScope: att.Scope, Permissions: att.Kept}, nil
}

// pinnedDelta materializes a parent's current effective scope into a child's
// delta (ADR-078(k)): the stored predicate stops following the parent and starts
// stating its own ceiling. Returns the canonical form of `parent's scope AND
// delta`, or nil when there is nothing to write.
//
// An unrestricted parent pins to the delta unchanged, which is not a gap: pinning
// only ever protects against a WIDENING, and a parent that is already unrestricted
// has no room to widen into. Anything it later gains is a narrowing, which every
// mode passes on.
func pinnedDelta(parent *Role, delta *string) (*string, error) {
	own, err := parseScopePtr(delta)
	if err != nil {
		return nil, err
	}
	pinned := andScopes(parent.DefaultScope, own)
	if pinned == nil {
		return nil, nil
	}
	if err := checkScopeResolvable(pinned); err != nil {
		return nil, err
	}
	s := pinned.String()
	return &s, nil
}

// effectiveRoleRights resolves a role's stored rows into the rights it would
// actually grant — the form EVERY least-privilege comparison must read (NIM-198).
//
// On a plain role (parent == nil) that is ADR-047 S1 alone: a bare permission
// carries the role's own default_scope. On a DERIVED role the parent's ceiling is
// conjoined first, exactly as [attenuate] does at snapshot build.
//
// Comparing the STORED rows instead — what the floor did before NIM-198 — makes it
// judge a right the role never grants. A delegator scoped to their own coven was
// refused a child that resolves squarely inside it, and the only way past was to
// restate the parent's predicate in the delta: a role that no longer follows its
// parent, i.e. the documented contract of ADR-078(b) inverted, silently, as the
// price of getting a 201.
//
// Rows the parent does not cover are absent from the result: they resolve to
// nothing, so requiring the caller to hold them is the same over-strictness in a
// smaller place. They are refused on their own terms by [assertWithinParent],
// which every write path runs alongside this.
func effectiveRoleRights(parent *Role, rawPerms []string, rawScope *string) ([]Permission, error) {
	if len(rawPerms) == 0 {
		return nil, nil
	}
	if parent == nil {
		scope, err := parseScopePtr(rawScope)
		if err != nil {
			return nil, err
		}
		perms, err := parsePermissions(rawPerms)
		if err != nil {
			return nil, err
		}
		return effectivePermissions(perms, scope), nil
	}
	att, err := attenuateRaw(parent, "", rawPerms, rawScope)
	if err != nil {
		return nil, err
	}
	return effectivePermissions(att.Kept, att.Scope), nil
}

// attenuateRaw parses a child's stored form and resolves it against a flattened
// parent. childName only decorates the error; pass "" when there is no role name
// to blame yet.
func attenuateRaw(parent *Role, childName string, childPerms []string, childScope *string) (attenuation, error) {
	own, err := parsePermissions(childPerms)
	if err != nil {
		return attenuation{}, err
	}
	delta, err := parseScopePtr(childScope)
	if err != nil {
		return attenuation{}, err
	}
	att, err := attenuate(parent, own, delta)
	if err != nil {
		if childName == "" {
			return attenuation{}, err
		}
		return attenuation{}, fmt.Errorf("rbac: role %q: %w", childName, err)
	}
	return att, nil
}

// plainRole builds the resolved form of a role with no parent: its own rows are
// its rights, exactly as ADR-047 has always read them. The other half of
// [assertWithinParent]'s return, so a caller holds "the role as it will be"
// whether or not it derives from anything.
func plainRole(name string, rawPerms []string, rawScope *string) (*Role, error) {
	perms, err := parsePermissions(rawPerms)
	if err != nil {
		return nil, err
	}
	scope, err := parseScopePtr(rawScope)
	if err != nil {
		return nil, err
	}
	return &Role{Name: name, DefaultScope: scope, Permissions: perms}, nil
}

// checkScopeMode rejects a mode that is not one of the three, or one set on a
// role with no parent — where it would describe a relationship that does not
// exist. The DB CHECKs the same pair (migration 105); this is the readable half.
func checkScopeMode(mode ScopeMode, derived bool) error {
	if !mode.Valid() {
		return fmt.Errorf("%w: %q (want track or pin)", ErrInvalidScopeMode, string(mode))
	}
	if mode != ScopeModeNone && !derived {
		return fmt.Errorf("%w: %q needs a parent_role — a plain role's default_scope is absolute, not a delta",
			ErrInvalidScopeMode, string(mode))
	}
	return nil
}

// parseScopePtr parses an optional raw default_scope; nil stays nil (the
// unrestricted top).
func parseScopePtr(raw *string) (*ScopeExpr, error) {
	if raw == nil {
		return nil, nil
	}
	scope, err := ParseDefaultScope(*raw)
	if err != nil {
		return nil, fmt.Errorf("rbac: invalid default_scope %q: %w", *raw, err)
	}
	return scope, nil
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
