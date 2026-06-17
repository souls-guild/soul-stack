//go:build integration

// CRUD + security e2e Synod-Service (ADR-049, эпик Synod S3). Замыкает на
// уровне Service+enforcer то, что synod_security_integration_test.go (S2)
// тестировал на уровне функций subset/self-lockout-ядра:
//   - CRUD happy-path: CreateSynod → AddOperator → GrantRole → архон получил
//     permissions через группу (enforcer.Check на свежем снимке);
//   - escalation e2e: оператор с synod.grant-role/add-operator БЕЗ `*` НЕ может
//     ни выдать группе `*`-роль, ни ввести себя в `*`-группу (subset 403);
//   - lockout e2e: delete/remove-operator/revoke-role последнего `*`-пути через
//     группу → 409 (ErrWouldLockOutCluster), tx откатывается;
//   - revoked-архон, каскады, 404, 409-дубль/builtin.
//
// Делит контейнер / resetRBAC / seedOperator / insertRole / insertRoleScoped /
// newService / membershipCount / roleExists + seedSynod / addToSynod /
// containsAID (synod_security_integration_test.go) — тот же пакет rbac.
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

// synodExists — есть ли группа name в synods.
func synodExists(t *testing.T, name string) bool {
	t.Helper()
	var n int
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM synods WHERE name = $1`, name).Scan(&n); err != nil {
		t.Fatalf("synodExists(%s): %v", name, err)
	}
	return n > 0
}

// synodOperatorCount — число членов группы (synod_operators).
func synodOperatorCount(t *testing.T, name string) int {
	t.Helper()
	var n int
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM synod_operators WHERE synod_name = $1`, name).Scan(&n); err != nil {
		t.Fatalf("synodOperatorCount(%s): %v", name, err)
	}
	return n
}

