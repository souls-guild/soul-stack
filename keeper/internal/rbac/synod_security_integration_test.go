//go:build integration

// Security guard-матрица Synod (ADR-049(f), эпик Synod S2): least-privilege
// subset-check и self-lockout-инвариант ОБЯЗАНЫ считать эффективные права/роли
// архона = прямые ∪ через Synod. Без этого:
//   - escalation-via-group: оператор выдаёт право шире/уже своих (subset
//     недосчитал права, пришедшие через группу — ложный deny ИЛИ ложный пропуск);
//   - lockout-via-group: снятие последнего `*`-пути, держащегося ТОЛЬКО через
//     Synod, незамеченно залочивает кластер (self-lockout посчитал лишь прямые).
//
// Делит контейнер / resetRBAC / seedOperator / insertRole / insertRoleScoped /
// newService / membershipCount / roleExists / rolePerms с integration_test.go +
// crud_integration_test.go + subset_integration_test.go (тот же пакет rbac).
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

// seedSynod создаёт группу name и бандлит в неё роли roles (synods + synod_roles).
// Роли должны существовать (FK synod_roles → rbac_roles).
func seedSynod(t *testing.T, name string, roles ...string) {
	t.Helper()
	ctx := context.Background()
	if _, err := integrationPool.Exec(ctx,
		`INSERT INTO synods (name, builtin) VALUES ($1, false)`, name); err != nil {
		t.Fatalf("seedSynod(%s): %v", name, err)
	}
	for _, r := range roles {
		if _, err := integrationPool.Exec(ctx,
			`INSERT INTO synod_roles (synod_name, role_name) VALUES ($1, $2)`, name, r); err != nil {
			t.Fatalf("seedSynod bundle (%s -> %s): %v", name, r, err)
		}
	}
}

// addToSynod добавляет архона aid в группу name (synod_operators).
func addToSynod(t *testing.T, name, aid string) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(),
		`INSERT INTO synod_operators (synod_name, aid) VALUES ($1, $2)`, name, aid); err != nil {
		t.Fatalf("addToSynod (%s -> %s): %v", name, aid, err)
	}
}

// containsAID — true, если AID присутствует в наборе (point-assert admin-set-а).
// Локально в тесте: production-хелпер isInSet живёт в пакете operator.
func containsAID(set []string, target string) bool {
	for _, a := range set {
		if a == target {
			return true
		}
	}
	return false
}

// ============================================================
// SUBSET (least-privilege) через Synod — ADR-049(f)
// ============================================================

// ЭСКАЛАЦИЯ-ЧЕРЕЗ-ГРУППУ заблокирована: caller держит role.create+grant через
// прямую роль granters, но право operator.create у него НЕТ ни напрямую, ни
// через Synod → выдать operator.create нельзя (subset deny). Контроль: даже с
// введённым Synod subset не «придумывает» лишних прав caller-у.
func TestIntegration_SynodSubset_ForeignPermission_Denied(t *testing.T) {
	resetRBAC(t)
	sub, _ := setupSuboperator(t) // sub: role.create + role.grant-operator (прямо)
	// sub в группе, но группа даёт лишь soul.list — не operator.create.
	insertRole(t, "synod-viewer", "soul.list")
	seedSynod(t, "team-x", "synod-viewer")
	addToSynod(t, "team-x", sub)
	s := newService(t)

	err := s.CreateRole(context.Background(), CreateRoleInput{
		Name:        "esc-via-group",
		Permissions: []string{"operator.create"}, // нет ни прямо, ни через Synod
		CallerAID:   sub,
	})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld (operator.create вне эффективных прав caller-а)", err)
	}
	if roleExists(t, "esc-via-group") {
		t.Error("роль создана несмотря на subset-check")
	}
}

