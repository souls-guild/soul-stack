package rbac

import "testing"

// TestCatalog_AugurPermissions — 6 omen.*/rite.*-permissions Augur (ADR-025,
// rbac.md §Augur) присутствуют в каталоге как 2-сегментные <resource>.<action>.
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

// TestCatalog_AugurWildcard — omen.* / rite.* валидны (cluster-admin `*`
// покрывает; wildcard разворачивается на известные action-ы resource-а).
func TestCatalog_AugurWildcard(t *testing.T) {
	if !IsAllowedPermission("omen", "*") {
		t.Error("omen.* should be valid (catalog has omen.create etc)")
	}
	if !IsAllowedPermission("rite", "*") {
		t.Error("rite.* should be valid (catalog has rite.create etc)")
	}
}
