package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

// claimsFor / wantProblem — shared test helpers of the handlers package (errand_test.go /
// operator_test.go); handler-native T5d tests for role/synod call *Typed directly through them.

// rbacFakePool — narrow mock of [rbac.ServicePool] for RoleHandler unit tests.
// Classifies SQL by substring and returns the test-provided outcome. Covers the
// TRANSPORT (decode / sentinel→problem mapping / statuses); Service SQL-logic
// consistency is validated by rbac/crud_integration_test.go (testcontainers
// PG). The Tx wrapper proxies Exec/Query back to the pool; Commit/Rollback are no-op.
type rbacFakePool struct {
	// lockRole — outcome of SELECT builtin FOR UPDATE (lockRole):
	// found=false → ErrRoleNotFound (empty row-set); builtin → value.
	lockRoleFound bool
	lockRoleValue bool

	// lockRoleOperatorFound — outcome of SELECT 1 FOR UPDATE (lockRoleOperator):
	// false → ErrRoleOperatorNotFound.
	lockRoleOperatorFound bool

	// rolePerms — the role's permissions (rolePermissions SELECT): controls whether
	// the role grants `*` (→ a self-lockout probe is needed).
	rolePerms []string

	// callerPermsSet — the caller's effective permissions (subset-check
	// SELECT rp.permission … JOIN). nil → default `["*"]` (caller=cluster-admin),
	// so transport tests without an explicit least-privilege scenario pass the
	// subset-check. callerPermsExplicit distinguishes "unset" (default `*`) from
	// "set empty" (caller with no rights).
	callerPermsSet      []string
	callerPermsExplicit bool

	// survivors — result of the self-lockout probes (lockWildcardAdmins*): empty →
	// ErrWouldLockOutCluster.
	survivors []string

	// roleScope — the role's RAW default_scope (roleDefaultScope SELECT, ADR-047 S1):
	// nil → NULL (a role without scope, bare-perms unrestricted — the transport-
	// tests default). Set explicitly by the default_scope-escalation scenario.
	roleScope *string

	// parentChain — rows of the WITH RECURSIVE chain query (resolveRoleChain,
	// ADR-078): the named parent and its ancestors. Empty → the parent is not in
	// the catalog (ErrRoleNotFound), which is what the non-derived scenarios want.
	parentChain []chainRow

	// insertRoleErr — error from INSERT INTO rbac_roles (Create): unique → 409.
	insertRoleErr error

	// insertMembershipErr — error from INSERT membership (GrantOperator / Synod
	// AddOperator / GrantRole): FK → 404.
	insertMembershipErr error

	// insertSynodErr — error from INSERT INTO synods (CreateSynod): unique → 409
	// (ADR-049). lockSynodFound controls whether the group exists for mutations.
	insertSynodErr  error
	lockSynodFound  bool
	lockSynodValue  bool
	synodRolesValue []string

	// updateSynodFound — outcome of UPDATE synods SET description (UpdateSynodDescription,
	// ADR-049 amend): true → RowsAffected 1 (group exists), false → 0 → ErrSynodNotFound.
	updateSynodFound bool

	beginErr error
}

func (p *rbacFakePool) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	switch {
	case contains(sql, "INSERT INTO rbac_roles"):
		if p.insertRoleErr != nil {
			return pgconn.CommandTag{}, p.insertRoleErr
		}
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	case contains(sql, "INSERT INTO rbac_role_operators"):
		if p.insertMembershipErr != nil {
			return pgconn.CommandTag{}, p.insertMembershipErr
		}
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	case contains(sql, "INSERT INTO rbac_role_permissions"):
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	case contains(sql, "DELETE FROM rbac_role_permissions"):
		return pgconn.NewCommandTag("DELETE 0"), nil
	case contains(sql, "DELETE FROM rbac_role_operators"):
		return pgconn.NewCommandTag("DELETE 1"), nil
	case contains(sql, "DELETE FROM rbac_roles"):
		return pgconn.NewCommandTag("DELETE 1"), nil
	case contains(sql, "UPDATE rbac_roles SET"):
		// default_scope (ADR-047) / parent_role (ADR-078) writes.
		return pgconn.NewCommandTag("UPDATE 1"), nil
	// Synod branches (ADR-049) — SynodHandler transport tests share this fake.
	case contains(sql, "INSERT INTO synods"):
		if p.insertSynodErr != nil {
			return pgconn.CommandTag{}, p.insertSynodErr
		}
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	case contains(sql, "INSERT INTO synod_operators"):
		if p.insertMembershipErr != nil {
			return pgconn.CommandTag{}, p.insertMembershipErr
		}
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	case contains(sql, "INSERT INTO synod_roles"):
		if p.insertMembershipErr != nil {
			return pgconn.CommandTag{}, p.insertMembershipErr
		}
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	case contains(sql, "DELETE FROM synod_operators"):
		return pgconn.NewCommandTag("DELETE 1"), nil
	case contains(sql, "DELETE FROM synod_roles"):
		return pgconn.NewCommandTag("DELETE 1"), nil
	case contains(sql, "DELETE FROM synods"):
		return pgconn.NewCommandTag("DELETE 1"), nil
	case contains(sql, "UPDATE synods SET description"):
		// UpdateSynodDescription (ADR-049 amend): found → 1 row, else 0 → 404.
		if p.updateSynodFound {
			return pgconn.NewCommandTag("UPDATE 1"), nil
		}
		return pgconn.NewCommandTag("UPDATE 0"), nil
	}
	return pgconn.CommandTag{}, errors.New("rbacFakePool.Exec: unexpected SQL: " + sql)
}