// ПОЗИТИВ (нет ложного deny): право caller-а приходит ТОЛЬКО через Synod-роль.
// caller должен мочь выдать его в его пределах. ЭТОТ кейс ловит регресс
// «subset считает лишь прямые роли» — без Synod-ветки в selectAIDPermissionsSQL
// у caller-а 0 прямых прав → ложный ErrPermissionNotHeld на СВОЁ право.
func TestIntegration_SynodSubset_OwnedViaGroup_OK(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	// alice — cluster-admin (источник `*` + второй admin против self-lockout).
	seedOperator(t, "archon-alice", nil)
	alice := "archon-alice"
	if err := GrantOperator(ctx, integrationPool, "cluster-admin", "archon-alice", nil); err != nil {
		t.Fatalf("grant alice→cluster-admin: %v", err)
	}
	// sub НЕ имеет прямых ролей вовсе — все права приходят через группу granters-grp.
	seedOperator(t, "archon-sub", &alice)
	sub := "archon-sub"
	insertRole(t, "grp-granters", "role.create", "role.grant-operator")
	seedSynod(t, "granters-grp", "grp-granters")
	addToSynod(t, "granters-grp", sub)
	s := newService(t)

	// sub выдаёт role.create (есть у него ТОЛЬКО через Synod) → должно пройти.
	if err := s.CreateRole(context.Background(), CreateRoleInput{
		Name:        "more-granters",
		Permissions: []string{"role.create"},
		CallerAID:   sub,
	}); err != nil {
		t.Fatalf("CreateRole (право через Synod, не должно быть ложного deny): %v", err)
	}
	if !roleExists(t, "more-granters") {
		t.Error("роль не создана (ложный deny — subset не учёл Synod-права caller-а)")
	}
}

// ПОЗИТИВ-граница: право через Synod НЕ расширяется. caller держит role.create
// через группу, но НЕ держит `*` ни прямо, ни через группу → выдать `*` нельзя.
func TestIntegration_SynodSubset_WildcardViaGroup_Absent_Denied(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	seedOperator(t, "archon-alice", nil)
	alice := "archon-alice"
	if err := GrantOperator(ctx, integrationPool, "cluster-admin", "archon-alice", nil); err != nil {
		t.Fatalf("grant alice→cluster-admin: %v", err)
	}
	seedOperator(t, "archon-sub", &alice)
	sub := "archon-sub"
	insertRole(t, "grp-granters", "role.create", "role.grant-operator")
	seedSynod(t, "granters-grp", "grp-granters")
	addToSynod(t, "granters-grp", sub)
	s := newService(t)

	err := s.CreateRole(context.Background(), CreateRoleInput{
		Name:        "esc-wildcard",
		Permissions: []string{"*"}, // нет ни прямо, ни через Synod
		CallerAID:   sub,
	})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld (`*` вне эффективных прав)", err)
	}
	if roleExists(t, "esc-wildcard") {
		t.Error("роль с `*` создана несмотря на subset-check")
	}
}

// SCOPE через Synod-роль наследуется: право caller-а приходит через Synod-роль
// с default_scope=coven=prod → его эффективный scope = prod. Выдать
// `incarnation.run on coven=staging` нельзя (вне scope), `... on coven=prod` —
// можно. Без Synod-ветки subset не увидел бы ни право, ни его scope.
func TestIntegration_SynodSubset_ScopedRoleViaGroup_Escalation_Denied(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	seedOperator(t, "archon-alice", nil)
	alice := "archon-alice"
	if err := GrantOperator(ctx, integrationPool, "cluster-admin", "archon-alice", nil); err != nil {
		t.Fatalf("grant alice→cluster-admin: %v", err)
	}
	seedOperator(t, "archon-sub", &alice)
	sub := "archon-sub"
	// Группа даёт incarnation.run+role.create под scope=coven=prod.
	insertRoleScoped(t, "grp-prod-runners", "coven=prod", "incarnation.run", "role.create")
	seedSynod(t, "prod-grp", "grp-prod-runners")
	addToSynod(t, "prod-grp", sub)
	s := newService(t)

	// staging вне scope=prod → отказ.
	err := s.CreateRole(context.Background(), CreateRoleInput{
		Name:        "synod-staging-esc",
		Permissions: []string{"incarnation.run on coven=staging"},
		CallerAID:   sub,
	})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld (scope=prod через Synod не покрывает staging)", err)
	}
	if roleExists(t, "synod-staging-esc") {
		t.Error("роль создана несмотря на subset-check (scope-эскалация через Synod)")
	}

	// prod в пределах scope → ок (наследование scope от Synod-роли работает).
	if err := s.CreateRole(context.Background(), CreateRoleInput{
		Name:        "synod-prod-ok",
		Permissions: []string{"incarnation.run on coven=prod"},
		CallerAID:   sub,
	}); err != nil {
		t.Fatalf("CreateRole (в scope=prod через Synod): %v", err)
	}
	if !roleExists(t, "synod-prod-ok") {
		t.Error("роль не создана (ложный deny — scope от Synod-роли не учтён)")
	}
}

