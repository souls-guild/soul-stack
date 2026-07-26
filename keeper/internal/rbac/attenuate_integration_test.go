//go:build integration

// Guard matrix for the derived-role authz gate (ADR-078(c)/(h)/(i), NIM-180)
// against a real DB: the write-time attenuation check on role.create /
// role.update, the caller-holds-parent floor, and the self-lockout rule that a
// derived role is never a source of cluster-admin.
//
// Against the DB rather than a hand-built snapshot because that is where the
// invariants actually live: the caller's rights are read in the mutation's own
// transaction, the self-lockout probes take row locks, and the parent chain is
// resolved from live rows by the same code the enforcer uses.
//
// Shares the container / TestMain / resetRBAC / seedOperator / insertRole /
// insertRoleScoped / newService with integration_test.go, crud_integration_test.go
// and subset_integration_test.go.
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

// insertDerived inserts a role with permissions, a delta scope and a parent, by
// raw SQL — a fixture for cases whose SUBJECT is a later mutation, not the
// creation. scope may be empty for "no delta".
func insertDerived(t *testing.T, name, parent, scope string, perms ...string) {
	t.Helper()
	ctx := context.Background()
	if _, err := integrationPool.Exec(ctx,
		`INSERT INTO rbac_roles (name, builtin, default_scope, parent_role) VALUES ($1, false, NULLIF($2, ''), $3)`,
		name, scope, parent); err != nil {
		t.Fatalf("insert derived role %q: %v", name, err)
	}
	for _, p := range perms {
		if _, err := integrationPool.Exec(ctx,
			`INSERT INTO rbac_role_permissions (role_name, permission) VALUES ($1, $2)`, name, p); err != nil {
			t.Fatalf("insert perm %q for %q: %v", p, name, err)
		}
	}
}

