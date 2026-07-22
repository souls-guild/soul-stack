package rbac

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5/pgconn"
)

// ErrSynodNotFound means no group with the given name exists in synods.
// Returned when SELECT … FOR UPDATE / DELETE finds no row. Transport maps
// "no such group" to 404 (symmetric with [ErrRoleNotFound]).
var ErrSynodNotFound = errors.New("rbac: synod not found")

// ErrSynodAlreadyExists signals a UNIQUE violation (`23505`) on the
// synods.name PK during CreateSynod. Transport maps it to 409 (symmetric
// with [ErrRoleAlreadyExists]).
var ErrSynodAlreadyExists = errors.New("rbac: synod already exists")

// ErrInvalidSynodName means the group name fails [reSynodName]. A
// validation-error sentinel (transport maps it to 422, symmetric with
// [ErrInvalidRoleName]).
var ErrInvalidSynodName = errors.New("rbac: invalid synod name")

// ErrSynodBuiltin signals a synod.delete attempt on a group with
// builtin=true. A builtin group cannot be deleted (ADR-049(g), symmetric
// with [ErrRoleBuiltin]). Transport maps it to 409.
var ErrSynodBuiltin = errors.New("rbac: synod is builtin (delete forbidden)")

// ErrSynodOperatorNotFound means the membership row (synod_name, aid) is
// missing from synod_operators during RemoveOperator. Transport maps it to
// 404.
var ErrSynodOperatorNotFound = errors.New("rbac: synod-operator membership not found")

// ErrSynodRoleNotFound means the bundle row (synod_name, role_name) is
// missing from synod_roles during RevokeRole. Transport maps it to 404.
var ErrSynodRoleNotFound = errors.New("rbac: synod-role bundle entry not found")

// SynodDescriptionMaxLen caps a group's description length (guards against
// bloating the UI/audit payload). The single source of truth for BOTH write
// paths: the HTTP handler (PATCH /v1/synods/{name}) and the MCP tool
// (keeper.synod.update) both reference it, so exceeding it produces the same
// validation-failed (422). The OpenAPI schema carries the same maxLength —
// belt-and-suspenders (spec + transport). Exceeding it →
// [problem.TypeValidationFailed] / mcpCodeValidationFailed.
const SynodDescriptionMaxLen = 1024

// reSynodName matches the SQL CHECK synods_name_format (migration 069) and
// [reRoleName] — kebab-case. Duplicated in Go for pre-round-trip validation.
// Reuses the single [reRoleName] (identical format); no separate regexp is
// defined, to avoid two sources for the same pattern.

