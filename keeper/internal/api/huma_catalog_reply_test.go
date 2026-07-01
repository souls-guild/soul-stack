// GOLDEN byte-exact wire-guard для NATIVE wire-DTO CATALOG-домена (handler-native T5d;
// permissions / event-types / me-permissions). catalog больше НЕ зависит от legacy-генерата
// (0 legacy-генерата в catalog-файлах), поэтому golden сверяет json native-значения с
// ЗАФИКСИРОВАННОЙ строкой-эталоном. Покрыты nested-цепочки (Reply→Item→Action;
// MyPermission→Scope) и обе указательные ветки (scope nil/non-nil, измерения nil/non-nil).
package api

import (
	"encoding/json"
	"testing"
)

// goldenCatalogWire сверяет json.Marshal(native) байт-в-байт с зафиксированным эталоном.
func goldenCatalogWire(t *testing.T, name string, native any, want string) {
	t.Helper()
	got, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("%s: marshal native: %v", name, err)
	}
	if string(got) != want {
		t.Errorf("%s: WIRE DRIFT\n got  = %s\n want = %s", name, got, want)
	}
}

func TestGoldenWire_CatalogReply(t *testing.T) {
	// --- PermissionCatalogReply: nested actions+selector_keys ---
	goldenCatalogWire(t, "PermissionCatalogReply/full",
		PermissionCatalogReply{Items: []PermissionCatalogItem{{Resource: "incarnation", Actions: []PermissionAction{{Action: "run", SelectorKeys: []string{"coven", "host"}}}}}},
		`{"items":[{"actions":[{"action":"run","selector_keys":["coven","host"]}],"resource":"incarnation"}]}`)
	goldenCatalogWire(t, "PermissionCatalogReply/nil_items",
		PermissionCatalogReply{Items: nil},
		`{"items":null}`)

	// --- EventTypeCatalogReply: areas + point_events ---
	goldenCatalogWire(t, "EventTypeCatalogReply/full",
		EventTypeCatalogReply{Areas: []EventTypeArea{{Name: "scenario_run.*"}}, PointEvents: []EventTypePoint{{Name: "incarnation.run_completed"}}},
		`{"areas":[{"name":"scenario_run.*"}],"point_events":[{"name":"incarnation.run_completed"}]}`)
	goldenCatalogWire(t, "EventTypeCatalogReply/nil",
		EventTypeCatalogReply{Areas: nil, PointEvents: nil},
		`{"areas":null,"point_events":null}`)

	// --- HeraldTypeCatalogReply: types + nested fields (ADR-052 amendment) ---
	goldenCatalogWire(t, "HeraldTypeCatalogReply/full",
		HeraldTypeCatalogReply{Types: []HeraldTypeCatalogEntry{{Type: "telegram", Fields: []HeraldTypeFieldSpec{{Name: "bot_token_ref", Label: "Vault-ref токена бота", Required: true, Secret: true, Kind: "vault_ref"}}}}},
		`{"types":[{"type":"telegram","fields":[{"name":"bot_token_ref","label":"Vault-ref токена бота","required":true,"secret":true,"kind":"vault_ref"}]}]}`)
	goldenCatalogWire(t, "HeraldTypeCatalogReply/nil",
		HeraldTypeCatalogReply{Types: nil},
		`{"types":null}`)

	// --- MyPermissionsReply: scope set/nil, измерения set/nil, wildcard ---
	res := "soul"
	act := "list"
	wild := true
	covens := []string{"prod"}
	regex := []string{"^web.*"}
	goldenCatalogWire(t, "MyPermissionsReply/full",
		MyPermissionsReply{Permissions: []MyPermission{{Resource: &res, Action: &act, Scope: &MyPermissionScope{Covens: &covens, Regex: &regex, Unrestricted: false}}}},
		`{"permissions":[{"action":"list","resource":"soul","scope":{"covens":["prod"],"regex":["^web.*"],"unrestricted":false}}]}`)
	goldenCatalogWire(t, "MyPermissionsReply/wildcard_no_scope",
		MyPermissionsReply{Permissions: []MyPermission{{Wildcard: &wild}}},
		`{"permissions":[{"wildcard":true}]}`)
	goldenCatalogWire(t, "MyPermissionsReply/unrestricted_scope",
		MyPermissionsReply{Permissions: []MyPermission{{Resource: &res, Action: &act, Scope: &MyPermissionScope{Unrestricted: true}}}},
		`{"permissions":[{"action":"list","resource":"soul","scope":{"unrestricted":true}}]}`)
	goldenCatalogWire(t, "MyPermissionsReply/nil_perms",
		MyPermissionsReply{Permissions: nil},
		`{"permissions":null}`)
}
