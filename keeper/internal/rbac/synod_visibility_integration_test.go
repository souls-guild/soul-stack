//go:build integration

// Guard matrix for Synod catalog visibility (NIM-216, synod_visibility.go):
// `synod.list` answers with the groups the caller could add someone to, not with
// the cluster's delegation structure. The sibling of the role-catalog rule
// (NIM-202) and the last list that still carried a piece of the privilege map.
//
// Against a real DB rather than a fake pool: the answer is assembled from three
// catalogs at once — groups, the resolved role views their bundles point at, and
// the caller's own effective permissions — and a stub of that shape would be
// testing the stub. The fixtures here are the ones a leak would be reported on: a
// scoped operator, a group they administer, a group they must not see.
//
// Shares container / resetRBAC / seedOperator / insertRole / insertRoleScoped /
// seedClusterAdmin / seedSynod / addToSynod / newService with the other
// integration files in this package.
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

// synodNames lists the catalog a caller sees, for set assertions.
func synodNames(views []SynodView) []string {
	out := make([]string, 0, len(views))
	for _, v := range views {
		out = append(out, v.Name)
	}
	return out
}

// seesSynod reports whether name is in the caller's answer.
func seesSynod(views []SynodView, name string) bool {
	for _, v := range views {
		if v.Name == name {
			return true
		}
	}
	return false
}

// setupSynodCatalog builds the shape the rule is about: `prod-ops` bundles a role
// scoped to coven=prod, `payments` bundles a role the scoped caller holds nothing
// of, and `empty` bundles nothing at all. sub holds only the prod role.
func setupSynodCatalog(t *testing.T) (sub, admin string) {
	t.Helper()
	ctx := context.Background()
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	seedClusterAdmin(t, "archon-alice")
	a := "archon-alice"
	seedOperator(t, "archon-sub", &a)

	insertRoleScoped(t, "prod-runners", "coven=prod", "incarnation.run")
	insertRole(t, "payments-admin", "incarnation.destroy")
	if err := GrantOperator(ctx, integrationPool, "prod-runners", "archon-sub", &a); err != nil {
		t.Fatalf("grant sub→prod-runners: %v", err)
	}

	seedSynod(t, "prod-ops", "prod-runners")
	seedSynod(t, "payments", "payments-admin")
	seedSynod(t, "empty")
	addToSynod(t, "payments", "archon-alice")
	return "archon-sub", a
}

// The leak itself: a group bundling a role the caller holds nothing of exposes
// both its membership and the fact that such a bundle exists. `synod.list` is the
// right to read the catalog, not to read who holds which package of privilege.
func TestIntegration_SynodVisibility_ForeignGroupHidden(t *testing.T) {
	sub, _ := setupSynodCatalog(t)

	views, err := newService(t).ListSynods(context.Background(), sub)
	if err != nil {
		t.Fatalf("ListSynods: %v", err)
	}
	if seesSynod(views, "payments") {
		t.Errorf("catalog = %v, must NOT contain payments — sub holds no incarnation.destroy", synodNames(views))
	}
}

// The other half of the same rule: the group whose bundle the caller covers stays
// visible. A filter that hid everything would "fix" the leak and break the feature.
func TestIntegration_SynodVisibility_CoveredGroupVisible(t *testing.T) {
	sub, _ := setupSynodCatalog(t)

	views, err := newService(t).ListSynods(context.Background(), sub)
	if err != nil {
		t.Fatalf("ListSynods: %v", err)
	}
	if !seesSynod(views, "prod-ops") {
		t.Errorf("catalog = %v, must contain prod-ops — sub holds incarnation.run on coven=prod", synodNames(views))
	}
}

// A group that bundles NOTHING is visible to everyone: the quantifier runs over an
// empty bundle, and there is no privilege to expose. Mirrors the role rule, where
// a role granting nothing is visible to everyone.
func TestIntegration_SynodVisibility_EmptyGroupVisibleToAll(t *testing.T) {
	sub, _ := setupSynodCatalog(t)

	views, err := newService(t).ListSynods(context.Background(), sub)
	if err != nil {
		t.Fatalf("ListSynods: %v", err)
	}
	if !seesSynod(views, "empty") {
		t.Errorf("catalog = %v, must contain empty — it bundles nothing", synodNames(views))
	}
}

