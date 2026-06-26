package rbac

import "testing"

// TestCatalog_IncarnationTraitsSet — permission incarnation.traits-set (релокация
// Trait per-soul → per-incarnation, ADR-060 amend R1) присутствует в каталоге как
// 2-сегментная <resource>.<action> и покрывается incarnation.* wildcard-ом
// (cluster-admin `*`). per-soul soul.traits-assign остаётся в каталоге
// (forward-compat, deprecated на API-слое — не в RBAC).
func TestCatalog_IncarnationTraitsSet(t *testing.T) {
	if !IsAllowedPermission("incarnation", "traits-set") {
		t.Error("incarnation.traits-set missing from catalog")
	}
	if !IsAllowedPermission("incarnation", "*") {
		t.Error("incarnation.* should cover traits-set")
	}
	// Deprecated per-soul permission НЕ удалён (forward-compat, closed enum).
	if !IsAllowedPermission("soul", "traits-assign") {
		t.Error("soul.traits-assign removed — deprecate ≠ remove (роли в keeper.yml сломаются)")
	}
}
