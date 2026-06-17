//go:build integration

// Integration-матрица RBAC-CRUD + self-lockout-ядра (ADR-028 Фаза 2 Slice 1)
// через testcontainers-go. Делит контейнер / TestMain / resetRBAC / seedOperator
// с integration_test.go (тот же пакет rbac).
//
// Self-lockout-матрица — qa-blocker: каждая из трёх мутаций (role.delete /
// role.update / role.revoke-operator) над последним `*`-путём → lockout, над
// НЕ-последним → проходит; + конкурентность (R2/R5) под FOR UPDATE.
//
// Запуск:
//
//	cd keeper && SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1 go test -tags=integration -race -count=1 ./internal/rbac/

package rbac

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// newService собирает Service против реального pool-а.
func newService(t *testing.T) *Service {
	t.Helper()
	s, err := NewService(ServiceDeps{Pool: integrationPool})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return s
}

// seedClusterAdmin выдаёт caller-у membership builtin-роли cluster-admin (`*`)
// в rbac_role_operators. Без этого subset-check (least-privilege, subset.go)
// видит у caller-а 0 эффективных permissions и отказывает в мутации — модель-C
// (RBAC в Postgres) резолвит права caller-а из реальной membership, а не из
// config-RBAC enforcer-а. resetRBAC ре-сидит саму роль cluster-admin с `*`;
// здесь только привязываем к ней оператора.
func seedClusterAdmin(t *testing.T, aid string) {
	t.Helper()
	if err := GrantOperator(context.Background(), integrationPool, "cluster-admin", aid, nil); err != nil {
		t.Fatalf("seedClusterAdmin(%s): %v", aid, err)
	}
}

// insertRole — прямой INSERT кастомной роли + её permissions (минуя service,
// для подготовки фикстур независимо от self-lockout-границы). builtin=false.
func insertRole(t *testing.T, name string, perms ...string) {
	t.Helper()
	ctx := context.Background()
	if _, err := integrationPool.Exec(ctx,
		`INSERT INTO rbac_roles (name, builtin) VALUES ($1, false)`, name); err != nil {
		t.Fatalf("insert role %q: %v", name, err)
	}
	for _, p := range perms {
		if _, err := integrationPool.Exec(ctx,
			`INSERT INTO rbac_role_permissions (role_name, permission) VALUES ($1, $2)`, name, p); err != nil {
			t.Fatalf("insert perm %q for %q: %v", p, name, err)
		}
	}
}

// rolePerms читает permission-строки роли напрямую (для assert-ов).
func rolePerms(t *testing.T, name string) []string {
	t.Helper()
	rows, err := integrationPool.Query(context.Background(),
		`SELECT permission FROM rbac_role_permissions WHERE role_name = $1 ORDER BY permission`, name)
	if err != nil {
		t.Fatalf("rolePerms %q: %v", name, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan perm: %v", err)
		}
		out = append(out, p)
	}
	return out
}

// roleExists / membershipExists — point-проверки наличия строк.
func roleExists(t *testing.T, name string) bool {
	t.Helper()
	var n int
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM rbac_roles WHERE name = $1`, name).Scan(&n); err != nil {
		t.Fatalf("roleExists %q: %v", name, err)
	}
	return n > 0
}

func membershipCount(t *testing.T, roleName string) int {
	t.Helper()
	var n int
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM rbac_role_operators WHERE role_name = $1`, roleName).Scan(&n); err != nil {
		t.Fatalf("membershipCount %q: %v", roleName, err)
	}
	return n
}

