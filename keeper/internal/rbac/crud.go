package rbac

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5/pgconn"
)

// ErrRoleNotFound — a role with the given name is absent from rbac_roles.
// Returned by DeleteRole / UpdateRolePermissions on 0 affected rows, and by
// the service on a SELECT … FOR UPDATE that finds no row. A dedicated
// sentinel so transport maps "no such role" to 404, not 500.
var ErrRoleNotFound = errors.New("rbac: role not found")

// ErrRoleAlreadyExists — a UNIQUE violation (`23505`) on PK rbac_roles.name
// during CreateRole. Transport maps it to 409 (`role-already-exists`).
var ErrRoleAlreadyExists = errors.New("rbac: role already exists")

// ErrRoleBuiltin — an attempted role.delete / role.update on a role with
// builtin=true (cluster-admin). A builtin role can't be edited or deleted
// (ADR-028(b)). Transport maps it to 409 / 422.
var ErrRoleBuiltin = errors.New("rbac: role is builtin (delete/update forbidden)")

// ErrRoleOperatorNotFound — a membership row (role_name, aid) is absent
// from rbac_role_operators during RevokeOperator. Transport maps it to 404.
var ErrRoleOperatorNotFound = errors.New("rbac: role-operator membership not found")

// ErrOperatorNotFound — grant-operator on a non-existent AID: an FK
// violation (23503) on rbac_role_operators_aid_fk. A dedicated sentinel so
// transport maps "no such Archon" to 404, not 500. The service checks role
// existence via a lock BEFORE the insert ([ErrRoleNotFound]), so an FK
// violation on the grant path can only come from aid (and, theoretically,
// granted_by_aid — middleware guarantees a valid caller, but the FK
// protects anyway; both lead to this sentinel as "referenced operator not
// found").
var ErrOperatorNotFound = errors.New("rbac: operator (AID) not found")

// ErrWouldLockOutCluster — the mutation (role.delete / role.update /
// role.revoke-operator) would leave the cluster without an active Archon
// holding an effective `*` permission (the self-lockout invariant,
// ADR-028(f), rbac.md → § Built-in roles).
//
// A SEPARATE sentinel from [operator.ErrWouldLockOutCluster]: the rbac and
// operator packages must not depend on each other (avoids an import cycle
// — operator.Service already pulls in rbac.Holder's RBACSource surface).
// The transport layer maps BOTH sentinels to a single problem-type
// `would-lock-out-cluster` (409); there's no shared string between the
// packages.
var ErrWouldLockOutCluster = errors.New("rbac: would lock out cluster (no active operator with effective '*' would remain)")

// reRoleName — the role name format (rbac.md → § Storage, SQL CHECK
// rbac_roles_name_format). Duplicated in Go for application-side validation
// before a round-trip (a better error, no wasted DB call on a bad name).
var reRoleName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// RoleNamePattern — the public string form of [reRoleName] for callers in
// other packages (e.g. operator.Service on Create-with-roles, to validate
// names before opening a tx). Matches the SQL CHECK
// rbac_roles_name_format.
const RoleNamePattern = `^[a-z][a-z0-9-]*$`

// ValidRoleName checks a role name against [reRoleName]. Exported for
// callers outside the package that pre-validate before a round-trip
// (operator.Service.Create when Roles[] is present).
func ValidRoleName(name string) bool {
	return reRoleName.MatchString(name)
}

// pgErrCodeUniqueViolation / pgErrCodeForeignKeyViolation — SQLSTATE codes
// for UNIQUE / FK violations. Kept local (as in operator/crud.go) to avoid
// pulling pgerrcode into keeper.
const (
	pgErrCodeUniqueViolation     = "23505"
	pgErrCodeForeignKeyViolation = "23503"
)