func (p *rbacFakePool) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	switch {
	case contains(sql, "SELECT builtin FROM rbac_roles"):
		// lockRole: empty → ErrRoleNotFound; else a single bool row.
		if !p.lockRoleFound {
			return &boolRows{}, nil
		}
		return &boolRows{values: []bool{p.lockRoleValue}}, nil
	case contains(sql, "SELECT 1 FROM rbac_role_operators"):
		// lockRoleOperator: empty → ErrRoleOperatorNotFound.
		if !p.lockRoleOperatorFound {
			return &intRows{}, nil
		}
		return &intRows{values: []int{1}}, nil
	case contains(sql, "SELECT rp.permission"):
		// caller-perms (subset-check): default `["*"]` (caller=cluster-admin),
		// unless the test set the list explicitly.
		if p.callerPermsExplicit {
			return &roleStringRows{values: p.callerPermsSet}, nil
		}
		return &roleStringRows{values: []string{"*"}}, nil
	case contains(sql, "SELECT permission FROM rbac_role_permissions"):
		return &roleStringRows{values: p.rolePerms}, nil
	case contains(sql, "SELECT default_scope FROM rbac_roles"):
		// roleDefaultScope (subset-check granted side): a single nullable row.
		return &nullStringRows{value: p.roleScope}, nil
	case contains(sql, "SELECT parent_role FROM rbac_roles"):
		// roleParent (ADR-078): NULL → a plain role, which is what these
		// transport tests exercise. Derivation is covered against a real DB.
		return &nullStringRows{value: nil}, nil
	case contains(sql, "WITH RECURSIVE chain"):
		// resolveRoleChain (ADR-078): the named parent plus its ancestors, one
		// row per (role, permission). Empty → the parent isn't in the catalog.
		return &chainRows{rows: p.parentChain}, nil
	case contains(sql, "SELECT builtin FROM synods"):
		// lockSynod: empty → ErrSynodNotFound; else a single bool row.
		if !p.lockSynodFound {
			return &boolRows{}, nil
		}
		return &boolRows{values: []bool{p.lockSynodValue}}, nil
	case contains(sql, "SELECT 1 FROM synod_operators"):
		// lockSynodOperator: empty → ErrSynodOperatorNotFound.
		if !p.lockRoleOperatorFound {
			return &intRows{}, nil
		}
		return &intRows{values: []int{1}}, nil
	case contains(sql, "SELECT 1 FROM synod_roles"):
		// lockSynodRole: empty → ErrSynodRoleNotFound.
		if !p.lockRoleOperatorFound {
			return &intRows{}, nil
		}
		return &intRows{values: []int{1}}, nil
	case contains(sql, "SELECT role_name FROM synod_roles"):
		// synodRoles (subset-check add-operator): the bundle's role set.
		return &roleStringRows{values: p.synodRolesValue}, nil
	case contains(sql, "FROM synod_roles sr"):
		// synodGivesWildcard probe: empty → the group does not bundle `*`.
		return &intRows{}, nil
	case contains(sql, "FROM synod_operators"):
		// Synod branch of the self-lockout probe (ADR-049(f)): the second locking
		// query over synod_operators. These handler-unit scenarios don't model
		// group admins — empty; rbac integration-guard tests cover them.
		return &roleStringRows{}, nil
	case contains(sql, "FOR UPDATE OF ro, rp, r, o"):
		// direct self-lockout probe (excluding role / pair / core). `r` is
		// rbac_roles, joined in to skip derived roles (ADR-078(i)).
		return &roleStringRows{values: p.survivors}, nil
	}
	return nil, errors.New("rbacFakePool.Query: unexpected SQL: " + sql)
}

func (p *rbacFakePool) BeginTx(_ context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	if p.beginErr != nil {
		return nil, p.beginErr
	}
	return &rbacFakeTx{pool: p}, nil
}

