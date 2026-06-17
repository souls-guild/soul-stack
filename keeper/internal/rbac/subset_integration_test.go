//go:build integration

// Integration-матрица least-privilege subset-check (security-fix: вертикальная
// эскалация привилегий через role.create/update/grant-operator). Делит
// контейнер / resetRBAC / seedOperator / insertRole / newService с
// integration_test.go + crud_integration_test.go (тот же пакет rbac).
//
// Запуск:
//
//	cd keeper && SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1 go test -tags=integration -race -count=1 ./internal/rbac/

package rbac

import (
	"context"
	"errors"
	"testing"
)

// setupSuboperator поднимает caller-а без `*`: archon-sub держит ровно
// role.create + role.grant-operator (через кастомную роль granters), плюс
// archon-alice как bootstrap-admin (`*` через cluster-admin) — чтобы кластер
// не залочился и был источник «сильных» прав для grant-сценариев.
func setupSuboperator(t *testing.T) (sub, alice string) {
	t.Helper()
	ctx := context.Background()
	seedOperator(t, "archon-alice", nil)
	a := "archon-alice"
	seedOperator(t, "archon-sub", &a)
	// alice — cluster-admin (источник `*` и второй admin против self-lockout).
	if err := GrantOperator(ctx, integrationPool, "cluster-admin", "archon-alice", nil); err != nil {
		t.Fatalf("grant alice→cluster-admin: %v", err)
	}
	// sub — только role.create + role.grant-operator.
	insertRole(t, "granters", "role.create", "role.grant-operator")
	if err := GrantOperator(ctx, integrationPool, "granters", "archon-sub", &a); err != nil {
		t.Fatalf("grant sub→granters: %v", err)
	}
	return "archon-sub", a
}

// insertRoleScoped — insertRole + default_scope (ADR-047 S1). Прямой INSERT
// фикстуры роли с per-role scope, минуя Service (для caller-фикстур, чьи права
// сами под default_scope).
func insertRoleScoped(t *testing.T, name, scope string, perms ...string) {
	t.Helper()
	ctx := context.Background()
	if _, err := integrationPool.Exec(ctx,
		`INSERT INTO rbac_roles (name, builtin, default_scope) VALUES ($1, false, $2)`, name, scope); err != nil {
		t.Fatalf("insert scoped role %q: %v", name, err)
	}
	for _, p := range perms {
		if _, err := integrationPool.Exec(ctx,
			`INSERT INTO rbac_role_permissions (role_name, permission) VALUES ($1, $2)`, name, p); err != nil {
			t.Fatalf("insert perm %q for %q: %v", p, name, err)
		}
	}
}

// setupScopedCaller поднимает caller-а scoped-ролью (default_scope=coven=prod +
// bare incarnation.run) → его эффективный scope = covens[prod]. alice —
// cluster-admin (источник `*`, второй admin против self-lockout).
func setupScopedCaller(t *testing.T) (sub, alice string) {
	t.Helper()
	ctx := context.Background()
	seedOperator(t, "archon-alice", nil)
	a := "archon-alice"
	seedOperator(t, "archon-sub", &a)
	if err := GrantOperator(ctx, integrationPool, "cluster-admin", "archon-alice", nil); err != nil {
		t.Fatalf("grant alice→cluster-admin: %v", err)
	}
	// sub держит incarnation.run, но ограниченный coven=prod через default_scope
	// + role.create/grant-operator (чтобы вообще иметь право на мутацию).
	insertRoleScoped(t, "prod-runners", "coven=prod", "incarnation.run", "role.create", "role.grant-operator")
	if err := GrantOperator(ctx, integrationPool, "prod-runners", "archon-sub", &a); err != nil {
		t.Fatalf("grant sub→prod-runners: %v", err)
	}
	return "archon-sub", a
}

// ---- default_scope privilege-escalation (security-fix) ----

// ЭСКАЛАЦИЯ: caller scope=prod + bare incarnation.run создаёт роль с
// `incarnation.run on coven=staging` → должен быть ОТКАЗ (его эффективный scope
// не покрывает staging). До фикса subset сравнивал сырую bare-perm (покрывает
// всё) → грант проходил.
func TestIntegration_Subset_DefaultScope_CreateRole_Escalation_Denied(t *testing.T) {
	resetRBAC(t)
	sub, _ := setupScopedCaller(t)
	s := newService(t)

	err := s.CreateRole(context.Background(), CreateRoleInput{
		Name:        "staging-escalation",
		Permissions: []string{"incarnation.run on coven=staging"},
		CallerAID:   sub,
	})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld (caller scope=prod не покрывает staging)", err)
	}
	if roleExists(t, "staging-escalation") {
		t.Error("роль создана несмотря на subset-check (эскалация на staging)")
	}
}

