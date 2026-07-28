//go:build integration

// The API catalog of derived roles against a real DB (ADR-078, NIM-181): a chain
// built through the SERVICE — the same calls the REST/MCP surface makes — and read
// back through [Service.ListRoles].
//
// Unit coverage of the resolution itself is role_catalog_test.go; what needs a DB is
// the round trip: that a derived role written through the write gate comes back with
// the effective form the enforcer would compute for it, across a chain deeper than
// one hop.
//
// Run:
//
//	cd keeper && SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1 go test -tags=integration -race -count=1 ./internal/rbac/
package rbac

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// viewOf returns one role from the catalog by name.
func viewOf(t *testing.T, views []RoleView, name string) RoleView {
	t.Helper()
	for _, v := range views {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("role %q missing from the catalog", name)
	return RoleView{}
}

// TestIntegration_Catalog_ResolvesAChainOfThree — the J3 acceptance case end to end:
// three roles created through the service, each deriving from the previous, read
// back with their effective rights resolved.
//
// The two forms must DIFFER on the leaf and differ in the safe direction: its stored
// scope is the delta alone (so a later PATCH does not freeze the cascade by restating
// the parent's predicate), while its effective scope carries every hop.
func TestIntegration_Catalog_ResolvesAChainOfThree(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-root", nil)
	seedClusterAdmin(t, "archon-root")

	svc := newService(t)
	ctx := context.Background()

	scope := func(s string) *string { return &s }
	if err := svc.CreateRole(ctx, CreateRoleInput{
		Name: "root", Permissions: []string{"incarnation.get", "incarnation.destroy"},
		DefaultScope: scope("coven=dba"), CallerAID: "archon-root",
	}); err != nil {
		t.Fatalf("CreateRole(root): %v", err)
	}
	if err := svc.CreateRole(ctx, CreateRoleInput{
		Name: "mid", Permissions: []string{"incarnation.get", "incarnation.destroy"},
		DefaultScope: scope("service=redis"), ParentRole: scope("root"), CallerAID: "archon-root",
	}); err != nil {
		t.Fatalf("CreateRole(mid): %v", err)
	}
	if err := svc.CreateRole(ctx, CreateRoleInput{
		Name: "leaf", Permissions: []string{"incarnation.get"},
		DefaultScope: scope("trait.project=aboba"), ParentRole: scope("mid"), CallerAID: "archon-root",
	}); err != nil {
		t.Fatalf("CreateRole(leaf): %v", err)
	}

	views, err := svc.ListRoles(ctx, "archon-root")
	if err != nil {
		t.Fatalf("ListRoles: %v", err)
	}

	leaf := viewOf(t, views, "leaf")
	if leaf.ParentRole != "mid" {
		t.Errorf("leaf parent_role = %q, want mid", leaf.ParentRole)
	}
	if leaf.DefaultScope != "trait.project=aboba" {
		t.Errorf("leaf stored default_scope = %q, want the delta alone", leaf.DefaultScope)
	}
	if want := "coven=dba AND service=redis AND trait.project=aboba"; leaf.EffectiveScope != want {
		t.Errorf("leaf effective scope = %q, want %q", leaf.EffectiveScope, want)
	}
	if got := strings.Join(leaf.EffectivePermissions, ", "); got != "incarnation.get" {
		t.Errorf("leaf effective permissions = %q, want incarnation.get", got)
	}

	// The middle hop resolves too — a catalog that only resolved leaves would show
	// `mid` at service=redis alone, i.e. WIDER than it is.
	mid := viewOf(t, views, "mid")
	if want := "coven=dba AND service=redis"; mid.EffectiveScope != want {
		t.Errorf("mid effective scope = %q, want %q", mid.EffectiveScope, want)
	}

	// A plain role reads the same on both sides, so a consumer can use the
	// effective fields unconditionally.
	root := viewOf(t, views, "root")
	if root.ParentRole != "" || root.EffectiveScope != root.DefaultScope {
		t.Errorf("plain role root: parent=%q effective=%q stored=%q — want no parent and identical scopes",
			root.ParentRole, root.EffectiveScope, root.DefaultScope)
	}
}

// TestIntegration_Catalog_RejectedRowStaysStoredButGrantsNothing — the write gate
// refuses a role that exceeds its parent, so the only way a stored row ends up
// outside the ceiling is the parent narrowing AFTERWARDS. The catalog must then show
// the row as stored and absent from the effective set: that difference is the cascade
// working, and hiding it would leave an operator unable to see why a role stopped
// granting.
func TestIntegration_Catalog_RejectedRowStaysStoredButGrantsNothing(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-root", nil)
	seedClusterAdmin(t, "archon-root")

	svc := newService(t)
	ctx := context.Background()
	scope := func(s string) *string { return &s }

	if err := svc.CreateRole(ctx, CreateRoleInput{
		Name: "dba", Permissions: []string{"incarnation.get", "incarnation.destroy"},
		DefaultScope: scope("coven=dba"), CallerAID: "archon-root",
	}); err != nil {
		t.Fatalf("CreateRole(dba): %v", err)
	}
	if err := svc.CreateRole(ctx, CreateRoleInput{
		Name: "dba-aboba", Permissions: []string{"incarnation.get", "incarnation.destroy"},
		DefaultScope: scope("trait.project=aboba"), ParentRole: scope("dba"), CallerAID: "archon-root",
	}); err != nil {
		t.Fatalf("CreateRole(dba-aboba): %v", err)
	}

	// The gate refuses the same set the other way round: asking for a right the
	// parent does not hold is a refusal, not a stored row.
	err := svc.CreateRole(ctx, CreateRoleInput{
		Name: "dba-over", Permissions: []string{"incarnation.get", "operator.create"},
		ParentRole: scope("dba"), CallerAID: "archon-root",
	})
	if !errors.Is(err, ErrRoleExceedsParent) {
		t.Fatalf("CreateRole(beyond the parent) = %v, want ErrRoleExceedsParent", err)
	}

	// Now narrow the parent: the child's incarnation.destroy row survives in
	// storage and drops out of its effective set at the next read. Confirmed
	// explicitly — narrowing a parent moves the roles below it, and that is
	// exactly what this case is arranging (ADR-078(k)).
	if err := svc.UpdateRolePermissions(ctx, UpdateRolePermissionsInput{
		Name: "dba", Permissions: []string{"incarnation.get"}, CallerAID: "archon-root",
		ConfirmCascade: true,
	}); err != nil {
		t.Fatalf("UpdateRolePermissions(dba): %v", err)
	}

	views, err := svc.ListRoles(ctx, "archon-root")
	if err != nil {
		t.Fatalf("ListRoles: %v", err)
	}
	child := viewOf(t, views, "dba-aboba")
	if len(child.Permissions) != 2 {
		t.Errorf("stored permissions = %v, want both rows as written", child.Permissions)
	}
	if got := strings.Join(child.EffectivePermissions, ", "); got != "incarnation.get" {
		t.Errorf("effective permissions = %q, want incarnation.get — the parent no longer holds incarnation.destroy", got)
	}
}
