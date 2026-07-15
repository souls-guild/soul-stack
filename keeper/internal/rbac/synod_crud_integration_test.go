//go:build integration

// CRUD + security e2e for Synod-Service (ADR-049, Synod epic S3). Closes, at
// the Service+enforcer level, what synod_security_integration_test.go (S2)
// tested at the subset/self-lockout-core function level:
//   - CRUD happy-path: CreateSynod → AddOperator → GrantRole → archon gets
//     permissions through the group (enforcer.Check on a fresh snapshot);
//   - escalation e2e: an operator holding synod.grant-role/add-operator
//     WITHOUT `*` can neither grant the group a `*`-role nor add itself to a
//     `*`-group (subset 403);
//   - lockout e2e: delete/remove-operator/revoke-role of the last `*`-path
//     through a group → 409 (ErrWouldLockOutCluster), tx rolls back;
//   - revoked archon, cascades, 404, 409-duplicate/builtin.
//
// Shares container / resetRBAC / seedOperator / insertRole / insertRoleScoped /
// newService / membershipCount / roleExists + seedSynod / addToSynod /
// containsAID (synod_security_integration_test.go) — same rbac package.
//
// Run:
//
//	cd keeper && SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1 go test -tags=integration -race -count=1 ./internal/rbac/

package rbac

import (
	"context"
	"errors"
	"testing"
)

// synodExists reports whether group name exists in synods.
func synodExists(t *testing.T, name string) bool {
	t.Helper()
	var n int
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM synods WHERE name = $1`, name).Scan(&n); err != nil {
		t.Fatalf("synodExists(%s): %v", name, err)
	}
	return n > 0
}

// synodOperatorCount returns the number of group members (synod_operators).
func synodOperatorCount(t *testing.T, name string) int {
	t.Helper()
	var n int
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM synod_operators WHERE synod_name = $1`, name).Scan(&n); err != nil {
		t.Fatalf("synodOperatorCount(%s): %v", name, err)
	}
	return n
}

