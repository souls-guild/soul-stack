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
		t.Fatal("каталог permissions пуст")
	}

	// Сумма actions по всем resource == размеру AllowedPermissions (ни одного
	// permission не потеряли, ни одного лишнего не добавили).
	total := 0
	for _, it := range resp.Items {
		total += len(it.Actions)
	}
	if total != len(rbac.AllowedPermissions) {
		t.Errorf("сумма actions=%d, в каталоге rbac.AllowedPermissions=%d", total, len(rbac.AllowedPermissions))
	}

	// Детерминированный порядок: resource отсортированы, внутри — actions.
	for i := 1; i < len(resp.Items); i++ {
		if resp.Items[i-1].Resource >= resp.Items[i].Resource {
			t.Fatalf("resource не отсортированы или дубль: %q >= %q", resp.Items[i-1].Resource, resp.Items[i].Resource)
		}
	}
	for _, it := range resp.Items {
		for i := 1; i < len(it.Actions); i++ {
			if it.Actions[i-1].Action >= it.Actions[i].Action {
				t.Errorf("actions resource %q не отсортированы или дубль: %q >= %q",
					it.Resource, it.Actions[i-1].Action, it.Actions[i].Action)
			}
		}
	}
}

func TestPermissionCatalog_KnownPermissionsPresent(t *testing.T) {
	resp := PermissionCatalog{Items: buildPermissionCatalog()}

	// Реальные имена из rbac.catalog.go (НЕ мифический soul.read). Чтение soul —
	// это soul.list (одно read-permission покрывает list+get+soulprint, router.go).
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
			t.Errorf("resource %q отсутствует в каталоге", c.resource)
			continue
		}
		if !hasAction(res.Actions, c.action) {
			t.Errorf("%s.%s отсутствует в каталоге", c.resource, c.action)
		}
	}

	// soul.read — мифическое имя (баг UI-хардкода); его в каталоге БЫТЬ не
	// должно (фиксируем причину бага в тесте).
	if soul, ok := findResource(resp.Items, "soul"); ok && hasAction(soul.Actions, "read") {
		t.Error("soul.read не должен присутствовать (нет в rbac.catalog.go — источник unknown_permission)")
	}
}

func TestPermissionCatalog_SelectorKeysCommon(t *testing.T) {
	resp := PermissionCatalog{Items: buildPermissionCatalog()}

	want := rbac.SelectorKeys()
	if len(want) == 0 {
		t.Fatal("rbac.SelectorKeys() пуст — тест не сможет проверить")
	}

	// selector_keys — ОДИН И ТОТ ЖЕ общий список для каждого action (per-
	// permission-метаданных в каталоге MVP нет).
	for _, res := range resp.Items {
		for _, a := range res.Actions {
			got := append([]string(nil), a.SelectorKeys...)
			sort.Strings(got)
			if len(got) != len(want) {
				t.Fatalf("%s.%s selector_keys=%v, ожидали общий %v", res.Resource, a.Action, got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("%s.%s selector_keys=%v, ожидали общий %v", res.Resource, a.Action, got, want)
				}
			}
		}
	}
}
