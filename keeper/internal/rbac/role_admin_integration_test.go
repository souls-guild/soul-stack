//go:build integration

// Guard matrix for the administration gate (NIM-214, role_admin.go): editing and
// deleting a role are bounded by what the caller could GRANT, not by holding a
// cluster-level `role.update` / `role.delete`.
//
// Against a real DB because the rule is about resolved chains: a derived role is
// judged on what it grants after attenuation, and the model the ticket states —
// "every holder of P administers the roles derived from P" — is a CONSEQUENCE of
// the coverage rule rather than a separate lookup, which only a real subtree can
// demonstrate.
//
// Shares container / resetRBAC / seedOperator / seedClusterAdmin / insertRole /
// insertRoleScoped / insertDerived / newService / roleExists / rolePerms with the
// other integration files in this package.
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

// setupPlatformSubtree builds the shape the model is about: sub holds `platform`,
// under which `platform-web` and its own child `platform-web-eu` are derived.
// `payments` stands outside that subtree entirely and grants something sub does
// not hold. alice is cluster-admin so the cluster is never one revocation from
// lockout.
func setupPlatformSubtree(t *testing.T) (sub, alice string) {
	t.Helper()
	ctx := context.Background()
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	seedClusterAdmin(t, "archon-alice")
	a := "archon-alice"
	seedOperator(t, "archon-sub", &a)

	insertRole(t, "platform", "incarnation.run", "soul.list")
	insertDerived(t, "platform-web", "platform", "", "incarnation.run")
	insertDerived(t, "platform-web-eu", "platform-web", "", "incarnation.run")
	insertRole(t, "payments", "incarnation.destroy")
	if err := GrantOperator(ctx, integrationPool, "platform", "archon-sub", &a); err != nil {
		t.Fatalf("grant sub→platform: %v", err)
	}
	return "archon-sub", a
}

// The model the ticket states, holding for a GRANDCHILD: every holder of P
// administers the roles derived from P. Nothing in the gate mentions subtrees —
// a derived role's rights are contained in its parent's, so anyone holding the
// parent covers them, and the coverage rule says yes on its own.
func TestIntegration_RoleAdmin_HolderOfParentAdministersDescendant(t *testing.T) {
	sub, _ := setupPlatformSubtree(t)
	s := newService(t)

	if err := s.UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name:        "platform-web-eu",
		Permissions: []string{},
		CallerAID:   sub,
	}); err != nil {
		t.Fatalf("a holder of platform must administer its grandchild: %v", err)
	}
	if got := rolePerms(t, "platform-web-eu"); len(got) != 0 {
		t.Errorf("permissions = %v, want the trim applied", got)
	}
}

// The refusal that did not exist before: a role outside the caller's reach. sub
// holds nothing of `incarnation.destroy`, so `payments` is not its to rewrite —
// even though the edit here only REMOVES a permission, which no floor objects to.
func TestIntegration_RoleAdmin_ForeignRoleRefused(t *testing.T) {
	sub, _ := setupPlatformSubtree(t)
	s := newService(t)

	err := s.UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name:        "payments",
		Permissions: []string{},
		CallerAID:   sub,
	})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld (payments is not sub's to administer)", err)
	}
	if got := rolePerms(t, "payments"); len(got) != 1 {
		t.Errorf("permissions = %v, want the role left untouched", got)
	}
}

// `role.delete` had NO caller-side check of any kind: the service did not even
// take a caller, so any holder could drop any non-builtin role in the cluster.
// This is the first refusal on that path.
func TestIntegration_RoleAdmin_DeleteForeignRoleRefused(t *testing.T) {
	sub, _ := setupPlatformSubtree(t)
	s := newService(t)

	err := s.DeleteRole(context.Background(), "payments", sub)
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld (deleting a role beyond the caller's rights)", err)
	}
	if !roleExists(t, "payments") {
		t.Error("payments was deleted by an operator who could not grant what it gives")
	}
}

// Deleting inside one's own subtree stays open — the gate scopes administration,
// it does not remove it. The leaf goes first because the orphan policy is
// fail-closed RESTRICT (ADR-078(g)).
func TestIntegration_RoleAdmin_DeleteDescendantAllowed(t *testing.T) {
	sub, _ := setupPlatformSubtree(t)
	s := newService(t)

	if err := s.DeleteRole(context.Background(), "platform-web-eu", sub); err != nil {
		t.Fatalf("a holder of platform must be able to delete its descendant: %v", err)
	}
	if roleExists(t, "platform-web-eu") {
		t.Error("role not deleted")
	}
}

// A bare `*` covers everything, so cluster-admins administer the whole catalog
// exactly as before. If this ever failed, bootstrap could not maintain the roles
// it lays down.
func TestIntegration_RoleAdmin_ClusterAdminUnaffected(t *testing.T) {
	_, alice := setupPlatformSubtree(t)
	s := newService(t)

	if err := s.UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name:        "payments",
		Permissions: []string{"incarnation.destroy", "soul.list"},
		CallerAID:   alice,
	}); err != nil {
		t.Fatalf("cluster-admin must administer any role: %v", err)
	}
	if err := s.DeleteRole(context.Background(), "payments", alice); err != nil {
		t.Fatalf("cluster-admin must delete any role: %v", err)
	}
}

// A role that grants NOTHING is covered by everyone, so anyone may administer it.
// The same answer the catalog filter gives for visibility, and for the same
// reason: there is no privilege to protect. Without this, an empty role created
// by one operator would be undeletable by any other.
func TestIntegration_RoleAdmin_EmptyRoleIsAdministrableByAnyone(t *testing.T) {
	sub, _ := setupPlatformSubtree(t)
	insertRole(t, "empty-shell")
	s := newService(t)

	if err := s.DeleteRole(context.Background(), "empty-shell", sub); err != nil {
		t.Fatalf("an empty role exposes no privilege and must stay administrable: %v", err)
	}
	if roleExists(t, "empty-shell") {
		t.Error("empty role not deleted")
	}
}