// synodRoleCount returns the number of roles in the group's bundle (synod_roles).
func synodRoleCount(t *testing.T, name string) int {
	t.Helper()
	var n int
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM synod_roles WHERE synod_name = $1`, name).Scan(&n); err != nil {
		t.Fatalf("synodRoleCount(%s): %v", name, err)
	}
	return n
}

// effectiveCheck builds a fresh enforcer snapshot from the DB and checks an
// archon's permission. Proves the end-to-end chain "Synod CRUD → snapshot
// union build → Check".
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
// CRUD HAPPY-PATH — end-to-end chain with enforcer.Check
// ============================================================

// CreateSynod → GrantRole → AddOperator: archon gets the group role's
// permission. Via a cluster-admin caller (holds `*`) — subset passes.
// enforcer.Check on a fresh snapshot confirms the union direct ∪ Synod grants
// the member the permission.
func TestIntegration_SynodCRUD_HappyPath_EffectivePermission(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	admin := "archon-admin"
	seedOperator(t, admin, nil)
	if err := GrantOperator(ctx, integrationPool, "cluster-admin", admin, nil); err != nil {
		t.Fatalf("grant admin→cluster-admin: %v", err)
	}
	// member — target archon with no direct roles.
	member := "archon-member"
	a := admin
	seedOperator(t, member, &a)
	insertRole(t, "deployer", "incarnation.run", "soul.list")
	s := newService(t)

	// 1. Create the group.
	if err := s.CreateSynod(ctx, CreateSynodInput{Name: "ops-team", Description: "ops", CallerAID: admin}); err != nil {
		t.Fatalf("CreateSynod: %v", err)
	}
	if !synodExists(t, "ops-team") {
		t.Fatal("группа не создана")
	}
	// 2. Bundle the role (cluster-admin holds its permissions via `*` → subset ok).
	if err := s.GrantRole(ctx, GrantRoleInput{SynodName: "ops-team", RoleName: "deployer", CallerAID: admin}); err != nil {
		t.Fatalf("GrantRole: %v", err)
	}
	// 3. Add the member.
	if err := s.AddOperator(ctx, AddOperatorInput{SynodName: "ops-team", AID: member, CallerAID: admin}); err != nil {
		t.Fatalf("AddOperator: %v", err)
	}

	// Until now the member had no incarnation.run permission — now it has it
	// THROUGH the group (snapshot union). Verify via enforcer.Check.
	if err := effectiveCheck(t, member, "incarnation", "run", nil); err != nil {
		t.Errorf("Check(member, incarnation.run) = %v, want nil (право через Synod)", err)
	}
	if err := effectiveCheck(t, member, "soul", "list", nil); err != nil {
		t.Errorf("Check(member, soul.list) = %v, want nil (право через Synod)", err)
	}
	// A permission the role does NOT grant — deny.
	if err := effectiveCheck(t, member, "operator", "create", nil); err == nil {
		t.Error("Check(member, operator.create) = nil, want deny (роль такого права не даёт)")
	}
}

// List returns the expanded bundle and members.
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

// Idempotency: repeated GrantRole/AddOperator calls are no-ops, not errors.
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
// ESCALATION e2e — subset on grant-role / add-operator (ADR-049(f))
// ============================================================

// GrantRole-escalation: sub holds synod.grant-role (+create/add) WITHOUT `*`
// and cannot bundle a `*`-role into a group → subset 403. Otherwise: build a
// group with `*`, add yourself — you've escalated to cluster-admin.
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
	// A powerful role with `*` exists (created by the admin).
	insertRole(t, "super", "*")
	s := newService(t)

	if err := s.CreateSynod(ctx, CreateSynodInput{Name: "trap", CallerAID: sub}); err != nil {
		t.Fatalf("CreateSynod (sub вправе): %v", err)
	}
	// sub bundles the `*`-role → subset must deny it (sub doesn't hold `*`).
	err := s.GrantRole(ctx, GrantRoleInput{SynodName: "trap", RoleName: "super", CallerAID: sub})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld (sub не держит `*`)", err)
	}
	if synodRoleCount(t, "trap") != 0 {
		t.Error("`*`-роль забандлена несмотря на subset-check")
	}
}

// GrantRole-escalation, scoped: sub holds incarnation.run on coven=prod (via a
// direct scoped role) and bundles a role with incarnation.run on
// coven=staging → outside its scope → 403. Bundling a role IN scope=prod → ok.
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
	// sub: synod-managers + incarnation.run restricted to scope=coven=prod.
	insertRole(t, "synod-managers", "synod.create", "synod.grant-role")
	insertRoleScoped(t, "prod-runner", "coven=prod", "incarnation.run")
	if err := GrantOperator(ctx, integrationPool, "synod-managers", sub, &alice); err != nil {
		t.Fatalf("grant sub→synod-managers: %v", err)
	}
	if err := GrantOperator(ctx, integrationPool, "prod-runner", sub, &alice); err != nil {
		t.Fatalf("grant sub→prod-runner: %v", err)
	}
	// Roles for the bundle: one outside scope (staging), one in scope (prod).
	insertRole(t, "staging-run", "incarnation.run on coven=staging")
	insertRole(t, "prod-run", "incarnation.run on coven=prod")
	s := newService(t)
	if err := s.CreateSynod(ctx, CreateSynodInput{Name: "g", CallerAID: sub}); err != nil {
		t.Fatalf("CreateSynod: %v", err)
	}

	// staging is outside scope=prod → denied.
	err := s.GrantRole(ctx, GrantRoleInput{SynodName: "g", RoleName: "staging-run", CallerAID: sub})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld (staging вне scope=prod)", err)
	}
	// prod is in scope → ok.
	if err := s.GrantRole(ctx, GrantRoleInput{SynodName: "g", RoleName: "prod-run", CallerAID: sub}); err != nil {
		t.Fatalf("GrantRole (prod в scope): %v", err)
	}
}

// AddOperator-escalation: sub holds synod.add-operator WITHOUT `*`, but
// cluster-admin has built a group with a `*`-role. sub CANNOT add
// itself/another into this group → subset 403 (the member would get `*`).
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
	// A group with a `*`-role, built directly by the admin.
	insertRole(t, "super", "*")
	seedSynod(t, "powerful", "super")
	s := newService(t)

	// sub adds itself to the `*`-group → subset must deny it.
	err := s.AddOperator(ctx, AddOperatorInput{SynodName: "powerful", AID: sub, CallerAID: sub})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld (член получил бы `*`)", err)
	}
	if synodOperatorCount(t, "powerful") != 0 {
		t.Error("архон добавлен в `*`-группу несмотря на subset-check")
	}
}

// AddOperator-OK: caller holds all of the group bundle's permissions
// directly → can add members.
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
	// sub holds add-operator + soul.list (exactly what the group bundles).
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

// DeleteSynod: the only path to `*` is through the group. Deleting the group → lockout.
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

// DeleteSynod-OK: a second admin exists directly → deleting the group doesn't lock out.
func TestIntegration_SynodLockout_Delete_SurvivorDirect_OK(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	seedOperator(t, "archon-alice", nil)
	if err := GrantOperator(ctx, integrationPool, "cluster-admin", "archon-alice", nil); err != nil {
		t.Fatalf("grant alice→cluster-admin: %v", err)
	}
	// bob — admin ONLY through the group.
	alice := "archon-alice"
	seedOperator(t, "archon-bob", &alice)
	insertRole(t, "grp-admin-role", "*")
	seedSynod(t, "admins-grp", "grp-admin-role")
	addToSynod(t, "admins-grp", "archon-bob")
	s := newService(t)

	// Delete the group — alice remains admin directly → ok.
	if err := s.DeleteSynod(ctx, "admins-grp"); err != nil {
		t.Fatalf("DeleteSynod (выживший admin напрямую): %v", err)
	}
	if synodExists(t, "admins-grp") {
		t.Error("группа не удалена")
	}
}

// RemoveOperator: the member holds `*` ONLY through this group and is the
// sole admin → removing it from the group locks out.
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

// RemoveOperator-OK: the member also holds `*` directly → removing it from the group doesn't lock out.
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
	// admin remains through the direct path.
	admins, err := LockEffectiveClusterAdmins(ctx, beginRoTx(t))
	if err != nil {
		t.Fatalf("LockEffectiveClusterAdmins: %v", err)
	}
	if !containsAID(admins, "archon-grpadmin") {
		t.Errorf("admins = %v, want содержит archon-grpadmin (через прямой `*`)", admins)
	}
}

// RevokeRole: the group's only `*`-role is revoked → its admin members become
// orphaned → lockout.
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

// RevokeRole-OK: the revoked role doesn't grant `*` → self-lockout doesn't apply.
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
// REVOKED / CASCADES / 404 / 409
// ============================================================

// Revoked member: a revoked archon does NOT get group permissions (snapshot
// filters out revoked). enforcer.Check denies even a permission bundled by
// its group.
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

	// The snapshot carries a revoked projection → Check denies a revoked AID
	// regardless of roles (direct or group).
	if err := effectiveCheck(t, member, "incarnation", "run", nil); err == nil {
		t.Error("Check(revoked member, incarnation.run) = nil, want deny")
	}
}

// Cascade: DeleteSynod tears down both the group's synod_operators and synod_roles.
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

// Cascade role.delete: deleting a role removes it from the bundle of every
// group (synod_roles FK ON DELETE CASCADE). DeleteRole's self-lockout guard
// is already Synod-aware (S2); here we verify the cascade itself on a
// non-`*` role.
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

// 404: mutations on a nonexistent group.
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

// 404: remove-operator/revoke-role on a nonexistent pair.
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

// 404: grant-role on a nonexistent role (FK → ErrRoleNotFound).
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

// 404: add-operator on a nonexistent AID (FK → ErrOperatorNotFound).
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

// 409: duplicate create.
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

// 409: a builtin group cannot be deleted.
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

// 422: invalid group name.
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
