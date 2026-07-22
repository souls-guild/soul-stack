package rbac

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// CreateSynodInput holds the parameters for CreateSynod.
type CreateSynodInput struct {
	Name        string
	Description string
	CallerAID   string
}

// CreateSynod creates an empty group. Least-privilege/self-lockout don't
// apply to creation: an empty group grants no permissions (roles are added
// to the bundle separately via GrantRole under a subset check). Symmetric
// with [Service.CreateRole].
//
// Returns:
//   - [ErrInvalidSynodName] — name doesn't match the format (422).
//   - [ErrSynodAlreadyExists] — name already taken (409).
//   - wrapped FK-violation — CallerAID doesn't exist in operators.
func (s *Service) CreateSynod(ctx context.Context, in CreateSynodInput) error {
	if !reRoleName.MatchString(in.Name) {
		return fmt.Errorf("%w: %q must match %s", ErrInvalidSynodName, in.Name, reRoleName.String())
	}
	var createdBy *string
	if in.CallerAID != "" {
		createdBy = &in.CallerAID
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("rbac: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := CreateSynod(ctx, tx, in.Name, in.Description, createdBy); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("rbac: commit tx: %w", err)
	}
	s.invalidate(ctx)
	return nil
}

// DeleteSynod deletes a group (cascading membership + bundle).
//
// Order within the tx (deterministic lock order: group → its roles → admin set):
//  1. lock the group row; missing → [ErrSynodNotFound].
//  2. builtin=true → [ErrSynodBuiltin] (FIRST, before lockout — builtin takes
//     priority).
//  3. if the group bundles a `*`-granting role — self-lockout: the group's
//     disappearance must not orphan the last admin whose `*` is held through
//     it (ADR-049(f)). Empty → [ErrWouldLockOutCluster].
//  4. DELETE.
func (s *Service) DeleteSynod(ctx context.Context, name string) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("rbac: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	builtin, err := lockSynod(ctx, tx, name)
	if err != nil {
		return err
	}
	if builtin {
		return ErrSynodBuiltin
	}

	// `*` via the group might be its members' last path to admin — check only
	// if the group actually bundles a `*`-granting role (otherwise lockout is
	// impossible and the extra query is unnecessary).
	wildcard, err := s.synodGivesWildcard(ctx, tx, name)
	if err != nil {
		return err
	}
	if wildcard {
		if err := s.assertNotLastWildcardSynod(ctx, tx, name); err != nil {
			return err
		}
	}

	if err := DeleteSynod(ctx, tx, name); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("rbac: commit tx: %w", err)
	}
	s.invalidate(ctx)
	return nil
}

// UpdateSynodDescription updates ONLY the group's description (ADR-049
// amend). name (PK) is immutable — renaming is deliberately rejected
// (audit drift + an enforcer-snapshot desync window + asymmetry with
// rbac_roles.name).
//
// No tx/lock/subset/self-lockout: it's a single UPDATE row, description
// neither grants nor takes away permissions, so self-lockout is impossible
// and least-privilege doesn't apply. builtin is ALLOWED (description is
// cosmetic, not behavioral). invalidate is NOT triggered: the enforcer
// snapshot ([loadSynodMembership]/[loadSynodRoles]) only carries
// name/roles/membership — description doesn't enter matching, so
// authorization is unaffected by editing it.
//
// Returns [ErrSynodNotFound] on 0 rows affected (group doesn't exist).
func (s *Service) UpdateSynodDescription(ctx context.Context, name, description string) error {
	tag, err := s.pool.Exec(ctx, updateSynodDescriptionSQL, name, description)
	if err != nil {
		return fmt.Errorf("rbac: update synod %q description: %w", name, wrapPgErr(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrSynodNotFound
	}
	return nil
}

// AddOperatorInput holds the parameters for AddOperator.
type AddOperatorInput struct {
	SynodName string
	AID       string
	CallerAID string
}

// AddOperator adds an archon to a group (synod_operators). Idempotent.
//
// SECURITY (ADR-049(f)): subject to a least-privilege subset check. A group
// member gets its ENTIRE role bundle — the caller may not add an archon to
// the group unless it itself holds all effective permissions of that bundle
// (otherwise a bypass: a cluster-admin builds a group with a powerful role,
// a sub-operator with synod.add-operator adds itself/others and escalates).
// Checks the effective permissions of ALL of the group's roles under their
// default_scope (same as role.grant-operator checks the granted role's
// permissions).
//
// Order within the tx (deterministic: group → admin-set/subset):
//  1. lock the group; missing → [ErrSynodNotFound].
//  2. subset check against the group bundle's effective permissions →
//     [ErrPermissionNotHeld].
//  3. INSERT the membership row; FK on a nonexistent AID →
//     [ErrOperatorNotFound].
//
// NO self-lockout: add only expands the admin set (the member gains roles)
// — it cannot lock the cluster (symmetric with role.grant-operator).
func (s *Service) AddOperator(ctx context.Context, in AddOperatorInput) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("rbac: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := lockSynod(ctx, tx, in.SynodName); err != nil {
		return err
	}

	// Least-privilege: effective permissions of ALL of the group's roles.
	// CallerAID always carries the subject (transport is claims.Subject); an
	// empty caller with a non-empty required set is rejected in
	// assertCallerMayGrant ([ErrPermissionNotHeld]).
	required, err := s.synodEffectivePermissions(ctx, tx, in.SynodName)
	if err != nil {
		return err
	}
	if err := s.assertCallerMayGrant(ctx, tx, in.CallerAID, required); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, insertSynodOperatorSQL, in.SynodName, in.AID, callerArg(in.CallerAID)); err != nil {
		return mapSynodMemberError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("rbac: commit tx: %w", err)
	}
	s.invalidate(ctx)
	return nil
}

