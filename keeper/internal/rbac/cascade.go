package rbac

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// The blast radius of editing a parent role (ADR-078(k), NIM-199).
//
// Cascade is the feature: narrow a parent and every role derived from it narrows
// with it, at the next snapshot build, with nothing rewritten by hand
// (ADR-078(d)). What was missing is that it ran SILENTLY. An operator adding
// `coven=web` to a role saw a successful PATCH; what they had actually done was
// hand every derived role, and everyone holding one, a coven they had never been
// shown. Nothing in the response or the audit record said so.
//
// So a mutation that changes what the roles BELOW it grant is refused until the
// caller says they meant it, and the refusal names what would move: which derived
// roles gain rights, which lose them, and how many active operators hold any of
// them. The shape is [ConfirmDestroy]'s (NIM-191): the dangerous step is not
// forbidden, it is made deliberate, and the thing the operator needs in order to
// decide travels in the refusal rather than in a doc.
//
// Both directions are reported, and neither is the safe one:
//
//	widened   somebody gains access they were never granted directly. The
//	          escalation case, and the one that motivated the ticket.
//	narrowed  somebody loses access, possibly mid-incident. Cheaper to undo,
//	          just as surprising to run into.
//
// The edited role is normally NOT part of its own blast radius: the operator sent
// its permission list, so whatever it now grants is what they typed. One case
// breaks that (NIM-252) — the role STOPS TRACKING, i.e. this PATCH takes its
// parent away. Then the rows can be identical in the request and the rights still
// move, because the ceiling they resolved under is gone; the holders of that role
// gain access nobody granted them, and the operator has no way to know how many
// they are. That is the same surprise this file exists for, one level up, so the
// role joins its own report exactly there and nowhere else.
//
// Nothing here participates in a permission decision. The impact report is
// computed from the same [attenuate] the enforcer runs, so it cannot disagree
// with what will happen — but if it were skipped entirely, the resulting rights
// would be exactly the same. It is a gate on the operator's attention, not on the
// boundary.

// ErrRoleCascadeNeedsConfirm — a role mutation would change the rights of roles
// derived from it, and the caller has not confirmed the cascade. Transport maps
// it to 409, alongside [ErrRoleHasChildren]: both are "this role is a parent, and
// that has consequences you have to take a position on".
var ErrRoleCascadeNeedsConfirm = errors.New("rbac: mutation cascades into derived roles (confirmation required)")

// RoleCascade is what a pending mutation does to the roles it moves: the ones
// that would gain rights, the ones that would lose them, and the number of
// distinct active operators holding any of them (directly or through a Synod).
// Names are sorted, so a report reads the same twice.
//
// Normally every name here is a role DERIVED from the edited one. The edited role
// appears only when this PATCH takes its parent away (NIM-252) — see the file
// header for why that one case counts as a consequence rather than a request.
//
// A role appears in BOTH lists when a change trades one right for another — a
// scope moved sideways from `coven=dba` to `coven=web` narrows on the first term
// and widens on the second.
type RoleCascade struct {
	Widened   []string
	Narrowed  []string
	Operators int
}

// Empty reports whether the mutation leaves every derived role exactly as it was.
// An edit to a parent that its children do not feel needs no confirmation: the
// role graph is not the point, the rights are.
func (c RoleCascade) Empty() bool { return len(c.Widened) == 0 && len(c.Narrowed) == 0 }

// String renders the report for the refusal's message. The transport carries no
// extension members (RFC 7807 problem bodies here are fixed-shape), so this text
// IS the machine-readable answer, and it names every affected role rather than
// summarizing — an operator deciding whether to confirm needs the list, not a
// count.
func (c RoleCascade) String() string {
	var parts []string
	if len(c.Widened) > 0 {
		parts = append(parts, fmt.Sprintf("widens %d role(s): %s",
			len(c.Widened), strings.Join(c.Widened, ", ")))
	}
	if len(c.Narrowed) > 0 {
		parts = append(parts, fmt.Sprintf("narrows %d role(s): %s",
			len(c.Narrowed), strings.Join(c.Narrowed, ", ")))
	}
	return fmt.Sprintf("%s; %d operator(s) hold them", strings.Join(parts, ", "), c.Operators)
}

// CascadeError carries the report of a refused-pending-confirmation mutation, so
// a caller can show the operator what they are about to do. Matches
// [ErrRoleCascadeNeedsConfirm] under errors.Is.
type CascadeError struct{ Cascade RoleCascade }

func (e *CascadeError) Error() string {
	return fmt.Sprintf("%s: %s", ErrRoleCascadeNeedsConfirm.Error(), e.Cascade.String())
}
func (e *CascadeError) Is(target error) bool { return target == ErrRoleCascadeNeedsConfirm }

