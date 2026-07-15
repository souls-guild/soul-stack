package rbac

import "testing"

// TestCatalog_OraclePermissions — the 6 vigil.*/decree.* Oracle permissions
// (ADR-030 beacons, rbac.md §Oracle) are present in the catalog as
// 2-segment <resource>.<action>.
func TestCatalog_OraclePermissions(t *testing.T) {
	cases := []struct{ resource, action string }{
		{"vigil", "create"}, {"vigil", "list"}, {"vigil", "delete"},
		{"decree", "create"}, {"decree", "list"}, {"decree", "delete"},
	}
	for _, c := range cases {
		if !IsAllowedPermission(c.resource, c.action) {
			t.Errorf("%s.%s missing from catalog", c.resource, c.action)
		}
	}
}

// TestCatalog_OracleWildcard — vigil.* / decree.* are valid (cluster-admin
// `*` covers them; the wildcard expands to the resource's known actions).
func TestCatalog_OracleWildcard(t *testing.T) {
	if !IsAllowedPermission("vigil", "*") {
		t.Error("vigil.* should be valid (catalog has vigil.create etc)")
	}
	if !IsAllowedPermission("decree", "*") {
		t.Error("decree.* should be valid (catalog has decree.create etc)")
	}
}