// RemoveOperatorInput holds the parameters for RemoveOperator.
type RemoveOperatorInput struct {
	SynodName string
	AID       string
}

// RemoveOperator removes an archon from a group (synod_operators).
//
// SECURITY (ADR-049(f)): self-lockout. Removing an archon from a group takes
// away its group roles (including any `*`-granting one) — this can orphan
// the last admin whose `*` is held ONLY through this group.
//
// Order within the tx (deterministic: membership → group → admin-set):
//  1. lock the membership row; missing → [ErrSynodOperatorNotFound].
//  2. if the group bundles `*` — self-lockout, excluding the (synod, aid)
//     pair from the Synod branch (excludeAID might still hold `*` directly
//     or through another group — it survives); empty →
//     [ErrWouldLockOutCluster].
//  3. DELETE.
func (s *Service) RemoveOperator(ctx context.Context, in RemoveOperatorInput) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("rbac: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockSynodOperator(ctx, tx, in.SynodName, in.AID); err != nil {
		return err
	}
	if _, err := lockSynod(ctx, tx, in.SynodName); err != nil {
		return err
	}

	wildcard, err := s.synodGivesWildcard(ctx, tx, in.SynodName)
	if err != nil {
		return err
	}
	if wildcard {
		if err := s.assertNotLastWildcardSynodOperator(ctx, tx, in.SynodName, in.AID); err != nil {
			return err
		}
	}

	tag, err := tx.Exec(ctx, deleteSynodOperatorSQL, in.SynodName, in.AID)
	if err != nil {
		return fmt.Errorf("rbac: remove operator (%s -> %s): %w", in.SynodName, in.AID, wrapPgErr(err))
	}
	if tag.RowsAffected() == 0 {
		// The lock above already guaranteed the pair exists; this guards against a race.
		return ErrSynodOperatorNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("rbac: commit tx: %w", err)
	}
	s.invalidate(ctx)
	return nil
}

// GrantRoleInput holds the parameters for GrantRole.
type GrantRoleInput struct {
	SynodName string
	RoleName  string
	CallerAID string
}

// GrantRole adds a role to a group's bundle (synod_roles). Idempotent.
//
// SECURITY (ADR-049(f)): subject to a least-privilege subset check. Adding a
// role to a group grants ALL of its members the role's effective
// permissions — the caller may not add a role whose permissions exceed its
// own set (otherwise a bypass: a sub-operator with synod.grant-role bundles
// a cluster-admin role into a group it belongs to and escalates). Checks the
// granted role's effective permissions under its default_scope (same as
// role.grant-operator).
//
// Order within the tx (deterministic: group → role → subset):
//  1. lock the group; missing → [ErrSynodNotFound].
//  2. subset check against the role's effective permissions →
//     [ErrPermissionNotHeld].
//  3. INSERT the bundle row; FK on a nonexistent role → [ErrRoleNotFound].
//
// NO self-lockout: grant only expands the admin set.
func (s *Service) GrantRole(ctx context.Context, in GrantRoleInput) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("rbac: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := lockSynod(ctx, tx, in.SynodName); err != nil {
		return err
	}

	// Effective permissions of the granted role under its default_scope. The
	// role might not exist — then rolePermissions returns an empty set,
	// subset passes (nothing to check), and the INSERT fails with an
	// FK-violation → ErrRoleNotFound. The order is correct: a nonexistent
	// role is a 404, not a false subset-pass granting permissions that don't
	// exist.
	required, err := s.roleEffectivePermissions(ctx, tx, in.RoleName)
	if err != nil {
		return err
	}
	if err := s.assertCallerMayGrant(ctx, tx, in.CallerAID, required); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, insertSynodRoleSQL, in.SynodName, in.RoleName, callerArg(in.CallerAID)); err != nil {
		return mapSynodMemberError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("rbac: commit tx: %w", err)
	}
	s.invalidate(ctx)
	return nil
}

