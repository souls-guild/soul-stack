package handlers

import (
	"sort"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

func findResource(items []permissionResource, name string) (permissionResource, bool) {
	for _, it := range items {
		if it.Resource == name {
			return it, true
		}
	}
	return permissionResource{}, false
}

func hasAction(actions []permissionAction, name string) bool {
	for _, a := range actions {
		if a.Action == name {
			return true
		}
	}
	return false
}

func TestPermissionCatalog_List(t *testing.T) {
	h := NewPermissionCatalogHandler(nil)
	resp := h.ListTyped()

	if len(resp.Items) == 0 {
		t.Fatal("permissions catalog is empty")
	}

	// Sum of actions across all resources == size of AllowedPermissions (we lost no
	// permission and added none extra).
	total := 0
	for _, it := range resp.Items {
		total += len(it.Actions)
	}
	if total != len(rbac.AllowedPermissions) {
		t.Errorf("sum actions=%d, in catalog rbac.AllowedPermissions=%d", total, len(rbac.AllowedPermissions))
	}

	// Deterministic order: resources are sorted, actions within them too.
	for i := 1; i < len(resp.Items); i++ {
		if resp.Items[i-1].Resource >= resp.Items[i].Resource {
			t.Fatalf("resources are not sorted or have a duplicate: %q >= %q", resp.Items[i-1].Resource, resp.Items[i].Resource)
		}
	}
	for _, it := range resp.Items {
		for i := 1; i < len(it.Actions); i++ {
			if it.Actions[i-1].Action >= it.Actions[i].Action {
				t.Errorf("actions for resource %q are not sorted or have a duplicate: %q >= %q",
					it.Resource, it.Actions[i-1].Action, it.Actions[i].Action)
			}
		}
	}
}

func TestPermissionCatalog_KnownPermissionsPresent(t *testing.T) {
	resp := PermissionCatalog{Items: buildPermissionCatalog()}

	// Real names from rbac.catalog.go (NOT the mythical soul.read). Reading a soul is
	// soul.list (one read permission covers list+get+soulprint, router.go).
	cases := []struct{ resource, action string }{
		{"soul", "list"},
		{"soul", "create"},
		{"incarnation", "run"},
		{"role", "update"},
		{"operator", "create"},
		{"audit", "read"},
	}
	for _, c := range cases {
		res, ok := findResource(resp.Items, c.resource)
		if !ok {
			t.Errorf("resource %q is missing from the catalog", c.resource)
			continue
		}
		if !hasAction(res.Actions, c.action) {
			t.Errorf("%s.%s is missing from the catalog", c.resource, c.action)
		}
	}

	// soul.read is a mythical name (UI hardcode bug); it MUST NOT be in the
	// catalog (this test pins the root cause of the bug).
	if soul, ok := findResource(resp.Items, "soul"); ok && hasAction(soul.Actions, "read") {
		t.Error("soul.read must not be present (not in rbac.catalog.go - source of unknown_permission)")
	}
}

func TestPermissionCatalog_SelectorKeysCommon(t *testing.T) {
	resp := PermissionCatalog{Items: buildPermissionCatalog()}

	want := rbac.SelectorKeys()
	if len(want) == 0 {
		t.Fatal("rbac.SelectorKeys() is empty - test cannot verify")
	}

	// selector_keys is the SAME common list for every action (there is no
	// per-permission metadata in the MVP catalog).
	for _, res := range resp.Items {
		for _, a := range res.Actions {
			got := append([]string(nil), a.SelectorKeys...)
			sort.Strings(got)
			if len(got) != len(want) {
				t.Fatalf("%s.%s selector_keys=%v, expected common %v", res.Resource, a.Action, got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("%s.%s selector_keys=%v, expected common %v", res.Resource, a.Action, got, want)
				}
			}
		}
	}
}
