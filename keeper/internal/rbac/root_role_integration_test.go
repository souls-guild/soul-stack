//go:build integration

// Guard matrix for the root-role gate (NIM-201, root_role.go): minting a role
// with no parent that grants something requires `role.create-root`.
//
// The gate had no service-level test of its own — NIM-201 shipped with coverage
// only at the API handler. That is what let it land as a silent break of nine
// integration tests in neighbouring files (NIM-218): every fixture there minted
// PLAIN roles, because that was the only shape a role could have when they were
// written. Those fixtures now carry the shape right; this file is where the gate
// itself is pinned, so a future change to it fails HERE and not as collateral in
// a test about something else.
//
// Two properties are load-bearing and easy to lose by accident:
//
//   - the gate judges the SHAPE of the result, not the verb — so it must fire on
//     a PATCH that grows an already-plain role just as it does on create, and
//     must NOT fire on a pure trim;
//   - the least-privilege floor runs BEFORE it (service.go), so a caller that
//     cannot grant the content at all still gets [ErrPermissionNotHeld]. Swap the
//     order and every "escalation denied" guard in subset_integration_test.go and
//     synod_security_integration_test.go starts passing for the wrong reason.
//
// Shares container / resetRBAC / seedOperator / insertRole / insertRoleScoped /
// newService / roleExists / rolePerms with integration_test.go +
// crud_integration_test.go + subset_integration_test.go (same rbac package).
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

// seedMinter seeds alice (cluster-admin, so the cluster is never one revocation
// from lockout) plus sub, and hands sub one UNSCOPED role carrying perms.
// Whether that role includes `role.create-root` is the whole variable in this
// file, so each test spells it out at the call site.
func seedMinter(t *testing.T, roleName string, perms ...string) (sub, alice string) {
	t.Helper()
	ctx := context.Background()
	seedOperator(t, "archon-alice", nil)
	a := "archon-alice"
	if err := GrantOperator(ctx, integrationPool, "cluster-admin", a, nil); err != nil {
		t.Fatalf("grant alice→cluster-admin: %v", err)
	}
	seedOperator(t, "archon-sub", &a)
	insertRole(t, roleName, perms...)
	if err := GrantOperator(ctx, integrationPool, roleName, "archon-sub", &a); err != nil {
		t.Fatalf("grant sub→%s: %v", roleName, err)
	}
	return "archon-sub", a
}

// ============================================================
// CREATE
// ============================================================

// The refusal is about SHAPE alone: the caller holds every permission it is
// putting in, so the floor has nothing to say, and the only thing standing in
// the way is that the resulting role would track nothing.
func TestIntegration_RootRole_CreateWithoutRight_Denied(t *testing.T) {
	resetRBAC(t)
	sub, _ := seedMinter(t, "runners", "incarnation.run", "role.create")
	s := newService(t)

	err := s.CreateRole(context.Background(), CreateRoleInput{
		Name:        "orphan-runners",
		Permissions: []string{"incarnation.run"},
		CallerAID:   sub,
	})
	if !errors.Is(err, ErrRootRoleNotPermitted) {
		t.Fatalf("err = %v, want ErrRootRoleNotPermitted (plain role, caller has no role.create-root)", err)
	}
	if roleExists(t, "orphan-runners") {
		t.Error("plain role created without role.create-root")
	}
}

// A parentless role that grants NOTHING is free — there is no privilege to
// strand. Mirrors the visibility rule, where an empty role is visible to
// everyone (role_visibility.go).
func TestIntegration_RootRole_CreateEmpty_Allowed(t *testing.T) {
	resetRBAC(t)
	sub, _ := seedMinter(t, "runners", "incarnation.run", "role.create")
	s := newService(t)

	if err := s.CreateRole(context.Background(), CreateRoleInput{
		Name:      "empty-shell",
		CallerAID: sub,
	}); err != nil {
		t.Fatalf("CreateRole (parentless but empty, nothing to strand): %v", err)
	}
	if !roleExists(t, "empty-shell") {
		t.Error("empty parentless role not created")
	}
}

