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
		t.Fatalf("ожидали 2 права, получили %d: %+v", len(resp.Permissions), resp.Permissions)
	}
	if _, ok := findMyPermission(resp.Permissions, "incarnation", "run"); !ok {
		t.Errorf("incarnation.run отсутствует: %+v", resp.Permissions)
	}
	if _, ok := findMyPermission(resp.Permissions, "soul", "list"); !ok {
		t.Errorf("soul.list отсутствует: %+v", resp.Permissions)
	}
}

func TestMyPermissions_Wildcard(t *testing.T) {
	e := rbactest.MustEnforcer(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "cluster-admin", Operators: []string{"archon-root"}, Permissions: []string{"*"}},
	}})
	h := NewMyPermissionsHandler(e, nil)

	resp := h.GetTyped("archon-root")
	if len(resp.Permissions) != 1 || !resp.Permissions[0].Wildcard {
		t.Fatalf("cluster-admin: ожидали один wildcard-маркер, получили %+v", resp.Permissions)
	}
}

// TestMyPermissions_StateScope — guard for the state dimension of scope (ADR-047 S2c):
// a permission with a per-perm `state='...'` selector must carry the predicate down to
// the domain shape under the State field (the native projection in api puts it under
// snake_case `state`; wire byte-exact is checked by golden huma_catalog_reply_test.go).
func TestMyPermissions_StateScope(t *testing.T) {
	e := rbactest.MustEnforcer(t, &rbactest.Config{Roles: []rbactest.Role{
		{
			Name:        "state-scoped",
			Operators:   []string{"archon-state"},
			Permissions: []string{`incarnation.run on state='state.redis_version == "8.0"'`},
		},
	}})
	h := NewMyPermissionsHandler(e, nil)

	resp := h.GetTyped("archon-state")
	p, ok := findMyPermission(resp.Permissions, "incarnation", "run")
	if !ok || p.Scope == nil {
		t.Fatalf("incarnation.run без scope: %+v", resp.Permissions)
	}
	if len(p.Scope.State) != 1 || p.Scope.State[0] != `state.redis_version == "8.0"` {
		t.Errorf("scope.State = %v, ожидали один state-предикат", p.Scope.State)
	}
	if p.Scope.Unrestricted {
		t.Errorf("scope с state-селектором не должен быть unrestricted: %+v", p.Scope)
	}
}

func TestMyPermissions_ScopeIncluded(t *testing.T) {
	// default_scope=coven=prod on the role → bare incarnation.run inherits scope:
	// covens=[prod], unrestricted=false.
	snap := rbactest.Snapshot(&rbactest.Config{Roles: []rbactest.Role{
		{Name: "prod-runner", Operators: []string{"archon-prod"}, Permissions: []string{"incarnation.run"}},
	}})
	snap.RoleScopes = map[string]string{"prod-runner": "coven=prod"}
	e := mustEnforcerFromSnapshot(t, snap)
	h := NewMyPermissionsHandler(e, nil)

	resp := h.GetTyped("archon-prod")
	p, ok := findMyPermission(resp.Permissions, "incarnation", "run")
	if !ok {
		t.Fatalf("incarnation.run отсутствует: %+v", resp.Permissions)
	}
	if p.Scope == nil {
		t.Fatalf("scope не собран, ожидали covens=[prod]: %+v", p)
	}
	if p.Scope.Unrestricted {
		t.Errorf("scope с coven=prod не должен быть unrestricted: %+v", p.Scope)
	}
	if len(p.Scope.Covens) != 1 || p.Scope.Covens[0] != "prod" {
		t.Errorf("Covens = %v, ожидали [prod]", p.Scope.Covens)
	}
}