// contains — a short substring helper (bytes-free).
func contains(s, sub string) bool {
	return len(sub) <= len(s) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// rbacFakeTx proxies Exec/Query to the pool; everything else panics (must not
// be called in the scope of these tests).
type rbacFakeTx struct{ pool *rbacFakePool }

func (t *rbacFakeTx) Begin(ctx context.Context) (pgx.Tx, error) {
	return t.pool.BeginTx(ctx, pgx.TxOptions{})
}
func (t *rbacFakeTx) Commit(_ context.Context) error   { return nil }
func (t *rbacFakeTx) Rollback(_ context.Context) error { return nil }
func (t *rbacFakeTx) CopyFrom(_ context.Context, _ pgx.Identifier, _ []string, _ pgx.CopyFromSource) (int64, error) {
	panic("rbacFakeTx.CopyFrom: unexpected")
}
func (t *rbacFakeTx) SendBatch(_ context.Context, _ *pgx.Batch) pgx.BatchResults {
	panic("rbacFakeTx.SendBatch: unexpected")
}
func (t *rbacFakeTx) LargeObjects() pgx.LargeObjects { panic("rbacFakeTx.LargeObjects: unexpected") }
func (t *rbacFakeTx) Prepare(_ context.Context, _, _ string) (*pgconn.StatementDescription, error) {
	panic("rbacFakeTx.Prepare: unexpected")
}
func (t *rbacFakeTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.pool.Exec(ctx, sql, args...)
}
func (t *rbacFakeTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.pool.Query(ctx, sql, args...)
}
func (t *rbacFakeTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	panic("rbacFakeTx.QueryRow: unexpected")
}
func (t *rbacFakeTx) Conn() *pgx.Conn { return nil }

// --- minimal pgx.Rows wrappers for single-column bool / int / string selects ---

type boolRows struct {
	values []bool
	idx    int
}

func (r *boolRows) Next() bool                                   { r.idx++; return r.idx <= len(r.values) }
func (r *boolRows) Scan(dest ...any) error                       { *dest[0].(*bool) = r.values[r.idx-1]; return nil }
func (r *boolRows) Err() error                                   { return nil }
func (r *boolRows) Close()                                       {}
func (r *boolRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *boolRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *boolRows) Values() ([]any, error)                       { return nil, nil }
func (r *boolRows) RawValues() [][]byte                          { return nil }
func (r *boolRows) Conn() *pgx.Conn                              { return nil }

type intRows struct {
	values []int
	idx    int
}

func (r *intRows) Next() bool                                   { r.idx++; return r.idx <= len(r.values) }
func (r *intRows) Scan(dest ...any) error                       { *dest[0].(*int) = r.values[r.idx-1]; return nil }
func (r *intRows) Err() error                                   { return nil }
func (r *intRows) Close()                                       {}
func (r *intRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *intRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *intRows) Values() ([]any, error)                       { return nil, nil }
func (r *intRows) RawValues() [][]byte                          { return nil }
func (r *intRows) Conn() *pgx.Conn                              { return nil }

type roleStringRows struct {
	values []string
	idx    int
}

func (r *roleStringRows) Next() bool { r.idx++; return r.idx <= len(r.values) }
func (r *roleStringRows) Scan(dest ...any) error {
	*dest[0].(*string) = r.values[r.idx-1]
	return nil
}
func (r *roleStringRows) Err() error                                   { return nil }
func (r *roleStringRows) Close()                                       {}
func (r *roleStringRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *roleStringRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *roleStringRows) Values() ([]any, error)                       { return nil, nil }
func (r *roleStringRows) RawValues() [][]byte                          { return nil }
func (r *roleStringRows) Conn() *pgx.Conn                              { return nil }

// nullStringRows — a single row with a nullable default_scope (scanned into *string).
// value=nil → NULL (a role without scope). Always one row (the role exists, having
// been locked earlier by lockRole).
type nullStringRows struct {
	value *string
	done  bool
}

func (r *nullStringRows) Next() bool {
	if r.done {
		return false
	}
	r.done = true
	return true
}
func (r *nullStringRows) Scan(dest ...any) error {
	*dest[0].(**string) = r.value
	return nil
}
func (r *nullStringRows) Err() error                                   { return nil }
func (r *nullStringRows) Close()                                       {}
func (r *nullStringRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *nullStringRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *nullStringRows) Values() ([]any, error)                       { return nil, nil }
func (r *nullStringRows) RawValues() [][]byte                          { return nil }
func (r *nullStringRows) Conn() *pgx.Conn                              { return nil }

// chainRow — one row of the parent-chain query (resolveRoleChain, ADR-078):
// role name, its parent (empty = a root), its RAW default_scope (empty = NULL) and
// ONE of its permissions (empty = the LEFT JOIN miss, a role granting nothing).
type chainRow struct {
	name       string
	parent     string
	scope      string
	permission string
}

// chainRows — four-column rows over []chainRow, with the empty-string-as-NULL
// convention of chainRow.
type chainRows struct {
	rows []chainRow
	idx  int
}

func (r *chainRows) Next() bool { r.idx++; return r.idx <= len(r.rows) }
func (r *chainRows) Scan(dest ...any) error {
	row := r.rows[r.idx-1]
	*dest[0].(*string) = row.name
	*dest[1].(**string) = nullable(row.parent)
	*dest[2].(**string) = nullable(row.scope)
	*dest[3].(**string) = nullable(row.permission)
	return nil
}

// nullable maps the empty string to a NULL column.
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
func (r *chainRows) Err() error                                   { return nil }
func (r *chainRows) Close()                                       {}
func (r *chainRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *chainRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *chainRows) Values() ([]any, error)                       { return nil, nil }
func (r *chainRows) RawValues() [][]byte                          { return nil }
func (r *chainRows) Conn() *pgx.Conn                              { return nil }

// newRoleHandler assembles a RoleHandler over rbac.Service on a fake pool.
func newRoleHandler(t *testing.T, pool *rbacFakePool) *RoleHandler {
	t.Helper()
	svc, err := rbac.NewService(rbac.ServiceDeps{Pool: pool})
	if err != nil {
		t.Fatalf("rbac.NewService: %v", err)
	}
	return NewRoleHandler(svc, nil)
}

// --- Create ---

func TestRoleHandler_Create_201(t *testing.T) {
	h := newRoleHandler(t, &rbacFakePool{})
	_, err := h.CreateTyped(context.Background(), claimsFor("archon-alice"),
		RoleCreateInput{Name: "ops", Description: "ops team", Permissions: []string{"soul.list"}})
	if err != nil {
		t.Fatalf("CreateTyped: %v", err)
	}
}

func TestRoleHandler_Create_EmptyName_422(t *testing.T) {
	h := newRoleHandler(t, &rbacFakePool{})
	_, err := h.CreateTyped(context.Background(), claimsFor("archon-alice"), RoleCreateInput{Name: ""})
	wantProblem(t, err, problem.TypeValidationFailed)
}

func TestRoleHandler_Create_BadName_422(t *testing.T) {
	h := newRoleHandler(t, &rbacFakePool{})
	_, err := h.CreateTyped(context.Background(), claimsFor("archon-alice"), RoleCreateInput{Name: "Bad_Name"})
	wantProblem(t, err, problem.TypeValidationFailed)
}

func TestRoleHandler_Create_BadPermission_422(t *testing.T) {
	h := newRoleHandler(t, &rbacFakePool{})
	_, err := h.CreateTyped(context.Background(), claimsFor("archon-alice"),
		RoleCreateInput{Name: "ops", Permissions: []string{"totally.invalid.three.seg"}})
	wantProblem(t, err, problem.TypeValidationFailed)
}

func TestRoleHandler_Create_Duplicate_409(t *testing.T) {
	pool := &rbacFakePool{
		insertRoleErr: &pgconn.PgError{Code: "23505", ConstraintName: "rbac_roles_pkey"},
	}
	h := newRoleHandler(t, pool)
	_, err := h.CreateTyped(context.Background(), claimsFor("archon-alice"), RoleCreateInput{Name: "ops"})
	wantProblem(t, err, problem.TypeRoleExists)
}

// --- Delete ---

func TestRoleHandler_Delete_204(t *testing.T) {
	pool := &rbacFakePool{lockRoleFound: true, lockRoleValue: false, rolePerms: []string{"soul.list"}}
	h := newRoleHandler(t, pool)
	if _, err := h.DeleteTyped(context.Background(), "ops"); err != nil {
		t.Fatalf("DeleteTyped: %v", err)
	}
}

func TestRoleHandler_Delete_NotFound_404(t *testing.T) {
	pool := &rbacFakePool{lockRoleFound: false}
	h := newRoleHandler(t, pool)
	_, err := h.DeleteTyped(context.Background(), "ghost")
	wantProblem(t, err, problem.TypeRoleNotFound)
}

func TestRoleHandler_Delete_Builtin_409(t *testing.T) {
	pool := &rbacFakePool{lockRoleFound: true, lockRoleValue: true}
	h := newRoleHandler(t, pool)
	_, err := h.DeleteTyped(context.Background(), "cluster-admin")
	wantProblem(t, err, problem.TypeRoleBuiltin)
}

func TestRoleHandler_Delete_Lockout_409(t *testing.T) {
	// The role grants `*`, no surviving admins → ErrWouldLockOutCluster.
	pool := &rbacFakePool{lockRoleFound: true, lockRoleValue: false, rolePerms: []string{"*"}, survivors: nil}
	h := newRoleHandler(t, pool)
	_, err := h.DeleteTyped(context.Background(), "admins")
	wantProblem(t, err, problem.TypeWouldLockOutCluster)
}

// --- UpdatePermissions ---

func TestRoleHandler_Update_204(t *testing.T) {
	pool := &rbacFakePool{lockRoleFound: true, lockRoleValue: false, rolePerms: []string{"soul.list"}}
	h := newRoleHandler(t, pool)
	_, err := h.UpdatePermissionsTyped(context.Background(), claimsFor("archon-alice"),
		UpdatePermissionsInput{Name: "ops", Permissions: []string{"soul.list", "incarnation.get"}})
	if err != nil {
		t.Fatalf("UpdatePermissionsTyped: %v", err)
	}
}

func TestRoleHandler_Update_NotFound_404(t *testing.T) {
	pool := &rbacFakePool{lockRoleFound: false}
	h := newRoleHandler(t, pool)
	_, err := h.UpdatePermissionsTyped(context.Background(), claimsFor("archon-alice"),
		UpdatePermissionsInput{Name: "ghost", Permissions: []string{}})
	wantProblem(t, err, problem.TypeRoleNotFound)
}

func TestRoleHandler_Update_Builtin_409(t *testing.T) {
	pool := &rbacFakePool{lockRoleFound: true, lockRoleValue: true}
	h := newRoleHandler(t, pool)
	_, err := h.UpdatePermissionsTyped(context.Background(), claimsFor("archon-alice"),
		UpdatePermissionsInput{Name: "cluster-admin", Permissions: []string{"soul.list"}})
	wantProblem(t, err, problem.TypeRoleBuiltin)
}

func TestRoleHandler_Update_BadPermission_422(t *testing.T) {
	pool := &rbacFakePool{lockRoleFound: true, lockRoleValue: false}
	h := newRoleHandler(t, pool)
	_, err := h.UpdatePermissionsTyped(context.Background(), claimsFor("archon-alice"),
		UpdatePermissionsInput{Name: "ops", Permissions: []string{"totally.invalid.three.seg"}})
	wantProblem(t, err, problem.TypeValidationFailed)
}

func TestRoleHandler_Update_Lockout_409(t *testing.T) {
	// The old set grants `*`, the new one doesn't, no survivors → lockout.
	pool := &rbacFakePool{lockRoleFound: true, lockRoleValue: false, rolePerms: []string{"*"}, survivors: nil}
	h := newRoleHandler(t, pool)
	_, err := h.UpdatePermissionsTyped(context.Background(), claimsFor("archon-alice"),
		UpdatePermissionsInput{Name: "admins", Permissions: []string{"soul.list"}})
	wantProblem(t, err, problem.TypeWouldLockOutCluster)
}

// --- GrantOperator ---

func TestRoleHandler_GrantOperator_204(t *testing.T) {
	pool := &rbacFakePool{lockRoleFound: true, lockRoleValue: false}
	h := newRoleHandler(t, pool)
	_, err := h.GrantOperatorTyped(context.Background(), claimsFor("archon-alice"), "ops", "archon-bob")
	if err != nil {
		t.Fatalf("GrantOperatorTyped: %v", err)
	}
}

func TestRoleHandler_GrantOperator_EmptyAID_422(t *testing.T) {
	h := newRoleHandler(t, &rbacFakePool{})
	_, err := h.GrantOperatorTyped(context.Background(), claimsFor("archon-alice"), "ops", "")
	wantProblem(t, err, problem.TypeValidationFailed)
}

func TestRoleHandler_GrantOperator_BadAID_422(t *testing.T) {
	h := newRoleHandler(t, &rbacFakePool{})
	_, err := h.GrantOperatorTyped(context.Background(), claimsFor("archon-alice"), "ops", "BOB")
	wantProblem(t, err, problem.TypeValidationFailed)
}

func TestRoleHandler_GrantOperator_RoleNotFound_404(t *testing.T) {
	pool := &rbacFakePool{lockRoleFound: false}
	h := newRoleHandler(t, pool)
	_, err := h.GrantOperatorTyped(context.Background(), claimsFor("archon-alice"), "ghost", "archon-bob")
	wantProblem(t, err, problem.TypeRoleNotFound)
}

func TestRoleHandler_GrantOperator_OperatorNotFound_404(t *testing.T) {
	pool := &rbacFakePool{
		lockRoleFound:       true,
		insertMembershipErr: &pgconn.PgError{Code: "23503", ConstraintName: "rbac_role_operators_aid_fk"},
	}
	h := newRoleHandler(t, pool)
	_, err := h.GrantOperatorTyped(context.Background(), claimsFor("archon-alice"), "ops", "archon-ghost")
	wantProblem(t, err, problem.TypeNotFound)
}

// TestRoleHandler_GrantOperator_CallerAIDFromClaims checks that CallerAID
// (granted_by_aid) is taken from the claims subject and reaches the Service call.
func TestRoleHandler_GrantOperator_CallerAIDFromClaims(t *testing.T) {
	var gotGrantedBy any
	pool := &grantSpyPool{rbacFakePool: rbacFakePool{lockRoleFound: true}, captured: &gotGrantedBy}
	svc, err := rbac.NewService(rbac.ServiceDeps{Pool: pool})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	h := NewRoleHandler(svc, nil)
	reply, err := h.GrantOperatorTyped(context.Background(), claimsFor("archon-alice"), "ops", "archon-bob")
	if err != nil {
		t.Fatalf("GrantOperatorTyped: %v", err)
	}
	if reply.GrantedByAID != "archon-alice" {
		t.Errorf("reply.GrantedByAID = %q, want archon-alice", reply.GrantedByAID)
	}
	if gotGrantedBy != "archon-alice" {
		t.Errorf("granted_by_aid (service arg) = %v, want archon-alice", gotGrantedBy)
	}
}

// grantSpyPool — an rbacFakePool that captures granted_by_aid (the 3rd arg of
// the INSERT membership) to verify CallerAID is threaded through from claims.
type grantSpyPool struct {
	rbacFakePool
	captured *any
}

func (p *grantSpyPool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if contains(sql, "INSERT INTO rbac_role_operators") && len(args) >= 3 {
		*p.captured = args[2]
	}
	return p.rbacFakePool.Exec(ctx, sql, args...)
}

func (p *grantSpyPool) BeginTx(_ context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	return &grantSpyTx{pool: p}, nil
}

type grantSpyTx struct{ pool *grantSpyPool }

func (t *grantSpyTx) Begin(ctx context.Context) (pgx.Tx, error) {
	return t.pool.BeginTx(ctx, pgx.TxOptions{})
}
func (t *grantSpyTx) Commit(_ context.Context) error   { return nil }
func (t *grantSpyTx) Rollback(_ context.Context) error { return nil }
func (t *grantSpyTx) CopyFrom(_ context.Context, _ pgx.Identifier, _ []string, _ pgx.CopyFromSource) (int64, error) {
	panic("unexpected")
}
func (t *grantSpyTx) SendBatch(_ context.Context, _ *pgx.Batch) pgx.BatchResults { panic("unexpected") }
func (t *grantSpyTx) LargeObjects() pgx.LargeObjects                             { panic("unexpected") }
func (t *grantSpyTx) Prepare(_ context.Context, _, _ string) (*pgconn.StatementDescription, error) {
	panic("unexpected")
}
func (t *grantSpyTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.pool.Exec(ctx, sql, args...)
}
func (t *grantSpyTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.pool.rbacFakePool.Query(ctx, sql, args...)
}
func (t *grantSpyTx) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row { panic("unexpected") }
func (t *grantSpyTx) Conn() *pgx.Conn                                        { return nil }

// --- RevokeOperator ---

func TestRoleHandler_RevokeOperator_204(t *testing.T) {
	pool := &rbacFakePool{lockRoleOperatorFound: true, lockRoleFound: true, rolePerms: []string{"soul.list"}}
	h := newRoleHandler(t, pool)
	if _, err := h.RevokeOperatorTyped(context.Background(), "ops", "archon-bob"); err != nil {
		t.Fatalf("RevokeOperatorTyped: %v", err)
	}
}

func TestRoleHandler_RevokeOperator_BadAID_422(t *testing.T) {
	h := newRoleHandler(t, &rbacFakePool{})
	_, err := h.RevokeOperatorTyped(context.Background(), "ops", "BOB")
	wantProblem(t, err, problem.TypeValidationFailed)
}

func TestRoleHandler_RevokeOperator_NotFound_404(t *testing.T) {
	pool := &rbacFakePool{lockRoleOperatorFound: false}
	h := newRoleHandler(t, pool)
	_, err := h.RevokeOperatorTyped(context.Background(), "ops", "archon-bob")
	wantProblem(t, err, problem.TypeNotFound)
}

func TestRoleHandler_RevokeOperator_Lockout_409(t *testing.T) {
	// membership exists, the role grants `*`, no survivors → lockout.
	pool := &rbacFakePool{
		lockRoleOperatorFound: true,
		lockRoleFound:         true,
		rolePerms:             []string{"*"},
		survivors:             nil,
	}
	h := newRoleHandler(t, pool)
	_, err := h.RevokeOperatorTyped(context.Background(), "admins", "archon-alice")
	wantProblem(t, err, problem.TypeWouldLockOutCluster)
}

// --- List ---

// listFakePool returns a fixed catalog from three SELECTs (LoadRoleViews).
type listFakePool struct{ rbacFakePool }

func (p *listFakePool) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	switch {
	case contains(sql, "SELECT name, description, builtin, default_scope, parent_role FROM rbac_roles"):
		return &roleViewRows{rows: [][5]any{
			{"admins", "cluster admins", true, nil, nil},
			{"ops", "ops team", false, ptrStr("coven=prod"), nil},
		}}, nil
	case contains(sql, "SELECT role_name, permission FROM rbac_role_permissions"):
		return &pairRows{rows: [][2]string{{"admins", "*"}, {"ops", "soul.list"}}}, nil
	case contains(sql, "SELECT role_name, aid FROM rbac_role_operators"):
		return &pairRows{rows: [][2]string{{"admins", "archon-alice"}}}, nil
	}
	return nil, errors.New("listFakePool.Query: unexpected SQL: " + sql)
}