// The same request as CreateWithoutRight_Denied, with the shape right added and
// nothing else changed.
func TestIntegration_RootRole_CreateWithRight_Allowed(t *testing.T) {
	resetRBAC(t)
	sub, _ := seedMinter(t, "runners", "incarnation.run", "role.create", "role.create-root")
	s := newService(t)

	if err := s.CreateRole(context.Background(), CreateRoleInput{
		Name:        "orphan-runners",
		Permissions: []string{"incarnation.run"},
		CallerAID:   sub,
	}); err != nil {
		t.Fatalf("CreateRole (caller holds role.create-root): %v", err)
	}
	if got := rolePerms(t, "orphan-runners"); len(got) != 1 || got[0] != "incarnation.run" {
		t.Errorf("permissions = %v, want [incarnation.run]", got)
	}
}

// `*` covers the action for free, so a cluster-admin never meets this gate. If
// it ever did, bootstrap would be unable to lay down the first roles.
func TestIntegration_RootRole_ClusterAdminUnaffected(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	if err := GrantOperator(context.Background(), integrationPool, "cluster-admin", "archon-alice", nil); err != nil {
		t.Fatalf("grant alice→cluster-admin: %v", err)
	}
	s := newService(t)

	if err := s.CreateRole(context.Background(), CreateRoleInput{
		Name:        "ops",
		Permissions: []string{"incarnation.run", "soul.list"},
		CallerAID:   "archon-alice",
	}); err != nil {
		t.Fatalf("CreateRole as cluster-admin: %v", err)
	}
	if !roleExists(t, "ops") {
		t.Error("cluster-admin could not mint a plain role")
	}
}

// The sanctioned path out of the refusal: derive. Same caller, same permission,
// no `role.create-root` anywhere — it goes through because the result tracks its
// parent and narrows when the parent narrows (ADR-078(c)/(d)).
func TestIntegration_RootRole_DerivedRoleIsTheSanctionedPath(t *testing.T) {
	resetRBAC(t)
	sub, _ := seedMinter(t, "runners", "incarnation.run", "role.create")
	insertRole(t, "base-runners", "incarnation.run")
	s := newService(t)

	parent := "base-runners"
	if err := s.CreateRole(context.Background(), CreateRoleInput{
		Name:        "team-runners",
		Permissions: []string{"incarnation.run"},
		ParentRole:  &parent,
		CallerAID:   sub,
	}); err != nil {
		t.Fatalf("CreateRole (derived — the path the gate points at): %v", err)
	}
	if !roleExists(t, "team-runners") {
		t.Error("derived role not created")
	}
}

// A SCOPED `role.create-root` does not satisfy the gate: `callerPermissions`
// expands a bare permission under its role's default_scope, and the gate asks
// [callerHolds] for an UNRESTRICTED holder. So this caller — which explicitly
// has the right — still cannot mint, and the refusal is deliberate: the scope
// grammar has no `role=` dimension, so `role.create-root on coven=prod` could
// not say WHICH roles it covers (root_role.go, "Bare on purpose").
//
// Pinned here because it is invisible from the gate's own code and would flip
// silently if scope handling in [callerHolds] were ever relaxed toward
// scope-blindness — the direction NIM-219 is looking at for Enforcer.Check.
// The floor deliberately has nothing to say about this request: the permission
// being granted is inside the caller's scope.
func TestIntegration_RootRole_ScopedRight_StillDenied(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	alice := "archon-alice"
	if err := GrantOperator(context.Background(), integrationPool, "cluster-admin", alice, nil); err != nil {
		t.Fatalf("grant alice→cluster-admin: %v", err)
	}
	seedOperator(t, "archon-sub", &alice)
	insertRoleScoped(t, "prod-ops", "coven=prod", "incarnation.run", "role.create", "role.create-root")
	if err := GrantOperator(context.Background(), integrationPool, "prod-ops", "archon-sub", &alice); err != nil {
		t.Fatalf("grant sub→prod-ops: %v", err)
	}
	s := newService(t)

	err := s.CreateRole(context.Background(), CreateRoleInput{
		Name:        "prod-orphan",
		Permissions: []string{"incarnation.run on coven=prod"},
		CallerAID:   "archon-sub",
	})
	if !errors.Is(err, ErrRootRoleNotPermitted) {
		t.Fatalf("err = %v, want ErrRootRoleNotPermitted (role.create-root under a default_scope is not unrestricted)", err)
	}
	if roleExists(t, "prod-orphan") {
		t.Error("plain role created from a scoped role.create-root")
	}
}