// revoked caller с правом через Synod прав НЕ держит: Synod-ветка
// selectAIDPermissionsSQL фильтрует o.revoked_at IS NULL → пустой набор → отказ
// даже на «своё» групповое право.
func TestIntegration_SynodSubset_RevokedCallerViaGroup_NoPermissions(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	seedOperator(t, "archon-alice", nil)
	alice := "archon-alice"
	if err := GrantOperator(ctx, integrationPool, "cluster-admin", "archon-alice", nil); err != nil {
		t.Fatalf("grant alice→cluster-admin: %v", err)
	}
	seedOperator(t, "archon-sub", &alice)
	sub := "archon-sub"
	insertRole(t, "grp-granters", "role.create", "role.grant-operator")
	seedSynod(t, "granters-grp", "grp-granters")
	addToSynod(t, "granters-grp", sub)
	// Ревокнуть sub-а.
	if _, err := integrationPool.Exec(ctx,
		`UPDATE operators SET revoked_at = NOW() WHERE aid = $1`, sub); err != nil {
		t.Fatalf("revoke sub: %v", err)
	}
	s := newService(t)

	err := s.CreateRole(context.Background(), CreateRoleInput{
		Name:        "x",
		Permissions: []string{"role.create"}, // было бы у активного sub через Synod
		CallerAID:   sub,
	})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld (revoked caller прав через Synod не держит)", err)
	}
}

// ============================================================
// SELF-LOCKOUT через Synod — ADR-049(f)
// ============================================================

// LOCKOUT-VIA-GROUP: единственный admin держит `*` ТОЛЬКО через Synod-роль.
// LockEffectiveClusterAdmins обязан считать его «выжившим» — без Synod-ветки
// ядро вернуло бы пустой admin-set, и любая операция, считающая «останется ≥1
// admin», ошиблась бы. Прямой smoke: ядро видит группового admin-а.
func TestIntegration_SynodLockout_AdminViaGroup_Counted(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	seedOperator(t, "archon-grpadmin", nil)
	insertRole(t, "grp-admin-role", "*")
	seedSynod(t, "admins-grp", "grp-admin-role")
	addToSynod(t, "admins-grp", "archon-grpadmin")

	admins, err := LockEffectiveClusterAdmins(ctx, beginRoTx(t))
	if err != nil {
		t.Fatalf("LockEffectiveClusterAdmins: %v", err)
	}
	if !containsAID(admins, "archon-grpadmin") {
		t.Fatalf("admins = %v, want содержит archon-grpadmin (`*` через Synod)", admins)
	}
}

// REVOKED-В-ГРУППЕ-С-`*` НЕ «выживший»: единственный путь к `*` — группа, но её
// член revoked → admin-set пуст. Подтверждает фильтр o.revoked_at IS NULL в
// Synod-ветке self-lockout-ядра.
func TestIntegration_SynodLockout_RevokedAdminViaGroup_NotCounted(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	seedOperator(t, "archon-grpadmin", nil)
	insertRole(t, "grp-admin-role", "*")
	seedSynod(t, "admins-grp", "grp-admin-role")
	addToSynod(t, "admins-grp", "archon-grpadmin")
	if _, err := integrationPool.Exec(ctx,
		`UPDATE operators SET revoked_at = NOW() WHERE aid = $1`, "archon-grpadmin"); err != nil {
		t.Fatalf("revoke grpadmin: %v", err)
	}

	admins, err := LockEffectiveClusterAdmins(ctx, beginRoTx(t))
	if err != nil {
		t.Fatalf("LockEffectiveClusterAdmins: %v", err)
	}
	if containsAID(admins, "archon-grpadmin") {
		t.Fatalf("admins = %v, revoked grpadmin не должен считаться admin-ом", admins)
	}
	if len(admins) != 0 {
		t.Fatalf("admins = %v, want пусто (единственный admin revoked)", admins)
	}
}

