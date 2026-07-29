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
//
// scope_mode is written as `track` because migration 105 CHECKs it NULL exactly
// when parent_role is: a derived row without an intent is a state nothing should
// ever be in, fixtures included.
func insertDerived(t *testing.T, name, parent, scope string, perms ...string) {
	t.Helper()
	ctx := context.Background()
	if _, err := integrationPool.Exec(ctx,
		`INSERT INTO rbac_roles (name, builtin, default_scope, parent_role, scope_mode)
		 VALUES ($1, false, NULLIF($2, ''), $3, 'track')`,
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
		RoleName: "cluster-admin", AID: "archon-alice", CallerAID: "archon-alice",
	}); err != nil {
		t.Fatalf("revoke alice while carol holds `*` through a plain role: %v", err)
	}

	// Revoking carol too must now be REFUSED. bob still resolves to an
	// unrestricted `*` through his derived role — and it does not count.
	//
	// carol is the caller: alice just gave up cluster-admin above and now holds
	// nothing, so she could no longer administer a `*`-granting role (NIM-285).
	// carol holds `*` through `ceiling` itself, which keeps the lockout probe —
	// not the unbinding gate ahead of it — the thing that decides here.
	if err := svc.RevokeOperator(ctx, RevokeOperatorInput{
		RoleName: "ceiling", AID: "archon-carol", CallerAID: "archon-carol",
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

	if err := newService(t).DeleteRole(context.Background(), "admin-copy", "archon-alice"); err != nil {
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

	if err := newService(t).DeleteRole(context.Background(), "dba", "archon-alice"); !errors.Is(err, ErrRoleHasChildren) {
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

// ---- the caller floor reads the RESOLVED role, not its stored rows (NIM-198) ----

// setupScopedDelegator is the operator of the NIM-198 report: `incarnation.*`
// bounded to their own coven, plus an unscoped right to create and bind roles.
// The parent `dba` carries the same ceiling as a role default_scope, which is the
// shape the delegation scenario actually takes — the delegator runs a coven and
// hands out narrower slices of it.
func setupScopedDelegator(t *testing.T) (aid string) {
	t.Helper()
	ctx := context.Background()
	seedOperator(t, "archon-alice", nil)
	a := "archon-alice"
	seedOperator(t, "archon-dba", &a)
	seedClusterAdmin(t, "archon-alice")

	insertRoleScoped(t, "dba-rights", "coven=dba", "incarnation.*")
	insertRole(t, "dba-granters", "role.create", "role.grant-operator")
	for _, r := range []string{"dba-rights", "dba-granters"} {
		if err := GrantOperator(ctx, integrationPool, r, "archon-dba", &a); err != nil {
			t.Fatalf("grant archon-dba→%s: %v", r, err)
		}
	}
	insertRoleScoped(t, "dba", "coven=dba", "incarnation.*")
	return "archon-dba"
}

// TestIntegration_Derived_ScopedDelegatorNeedNotRepeatTheParentScope is the
// NIM-198 report, end to end: an operator scoped to `coven=dba` derives from a
// parent carrying that same ceiling, WITHOUT restating it.
//
// Before the fix the floor compared the child's rows as stored — a bare
// `incarnation.get`, which reads as unrestricted — against a caller who is not,
// and refused. The only way through was to write `coven=dba` into the delta,
// which is not a delta at all: it pins the child to the coven the parent happens
// to be in today (ADR-078(b)), turning the documented tracking contract into its
// opposite as the price of a 201.
//
// So the assertion is not merely "it succeeds": the stored default_scope must
// still be the delta the operator wrote, empty or otherwise.
func TestIntegration_Derived_ScopedDelegatorNeedNotRepeatTheParentScope(t *testing.T) {
	for _, tc := range []struct {
		name  string
		delta *string
		want  *string
	}{
		{name: "no delta at all", delta: nil, want: nil},
		{name: "a delta on another dimension", delta: ptr("trait.project=probe"), want: ptr("trait.project=probe")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetRBAC(t)
			dba := setupScopedDelegator(t)

			if err := newService(t).CreateRole(context.Background(), CreateRoleInput{
				Name:         "dba-probe",
				Permissions:  []string{"incarnation.get"},
				DefaultScope: tc.delta,
				ParentRole:   ptr("dba"),
				CallerAID:    dba,
			}); err != nil {
				t.Fatalf("CreateRole as a delegator scoped to the parent's own coven: %v", err)
			}

			got := scopeOf(t, "dba-probe")
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("stored default_scope = %q, want NULL — the role must track its parent, not pin it", *got)
			case tc.want != nil && (got == nil || *got != *tc.want):
				t.Fatalf("stored default_scope = %v, want %q", got, *tc.want)
			}
		})
	}
}

// TestIntegration_Derived_ScopedDelegatorMayBindWhatItCreated — creating the role
// is half the delegation; the floor gates binding it too, and read the same stored
// rows there. A delegator who cannot hand out the role they were just allowed to
// create has not been delegated anything.
func TestIntegration_Derived_ScopedDelegatorMayBindWhatItCreated(t *testing.T) {
	resetRBAC(t)
	dba := setupScopedDelegator(t)
	ctx := context.Background()
	s := newService(t)

	if err := s.CreateRole(ctx, CreateRoleInput{
		Name: "dba-probe", Permissions: []string{"incarnation.get"},
		ParentRole: ptr("dba"), CallerAID: dba,
	}); err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	seedOperator(t, "archon-probe", &dba)
	if err := s.GrantOperator(ctx, GrantOperatorInput{
		RoleName: "dba-probe", AID: "archon-probe", CallerAID: &dba,
	}); err != nil {
		t.Fatalf("GrantOperator on the derived role just created: %v", err)
	}
}

// TestIntegration_Derived_ResolvedFloorDoesNotReachPlainRoles — the relaxation is
// the parent's ceiling and nothing else. A role with no parent has no ceiling to
// resolve against, so a scoped caller writing a bare permission is still asking to
// grant it unrestricted, and is still refused (ADR-047 S1, NIM-130).
func TestIntegration_Derived_ResolvedFloorDoesNotReachPlainRoles(t *testing.T) {
	resetRBAC(t)
	dba := setupScopedDelegator(t)

	err := newService(t).CreateRole(context.Background(), CreateRoleInput{
		Name:        "unbounded",
		Permissions: []string{"incarnation.get"},
		CallerAID:   dba,
	})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld (a plain role has no ceiling to be judged under)", err)
	}
	assertRoleAbsent(t, "unbounded")
}

// TestIntegration_Derived_ResolvedFloorKeepsTheParentBoundary — the floor moved to
// the resolved form, not away. A parent OUTSIDE the caller's own area is refused
// whatever the child asks for: caller-holds-parent is the boundary that makes the
// relaxation safe, since the child's rights are bounded by the parent's and the
// caller must cover those.
func TestIntegration_Derived_ResolvedFloorKeepsTheParentBoundary(t *testing.T) {
	resetRBAC(t)
	dba := setupScopedDelegator(t)
	insertRoleScoped(t, "prod", "coven=prod", "incarnation.*")

	err := newService(t).CreateRole(context.Background(), CreateRoleInput{
		Name:        "prod-probe",
		Permissions: []string{"incarnation.get"},
		ParentRole:  ptr("prod"),
		CallerAID:   dba,
	})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld (the caller does not hold the prod parent)", err)
	}
	assertRoleAbsent(t, "prod-probe")
}