const (
	// insertSynodSQL inserts a synods row. builtin is always false for
	// groups created through the API (only a seed migration, if one ever
	// appears, would set builtin=true). created_at comes from DEFAULT NOW().
	insertSynodSQL = `
INSERT INTO synods (name, description, builtin, created_by_aid)
VALUES ($1, $2, false, $3)
`

	// updateSynodDescriptionSQL edits ONLY the group's description (ADR-049
	// amend). name (PK) is immutable, so it only appears in WHERE and is
	// never changed. builtin/roles/membership are untouched — description is
	// cosmetic and grants no rights.
	updateSynodDescriptionSQL = `UPDATE synods SET description = $2 WHERE name = $1`

	// deleteSynodSQL deletes a synods row. CASCADE on synod_operators /
	// synod_roles removes membership and the bundle in one operation.
	deleteSynodSQL = `DELETE FROM synods WHERE name = $1`

	// lockSynodForUpdateSQL selects a group row's builtin flag under a row
	// lock (FOR UPDATE) for the rest of the tx. The first step of any
	// mutation on a group: serializes concurrent operations and reads
	// builtin for the builtin boundary check.
	lockSynodForUpdateSQL = `SELECT builtin FROM synods WHERE name = $1 FOR UPDATE`

	// insertSynodOperatorSQL inserts a membership row (synod_name, aid).
	// ON CONFLICT DO NOTHING makes the grant idempotent (re-adding an archon
	// to a group is a no-op, symmetric with insertRoleOperatorSQL).
	insertSynodOperatorSQL = `
INSERT INTO synod_operators (synod_name, aid, added_by_aid)
VALUES ($1, $2, $3)
ON CONFLICT (synod_name, aid) DO NOTHING
`

	// deleteSynodOperatorSQL deletes a single membership row (synod, aid).
	deleteSynodOperatorSQL = `DELETE FROM synod_operators WHERE synod_name = $1 AND aid = $2`

	// insertSynodRoleSQL inserts a bundle row (synod_name, role_name).
	// ON CONFLICT DO NOTHING — re-granting a role to a group is a no-op.
	insertSynodRoleSQL = `
INSERT INTO synod_roles (synod_name, role_name, granted_by_aid)
VALUES ($1, $2, $3)
ON CONFLICT (synod_name, role_name) DO NOTHING
`

	// deleteSynodRoleSQL deletes a single bundle row (synod, role).
	deleteSynodRoleSQL = `DELETE FROM synod_roles WHERE synod_name = $1 AND role_name = $2`

	// lockSynodRoleForUpdateSQL row-locks a bundle row (synod, role) for the
	// rest of the tx. Used by the RevokeRole path before the self-lockout
	// check.
	lockSynodRoleForUpdateSQL = `SELECT 1 FROM synod_roles WHERE synod_name = $1 AND role_name = $2 FOR UPDATE`

	// lockSynodOperatorForUpdateSQL row-locks a membership row (synod, aid)
	// for the rest of the tx. Used by the RemoveOperator path before
	// self-lockout.
	lockSynodOperatorForUpdateSQL = `SELECT 1 FROM synod_operators WHERE synod_name = $1 AND aid = $2 FOR UPDATE`
)

// CreateSynod creates a group. The builtin boundary and self-lockout don't
// apply to create (creating an empty group grants no rights). db must be a
// tx/pool.
//
// Errors:
//   - [ErrSynodAlreadyExists] on a UNIQUE violation of synods.name (23505).
//   - [ErrInvalidSynodName] on a malformed name (before the round trip).
//   - a wrapped FK violation on created_by_aid (nonexistent AID).
func CreateSynod(ctx context.Context, db ExecQueryRower, name, description string, createdByAID *string) error {
	if !reRoleName.MatchString(name) {
		return fmt.Errorf("%w: %q must match %s", ErrInvalidSynodName, name, reRoleName.String())
	}
	var createdBy any
	if createdByAID != nil {
		createdBy = *createdByAID
	}
	if _, err := db.Exec(ctx, insertSynodSQL, name, description, createdBy); err != nil {
		return mapSynodError(err)
	}
	return nil
}

// DeleteSynod deletes a group; CASCADE removes its membership and bundle.
// The builtin boundary and self-lockout check live in [Service.DeleteSynod]
// (this is just the DELETE).
func DeleteSynod(ctx context.Context, db ExecQueryRower, name string) error {
	tag, err := db.Exec(ctx, deleteSynodSQL, name)
	if err != nil {
		return fmt.Errorf("rbac: delete synod %q: %w", name, wrapPgErr(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrSynodNotFound
	}
	return nil
}

// lockSynod takes a row lock on the group row (FOR UPDATE) and returns its
// builtin flag. The first step of any service mutation on a group
// (symmetric with [lockRole]). tx MUST be a transaction. Returns
// [ErrSynodNotFound] if the row doesn't exist.
func lockSynod(ctx context.Context, tx ExecQueryRower, name string) (builtin bool, err error) {
	rows, err := tx.Query(ctx, lockSynodForUpdateSQL, name)
	if err != nil {
		return false, fmt.Errorf("rbac: lock synod %q: %w", name, wrapPgErr(err))
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return false, fmt.Errorf("rbac: lock synod %q iter: %w", name, err)
		}
		return false, ErrSynodNotFound
	}
	if err := rows.Scan(&builtin); err != nil {
		return false, fmt.Errorf("rbac: scan synod %q builtin: %w", name, err)
	}
	return builtin, nil
}

// lockSynodRole takes a row lock on a bundle row (synod, role). Returns
// [ErrSynodRoleNotFound] if the pair doesn't exist. tx MUST be a tx.
func lockSynodRole(ctx context.Context, tx ExecQueryRower, synodName, roleName string) error {
	rows, err := tx.Query(ctx, lockSynodRoleForUpdateSQL, synodName, roleName)
	if err != nil {
		return fmt.Errorf("rbac: lock synod-role (%s -> %s): %w", synodName, roleName, wrapPgErr(err))
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return fmt.Errorf("rbac: lock synod-role iter: %w", err)
		}
		return ErrSynodRoleNotFound
	}
	return nil
}