func TestRoleHandler_List_200(t *testing.T) {
	svc, err := rbac.NewService(rbac.ServiceDeps{Pool: &listFakePool{}})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	h := NewRoleHandler(svc, nil)
	page, err := h.ListTyped(context.Background())
	if err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(page.Items))
	}
	// Deterministic order (ORDER BY name): admins, ops.
	if page.Items[0].Name != "admins" || !page.Items[0].Builtin {
		t.Errorf("item[0] = %+v", page.Items[0])
	}
	if len(page.Items[0].Permissions) != 1 || page.Items[0].Permissions[0] != "*" {
		t.Errorf("admins permissions = %v", page.Items[0].Permissions)
	}
	if len(page.Items[0].Operators) != 1 || page.Items[0].Operators[0] != "archon-alice" {
		t.Errorf("admins operators = %v", page.Items[0].Operators)
	}
	// ops without operators — a non-nil empty slice (on wire `[]`, native projection in api).
	if page.Items[1].Operators == nil {
		t.Errorf("ops operators is nil, want empty slice")
	}
	// ADR-047 S1: default_scope (handler-flat RoleView carries a RAW string) — admins
	// "" (NULL), ops="coven=prod". The native projection builds the nullable wire form.
	if page.Items[0].DefaultScope != "" {
		t.Errorf("admins default_scope = %q, want \"\" (NULL)", page.Items[0].DefaultScope)
	}
	if page.Items[1].DefaultScope != "coven=prod" {
		t.Errorf("ops default_scope = %q, want coven=prod", page.Items[1].DefaultScope)
	}
}

