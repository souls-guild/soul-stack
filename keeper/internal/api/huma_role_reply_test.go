// GOLDEN byte-exact wire-guard for the NATIVE wire-DTO of the ROLE domain (handler-native T5d).
// role no longer depends on the legacy generator (0 legacy generator in role files), so the golden
// checks the json native values against a PINNED reference string (not against a legacy-generator value).
// This pins the exact wire bytes: a shape mutation (drop omitempty / change a json tag /
// field type / order) reddens the matching case. The only reply route with a body is
// GET /v1/roles (RoleListReply.Items []RoleView); create/delete/update-permissions/
// grant/revoke are 201/204 with no body (no golden needed). Both pointer branches
// (nil/non-nil) are covered for default_scope/description + []-vs-null for operators/permissions.
package api

import (
	"encoding/json"
	"testing"
)

// goldenRoleWire checks json.Marshal(native) byte-for-byte against a pinned reference.
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
	desc := "ops role"

	// --- RoleView: default_scope/description omitempty (both branches) + []-vs-null ---
	goldenRoleWire(t, "RoleView/full",
		RoleView{Builtin: true, DefaultScope: &scope, Description: &desc, Name: "ops", Operators: []string{"archon-alice"}, Permissions: []string{"role.list", "service.list"}},
		`{"builtin":true,"default_scope":"coven=prod,eu","description":"ops role","name":"ops","operators":["archon-alice"],"permissions":["role.list","service.list"]}`)
	// default_scope omitted (nil), description present and empty; operators/permissions
	// empty arrays (non-nil) → `[]`.
	goldenRoleWire(t, "RoleView/nil_scope_empty_lists",
		RoleView{Builtin: false, DefaultScope: nil, Description: nil, Name: "viewer", Operators: []string{}, Permissions: []string{}},
		`{"builtin":false,"name":"viewer","operators":[],"permissions":[]}`)
	// nil-slice branch (on the wire `null`).
	goldenRoleWire(t, "RoleView/nil_lists",
		RoleView{Builtin: false, Name: "x", Operators: nil, Permissions: nil},
		`{"builtin":false,"name":"x","operators":null,"permissions":null}`)

	// --- RoleListReply: items populated / empty / nil ---
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