const (
	// insertRoleSQL — INSERT of an rbac_roles row. builtin is always false
	// for roles created via the API: only seed migration 027
	// (cluster-admin) sets builtin=true. created_at comes from DEFAULT
	// NOW(). default_scope is nullable (ADR-047 S1): NULL = a role with no
	// scope restriction (backcompat).
	insertRoleSQL = `
INSERT INTO rbac_roles (name, description, builtin, created_by_aid, default_scope)
VALUES ($1, $2, false, $3, $4)
`

	// updateRoleDefaultScopeSQL — sets/clears a role's default_scope
	// (ADR-047 S1). NULL clears the scope (role → backcompat-unrestricted
	// for bare perms). Used by UpdateRole's replace semantics.
	updateRoleDefaultScopeSQL = `UPDATE rbac_roles SET default_scope = $2 WHERE name = $1`

	// insertRolePermissionSQL — INSERT of a single role-permission row.
	// ON CONFLICT DO NOTHING makes the batch insert idempotent on
	// duplicates within the set (a dup within one role.create isn't a DB
	// error).
	insertRolePermissionSQL = `
INSERT INTO rbac_role_permissions (role_name, permission)
VALUES ($1, $2)
ON CONFLICT (role_name, permission) DO NOTHING
`

	// deleteRoleSQL — DELETE of an rbac_roles row. CASCADE onto
	// rbac_role_permissions / rbac_role_operators wipes permissions and
	// membership in one operation.
	deleteRoleSQL = `DELETE FROM rbac_roles WHERE name = $1`

	// deleteRolePermissionsSQL — DELETE of all of a role's permission rows.
	// The first half of UpdateRolePermissions' replace semantics.
	deleteRolePermissionsSQL = `DELETE FROM rbac_role_permissions WHERE role_name = $1`

	// deleteRoleOperatorSQL — DELETE of a single membership row (role, aid).
	deleteRoleOperatorSQL = `DELETE FROM rbac_role_operators WHERE role_name = $1 AND aid = $2`

	// lockRoleForUpdateSQL — SELECT of a role row's builtin flag under a
	// row lock (FOR UPDATE) held to the end of the transaction. Used by
	// the service as the first step of every mutation (delete/update): it
	// serializes concurrent operations on the same role and reads builtin
	// for the builtin guard.
	lockRoleForUpdateSQL = `SELECT builtin FROM rbac_roles WHERE name = $1 FOR UPDATE`

	// lockRoleOperatorForUpdateSQL — row-locks a membership row (role, aid)
	// until the end of the transaction. Used by the service's
	// RevokeOperator path before the self-lockout check.
	lockRoleOperatorForUpdateSQL = `SELECT 1 FROM rbac_role_operators WHERE role_name = $1 AND aid = $2 FOR UPDATE`

	// directClusterAdminsForUpdateSQL — the DIRECT branch of the
	// self-lockout core.
	//
	// Active operators (operators.revoked_at IS NULL) with an effective `*`
	// via ANY DIRECT role (rbac_role_operators), under a row lock on three
	// tables in one tx.
	//
	// WHY the source is the DB (FOR UPDATE) and NOT enforcer.ClusterAdmins():
	//  1. The enforcer's snapshot (in-memory, ADR-028(d)) refreshes with a
	//     TTL/pub-sub delay — it goes stale between reading the snapshot
	//     and the mutation. Deciding on a stale snapshot is a staleness
	//     hole: you could remove the last admin if the snapshot still
	//     "remembers" an already-revoked second one.
	//  2. FOR UPDATE on ro/rp/o serializes concurrent lockout operations
	//     (R2/R5): two parallel tx removing `*` via different paths can't
	//     both pass the "≥1 admin remains" check — the first holds the
	//     locks until COMMIT, the second waits and sees the already-applied
	//     state.
	// Do NOT unify this back onto the enforcer snapshot — that would
	// reopen hole (1).
	//
	// NO DISTINCT: PostgreSQL forbids `FOR UPDATE` with `DISTINCT`
	// (SQLSTATE 0A000). Deduping AIDs that hold `*` via multiple roles is
	// done in Go ([dedupAIDs]); this changes neither the row lock nor the
	// resulting set.
	//
	// LOCK-ORDER INVARIANT: the self-lockout core takes locks via TWO
	// queries in a fixed order — FIRST this direct branch (ro,rp,o), THEN
	// the Synod branch ([synodClusterAdminsForUpdateSQL], so,sr,rp,o). The
	// order is the SAME at every call site (operator.Revoke + the role
	// mutations DeleteRole/UpdateRolePermissions/RevokeOperator + their
	// exclude variants in service.go) — otherwise a different lock order
	// would deadlock between concurrent lockout operations. Splitting into
	// two SELECTs (instead of one UNION) is FORCED: PostgreSQL forbids
	// `FOR UPDATE` with UNION (SQLSTATE 0A000) — the Synod join
	// (ADR-049(f)) can't be folded into one locking query with the direct
	// one.
	directClusterAdminsForUpdateSQL = `
SELECT ro.aid
FROM rbac_role_operators ro
JOIN rbac_role_permissions rp ON rp.role_name = ro.role_name
JOIN operators o ON o.aid = ro.aid
WHERE rp.permission = '*' AND o.revoked_at IS NULL
FOR UPDATE OF ro, rp, o
`

	// synodClusterAdminsForUpdateSQL — the SYNOD branch of the self-lockout
	// core (ADR-049(f)).
	//
	// Active operators with an effective `*` arriving via ANY Synod
	// (synod_operators ⋈ synod_roles → a role with `*`), under a row lock
	// on four tables. Without this branch, `*` granted through a group
	// would be invisible to self-lockout: removing the direct `*` could
	// lock out the cluster even if the last admin still holds `*` via a
	// Synod (and conversely, an admin whose only `*` is via a group must
	// not be orphaned).
	//
	// Taken as the SECOND query after [directClusterAdminsForUpdateSQL]
	// (see the lock-order invariant there). The combined set is deduped in
	// Go ([dedupAIDs]): an AID holding `*` both directly and via Synod must
	// be counted once.
	synodClusterAdminsForUpdateSQL = `
SELECT so.aid
FROM synod_operators so
JOIN synod_roles sr ON sr.synod_name = so.synod_name
JOIN rbac_role_permissions rp ON rp.role_name = sr.role_name
JOIN operators o ON o.aid = so.aid
WHERE rp.permission = '*' AND o.revoked_at IS NULL
FOR UPDATE OF so, sr, rp, o
`
)