func permCount(t *testing.T, roleName string) int {
	t.Helper()
	var n int
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM rbac_role_permissions WHERE role_name = $1`, roleName).Scan(&n); err != nil {
		t.Fatalf("permCount %q: %v", roleName, err)
	}
	return n
}

// ---- CreateRole ----

func TestIntegration_CreateRole_Happy(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	seedClusterAdmin(t, "archon-alice")
	s := newService(t)
	caller := "archon-alice"

	err := s.CreateRole(context.Background(), CreateRoleInput{
		Name:        "soul-reader",
		Description: "видит Souls",
		Permissions: []string{"soul.list", "incarnation.get"},
		CallerAID:   caller,
	})
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	if !roleExists(t, "soul-reader") {
		t.Fatal("роль soul-reader не создана")
	}
	if got := rolePerms(t, "soul-reader"); len(got) != 2 {
		t.Errorf("permissions = %v, want 2", got)
	}
	// created_by_aid = caller.
	var by *string
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT created_by_aid FROM rbac_roles WHERE name = 'soul-reader'`).Scan(&by); err != nil {
		t.Fatalf("scan created_by_aid: %v", err)
	}
	if by == nil || *by != caller {
		t.Errorf("created_by_aid = %v, want %q", by, caller)
	}
}

// TestIntegration_CreateRole_DefaultScope — ADR-047 S1: default_scope
// персистится и наследуется bare-permission-ами через ResolvePurview
// (round-trip CreateRole → LoadSnapshot → enforcer).
func TestIntegration_CreateRole_DefaultScope(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	seedClusterAdmin(t, "archon-alice")
	alice := "archon-alice"
	seedOperator(t, "archon-prod", &alice)
	s := newService(t)

	scope := "coven=prod"
	if err := s.CreateRole(context.Background(), CreateRoleInput{
		Name:         "prod-ops",
		Permissions:  []string{"incarnation.run"},
		CallerAID:    "archon-alice",
		DefaultScope: &scope,
	}); err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	if err := s.GrantOperator(context.Background(), GrantOperatorInput{
		RoleName: "prod-ops", AID: "archon-prod", CallerAID: &alice,
	}); err != nil {
		t.Fatalf("GrantOperator: %v", err)
	}

	// Колонка default_scope записана.
	var got *string
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT default_scope FROM rbac_roles WHERE name = 'prod-ops'`).Scan(&got); err != nil {
		t.Fatalf("scan default_scope: %v", err)
	}
	if got == nil || *got != "coven=prod" {
		t.Fatalf("default_scope = %v, want coven=prod", got)
	}

	// Наследование через enforcer: bare-perm incarnation.run → covens=[prod].
	snap, err := LoadSnapshot(context.Background(), integrationPool)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	enf, err := NewEnforcerFromSnapshot(snap)
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	p := enf.ResolvePurview("archon-prod", "incarnation", "run")
	if p.Unrestricted {
		t.Errorf("Unrestricted=true, want false (bare-perm наследует default_scope)")
	}
	if len(p.Covens) != 1 || p.Covens[0] != "prod" {
		t.Errorf("Covens=%v, want [prod]", p.Covens)
	}
}

func TestIntegration_CreateRole_Duplicate(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	seedClusterAdmin(t, "archon-alice")
	s := newService(t)

	in := CreateRoleInput{Name: "dup-role", Permissions: []string{"soul.list"}, CallerAID: "archon-alice"}
	if err := s.CreateRole(context.Background(), in); err != nil {
		t.Fatalf("CreateRole #1: %v", err)
	}
	err := s.CreateRole(context.Background(), in)
	if !errors.Is(err, ErrRoleAlreadyExists) {
		t.Fatalf("CreateRole #2: err = %v, want ErrRoleAlreadyExists", err)
	}
}

func TestIntegration_CreateRole_BadPermission(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	s := newService(t)

	err := s.CreateRole(context.Background(), CreateRoleInput{
		Name:        "broken",
		Permissions: []string{"unknown.permission"},
		CallerAID:   "archon-alice",
	})
	if err == nil {
		t.Fatal("CreateRole с битым permission: want error, got nil")
	}
	if roleExists(t, "broken") {
		t.Error("роль broken создана несмотря на validation-ошибку (валидация должна быть ДО tx)")
	}
}

func TestIntegration_CreateRole_BadName(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	s := newService(t)

	err := s.CreateRole(context.Background(), CreateRoleInput{
		Name: "Bad_Name", Permissions: []string{"soul.list"}, CallerAID: "archon-alice",
	})
	if !errors.Is(err, ErrInvalidRoleName) {
		t.Fatalf("err = %v, want ErrInvalidRoleName", err)
	}
}

// ---- DeleteRole ----

func TestIntegration_DeleteRole_Happy(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	insertRole(t, "tmp-role", "soul.list")
	s := newService(t)

	if err := s.DeleteRole(context.Background(), "tmp-role"); err != nil {
		t.Fatalf("DeleteRole: %v", err)
	}
	if roleExists(t, "tmp-role") {
		t.Error("роль tmp-role не удалена")
	}
}

func TestIntegration_DeleteRole_NotFound(t *testing.T) {
	resetRBAC(t)
	s := newService(t)
	err := s.DeleteRole(context.Background(), "ghost-role")
	if !errors.Is(err, ErrRoleNotFound) {
		t.Fatalf("err = %v, want ErrRoleNotFound", err)
	}
}

func TestIntegration_DeleteRole_Builtin(t *testing.T) {
	resetRBAC(t)
	s := newService(t)
	// cluster-admin (builtin=true) есть из re-seed-а resetRBAC.
	err := s.DeleteRole(context.Background(), "cluster-admin")
	if !errors.Is(err, ErrRoleBuiltin) {
		t.Fatalf("err = %v, want ErrRoleBuiltin", err)
	}
	if !roleExists(t, "cluster-admin") {
		t.Error("builtin-роль удалена")
	}
}

func TestIntegration_DeleteRole_Cascade(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	insertRole(t, "casc-role", "soul.list", "incarnation.get")
	if err := GrantOperator(context.Background(), integrationPool, "casc-role", "archon-alice", nil); err != nil {
		t.Fatalf("grant: %v", err)
	}
	s := newService(t)

	if err := s.DeleteRole(context.Background(), "casc-role"); err != nil {
		t.Fatalf("DeleteRole: %v", err)
	}
	if permCount(t, "casc-role") != 0 {
		t.Error("permissions не снесены каскадом")
	}
	if membershipCount(t, "casc-role") != 0 {
		t.Error("membership не снесён каскадом")
	}
}

// ---- UpdateRolePermissions ----

func TestIntegration_UpdateRolePermissions_Replace(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	seedClusterAdmin(t, "archon-alice")
	insertRole(t, "upd-role", "soul.list")
	s := newService(t)

	err := s.UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name:        "upd-role",
		Permissions: []string{"incarnation.get", "incarnation.list", "push.apply"},
		CallerAID:   "archon-alice",
	})
	if err != nil {
		t.Fatalf("UpdateRolePermissions: %v", err)
	}
	got := rolePerms(t, "upd-role")
	if len(got) != 3 {
		t.Fatalf("permissions = %v, want 3 (replace, старый soul.list снят)", got)
	}
	for _, p := range got {
		if p == "soul.list" {
			t.Error("старый permission soul.list не снят (replace-семантика нарушена)")
		}
	}
}

func TestIntegration_UpdateRolePermissions_NotFound(t *testing.T) {
	resetRBAC(t)
	s := newService(t)
	err := s.UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name: "ghost-role", Permissions: []string{"soul.list"},
	})
	if !errors.Is(err, ErrRoleNotFound) {
		t.Fatalf("err = %v, want ErrRoleNotFound", err)
	}
}

func TestIntegration_UpdateRolePermissions_Builtin(t *testing.T) {
	resetRBAC(t)
	s := newService(t)
	err := s.UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name: "cluster-admin", Permissions: []string{"soul.list"},
	})
	if !errors.Is(err, ErrRoleBuiltin) {
		t.Fatalf("err = %v, want ErrRoleBuiltin", err)
	}
}

// TestIntegration_UpdateRolePermissions_EmptySetRemovesWildcard — пустой набор
// снимает все permissions (включая `*`). Проверяется на НЕ-последнем `*`-пути
// (есть второй admin через cluster-admin), иначе сработал бы self-lockout.
func TestIntegration_UpdateRolePermissions_EmptySetRemovesWildcard(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	alice := "archon-alice"
	seedOperator(t, "archon-bob", &alice)
	// alice — admin через cluster-admin; extra-admin-role даёт `*` для bob.
	if err := GrantOperator(context.Background(), integrationPool, "cluster-admin", "archon-alice", nil); err != nil {
		t.Fatalf("grant alice: %v", err)
	}
	insertRole(t, "extra-admin", "*")
	if err := GrantOperator(context.Background(), integrationPool, "extra-admin", "archon-bob", &alice); err != nil {
		t.Fatalf("grant bob: %v", err)
	}
	s := newService(t)

	// Снять `*` у extra-admin пустым набором — alice остаётся admin → ok.
	if err := s.UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name: "extra-admin", Permissions: nil,
	}); err != nil {
		t.Fatalf("UpdateRolePermissions (empty set): %v", err)
	}
	if permCount(t, "extra-admin") != 0 {
		t.Error("пустой набор не снял permissions")
	}
}

// ---- RevokeOperator ----

func TestIntegration_RevokeOperator_Happy(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	insertRole(t, "viewer", "soul.list")
	if err := GrantOperator(context.Background(), integrationPool, "viewer", "archon-alice", nil); err != nil {
		t.Fatalf("grant: %v", err)
	}
	s := newService(t)

	if err := s.RevokeOperator(context.Background(), RevokeOperatorInput{
		RoleName: "viewer", AID: "archon-alice",
	}); err != nil {
		t.Fatalf("RevokeOperator: %v", err)
	}
	if membershipCount(t, "viewer") != 0 {
		t.Error("membership не снят")
	}
}

func TestIntegration_RevokeOperator_NotFound(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	insertRole(t, "viewer", "soul.list")
	s := newService(t)

	err := s.RevokeOperator(context.Background(), RevokeOperatorInput{
		RoleName: "viewer", AID: "archon-alice",
	})
	if !errors.Is(err, ErrRoleOperatorNotFound) {
		t.Fatalf("err = %v, want ErrRoleOperatorNotFound", err)
	}
}

// ============================================================
// SELF-LOCKOUT МАТРИЦА (qa-blocker)
// ============================================================

// setupSingleAdmin — единственный путь к `*`: alice через cluster-admin.
func setupSingleAdmin(t *testing.T) {
	t.Helper()
	seedOperator(t, "archon-alice", nil)
	if err := GrantOperator(context.Background(), integrationPool, "cluster-admin", "archon-alice", nil); err != nil {
		t.Fatalf("grant alice→cluster-admin: %v", err)
	}
}

// setupTwoAdminPaths — два независимых пути к `*`:
//   - alice через cluster-admin (builtin);
//   - bob через кастомную extra-admin (`*`).
//
// Снятие любого ОДНОГО пути оставляет второй → self-lockout НЕ должен сработать.
func setupTwoAdminPaths(t *testing.T) {
	t.Helper()
	seedOperator(t, "archon-alice", nil)
	alice := "archon-alice"
	seedOperator(t, "archon-bob", &alice)
	if err := GrantOperator(context.Background(), integrationPool, "cluster-admin", "archon-alice", nil); err != nil {
		t.Fatalf("grant alice: %v", err)
	}
	insertRole(t, "extra-admin", "*")
	if err := GrantOperator(context.Background(), integrationPool, "extra-admin", "archon-bob", &alice); err != nil {
		t.Fatalf("grant bob: %v", err)
	}
}

// --- role.delete ---

// DeleteRole последней `*`-роли → lockout. Удаляем extra-admin, когда она
// единственный путь к `*` (cluster-admin без membership-а).
func TestIntegration_SelfLockout_DeleteRole_Last(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	insertRole(t, "extra-admin", "*")
	if err := GrantOperator(context.Background(), integrationPool, "extra-admin", "archon-alice", nil); err != nil {
		t.Fatalf("grant: %v", err)
	}
	s := newService(t)

	err := s.DeleteRole(context.Background(), "extra-admin")
	if !errors.Is(err, ErrWouldLockOutCluster) {
		t.Fatalf("err = %v, want ErrWouldLockOutCluster", err)
	}
	if !roleExists(t, "extra-admin") {
		t.Error("роль удалена несмотря на lockout (tx не откатилась)")
	}
}

// DeleteRole НЕ-последней `*`-роли → проходит (есть второй путь).
func TestIntegration_SelfLockout_DeleteRole_NotLast(t *testing.T) {
	resetRBAC(t)
	setupTwoAdminPaths(t)
	s := newService(t)

	if err := s.DeleteRole(context.Background(), "extra-admin"); err != nil {
		t.Fatalf("DeleteRole (есть второй admin-путь): %v", err)
	}
	if roleExists(t, "extra-admin") {
		t.Error("роль не удалена")
	}
}

// --- role.update (снятие `*`) ---

func TestIntegration_SelfLockout_UpdateRole_Last(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	insertRole(t, "extra-admin", "*")
	if err := GrantOperator(context.Background(), integrationPool, "extra-admin", "archon-alice", nil); err != nil {
		t.Fatalf("grant: %v", err)
	}
	s := newService(t)

	err := s.UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name: "extra-admin", Permissions: []string{"soul.list"}, CallerAID: "archon-alice",
	})
	if !errors.Is(err, ErrWouldLockOutCluster) {
		t.Fatalf("err = %v, want ErrWouldLockOutCluster", err)
	}
	// `*` остался — tx откатилась.
	got := rolePerms(t, "extra-admin")
	if len(got) != 1 || got[0] != "*" {
		t.Errorf("permissions = %v, want [*] (откат)", got)
	}
}

func TestIntegration_SelfLockout_UpdateRole_NotLast(t *testing.T) {
	resetRBAC(t)
	setupTwoAdminPaths(t)
	s := newService(t)

	if err := s.UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name: "extra-admin", Permissions: []string{"soul.list"}, CallerAID: "archon-alice",
	}); err != nil {
		t.Fatalf("UpdateRolePermissions (есть второй admin): %v", err)
	}
}

// UpdateRole, оставляющий `*` в новом наборе → self-lockout НЕ срабатывает,
// даже если это единственный путь (новый набор тоже даёт `*`).
func TestIntegration_SelfLockout_UpdateRole_KeepsWildcard(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	insertRole(t, "extra-admin", "*")
	if err := GrantOperator(context.Background(), integrationPool, "extra-admin", "archon-alice", nil); err != nil {
		t.Fatalf("grant: %v", err)
	}
	s := newService(t)

	if err := s.UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name: "extra-admin", Permissions: []string{"*", "soul.list"}, CallerAID: "archon-alice",
	}); err != nil {
		t.Fatalf("UpdateRolePermissions (новый набор тоже даёт *): %v", err)
	}
}

// --- role.revoke-operator ---

func TestIntegration_SelfLockout_RevokeOperator_Last(t *testing.T) {
	resetRBAC(t)
	setupSingleAdmin(t)
	s := newService(t)

	err := s.RevokeOperator(context.Background(), RevokeOperatorInput{
		RoleName: "cluster-admin", AID: "archon-alice",
	})
	if !errors.Is(err, ErrWouldLockOutCluster) {
		t.Fatalf("err = %v, want ErrWouldLockOutCluster", err)
	}
	if membershipCount(t, "cluster-admin") != 1 {
		t.Error("membership снят несмотря на lockout (tx не откатилась)")
	}
}

func TestIntegration_SelfLockout_RevokeOperator_NotLast(t *testing.T) {
	resetRBAC(t)
	setupTwoAdminPaths(t)
	s := newService(t)

	// Снять bob с extra-admin — alice остаётся admin → ok.
	if err := s.RevokeOperator(context.Background(), RevokeOperatorInput{
		RoleName: "extra-admin", AID: "archon-bob",
	}); err != nil {
		t.Fatalf("RevokeOperator (есть второй admin): %v", err)
	}
}

// AID держит `*` через ДВЕ роли: снятие одной membership-строки его не
// разжалует (он остаётся admin через вторую) → revoke проходит.
func TestIntegration_SelfLockout_RevokeOperator_AdminViaTwoRoles(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	insertRole(t, "extra-admin", "*")
	// alice — admin и через cluster-admin, и через extra-admin.
	if err := GrantOperator(context.Background(), integrationPool, "cluster-admin", "archon-alice", nil); err != nil {
		t.Fatalf("grant cluster-admin: %v", err)
	}
	if err := GrantOperator(context.Background(), integrationPool, "extra-admin", "archon-alice", nil); err != nil {
		t.Fatalf("grant extra-admin: %v", err)
	}
	s := newService(t)

	// Снять alice с extra-admin — она остаётся admin через cluster-admin → ok.
	if err := s.RevokeOperator(context.Background(), RevokeOperatorInput{
		RoleName: "extra-admin", AID: "archon-alice",
	}); err != nil {
		t.Fatalf("RevokeOperator (admin через вторую роль): %v", err)
	}
	// Через cluster-admin alice всё ещё admin.
	if membershipCount(t, "cluster-admin") != 1 {
		t.Error("cluster-admin membership alice потерян")
	}
}

// ============================================================
// КОНКУРЕНТНОСТЬ (R2/R5) — FOR UPDATE сериализует lockout-гонку
// ============================================================

// Две параллельные tx снимают последний `*` разными путями:
//   - revoke alice с cluster-admin;
//   - delete extra-admin (через которую тот же alice — admin).
//
// alice — admin РОВНО через эти два пути. Снятие обоих залочило бы кластер.
// Без FOR UPDATE обе tx прошли бы probe «останется ≥1 admin» (каждая видит
// чужой путь ещё живым) и закоммитили → lockout. С сериализацией ровно одна
// успешна, вторая → lockout. Deadlock-а нет (детерминированный lock-порядок).
func TestIntegration_SelfLockout_Concurrent_TwoPaths(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	insertRole(t, "extra-admin", "*")
	if err := GrantOperator(context.Background(), integrationPool, "cluster-admin", "archon-alice", nil); err != nil {
		t.Fatalf("grant cluster-admin: %v", err)
	}
	if err := GrantOperator(context.Background(), integrationPool, "extra-admin", "archon-alice", nil); err != nil {
		t.Fatalf("grant extra-admin: %v", err)
	}
	s := newService(t)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	start := make(chan struct{})

	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		errs[0] = s.RevokeOperator(context.Background(), RevokeOperatorInput{
			RoleName: "cluster-admin", AID: "archon-alice",
		})
	}()
	go func() {
		defer wg.Done()
		<-start
		errs[1] = s.DeleteRole(context.Background(), "extra-admin")
	}()
	close(start)
	wg.Wait()

	successes, lockouts := 0, 0
	for _, e := range errs {
		switch {
		case e == nil:
			successes++
		case errors.Is(e, ErrWouldLockOutCluster):
			lockouts++
		default:
			t.Fatalf("неожиданная ошибка: %v", e)
		}
	}
	if successes != 1 || lockouts != 1 {
		t.Fatalf("successes=%d lockouts=%d, want 1/1 (сериализация FOR UPDATE)", successes, lockouts)
	}

	// Инвариант: alice осталась admin хотя бы через один путь.
	admins, err := LockEffectiveClusterAdmins(context.Background(), beginRoTx(t))
	if err != nil {
		t.Fatalf("LockEffectiveClusterAdmins: %v", err)
	}
	if len(admins) < 1 {
		t.Fatalf("активных admin-ов %d, want >= 1 (кластер не должен залочиться)", len(admins))
	}
}

// beginRoTx — короткая tx для финального assert-а LockEffectiveClusterAdmins
// (FOR UPDATE требует tx). Tx закрывается t.Cleanup-ом.
func beginRoTx(t *testing.T) ExecQueryRower {
	t.Helper()
	tx, err := integrationPool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin ro tx: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

// grantedByOf читает granted_by_aid одной membership-строки (role, aid).
func grantedByOf(t *testing.T, roleName, aid string) *string {
	t.Helper()
	var by *string
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT granted_by_aid FROM rbac_role_operators WHERE role_name = $1 AND aid = $2`,
		roleName, aid).Scan(&by); err != nil {
		t.Fatalf("grantedByOf (%s -> %s): %v", roleName, aid, err)
	}
	return by
}