// selectRoleSubtreeSQL — the roles DERIVED from $1, transitively, with each one's
// default_scope and permission rows. The mirror image of [selectRoleChainSQL]:
// that one walks UP the parent edges from a role, this one walks DOWN.
//
// `lvl` orders the result by distance from the root, which is what makes
// resolution a single pass — a role's parent is always already resolved when its
// own turn comes. The bound is the same belt-and-braces one as the upward walk: a
// subtree corrupted out-of-band must make the query terminate, not spin.
//
// The root itself is excluded. Its resolved form is the INPUT to the walk, and it
// exists in two versions here (before the mutation and after), neither of which
// is in the table yet.
const selectRoleSubtreeSQL = `
WITH RECURSIVE sub(name, parent_role, default_scope, lvl) AS (
    SELECT r.name, r.parent_role, r.default_scope, 1
      FROM rbac_roles r
     WHERE r.parent_role = $1
    UNION ALL
    SELECT r.name, r.parent_role, r.default_scope, s.lvl + 1
      FROM rbac_roles r
      JOIN sub s ON r.parent_role = s.name
     WHERE s.lvl <= $2
)
SELECT s.name, s.parent_role, s.default_scope, rp.permission, s.lvl
FROM sub s
LEFT JOIN rbac_role_permissions rp ON rp.role_name = s.name
ORDER BY s.lvl, s.name
`

// selectCascadeOperatorsSQL — how many distinct ACTIVE operators hold any of the
// affected roles, directly or through a Synod (ADR-049(f)). The number an
// operator actually weighs when deciding whether to confirm: "three roles" means
// little, "three roles, forty people" means a great deal.
const selectCascadeOperatorsSQL = `
SELECT count(*) FROM (
    SELECT ro.aid
      FROM rbac_role_operators ro
      JOIN operators o ON o.aid = ro.aid
     WHERE ro.role_name = ANY($1) AND o.revoked_at IS NULL
    UNION
    SELECT so.aid
      FROM synod_roles sr
      JOIN synod_operators so ON so.synod_name = sr.synod_name
      JOIN operators o ON o.aid = so.aid
     WHERE sr.role_name = ANY($1) AND o.revoked_at IS NULL
) held
`

// subtreeRole is one derived role as STORED — its own rows, before any
// resolution. Kept separate from [Role] because the impact report resolves the
// same subtree twice, against two different versions of the root, and flattening
// mutates.
type subtreeRole struct {
	Name         string
	ParentRole   string
	DefaultScope *ScopeExpr
	Permissions  []Permission
}

// role materializes a fresh [Role] for one resolution pass.
func (r subtreeRole) role() *Role {
	perms := make([]Permission, len(r.Permissions))
	copy(perms, r.Permissions)
	return &Role{Name: r.Name, ParentRole: r.ParentRole, DefaultScope: r.DefaultScope, Permissions: perms}
}