// CreateRole creates a role and its permissions in a single transaction:
// INSERT rbac_roles + batch INSERT rbac_role_permissions.
//
// Pre-validation (defense-in-depth; the primary check is in
// [Service.CreateRole]):
//   - name against [reRoleName] (matches the SQL CHECK
//     rbac_roles_name_format);
//   - each permission via [ParsePermission] (the DB stores the RAW string
//     and doesn't validate it — the parser catches garbage before it's
//     written).
//
// db must be a transaction (`pgx.Tx`): on a batch-insert error, the
// rollback removes the already-inserted rbac_roles row. Calling this on a
// pool would leave a partially created role on failure — the caller must
// pass a tx.
//
// Errors:
//   - [ErrRoleAlreadyExists] on a UNIQUE violation of rbac_roles.name (23505).
//   - a wrapped FK violation on created_by_aid (non-existent AID).
//   - fmt.Errorf on a bad name / permission (before the round-trip).
func CreateRole(ctx context.Context, db ExecQueryRower, name, description string, permissions []string, createdByAID *string, defaultScope *string) error {
	if !reRoleName.MatchString(name) {
		return fmt.Errorf("rbac: invalid role name %q (must match %s)", name, reRoleName.String())
	}
	for _, raw := range permissions {
		if _, err := ParsePermission(raw); err != nil {
			return fmt.Errorf("rbac: invalid permission %q: %w", raw, err)
		}
	}
	if defaultScope != nil {
		if _, err := ParseDefaultScope(*defaultScope); err != nil {
			return fmt.Errorf("rbac: invalid default_scope %q: %w", *defaultScope, err)
		}
	}

	var createdBy any
	if createdByAID != nil {
		createdBy = *createdByAID
	}
	if _, err := db.Exec(ctx, insertRoleSQL, name, description, createdBy, defaultScopeArg(defaultScope)); err != nil {
		return mapRoleError(err)
	}
	for _, perm := range permissions {
		if _, err := db.Exec(ctx, insertRolePermissionSQL, name, perm); err != nil {
			return mapRoleError(err)
		}
	}
	return nil
}