// Ordering: the floor speaks first. This caller fails BOTH gates — it holds
// neither `incarnation.run` nor `role.create-root` — and must hear the more
// actionable of the two refusals, "you cannot grant that at all". Every
// escalation-denied guard elsewhere in the package asserts ErrPermissionNotHeld
// on exactly this ordering.
func TestIntegration_RootRole_FloorRunsBeforeTheGate(t *testing.T) {
	resetRBAC(t)
	sub, _ := seedMinter(t, "granters", "role.create")
	s := newService(t)

	err := s.CreateRole(context.Background(), CreateRoleInput{
		Name:        "double-fail",
		Permissions: []string{"incarnation.run"},
		CallerAID:   sub,
	})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld — the floor must be reported before the shape gate", err)
	}
	if errors.Is(err, ErrRootRoleNotPermitted) {
		t.Error("shape gate reported ahead of the least-privilege floor")
	}
}

// ============================================================
// UPDATE — the gate is on the resulting shape, not on the verb
// ============================================================

// Growing an already-plain role puts privilege into something that tracks
// nothing, which is the same act as creating it that way. Gating only creation
// would leave the whole policy one PATCH wide (ADR-078(h) records the same trap
// for the attenuation gate).
func TestIntegration_RootRole_PatchGrowingPlainRole_Denied(t *testing.T) {
	resetRBAC(t)
	sub, _ := seedMinter(t, "runners", "incarnation.run", "soul.list", "role.create")
	insertRole(t, "team", "soul.list")
	s := newService(t)

	err := s.UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name:        "team",
		Permissions: []string{"soul.list", "incarnation.run"},
		CallerAID:   sub,
	})
	if !errors.Is(err, ErrRootRoleNotPermitted) {
		t.Fatalf("err = %v, want ErrRootRoleNotPermitted (growing a plain role is minting)", err)
	}
	if got := rolePerms(t, "team"); len(got) != 1 || got[0] != "soul.list" {
		t.Errorf("permissions = %v, want the role left untouched at [soul.list]", got)
	}
}

// A pure trim stays ungated: `required` is empty, nothing is being stranded, and
// an operator must be able to take rights AWAY from someone else's role without
// holding them. Removing privilege is never the escalation this gate is about.
func TestIntegration_RootRole_PatchTrimmingPlainRole_Allowed(t *testing.T) {
	resetRBAC(t)
	sub, _ := seedMinter(t, "granters", "role.create")
	insertRole(t, "team", "soul.list", "incarnation.run")
	s := newService(t)

	if err := s.UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name:        "team",
		Permissions: []string{"soul.list"},
		CallerAID:   sub,
	}); err != nil {
		t.Fatalf("UpdateRolePermissions (pure trim, caller holds neither right): %v", err)
	}
	if got := rolePerms(t, "team"); len(got) != 1 || got[0] != "soul.list" {
		t.Errorf("permissions = %v, want [soul.list]", got)
	}
}