// lockSynodOperator takes a row lock on a membership row (synod, aid).
// Returns [ErrSynodOperatorNotFound] if the pair doesn't exist. tx MUST be a
// tx.
func lockSynodOperator(ctx context.Context, tx ExecQueryRower, synodName, aid string) error {
	rows, err := tx.Query(ctx, lockSynodOperatorForUpdateSQL, synodName, aid)
	if err != nil {
		return fmt.Errorf("rbac: lock synod-operator (%s -> %s): %w", synodName, aid, wrapPgErr(err))
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return fmt.Errorf("rbac: lock synod-operator iter: %w", err)
		}
		return ErrSynodOperatorNotFound
	}
	return nil
}

// synodRoles reads a group's roles (the synod_roles bundle) without a lock —
// needed by the service on grant-role/revoke-role to compute `*`/
// least-privilege. The group is already locked by lockSynod in the same tx.
func synodRoles(ctx context.Context, tx ExecQueryRower, name string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT role_name FROM synod_roles WHERE synod_name = $1`, name)
	if err != nil {
		return nil, fmt.Errorf("rbac: read roles of synod %q: %w", name, wrapPgErr(err))
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, fmt.Errorf("rbac: scan role of synod %q: %w", name, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rbac: iter roles of synod %q: %w", name, err)
	}
	return out, nil
}

// mapSynodError maps pgx errors from synod INSERTs to sentinels (same
// pattern as [mapRoleError]):
//   - 23505 (UNIQUE) → [ErrSynodAlreadyExists].
//   - 23503 (FK) → wrapped with the constraint name (created_by_aid).
//   - anything else → wrapped with the SQLSTATE.
func mapSynodError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeUniqueViolation:
			return fmt.Errorf("%w (constraint %s): %w", ErrSynodAlreadyExists, pgErr.ConstraintName, err)
		case pgErrCodeForeignKeyViolation:
			return fmt.Errorf("rbac: FK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("rbac: %w", wrapPgErr(err))
}

// mapSynodMemberError maps a pgx error from a membership/bundle INSERT
// (add-operator / grant-role). The service already locked the group before
// the insert, so an FK violation here means a nonexistent operator
// (add-operator) OR role (grant-role):
//   - 23503 (FK) on aid/added_by_aid → [ErrOperatorNotFound];
//   - 23503 (FK) on role_name → [ErrRoleNotFound] (grant-role targeting a
//     nonexistent role);
//   - anything else → wrapped with the SQLSTATE.
//
// Distinguished by constraint name: synod_roles_role_fk → role, otherwise
// operator.
func mapSynodMemberError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgErrCodeForeignKeyViolation {
		if pgErr.ConstraintName == "synod_roles_role_fk" {
			return fmt.Errorf("%w (constraint %s): %w", ErrRoleNotFound, pgErr.ConstraintName, err)
		}
		return fmt.Errorf("%w (constraint %s): %w", ErrOperatorNotFound, pgErr.ConstraintName, err)
	}
	return fmt.Errorf("rbac: %w", wrapPgErr(err))
}

// SynodView is the API projection of a group for the synod.list read
// endpoint (ADR-049(g)): catalog fields (Description / Builtin) plus
// expanded Roles (role names in the bundle) and Operators (AID members).
// Symmetric with [RoleView]. Roles/Operators are sorted deterministically
// (stable list output).
type SynodView struct {
	Name        string
	Description string
	Builtin     bool
	Roles       []string
	Operators   []string
}

const (
	// selectSynodViewsSQL is the group catalog with description/builtin.
	// ORDER BY name — deterministic list order (symmetric with
	// selectRoleViewsSQL).
	selectSynodViewsSQL = `SELECT name, description, builtin FROM synods ORDER BY name`
)

// LoadSynodViews assembles the group API catalog with three SELECTs
// (groups / bundle / membership) — no N+1, symmetric with [LoadRoleViews].
// The "synod → roles/operators" join happens in Go, keyed by name. A group
// with no rows gets an empty Roles/Operators slice. Orphaned rows (role/AID
// outside the group catalog) are dropped.
func LoadSynodViews(ctx context.Context, db ExecQueryRower) ([]SynodView, error) {
	views, index, err := loadSynodViewRows(ctx, db)
	if err != nil {
		return nil, err
	}
	if err := loadSynodViewRoles(ctx, db, index); err != nil {
		return nil, err
	}
	if err := loadSynodViewOperators(ctx, db, index); err != nil {
		return nil, err
	}
	for i := range views {
		sort.Strings(views[i].Roles)
		sort.Strings(views[i].Operators)
	}
	return views, nil
}

func loadSynodViewRows(ctx context.Context, db ExecQueryRower) ([]SynodView, map[string]*SynodView, error) {
	rows, err := db.Query(ctx, selectSynodViewsSQL)
	if err != nil {
		return nil, nil, fmt.Errorf("rbac: query synod views: %w", wrapPgErr(err))
	}
	defer rows.Close()
	var views []SynodView
	for rows.Next() {
		var v SynodView
		if err := rows.Scan(&v.Name, &v.Description, &v.Builtin); err != nil {
			return nil, nil, fmt.Errorf("rbac: scan synod view: %w", err)
		}
		views = append(views, v)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("rbac: iter synod views: %w", err)
	}
	index := make(map[string]*SynodView, len(views))
	for i := range views {
		index[views[i].Name] = &views[i]
	}
	return views, index, nil
}

func loadSynodViewRoles(ctx context.Context, db ExecQueryRower, index map[string]*SynodView) error {
	rows, err := db.Query(ctx, selectSynodRolesSQL)
	if err != nil {
		return fmt.Errorf("rbac: query synod-view roles: %w", wrapPgErr(err))
	}
	defer rows.Close()
	for rows.Next() {
		var synodName, roleName string
		if err := rows.Scan(&synodName, &roleName); err != nil {
			return fmt.Errorf("rbac: scan synod-view role: %w", err)
		}
		if v, ok := index[synodName]; ok {
			v.Roles = append(v.Roles, roleName)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rbac: iter synod-view roles: %w", err)
	}
	return nil
}

func loadSynodViewOperators(ctx context.Context, db ExecQueryRower, index map[string]*SynodView) error {
	rows, err := db.Query(ctx, selectSynodOperatorsSQL)
	if err != nil {
		return fmt.Errorf("rbac: query synod-view operators: %w", wrapPgErr(err))
	}
	defer rows.Close()
	for rows.Next() {
		var synodName, aid string
		if err := rows.Scan(&synodName, &aid); err != nil {
			return fmt.Errorf("rbac: scan synod-view operator: %w", err)
		}
		if v, ok := index[synodName]; ok {
			v.Operators = append(v.Operators, aid)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rbac: iter synod-view operators: %w", err)
	}
	return nil
}