// ptrStr — a *string literal for the nullable default_scope in fixtures.
func ptrStr(s string) *string { return &s }

// roleViewRows — five-column rows (name, description, builtin, default_scope,
// parent_role). The trailing two are nullable: a nil cell → NULL.
type roleViewRows struct {
	rows [][5]any
	idx  int
}

func (r *roleViewRows) Next() bool { r.idx++; return r.idx <= len(r.rows) }
func (r *roleViewRows) Scan(dest ...any) error {
	row := r.rows[r.idx-1]
	*dest[0].(*string) = row[0].(string)
	*dest[1].(*string) = row[1].(string)
	*dest[2].(*bool) = row[2].(bool)
	assignNullableCell(dest[3].(**string), row[3])
	assignNullableCell(dest[4].(**string), row[4])
	return nil
}

// assignNullableCell writes a nullable stub cell into a *string dest: a nil cell
// stands in for a NULL column.
func assignNullableCell(dest **string, cell any) {
	if cell == nil {
		*dest = nil
		return
	}
	*dest = cell.(*string)
}
func (r *roleViewRows) Err() error                                   { return nil }
func (r *roleViewRows) Close()                                       {}
func (r *roleViewRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *roleViewRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *roleViewRows) Values() ([]any, error)                       { return nil, nil }
func (r *roleViewRows) RawValues() [][]byte                          { return nil }
func (r *roleViewRows) Conn() *pgx.Conn                              { return nil }