// A PLAIN role sits in nobody's subtree, so a rule phrased as "descendant of a
// role you hold" would leave it unadministrable by anyone but a cluster-admin —
// the orphan the ticket asks about. Phrased as coverage it has an answer:
// whoever could have created it can maintain it, which is what `role.create-root`
// already selects for.
func TestIntegration_RoleAdmin_PlainRoleIsNotOrphaned(t *testing.T) {
	sub, alice := setupPlatformSubtree(t)
	// A plain role entirely inside sub's rights, derived from nothing.
	insertRole(t, "standalone", "soul.list")
	_ = alice
	s := newService(t)

	if err := s.UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name:        "standalone",
		Permissions: []string{},
		CallerAID:   sub,
	}); err != nil {
		t.Fatalf("a plain role within the caller's rights must stay administrable: %v", err)
	}
}

// ============================================================
// Taking a binding apart is administering it too (NIM-285)
// ============================================================

// The hole one door along from the gate above: revoking a role from an operator
// carried no caller AT ALL — the input struct did not have the field — so any
// holder of `role.revoke-operator` could strip any archon of any role. Only
// self-lockout stood in the way, and it fires when the LAST `*` admin would go,
// saying nothing about the thousand revocations before that.
func TestIntegration_RoleAdmin_RevokeFromForeignRoleRefused(t *testing.T) {
	sub, alice := setupPlatformSubtree(t)
	seedOperator(t, "archon-victim", &alice)
	if err := GrantOperator(context.Background(), integrationPool, "payments", "archon-victim", &alice); err != nil {
		t.Fatalf("grant victim→payments: %v", err)
	}
	s := newService(t)

	err := s.RevokeOperator(context.Background(), RevokeOperatorInput{
		RoleName:  "payments",
		AID:       "archon-victim",
		CallerAID: sub,
	})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld (payments is not sub's to unbind)", err)
	}
	if membershipCount(t, "payments") != 1 {
		t.Error("membership removed by an operator who could not have created it")
	}
}

// The symmetry that defines the rule: unbinding asks for exactly what binding
// asks for. sub could have granted `platform-web` to anyone — it is inside the
// role sub holds — so sub may take it back.
func TestIntegration_RoleAdmin_RevokeWithinReachAllowed(t *testing.T) {
	sub, alice := setupPlatformSubtree(t)
	seedOperator(t, "archon-teammate", &alice)
	if err := GrantOperator(context.Background(), integrationPool, "platform-web", "archon-teammate", &alice); err != nil {
		t.Fatalf("grant teammate→platform-web: %v", err)
	}
	s := newService(t)

	if err := s.RevokeOperator(context.Background(), RevokeOperatorInput{
		RoleName:  "platform-web",
		AID:       "archon-teammate",
		CallerAID: sub,
	}); err != nil {
		t.Fatalf("a holder of platform must be able to unbind its descendant: %v", err)
	}
	if membershipCount(t, "platform-web") != 0 {
		t.Error("membership not removed")
	}
}

// The Synod pair of the same rule. Emptying a group of its members is measured
// against the WHOLE bundle, exactly as adding one is (ADR-049(f)): a member
// receives everything the group bundles, so both directions weigh the same set.
func TestIntegration_RoleAdmin_RemoveFromForeignSynodRefused(t *testing.T) {
	sub, alice := setupPlatformSubtree(t)
	seedOperator(t, "archon-victim", &alice)
	seedSynod(t, "payments-team", "payments")
	addToSynod(t, "payments-team", "archon-victim")
	s := newService(t)

	err := s.RemoveOperator(context.Background(), RemoveOperatorInput{
		SynodName: "payments-team",
		AID:       "archon-victim",
		CallerAID: sub,
	})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld (the bundle is beyond sub's rights)", err)
	}
	if synodOperatorCount(t, "payments-team") != 1 {
		t.Error("membership removed by an operator who could not have added it")
	}
}

// And the bundle itself: revoking a role from a group is measured against THAT
// role, the set `synod.grant-role` demands to put it there.
func TestIntegration_RoleAdmin_RevokeForeignRoleFromSynodRefused(t *testing.T) {
	sub, _ := setupPlatformSubtree(t)
	seedSynod(t, "mixed-team", "payments")
	s := newService(t)

	err := s.RevokeRole(context.Background(), RevokeRoleInput{
		SynodName: "mixed-team",
		RoleName:  "payments",
		CallerAID: sub,
	})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld (payments is beyond sub's rights)", err)
	}
	if synodRoleCount(t, "mixed-team") != 1 {
		t.Error("role removed from the bundle by an operator who could not have granted it")
	}
}

// Administration is strictly narrower than the route right: holding `role.update`
// buys nothing on a role beyond the caller's reach. Pinned because the route gate
// (RequireAction, NoSelector) is unchanged — the boundary moved into the service,
// and a future refactor that "simplifies" it back to the route would reopen the
// whole surface.
func TestIntegration_RoleAdmin_RoleUpdateRightIsNotAdministration(t *testing.T) {
	sub, alice := setupPlatformSubtree(t)
	insertRole(t, "updaters", "role.update", "role.delete")
	if err := GrantOperator(context.Background(), integrationPool, "updaters", sub, &alice); err != nil {
		t.Fatalf("grant sub→updaters: %v", err)
	}
	s := newService(t)

	err := s.UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name:        "payments",
		Permissions: []string{},
		CallerAID:   sub,
	})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld — role.update is the route, not the reach", err)
	}
}