// ---- Service.GrantOperator ----

func TestIntegration_ServiceGrantOperator_Happy(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	seedClusterAdmin(t, "archon-alice")
	alice := "archon-alice"
	seedOperator(t, "archon-bob", &alice)
	insertRole(t, "viewer", "soul.list")
	s := newService(t)

	if err := s.GrantOperator(context.Background(), GrantOperatorInput{
		RoleName: "viewer", AID: "archon-bob", CallerAID: &alice,
	}); err != nil {
		t.Fatalf("GrantOperator: %v", err)
	}
	if membershipCount(t, "viewer") != 1 {
		t.Error("membership не вставлен")
	}
	// granted_by_aid = CallerAID.
	if by := grantedByOf(t, "viewer", "archon-bob"); by == nil || *by != alice {
		t.Errorf("granted_by_aid = %v, want %q", by, alice)
	}
}

// CallerAID=nil → granted_by_aid IS NULL (bootstrap-membership без инициатора).
func TestIntegration_ServiceGrantOperator_NilCaller(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	insertRole(t, "viewer", "soul.list")
	s := newService(t)

	if err := s.GrantOperator(context.Background(), GrantOperatorInput{
		RoleName: "viewer", AID: "archon-alice", CallerAID: nil,
	}); err != nil {
		t.Fatalf("GrantOperator: %v", err)
	}
	if by := grantedByOf(t, "viewer", "archon-alice"); by != nil {
		t.Errorf("granted_by_aid = %v, want NULL", *by)
	}
}

