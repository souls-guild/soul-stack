// GOLDEN byte-exact wire-guard для NATIVE wire-DTO SYNOD-домена (handler-native T5d).
// synod больше НЕ зависит от legacy-генерата (0 legacy-генерата в synod-файлах), поэтому golden сверяет
// json native-значения с ЗАФИКСИРОВАННОЙ строкой-эталоном. Единственный reply-роут с
// телом — GET /v1/synods (SynodListReply.Items []SynodView); create/update/delete/
// add/remove-operator/grant/revoke-role — 201/204 БЕЗ тела. Покрыты обе ветки
// description (nil/non-nil) + []-vs-null для operators/roles.
package api

import (
	"encoding/json"
	"testing"
)

// goldenSynodWire сверяет json.Marshal(native) байт-в-байт с зафиксированным эталоном.
func goldenSynodWire(t *testing.T, name string, native any, want string) {
	t.Helper()
	got, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("%s: marshal native: %v", name, err)
	}
	if string(got) != want {
		t.Errorf("%s: WIRE DRIFT\n got  = %s\n want = %s", name, got, want)
	}
}

func TestGoldenWire_SynodReply(t *testing.T) {
	desc := "группа ops"

	// --- SynodView: description omitempty (обе ветки) + []-vs-null ---
	goldenSynodWire(t, "SynodView/full",
		SynodView{Builtin: true, Description: &desc, Name: "ops-group", Operators: []string{"archon-alice"}, Roles: []string{"cluster-admin", "viewer"}},
		`{"builtin":true,"description":"группа ops","name":"ops-group","operators":["archon-alice"],"roles":["cluster-admin","viewer"]}`)
	// description опущен (nil); operators/roles пустые массивы (non-nil) → `[]`.
	goldenSynodWire(t, "SynodView/nil_desc_empty_lists",
		SynodView{Builtin: false, Description: nil, Name: "empty-group", Operators: []string{}, Roles: []string{}},
		`{"builtin":false,"name":"empty-group","operators":[],"roles":[]}`)
	// nil-слайс ветка (на wire `null`).
	goldenSynodWire(t, "SynodView/nil_lists",
		SynodView{Builtin: false, Name: "x", Operators: nil, Roles: nil},
		`{"builtin":false,"name":"x","operators":null,"roles":null}`)

	// --- SynodListReply: items наполнен / пустой / nil ---
	sv := SynodView{Builtin: true, Name: "ops", Operators: []string{}, Roles: []string{}}
	goldenSynodWire(t, "SynodListReply/items",
		SynodListReply{Items: []SynodView{sv}},
		`{"items":[{"builtin":true,"name":"ops","operators":[],"roles":[]}]}`)
	goldenSynodWire(t, "SynodListReply/empty",
		SynodListReply{Items: []SynodView{}},
		`{"items":[]}`)
	goldenSynodWire(t, "SynodListReply/nil",
		SynodListReply{Items: nil},
		`{"items":null}`)
}
