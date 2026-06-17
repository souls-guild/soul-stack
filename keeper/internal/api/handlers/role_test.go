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

// claimsFor / wantProblem — общие test-helper-ы handlers-пакета (errand_test.go /
// operator_test.go); handler-native T5d-тесты role/synod зовут *Typed напрямую через них.

// rbacFakePool — узкий мок [rbac.ServicePool] для unit-тестов RoleHandler-а.
// Классифицирует SQL по подстроке и отдаёт заданный тестом исход. Покрывает
// ТРАНСПОРТ (декод / маппинг sentinel→problem / статусы); консистентность
// SQL-логики Service-а валидируют rbac/crud_integration_test.go (testcontainers
// PG). Tx-обёртка проксирует Exec/Query обратно на pool; Commit/Rollback no-op.
type rbacFakePool struct {
	// lockRole — исход SELECT builtin FOR UPDATE (lockRole):
	// found=false → ErrRoleNotFound (пустой row-set); builtin → значение.
	lockRoleFound bool
	lockRoleValue bool

	// lockRoleOperatorFound — исход SELECT 1 FOR UPDATE (lockRoleOperator):
	// false → ErrRoleOperatorNotFound.
	lockRoleOperatorFound bool

	// rolePerms — permissions роли (rolePermissions SELECT): управляет тем,
	// даёт ли роль `*` (→ нужна self-lockout-проба).
	rolePerms []string

	// callerPermsSet — эффективные permissions caller-а (subset-check
	// SELECT rp.permission … JOIN). nil → дефолт `["*"]` (caller=cluster-admin),
	// чтобы транспорт-тесты без явного least-privilege-сценария проходили
	// subset-check. callerPermsExplicit отличает «не задано» (дефолт `*`) от
	// «задано пустым» (caller без прав).
	callerPermsSet      []string
	callerPermsExplicit bool

	// survivors — результат self-lockout-проб (lockWildcardAdmins*): пусто →
	// ErrWouldLockOutCluster.
	survivors []string

	// roleScope — RAW default_scope роли (roleDefaultScope SELECT, ADR-047 S1):
	// nil → NULL (роль без scope, bare-perms unrestricted — дефолт транспорт-
	// тестов). Задаётся явно сценарием default_scope-эскалации.
	roleScope *string

	// insertRoleErr — ошибка INSERT INTO rbac_roles (Create): unique → 409.
	insertRoleErr error

	// insertMembershipErr — ошибка INSERT membership (GrantOperator / Synod
	// AddOperator / GrantRole): FK → 404.
	insertMembershipErr error

	// insertSynodErr — ошибка INSERT INTO synods (CreateSynod): unique → 409
	// (ADR-049). lockSynodFound управляет существованием группы для мутаций.
	insertSynodErr  error
	lockSynodFound  bool
	lockSynodValue  bool
	synodRolesValue []string

	// updateSynodFound — исход UPDATE synods SET description (UpdateSynodDescription,
	// ADR-049 amend): true → RowsAffected 1 (группа есть), false → 0 → ErrSynodNotFound.
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
	// Synod-ветки (ADR-049) — transport-тесты SynodHandler-а делят этот fake.
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
		// UpdateSynodDescription (ADR-049 amend): found → 1 row, иначе 0 → 404.
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
		// lockRole: пусто → ErrRoleNotFound; иначе один bool-row.
		if !p.lockRoleFound {
			return &boolRows{}, nil
		}
		return &boolRows{values: []bool{p.lockRoleValue}}, nil
	case contains(sql, "SELECT 1 FROM rbac_role_operators"):
		// lockRoleOperator: пусто → ErrRoleOperatorNotFound.
		if !p.lockRoleOperatorFound {
			return &intRows{}, nil
		}
		return &intRows{values: []int{1}}, nil
	case contains(sql, "SELECT rp.permission"):
		// caller-perms (subset-check): дефолт — `["*"]` (caller=cluster-admin),
		// если тест не задал набор явно.
		if p.callerPermsExplicit {
			return &roleStringRows{values: p.callerPermsSet}, nil
		}
		return &roleStringRows{values: []string{"*"}}, nil
	case contains(sql, "SELECT permission FROM rbac_role_permissions"):
		return &roleStringRows{values: p.rolePerms}, nil
	case contains(sql, "SELECT default_scope FROM rbac_roles"):
		// roleDefaultScope (subset-check granted-сторона): один nullable-row.
		return &nullStringRows{value: p.roleScope}, nil
	case contains(sql, "SELECT builtin FROM synods"):
		// lockSynod: пусто → ErrSynodNotFound; иначе один bool-row.
		if !p.lockSynodFound {
			return &boolRows{}, nil
		}
		return &boolRows{values: []bool{p.lockSynodValue}}, nil
	case contains(sql, "SELECT 1 FROM synod_operators"):
		// lockSynodOperator: пусто → ErrSynodOperatorNotFound.
		if !p.lockRoleOperatorFound {
			return &intRows{}, nil
		}
		return &intRows{values: []int{1}}, nil
	case contains(sql, "SELECT 1 FROM synod_roles"):
		// lockSynodRole: пусто → ErrSynodRoleNotFound.
		if !p.lockRoleOperatorFound {
			return &intRows{}, nil
		}
		return &intRows{values: []int{1}}, nil
	case contains(sql, "SELECT role_name FROM synod_roles"):
		// synodRoles (subset-check add-operator): набор ролей bundle.
		return &roleStringRows{values: p.synodRolesValue}, nil
	case contains(sql, "FROM synod_roles sr"):
		// synodGivesWildcard-проба: пусто → группа `*` не бандлит.
		return &intRows{}, nil
	case contains(sql, "FROM synod_operators"):
		// Synod-ветка self-lockout-пробы (ADR-049(f)): второй locking-запрос
		// по synod_operators. Эти handler-unit-сценарии групповых админов не
		// моделируют — пусто; их покрывают rbac integration-guard-тесты.
		return &roleStringRows{}, nil
	case contains(sql, "FOR UPDATE OF ro, rp, o"):
		// прямая self-lockout-проба (excluding role / pair / core).
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

// contains — короткий substring-helper (bytes-free).
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

// rbacFakeTx проксирует Exec/Query на pool; остальное — panic (не должно
// вызываться в scope-е этих тестов).
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

// --- минимальные pgx.Rows-обёртки для bool / int / string одностолбцовых выборок ---

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

// nullStringRows — один row с nullable default_scope (scan в *string).
// value=nil → NULL (роль без scope). Один row всегда (роль существует, её
// заранее залочил lockRole).
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

// newRoleHandler собирает RoleHandler поверх rbac.Service на fake-pool.
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
	// Роль даёт `*`, выживших админов нет → ErrWouldLockOutCluster.
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
	// Старый набор даёт `*`, новый — нет, выживших нет → lockout.
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

// TestRoleHandler_GrantOperator_CallerAIDFromClaims проверяет, что CallerAID
// (granted_by_aid) берётся из claims subject и доезжает до Service-вызова.
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

// grantSpyPool — rbacFakePool, перехватывающий granted_by_aid (3-й arg
// INSERT-membership-а) для проверки проброса CallerAID из claims.
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
	// membership есть, роль даёт `*`, выживших нет → lockout.
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

// listFakePool отдаёт фиксированный каталог из трёх SELECT-ов (LoadRoleViews).
type listFakePool struct{ rbacFakePool }

func (p *listFakePool) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	switch {
	case contains(sql, "SELECT name, description, builtin, default_scope FROM rbac_roles"):
		return &roleViewRows{rows: [][4]any{
			{"admins", "cluster admins", true, nil},
			{"ops", "ops team", false, ptrStr("coven=prod")},
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
	// Детерминированный порядок (ORDER BY name): admins, ops.
	if page.Items[0].Name != "admins" || !page.Items[0].Builtin {
		t.Errorf("item[0] = %+v", page.Items[0])
	}
	if len(page.Items[0].Permissions) != 1 || page.Items[0].Permissions[0] != "*" {
		t.Errorf("admins permissions = %v", page.Items[0].Permissions)
	}
	if len(page.Items[0].Operators) != 1 || page.Items[0].Operators[0] != "archon-alice" {
		t.Errorf("admins operators = %v", page.Items[0].Operators)
	}
	// ops без operators — non-nil пустой срез (на wire `[]`, native-проекция в api).
	if page.Items[1].Operators == nil {
		t.Errorf("ops operators is nil, want empty slice")
	}
	// ADR-047 S1: default_scope (handler-flat RoleView несёт RAW string) — admins
	// "" (NULL), ops="coven=prod". Nullable-форму wire строит native-проекция.
	if page.Items[0].DefaultScope != "" {
		t.Errorf("admins default_scope = %q, want \"\" (NULL)", page.Items[0].DefaultScope)
	}
	if page.Items[1].DefaultScope != "coven=prod" {
		t.Errorf("ops default_scope = %q, want coven=prod", page.Items[1].DefaultScope)
	}
}

// ptrStr — *string-литерал для nullable default_scope в фикстурах.
func ptrStr(s string) *string { return &s }

// roleViewRows — четырёхстолбцовые строки (name, description, builtin,
// default_scope). default_scope nullable: row[3] == nil → NULL.
type roleViewRows struct {
	rows [][4]any
	idx  int
}

func (r *roleViewRows) Next() bool { r.idx++; return r.idx <= len(r.rows) }
func (r *roleViewRows) Scan(dest ...any) error {
	row := r.rows[r.idx-1]
	*dest[0].(*string) = row[0].(string)
	*dest[1].(*string) = row[1].(string)
	*dest[2].(*bool) = row[2].(bool)
	scopeDest := dest[3].(**string)
	if row[3] == nil {
		*scopeDest = nil
	} else {
		*scopeDest = row[3].(*string)
	}
	return nil
}
func (r *roleViewRows) Err() error                                   { return nil }
func (r *roleViewRows) Close()                                       {}
func (r *roleViewRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *roleViewRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *roleViewRows) Values() ([]any, error)                       { return nil, nil }
func (r *roleViewRows) RawValues() [][]byte                          { return nil }
func (r *roleViewRows) Conn() *pgx.Conn                              { return nil }

// pairRows — двустолбцовые строки string/string.
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

// Create роли с правом вне набора caller-а → 403 forbidden.
func TestRoleHandler_Create_PermissionNotHeld_403(t *testing.T) {
	// caller держит только role.create; пытается создать роль с `*`.
	pool := &rbacFakePool{callerPermsExplicit: true, callerPermsSet: []string{"role.create"}}
	h := newRoleHandler(t, pool)
	_, err := h.CreateTyped(context.Background(), claimsFor("archon-sub"),
		RoleCreateInput{Name: "escalation", Permissions: []string{"*"}})
	wantProblem(t, err, problem.TypeForbidden)
}

// Update роли: добавление чужого права → 403 forbidden.
func TestRoleHandler_Update_PermissionNotHeld_403(t *testing.T) {
	// Старый набор роли — role.create; caller держит только role.create;
	// добавляет operator.create (вне набора).
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

// GrantOperator: грант роли, содержащей право вне набора caller-а → 403.
func TestRoleHandler_GrantOperator_PermissionNotHeld_403(t *testing.T) {
	// Грантящаяся роль даёт `*` (rolePerms); caller держит только role.create.
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