// Повторный grant той же пары — no-op (ON CONFLICT DO NOTHING), без ошибки.
func TestIntegration_ServiceGrantOperator_Idempotent(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	insertRole(t, "viewer", "soul.list")
	s := newService(t)
	in := GrantOperatorInput{RoleName: "viewer", AID: "archon-alice"}

	if err := s.GrantOperator(context.Background(), in); err != nil {
		t.Fatalf("GrantOperator #1: %v", err)
	}
	if err := s.GrantOperator(context.Background(), in); err != nil {
		t.Fatalf("GrantOperator #2 (idempotent): %v", err)
	}
	if membershipCount(t, "viewer") != 1 {
		t.Errorf("membership rows = %d, want 1 (idempotent)", membershipCount(t, "viewer"))
	}
}

func TestIntegration_ServiceGrantOperator_RoleNotFound(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	s := newService(t)

	err := s.GrantOperator(context.Background(), GrantOperatorInput{
		RoleName: "ghost-role", AID: "archon-alice",
	})
	if !errors.Is(err, ErrRoleNotFound) {
		t.Fatalf("err = %v, want ErrRoleNotFound", err)
	}
	if membershipCount(t, "ghost-role") != 0 {
		t.Error("membership вставлен для несуществующей роли")
	}
}

func TestIntegration_ServiceGrantOperator_OperatorNotFound(t *testing.T) {
	resetRBAC(t)
	insertRole(t, "viewer", "soul.list")
	s := newService(t)

	err := s.GrantOperator(context.Background(), GrantOperatorInput{
		RoleName: "viewer", AID: "archon-ghost",
	})
	if !errors.Is(err, ErrOperatorNotFound) {
		t.Fatalf("err = %v, want ErrOperatorNotFound", err)
	}
	if membershipCount(t, "viewer") != 0 {
		t.Error("membership вставлен для несуществующего AID (FK должен откатить)")
	}
}