// TestIntegration_Derived_ResolvedFloorStillAttenuates — the structural half is
// untouched by the floor's move: a scoped delegator asking for a permission the
// parent does not hold is refused on the parent, not waved through because the
// ceiling would have narrowed it anyway.
func TestIntegration_Derived_ResolvedFloorStillAttenuates(t *testing.T) {
	resetRBAC(t)
	dba := setupScopedDelegator(t)
	insertRoleScoped(t, "dba-narrow", "coven=dba", "incarnation.get")

	err := newService(t).CreateRole(context.Background(), CreateRoleInput{
		Name:        "too-much",
		Permissions: []string{"incarnation.get", "incarnation.destroy"},
		ParentRole:  ptr("dba-narrow"),
		CallerAID:   dba,
	})
	if !errors.Is(err, ErrRoleExceedsParent) {
		t.Fatalf("err = %v, want ErrRoleExceedsParent", err)
	}
	assertRoleAbsent(t, "too-much")
}

// scopeOf reads a role's stored default_scope (nil = NULL).
func scopeOf(t *testing.T, name string) *string {
	t.Helper()
	var scope *string
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT default_scope FROM rbac_roles WHERE name = $1`, name).Scan(&scope); err != nil {
		t.Fatalf("read default_scope of %q: %v", name, err)
	}
	return scope
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
