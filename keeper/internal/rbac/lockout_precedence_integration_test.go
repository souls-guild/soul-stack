//go:build integration

// The self-lockout guard runs before every caller-rights gate, and its refusal
// outranks theirs (NIM-319, ADR-078(n)).
//
// The guards here exist because the ordering is invisible in the happy path: with
// a cluster-admin caller both orders refuse, and every self-lockout test in
// crud_integration_test.go and synod_crud_integration_test.go passes either way.
// NIM-214 and NIM-285 inverted it on five mutations and the whole suite stayed
// green — only keeper/internal/bootstrap, which calls the service with no caller,
// noticed. So each test below drives the mutation down a path where the two orders
// disagree, and asserts which error comes back.
//
// Two directions are pinned, and both matter:
//
//   - a mutation reaches the guard with NO caller in the picture. The guarantee is
//     about cluster state and must not depend on a subject being present.
//   - the caller gates are still there. The fix is an ordering, not a removal: a
//     caller-less mutation that would NOT lock anyone out is still refused.
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

// seedLoneAdmin makes archon-alice the only holder of `*`, through the plain role
// extra-admin. cluster-admin keeps its `*` but has no members, so extra-admin is
// the single cluster-admin source — the shape every lockout probe is about.
func seedLoneAdmin(t *testing.T) {
	t.Helper()
	seedOperator(t, "archon-alice", nil)
	insertRole(t, "extra-admin", "*")
	if err := GrantOperator(context.Background(), integrationPool, "extra-admin", "archon-alice", nil); err != nil {
		t.Fatalf("seedLoneAdmin grant: %v", err)
	}
}

// seedLoneAdminViaSynod makes archon-grpadmin the only holder of `*`, and only
// through the group admins-grp.
func seedLoneAdminViaSynod(t *testing.T) {
	t.Helper()
	seedOperator(t, "archon-grpadmin", nil)
	insertRole(t, "grp-admin-role", "*")
	seedSynod(t, "admins-grp", "grp-admin-role")
	addToSynod(t, "admins-grp", "archon-grpadmin")
}

// --- the guard is reached with no caller at all ---

// This is the regression keeper/internal/bootstrap caught: `keeper init` builds a
// Service and revokes without claims, and got a least-privilege refusal naming a
// missing caller where the lockout guard should have answered.
func TestIntegration_LockoutPrecedence_RevokeOperator_NoCaller(t *testing.T) {
	resetRBAC(t)
	seedLoneAdmin(t)
	s := newService(t)

	err := s.RevokeOperator(context.Background(), RevokeOperatorInput{
		RoleName: "extra-admin", AID: "archon-alice",
	})
	if !errors.Is(err, ErrWouldLockOutCluster) {
		t.Fatalf("err = %v, want ErrWouldLockOutCluster.\n"+
			"The last `*` admin is being revoked with no caller in the input. A refusal "+
			"about the caller's rights means the guard sits behind a gate that needs a "+
			"subject, and the cluster-state invariant was not consulted at all.", err)
	}
	if membershipCount(t, "extra-admin") != 1 {
		t.Error("membership was revoked despite lockout (tx did not roll back)")
	}
}

func TestIntegration_LockoutPrecedence_DeleteRole_NoCaller(t *testing.T) {
	resetRBAC(t)
	seedLoneAdmin(t)
	s := newService(t)

	err := s.DeleteRole(context.Background(), "extra-admin", "")
	if !errors.Is(err, ErrWouldLockOutCluster) {
		t.Fatalf("err = %v, want ErrWouldLockOutCluster (deleting the last `*` role, no caller)", err)
	}
	if !roleExists(t, "extra-admin") {
		t.Error("role was deleted despite lockout (tx did not roll back)")
	}
}

func TestIntegration_LockoutPrecedence_UpdateRole_NoCaller(t *testing.T) {
	resetRBAC(t)
	seedLoneAdmin(t)
	s := newService(t)

	err := s.UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name: "extra-admin", Permissions: []string{"soul.list"},
	})
	if !errors.Is(err, ErrWouldLockOutCluster) {
		t.Fatalf("err = %v, want ErrWouldLockOutCluster (`*` removed from the last admin role, no caller)", err)
	}
	if got := rolePerms(t, "extra-admin"); len(got) != 1 || got[0] != "*" {
		t.Errorf("permissions = %v, want [*] (rollback)", got)
	}
}

func TestIntegration_LockoutPrecedence_SynodRemoveOperator_NoCaller(t *testing.T) {
	resetRBAC(t)
	seedLoneAdminViaSynod(t)
	s := newService(t)

	err := s.RemoveOperator(context.Background(), RemoveOperatorInput{
		SynodName: "admins-grp", AID: "archon-grpadmin",
	})
	if !errors.Is(err, ErrWouldLockOutCluster) {
		t.Fatalf("err = %v, want ErrWouldLockOutCluster (last `*` member removed from the group, no caller)", err)
	}
	if synodOperatorCount(t, "admins-grp") != 1 {
		t.Error("member removed despite lockout (tx did not roll back)")
	}
}