// ---- Service.ListRoles ----

func TestIntegration_ServiceListRoles_WithPermissionsAndOperators(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	alice := "archon-alice"
	seedOperator(t, "archon-bob", &alice)
	insertRole(t, "viewer", "soul.list", "incarnation.get")
	if err := GrantOperator(context.Background(), integrationPool, "viewer", "archon-alice", nil); err != nil {
		t.Fatalf("grant alice: %v", err)
	}
	if err := GrantOperator(context.Background(), integrationPool, "viewer", "archon-bob", &alice); err != nil {
		t.Fatalf("grant bob: %v", err)
	}
	s := newService(t)

	views, err := s.ListRoles(context.Background())
	if err != nil {
		t.Fatalf("ListRoles: %v", err)
	}
	byName := indexViews(views)

	// builtin cluster-admin: builtin=true, `*`, без operators.
	ca, ok := byName["cluster-admin"]
	if !ok {
		t.Fatal("cluster-admin отсутствует в каталоге")
	}
	if !ca.Builtin {
		t.Error("cluster-admin.Builtin = false, want true")
	}
	if len(ca.Permissions) != 1 || ca.Permissions[0] != "*" {
		t.Errorf("cluster-admin.Permissions = %v, want [*]", ca.Permissions)
	}
	if len(ca.Operators) != 0 {
		t.Errorf("cluster-admin.Operators = %v, want empty", ca.Operators)
	}

	// custom viewer: builtin=false, 2 permissions, 2 operators.
	v, ok := byName["viewer"]
	if !ok {
		t.Fatal("viewer отсутствует в каталоге")
	}
	if v.Builtin {
		t.Error("viewer.Builtin = true, want false")
	}
	if len(v.Permissions) != 2 {
		t.Errorf("viewer.Permissions = %v, want 2", v.Permissions)
	}
	if len(v.Operators) != 2 {
		t.Errorf("viewer.Operators = %v, want 2 (alice+bob)", v.Operators)
	}
}

// Пустой каталог (только seed cluster-admin без permissions): resetRBAC
// re-сидит cluster-admin с `*`, поэтому «пустой каталог» в смысле «нет
// custom-ролей» — проверяем, что виден ровно cluster-admin без operators.
func TestIntegration_ServiceListRoles_SeedOnly(t *testing.T) {
	resetRBAC(t)
	s := newService(t)

	views, err := s.ListRoles(context.Background())
	if err != nil {
		t.Fatalf("ListRoles: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("len(views) = %d, want 1 (только seed cluster-admin)", len(views))
	}
	if views[0].Name != "cluster-admin" || !views[0].Builtin {
		t.Errorf("view = %+v, want builtin cluster-admin", views[0])
	}
	if len(views[0].Operators) != 0 {
		t.Errorf("Operators = %v, want empty", views[0].Operators)
	}
}

// indexViews — name → RoleView для point-assert-ов.
func indexViews(views []RoleView) map[string]RoleView {
	m := make(map[string]RoleView, len(views))
	for _, v := range views {
		m[v.Name] = v
	}
	return m
}