// Clearing parent_role AND adding a permission in the same PATCH: the result is
// plain and grew, so the gate fires. The companion case — clearing the parent
// while leaving the rows alone — is the section below.
func TestIntegration_RootRole_PatchClearingParentAndGrowing_Denied(t *testing.T) {
	resetRBAC(t)
	sub, _ := seedMinter(t, "runners", "incarnation.run", "soul.list", "role.create")
	insertRole(t, "base-runners", "incarnation.run", "soul.list")
	insertChildRole(t, "team-runners", "base-runners", "incarnation.run")
	s := newService(t)

	err := s.UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name:          "team-runners",
		Permissions:   []string{"incarnation.run", "soul.list"},
		SetParentRole: true,
		ParentRole:    nil,
		CallerAID:     sub,
	})
	if !errors.Is(err, ErrRootRoleNotPermitted) {
		t.Fatalf("err = %v, want ErrRootRoleNotPermitted (re-rooted and grown in one PATCH)", err)
	}
	if got := storedParent(t, "team-runners"); got == nil || *got != "base-runners" {
		t.Errorf("parent_role = %v after a rejected PATCH, want base-runners", got)
	}
}

// ============================================================
// UPDATE — clearing parent_role moves the ceiling on its own (NIM-230)
// ============================================================

// The escalation the ticket reproduced, and the reason both gates stopped reading
// row diffs. This PATCH changes NOTHING about the permission rows. It drops the
// parent, and with it the `coven=prod` ceiling every bare row was resolving under:
// effective rights go from `incarnation.run on coven=prod` to `incarnation.run`
// across the whole cluster, and everyone holding the role goes with them. The row
// diff that used to feed both gates reported ∅ and waved it through.
//
// The floor speaks first, as everywhere: this caller cannot grant unrestricted
// `incarnation.run` at all, which is the more actionable of the two refusals.
func TestIntegration_RootRole_PatchClearingParentAlone_Denied(t *testing.T) {
	resetRBAC(t)
	sub, _ := seedMinter(t, "watchers", "soul.list")
	insertRoleScoped(t, "base-prod", "coven=prod", "incarnation.run")
	insertChildRole(t, "team-child", "base-prod", "incarnation.run")
	s := newService(t)

	err := s.UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name:          "team-child",
		Permissions:   []string{"incarnation.run"}, // unchanged — the whole point
		SetParentRole: true,
		ParentRole:    nil,
		CallerAID:     sub,
	})
	if !errors.Is(err, ErrPermissionNotHeld) {
		t.Fatalf("err = %v, want ErrPermissionNotHeld (un-parenting lifts the coven=prod ceiling)", err)
	}
	if got := storedParent(t, "team-child"); got == nil || *got != "base-prod" {
		t.Errorf("parent_role = %v after a rejected PATCH, want base-prod", got)
	}
}

// The same PATCH by an operator who CAN grant the result: the floor has nothing
// to say, and what refuses is the shape gate. Un-parenting is minting — the
// rights stop tracking the role they came from, which is the entire subject of
// NIM-201 — so it costs the same right that creating the role plain would have.
func TestIntegration_RootRole_PatchClearingParentAlone_DeniedForHolder(t *testing.T) {
	resetRBAC(t)
	sub, _ := seedMinter(t, "runners", "incarnation.run", "role.create")
	insertRoleScoped(t, "base-prod", "coven=prod", "incarnation.run")
	insertChildRole(t, "team-child", "base-prod", "incarnation.run")
	s := newService(t)

	err := s.UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name:          "team-child",
		Permissions:   []string{"incarnation.run"},
		SetParentRole: true,
		ParentRole:    nil,
		CallerAID:     sub,
	})
	if !errors.Is(err, ErrRootRoleNotPermitted) {
		t.Fatalf("err = %v, want ErrRootRoleNotPermitted (un-parenting strands what the role grants)", err)
	}
	if got := storedParent(t, "team-child"); got == nil || *got != "base-prod" {
		t.Errorf("parent_role = %v after a rejected PATCH, want base-prod", got)
	}
}