// pairRows — two-column string/string rows.
type pairRows struct {
	rows [][2]string
	idx  int
}

func (r *pairRows) Next() bool { r.idx++; return r.idx <= len(r.rows) }
func (r *pairRows) Scan(dest ...any) error {
	row := r.rows[r.idx-1]
	*dest[0].(*string) = row[0]
	*dest[1].(*string) = row[1]
	return nil
}
func (r *pairRows) Err() error                                   { return nil }
func (r *pairRows) Close()                                       {}
func (r *pairRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *pairRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *pairRows) Values() ([]any, error)                       { return nil, nil }
func (r *pairRows) RawValues() [][]byte                          { return nil }
func (r *pairRows) Conn() *pgx.Conn                              { return nil }

// --- least-privilege subset-check (ErrPermissionNotHeld → 403) ---

// Create a role with a permission outside the caller's set → 403 forbidden.
func TestRoleHandler_Create_PermissionNotHeld_403(t *testing.T) {
	// caller holds only role.create; tries to create a role with `*`.
	pool := &rbacFakePool{callerPermsExplicit: true, callerPermsSet: []string{"role.create"}}
	h := newRoleHandler(t, pool)
	_, err := h.CreateTyped(context.Background(), claimsFor("archon-sub"),
		RoleCreateInput{Name: "escalation", Permissions: []string{"*"}})
	wantProblem(t, err, problem.TypeForbidden)
}

// Update a role: adding a permission not held → 403 forbidden.
func TestRoleHandler_Update_PermissionNotHeld_403(t *testing.T) {
	// The role's old set is role.create; caller holds only role.create;
	// adds operator.create (outside the set).
	pool := &rbacFakePool{
		lockRoleFound:       true,
		rolePerms:           []string{"role.create"},
		callerPermsExplicit: true,
		callerPermsSet:      []string{"role.create"},
	}
	h := newRoleHandler(t, pool)
	_, err := h.UpdatePermissionsTyped(context.Background(), claimsFor("archon-sub"),
		UpdatePermissionsInput{Name: "target", Permissions: []string{"role.create", "operator.create"}})
	wantProblem(t, err, problem.TypeForbidden)
}

// GrantOperator: granting a role that contains a permission outside the caller's set → 403.
func TestRoleHandler_GrantOperator_PermissionNotHeld_403(t *testing.T) {
	// The granted role gives `*` (rolePerms); caller holds only role.create.
	pool := &rbacFakePool{
		lockRoleFound:       true,
		rolePerms:           []string{"*"},
		callerPermsExplicit: true,
		callerPermsSet:      []string{"role.create", "role.grant-operator"},
	}
	h := newRoleHandler(t, pool)
	_, err := h.GrantOperatorTyped(context.Background(), claimsFor("archon-sub"), "powerful", "archon-victim")
	wantProblem(t, err, problem.TypeForbidden)
}