func TestIntegration_LockoutPrecedence_SynodRevokeRole_NoCaller(t *testing.T) {
	resetRBAC(t)
	seedLoneAdminViaSynod(t)
	s := newService(t)

	err := s.RevokeRole(context.Background(), RevokeRoleInput{
		SynodName: "admins-grp", RoleName: "grp-admin-role",
	})
	if !errors.Is(err, ErrWouldLockOutCluster) {
		t.Fatalf("err = %v, want ErrWouldLockOutCluster (group's last `*` role revoked, no caller)", err)
	}
	if synodRoleCount(t, "admins-grp") != 1 {
		t.Error("role revoked despite lockout (tx did not roll back)")
	}
}

// --- the guard outranks a refusal about the caller ---

// A caller who could not unbind the role gets the lockout error, not the
// least-privilege one. This is the disclosure accepted deliberately in NIM-319:
// the refusal tells a holder of `role.revoke-operator` that the target is the last
// `*` holder. That audience already passed the enforcer's Check for a
// role-administration right, and the row lock above already discloses whether the
// membership exists. Locking a live cluster out has no path back through the API.
func TestIntegration_LockoutPrecedence_UnprivilegedCallerStillHitsGuard(t *testing.T) {
	resetRBAC(t)
	seedLoneAdmin(t)
	alice := "archon-alice"
	seedOperator(t, "archon-carol", &alice)
	insertRole(t, "viewer", "soul.list")
	if err := GrantOperator(context.Background(), integrationPool, "viewer", "archon-carol", &alice); err != nil {
		t.Fatalf("grant carol: %v", err)
	}
	s := newService(t)

	err := s.RevokeOperator(context.Background(), RevokeOperatorInput{
		RoleName: "extra-admin", AID: "archon-alice", CallerAID: "archon-carol",
	})
	if !errors.Is(err, ErrWouldLockOutCluster) {
		t.Fatalf("err = %v, want ErrWouldLockOutCluster.\n"+
			"carol holds soul.list and could not unbind a `*` role, so both gates refuse. "+
			"The lockout guard is the one no caller can satisfy, so it answers first.", err)
	}
}

// The same precedence against a refusal that is not about rights at all: this
// PATCH names a parent that does not exist AND turns the last cluster-admin source
// derived (ADR-078(i): a derived `*` is capped and the probes stop counting it).
// Both are refusals; the unwaivable one is reported.
func TestIntegration_LockoutPrecedence_UpdateRole_BeatsUnresolvableParent(t *testing.T) {
	resetRBAC(t)
	seedLoneAdmin(t)
	s := newService(t)
	ghost := "no-such-role"

	err := s.UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name: "extra-admin", Permissions: []string{"*"}, CallerAID: "archon-alice",
		SetParentRole: true, ParentRole: &ghost,
	})
	if !errors.Is(err, ErrWouldLockOutCluster) {
		t.Fatalf("err = %v, want ErrWouldLockOutCluster.\n"+
			"Deriving the last `*` role would leave the cluster with no admin source, and "+
			"that is decided before the parent is resolved. A parent-side error here means "+
			"the guard moved back behind the gates that read the chain.", err)
	}
}

// --- the caller gates are still in force ---

// The fix is an ordering, not a removal. Revoking a NON-last admin with no caller
// passes the lockout guard and must then be refused by NIM-285's gate — if this
// starts succeeding, the caller gate was dropped rather than reordered.
func TestIntegration_LockoutPrecedence_NoCaller_NotLastAdmin_StillRefused(t *testing.T) {
	resetRBAC(t)
	setupTwoAdminPaths(t)
	s := newService(t)

	err := s.RevokeOperator(context.Background(), RevokeOperatorInput{
		RoleName: "extra-admin", AID: "archon-bob",
	})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld (alice survives as admin, so the "+
			"lockout guard passes and NIM-285's caller gate must refuse a caller-less unbind)", err)
	}
	if membershipCount(t, "extra-admin") != 1 {
		t.Error("membership was revoked despite the refusal")
	}
}

// Same for the role-administration gate (NIM-214): a role that grants no `*`
// never reaches a lockout probe, so a caller-less delete must be refused.
func TestIntegration_LockoutPrecedence_NoCaller_PlainRoleDelete_StillRefused(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	insertRole(t, "viewer", "soul.list")
	s := newService(t)

	err := s.DeleteRole(context.Background(), "viewer", "")
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld (no `*` involved, so NIM-214's gate answers)", err)
	}
	if !roleExists(t, "viewer") {
		t.Error("role was deleted despite the refusal")
	}
}

// And for the Synod side of NIM-285.
func TestIntegration_LockoutPrecedence_NoCaller_PlainSynodRemove_StillRefused(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	insertRole(t, "viewer", "soul.list")
	seedSynod(t, "team", "viewer")
	addToSynod(t, "team", "archon-alice")
	s := newService(t)

	err := s.RemoveOperator(context.Background(), RemoveOperatorInput{
		SynodName: "team", AID: "archon-alice",
	})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld (the group bundles no `*`, so NIM-285's gate answers)", err)
	}
	if synodOperatorCount(t, "team") != 1 {
		t.Error("member was removed despite the refusal")
	}
}
