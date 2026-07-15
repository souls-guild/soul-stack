//go:build integration

// Integration matrix for the least-privilege subset check (security fix:
// vertical privilege escalation via role.create/update/grant-operator).
// Shares the container / resetRBAC / seedOperator / insertRole / newService
// with integration_test.go + crud_integration_test.go (same rbac package).
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

// setupSuboperator sets up a caller without `*`: archon-sub holds exactly
// role.create + role.grant-operator (via the custom granters role), plus
// archon-alice as bootstrap-admin (`*` via cluster-admin) — so the cluster
// isn't locked out and there's a source of "strong" rights for grant
// scenarios.
func setupSuboperator(t *testing.T) (sub, alice string) {
	t.Helper()
	ctx := context.Background()
	seedOperator(t, "archon-alice", nil)
	a := "archon-alice"
	seedOperator(t, "archon-sub", &a)
	// alice is cluster-admin (source of `*` and a second admin against self-lockout).
	if err := GrantOperator(ctx, integrationPool, "cluster-admin", "archon-alice", nil); err != nil {
		t.Fatalf("grant alice→cluster-admin: %v", err)
	}
	// sub gets only role.create + role.grant-operator.
	insertRole(t, "granters", "role.create", "role.grant-operator")
	if err := GrantOperator(ctx, integrationPool, "granters", "archon-sub", &a); err != nil {
		t.Fatalf("grant sub→granters: %v", err)
	}
	return "archon-sub", a
}

// insertRoleScoped is insertRole + default_scope (ADR-047 S1). A direct
// INSERT of a role fixture with a per-role scope, bypassing Service (for
// caller fixtures whose own rights are under a default_scope).
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

// setupScopedCaller sets up a caller with a scoped role (default_scope=
// coven=prod + bare incarnation.run) → its effective scope is covens[prod].
// alice is cluster-admin (source of `*`, a second admin against
// self-lockout).
func setupScopedCaller(t *testing.T) (sub, alice string) {
	t.Helper()
	ctx := context.Background()
	seedOperator(t, "archon-alice", nil)
	a := "archon-alice"
	seedOperator(t, "archon-sub", &a)
	if err := GrantOperator(ctx, integrationPool, "cluster-admin", "archon-alice", nil); err != nil {
		t.Fatalf("grant alice→cluster-admin: %v", err)
	}
	// sub holds incarnation.run, but restricted to coven=prod via default_scope
	// + role.create/grant-operator (so it has any right to mutate at all).
	insertRoleScoped(t, "prod-runners", "coven=prod", "incarnation.run", "role.create", "role.grant-operator")
	if err := GrantOperator(ctx, integrationPool, "prod-runners", "archon-sub", &a); err != nil {
		t.Fatalf("grant sub→prod-runners: %v", err)
	}
	return "archon-sub", a
}

// ---- default_scope privilege escalation (security fix) ----

// ESCALATION: caller scope=prod + bare incarnation.run creates a role with
// `incarnation.run on coven=staging` → must be DENIED (its effective scope
// doesn't cover staging). Before the fix, subset compared the raw bare perm
// (covers everything) → the grant would go through.
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

// caller scope=prod creates a role with incarnation.run on coven=prod → OK
// (within its scope).
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

// caller scope=prod creates a role with default_scope=prod + bare → OK
// (effectively the same scope: bare inherits prod on both sides).
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

// caller scope=prod creates a role with default_scope=staging + bare →
// DENIED (the granted side is effectively coven=staging, outside the
// caller's scope).
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

// cluster-admin (`*`) can grant any scope → OK (exception #1 from ADR-047).
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

// backcompat: caller WITHOUT default_scope (unrestricted) + bare
// incarnation.run grants `incarnation.run on coven=staging` → OK (as before
// the fix: an unrestricted bare perm covers any scope, existing behavior
// isn't broken).
func TestIntegration_Subset_DefaultScope_UnrestrictedCaller_AnyScope_OK(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	a := "archon-alice"
	seedOperator(t, "archon-unrestricted", &a)
	if err := GrantOperator(context.Background(), integrationPool, "cluster-admin", "archon-alice", nil); err != nil {
		t.Fatalf("grant alice→cluster-admin: %v", err)
	}
	// A role WITHOUT default_scope (NULL) → bare perms unrestricted.
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

// ---- CreateRole subset check ----

// suboperator tries to create a role with `*` → denied (escalation to cluster-admin).
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

// suboperator tries to create a role with a permission outside its set → denied.
func TestIntegration_Subset_CreateRole_ForeignPermission_Denied(t *testing.T) {
	resetRBAC(t)
	sub, _ := setupSuboperator(t)
	s := newService(t)

	err := s.CreateRole(context.Background(), CreateRoleInput{
		Name:        "ops",
		Permissions: []string{"operator.create"}, // sub doesn't have this
		CallerAID:   sub,
	})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld", err)
	}
	if roleExists(t, "ops") {
		t.Error("роль создана несмотря на subset-check")
	}
}