// caller scope=prod создаёт роль с incarnation.run on coven=prod → ОК
// (в пределах его scope).
func TestIntegration_Subset_DefaultScope_CreateRole_InScope_OK(t *testing.T) {
	resetRBAC(t)
	sub, _ := setupScopedCaller(t)
	s := newService(t)

	if err := s.CreateRole(context.Background(), CreateRoleInput{
		Name:        "prod-only",
		Permissions: []string{"incarnation.run on coven=prod"},
		CallerAID:   sub,
	}); err != nil {
		t.Fatalf("CreateRole (в scope=prod): %v", err)
	}
	if !roleExists(t, "prod-only") {
		t.Error("роль не создана")
	}
}

// caller scope=prod создаёт роль с default_scope=prod + bare → ОК
// (эффективно тот же scope: bare наследует prod обеих сторон).
func TestIntegration_Subset_DefaultScope_CreateRole_SameScope_OK(t *testing.T) {
	resetRBAC(t)
	sub, _ := setupScopedCaller(t)
	s := newService(t)

	scope := "coven=prod"
	if err := s.CreateRole(context.Background(), CreateRoleInput{
		Name:         "prod-runners-2",
		Permissions:  []string{"incarnation.run"},
		CallerAID:    sub,
		DefaultScope: &scope,
	}); err != nil {
		t.Fatalf("CreateRole (default_scope=prod + bare): %v", err)
	}
	if !roleExists(t, "prod-runners-2") {
		t.Error("роль не создана")
	}
}

// caller scope=prod создаёт роль с default_scope=staging + bare → ОТКАЗ
// (granted-сторона эффективно coven=staging, вне scope caller-а).
func TestIntegration_Subset_DefaultScope_CreateRole_OtherScope_Denied(t *testing.T) {
	resetRBAC(t)
	sub, _ := setupScopedCaller(t)
	s := newService(t)

	scope := "coven=staging"
	err := s.CreateRole(context.Background(), CreateRoleInput{
		Name:         "staging-runners",
		Permissions:  []string{"incarnation.run"},
		CallerAID:    sub,
		DefaultScope: &scope,
	})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld (default_scope=staging вне scope caller-а)", err)
	}
	if roleExists(t, "staging-runners") {
		t.Error("роль создана несмотря на subset-check")
	}
}

// cluster-admin (`*`) выдаёт любой scope → ОК (исключение №1 ADR-047).
func TestIntegration_Subset_DefaultScope_ClusterAdmin_AnyScope_OK(t *testing.T) {
	resetRBAC(t)
	_, alice := setupScopedCaller(t)
	s := newService(t)

	if err := s.CreateRole(context.Background(), CreateRoleInput{
		Name:        "any-scope",
		Permissions: []string{"incarnation.run on coven=staging"},
		CallerAID:   alice,
	}); err != nil {
		t.Fatalf("CreateRole (cluster-admin любой scope): %v", err)
	}
	if !roleExists(t, "any-scope") {
		t.Error("роль не создана")
	}
}

// backcompat: caller БЕЗ default_scope (unrestricted) + bare incarnation.run
// выдаёт `incarnation.run on coven=staging` → ОК (как было до фикса:
// unrestricted bare покрывает любой scope, существующее поведение не сломано).
func TestIntegration_Subset_DefaultScope_UnrestrictedCaller_AnyScope_OK(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	a := "archon-alice"
	seedOperator(t, "archon-unrestricted", &a)
	if err := GrantOperator(context.Background(), integrationPool, "cluster-admin", "archon-alice", nil); err != nil {
		t.Fatalf("grant alice→cluster-admin: %v", err)
	}
	// Роль БЕЗ default_scope (NULL) → bare-perms unrestricted.
	insertRole(t, "unrestricted-runners", "incarnation.run", "role.create")
	if err := GrantOperator(context.Background(), integrationPool, "unrestricted-runners", "archon-unrestricted", &a); err != nil {
		t.Fatalf("grant→unrestricted-runners: %v", err)
	}
	s := newService(t)

	unrestricted := "archon-unrestricted"
	if err := s.CreateRole(context.Background(), CreateRoleInput{
		Name:        "bc-staging",
		Permissions: []string{"incarnation.run on coven=staging"},
		CallerAID:   unrestricted,
	}); err != nil {
		t.Fatalf("CreateRole (unrestricted caller, backcompat): %v", err)
	}
	if !roleExists(t, "bc-staging") {
		t.Error("роль не создана (backcompat сломан)")
	}
}