// role.delete: удаление прямой `*`-роли НЕ лочит кластер, если другой admin
// держит `*` через Synod. Без Synod-ветки в lockWildcardAdminsExcludingRole
// группового admin-а не видно → ложный lockout.
func TestIntegration_SynodLockout_DeleteRole_SurvivorViaGroup_OK(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	// alice — admin через прямую extra-admin; bob — admin через Synod.
	seedOperator(t, "archon-alice", nil)
	insertRole(t, "extra-admin", "*")
	if err := GrantOperator(ctx, integrationPool, "extra-admin", "archon-alice", nil); err != nil {
		t.Fatalf("grant alice→extra-admin: %v", err)
	}
	alice := "archon-alice"
	seedOperator(t, "archon-bob", &alice)
	insertRole(t, "grp-admin-role", "*")
	seedSynod(t, "admins-grp", "grp-admin-role")
	addToSynod(t, "admins-grp", "archon-bob")
	s := newService(t)

	// Удалить прямую extra-admin — bob остаётся admin через Synod → ok.
	if err := s.DeleteRole(context.Background(), "extra-admin"); err != nil {
		t.Fatalf("DeleteRole (выживший admin через Synod): %v", err)
	}
	if roleExists(t, "extra-admin") {
		t.Error("роль не удалена")
	}
}

// role.delete: удаление роли, бандленной в Synod и дающей последний `*`, ЛОЧИТ
// кластер. excludeRole убирается из ОБЕИХ веток (прямой и Synod-bundle) — её
// вклад исчезает, выживших нет.
func TestIntegration_SynodLockout_DeleteRole_LastViaGroup_Locked(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-grpadmin", nil)
	insertRole(t, "grp-admin-role", "*")
	seedSynod(t, "admins-grp", "grp-admin-role")
	addToSynod(t, "admins-grp", "archon-grpadmin")
	s := newService(t)

	// grp-admin-role — единственный путь к `*` (через Synod). Удалить её → lockout.
	err := s.DeleteRole(context.Background(), "grp-admin-role")
	if !errors.Is(err, ErrWouldLockOutCluster) {
		t.Fatalf("err = %v, want ErrWouldLockOutCluster (удаление последней `*`-роли через Synod)", err)
	}
	if !roleExists(t, "grp-admin-role") {
		t.Error("роль удалена несмотря на lockout (tx не откатилась)")
	}
}

// role.update→remove-`*`: снятие `*` с прямой роли НЕ лочит, если групповой
// admin держит `*` через Synod (симметрия DeleteRole).
func TestIntegration_SynodLockout_UpdateRole_SurvivorViaGroup_OK(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	seedOperator(t, "archon-alice", nil)
	insertRole(t, "extra-admin", "*")
	if err := GrantOperator(ctx, integrationPool, "extra-admin", "archon-alice", nil); err != nil {
		t.Fatalf("grant alice→extra-admin: %v", err)
	}
	alice := "archon-alice"
	seedOperator(t, "archon-bob", &alice)
	insertRole(t, "grp-admin-role", "*")
	seedSynod(t, "admins-grp", "grp-admin-role")
	addToSynod(t, "admins-grp", "archon-bob")
	s := newService(t)

	if err := s.UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name: "extra-admin", Permissions: []string{"soul.list"}, CallerAID: "archon-alice",
	}); err != nil {
		t.Fatalf("UpdateRolePermissions (выживший admin через Synod): %v", err)
	}
}

