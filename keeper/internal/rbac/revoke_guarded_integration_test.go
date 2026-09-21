//go:build integration

// The package-level revoke carries the self-lockout probe, and the bare row
// delete is reachable only by its own name (NIM-320).
//
// [Service.RevokeOperator] was always guarded, and the package-level function's
// doc said so — "the self-lockout check lives in Service.RevokeOperator". The
// federated reconciler in keeper/internal/auth cannot use the Service (it revokes
// inside its own transaction, alongside the grants it makes in the same breath),
// took the package-level function, and stripped memberships with no probe. These
// guards pin the arrangement that replaced the comment.
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

// The guarded entry point refuses to strand the cluster — no Service, no caller,
// no transaction of its own. This is the call the reconciler makes.
func TestIntegration_RevokeOperator_PackageLevelIsGuarded(t *testing.T) {
	resetRBAC(t)
	seedLoneAdmin(t)

	err := RevokeOperator(context.Background(), integrationPool, "extra-admin", "archon-alice")
	if !errors.Is(err, ErrWouldLockOutCluster) {
		t.Fatalf("err = %v, want ErrWouldLockOutCluster.\n"+
			"rbac.RevokeOperator is the name every caller outside the Service reaches for; "+
			"if it stops probing, the federated reconciler stops probing with it.", err)
	}
	if membershipCount(t, "extra-admin") != 1 {
		t.Error("membership was revoked despite the refusal")
	}
}

// And it stays out of the way when another administrator survives.
func TestIntegration_RevokeOperator_PackageLevelAllowsSafeRevoke(t *testing.T) {
	resetRBAC(t)
	setupTwoAdminPaths(t)

	if err := RevokeOperator(context.Background(), integrationPool, "extra-admin", "archon-bob"); err != nil {
		t.Fatalf("RevokeOperator (alice still administers the cluster): %v", err)
	}
	if membershipCount(t, "extra-admin") != 0 {
		t.Error("membership was not revoked")
	}
}

// The probe is skipped for a role that is not a cluster-admin source, and that
// skip is load-bearing rather than an optimisation: the probe reports lockout
// whenever the surviving set is empty, so a cluster with no administrator at all
// would refuse every ordinary revoke if the short-circuit went away. Here nobody
// holds `*` and revoking a plain role still works.
func TestIntegration_RevokeOperator_NonAdminRoleSkipsProbe(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	insertRole(t, "viewer", "soul.list")
	if err := GrantOperator(context.Background(), integrationPool, "viewer", "archon-alice", nil); err != nil {
		t.Fatalf("grant: %v", err)
	}

	if err := RevokeOperator(context.Background(), integrationPool, "viewer", "archon-alice"); err != nil {
		t.Fatalf("RevokeOperator on a role that grants no `*`: %v", err)
	}
	if membershipCount(t, "viewer") != 0 {
		t.Error("membership was not revoked")
	}
}

// RevokeOperatorRow enforces nothing, on purpose. Pinned so the distinction
// between the two names is a tested property rather than a naming convention —
// and so that anyone who swaps one for the other sees this test rather than
// discovering the difference in a live cluster.
func TestIntegration_RevokeOperatorRow_IsUnguarded(t *testing.T) {
	resetRBAC(t)
	seedLoneAdmin(t)

	if err := RevokeOperatorRow(context.Background(), integrationPool, "extra-admin", "archon-alice"); err != nil {
		t.Fatalf("RevokeOperatorRow is the bare DELETE and must not refuse: %v", err)
	}
	if membershipCount(t, "extra-admin") != 0 {
		t.Error("the row was not deleted")
	}
	// The cluster now has no administrator. That is exactly why this function is
	// named the way it is and has one caller.
	if admins, err := LockEffectiveClusterAdmins(context.Background(), integrationPool); err != nil {
		t.Fatalf("LockEffectiveClusterAdmins: %v", err)
	} else if len(admins) != 0 {
		t.Errorf("effective admins = %v, want none (the point of this test)", admins)
	}
}