// ---- CreateRole subset-check ----

// suboperator пытается создать роль с `*` → отказ (эскалация до cluster-admin).
func TestIntegration_Subset_CreateRole_Wildcard_Denied(t *testing.T) {
	resetRBAC(t)
	sub, _ := setupSuboperator(t)
	s := newService(t)

	err := s.CreateRole(context.Background(), CreateRoleInput{
		Name:        "escalation",
		Permissions: []string{"*"},
		CallerAID:   sub,
	})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld", err)
	}
	if roleExists(t, "escalation") {
		t.Error("роль создана несмотря на subset-check (tx не откатилась)")
	}
}

// suboperator пытается создать роль с правом вне своего набора → отказ.
func TestIntegration_Subset_CreateRole_ForeignPermission_Denied(t *testing.T) {
	resetRBAC(t)
	sub, _ := setupSuboperator(t)
	s := newService(t)

	err := s.CreateRole(context.Background(), CreateRoleInput{
		Name:        "ops",
		Permissions: []string{"operator.create"}, // нет у sub
		CallerAID:   sub,
	})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld", err)
	}
	if roleExists(t, "ops") {
		t.Error("роль создана несмотря на subset-check")
	}
}

// suboperator создаёт роль с правом В своём наборе → ок.
func TestIntegration_Subset_CreateRole_OwnedPermission_OK(t *testing.T) {
	resetRBAC(t)
	sub, _ := setupSuboperator(t)
	s := newService(t)

	if err := s.CreateRole(context.Background(), CreateRoleInput{
		Name:        "more-granters",
		Permissions: []string{"role.create"}, // есть у sub
		CallerAID:   sub,
	}); err != nil {
		t.Fatalf("CreateRole (право в наборе): %v", err)
	}
	if !roleExists(t, "more-granters") {
		t.Error("роль не создана")
	}
}

// cluster-admin создаёт роль с любым правом → ок (subset покрывает всё через `*`).
func TestIntegration_Subset_CreateRole_ClusterAdmin_OK(t *testing.T) {
	resetRBAC(t)
	_, alice := setupSuboperator(t)
	s := newService(t)

	if err := s.CreateRole(context.Background(), CreateRoleInput{
		Name:        "powerful",
		Permissions: []string{"*", "operator.create", "incarnation.run"},
		CallerAID:   alice,
	}); err != nil {
		t.Fatalf("CreateRole (cluster-admin): %v", err)
	}
	if !roleExists(t, "powerful") {
		t.Error("роль не создана")
	}
}

// ---- UpdateRolePermissions subset-check ----

// suboperator добавляет чужое право в существующую роль → отказ.
func TestIntegration_Subset_UpdateRole_AddForeign_Denied(t *testing.T) {
	resetRBAC(t)
	sub, _ := setupSuboperator(t)
	insertRole(t, "target", "role.create")
	s := newService(t)

	err := s.UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name:        "target",
		Permissions: []string{"role.create", "*"}, // добавляет `*`
		CallerAID:   sub,
	})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld", err)
	}
	// `*` не добавлен — tx откатилась.
	got := rolePerms(t, "target")
	if len(got) != 1 || got[0] != "role.create" {
		t.Errorf("permissions = %v, want [role.create] (откат)", got)
	}
}

// suboperator добавляет своё право → ок.
func TestIntegration_Subset_UpdateRole_AddOwned_OK(t *testing.T) {
	resetRBAC(t)
	sub, _ := setupSuboperator(t)
	insertRole(t, "target", "role.create")
	s := newService(t)

	if err := s.UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name:        "target",
		Permissions: []string{"role.create", "role.grant-operator"}, // оба у sub
		CallerAID:   sub,
	}); err != nil {
		t.Fatalf("UpdateRolePermissions (свои права): %v", err)
	}
	if len(rolePerms(t, "target")) != 2 {
		t.Errorf("permissions = %v, want 2", rolePerms(t, "target"))
	}
}

