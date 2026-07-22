//go:build integration

// Security guard matrix for Synod (ADR-049(f), Synod epic S2): the
// least-privilege subset check and the self-lockout invariant MUST treat an
// archon's effective permissions/roles as direct ∪ via Synod. Without this:
//   - escalation-via-group: an operator grants a permission wider/narrower
//     than its own (subset undercounted permissions arriving through a group
//     — false deny OR false pass);
//   - lockout-via-group: revoking the last `*`-path held ONLY through Synod
//     silently locks out the cluster (self-lockout only counted direct
//     paths).
//
// Shares container / resetRBAC / seedOperator / insertRole / insertRoleScoped /
// newService / membershipCount / roleExists / rolePerms with
// integration_test.go + crud_integration_test.go + subset_integration_test.go
// (same rbac package).
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

// seedSynod creates group name and bundles roles into it (synods + synod_roles).
// The roles must already exist (FK synod_roles → rbac_roles).
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

// addToSynod adds archon aid to group name (synod_operators).
func addToSynod(t *testing.T, name, aid string) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(),
		`INSERT INTO synod_operators (synod_name, aid) VALUES ($1, $2)`, name, aid); err != nil {
		t.Fatalf("addToSynod (%s -> %s): %v", name, aid, err)
	}
}

// containsAID reports whether AID is present in the set (point-assert for an
// admin set). Local to the test: the production helper isInSet lives in the
// operator package.
func containsAID(set []string, target string) bool {
	for _, a := range set {
		if a == target {
			return true
		}
	}
	return false
}

// ============================================================
// SUBSET (least-privilege) via Synod — ADR-049(f)
// ============================================================

// ESCALATION-VIA-GROUP is blocked: caller holds role.create+grant through the
// direct granters role, but does NOT have operator.create either directly or
// via Synod → cannot grant operator.create (subset deny). Control: even with
// Synod wired in, subset doesn't "invent" extra permissions for the caller.
func TestIntegration_SynodSubset_ForeignPermission_Denied(t *testing.T) {
	resetRBAC(t)
	sub, _ := setupSuboperator(t) // sub: role.create + role.grant-operator (direct)
	// sub is in the group, but the group only grants soul.list — not operator.create.
	insertRole(t, "synod-viewer", "soul.list")
	seedSynod(t, "team-x", "synod-viewer")
	addToSynod(t, "team-x", sub)
	s := newService(t)

	err := s.CreateRole(context.Background(), CreateRoleInput{
		Name:        "esc-via-group",
		Permissions: []string{"operator.create"}, // neither directly nor via Synod
		CallerAID:   sub,
	})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld (operator.create outside caller effective permissions)", err)
	}
	if roleExists(t, "esc-via-group") {
		t.Error("role created despite the subset-check")
	}
}

// POSITIVE (no false deny): the caller's permission comes ONLY through a
// Synod role. The caller must be able to grant it within its bounds. THIS
// case catches the regression "subset only counts direct roles" — without
// the Synod branch in selectAIDPermissionsSQL, the caller has 0 direct
// permissions → false ErrPermissionNotHeld on its OWN permission.
func TestIntegration_SynodSubset_OwnedViaGroup_OK(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	// alice — cluster-admin (source of `*` + a second admin to avoid self-lockout).
	seedOperator(t, "archon-alice", nil)
	alice := "archon-alice"
	if err := GrantOperator(ctx, integrationPool, "cluster-admin", "archon-alice", nil); err != nil {
		t.Fatalf("grant alice→cluster-admin: %v", err)
	}
	// sub has NO direct roles at all — all permissions come through the granters-grp group.
	seedOperator(t, "archon-sub", &alice)
	sub := "archon-sub"
	insertRole(t, "grp-granters", "role.create", "role.grant-operator")
	seedSynod(t, "granters-grp", "grp-granters")
	addToSynod(t, "granters-grp", sub)
	s := newService(t)

	// sub grants role.create (which it has ONLY via Synod) → must succeed.
	if err := s.CreateRole(context.Background(), CreateRoleInput{
		Name:        "more-granters",
		Permissions: []string{"role.create"},
		CallerAID:   sub,
	}); err != nil {
		t.Fatalf("CreateRole (permission via Synod, should not be a false deny): %v", err)
	}
	if !roleExists(t, "more-granters") {
		t.Error("role not created (false deny - subset did not account for the caller Synod permissions)")
	}
}

// POSITIVE-boundary: a permission via Synod does NOT expand. caller holds
// role.create through the group, but does NOT hold `*` either directly or
// through the group → cannot grant `*`.
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
		Permissions: []string{"*"}, // neither directly nor via Synod
		CallerAID:   sub,
	})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld (`*` outside effective permissions)", err)
	}
	if roleExists(t, "esc-wildcard") {
		t.Error("role with `*` created despite the subset-check")
	}
}

