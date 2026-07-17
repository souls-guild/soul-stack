//go:build integration

// Integration matrix for RBAC CRUD + the self-lockout core (ADR-028 Phase 2
// Slice 1) via testcontainers-go. Shares the container / TestMain /
// resetRBAC / seedOperator with integration_test.go (same rbac package).
//
// Self-lockout matrix — qa-blocker: each of the three mutations
// (role.delete / role.update / role.revoke-operator) over the last `*`
// path → lockout, over a non-last one → passes; plus concurrency (R2/R5)
// under FOR UPDATE.
//
// Run:
//
//	cd keeper && SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1 go test -tags=integration -race -count=1 ./internal/rbac/

package rbac

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// newService builds a Service against a real pool.
func newService(t *testing.T) *Service {
	t.Helper()
	s, err := NewService(ServiceDeps{Pool: integrationPool})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return s
}

// seedClusterAdmin gives the caller membership in the builtin cluster-admin
// role (`*`) in rbac_role_operators. Without this, the subset check
// (least-privilege, subset.go) sees 0 effective permissions for the caller
// and denies the mutation — model C (RBAC in Postgres) resolves the
// caller's rights from real membership, not from a config-RBAC enforcer.
// resetRBAC re-seeds the cluster-admin role itself with `*`; here we only
// attach the operator to it.
func seedClusterAdmin(t *testing.T, aid string) {
	t.Helper()
	if err := GrantOperator(context.Background(), integrationPool, "cluster-admin", aid, nil); err != nil {
		t.Fatalf("seedClusterAdmin(%s): %v", aid, err)
	}
}

// insertRole — a direct INSERT of a custom role + its permissions (bypassing
// the service, to set up fixtures independent of the self-lockout guard).
// builtin=false.
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

// rolePerms reads the role's permission strings directly (for assertions).
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