// Удаление чужого права (без добавления нового) suboperator-ом → ок:
// subset-check ограничивает только ДОБАВЛЯЕМЫЕ права. target держит право вне
// набора sub-а; sub снимает его, оставляя только свои.
func TestIntegration_Subset_UpdateRole_RemoveForeign_OK(t *testing.T) {
	resetRBAC(t)
	sub, _ := setupSuboperator(t)
	insertRole(t, "target", "role.create", "operator.create")
	s := newService(t)

	// Снять operator.create (которого у sub нет) — это удаление, не эскалация.
	if err := s.UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name:        "target",
		Permissions: []string{"role.create"},
		CallerAID:   sub,
	}); err != nil {
		t.Fatalf("UpdateRolePermissions (удаление чужого права): %v", err)
	}
	got := rolePerms(t, "target")
	if len(got) != 1 || got[0] != "role.create" {
		t.Errorf("permissions = %v, want [role.create]", got)
	}
}

// ---- GrantOperator subset-check ----

// suboperator грантит роль, содержащую `*`, → отказ (обход: привязал бы мощную
// роль себе/другому и поднялся).
func TestIntegration_Subset_GrantOperator_PowerfulRole_Denied(t *testing.T) {
	resetRBAC(t)
	sub, alice := setupSuboperator(t)
	seedOperator(t, "archon-victim", &alice)
	insertRole(t, "powerful", "*")
	s := newService(t)

	err := s.GrantOperator(context.Background(), GrantOperatorInput{
		RoleName:  "powerful",
		AID:       "archon-victim",
		CallerAID: &sub,
	})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld", err)
	}
	if membershipCount(t, "powerful") != 0 {
		t.Error("membership вставлен несмотря на subset-check")
	}
}

// suboperator грантит роль в пределах своих прав → ок.
func TestIntegration_Subset_GrantOperator_WithinRights_OK(t *testing.T) {
	resetRBAC(t)
	sub, alice := setupSuboperator(t)
	seedOperator(t, "archon-bob", &alice)
	insertRole(t, "weak", "role.create") // право есть у sub
	s := newService(t)

	if err := s.GrantOperator(context.Background(), GrantOperatorInput{
		RoleName:  "weak",
		AID:       "archon-bob",
		CallerAID: &sub,
	}); err != nil {
		t.Fatalf("GrantOperator (в пределах прав): %v", err)
	}
	if membershipCount(t, "weak") != 1 {
		t.Error("membership не вставлен")
	}
}

// cluster-admin грантит роль с `*` → ок.
func TestIntegration_Subset_GrantOperator_ClusterAdmin_OK(t *testing.T) {
	resetRBAC(t)
	_, alice := setupSuboperator(t)
	seedOperator(t, "archon-bob", &alice)
	insertRole(t, "powerful", "*")
	s := newService(t)

	if err := s.GrantOperator(context.Background(), GrantOperatorInput{
		RoleName:  "powerful",
		AID:       "archon-bob",
		CallerAID: &alice,
	}); err != nil {
		t.Fatalf("GrantOperator (cluster-admin грантит `*`-роль): %v", err)
	}
	if membershipCount(t, "powerful") != 1 {
		t.Error("membership не вставлен")
	}
}

// Bootstrap-грант (CallerAID=nil) обходит subset-check — keeper init привязывает
// первого Архонта к cluster-admin (`*`) без caller-субъекта.
func TestIntegration_Subset_GrantOperator_NilCaller_BypassesCheck(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	s := newService(t)

	if err := s.GrantOperator(context.Background(), GrantOperatorInput{
		RoleName:  "cluster-admin",
		AID:       "archon-alice",
		CallerAID: nil,
	}); err != nil {
		t.Fatalf("GrantOperator (bootstrap nil-caller): %v", err)
	}
	if membershipCount(t, "cluster-admin") != 1 {
		t.Error("bootstrap membership не вставлен")
	}
}

// revoked caller теряет права для subset-а: его permissions не учитываются
// (revoked_at IS NULL фильтр в callerPermissions). Тут sub держит role.create
// через granters, но если оператор revoked — subset-набор пуст → отказ даже на
// «своё» право. Проверяем фильтр revoked_at.
func TestIntegration_Subset_RevokedCaller_HasNoPermissions(t *testing.T) {
	resetRBAC(t)
	sub, _ := setupSuboperator(t)
	// Ревокнуть sub-а.
	if _, err := integrationPool.Exec(context.Background(),
		`UPDATE operators SET revoked_at = NOW() WHERE aid = $1`, sub); err != nil {
		t.Fatalf("revoke sub: %v", err)
	}
	s := newService(t)

	err := s.CreateRole(context.Background(), CreateRoleInput{
		Name:        "x",
		Permissions: []string{"role.create"}, // было бы у активного sub
		CallerAID:   sub,
	})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld (revoked caller без прав)", err)
	}
}