// role.revoke-operator: снятие ПРЯМОЙ membership-строки НЕ разжалует архона,
// если тот же `*` держится через Synod. lockWildcardAdminsExcludingPair
// исключает пару только из прямой ветки — Synod-путь excludeAID жив.
func TestIntegration_SynodLockout_RevokeOperator_AdminAlsoViaGroup_OK(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	seedOperator(t, "archon-grpadmin", nil)
	// admin держит `*` И напрямую (direct-admin), И через Synod (grp-admin-role).
	insertRole(t, "direct-admin", "*")
	if err := GrantOperator(ctx, integrationPool, "direct-admin", "archon-grpadmin", nil); err != nil {
		t.Fatalf("grant direct: %v", err)
	}
	insertRole(t, "grp-admin-role", "*")
	seedSynod(t, "admins-grp", "grp-admin-role")
	addToSynod(t, "admins-grp", "archon-grpadmin")
	s := newService(t)

	// Снять прямую membership — admin остаётся через Synod → ok (не lockout).
	if err := s.RevokeOperator(context.Background(), RevokeOperatorInput{
		RoleName: "direct-admin", AID: "archon-grpadmin",
	}); err != nil {
		t.Fatalf("RevokeOperator (admin держит `*` ещё и через Synod): %v", err)
	}
	if membershipCount(t, "direct-admin") != 0 {
		t.Error("прямой membership не снят")
	}
	// Synod-путь жив — admin-set не пуст.
	admins, err := LockEffectiveClusterAdmins(ctx, beginRoTx(t))
	if err != nil {
		t.Fatalf("LockEffectiveClusterAdmins: %v", err)
	}
	if !containsAID(admins, "archon-grpadmin") {
		t.Fatalf("admins = %v, want содержит archon-grpadmin (`*` через Synod после снятия прямого)", admins)
	}
}

// role.revoke-operator: снятие ПОСЛЕДНЕЙ прямой membership-строки админа, чей
// `*` держится ТОЛЬКО прямо (нет Synod-пути), ЛОЧИТ кластер — Synod-ветка
// excludeAID пуста, выживших нет. Контроль: Synod-ветка не «выдумывает»
// несуществующего группового админа.
func TestIntegration_SynodLockout_RevokeOperator_LastDirectNoGroup_Locked(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	seedOperator(t, "archon-alice", nil)
	if err := GrantOperator(ctx, integrationPool, "cluster-admin", "archon-alice", nil); err != nil {
		t.Fatalf("grant alice→cluster-admin: %v", err)
	}
	// Группа существует, но НЕ даёт `*` и alice в ней не состоит.
	insertRole(t, "grp-viewer", "soul.list")
	seedSynod(t, "viewers-grp", "grp-viewer")
	s := newService(t)

	err := s.RevokeOperator(context.Background(), RevokeOperatorInput{
		RoleName: "cluster-admin", AID: "archon-alice",
	})
	if !errors.Is(err, ErrWouldLockOutCluster) {
		t.Fatalf("err = %v, want ErrWouldLockOutCluster (последний прямой admin, Synod не держит `*`)", err)
	}
	if membershipCount(t, "cluster-admin") != 1 {
		t.Error("membership снят несмотря на lockout (tx не откатилась)")
	}
}

// operator.Revoke (через rbac.LockEffectiveClusterAdmins): нельзя ревокнуть
// последнего архона, чей `*` держится ТОЛЬКО через Synod. Покрывает Synod-
// awareness ядра на operator-пути (другой пакет, общее ядро).
func TestIntegration_SynodLockout_OperatorRevoke_LastAdminViaGroup_Counted(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	seedOperator(t, "archon-grpadmin", nil)
	insertRole(t, "grp-admin-role", "*")
	seedSynod(t, "admins-grp", "grp-admin-role")
	addToSynod(t, "admins-grp", "archon-grpadmin")

	// Прямой инвариант ядра: единственный admin (через Synod) считается выжившим,
	// исключение его из admin-set оставляет пусто → operator.Revoke обязан
	// отказать. Проверяем именно множество, возвращаемое общим ядром.
	admins, err := LockEffectiveClusterAdmins(ctx, beginRoTx(t))
	if err != nil {
		t.Fatalf("LockEffectiveClusterAdmins: %v", err)
	}
	if len(admins) != 1 || admins[0] != "archon-grpadmin" {
		t.Fatalf("admins = %v, want [archon-grpadmin] (единственный admin через Synod)", admins)
	}
}
