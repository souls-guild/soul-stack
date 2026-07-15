package rbac

import "testing"

// TestCatalog_AugurPermissions — the 6 omen.*/rite.* Augur permissions
// (ADR-025, rbac.md §Augur) are present in the catalog as 2-segment
// <resource>.<action>.
func TestCatalog_AugurPermissions(t *testing.T) {
	cases := []struct{ resource, action string }{
		{"omen", "create"}, {"omen", "list"}, {"omen", "delete"},
		{"rite", "create"}, {"rite", "list"}, {"rite", "delete"},
	}
	for _, c := range cases {
		if !IsAllowedPermission(c.resource, c.action) {
			t.Errorf("%s.%s missing from catalog", c.resource, c.action)
		}
	}
}

// TestCatalog_AugurWildcard — omen.* / rite.* are valid (cluster-admin `*`
// covers them; the wildcard expands to the resource's known actions).
func TestCatalog_AugurWildcard(t *testing.T) {
	if !IsAllowedPermission("omen", "*") {
		t.Error("omen.* should be valid (catalog has omen.create etc)")
	}
	if !IsAllowedPermission("rite", "*") {
		t.Error("rite.* should be valid (catalog has rite.create etc)")
	}
}
