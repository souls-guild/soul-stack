// GOLDEN byte-exact wire-guard для NATIVE wire-DTO ROLE-домена (handler-native T5d).
// role больше НЕ зависит от legacy-генерата (0 legacy-генерата в role-файлах), поэтому golden сверяет
// json native-значения с ЗАФИКСИРОВАННОЙ строкой-эталоном (а не с legacy-генерата-значением).
// Это пиннит точные wire-байты: мутация формы (убрать omitempty / сменить json-тег /
// тип поля / порядок) краснит соответствующий case. Единственный reply-роут с телом —
// GET /v1/roles (RoleListReply.Items []RoleView); create/delete/update-permissions/
// grant/revoke — 201/204 БЕЗ тела (golden им не нужен). Покрыты обе указательных
// ветки (nil/non-nil) для default_scope/description + []-vs-null для operators/permissions.
package api

import (
	"encoding/json"
	"testing"
)

// goldenRoleWire сверяет json.Marshal(native) байт-в-байт с зафиксированным эталоном.
func goldenRoleWire(t *testing.T, name string, native any, want string) {
	t.Helper()
	got, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("%s: marshal native: %v", name, err)
	}
	if string(got) != want {
		t.Errorf("%s: WIRE DRIFT\n got  = %s\n want = %s", name, got, want)
	}
}

func TestGoldenWire_RoleReply(t *testing.T) {
	scope := "coven=prod,eu"
	desc := "ops роль"

	// --- RoleView: default_scope/description omitempty (обе ветки) + []-vs-null ---
	goldenRoleWire(t, "RoleView/full",
		RoleView{Builtin: true, DefaultScope: &scope, Description: &desc, Name: "ops", Operators: []string{"archon-alice"}, Permissions: []string{"role.list", "service.list"}},
		`{"builtin":true,"default_scope":"coven=prod,eu","description":"ops роль","name":"ops","operators":["archon-alice"],"permissions":["role.list","service.list"]}`)
	// default_scope опущен (nil), description присутствует пустой; operators/permissions
	// пустые массивы (non-nil) → `[]`.
	goldenRoleWire(t, "RoleView/nil_scope_empty_lists",
		RoleView{Builtin: false, DefaultScope: nil, Description: nil, Name: "viewer", Operators: []string{}, Permissions: []string{}},
		`{"builtin":false,"name":"viewer","operators":[],"permissions":[]}`)
	// nil-слайс ветка (на wire `null`).
	goldenRoleWire(t, "RoleView/nil_lists",
		RoleView{Builtin: false, Name: "x", Operators: nil, Permissions: nil},
		`{"builtin":false,"name":"x","operators":null,"permissions":null}`)

	// --- RoleListReply: items наполнен / пустой / nil ---
	rv := RoleView{Builtin: true, Name: "ops", Operators: []string{}, Permissions: []string{}}
	goldenRoleWire(t, "RoleListReply/items",
		RoleListReply{Items: []RoleView{rv}},
		`{"items":[{"builtin":true,"name":"ops","operators":[],"permissions":[]}]}`)
	goldenRoleWire(t, "RoleListReply/empty",
		RoleListReply{Items: []RoleView{}},
		`{"items":[]}`)
	goldenRoleWire(t, "RoleListReply/nil",
		RoleListReply{Items: nil},
		`{"items":null}`)
}