// --- Derived roles (ADR-078, NIM-181) ---
//
// The API surface for parent_role. The attenuation rules themselves belong to
// rbac.Service (guarded in rbac/attenuate_integration_test.go against a real DB);
// what these pin is that the surface REACHES them — a handler that quietly dropped
// ParentRole would create a plain role where the operator asked for a bounded one,
// and every refusal below would silently turn into a 201.

// dbaChain — the parent `dba`: a root role granting incarnation.get at coven=dba
// through its default_scope.
var dbaChain = []chainRow{{name: "dba", scope: "coven=dba", permission: "incarnation.get"}}

// dbaPermScopedChain — the same rights, narrowed in the PERMISSION rather than in
// default_scope. The parent then has no ceiling to conjoin, so only the set
// intersection can refuse a child that reaches outside it (ADR-078(d): the two
// mechanisms, neither sufficient alone).
var dbaPermScopedChain = []chainRow{{name: "dba", permission: "incarnation.get on coven=dba"}}

func TestRoleHandler_Create_DerivedWithinParent_201(t *testing.T) {
	pool := &rbacFakePool{parentChain: dbaChain}
	h := newRoleHandler(t, pool)
	parent, delta := "dba", "trait.project=aboba"
	_, err := h.CreateTyped(context.Background(), claimsFor("archon-alice"),
		RoleCreateInput{Name: "dba-aboba", Permissions: []string{"incarnation.get"}, DefaultScope: &delta, ParentRole: &parent})
	if err != nil {
		t.Fatalf("CreateTyped: %v", err)
	}
}

// TestRoleHandler_Create_BeyondParent_403 — the headline guard of the ticket: a
// permission the chosen parent does not hold is refused at the new surface, not
// stored and silently dropped later.
func TestRoleHandler_Create_BeyondParent_403(t *testing.T) {
	pool := &rbacFakePool{parentChain: dbaChain}
	h := newRoleHandler(t, pool)
	parent := "dba"
	_, err := h.CreateTyped(context.Background(), claimsFor("archon-alice"),
		RoleCreateInput{Name: "dba-aboba", Permissions: []string{"incarnation.destroy"}, ParentRole: &parent})
	wantProblem(t, err, problem.TypeForbidden)
}

// TestRoleHandler_Create_WiderScopeThanParent_403 — the same refusal by the other
// mechanism: the permission is the parent's, the AREA is not. Against a parent whose
// narrowing lives in a per-permission scope there is no ceiling to conjoin, so a
// child reaching sideways — or dropping the scope entirely — is refused by the set
// intersection instead.
func TestRoleHandler_Create_WiderScopeThanParent_403(t *testing.T) {
	tests := []struct {
		name string
		perm string
	}{
		{"wider value set", "incarnation.get on coven in (dba, prod)"},
		{"bare against a scoped parent", "incarnation.get"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pool := &rbacFakePool{parentChain: dbaPermScopedChain}
			h := newRoleHandler(t, pool)
			parent := "dba"
			_, err := h.CreateTyped(context.Background(), claimsFor("archon-alice"),
				RoleCreateInput{Name: "dba-prod", Permissions: []string{tc.perm}, ParentRole: &parent})
			wantProblem(t, err, problem.TypeForbidden)
		})
	}
}

// TestRoleHandler_Create_UnknownParent_404 — a parent that isn't in the catalog is a
// 404 about the parent, not a 201 for a role with a dangling ceiling.
func TestRoleHandler_Create_UnknownParent_404(t *testing.T) {
	pool := &rbacFakePool{} // no chain rows → the parent doesn't exist
	h := newRoleHandler(t, pool)
	parent := "ghost"
	_, err := h.CreateTyped(context.Background(), claimsFor("archon-alice"),
		RoleCreateInput{Name: "orphan", Permissions: []string{"incarnation.get"}, ParentRole: &parent})
	wantProblem(t, err, problem.TypeRoleNotFound)
}

// TestRoleHandler_Create_SelfParent_422 — a role naming itself has no root, so there
// is no ceiling to attenuate against: malformed input (422), not a missing right.
func TestRoleHandler_Create_SelfParent_422(t *testing.T) {
	h := newRoleHandler(t, &rbacFakePool{})
	self := "loop"
	_, err := h.CreateTyped(context.Background(), claimsFor("archon-alice"),
		RoleCreateInput{Name: "loop", Permissions: []string{"incarnation.get"}, ParentRole: &self})
	wantProblem(t, err, problem.TypeValidationFailed)
}

// TestRoleHandler_Create_AuditCarriesParentAndDelta — an audit record of an
// authorization change must answer what the role is bounded by. The permission list
// alone does not: on a derived role it is the delta, and the ceiling lives in
// parent_role. Both keys are present even for a plain role (null), so a reader never
// has to guess whether the field was omitted or the role was not derived.
func TestRoleHandler_Create_AuditCarriesParentAndDelta(t *testing.T) {
	pool := &rbacFakePool{parentChain: dbaChain}
	h := newRoleHandler(t, pool)
	parent, delta := "dba", "trait.project=aboba"
	reply, err := h.CreateTyped(context.Background(), claimsFor("archon-alice"),
		RoleCreateInput{Name: "dba-aboba", Permissions: []string{"incarnation.get"}, DefaultScope: &delta, ParentRole: &parent})
	if err != nil {
		t.Fatalf("CreateTyped: %v", err)
	}

	payload := reply.AuditPayload()
	gotParent, ok := payload["parent_role"].(*string)
	if !ok || gotParent == nil || *gotParent != "dba" {
		t.Errorf("audit parent_role = %v, want dba", payload["parent_role"])
	}
	gotScope, ok := payload["default_scope"].(*string)
	if !ok || gotScope == nil || *gotScope != "trait.project=aboba" {
		t.Errorf("audit default_scope = %v, want trait.project=aboba", payload["default_scope"])
	}

	plain, err := h.CreateTyped(context.Background(), claimsFor("archon-alice"),
		RoleCreateInput{Name: "ops", Permissions: []string{"incarnation.get"}})
	if err != nil {
		t.Fatalf("CreateTyped (plain): %v", err)
	}
	for _, key := range []string{"parent_role", "default_scope"} {
		v, present := plain.AuditPayload()[key]
		if !present {
			t.Errorf("audit %s missing on a plain role, want an explicit null", key)
		}
		if p, _ := v.(*string); p != nil {
			t.Errorf("audit %s = %v on a plain role, want null", key, *p)
		}
	}
}