// parentOf reads a role's stored parent_role (nil = plain).
func parentOf(t *testing.T, name string) *string {
	t.Helper()
	var parent *string
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT parent_role FROM rbac_roles WHERE name = $1`, name).Scan(&parent); err != nil {
		t.Fatalf("read parent_role of %q: %v", name, err)
	}
	return parent
}

// ptr is a one-liner for the *string inputs of CreateRoleInput.
func ptr(s string) *string { return &s }

// ---- create: child ⊆ parent ----

// TestIntegration_Derived_CreateWithinParent_OK — the canonical shape from
// ADR-078: keep a subset of the parent's permissions, add a narrowing delta.
func TestIntegration_Derived_CreateWithinParent_OK(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	seedClusterAdmin(t, "archon-alice")
	insertRoleScoped(t, "dba", "coven=dba", "incarnation.get", "incarnation.run")

	if err := newService(t).CreateRole(context.Background(), CreateRoleInput{
		Name:         "dba-aboba",
		Permissions:  []string{"incarnation.get"},
		DefaultScope: ptr("trait.project=aboba"),
		ParentRole:   ptr("dba"),
		CallerAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("CreateRole (subset of the parent + a narrowing delta): %v", err)
	}

	got := parentOf(t, "dba-aboba")
	if got == nil || *got != "dba" {
		t.Fatalf("stored parent_role = %v, want dba", got)
	}
}

// TestIntegration_Derived_CreateAddingPermission_Denied — variant B: the child's
// permission SET must be a subset too, not just its scope.
func TestIntegration_Derived_CreateAddingPermission_Denied(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	seedClusterAdmin(t, "archon-alice")
	insertRoleScoped(t, "dba", "coven=dba", "incarnation.get")

	err := newService(t).CreateRole(context.Background(), CreateRoleInput{
		Name:        "dba-plus",
		Permissions: []string{"incarnation.get", "incarnation.destroy"},
		ParentRole:  ptr("dba"),
		CallerAID:   "archon-alice",
	})
	if !errors.Is(err, ErrRoleExceedsParent) {
		t.Fatalf("err = %v, want ErrRoleExceedsParent", err)
	}
	assertRoleAbsent(t, "dba-plus")
}

// TestIntegration_Derived_CreateWideningScope_Denied — a child that reaches
// outside its parent's area is refused. The parent's narrowing lives in a
// per-permission scope here, which is exactly the case the ceiling conjunction
// alone cannot catch: without the containment check the child would simply
// override it.
func TestIntegration_Derived_CreateWideningScope_Denied(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	seedClusterAdmin(t, "archon-alice")
	insertRole(t, "dba", "incarnation.get on coven=dba")

	for _, tc := range []struct {
		name  string
		perms []string
		scope *string
	}{
		{name: "wider value set", perms: []string{"incarnation.get on coven in (dba, prod)"}},
		{name: "another coven", perms: []string{"incarnation.get on coven=prod"}},
		{name: "bare, i.e. unrestricted", perms: []string{"incarnation.get"}},
		{name: "delta on a different dimension does not rescue it",
			perms: []string{"incarnation.get on coven=prod"}, scope: ptr("trait.project=aboba")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := newService(t).CreateRole(context.Background(), CreateRoleInput{
				Name:         "wide-child",
				Permissions:  tc.perms,
				DefaultScope: tc.scope,
				ParentRole:   ptr("dba"),
				CallerAID:    "archon-alice",
			})
			if !errors.Is(err, ErrRoleExceedsParent) {
				t.Fatalf("err = %v, want ErrRoleExceedsParent", err)
			}
			assertRoleAbsent(t, "wide-child")
		})
	}
}

// TestIntegration_Derived_CallerLacksParent_Denied — ADR-078(h). The caller may
// hold everything the CHILD asks for and still be refused: what they must hold is
// the PARENT, because the cascade will carry every later widening of that parent
// straight into the child.
func TestIntegration_Derived_CallerLacksParent_Denied(t *testing.T) {
	resetRBAC(t)
	sub, _ := setupSuboperator(t) // archon-sub: role.create + role.grant-operator only
	insertRole(t, "dba", "incarnation.get", "incarnation.destroy")

	// The child asks for nothing at all — no permission the caller could be said
	// to be granting itself. It is still refused, on the parent alone.
	err := newService(t).CreateRole(context.Background(), CreateRoleInput{
		Name:       "dba-child",
		ParentRole: ptr("dba"),
		CallerAID:  sub,
	})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld (caller does not hold the parent)", err)
	}
	assertRoleAbsent(t, "dba-child")
}

// TestIntegration_Derived_CallerHoldsParent_OK — the same shape as above with the
// caller actually holding the parent's rights: the gate lets it through, so the
// rule is a boundary and not a blanket refusal.
func TestIntegration_Derived_CallerHoldsParent_OK(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	a := "archon-alice"
	seedOperator(t, "archon-sub", &a)
	seedClusterAdmin(t, "archon-alice")

	insertRole(t, "dba", "incarnation.get")
	// sub holds exactly the parent's right, plus the right to create roles.
	insertRole(t, "sub-rights", "incarnation.get", "role.create")
	if err := GrantOperator(context.Background(), integrationPool, "sub-rights", "archon-sub", &a); err != nil {
		t.Fatalf("grant sub→sub-rights: %v", err)
	}

	if err := newService(t).CreateRole(context.Background(), CreateRoleInput{
		Name:         "dba-child",
		Permissions:  []string{"incarnation.get"},
		DefaultScope: ptr("coven=dba"),
		ParentRole:   ptr("dba"),
		CallerAID:    "archon-sub",
	}); err != nil {
		t.Fatalf("CreateRole with the caller holding the parent: %v", err)
	}
}

// TestIntegration_Derived_UnknownParent_NotFound — deriving from a role that does
// not exist is a 404, not a silently plain role.
func TestIntegration_Derived_UnknownParent_NotFound(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	seedClusterAdmin(t, "archon-alice")

	err := newService(t).CreateRole(context.Background(), CreateRoleInput{
		Name:       "orphan",
		ParentRole: ptr("ghost"),
		CallerAID:  "archon-alice",
	})
	if !errors.Is(err, ErrRoleNotFound) {
		t.Fatalf("err = %v, want ErrRoleNotFound", err)
	}
	assertRoleAbsent(t, "orphan")
}

// TestIntegration_Derived_SelfParent_Refused — a role deriving from itself is
// caught before any chain is resolved.
func TestIntegration_Derived_SelfParent_Refused(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	seedClusterAdmin(t, "archon-alice")

	err := newService(t).CreateRole(context.Background(), CreateRoleInput{
		Name:       "loop",
		ParentRole: ptr("loop"),
		CallerAID:  "archon-alice",
	})
	if !errors.Is(err, ErrRoleParentCycle) {
		t.Fatalf("err = %v, want ErrRoleParentCycle", err)
	}
	assertRoleAbsent(t, "loop")
}

// TestIntegration_Derived_CycleThroughUpdate_Refused — re-parenting a role onto
// its own descendant closes a cycle. The service write path must surface the
// trigger's SQLSTATE as the model sentinel, and the transaction must roll back.
func TestIntegration_Derived_CycleThroughUpdate_Refused(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	seedClusterAdmin(t, "archon-alice")
	insertRole(t, "a", "incarnation.get")
	insertDerived(t, "b", "a", "", "incarnation.get")

	err := newService(t).UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name:          "a",
		Permissions:   []string{"incarnation.get"},
		SetParentRole: true,
		ParentRole:    ptr("b"),
		CallerAID:     "archon-alice",
	})
	if !errors.Is(err, ErrRoleParentCycle) {
		t.Fatalf("err = %v, want ErrRoleParentCycle", err)
	}
	if p := parentOf(t, "a"); p != nil {
		t.Errorf("a.parent_role = %q after a rejected write, want NULL", *p)
	}
}

// TestIntegration_Derived_DepthCap_Refused — a chain longer than
// [maxRoleChainDepth] is refused through the service, not only through raw SQL.
func TestIntegration_Derived_DepthCap_Refused(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	seedClusterAdmin(t, "archon-alice")

	insertRole(t, "r1", "incarnation.get")
	insertDerived(t, "r2", "r1", "", "incarnation.get")
	insertDerived(t, "r3", "r2", "", "incarnation.get")
	insertDerived(t, "r4", "r3", "", "incarnation.get") // 4 roles = the cap

	err := newService(t).CreateRole(context.Background(), CreateRoleInput{
		Name:        "r5",
		Permissions: []string{"incarnation.get"},
		ParentRole:  ptr("r4"),
		CallerAID:   "archon-alice",
	})
	if !errors.Is(err, ErrRoleChainTooDeep) {
		t.Fatalf("err = %v, want ErrRoleChainTooDeep", err)
	}
	assertRoleAbsent(t, "r5")
}

// ---- update: the gate is re-applied on every mutation ----

// TestIntegration_Derived_UpdateBeyondParent_Denied — an EXISTING derived role
// cannot be patched past its parent. Without this, the create gate would be a
// formality: create a legal child, then add the permission you actually wanted.
func TestIntegration_Derived_UpdateBeyondParent_Denied(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	seedClusterAdmin(t, "archon-alice")
	insertRoleScoped(t, "dba", "coven=dba", "incarnation.get")
	insertDerived(t, "dba-aboba", "dba", "trait.project=aboba", "incarnation.get")

	err := newService(t).UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name:        "dba-aboba",
		Permissions: []string{"incarnation.get", "incarnation.destroy"},
		CallerAID:   "archon-alice",
	})
	if !errors.Is(err, ErrRoleExceedsParent) {
		t.Fatalf("err = %v, want ErrRoleExceedsParent", err)
	}
	if got := rolePerms(t, "dba-aboba"); len(got) != 1 || got[0] != "incarnation.get" {
		t.Errorf("permissions after a rejected update = %v, want [incarnation.get]", got)
	}
}

// TestIntegration_Derived_UpdateWithinParent_OK — trimming a derived role, and
// re-scoping its delta inside the parent, both go through.
func TestIntegration_Derived_UpdateWithinParent_OK(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	seedClusterAdmin(t, "archon-alice")
	insertRoleScoped(t, "dba", "coven=dba", "incarnation.get", "incarnation.run")
	insertDerived(t, "dba-aboba", "dba", "trait.project=aboba", "incarnation.get", "incarnation.run")

	if err := newService(t).UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name:            "dba-aboba",
		Permissions:     []string{"incarnation.get"},
		SetDefaultScope: true,
		DefaultScope:    ptr("trait.project=other"),
		CallerAID:       "archon-alice",
	}); err != nil {
		t.Fatalf("trim + re-scope within the parent: %v", err)
	}
}

// ---- self-lockout (ADR-078(i)) ----

// TestIntegration_Derived_BecomingDerivedCannotStripTheLastAdmin — direction 1 of
// the self-lockout obligation. Re-rooting the last `*`-granting role under a
// parent removes it from the admin count, so the mutation must be refused exactly
// as removing the `*` would be.
func TestIntegration_Derived_BecomingDerivedCannotStripTheLastAdmin(t *testing.T) {
	ctx := context.Background()
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)

	// `ceiling` grants `*` and belongs to nobody — a legal parent for a `*` child
	// (so the attenuation gate passes and the lockout guard is what decides), but
	// not an admin anyone holds.
	insertRole(t, "ceiling", "*")
	// `admins` is alice's only source of `*`: the builtin membership is removed so
	// the case is unambiguous (and cluster-admin is builtin — it cannot be updated).
	insertRole(t, "admins", "*")
	seedClusterAdmin(t, "archon-alice")
	if err := GrantOperator(ctx, integrationPool, "admins", "archon-alice", nil); err != nil {
		t.Fatalf("grant alice→admins: %v", err)
	}
	if _, err := integrationPool.Exec(ctx,
		`DELETE FROM rbac_role_operators WHERE role_name = 'cluster-admin'`); err != nil {
		t.Fatalf("drop builtin membership: %v", err)
	}

	err := newService(t).UpdateRolePermissions(ctx, UpdateRolePermissionsInput{
		Name:          "admins",
		Permissions:   []string{"*"}, // the permission set is UNCHANGED
		SetParentRole: true,
		ParentRole:    ptr("ceiling"),
		CallerAID:     "archon-alice",
	})
	if !errors.Is(err, ErrWouldLockOutCluster) {
		t.Fatalf("err = %v, want ErrWouldLockOutCluster — a derived `*` no longer counts", err)
	}
	if p := parentOf(t, "admins"); p != nil {
		t.Errorf("admins.parent_role = %q after a rejected write, want NULL", *p)
	}
}

// TestIntegration_Derived_DerivedAdminIsNotASurvivor — direction 2. An operator
// whose `*` arrives only through a DERIVED role must not be counted as the
// surviving cluster-admin, even though that chain currently resolves to an
// unrestricted `*` (the parent has no scope).
//
// Counting it is the dangerous direction: revoke the last real admin on the
// strength of the derived one, then re-scope its parent, and nobody is left with
// an unrestricted `*` at all. The probe therefore refuses the revoke — the same
// answer the operator would get if the derived role did not exist.
func TestIntegration_Derived_DerivedAdminIsNotASurvivor(t *testing.T) {
	ctx := context.Background()
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	a := "archon-alice"
	seedOperator(t, "archon-bob", &a)
	seedOperator(t, "archon-carol", &a)
	seedClusterAdmin(t, "archon-alice")

	insertRole(t, "ceiling", "*")
	insertDerived(t, "admin-copy", "ceiling", "", "*")
	if err := GrantOperator(ctx, integrationPool, "ceiling", "archon-carol", &a); err != nil {
		t.Fatalf("grant carol→ceiling: %v", err)
	}
	if err := GrantOperator(ctx, integrationPool, "admin-copy", "archon-bob", &a); err != nil {
		t.Fatalf("grant bob→admin-copy: %v", err)
	}

	// carol holds `*` through the PLAIN `ceiling`, so she is a survivor: alice can
	// be revoked while carol stands.
	svc := newService(t)
	if err := svc.RevokeOperator(ctx, RevokeOperatorInput{
		RoleName: "cluster-admin", AID: "archon-alice",
	}); err != nil {
		t.Fatalf("revoke alice while carol holds `*` through a plain role: %v", err)
	}

	// Revoking carol too must now be REFUSED. bob still resolves to an
	// unrestricted `*` through his derived role — and it does not count.
	if err := svc.RevokeOperator(ctx, RevokeOperatorInput{
		RoleName: "ceiling", AID: "archon-carol",
	}); !errors.Is(err, ErrWouldLockOutCluster) {
		t.Fatalf("err = %v, want ErrWouldLockOutCluster — a DERIVED `*` is not a survivor", err)
	}

	// The refusal is about the COUNT, not about bob's rights: his chain still
	// grants what it grants.
	snap, err := LoadSnapshot(ctx, integrationPool)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	e, err := NewEnforcerFromSnapshot(snap)
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	if err := e.Check("archon-bob", "incarnation", "destroy", nil); err != nil {
		t.Errorf("bob lost the rights his chain grants: %v", err)
	}
	if e.HasWildcard("archon-bob") {
		t.Error("HasWildcard counted a derived `*` — it must not (ADR-078(i))")
	}
}

// TestIntegration_Derived_DeletingADerivedAdminNeedsNoProbe — the mirror: a
// derived `*` role was never counted, so removing it cannot lock anyone out and
// must NOT produce a false 409. Fail-closed has to stop at the boundary.
func TestIntegration_Derived_DeletingADerivedAdminNeedsNoProbe(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	seedClusterAdmin(t, "archon-alice")
	insertRole(t, "ceiling", "*")
	insertDerived(t, "admin-copy", "ceiling", "", "*")

	if err := newService(t).DeleteRole(context.Background(), "admin-copy"); err != nil {
		t.Fatalf("deleting a DERIVED `*` role: %v (it was never an admin source)", err)
	}
}

// TestIntegration_Derived_OrphanPolicyIsFailClosed — ADR-078(g): a parent with
// children cannot be deleted, and the refusal leaves both rows intact. Clearing
// the link instead would turn the child's delta into an absolute scope — a
// widening — which is why the FK is RESTRICT and not SET NULL.
func TestIntegration_Derived_OrphanPolicyIsFailClosed(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	seedClusterAdmin(t, "archon-alice")
	insertRoleScoped(t, "dba", "coven=dba", "incarnation.get")
	insertDerived(t, "dba-aboba", "dba", "trait.project=aboba", "incarnation.get")

	if err := newService(t).DeleteRole(context.Background(), "dba"); !errors.Is(err, ErrRoleHasChildren) {
		t.Fatalf("err = %v, want ErrRoleHasChildren", err)
	}
	if p := parentOf(t, "dba-aboba"); p == nil || *p != "dba" {
		t.Errorf("child parent_role = %v after the refused delete, want dba (unchanged)", p)
	}
}

// ---- cascade, end to end ----

// TestIntegration_Derived_CascadeThroughTheSnapshot — the whole point of storing
// a reference instead of a copy: move the parent's scope and the child's
// EFFECTIVE rights follow at the next snapshot build, with no role rewritten.
func TestIntegration_Derived_CascadeThroughTheSnapshot(t *testing.T) {
	ctx := context.Background()
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	insertRoleScoped(t, "dba", "coven=dba", "incarnation.get")
	insertDerived(t, "dba-aboba", "dba", "trait.project=aboba", "incarnation.get")
	if err := GrantOperator(ctx, integrationPool, "dba-aboba", "archon-alice", nil); err != nil {
		t.Fatalf("grant alice→dba-aboba: %v", err)
	}

	purview := func() string {
		t.Helper()
		snap, err := LoadSnapshot(ctx, integrationPool)
		if err != nil {
			t.Fatalf("LoadSnapshot: %v", err)
		}
		e, err := NewEnforcerFromSnapshot(snap)
		if err != nil {
			t.Fatalf("NewEnforcerFromSnapshot: %v", err)
		}
		p := e.ResolvePurview("archon-alice", "incarnation", "get")
		if p.Unrestricted {
			t.Fatal("a derived role resolved to UNRESTRICTED — the ceiling was lost")
		}
		if len(p.Exprs) != 1 {
			t.Fatalf("purview = %d predicates, want 1", len(p.Exprs))
		}
		return p.Exprs[0].String()
	}

	if got, want := purview(), "coven=dba AND trait.project=aboba"; got != want {
		t.Fatalf("effective purview = %q, want %q", got, want)
	}

	// Move the parent. Nothing about the child is touched.
	if _, err := integrationPool.Exec(ctx,
		`UPDATE rbac_roles SET default_scope = 'coven=dbaas' WHERE name = 'dba'`); err != nil {
		t.Fatalf("re-scope the parent: %v", err)
	}
	if got, want := purview(), "coven=dbaas AND trait.project=aboba"; got != want {
		t.Fatalf("after the parent moved, effective purview = %q, want %q", got, want)
	}

	// Narrowing the parent's permission SET cascades the same way.
	if _, err := integrationPool.Exec(ctx,
		`DELETE FROM rbac_role_permissions WHERE role_name = 'dba'`); err != nil {
		t.Fatalf("strip the parent's permissions: %v", err)
	}
	snap, err := LoadSnapshot(ctx, integrationPool)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	e, err := NewEnforcerFromSnapshot(snap)
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	if err := e.Check("archon-alice", "incarnation", "get", map[string]string{"coven": "dbaas"}); err == nil {
		t.Error("the parent no longer holds incarnation.get — the child must not either")
	}
}

// assertRoleAbsent fails if the role exists — a refused mutation must leave no
// partial row behind (the whole gate runs inside the transaction).
func assertRoleAbsent(t *testing.T, name string) {
	t.Helper()
	var n int
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT count(*) FROM rbac_roles WHERE name = $1`, name).Scan(&n); err != nil {
		t.Fatalf("count role %q: %v", name, err)
	}
	if n != 0 {
		t.Errorf("role %q exists after a refused mutation — the tx did not roll back", name)
	}
}
