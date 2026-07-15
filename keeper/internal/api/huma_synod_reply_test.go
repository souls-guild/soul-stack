// GOLDEN byte-exact wire-guard for the NATIVE wire-DTO SYNOD domain (handler-native T5d).
// synod no longer depends on the legacy generator (0 legacy generator in synod files), so golden
// compares json native values against a pinned reference string. The only reply route with a
// body is GET /v1/synods (SynodListReply.Items []SynodView); create/update/delete/
// add/remove-operator/grant/revoke-role — 201/204 with no body. Both description
// branches (nil/non-nil) + []-vs-null for operators/roles are covered.
package api

import (
	"encoding/json"
	"testing"
)

// goldenSynodWire compares json.Marshal(native) byte-for-byte against a pinned reference.
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

	// --- SynodView: description omitempty (both branches) + []-vs-null ---
	goldenSynodWire(t, "SynodView/full",
		SynodView{Builtin: true, Description: &desc, Name: "ops-group", Operators: []string{"archon-alice"}, Roles: []string{"cluster-admin", "viewer"}},
		`{"builtin":true,"description":"группа ops","name":"ops-group","operators":["archon-alice"],"roles":["cluster-admin","viewer"]}`)
	// description omitted (nil); operators/roles empty arrays (non-nil) → `[]`.
	goldenSynodWire(t, "SynodView/nil_desc_empty_lists",
		SynodView{Builtin: false, Description: nil, Name: "empty-group", Operators: []string{}, Roles: []string{}},
		`{"builtin":false,"name":"empty-group","operators":[],"roles":[]}`)
	// nil-slice branch (on wire `null`).
	goldenSynodWire(t, "SynodView/nil_lists",
		SynodView{Builtin: false, Name: "x", Operators: nil, Roles: nil},
		`{"builtin":false,"name":"x","operators":null,"roles":null}`)

	// --- SynodListReply: items filled / empty / nil ---
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
