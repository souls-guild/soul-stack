package handlers

import (
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
)

func mustEnforcerFromSnapshot(t *testing.T, snap *rbac.Snapshot) *rbac.Enforcer {
	t.Helper()
	e, err := rbac.NewEnforcerFromSnapshot(snap)
	if err != nil {
		t.Fatalf("rbac.NewEnforcerFromSnapshot: %v", err)
	}
	return e
}

func findMyPermission(items []MyPermission, resource, action string) (MyPermission, bool) {
	for _, p := range items {
		if p.Resource == resource && p.Action == action {
			return p, true
		}
	}
	return MyPermission{}, false
}

func TestMyPermissions_Self_OK(t *testing.T) {
	e := rbactest.MustEnforcer(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "ops", Operators: []string{"archon-alice"}, Permissions: []string{"incarnation.run", "soul.list"}},
	}})
	h := NewMyPermissionsHandler(e, nil)

	resp := h.GetTyped("archon-alice")
	if len(resp.Permissions) != 2 {
		t.Fatalf("expected 2 permissions, got %d: %+v", len(resp.Permissions), resp.Permissions)
	}
	if _, ok := findMyPermission(resp.Permissions, "incarnation", "run"); !ok {
		t.Errorf("incarnation.run missing: %+v", resp.Permissions)
	}
	if _, ok := findMyPermission(resp.Permissions, "soul", "list"); !ok {
		t.Errorf("soul.list missing: %+v", resp.Permissions)
	}
}

func TestMyPermissions_Wildcard(t *testing.T) {
	e := rbactest.MustEnforcer(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "cluster-admin", Operators: []string{"archon-root"}, Permissions: []string{"*"}},
	}})
	h := NewMyPermissionsHandler(e, nil)

	resp := h.GetTyped("archon-root")
	if len(resp.Permissions) != 1 || !resp.Permissions[0].Wildcard {
		t.Fatalf("cluster-admin: expected a single wildcard marker, got %+v", resp.Permissions)
	}
}

// TestMyPermissions_HostScope — guard for a boolean scope predicate (NIM-128):
// a permission with a per-perm `on host matches <glob>` selector must carry the
// canonical predicate string down to the domain shape under Exprs (the native
// projection in api puts it under snake_case `exprs`; wire byte-exact is checked
// by golden huma_catalog_reply_test.go).
func TestMyPermissions_HostScope(t *testing.T) {
	e := rbactest.MustEnforcer(t, &rbactest.Config{Roles: []rbactest.Role{
		{
			Name:        "host-scoped",
			Operators:   []string{"archon-host"},
			Permissions: []string{`incarnation.run on host matches web-*`},
		},
	}})
	h := NewMyPermissionsHandler(e, nil)

	resp := h.GetTyped("archon-host")
	p, ok := findMyPermission(resp.Permissions, "incarnation", "run")
	if !ok || p.Scope == nil {
		t.Fatalf("incarnation.run without scope: %+v", resp.Permissions)
	}
	if len(p.Scope.Exprs) != 1 || p.Scope.Exprs[0] != "host matches web-*" {
		t.Errorf("scope.Exprs = %v, expected a single host-glob predicate", p.Scope.Exprs)
	}
	if p.Scope.Unrestricted {
		t.Errorf("scope with a host selector must not be unrestricted: %+v", p.Scope)
	}
}

func TestMyPermissions_ScopeIncluded(t *testing.T) {
	// default_scope=coven=prod on the role → bare incarnation.run inherits scope:
	// exprs=[coven=prod], unrestricted=false.
	snap := rbactest.Snapshot(&rbactest.Config{Roles: []rbactest.Role{
		{Name: "prod-runner", Operators: []string{"archon-prod"}, Permissions: []string{"incarnation.run"}},
	}})
	snap.RoleScopes = map[string]string{"prod-runner": "coven=prod"}
	e := mustEnforcerFromSnapshot(t, snap)
	h := NewMyPermissionsHandler(e, nil)

	resp := h.GetTyped("archon-prod")
	p, ok := findMyPermission(resp.Permissions, "incarnation", "run")
	if !ok {
		t.Fatalf("incarnation.run missing: %+v", resp.Permissions)
	}
	if p.Scope == nil {
		t.Fatalf("scope not assembled, expected exprs=[coven=prod]: %+v", p)
	}
	if p.Scope.Unrestricted {
		t.Errorf("scope with coven=prod must not be unrestricted: %+v", p.Scope)
	}
	if len(p.Scope.Exprs) != 1 || p.Scope.Exprs[0] != "coven=prod" {
		t.Errorf("Exprs = %v, expected [coven=prod]", p.Scope.Exprs)
	}
}