// SCOPE via a Synod role is inherited: the caller's permission comes through
// a Synod role with default_scope=coven=prod → its effective scope = prod.
// Granting `incarnation.run on coven=staging` is denied (outside scope),
// `... on coven=prod` is allowed. Without the Synod branch, subset wouldn't
// see the permission or its scope at all.
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
	// The group grants incarnation.run+role.create under scope=coven=prod.
	insertRoleScoped(t, "grp-prod-runners", "coven=prod", "incarnation.run", "role.create")
	seedSynod(t, "prod-grp", "grp-prod-runners")
	addToSynod(t, "prod-grp", sub)
	s := newService(t)

	// staging is outside scope=prod → denied.
	err := s.CreateRole(context.Background(), CreateRoleInput{
		Name:        "synod-staging-esc",
		Permissions: []string{"incarnation.run on coven=staging"},
		CallerAID:   sub,
	})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld (scope=prod via Synod does not cover staging)", err)
	}
	if roleExists(t, "synod-staging-esc") {
		t.Error("role created despite the subset-check (scope escalation via Synod)")
	}

	// prod is within scope → ok (scope inheritance from the Synod role works).
	if err := s.CreateRole(context.Background(), CreateRoleInput{
		Name:        "synod-prod-ok",
		Permissions: []string{"incarnation.run on coven=prod"},
		CallerAID:   sub,
	}); err != nil {
		t.Fatalf("CreateRole (in scope=prod via Synod): %v", err)
	}
	if !roleExists(t, "synod-prod-ok") {
		t.Error("role not created (false deny - scope from the Synod role not accounted for)")
	}
}

// A revoked caller with a permission via Synod holds NO permissions: the
// Synod branch of selectAIDPermissionsSQL filters on o.revoked_at IS NULL →
// empty set → denied even for its "own" group permission.
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
	// Revoke sub.
	if _, err := integrationPool.Exec(ctx,
		`UPDATE operators SET revoked_at = NOW() WHERE aid = $1`, sub); err != nil {
		t.Fatalf("revoke sub: %v", err)
	}
	s := newService(t)

	err := s.CreateRole(context.Background(), CreateRoleInput{
		Name:        "x",
		Permissions: []string{"role.create"}, // would hold via Synod if sub were active
		CallerAID:   sub,
	})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld (revoked caller does not hold permissions via Synod)", err)
	}
}

// ============================================================
// SELF-LOCKOUT via Synod — ADR-049(f)
// ============================================================

// LOCKOUT-VIA-GROUP: the sole admin holds `*` ONLY through a Synod role.
// LockEffectiveClusterAdmins must count it as a "survivor" — without the
// Synod branch, the core would return an empty admin set, and any operation
// checking "at least 1 admin remains" would be wrong. Direct smoke test: the
// core sees the group-based admin.
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
		t.Fatalf("admins = %v, want to contain archon-grpadmin (`*` via Synod)", admins)
	}
}

// REVOKED-IN-A-`*`-GROUP is NOT a "survivor": the only path to `*` is the
// group, but its member is revoked → admin set is empty. Confirms the
// o.revoked_at IS NULL filter in the Synod branch of the self-lockout core.
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
		t.Fatalf("admins = %v, revoked grpadmin should not count as admin", admins)
	}
	if len(admins) != 0 {
		t.Fatalf("admins = %v, want empty (the only admin revoked)", admins)
	}
}

// role.delete: deleting a direct `*`-role does NOT lock the cluster if
// another admin holds `*` via Synod. Without the Synod branch in
// lockWildcardAdminsExcludingRole, the group-based admin is invisible →
// false lockout.
func TestIntegration_SynodLockout_DeleteRole_SurvivorViaGroup_OK(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	// alice — admin via a direct extra-admin role; bob — admin via Synod.
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

	// Delete the direct extra-admin role — bob remains admin via Synod → ok.
	if err := s.DeleteRole(context.Background(), "extra-admin"); err != nil {
		t.Fatalf("DeleteRole (surviving admin via Synod): %v", err)
	}
	if roleExists(t, "extra-admin") {
		t.Error("role not deleted")
	}
}

// role.delete: deleting a role bundled in Synod that grants the last `*`
// LOCKS the cluster. excludeRole is removed from BOTH branches (direct and
// Synod bundle) — its contribution disappears, no survivors remain.
func TestIntegration_SynodLockout_DeleteRole_LastViaGroup_Locked(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-grpadmin", nil)
	insertRole(t, "grp-admin-role", "*")
	seedSynod(t, "admins-grp", "grp-admin-role")
	addToSynod(t, "admins-grp", "archon-grpadmin")
	s := newService(t)

	// grp-admin-role is the only path to `*` (via Synod). Deleting it → lockout.
	err := s.DeleteRole(context.Background(), "grp-admin-role")
	if !errors.Is(err, ErrWouldLockOutCluster) {
		t.Fatalf("err = %v, want ErrWouldLockOutCluster (deleting the last `*` role via Synod)", err)
	}
	if !roleExists(t, "grp-admin-role") {
		t.Error("role deleted despite the lockout (tx not rolled back)")
	}
}