// loadRoleSubtree reads every role derived from name, transitively, in order of
// distance from it. Returns nil when the role has no children — the common case,
// and the one that skips the whole report.
func loadRoleSubtree(ctx context.Context, db ExecQueryRower, name string) ([]subtreeRole, error) {
	rows, err := db.Query(ctx, selectRoleSubtreeSQL, name, maxRoleChainDepth)
	if err != nil {
		return nil, fmt.Errorf("rbac: read subtree of role %q: %w", name, wrapPgErr(err))
	}
	defer rows.Close()

	var order []string
	byName := make(map[string]*subtreeRole)
	rawScopes := make(map[string]string)
	for rows.Next() {
		var (
			roleName     string
			parentRole   *string
			defaultScope *string
			permission   *string
			lvl          int
		)
		if err := rows.Scan(&roleName, &parentRole, &defaultScope, &permission, &lvl); err != nil {
			return nil, fmt.Errorf("rbac: scan subtree of role %q: %w", name, err)
		}
		r, seen := byName[roleName]
		if !seen {
			r = &subtreeRole{Name: roleName}
			byName[roleName] = r
			order = append(order, roleName)
			if parentRole != nil {
				r.ParentRole = *parentRole
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
		r.Permissions = append(r.Permissions, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rbac: iter subtree of role %q: %w", name, err)
	}
	for roleName, raw := range rawScopes {
		scope, err := ParseDefaultScope(raw)
		if err != nil {
			return nil, fmt.Errorf("rbac: role %q default_scope %q: %w", roleName, raw, err)
		}
		byName[roleName].DefaultScope = scope
	}

	out := make([]subtreeRole, 0, len(order))
	for _, n := range order {
		out = append(out, *byName[n])
	}
	return out, nil
}

// resolveSubtree resolves a subtree against a given version of its root and
// returns each role's EFFECTIVE permissions, keyed by name.
//
// Single pass: [loadRoleSubtree] returns roles ordered by distance from the root,
// so every role's parent is resolved before its own turn. A role whose parent is
// somehow absent is skipped rather than treated as a root — resolving it against
// the unrestricted top would report a widening that will not happen.
func resolveSubtree(root *Role, subtree []subtreeRole) (map[string][]Permission, error) {
	resolved := map[string]*Role{root.Name: root}
	out := make(map[string][]Permission, len(subtree))
	for _, raw := range subtree {
		parent, known := resolved[raw.ParentRole]
		if !known {
			continue
		}
		r := raw.role()
		att, err := attenuate(parent, r.Permissions, r.DefaultScope)
		if err != nil {
			return nil, fmt.Errorf("rbac: role %q: %w", r.Name, err)
		}
		r.Permissions, r.DefaultScope = att.Kept, att.Scope
		resolved[r.Name] = r
		out[r.Name] = effectivePermissions(r.Permissions, r.DefaultScope)
	}
	return out, nil
}

// cascadeImpact compares the subtree resolved against the root as it is with the
// subtree resolved against the root as it would be, and reports which derived
// roles move in which direction.
//
// Comparison is by COVERAGE, not by string: a right is gained when the old form
// does not cover it ([callerHolds], the one containment predicate in the
// codebase). Comparing rendered strings would report a role as changed whenever a
// scope re-canonicalized, and miss a genuine widening hidden behind an equal
// rendering.
func cascadeImpact(before, after *Role, subtree []subtreeRole) (RoleCascade, error) {
	was, err := resolveSubtree(before, subtree)
	if err != nil {
		return RoleCascade{}, err
	}
	now, err := resolveSubtree(after, subtree)
	if err != nil {
		return RoleCascade{}, err
	}

	var c RoleCascade
	// The edited role, but only when this PATCH takes its parent away (NIM-252).
	// Both directions are checked rather than just the widening: un-parenting alone
	// can only widen — the ceiling is dropped, never added — but the same request
	// may trim rows at the same time, and an operator confirming one consequence
	// should be shown the other.
	if before.ParentRole != "" && after.ParentRole == "" {
		wasEff := effectivePermissions(before.Permissions, before.DefaultScope)
		nowEff := effectivePermissions(after.Permissions, after.DefaultScope)
		if uncovered(wasEff, nowEff) {
			c.Widened = append(c.Widened, before.Name)
		}
		if uncovered(nowEff, wasEff) {
			c.Narrowed = append(c.Narrowed, before.Name)
		}
	}
	for name, nowPerms := range now {
		wasPerms := was[name]
		if uncovered(wasPerms, nowPerms) {
			c.Widened = append(c.Widened, name)
		}
		if uncovered(nowPerms, wasPerms) {
			c.Narrowed = append(c.Narrowed, name)
		}
	}
	sort.Strings(c.Widened)
	sort.Strings(c.Narrowed)
	return c, nil
}

// uncovered reports whether any permission in want falls outside have — the
// boolean form of [widenedRights], so the impact report and the write gates
// cannot disagree about what "gained a right" means.
func uncovered(have, want []Permission) bool { return len(widenedRights(have, want)) > 0 }

// countCascadeOperators counts the distinct active operators holding any of the
// affected roles. Zero is a legitimate answer — a derived role nobody holds yet
// still changes, and the operator should still be told which.
func countCascadeOperators(ctx context.Context, db ExecQueryRower, c RoleCascade) (int, error) {
	names := make([]string, 0, len(c.Widened)+len(c.Narrowed))
	seen := make(map[string]struct{}, cap(names))
	for _, n := range append(append([]string{}, c.Widened...), c.Narrowed...) {
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		names = append(names, n)
	}
	if len(names) == 0 {
		return 0, nil
	}
	rows, err := db.Query(ctx, selectCascadeOperatorsSQL, names)
	if err != nil {
		return 0, fmt.Errorf("rbac: count operators of cascaded roles: %w", wrapPgErr(err))
	}
	defer rows.Close()
	var n int
	if rows.Next() {
		if err := rows.Scan(&n); err != nil {
			return 0, fmt.Errorf("rbac: scan operator count of cascaded roles: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("rbac: iter operator count of cascaded roles: %w", err)
	}
	return n, nil
}

// assertCascadeConfirmed is the gate itself: resolve the subtree both ways, and
// refuse with the report unless the caller has already said yes.
//
// before is the role as stored (already flattened); after is the role as the
// pending mutation leaves it. A role with no children costs one indexed query —
// the partial index of migration 102 serves exactly this lookup — and then an
// empty walk. It no longer short-circuits on that emptiness: a role losing its
// parent is a consequence of its own, and childlessness says nothing about it
// (NIM-252).
func (s *Service) assertCascadeConfirmed(ctx context.Context, db ExecQueryRower, before, after *Role, confirmed bool) error {
	subtree, err := loadRoleSubtree(ctx, db, before.Name)
	if err != nil {
		return err
	}
	c, err := cascadeImpact(before, after, subtree)
	if err != nil {
		return err
	}
	if c.Empty() || confirmed {
		return nil
	}
	if c.Operators, err = countCascadeOperators(ctx, db, c); err != nil {
		return err
	}
	return &CascadeError{Cascade: c}
}