// TestRoleHandler_Update_BeyondParent_403 — the gate re-runs on update (ADR-078(h)):
// otherwise the create check would be one PATCH wide.
func TestRoleHandler_Update_BeyondParent_403(t *testing.T) {
	pool := &rbacFakePool{lockRoleFound: true, parentChain: dbaChain}
	h := newRoleHandler(t, pool)
	parent := "dba"
	_, err := h.UpdatePermissionsTyped(context.Background(), claimsFor("archon-alice"),
		UpdatePermissionsInput{
			Name:          "dba-aboba",
			Permissions:   []string{"incarnation.destroy"},
			SetParentRole: true,
			ParentRole:    &parent,
		})
	wantProblem(t, err, problem.TypeForbidden)
}

// TestRoleHandler_Update_AuditRecordsOnlyWhatWasSent — PATCH presence in the audit
// record: an absent key means "untouched", a present null means "cleared", and the
// two are different authorization changes (clearing a parent turns the child's delta
// into an absolute scope). Recording an untouched field as null would report a
// re-rooting that never happened.
func TestRoleHandler_Update_AuditRecordsOnlyWhatWasSent(t *testing.T) {
	pool := &rbacFakePool{lockRoleFound: true, survivors: []string{"archon-root"}}
	h := newRoleHandler(t, pool)

	untouched, err := h.UpdatePermissionsTyped(context.Background(), claimsFor("archon-alice"),
		UpdatePermissionsInput{Name: "ops", Permissions: []string{"incarnation.get"}})
	if err != nil {
		t.Fatalf("UpdatePermissionsTyped: %v", err)
	}
	for _, key := range []string{"parent_role", "default_scope"} {
		if _, present := untouched.AuditPayload()[key]; present {
			t.Errorf("audit carries %s for a request that did not send it", key)
		}
	}

	cleared, err := h.UpdatePermissionsTyped(context.Background(), claimsFor("archon-alice"),
		UpdatePermissionsInput{
			Name: "ops", Permissions: []string{"incarnation.get"},
			SetParentRole: true, ParentRole: nil,
			SetDefaultScope: true, DefaultScope: nil,
		})
	if err != nil {
		t.Fatalf("UpdatePermissionsTyped (cleared): %v", err)
	}
	for _, key := range []string{"parent_role", "default_scope"} {
		v, present := cleared.AuditPayload()[key]
		if !present {
			t.Fatalf("audit missing %s for an explicit clear", key)
		}
		if p, _ := v.(*string); p != nil {
			t.Errorf("audit %s = %v, want null", key, *p)
		}
	}
}

// derivedListPool — a catalog with a two-hop chain: `dba` (root, coven=dba) →
// `dba-aboba` (delta trait.project=aboba, one row the parent covers and one it
// does not).
type derivedListPool struct{ rbacFakePool }

func (p *derivedListPool) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	switch {
	case contains(sql, "SELECT name, description, builtin, default_scope, parent_role FROM rbac_roles"):
		return &roleViewRows{rows: [][5]any{
			{"dba", "", false, ptrStr("coven=dba"), nil},
			{"dba-aboba", "", false, ptrStr("trait.project=aboba"), ptrStr("dba")},
		}}, nil
	case contains(sql, "SELECT role_name, permission FROM rbac_role_permissions"):
		return &pairRows{rows: [][2]string{
			{"dba", "incarnation.get"},
			{"dba-aboba", "incarnation.get"},
			{"dba-aboba", "incarnation.destroy"},
		}}, nil
	case contains(sql, "SELECT role_name, aid FROM rbac_role_operators"):
		return &pairRows{}, nil
	}
	return nil, errors.New("derivedListPool.Query: unexpected SQL: " + sql)
}

// TestRoleHandler_List_ResolvesTheChain — role.list publishes the resolved form
// alongside the stored one (ADR-078), so a consumer never re-derives inheritance.
// The two must DIFFER here: the child's stored rows include one the parent does not
// hold, and its stored scope is only the delta.
func TestRoleHandler_List_ResolvesTheChain(t *testing.T) {
	svc, err := rbac.NewService(rbac.ServiceDeps{Pool: &derivedListPool{}})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	page, err := NewRoleHandler(svc, nil).ListTyped(context.Background())
	if err != nil {
		t.Fatalf("ListTyped: %v", err)
	}

	var child RoleView
	for _, v := range page.Items {
		if v.Name == "dba-aboba" {
			child = v
		}
	}
	if child.ParentRole != "dba" {
		t.Fatalf("parent_role = %q, want dba", child.ParentRole)
	}
	if len(child.EffectivePermissions) != 1 || child.EffectivePermissions[0] != "incarnation.get" {
		t.Errorf("effective permissions = %v, want [incarnation.get] — incarnation.destroy is not the parent's to give",
			child.EffectivePermissions)
	}
	if len(child.Permissions) != 2 {
		t.Errorf("stored permissions = %v, want both rows as written", child.Permissions)
	}
	if want := "coven=dba AND trait.project=aboba"; child.EffectiveScope != want {
		t.Errorf("effective scope = %q, want %q", child.EffectiveScope, want)
	}
	if child.DefaultScope != "trait.project=aboba" {
		t.Errorf("stored default_scope = %q, want the delta alone", child.DefaultScope)
	}
}