// A bare `*` covers every role, so the cluster-admin surface is unchanged. If this
// ever failed, bootstrap would lose sight of the groups it just created.
func TestIntegration_SynodVisibility_ClusterAdminSeesAll(t *testing.T) {
	_, admin := setupSynodCatalog(t)

	views, err := newService(t).ListSynods(context.Background(), admin)
	if err != nil {
		t.Fatalf("ListSynods: %v", err)
	}
	for _, want := range []string{"prod-ops", "payments", "empty"} {
		if !seesSynod(views, want) {
			t.Errorf("catalog = %v, cluster-admin must see %q", synodNames(views), want)
		}
	}
}

// The auditor: sees every group while holding nothing any of them bundle. No
// coverage rule can express that reader, which is why `synod.list-all` exists —
// and without it an auditor granted `role.list-all` would see every role but none
// of the groups those roles are bundled into.
func TestIntegration_SynodVisibility_ListAllRightSeesAll(t *testing.T) {
	sub, alice := setupSynodCatalog(t)
	insertRole(t, "synod-auditors", "synod.list", "synod.list-all")
	if err := GrantOperator(context.Background(), integrationPool, "synod-auditors", sub, &alice); err != nil {
		t.Fatalf("grant sub→synod-auditors: %v", err)
	}

	views, err := newService(t).ListSynods(context.Background(), sub)
	if err != nil {
		t.Fatalf("ListSynods: %v", err)
	}
	if !seesSynod(views, "payments") {
		t.Errorf("catalog = %v, synod.list-all must return payments too", synodNames(views))
	}
}

// A SCOPED `synod.list-all` is not the full catalog: the right is bare on purpose,
// so [callerHolds] demands an unrestricted holder. The scope grammar has no
// `synod=` dimension, so a scoped form could not say which groups it covers —
// the same reasoning that governs `role.list-all` and `role.create-root`.
func TestIntegration_SynodVisibility_ScopedListAllIsNotFullCatalog(t *testing.T) {
	sub, alice := setupSynodCatalog(t)
	insertRoleScoped(t, "scoped-auditors", "coven=prod", "synod.list", "synod.list-all")
	if err := GrantOperator(context.Background(), integrationPool, "scoped-auditors", sub, &alice); err != nil {
		t.Fatalf("grant sub→scoped-auditors: %v", err)
	}

	views, err := newService(t).ListSynods(context.Background(), sub)
	if err != nil {
		t.Fatalf("ListSynods: %v", err)
	}
	if seesSynod(views, "payments") {
		t.Errorf("catalog = %v, a scoped synod.list-all must not open the full catalog", synodNames(views))
	}
}

// Visibility is all-or-nothing per group: once the caller may see it, the roster
// comes back whole. They could add any of those members themselves, so hiding the
// list would be a half-truth with no boundary behind it — the same call NIM-202
// made for roles.
func TestIntegration_SynodVisibility_VisibleGroupComesBackWhole(t *testing.T) {
	sub, alice := setupSynodCatalog(t)
	addToSynod(t, "prod-ops", alice)

	views, err := newService(t).ListSynods(context.Background(), sub)
	if err != nil {
		t.Fatalf("ListSynods: %v", err)
	}
	for _, v := range views {
		if v.Name != "prod-ops" {
			continue
		}
		if len(v.Operators) != 1 || v.Operators[0] != alice {
			t.Errorf("operators of a visible group = %v, want [%s] — visible means whole", v.Operators, alice)
		}
		return
	}
	t.Fatalf("catalog = %v, prod-ops must be visible", synodNames(views))
}

// Fail-closed on a missing caller. The filter has no basis without one, and the
// unfiltered catalog is exactly the leak this closes — so a transport that forgot
// to pass the subject gets a refusal rather than everything.
func TestIntegration_SynodVisibility_MissingCallerRefused(t *testing.T) {
	setupSynodCatalog(t)

	_, err := newService(t).ListSynods(context.Background(), "")
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld for an empty caller", err)
	}
}