// role.update→remove-`*`: removing `*` from a direct role does NOT lock out
// if a group-based admin holds `*` via Synod (symmetric with DeleteRole).
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
		t.Fatalf("UpdateRolePermissions (surviving admin via Synod): %v", err)
	}
}

// role.revoke-operator: removing a DIRECT membership row does NOT demote the
// archon if the same `*` is held via Synod. lockWildcardAdminsExcludingPair
// excludes the pair only from the direct branch — the Synod path for
// excludeAID stays alive.
func TestIntegration_SynodLockout_RevokeOperator_AdminAlsoViaGroup_OK(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	seedOperator(t, "archon-grpadmin", nil)
	// admin holds `*` BOTH directly (direct-admin) AND via Synod (grp-admin-role).
	insertRole(t, "direct-admin", "*")
	if err := GrantOperator(ctx, integrationPool, "direct-admin", "archon-grpadmin", nil); err != nil {
		t.Fatalf("grant direct: %v", err)
	}
	insertRole(t, "grp-admin-role", "*")
	seedSynod(t, "admins-grp", "grp-admin-role")
	addToSynod(t, "admins-grp", "archon-grpadmin")
	s := newService(t)

	// Remove the direct membership — admin remains via Synod → ok (not a lockout).
	if err := s.RevokeOperator(context.Background(), RevokeOperatorInput{
		RoleName: "direct-admin", AID: "archon-grpadmin",
	}); err != nil {
		t.Fatalf("RevokeOperator (admin still holds `*` via Synod too): %v", err)
	}
	if membershipCount(t, "direct-admin") != 0 {
		t.Error("direct membership not removed")
	}
	// The Synod path is alive — the admin set is not empty.
	admins, err := LockEffectiveClusterAdmins(ctx, beginRoTx(t))
	if err != nil {
		t.Fatalf("LockEffectiveClusterAdmins: %v", err)
	}
	if !containsAID(admins, "archon-grpadmin") {
		t.Fatalf("admins = %v, want to contain archon-grpadmin (`*` via Synod after removing the direct one)", admins)
	}
}

// role.revoke-operator: removing the LAST direct membership row of an admin
// whose `*` is held ONLY directly (no Synod path) LOCKS the cluster — the
// Synod branch for excludeAID is empty, no survivors. Control: the Synod
// branch doesn't "invent" a nonexistent group-based admin.
func TestIntegration_SynodLockout_RevokeOperator_LastDirectNoGroup_Locked(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	seedOperator(t, "archon-alice", nil)
	if err := GrantOperator(ctx, integrationPool, "cluster-admin", "archon-alice", nil); err != nil {
		t.Fatalf("grant alice→cluster-admin: %v", err)
	}
	// The group exists but does NOT grant `*`, and alice is not a member of it.
	insertRole(t, "grp-viewer", "soul.list")
	seedSynod(t, "viewers-grp", "grp-viewer")
	s := newService(t)

	err := s.RevokeOperator(context.Background(), RevokeOperatorInput{
		RoleName: "cluster-admin", AID: "archon-alice",
	})
	if !errors.Is(err, ErrWouldLockOutCluster) {
		t.Fatalf("err = %v, want ErrWouldLockOutCluster (last direct admin, Synod does not hold `*`)", err)
	}
	if membershipCount(t, "cluster-admin") != 1 {
		t.Error("membership removed despite the lockout (tx not rolled back)")
	}
}

// operator.Revoke (via rbac.LockEffectiveClusterAdmins): cannot revoke the
// last archon whose `*` is held ONLY through Synod. Covers the core's
// Synod-awareness on the operator path (a different package, shared core).
func TestIntegration_SynodLockout_OperatorRevoke_LastAdminViaGroup_Counted(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	seedOperator(t, "archon-grpadmin", nil)
	insertRole(t, "grp-admin-role", "*")
	seedSynod(t, "admins-grp", "grp-admin-role")
	addToSynod(t, "admins-grp", "archon-grpadmin")

	// Direct core invariant: the sole admin (via Synod) counts as a survivor;
	// excluding it from the admin set leaves it empty → operator.Revoke must
	// refuse. We check exactly the set returned by the shared core.
	admins, err := LockEffectiveClusterAdmins(ctx, beginRoTx(t))
	if err != nil {
		t.Fatalf("LockEffectiveClusterAdmins: %v", err)
	}
	if len(admins) != 1 || admins[0] != "archon-grpadmin" {
		t.Fatalf("admins = %v, want [archon-grpadmin] (the only admin via Synod)", admins)
	}
}