// RevokeRoleInput holds the parameters for RevokeRole.
type RevokeRoleInput struct {
	SynodName string
	RoleName  string
}

// RevokeRole removes a role from a group's bundle (synod_roles).
//
// SECURITY (ADR-049(f)): self-lockout. Removing a role from a group takes
// away its permissions from ALL group members — if it was the group's last
// `*`-granting role and some member held `*` ONLY through it, the cluster
// locks out.
//
// Order within the tx (deterministic: bundle row → admin-set):
//  1. lock the bundle row; missing → [ErrSynodRoleNotFound].
//  2. if the revoked role grants `*` — self-lockout, excluding the
//     (synod, role) pair from the Synod branch; empty →
//     [ErrWouldLockOutCluster].
//  3. DELETE.
func (s *Service) RevokeRole(ctx context.Context, in RevokeRoleInput) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("rbac: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockSynodRole(ctx, tx, in.SynodName, in.RoleName); err != nil {
		return err
	}

	// Self-lockout is only needed if the revoked role grants `*`: otherwise
	// removing it doesn't shrink the admin set.
	perms, err := rolePermissions(ctx, tx, in.RoleName)
	if err != nil {
		return err
	}
	if roleGivesWildcard(perms) {
		if err := s.assertNotLastWildcardSynodRole(ctx, tx, in.SynodName, in.RoleName); err != nil {
			return err
		}
	}

	tag, err := tx.Exec(ctx, deleteSynodRoleSQL, in.SynodName, in.RoleName)
	if err != nil {
		return fmt.Errorf("rbac: revoke role (%s -> %s): %w", in.SynodName, in.RoleName, wrapPgErr(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrSynodRoleNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("rbac: commit tx: %w", err)
	}
	s.invalidate(ctx)
	return nil
}

// ListSynods returns the API catalog of groups (name / description / builtin
// + expanded roles and member AIDs). Read-only, no tx ([LoadSynodViews]).
func (s *Service) ListSynods(ctx context.Context) ([]SynodView, error) {
	return LoadSynodViews(ctx, s.pool)
}

// synodGivesWildcard reports whether at least one role in the group's bundle
// grants `*`. Self-lockout paths check this to avoid running an admin-set
// probe for nothing when the group doesn't bundle `*` (lockout is
// impossible).
func (s *Service) synodGivesWildcard(ctx context.Context, tx ExecQueryRower, name string) (bool, error) {
	rows, err := tx.Query(ctx, `
SELECT 1
FROM synod_roles sr
JOIN rbac_role_permissions rp ON rp.role_name = sr.role_name
WHERE sr.synod_name = $1 AND rp.permission = '*'
LIMIT 1`, name)
	if err != nil {
		return false, fmt.Errorf("rbac: probe synod %q wildcard: %w", name, wrapPgErr(err))
	}
	defer rows.Close()
	has := rows.Next()
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("rbac: iter synod %q wildcard: %w", name, err)
	}
	return has, nil
}

// synodEffectivePermissions returns the combined effective permissions of
// ALL of the group's roles (each bare permission is expanded under its OWN
// role's default_scope, ADR-047 S1). Needed by add-operator's subset check:
// a member gets the entire bundle, so the caller must hold all of it. A
// permission duplicated across two roles is idempotent (assertCallerCovers
// checks each one separately).
func (s *Service) synodEffectivePermissions(ctx context.Context, tx ExecQueryRower, synodName string) ([]Permission, error) {
	roles, err := synodRoles(ctx, tx, synodName)
	if err != nil {
		return nil, err
	}
	var out []Permission
	for _, r := range roles {
		eff, err := s.roleEffectivePermissions(ctx, tx, r)
		if err != nil {
			return nil, err
		}
		out = append(out, eff...)
	}
	return out, nil
}

// roleEffectivePermissions returns a role's effective permissions (bare
// expanded under its default_scope). Shared helper for the grant-role /
// add-operator subset check: exactly the same expansion
// [Service.GrantOperator] does for the granted role.
func (s *Service) roleEffectivePermissions(ctx context.Context, tx ExecQueryRower, roleName string) ([]Permission, error) {
	perms, err := rolePermissions(ctx, tx, roleName)
	if err != nil {
		return nil, err
	}
	scope, err := roleDefaultScope(ctx, tx, roleName)
	if err != nil {
		return nil, err
	}
	return requiredPermissions(perms, scope)
}

// callerArg converts a CallerAID string into the added_by_aid/granted_by_aid
// arg value: empty → PG NULL (bootstrap/seed with no initiating archon),
// otherwise the string.
func callerArg(callerAID string) any {
	if callerAID == "" {
		return nil
	}
	return callerAID
}