// synodRoleCount — число ролей в bundle группы (synod_roles).
func synodRoleCount(t *testing.T, name string) int {
	t.Helper()
	var n int
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM synod_roles WHERE synod_name = $1`, name).Scan(&n); err != nil {
		t.Fatalf("synodRoleCount(%s): %v", name, err)
	}
	return n
}

// effectiveCheck строит свежий enforcer-снимок из БД и проверяет право архона.
// Доказывает сквозную цепочку «Synod-CRUD → snapshot-сборка union → Check».
func effectiveCheck(t *testing.T, aid, resource, action string, ctxMap map[string]string) error {
	t.Helper()
	snap, err := LoadSnapshot(context.Background(), integrationPool)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	enf, err := NewEnforcerFromSnapshot(snap)
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	return enf.Check(aid, resource, action, ctxMap)
}

// ============================================================
// CRUD HAPPY-PATH — сквозная цепочка с enforcer.Check
// ============================================================

// CreateSynod → GrantRole → AddOperator: архон получает permission роли группы.
// Через cluster-admin caller (имеет `*`) — subset проходит. enforcer.Check на
// свежем снимке подтверждает, что union прямые ∪ Synod даёт право члену.
func TestIntegration_SynodCRUD_HappyPath_EffectivePermission(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	admin := "archon-admin"
	seedOperator(t, admin, nil)
	if err := GrantOperator(ctx, integrationPool, "cluster-admin", admin, nil); err != nil {
		t.Fatalf("grant admin→cluster-admin: %v", err)
	}
	// member — целевой архон без прямых ролей.
	member := "archon-member"
	a := admin
	seedOperator(t, member, &a)
	insertRole(t, "deployer", "incarnation.run", "soul.list")
	s := newService(t)

	// 1. Создать группу.
	if err := s.CreateSynod(ctx, CreateSynodInput{Name: "ops-team", Description: "ops", CallerAID: admin}); err != nil {
		t.Fatalf("CreateSynod: %v", err)
	}
	if !synodExists(t, "ops-team") {
		t.Fatal("группа не создана")
	}
	// 2. Бандлить роль (cluster-admin держит её права через `*` → subset ok).
	if err := s.GrantRole(ctx, GrantRoleInput{SynodName: "ops-team", RoleName: "deployer", CallerAID: admin}); err != nil {
		t.Fatalf("GrantRole: %v", err)
	}
	// 3. Добавить члена.
	if err := s.AddOperator(ctx, AddOperatorInput{SynodName: "ops-team", AID: member, CallerAID: admin}); err != nil {
		t.Fatalf("AddOperator: %v", err)
	}

	// До этого момента член права incarnation.run не имел — теперь имеет ЧЕРЕЗ
	// группу (snapshot-union). Проверяем через enforcer.Check.
	if err := effectiveCheck(t, member, "incarnation", "run", nil); err != nil {
		t.Errorf("Check(member, incarnation.run) = %v, want nil (право через Synod)", err)
	}
	if err := effectiveCheck(t, member, "soul", "list", nil); err != nil {
		t.Errorf("Check(member, soul.list) = %v, want nil (право через Synod)", err)
	}
	// Право, которого роль НЕ даёт — deny.
	if err := effectiveCheck(t, member, "operator", "create", nil); err == nil {
		t.Error("Check(member, operator.create) = nil, want deny (роль такого права не даёт)")
	}
}

// List отдаёт развёрнутый bundle и членов.
func TestIntegration_SynodCRUD_List(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	admin := "archon-admin"
	seedOperator(t, admin, nil)
	if err := GrantOperator(ctx, integrationPool, "cluster-admin", admin, nil); err != nil {
		t.Fatalf("grant admin: %v", err)
	}
	member := "archon-m1"
	a := admin
	seedOperator(t, member, &a)
	insertRole(t, "viewer", "soul.list")
	s := newService(t)

	if err := s.CreateSynod(ctx, CreateSynodInput{Name: "team", CallerAID: admin}); err != nil {
		t.Fatalf("CreateSynod: %v", err)
	}
	if err := s.GrantRole(ctx, GrantRoleInput{SynodName: "team", RoleName: "viewer", CallerAID: admin}); err != nil {
		t.Fatalf("GrantRole: %v", err)
	}
	if err := s.AddOperator(ctx, AddOperatorInput{SynodName: "team", AID: member, CallerAID: admin}); err != nil {
		t.Fatalf("AddOperator: %v", err)
	}

	views, err := s.ListSynods(ctx)
	if err != nil {
		t.Fatalf("ListSynods: %v", err)
	}
	var team *SynodView
	for i := range views {
		if views[i].Name == "team" {
			team = &views[i]
		}
	}
	if team == nil {
		t.Fatal("группа team не в списке")
	}
	if len(team.Roles) != 1 || team.Roles[0] != "viewer" {
		t.Errorf("Roles = %v, want [viewer]", team.Roles)
	}
	if len(team.Operators) != 1 || team.Operators[0] != member {
		t.Errorf("Operators = %v, want [%s]", team.Operators, member)
	}
}

// Идемпотентность: повторные GrantRole/AddOperator — no-op, не ошибка.
func TestIntegration_SynodCRUD_Idempotent(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	admin := "archon-admin"
	seedOperator(t, admin, nil)
	if err := GrantOperator(ctx, integrationPool, "cluster-admin", admin, nil); err != nil {
		t.Fatalf("grant admin: %v", err)
	}
	member := "archon-m1"
	a := admin
	seedOperator(t, member, &a)
	insertRole(t, "viewer", "soul.list")
	s := newService(t)
	if err := s.CreateSynod(ctx, CreateSynodInput{Name: "team", CallerAID: admin}); err != nil {
		t.Fatalf("CreateSynod: %v", err)
	}

	for i := 0; i < 2; i++ {
		if err := s.GrantRole(ctx, GrantRoleInput{SynodName: "team", RoleName: "viewer", CallerAID: admin}); err != nil {
			t.Fatalf("GrantRole #%d: %v", i, err)
		}
		if err := s.AddOperator(ctx, AddOperatorInput{SynodName: "team", AID: member, CallerAID: admin}); err != nil {
			t.Fatalf("AddOperator #%d: %v", i, err)
		}
	}
	if synodRoleCount(t, "team") != 1 {
		t.Errorf("roleCount = %d, want 1 (идемпотентно)", synodRoleCount(t, "team"))
	}
	if synodOperatorCount(t, "team") != 1 {
		t.Errorf("operatorCount = %d, want 1 (идемпотентно)", synodOperatorCount(t, "team"))
	}
}

// ============================================================
// ESCALATION e2e — subset на grant-role / add-operator (ADR-049(f))
// ============================================================

// GrantRole-escalation: sub держит synod.grant-role (+create/add) БЕЗ `*`, не
// может бандлить `*`-роль в группу → subset 403. Иначе: собрал группу с `*`,
// привязал себя — поднялся до cluster-admin.
func TestIntegration_SynodEscalation_GrantWildcardRole_Denied(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	seedOperator(t, "archon-alice", nil)
	alice := "archon-alice"
	if err := GrantOperator(ctx, integrationPool, "cluster-admin", alice, nil); err != nil {
		t.Fatalf("grant alice: %v", err)
	}
	seedOperator(t, "archon-sub", &alice)
	sub := "archon-sub"
	insertRole(t, "synod-managers", "synod.create", "synod.grant-role", "synod.add-operator")
	if err := GrantOperator(ctx, integrationPool, "synod-managers", sub, &alice); err != nil {
		t.Fatalf("grant sub→synod-managers: %v", err)
	}
	// Существует мощная роль с `*` (создана админом).
	insertRole(t, "super", "*")
	s := newService(t)

	if err := s.CreateSynod(ctx, CreateSynodInput{Name: "trap", CallerAID: sub}); err != nil {
		t.Fatalf("CreateSynod (sub вправе): %v", err)
	}
	// sub бандлит `*`-роль → subset обязан запретить (sub `*` не держит).
	err := s.GrantRole(ctx, GrantRoleInput{SynodName: "trap", RoleName: "super", CallerAID: sub})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld (sub не держит `*`)", err)
	}
	if synodRoleCount(t, "trap") != 0 {
		t.Error("`*`-роль забандлена несмотря на subset-check")
	}
}

// GrantRole-escalation, scoped: sub держит incarnation.run on coven=prod (через
// прямую scoped-роль), бандлит роль с incarnation.run on coven=staging → вне
// его scope → 403. И bundle роли В scope=prod → ok.
func TestIntegration_SynodEscalation_GrantScopedRole(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	seedOperator(t, "archon-alice", nil)
	alice := "archon-alice"
	if err := GrantOperator(ctx, integrationPool, "cluster-admin", alice, nil); err != nil {
		t.Fatalf("grant alice: %v", err)
	}
	seedOperator(t, "archon-sub", &alice)
	sub := "archon-sub"
	// sub: synod-managers + incarnation.run ограниченный scope=coven=prod.
	insertRole(t, "synod-managers", "synod.create", "synod.grant-role")
	insertRoleScoped(t, "prod-runner", "coven=prod", "incarnation.run")
	if err := GrantOperator(ctx, integrationPool, "synod-managers", sub, &alice); err != nil {
		t.Fatalf("grant sub→synod-managers: %v", err)
	}
	if err := GrantOperator(ctx, integrationPool, "prod-runner", sub, &alice); err != nil {
		t.Fatalf("grant sub→prod-runner: %v", err)
	}
	// Роли для bundle: одна вне scope (staging), одна в scope (prod).
	insertRole(t, "staging-run", "incarnation.run on coven=staging")
	insertRole(t, "prod-run", "incarnation.run on coven=prod")
	s := newService(t)
	if err := s.CreateSynod(ctx, CreateSynodInput{Name: "g", CallerAID: sub}); err != nil {
		t.Fatalf("CreateSynod: %v", err)
	}

	// staging вне scope=prod → отказ.
	err := s.GrantRole(ctx, GrantRoleInput{SynodName: "g", RoleName: "staging-run", CallerAID: sub})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld (staging вне scope=prod)", err)
	}
	// prod в scope → ок.
	if err := s.GrantRole(ctx, GrantRoleInput{SynodName: "g", RoleName: "prod-run", CallerAID: sub}); err != nil {
		t.Fatalf("GrantRole (prod в scope): %v", err)
	}
}

// AddOperator-escalation: sub держит synod.add-operator БЕЗ `*`, но cluster-admin
// собрал группу с `*`-ролью. sub НЕ может ввести себя/другого в эту группу →
// subset 403 (член получил бы `*`).
func TestIntegration_SynodEscalation_AddToWildcardGroup_Denied(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	seedOperator(t, "archon-alice", nil)
	alice := "archon-alice"
	if err := GrantOperator(ctx, integrationPool, "cluster-admin", alice, nil); err != nil {
		t.Fatalf("grant alice: %v", err)
	}
	seedOperator(t, "archon-sub", &alice)
	sub := "archon-sub"
	insertRole(t, "adder", "synod.add-operator")
	if err := GrantOperator(ctx, integrationPool, "adder", sub, &alice); err != nil {
		t.Fatalf("grant sub→adder: %v", err)
	}
	// Группа с `*`-ролью, собранная админом напрямую.
	insertRole(t, "super", "*")
	seedSynod(t, "powerful", "super")
	s := newService(t)

	// sub вводит себя в `*`-группу → subset обязан запретить.
	err := s.AddOperator(ctx, AddOperatorInput{SynodName: "powerful", AID: sub, CallerAID: sub})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld (член получил бы `*`)", err)
	}
	if synodOperatorCount(t, "powerful") != 0 {
		t.Error("архон добавлен в `*`-группу несмотря на subset-check")
	}
}

// AddOperator-OK: caller держит все права bundle группы напрямую → может вводить.
func TestIntegration_SynodEscalation_AddOwnedBundle_OK(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	seedOperator(t, "archon-alice", nil)
	alice := "archon-alice"
	if err := GrantOperator(ctx, integrationPool, "cluster-admin", alice, nil); err != nil {
		t.Fatalf("grant alice: %v", err)
	}
	seedOperator(t, "archon-sub", &alice)
	sub := "archon-sub"
	// sub держит add-operator + soul.list (ровно то, что бандлит группа).
	insertRole(t, "adder", "synod.add-operator", "soul.list")
	if err := GrantOperator(ctx, integrationPool, "adder", sub, &alice); err != nil {
		t.Fatalf("grant sub→adder: %v", err)
	}
	seedOperator(t, "archon-target", &alice)
	insertRole(t, "viewer", "soul.list")
	seedSynod(t, "viewers", "viewer")
	s := newService(t)

	if err := s.AddOperator(ctx, AddOperatorInput{SynodName: "viewers", AID: "archon-target", CallerAID: sub}); err != nil {
		t.Fatalf("AddOperator (sub держит весь bundle): %v", err)
	}
	if synodOperatorCount(t, "viewers") != 1 {
		t.Error("архон не добавлен (ложный subset-deny на покрытое право)")
	}
}

// ============================================================
// SELF-LOCKOUT e2e — delete / remove-operator / revoke-role (ADR-049(f))
// ============================================================

// DeleteSynod: единственный путь к `*` — через группу. Удаление группы → lockout.
func TestIntegration_SynodLockout_Delete_LastAdminViaGroup_Locked(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-grpadmin", nil)
	insertRole(t, "grp-admin-role", "*")
	seedSynod(t, "admins-grp", "grp-admin-role")
	addToSynod(t, "admins-grp", "archon-grpadmin")
	s := newService(t)

	err := s.DeleteSynod(context.Background(), "admins-grp")
	if !errors.Is(err, ErrWouldLockOutCluster) {
		t.Fatalf("err = %v, want ErrWouldLockOutCluster (delete последнего `*`-пути через группу)", err)
	}
	if !synodExists(t, "admins-grp") {
		t.Error("группа удалена несмотря на lockout (tx не откатилась)")
	}
}

// DeleteSynod-OK: есть второй admin напрямую → удаление группы не лочит.
func TestIntegration_SynodLockout_Delete_SurvivorDirect_OK(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	seedOperator(t, "archon-alice", nil)
	if err := GrantOperator(ctx, integrationPool, "cluster-admin", "archon-alice", nil); err != nil {
		t.Fatalf("grant alice→cluster-admin: %v", err)
	}
	// bob — admin ТОЛЬКО через группу.
	alice := "archon-alice"
	seedOperator(t, "archon-bob", &alice)
	insertRole(t, "grp-admin-role", "*")
	seedSynod(t, "admins-grp", "grp-admin-role")
	addToSynod(t, "admins-grp", "archon-bob")
	s := newService(t)

	// Удалить группу — alice остаётся admin напрямую → ok.
	if err := s.DeleteSynod(ctx, "admins-grp"); err != nil {
		t.Fatalf("DeleteSynod (выживший admin напрямую): %v", err)
	}
	if synodExists(t, "admins-grp") {
		t.Error("группа не удалена")
	}
}

// RemoveOperator: член держит `*` ТОЛЬКО через эту группу и он единственный
// admin → снятие его из группы лочит.
func TestIntegration_SynodLockout_RemoveOperator_LastAdmin_Locked(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-grpadmin", nil)
	insertRole(t, "grp-admin-role", "*")
	seedSynod(t, "admins-grp", "grp-admin-role")
	addToSynod(t, "admins-grp", "archon-grpadmin")
	s := newService(t)

	err := s.RemoveOperator(context.Background(), RemoveOperatorInput{SynodName: "admins-grp", AID: "archon-grpadmin"})
	if !errors.Is(err, ErrWouldLockOutCluster) {
		t.Fatalf("err = %v, want ErrWouldLockOutCluster (снятие последнего `*`-члена)", err)
	}
	if synodOperatorCount(t, "admins-grp") != 1 {
		t.Error("член снят несмотря на lockout (tx не откатилась)")
	}
}

// RemoveOperator-OK: член держит `*` ещё и напрямую → снятие из группы не лочит.
func TestIntegration_SynodLockout_RemoveOperator_AlsoDirect_OK(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	seedOperator(t, "archon-grpadmin", nil)
	insertRole(t, "direct-admin", "*")
	if err := GrantOperator(ctx, integrationPool, "direct-admin", "archon-grpadmin", nil); err != nil {
		t.Fatalf("grant direct: %v", err)
	}
	insertRole(t, "grp-admin-role", "*")
	seedSynod(t, "admins-grp", "grp-admin-role")
	addToSynod(t, "admins-grp", "archon-grpadmin")
	s := newService(t)

	if err := s.RemoveOperator(ctx, RemoveOperatorInput{SynodName: "admins-grp", AID: "archon-grpadmin"}); err != nil {
		t.Fatalf("RemoveOperator (admin держит `*` ещё напрямую): %v", err)
	}
	if synodOperatorCount(t, "admins-grp") != 0 {
		t.Error("член не снят")
	}
	// admin остаётся через прямой путь.
	admins, err := LockEffectiveClusterAdmins(ctx, beginRoTx(t))
	if err != nil {
		t.Fatalf("LockEffectiveClusterAdmins: %v", err)
	}
	if !containsAID(admins, "archon-grpadmin") {
		t.Errorf("admins = %v, want содержит archon-grpadmin (через прямой `*`)", admins)
	}
}

// RevokeRole: единственная `*`-роль группы снимается → её члены-admin осиротеют →
// lockout.
func TestIntegration_SynodLockout_RevokeRole_LastWildcard_Locked(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-grpadmin", nil)
	insertRole(t, "grp-admin-role", "*")
	seedSynod(t, "admins-grp", "grp-admin-role")
	addToSynod(t, "admins-grp", "archon-grpadmin")
	s := newService(t)

	err := s.RevokeRole(context.Background(), RevokeRoleInput{SynodName: "admins-grp", RoleName: "grp-admin-role"})
	if !errors.Is(err, ErrWouldLockOutCluster) {
		t.Fatalf("err = %v, want ErrWouldLockOutCluster (снятие последней `*`-роли группы)", err)
	}
	if synodRoleCount(t, "admins-grp") != 1 {
		t.Error("роль снята несмотря на lockout (tx не откатилась)")
	}
}

// RevokeRole-OK: снимаемая роль не даёт `*` → self-lockout неприменим.
func TestIntegration_SynodLockout_RevokeRole_NonWildcard_OK(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	seedOperator(t, "archon-alice", nil)
	if err := GrantOperator(ctx, integrationPool, "cluster-admin", "archon-alice", nil); err != nil {
		t.Fatalf("grant alice→cluster-admin: %v", err)
	}
	insertRole(t, "viewer", "soul.list")
	seedSynod(t, "team", "viewer")
	s := newService(t)

	if err := s.RevokeRole(ctx, RevokeRoleInput{SynodName: "team", RoleName: "viewer"}); err != nil {
		t.Fatalf("RevokeRole (не-`*` роль): %v", err)
	}
	if synodRoleCount(t, "team") != 0 {
		t.Error("роль не снята")
	}
}

// ============================================================
// REVOKED / КАСКАДЫ / 404 / 409
// ============================================================

// Revoked-член: ревокнутый архон прав группы НЕ получает (snapshot фильтрует
// revoked). enforcer.Check деньит даже право, бандленное его группой.
func TestIntegration_SynodRevokedMember_NoEffectivePermission(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	admin := "archon-admin"
	seedOperator(t, admin, nil)
	if err := GrantOperator(ctx, integrationPool, "cluster-admin", admin, nil); err != nil {
		t.Fatalf("grant admin: %v", err)
	}
	member := "archon-member"
	a := admin
	seedOperator(t, member, &a)
	insertRole(t, "deployer", "incarnation.run")
	s := newService(t)
	if err := s.CreateSynod(ctx, CreateSynodInput{Name: "ops", CallerAID: admin}); err != nil {
		t.Fatalf("CreateSynod: %v", err)
	}
	if err := s.GrantRole(ctx, GrantRoleInput{SynodName: "ops", RoleName: "deployer", CallerAID: admin}); err != nil {
		t.Fatalf("GrantRole: %v", err)
	}
	if err := s.AddOperator(ctx, AddOperatorInput{SynodName: "ops", AID: member, CallerAID: admin}); err != nil {
		t.Fatalf("AddOperator: %v", err)
	}
	if _, err := integrationPool.Exec(ctx, `UPDATE operators SET revoked_at = NOW() WHERE aid = $1`, member); err != nil {
		t.Fatalf("revoke member: %v", err)
	}

	// Snapshot держит revoked-проекцию → Check деньит revoked AID независимо
	// от ролей (прямых/групповых).
	if err := effectiveCheck(t, member, "incarnation", "run", nil); err == nil {
		t.Error("Check(revoked member, incarnation.run) = nil, want deny")
	}
}

// Каскад: DeleteSynod сносит и synod_operators, и synod_roles группы.
func TestIntegration_SynodCascade_DeleteClearsMembershipAndBundle(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	admin := "archon-admin"
	seedOperator(t, admin, nil)
	if err := GrantOperator(ctx, integrationPool, "cluster-admin", admin, nil); err != nil {
		t.Fatalf("grant admin: %v", err)
	}
	member := "archon-m1"
	a := admin
	seedOperator(t, member, &a)
	insertRole(t, "viewer", "soul.list")
	s := newService(t)
	if err := s.CreateSynod(ctx, CreateSynodInput{Name: "team", CallerAID: admin}); err != nil {
		t.Fatalf("CreateSynod: %v", err)
	}
	if err := s.GrantRole(ctx, GrantRoleInput{SynodName: "team", RoleName: "viewer", CallerAID: admin}); err != nil {
		t.Fatalf("GrantRole: %v", err)
	}
	if err := s.AddOperator(ctx, AddOperatorInput{SynodName: "team", AID: member, CallerAID: admin}); err != nil {
		t.Fatalf("AddOperator: %v", err)
	}

	if err := s.DeleteSynod(ctx, "team"); err != nil {
		t.Fatalf("DeleteSynod: %v", err)
	}
	if synodOperatorCount(t, "team") != 0 {
		t.Error("synod_operators не очищены каскадом")
	}
	if synodRoleCount(t, "team") != 0 {
		t.Error("synod_roles не очищены каскадом")
	}
}

// Каскад role.delete: удаление роли снимает её из bundle всех групп (synod_roles
// FK ON DELETE CASCADE). DeleteRole с self-lockout-guard уже Synod-aware (S2);
// здесь проверяем сам каскад на не-`*` роли.
func TestIntegration_SynodCascade_RoleDeleteRemovesFromBundle(t *testing.T) {
	resetRBAC(t)
	insertRole(t, "viewer", "soul.list")
	seedSynod(t, "team", "viewer")
	s := newService(t)

	if err := s.DeleteRole(context.Background(), "viewer"); err != nil {
		t.Fatalf("DeleteRole: %v", err)
	}
	if synodRoleCount(t, "team") != 0 {
		t.Error("роль не снята из bundle каскадом role.delete")
	}
}

// 404: мутации над несуществующей группой.
func TestIntegration_Synod404_UnknownSynod(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	admin := "archon-admin"
	seedOperator(t, admin, nil)
	if err := GrantOperator(ctx, integrationPool, "cluster-admin", admin, nil); err != nil {
		t.Fatalf("grant admin: %v", err)
	}
	s := newService(t)

	if err := s.DeleteSynod(ctx, "ghost"); !errors.Is(err, ErrSynodNotFound) {
		t.Errorf("DeleteSynod(ghost) = %v, want ErrSynodNotFound", err)
	}
	if err := s.AddOperator(ctx, AddOperatorInput{SynodName: "ghost", AID: admin, CallerAID: admin}); !errors.Is(err, ErrSynodNotFound) {
		t.Errorf("AddOperator(ghost) = %v, want ErrSynodNotFound", err)
	}
	insertRole(t, "viewer", "soul.list")
	if err := s.GrantRole(ctx, GrantRoleInput{SynodName: "ghost", RoleName: "viewer", CallerAID: admin}); !errors.Is(err, ErrSynodNotFound) {
		t.Errorf("GrantRole(ghost) = %v, want ErrSynodNotFound", err)
	}
}

// 404: remove-operator/revoke-role над несуществующей парой.
func TestIntegration_Synod404_UnknownPair(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	insertRole(t, "viewer", "soul.list")
	seedSynod(t, "team", "viewer")
	s := newService(t)

	if err := s.RemoveOperator(ctx, RemoveOperatorInput{SynodName: "team", AID: "archon-nobody"}); !errors.Is(err, ErrSynodOperatorNotFound) {
		t.Errorf("RemoveOperator(unknown) = %v, want ErrSynodOperatorNotFound", err)
	}
	if err := s.RevokeRole(ctx, RevokeRoleInput{SynodName: "team", RoleName: "nonexistent"}); !errors.Is(err, ErrSynodRoleNotFound) {
		t.Errorf("RevokeRole(unknown) = %v, want ErrSynodRoleNotFound", err)
	}
}

// 404: grant-role над несуществующей ролью (FK → ErrRoleNotFound).
func TestIntegration_Synod404_GrantUnknownRole(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	admin := "archon-admin"
	seedOperator(t, admin, nil)
	if err := GrantOperator(ctx, integrationPool, "cluster-admin", admin, nil); err != nil {
		t.Fatalf("grant admin: %v", err)
	}
	seedSynod(t, "team")
	s := newService(t)

	if err := s.GrantRole(ctx, GrantRoleInput{SynodName: "team", RoleName: "ghost-role", CallerAID: admin}); !errors.Is(err, ErrRoleNotFound) {
		t.Errorf("GrantRole(ghost-role) = %v, want ErrRoleNotFound", err)
	}
}

// 404: add-operator над несуществующим AID (FK → ErrOperatorNotFound).
func TestIntegration_Synod404_AddUnknownOperator(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	admin := "archon-admin"
	seedOperator(t, admin, nil)
	if err := GrantOperator(ctx, integrationPool, "cluster-admin", admin, nil); err != nil {
		t.Fatalf("grant admin: %v", err)
	}
	seedSynod(t, "team")
	s := newService(t)

	if err := s.AddOperator(ctx, AddOperatorInput{SynodName: "team", AID: "archon-ghost", CallerAID: admin}); !errors.Is(err, ErrOperatorNotFound) {
		t.Errorf("AddOperator(ghost) = %v, want ErrOperatorNotFound", err)
	}
}

// 409: дубль create.
func TestIntegration_Synod409_DuplicateCreate(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	admin := "archon-admin"
	seedOperator(t, admin, nil)
	if err := GrantOperator(ctx, integrationPool, "cluster-admin", admin, nil); err != nil {
		t.Fatalf("grant admin: %v", err)
	}
	s := newService(t)
	if err := s.CreateSynod(ctx, CreateSynodInput{Name: "team", CallerAID: admin}); err != nil {
		t.Fatalf("CreateSynod #1: %v", err)
	}
	if err := s.CreateSynod(ctx, CreateSynodInput{Name: "team", CallerAID: admin}); !errors.Is(err, ErrSynodAlreadyExists) {
		t.Errorf("CreateSynod #2 = %v, want ErrSynodAlreadyExists", err)
	}
}

// 409: builtin-группу удалять нельзя.
func TestIntegration_Synod409_DeleteBuiltin(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	if _, err := integrationPool.Exec(ctx,
		`INSERT INTO synods (name, builtin) VALUES ('protected', true)`); err != nil {
		t.Fatalf("seed builtin synod: %v", err)
	}
	s := newService(t)

	if err := s.DeleteSynod(ctx, "protected"); !errors.Is(err, ErrSynodBuiltin) {
		t.Errorf("DeleteSynod(builtin) = %v, want ErrSynodBuiltin", err)
	}
	if !synodExists(t, "protected") {
		t.Error("builtin-группа удалена")
	}
}

// 422: невалидное имя группы.
func TestIntegration_Synod422_InvalidName(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	admin := "archon-admin"
	seedOperator(t, admin, nil)
	if err := GrantOperator(ctx, integrationPool, "cluster-admin", admin, nil); err != nil {
		t.Fatalf("grant admin: %v", err)
	}
	s := newService(t)

	if err := s.CreateSynod(ctx, CreateSynodInput{Name: "Bad_Name", CallerAID: admin}); !errors.Is(err, ErrInvalidSynodName) {
		t.Errorf("CreateSynod(Bad_Name) = %v, want ErrInvalidSynodName", err)
	}
}