// roleExists / membershipExists — point checks for row existence.
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
		Description: "sees Souls",
		Permissions: []string{"soul.list", "incarnation.get"},
		CallerAID:   caller,
	})
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	if !roleExists(t, "soul-reader") {
		t.Fatal("role soul-reader was not created")
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

// TestIntegration_CreateRole_DefaultScope — ADR-047 S1: default_scope is
// persisted and inherited by bare permissions via ResolvePurview
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

	// The default_scope column is written.
	var got *string
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT default_scope FROM rbac_roles WHERE name = 'prod-ops'`).Scan(&got); err != nil {
		t.Fatalf("scan default_scope: %v", err)
	}
	if got == nil || *got != "coven=prod" {
		t.Fatalf("default_scope = %v, want coven=prod", got)
	}

	// Inheritance via the enforcer: bare-perm incarnation.run → covens=[prod].
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
		t.Errorf("Unrestricted=true, want false (bare-perm inherits default_scope)")
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
		t.Fatal("CreateRole with a broken permission: want error, got nil")
	}
	if roleExists(t, "broken") {
		t.Error("role broken was created despite the validation error (validation must run BEFORE tx)")
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
		t.Error("role tmp-role was not deleted")
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

// TestIntegration_DirectRolesOf — a new helper (HIGH-1 federated
// reconciliation): returns an AID's direct membership roles; revoke removes
// one; an unrelated AID — empty.
func TestIntegration_DirectRolesOf(t *testing.T) {
	resetRBAC(t)
	ctx := context.Background()
	seedOperator(t, "archon-bob", nil)
	insertRole(t, "operator", "soul.list")
	insertRole(t, "auditor", "audit.read")

	if err := GrantOperator(ctx, integrationPool, "operator", "archon-bob", nil); err != nil {
		t.Fatalf("grant operator: %v", err)
	}
	if err := GrantOperator(ctx, integrationPool, "auditor", "archon-bob", nil); err != nil {
		t.Fatalf("grant auditor: %v", err)
	}

	roles, err := DirectRolesOf(ctx, integrationPool, "archon-bob")
	if err != nil {
		t.Fatalf("DirectRolesOf: %v", err)
	}
	if len(roles) != 2 {
		t.Fatalf("DirectRolesOf = %v, want 2 roles", roles)
	}

	if err := RevokeOperator(ctx, integrationPool, "auditor", "archon-bob"); err != nil {
		t.Fatalf("revoke auditor: %v", err)
	}
	roles, err = DirectRolesOf(ctx, integrationPool, "archon-bob")
	if err != nil {
		t.Fatalf("DirectRolesOf after revoke: %v", err)
	}
	if len(roles) != 1 || roles[0] != "operator" {
		t.Fatalf("after revoke DirectRolesOf = %v, want [operator]", roles)
	}

	// An unrelated AID — empty (no error).
	other, err := DirectRolesOf(ctx, integrationPool, "archon-nobody")
	if err != nil {
		t.Fatalf("DirectRolesOf unknown: %v", err)
	}
	if len(other) != 0 {
		t.Errorf("unknown AID must have no direct roles, got %v", other)
	}
}

func TestIntegration_DeleteRole_Builtin(t *testing.T) {
	resetRBAC(t)
	s := newService(t)
	// cluster-admin (builtin=true) exists from resetRBAC's re-seed.
	err := s.DeleteRole(context.Background(), "cluster-admin")
	if !errors.Is(err, ErrRoleBuiltin) {
		t.Fatalf("err = %v, want ErrRoleBuiltin", err)
	}
	if !roleExists(t, "cluster-admin") {
		t.Error("builtin role was deleted")
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
		t.Error("permissions were not cascade-deleted")
	}
	if membershipCount(t, "casc-role") != 0 {
		t.Error("membership was not cascade-deleted")
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
		t.Fatalf("permissions = %v, want 3 (replace, old soul.list removed)", got)
	}
	for _, p := range got {
		if p == "soul.list" {
			t.Error("old permission soul.list was not removed (replace semantics violated)")
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

// TestIntegration_UpdateRolePermissions_EmptySetRemovesWildcard — an empty
// set removes all permissions (including `*`). Checked on a non-last `*`
// path (a second admin exists via cluster-admin), otherwise self-lockout
// would trigger.
func TestIntegration_UpdateRolePermissions_EmptySetRemovesWildcard(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	alice := "archon-alice"
	seedOperator(t, "archon-bob", &alice)
	// alice is admin via cluster-admin; the extra-admin role grants `*` to bob.
	if err := GrantOperator(context.Background(), integrationPool, "cluster-admin", "archon-alice", nil); err != nil {
		t.Fatalf("grant alice: %v", err)
	}
	insertRole(t, "extra-admin", "*")
	if err := GrantOperator(context.Background(), integrationPool, "extra-admin", "archon-bob", &alice); err != nil {
		t.Fatalf("grant bob: %v", err)
	}
	s := newService(t)

	// Remove `*` from extra-admin with an empty set — alice remains admin → ok.
	if err := s.UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name: "extra-admin", Permissions: nil,
	}); err != nil {
		t.Fatalf("UpdateRolePermissions (empty set): %v", err)
	}
	if permCount(t, "extra-admin") != 0 {
		t.Error("empty set did not clear permissions")
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
		t.Error("membership was not revoked")
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
// SELF-LOCKOUT MATRIX (qa-blocker)
// ============================================================

// setupSingleAdmin — the only path to `*`: alice via cluster-admin.
func setupSingleAdmin(t *testing.T) {
	t.Helper()
	seedOperator(t, "archon-alice", nil)
	if err := GrantOperator(context.Background(), integrationPool, "cluster-admin", "archon-alice", nil); err != nil {
		t.Fatalf("grant alice→cluster-admin: %v", err)
	}
}

// setupTwoAdminPaths — two independent paths to `*`:
//   - alice via cluster-admin (builtin);
//   - bob via the custom extra-admin (`*`).
//
// Removing any ONE path leaves the other → self-lockout must NOT trigger.
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

// DeleteRole on the last `*` role → lockout. We delete extra-admin when it's
// the only path to `*` (cluster-admin has no membership).
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
		t.Error("role was deleted despite lockout (tx did not roll back)")
	}
}

// DeleteRole on a non-last `*` role → passes (a second path exists).
func TestIntegration_SelfLockout_DeleteRole_NotLast(t *testing.T) {
	resetRBAC(t)
	setupTwoAdminPaths(t)
	s := newService(t)

	if err := s.DeleteRole(context.Background(), "extra-admin"); err != nil {
		t.Fatalf("DeleteRole (a second admin path exists): %v", err)
	}
	if roleExists(t, "extra-admin") {
		t.Error("role was not deleted")
	}
}

// --- role.update (removing `*`) ---

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
	// `*` remains — the tx rolled back.
	got := rolePerms(t, "extra-admin")
	if len(got) != 1 || got[0] != "*" {
		t.Errorf("permissions = %v, want [*] (rollback)", got)
	}
}

func TestIntegration_SelfLockout_UpdateRole_NotLast(t *testing.T) {
	resetRBAC(t)
	setupTwoAdminPaths(t)
	s := newService(t)

	if err := s.UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name: "extra-admin", Permissions: []string{"soul.list"}, CallerAID: "archon-alice",
	}); err != nil {
		t.Fatalf("UpdateRolePermissions (a second admin exists): %v", err)
	}
}

// UpdateRole that keeps `*` in the new set → self-lockout does NOT trigger,
// even if it's the only path (the new set also grants `*`).
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
		t.Fatalf("UpdateRolePermissions (new set also grants *): %v", err)
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
		t.Error("membership was revoked despite lockout (tx did not roll back)")
	}
}

func TestIntegration_SelfLockout_RevokeOperator_NotLast(t *testing.T) {
	resetRBAC(t)
	setupTwoAdminPaths(t)
	s := newService(t)

	// Revoke bob from extra-admin — alice remains admin → ok.
	if err := s.RevokeOperator(context.Background(), RevokeOperatorInput{
		RoleName: "extra-admin", AID: "archon-bob",
	}); err != nil {
		t.Fatalf("RevokeOperator (a second admin exists): %v", err)
	}
}

// An AID holds `*` via TWO roles: removing one membership row doesn't
// demote it (it remains admin via the other) → revoke passes.
func TestIntegration_SelfLockout_RevokeOperator_AdminViaTwoRoles(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	insertRole(t, "extra-admin", "*")
	// alice is admin both via cluster-admin and via extra-admin.
	if err := GrantOperator(context.Background(), integrationPool, "cluster-admin", "archon-alice", nil); err != nil {
		t.Fatalf("grant cluster-admin: %v", err)
	}
	if err := GrantOperator(context.Background(), integrationPool, "extra-admin", "archon-alice", nil); err != nil {
		t.Fatalf("grant extra-admin: %v", err)
	}
	s := newService(t)

	// Revoke alice from extra-admin — she remains admin via cluster-admin → ok.
	if err := s.RevokeOperator(context.Background(), RevokeOperatorInput{
		RoleName: "extra-admin", AID: "archon-alice",
	}); err != nil {
		t.Fatalf("RevokeOperator (admin via a second role): %v", err)
	}
	// Via cluster-admin alice is still admin.
	if membershipCount(t, "cluster-admin") != 1 {
		t.Error("cluster-admin membership for alice was lost")
	}
}

// ============================================================
// CONCURRENCY (R2/R5) — FOR UPDATE serializes the lockout race
// ============================================================

// Two parallel tx remove the last `*` via different paths:
//   - revoke alice from cluster-admin;
//   - delete extra-admin (through which that same alice is admin).
//
// alice is admin via EXACTLY these two paths. Removing both would lock out
// the cluster. Without FOR UPDATE, both tx would pass the "≥1 admin
// remains" probe (each still sees the other path alive) and commit →
// lockout. With serialization exactly one succeeds, the other → lockout.
// No deadlock (deterministic lock order).
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
			t.Fatalf("unexpected error: %v", e)
		}
	}
	if successes != 1 || lockouts != 1 {
		t.Fatalf("successes=%d lockouts=%d, want 1/1 (FOR UPDATE serialization)", successes, lockouts)
	}

	// Invariant: alice remains admin via at least one path.
	admins, err := LockEffectiveClusterAdmins(context.Background(), beginRoTx(t))
	if err != nil {
		t.Fatalf("LockEffectiveClusterAdmins: %v", err)
	}
	if len(admins) < 1 {
		t.Fatalf("active admins %d, want >= 1 (cluster must not lock itself out)", len(admins))
	}
}

// beginRoTx — a short tx for the final LockEffectiveClusterAdmins assertion
// (FOR UPDATE requires a tx). The tx is closed via t.Cleanup.
func beginRoTx(t *testing.T) ExecQueryRower {
	t.Helper()
	tx, err := integrationPool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin ro tx: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

// grantedByOf reads granted_by_aid of one membership row (role, aid).
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
		t.Error("membership was not inserted")
	}
	// granted_by_aid = CallerAID.
	if by := grantedByOf(t, "viewer", "archon-bob"); by == nil || *by != alice {
		t.Errorf("granted_by_aid = %v, want %q", by, alice)
	}
}

// CallerAID=nil → granted_by_aid IS NULL (bootstrap membership with no initiator).
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

// A repeated grant of the same pair is a no-op (ON CONFLICT DO NOTHING), no error.
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
		t.Error("membership was inserted for a nonexistent role")
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
		t.Error("membership was inserted for a nonexistent AID (FK should have rolled back)")
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

	// builtin cluster-admin: builtin=true, `*`, no operators.
	ca, ok := byName["cluster-admin"]
	if !ok {
		t.Fatal("cluster-admin is missing from the catalog")
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
		t.Fatal("viewer is missing from the catalog")
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

// An empty catalog (only the seed cluster-admin, no permissions): resetRBAC
// re-seeds cluster-admin with `*`, so "empty catalog" meaning "no custom
// roles" — we check that exactly cluster-admin is visible, with no operators.
func TestIntegration_ServiceListRoles_SeedOnly(t *testing.T) {
	resetRBAC(t)
	s := newService(t)

	views, err := s.ListRoles(context.Background())
	if err != nil {
		t.Fatalf("ListRoles: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("len(views) = %d, want 1 (only seed cluster-admin)", len(views))
	}
	if views[0].Name != "cluster-admin" || !views[0].Builtin {
		t.Errorf("view = %+v, want builtin cluster-admin", views[0])
	}
	if len(views[0].Operators) != 0 {
		t.Errorf("Operators = %v, want empty", views[0].Operators)
	}
}

// indexViews — name → RoleView, for point assertions.
func indexViews(views []RoleView) map[string]RoleView {
	m := make(map[string]RoleView, len(views))
	for _, v := range views {
		m[v.Name] = v
	}
	return m
}
