package rbac

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ExecQueryRower is the narrow subset of pgxpool.Pool the repository needs to
// read an RBAC snapshot and write membership rows. Mirrors
// [operator.ExecQueryRower]; declared locally so package rbac doesn't pull in
// operator. `*pgxpool.Pool` / `pgx.Tx` satisfy it automatically.
type ExecQueryRower interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Compile-time check: pgx.Tx satisfies ExecQueryRower (bootstrap writes
// membership inside its own advisory-lock transaction; pool is used in
// `keeper run`).
var _ ExecQueryRower = (pgx.Tx)(nil)

// Snapshot is a raw RBAC snapshot from three tables, before permission
// strings are parsed. Separating "raw snapshot" from "parsed Enforcer" lets
// us test DB loading independently of permission parsing/matching.
//
// Revoked is a fourth projection (ADR-014 Amendment 2026-05-27, JWT immediate
// revoke): AID → `operators.revoked_at` for revoked Archons. It lives in the
// same RBAC snapshot so revoke checks flow through the same path as a
// regular permission check (TTL poll + `rbac:invalidate` pub/sub), without a
// separate JWT-blocklist infrastructure.
type Snapshot struct {
	// Roles maps role name to its permission strings (RAW, as stored in the
	// DB). A role with no permissions is present with an empty/nil slice —
	// it's valid, it just grants nothing.
	Roles map[string][]string

	// RoleScopes maps role name to its RAW default_scope string (ADR-047 S1).
	// Only roles with a non-NULL default_scope appear here; a missing key
	// means NULL, i.e. the dimension isn't set (the role's bare perms are
	// unrestricted, backcompat).
	RoleScopes map[string]string

	// Membership maps AID to the names of roles bound to it
	// (rbac_role_operators).
	Membership map[string][]string

	// Revoked maps AID → `operators.revoked_at` for every Archon with
	// `revoked_at IS NOT NULL`. Active operators are absent here.
	// Used by [Enforcer.Check] as the first step — a revoked AID is denied
	// regardless of roles (ADR-014 Amendment 2026-05-27).
	Revoked map[string]time.Time
}

const (
	// selectRolesSQL — every catalog role with its default_scope (ADR-047 S1).
	// A role with no permissions still ends up in the snapshot (a LEFT JOIN
	// below would give us that too, but a separate SELECT is simpler and
	// needs no dedup). default_scope NULL → scanned into *string=nil.
	selectRolesSQL = `SELECT name, default_scope FROM rbac_roles`

	// selectRolePermissionsSQL — every (role_name, permission) pair.
	selectRolePermissionsSQL = `SELECT role_name, permission FROM rbac_role_permissions`

	// selectRoleOperatorsSQL — every membership row (role_name, aid).
	selectRoleOperatorsSQL = `SELECT role_name, aid FROM rbac_role_operators`

	// selectRevokedOperatorsSQL — AIDs and `revoked_at` for every revoked
	// Archon (ADR-014 Amendment 2026-05-27). Used by [LoadSnapshot] to
	// populate Snapshot.Revoked. Active operators aren't selected — the
	// snapshot only holds the revoked projection.
	selectRevokedOperatorsSQL = `SELECT aid, revoked_at FROM operators WHERE revoked_at IS NOT NULL`

	// selectSynodOperatorsSQL — "Synod ↔ archon" membership (ADR-049). Joined
	// with selectSynodRolesSQL in Go when the snapshot is assembled: an
	// archon's effective roles = direct ∪ roles via all its Synods.
	selectSynodOperatorsSQL = `SELECT synod_name, aid FROM synod_operators`

	// selectSynodRolesSQL — the "Synod ↔ role" bundle (ADR-049). Group → its
	// roles; expanded into Membership via synod_operators.
	selectSynodRolesSQL = `SELECT synod_name, role_name FROM synod_roles`
)

// LoadSnapshot reads the RBAC tables and assembles a [Snapshot]. Separate
// SELECTs (no JOIN) because the data volume is small (roles/membership are
// rare), and denormalizing via JOIN would complicate deduping a role's
// permissions across multiple membership rows.
//
// Synod (ADR-049): roles via all of an archon's Synods are added on top of
// direct membership (rbac_role_operators) — `synod_operators` ⋈
// `synod_roles`. An archon's effective roles in [Snapshot.Membership] =
// direct ∪ via Synod (set union, deduped). This is assembled BEFORE the
// snapshot is returned; `NewEnforcerFromSnapshot` and matching below it never
// see Synod — a role's source doesn't matter to them.
//
// Permission strings are NOT parsed here — that's [NewEnforcerFromSnapshot]'s
// job via the existing [ParsePermission]; the repository stays a transport
// layer.
//
// Membership rows (direct or via Synod) referencing a role outside Roles
// don't make it into the enforcer (the FK guarantees the role exists, but we
// still guard against drift: a role with no rbac_roles record is ignored
// when the Enforcer is assembled).
func LoadSnapshot(ctx context.Context, db ExecQueryRower) (*Snapshot, error) {
	snap := &Snapshot{
		Roles:      make(map[string][]string),
		RoleScopes: make(map[string]string),
		Membership: make(map[string][]string),
		Revoked:    make(map[string]time.Time),
	}

	if err := loadRoles(ctx, db, snap); err != nil {
		return nil, err
	}
	if err := loadPermissions(ctx, db, snap); err != nil {
		return nil, err
	}
	if err := loadMembership(ctx, db, snap); err != nil {
		return nil, err
	}
	if err := loadRevoked(ctx, db, snap); err != nil {
		return nil, err
	}
	if err := loadSynodMembership(ctx, db, snap); err != nil {
		return nil, err
	}
	return snap, nil
}

func loadRoles(ctx context.Context, db ExecQueryRower, snap *Snapshot) error {
	rows, err := db.Query(ctx, selectRolesSQL)
	if err != nil {
		return fmt.Errorf("rbac: query roles: %w", wrapPgErr(err))
	}
	defer rows.Close()
	for rows.Next() {
		var (
			name         string
			defaultScope *string
		)
		if err := rows.Scan(&name, &defaultScope); err != nil {
			return fmt.Errorf("rbac: scan role: %w", err)
		}
		if _, ok := snap.Roles[name]; !ok {
			snap.Roles[name] = nil
		}
		// NULL default_scope → we don't set the key (absence means the
		// dimension isn't set, see the RoleScopes comment). We treat an
		// empty string as NULL too (no such thing as "a set but empty
		// dimension" in the coven MVP — see the S1 Deny writeup).
		if defaultScope != nil && *defaultScope != "" {
			snap.RoleScopes[name] = *defaultScope
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rbac: iter roles: %w", err)
	}
	return nil
}

func loadPermissions(ctx context.Context, db ExecQueryRower, snap *Snapshot) error {
	rows, err := db.Query(ctx, selectRolePermissionsSQL)
	if err != nil {
		return fmt.Errorf("rbac: query permissions: %w", wrapPgErr(err))
	}
	defer rows.Close()
	for rows.Next() {
		var roleName, permission string
		if err := rows.Scan(&roleName, &permission); err != nil {
			return fmt.Errorf("rbac: scan permission: %w", err)
		}
		snap.Roles[roleName] = append(snap.Roles[roleName], permission)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rbac: iter permissions: %w", err)
	}
	return nil
}

func loadMembership(ctx context.Context, db ExecQueryRower, snap *Snapshot) error {
	rows, err := db.Query(ctx, selectRoleOperatorsSQL)
	if err != nil {
		return fmt.Errorf("rbac: query membership: %w", wrapPgErr(err))
	}
	defer rows.Close()
	for rows.Next() {
		var roleName, aid string
		if err := rows.Scan(&roleName, &aid); err != nil {
			return fmt.Errorf("rbac: scan membership: %w", err)
		}
		snap.Membership[aid] = append(snap.Membership[aid], roleName)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rbac: iter membership: %w", err)
	}
	return nil
}

// loadRevoked populates Snapshot.Revoked with revoked Archons' AIDs
// (ADR-014 Amendment 2026-05-27). Active operators are filtered out on the
// SQL side (WHERE revoked_at IS NOT NULL) — the snapshot stays compact.
func loadRevoked(ctx context.Context, db ExecQueryRower, snap *Snapshot) error {
	rows, err := db.Query(ctx, selectRevokedOperatorsSQL)
	if err != nil {
		return fmt.Errorf("rbac: query revoked: %w", wrapPgErr(err))
	}
	defer rows.Close()
	for rows.Next() {
		var (
			aid       string
			revokedAt time.Time
		)
		if err := rows.Scan(&aid, &revokedAt); err != nil {
			return fmt.Errorf("rbac: scan revoked: %w", err)
		}
		snap.Revoked[aid] = revokedAt
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rbac: iter revoked: %w", err)
	}
	return nil
}

// loadSynodMembership expands roles via Synods and ADDS them to
// snap.Membership (ADR-049(c)/(e)): an archon's effective roles = direct ∪
// via all its Synods. A duplicate role (via both a direct grant AND a Synod,
// or via two Synods) is idempotent — set union, not a multiset.
//
// The two SELECTs (synod_operators / synod_roles) are joined in Go: the
// "Synod → roles" bundle is built as a map, then for each synod_operators
// membership row the group's roles are added to the archon, deduped against
// what's already known (direct roles plus roles added by other Synods for
// the same archon).
//
// Called AFTER loadMembership — direct roles are already in snap.Membership
// and accounted for in the dedup. Synod roles referencing a role outside the
// catalog end up in Membership but are dropped by NewEnforcerFromSnapshot
// (same as a dangling direct membership) — the repository stays a transport
// layer.
func loadSynodMembership(ctx context.Context, db ExecQueryRower, snap *Snapshot) error {
	bundle, err := loadSynodRoles(ctx, db)
	if err != nil {
		return err
	}

	rows, err := db.Query(ctx, selectSynodOperatorsSQL)
	if err != nil {
		return fmt.Errorf("rbac: query synod operators: %w", wrapPgErr(err))
	}
	defer rows.Close()

	// known[aid] is the set of roles already assigned to the archon (direct
	// roles plus roles added by previously-processed Synods in this pass).
	// Lazily initialized from snap.Membership so direct roles participate in
	// the dedup.
	known := make(map[string]map[string]struct{})
	for rows.Next() {
		var synodName, aid string
		if err := rows.Scan(&synodName, &aid); err != nil {
			return fmt.Errorf("rbac: scan synod operator: %w", err)
		}
		seen, ok := known[aid]
		if !ok {
			seen = make(map[string]struct{}, len(snap.Membership[aid]))
			for _, r := range snap.Membership[aid] {
				seen[r] = struct{}{}
			}
			known[aid] = seen
		}
		for _, roleName := range bundle[synodName] {
			if _, dup := seen[roleName]; dup {
				continue
			}
			seen[roleName] = struct{}{}
			snap.Membership[aid] = append(snap.Membership[aid], roleName)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rbac: iter synod operators: %w", err)
	}
	return nil
}

// loadSynodRoles reads the "Synod → roles" bundle (synod_roles) into a map.
// A duplicate (synod_name, role_name) is excluded by the PK, but deduping on
// read is cheap and safe anyway.
func loadSynodRoles(ctx context.Context, db ExecQueryRower) (map[string][]string, error) {
	rows, err := db.Query(ctx, selectSynodRolesSQL)
	if err != nil {
		return nil, fmt.Errorf("rbac: query synod roles: %w", wrapPgErr(err))
	}
	defer rows.Close()
	bundle := make(map[string][]string)
	for rows.Next() {
		var synodName, roleName string
		if err := rows.Scan(&synodName, &roleName); err != nil {
			return nil, fmt.Errorf("rbac: scan synod role: %w", err)
		}
		bundle[synodName] = append(bundle[synodName], roleName)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rbac: iter synod roles: %w", err)
	}
	return bundle, nil
}

// RoleView is the API projection of a role for the role.list read endpoint
// (Slice 2a/2b): catalog fields (Description / Builtin) plus expanded
// Permissions and Operators (AIDs). Differs from [Snapshot]: that's an
// enforcer snapshot without description/builtin (matching-only); this is a
// human-/API-facing view. Operators is sorted deterministically (stable
// list output).
type RoleView struct {
	Name        string
	Description string
	Builtin     bool
	Permissions []string
	Operators   []string
	// DefaultScope — the role's RAW default_scope (ADR-047 S1); empty string
	// means NULL (role has no scope restriction). For the list endpoint.
	DefaultScope string
}

const (
	// selectRoleViewsSQL — the role catalog with description/builtin/
	// default_scope (unlike [selectRolesSQL], which reads name+default_scope
	// for the enforcer snapshot). ORDER BY name gives a deterministic list
	// order.
	selectRoleViewsSQL = `SELECT name, description, builtin, default_scope FROM rbac_roles ORDER BY name`
)

// LoadRoleViews assembles the API role catalog with three SELECTs (roles /
// permissions / membership) — no N+1, mirrors [LoadSnapshot]. The
// "role → its permissions/operators" assembly happens in Go, keyed by name.
//
// A role's Permissions/Operators are present as an empty slice when it has
// no records (the role is valid, it just grants nothing / isn't assigned to
// anyone). Permission rows and membership rows referencing a role outside
// the catalog are dropped (the FK guarantees consistency, but we still guard
// against drift — same as [LoadSnapshot]).
func LoadRoleViews(ctx context.Context, db ExecQueryRower) ([]RoleView, error) {
	views, index, err := loadRoleViewRows(ctx, db)
	if err != nil {
		return nil, err
	}
	if err := loadRoleViewPermissions(ctx, db, index); err != nil {
		return nil, err
	}
	if err := loadRoleViewOperators(ctx, db, index); err != nil {
		return nil, err
	}
	return views, nil
}

// loadRoleViewRows reads the role catalog into a slice (deterministic order
// via ORDER BY name) and builds a "name → *RoleView" index over the same
// backing array, so permissions/operators can be filled in in place without
// a second pass.
func loadRoleViewRows(ctx context.Context, db ExecQueryRower) ([]RoleView, map[string]*RoleView, error) {
	rows, err := db.Query(ctx, selectRoleViewsSQL)
	if err != nil {
		return nil, nil, fmt.Errorf("rbac: query role views: %w", wrapPgErr(err))
	}
	defer rows.Close()
	var views []RoleView
	for rows.Next() {
		var (
			v            RoleView
			defaultScope *string
		)
		if err := rows.Scan(&v.Name, &v.Description, &v.Builtin, &defaultScope); err != nil {
			return nil, nil, fmt.Errorf("rbac: scan role view: %w", err)
		}
		if defaultScope != nil {
			v.DefaultScope = *defaultScope
		}
		views = append(views, v)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("rbac: iter role views: %w", err)
	}
	index := make(map[string]*RoleView, len(views))
	for i := range views {
		index[views[i].Name] = &views[i]
	}
	return views, index, nil
}

func loadRoleViewPermissions(ctx context.Context, db ExecQueryRower, index map[string]*RoleView) error {
	rows, err := db.Query(ctx, selectRolePermissionsSQL)
	if err != nil {
		return fmt.Errorf("rbac: query role-view permissions: %w", wrapPgErr(err))
	}
	defer rows.Close()
	for rows.Next() {
		var roleName, permission string
		if err := rows.Scan(&roleName, &permission); err != nil {
			return fmt.Errorf("rbac: scan role-view permission: %w", err)
		}
		if v, ok := index[roleName]; ok {
			v.Permissions = append(v.Permissions, permission)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rbac: iter role-view permissions: %w", err)
	}
	return nil
}

func loadRoleViewOperators(ctx context.Context, db ExecQueryRower, index map[string]*RoleView) error {
	rows, err := db.Query(ctx, selectRoleOperatorsSQL)
	if err != nil {
		return fmt.Errorf("rbac: query role-view operators: %w", wrapPgErr(err))
	}
	defer rows.Close()
	for rows.Next() {
		var roleName, aid string
		if err := rows.Scan(&roleName, &aid); err != nil {
			return fmt.Errorf("rbac: scan role-view operator: %w", err)
		}
		if v, ok := index[roleName]; ok {
			v.Operators = append(v.Operators, aid)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rbac: iter role-view operators: %w", err)
	}
	return nil
}

// insertRoleOperatorSQL — INSERTs a membership row (role_name, aid).
// granted_at comes from DEFAULT NOW() if the caller doesn't set it;
// granted_by_aid is optional (NULL for seed/bootstrap membership).
// ON CONFLICT DO NOTHING makes the insert idempotent — regranting the same
// pair isn't an error (mirrors seed migration 027).
const insertRoleOperatorSQL = `
INSERT INTO rbac_role_operators (role_name, aid, granted_by_aid)
VALUES ($1, $2, $3)
ON CONFLICT (role_name, aid) DO NOTHING
`

// GrantOperator binds an AID to a role — adds a membership row to
// rbac_role_operators (ADR-028(c)). Used by keeper init to bind the first
// Archon to cluster-admin (BUG-1 fix) inside its advisory-lock transaction;
// in Phase 2, role.grant-operator uses it via the API.
//
// grantedByAID == nil → granted_by_aid IS NULL (bootstrap membership with no
// initiating Archon). Idempotent: regranting the same pair is a no-op.
//
// Errors:
//   - FK violation on role_name → the role doesn't exist (seed migration 027
//     not applied); on aid → the operator doesn't exist.
//   - anything else — wrapped with the SQLSTATE.
func GrantOperator(ctx context.Context, db ExecQueryRower, roleName, aid string, grantedByAID *string) error {
	var grantedBy any
	if grantedByAID != nil {
		grantedBy = *grantedByAID
	}
	if _, err := db.Exec(ctx, insertRoleOperatorSQL, roleName, aid, grantedBy); err != nil {
		return fmt.Errorf("rbac: grant operator (%s -> %s): %w", roleName, aid, wrapPgErr(err))
	}
	return nil
}

// selectDirectRolesOfSQL — names of an AID's DIRECT membership roles
// (rbac_role_operators). Does NOT include roles via Synod (ADR-049) —
// federated reconciliation (HIGH-1, ADR-058(d)) only reconciles direct
// membership managed by group_role_map, and must not touch Synod-granted
// roles.
const selectDirectRolesOfSQL = `SELECT role_name FROM rbac_role_operators WHERE aid = $1`

// DirectRolesOf returns the names of roles bound to an AID via a DIRECT
// membership row in rbac_role_operators (no Synod expansion). Used by
// federated reconciliation (auth/mapper.go, HIGH-1) to compute which roles
// need to be dropped when a user's external groups change.
//
// db may be a pool OR a tx (pgx.Tx satisfies ExecQueryRower) — the caller
// reads current membership and writes grant/revoke in a SINGLE transaction.
func DirectRolesOf(ctx context.Context, db ExecQueryRower, aid string) ([]string, error) {
	rows, err := db.Query(ctx, selectDirectRolesOfSQL, aid)
	if err != nil {
		return nil, fmt.Errorf("rbac: select direct roles of %q: %w", aid, wrapPgErr(err))
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("rbac: scan direct role of %q: %w", aid, err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rbac: iterate direct roles of %q: %w", aid, err)
	}
	return out, nil
}

// wrapPgErr adds the SQLSTATE to the message when the error is a
// pgconn.PgError. This makes it easier to tell "table rbac_* doesn't exist"
// (migration not applied) apart from transport failures.
func wrapPgErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return fmt.Errorf("pg %s: %w", pgErr.Code, err)
	}
	return err
}
