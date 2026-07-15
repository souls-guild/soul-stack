package rbac

import "testing"

// TestCatalog_IncarnationTraitsSet — the incarnation.traits-set permission
// (Trait relocation per-soul → per-incarnation, ADR-060 amend R1) is present
// in the catalog as a 2-segment <resource>.<action> and is covered by the
// incarnation.* wildcard (cluster-admin `*`). The per-soul
// soul.traits-assign stays in the catalog (forward-compat, deprecated at
// the API layer — not in RBAC).
func TestCatalog_IncarnationTraitsSet(t *testing.T) {
	if !IsAllowedPermission("incarnation", "traits-set") {
		t.Error("incarnation.traits-set missing from catalog")
	}
	if !IsAllowedPermission("incarnation", "*") {
		t.Error("incarnation.* should cover traits-set")
	}
	// The deprecated per-soul permission is NOT removed (forward-compat, closed enum).
	if !IsAllowedPermission("soul", "traits-assign") {
		t.Error("soul.traits-assign removed — deprecate ≠ remove (роли в keeper.yml сломаются)")
	}
}