// The child's own delta already says `coven=prod`, so dropping a parent whose
// ceiling said the same thing leaves the effective rights NUMERICALLY IDENTICAL.
// The floor is silent for exactly that reason — nothing new is being granted —
// and the shape gate still refuses, because what the PATCH removed is the
// tracking. Narrow `base-prod` tomorrow and a derived child would have followed it
// down (ADR-078(c)/(d)); the plain role this would leave behind never will.
//
// This is why the gate reads the whole resulting set when a role LOSES its parent
// and only the growth when it was already plain: measuring the widening alone
// would call this PATCH a no-op.
func TestIntegration_RootRole_PatchClearingParentKeepingRights_Denied(t *testing.T) {
	resetRBAC(t)
	sub, _ := seedMinter(t, "prod-runners", "incarnation.run on coven=prod", "role.create")
	insertRoleScoped(t, "base-prod", "coven=prod", "incarnation.run")
	insertDerived(t, "team-child", "base-prod", "coven=prod", "incarnation.run")
	s := newService(t)

	err := s.UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name:          "team-child",
		Permissions:   []string{"incarnation.run"},
		SetParentRole: true,
		ParentRole:    nil,
		CallerAID:     sub,
	})
	if !errors.Is(err, ErrRootRoleNotPermitted) {
		t.Fatalf("err = %v, want ErrRootRoleNotPermitted (same rights, no longer tracking)", err)
	}
	if got := storedParent(t, "team-child"); got == nil || *got != "base-prod" {
		t.Errorf("parent_role = %v after a rejected PATCH, want base-prod", got)
	}
}

// Un-parenting is not forbidden, it is priced: the same PATCH goes through for a
// caller holding both what the result grants and `role.create-root`. Pinned so a
// later tightening cannot quietly turn a gated operation into an impossible one.
func TestIntegration_RootRole_PatchClearingParentWithRight_Allowed(t *testing.T) {
	resetRBAC(t)
	sub, _ := seedMinter(t, "runners", "incarnation.run", "role.create", "role.create-root")
	insertRoleScoped(t, "base-prod", "coven=prod", "incarnation.run")
	insertChildRole(t, "team-child", "base-prod", "incarnation.run")
	s := newService(t)

	if err := s.UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name:          "team-child",
		Permissions:   []string{"incarnation.run"},
		SetParentRole: true,
		ParentRole:    nil,
		CallerAID:     sub,
	}); err != nil {
		t.Fatalf("UpdateRolePermissions (caller holds the right and role.create-root): %v", err)
	}
	if got := storedParent(t, "team-child"); got != nil {
		t.Errorf("parent_role = %v, want NULL (the PATCH was accepted)", *got)
	}
}

// insertChildRole inserts a derived role fixture with its own permission rows —
// insertDerivedRole (derived_integration_test.go) covers the schema guard and
// carries no permissions.
func insertChildRole(t *testing.T, name, parent string, perms ...string) {
	t.Helper()
	ctx := context.Background()
	if _, err := integrationPool.Exec(ctx,
		`INSERT INTO rbac_roles (name, builtin, parent_role, scope_mode) VALUES ($1, false, $2, 'track')`,
		name, parent); err != nil {
		t.Fatalf("insert derived role %q: %v", name, err)
	}
	for _, p := range perms {
		if _, err := integrationPool.Exec(ctx,
			`INSERT INTO rbac_role_permissions (role_name, permission) VALUES ($1, $2)`, name, p); err != nil {
			t.Fatalf("insert perm %q for %q: %v", p, name, err)
		}
	}
}

// storedParent reads a role's parent_role for assertions (the production
// roleParent takes a db handle and is used inside the mutation's tx).
func storedParent(t *testing.T, name string) *string {
	t.Helper()
	var parent *string
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT parent_role FROM rbac_roles WHERE name = $1`, name).Scan(&parent); err != nil {
		t.Fatalf("read parent_role of %q: %v", name, err)
	}
	return parent
}