// suboperator creates a role with a permission IN its own set → OK.
func TestIntegration_Subset_CreateRole_OwnedPermission_OK(t *testing.T) {
	resetRBAC(t)
	sub, _ := setupSuboperator(t)
	s := newService(t)

	if err := s.CreateRole(context.Background(), CreateRoleInput{
		Name:        "more-granters",
		Permissions: []string{"role.create"}, // sub has this
		CallerAID:   sub,
	}); err != nil {
		t.Fatalf("CreateRole (право в наборе): %v", err)
	}
	if !roleExists(t, "more-granters") {
		t.Error("роль не создана")
	}
}

// cluster-admin creates a role with any permission → OK (subset covers everything via `*`).
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

// ---- UpdateRolePermissions subset check ----

// suboperator adds a foreign permission to an existing role → denied.
func TestIntegration_Subset_UpdateRole_AddForeign_Denied(t *testing.T) {
	resetRBAC(t)
	sub, _ := setupSuboperator(t)
	insertRole(t, "target", "role.create")
	s := newService(t)

	err := s.UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name:        "target",
		Permissions: []string{"role.create", "*"}, // adds `*`
		CallerAID:   sub,
	})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld", err)
	}
	// `*` wasn't added — the tx rolled back.
	got := rolePerms(t, "target")
	if len(got) != 1 || got[0] != "role.create" {
		t.Errorf("permissions = %v, want [role.create] (откат)", got)
	}
}

// suboperator adds its own permission → OK.
func TestIntegration_Subset_UpdateRole_AddOwned_OK(t *testing.T) {
	resetRBAC(t)
	sub, _ := setupSuboperator(t)
	insertRole(t, "target", "role.create")
	s := newService(t)

	if err := s.UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name:        "target",
		Permissions: []string{"role.create", "role.grant-operator"}, // both held by sub
		CallerAID:   sub,
	}); err != nil {
		t.Fatalf("UpdateRolePermissions (свои права): %v", err)
	}
	if len(rolePerms(t, "target")) != 2 {
		t.Errorf("permissions = %v, want 2", rolePerms(t, "target"))
	}
}

// A suboperator removing a foreign permission (without adding a new one) →
// OK: the subset check only restricts ADDED permissions. target holds a
// permission outside sub's set; sub removes it, keeping only its own.
func TestIntegration_Subset_UpdateRole_RemoveForeign_OK(t *testing.T) {
	resetRBAC(t)
	sub, _ := setupSuboperator(t)
	insertRole(t, "target", "role.create", "operator.create")
	s := newService(t)

	// Removing operator.create (which sub doesn't have) is a removal, not escalation.
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

// ---- GrantOperator subset check ----

// suboperator grants a role containing `*` → denied (a workaround: it would
// bind a powerful role to itself/another and escalate).
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

// suboperator grants a role within its own rights → OK.
func TestIntegration_Subset_GrantOperator_WithinRights_OK(t *testing.T) {
	resetRBAC(t)
	sub, alice := setupSuboperator(t)
	seedOperator(t, "archon-bob", &alice)
	insertRole(t, "weak", "role.create") // sub has this permission
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

// cluster-admin grants a role with `*` → OK.
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

// A bootstrap grant (CallerAID=nil) bypasses the subset check — keeper init
// binds the first Archon to cluster-admin (`*`) with no caller subject.
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

// A revoked caller loses its rights for the subset check: its permissions
// aren't counted (the revoked_at IS NULL filter in callerPermissions). Here
// sub holds role.create via granters, but once the operator is revoked, its
// subset is empty → denied even on "its own" permission. Verifies the
// revoked_at filter.
func TestIntegration_Subset_RevokedCaller_HasNoPermissions(t *testing.T) {
	resetRBAC(t)
	sub, _ := setupSuboperator(t)
	// Revoke sub.
	if _, err := integrationPool.Exec(context.Background(),
		`UPDATE operators SET revoked_at = NOW() WHERE aid = $1`, sub); err != nil {
		t.Fatalf("revoke sub: %v", err)
	}
	s := newService(t)

	err := s.CreateRole(context.Background(), CreateRoleInput{
		Name:        "x",
		Permissions: []string{"role.create"}, // sub would have this if active
		CallerAID:   sub,
	})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld (revoked caller без прав)", err)
	}
}