// DeleteRole deletes a role; CASCADE wipes its permissions and membership.
// The builtin guard and the self-lockout check live in
// [Service.DeleteRole] (this is DELETE only).
//
// Errors:
//   - [ErrRoleNotFound] on 0 affected rows.
//   - a wrapped pgx error on a transport failure.
func DeleteRole(ctx context.Context, db ExecQueryRower, name string) error {
	tag, err := db.Exec(ctx, deleteRoleSQL, name)
	if err != nil {
		return fmt.Errorf("rbac: delete role %q: %w", name, wrapPgErr(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrRoleNotFound
	}
	return nil
}

// UpdateRolePermissions replaces a role's permission set (replace
// semantics): DELETE of all the role's permission rows + batch INSERT of
// the new set. Must be called within a transaction (`pgx.Tx`) — otherwise
// an insert failure would leave the role with an empty permission set.
//
// The role must exist: checked via the DELETE's rows-affected — 0 rows is
// ambiguous when the old set was already empty, so the existence guard
// (locking the role) is done in [Service.UpdateRolePermissions] before
// calling this function. Here, returning [ErrRoleNotFound] is unreachable
// directly (the role is already locked by the service); this function stays
// purely transport-level.
//
// Permission pre-validation is defense-in-depth (as in CreateRole).
func UpdateRolePermissions(ctx context.Context, db ExecQueryRower, name string, permissions []string) error {
	for _, raw := range permissions {
		if _, err := ParsePermission(raw); err != nil {
			return fmt.Errorf("rbac: invalid permission %q: %w", raw, err)
		}
	}
	if _, err := db.Exec(ctx, deleteRolePermissionsSQL, name); err != nil {
		return fmt.Errorf("rbac: clear permissions of role %q: %w", name, wrapPgErr(err))
	}
	for _, perm := range permissions {
		if _, err := db.Exec(ctx, insertRolePermissionSQL, name, perm); err != nil {
			return mapRoleError(err)
		}
	}
	return nil
}

// RevokeOperator removes a membership row (roleName, aid) from
// rbac_role_operators. The self-lockout check lives in
// [Service.RevokeOperator] (this is DELETE only).
//
// Errors:
//   - [ErrRoleOperatorNotFound] on 0 affected rows (the pair doesn't exist).
//   - a wrapped pgx error on a transport failure.
func RevokeOperator(ctx context.Context, db ExecQueryRower, roleName, aid string) error {
	tag, err := db.Exec(ctx, deleteRoleOperatorSQL, roleName, aid)
	if err != nil {
		return fmt.Errorf("rbac: revoke operator (%s -> %s): %w", roleName, aid, wrapPgErr(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrRoleOperatorNotFound
	}
	return nil
}

// LockEffectiveClusterAdmins returns the AIDs of active operators
// (operators.revoked_at IS NULL) with an effective `*` permission via any
// role — DIRECT (rbac_role_operators) OR via a Synod (synod_operators ⋈
// synod_roles) — taking a row lock (FOR UPDATE) on the relevant tables.
// The CORE of the self-lockout invariant (ADR-028(f) + the ADR-049(f)
// Synod join).
//
// Two locking queries in a fixed order (direct → Synod, see
// [directClusterAdminsForUpdateSQL]): PostgreSQL forbids UNION in one
// locking query (SQLSTATE 0A000), so the branches are taken as separate
// SELECTs and merged in Go ([dedupAIDs]) — an AID holding `*` via both
// paths is counted once.
//
// tx MUST be a transaction (`pgx.Tx`): FOR UPDATE outside a tx raises the
// PG error "cannot use FOR UPDATE outside transaction". The lock is held
// until COMMIT/ROLLBACK and serializes concurrent lockout operations.
//
// The source is the DB, NOT [Enforcer.ClusterAdmins] — see the comment on
// [directClusterAdminsForUpdateSQL]: the enforcer's snapshot goes stale
// within its TTL (a staleness hole), FOR UPDATE serializes the race.
func LockEffectiveClusterAdmins(ctx context.Context, tx ExecQueryRower) ([]string, error) {
	direct, err := scanAIDs(ctx, tx, directClusterAdminsForUpdateSQL)
	if err != nil {
		return nil, fmt.Errorf("rbac: lock direct cluster-admins: %w", err)
	}
	synod, err := scanAIDs(ctx, tx, synodClusterAdminsForUpdateSQL)
	if err != nil {
		return nil, fmt.Errorf("rbac: lock synod cluster-admins: %w", err)
	}
	return dedupAIDs(append(direct, synod...)), nil
}

// dedupAIDs removes duplicate AIDs (one AID holding `*` via multiple roles
// produces multiple JOIN rows). Deduping in Go replaces SQL DISTINCT, which
// is forbidden with FOR UPDATE (see [directClusterAdminsForUpdateSQL]).
func dedupAIDs(in []string) []string {
	if len(in) <= 1 {
		return in
	}
	seen := make(map[string]struct{}, len(in))
	// A new slice, NOT in-place in[:0]: deliberate safety on the auth
	// path — dedup must not mutate the caller's input slice (performance
	// doesn't matter here).
	out := make([]string, 0, len(in))
	for _, a := range in {
		if _, ok := seen[a]; ok {
			continue
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}
	return out
}

// lockRole takes a row lock on a role row (FOR UPDATE) and returns its
// builtin flag. The first step of service mutations on a role: it
// serializes concurrent delete/update and reads builtin for the builtin
// guard.
//
// tx MUST be a transaction. Returns [ErrRoleNotFound] if the row doesn't exist.
func lockRole(ctx context.Context, tx ExecQueryRower, name string) (builtin bool, err error) {
	rows, err := tx.Query(ctx, lockRoleForUpdateSQL, name)
	if err != nil {
		return false, fmt.Errorf("rbac: lock role %q: %w", name, wrapPgErr(err))
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return false, fmt.Errorf("rbac: lock role %q iter: %w", name, err)
		}
		return false, ErrRoleNotFound
	}
	if err := rows.Scan(&builtin); err != nil {
		return false, fmt.Errorf("rbac: scan role %q builtin: %w", name, err)
	}
	return builtin, nil
}

// lockRoleOperator takes a row lock on a membership row (role, aid).
// Returns [ErrRoleOperatorNotFound] if the pair doesn't exist. tx MUST be a tx.
func lockRoleOperator(ctx context.Context, tx ExecQueryRower, roleName, aid string) error {
	rows, err := tx.Query(ctx, lockRoleOperatorForUpdateSQL, roleName, aid)
	if err != nil {
		return fmt.Errorf("rbac: lock membership (%s -> %s): %w", roleName, aid, wrapPgErr(err))
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return fmt.Errorf("rbac: lock membership iter: %w", err)
		}
		return ErrRoleOperatorNotFound
	}
	return nil
}

// UpdateRoleDefaultScope replaces a role's default_scope (ADR-047 S1;
// replace semantics, NULL clears the scope). The role must exist — the
// existence guard (locking the role) is done in
// [Service.UpdateRolePermissions] before this call. Here we validate the
// scope grammar and write the UPDATE.
func UpdateRoleDefaultScope(ctx context.Context, db ExecQueryRower, name string, defaultScope *string) error {
	if defaultScope != nil {
		if _, err := ParseDefaultScope(*defaultScope); err != nil {
			return fmt.Errorf("rbac: invalid default_scope %q: %w", *defaultScope, err)
		}
	}
	if _, err := db.Exec(ctx, updateRoleDefaultScopeSQL, name, defaultScopeArg(defaultScope)); err != nil {
		return fmt.Errorf("rbac: update default_scope of role %q: %w", name, wrapPgErr(err))
	}
	return nil
}

// defaultScopeArg converts a *string default_scope into an args value: nil
// → PG NULL (a role with no scope restriction), otherwise the dereferenced
// RAW string. An empty string is treated as NULL — an "entered empty" value
// has no meaning in the coven MVP (see ResolvePurview / the S1 report).
func defaultScopeArg(scope *string) any {
	if scope == nil || *scope == "" {
		return nil
	}
	return *scope
}

// roleGivesWildcard is true if a role's set of permission strings contains
// `*`. Used by the service to decide whether a self-lockout check is
// needed (mutating a role without `*` can't lock out the cluster).
func roleGivesWildcard(permissions []string) bool {
	for _, p := range permissions {
		if p == "*" {
			return true
		}
	}
	return false
}

// rolePermissions reads a role's permission strings (no lock) — needed by
// the service to know whether the role granted `*` BEFORE update/delete. A
// separate SELECT (the role is already locked by lockRole in the same tx).
func rolePermissions(ctx context.Context, tx ExecQueryRower, name string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT permission FROM rbac_role_permissions WHERE role_name = $1`, name)
	if err != nil {
		return nil, fmt.Errorf("rbac: read permissions of role %q: %w", name, wrapPgErr(err))
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("rbac: scan permission of role %q: %w", name, err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rbac: iter permissions of role %q: %w", name, err)
	}
	return out, nil
}

// roleDefaultScope reads a role's RAW default_scope (ADR-047 S1) — needed
// by the subset check on UpdateRolePermissions without SetDefaultScope:
// newly added bare perms inherit the role's EXISTING scope (PATCH
// semantics). nil = NULL (role has no scope). The role is already locked
// by lockRole in the same tx.
func roleDefaultScope(ctx context.Context, tx ExecQueryRower, name string) (*string, error) {
	rows, err := tx.Query(ctx, `SELECT default_scope FROM rbac_roles WHERE name = $1`, name)
	if err != nil {
		return nil, fmt.Errorf("rbac: read default_scope of role %q: %w", name, wrapPgErr(err))
	}
	defer rows.Close()
	var scope *string
	if rows.Next() {
		if err := rows.Scan(&scope); err != nil {
			return nil, fmt.Errorf("rbac: scan default_scope of role %q: %w", name, err)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rbac: iter default_scope of role %q: %w", name, err)
	}
	return scope, nil
}

// mapRoleError maps pgx INSERT errors to the package's sentinels, following
// the [operator.mapInsertError] pattern:
//   - 23505 (UNIQUE) → [ErrRoleAlreadyExists] (multi-wrap: sentinel + original).
//   - 23503 (FK) → wrapped with the constraint name (created_by_aid /
//     role_name / aid reference a non-existent row).
//   - anything else → wrapped with the SQLSTATE.
func mapRoleError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeUniqueViolation:
			return fmt.Errorf("%w (constraint %s): %w", ErrRoleAlreadyExists, pgErr.ConstraintName, err)
		case pgErrCodeForeignKeyViolation:
			return fmt.Errorf("rbac: FK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("rbac: %w", wrapPgErr(err))
}

// mapGrantError maps a pgx INSERT-membership error (grant-operator path).
// The service checks the role via a lock before the insert, so an FK
// violation here means a non-existent operator (aid or granted_by_aid):
//   - 23503 (FK) → [ErrOperatorNotFound] (multi-wrap: sentinel + original).
//   - anything else → wrapped with the SQLSTATE.
func mapGrantError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgErrCodeForeignKeyViolation {
		return fmt.Errorf("%w (constraint %s): %w", ErrOperatorNotFound, pgErr.ConstraintName, err)
	}
	return fmt.Errorf("rbac: %w", wrapPgErr(err))
}
