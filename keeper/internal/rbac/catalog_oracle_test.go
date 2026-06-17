package rbac

import "testing"

// TestCatalog_OraclePermissions — 6 vigil.*/decree.*-permissions Oracle (ADR-030
// beacons, rbac.md §Oracle) присутствуют в каталоге как 2-сегментные
// <resource>.<action>.
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

// TestCatalog_OracleWildcard — vigil.* / decree.* валидны (cluster-admin `*`
// покрывает; wildcard разворачивается на известные action-ы resource-а).
func TestCatalog_OracleWildcard(t *testing.T) {
	if !IsAllowedPermission("vigil", "*") {
		t.Error("vigil.* should be valid (catalog has vigil.create etc)")
	}
	if !IsAllowedPermission("decree", "*") {
		t.Error("decree.* should be valid (catalog has decree.create etc)")
	}
}
