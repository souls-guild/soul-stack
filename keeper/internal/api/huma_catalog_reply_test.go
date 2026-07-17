// GOLDEN byte-exact wire-guard for the NATIVE wire-DTO CATALOG domain (handler-native T5d;
// permissions / event-types / me-permissions). catalog no longer depends on the legacy generator
// (0 legacy generator in catalog files), so golden compares json native values against a
// pinned reference string. Nested chains (Reply→Item→Action; MyPermission→Scope) and both
// pointer branches (scope nil/non-nil, dimensions nil/non-nil) are covered.
package api

import (
	"encoding/json"
	"testing"
)

// goldenCatalogWire compares json.Marshal(native) byte-for-byte against a pinned reference.
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
	// secret_required is always present (bool with no omitempty); enum_values is omitted
	// for the non-enum field (vault_ref) via omitempty.
	goldenCatalogWire(t, "HeraldTypeCatalogReply/full",
		HeraldTypeCatalogReply{Types: []HeraldTypeCatalogEntry{{Type: "telegram", Fields: []HeraldTypeFieldSpec{{Name: "bot_token_ref", Label: "Vault ref for bot token", Required: true, Secret: true, Kind: "vault_ref"}}}}},
		`{"types":[{"type":"telegram","fields":[{"name":"bot_token_ref","label":"Vault ref for bot token","required":true,"secret":true,"kind":"vault_ref"}],"secret_required":false}]}`)
	// enum field → enum_values present (select render); webhook → secret_required=true.
	goldenCatalogWire(t, "HeraldTypeCatalogReply/enum_and_secret_required",
		HeraldTypeCatalogReply{Types: []HeraldTypeCatalogEntry{{Type: "webhook", SecretRequired: true, Fields: []HeraldTypeFieldSpec{{Name: "parse_mode", Label: "Text format", Kind: "enum", EnumValues: []string{"", "MarkdownV2", "HTML"}}}}}},
		`{"types":[{"type":"webhook","fields":[{"name":"parse_mode","label":"Text format","required":false,"secret":false,"kind":"enum","enum_values":["","MarkdownV2","HTML"]}],"secret_required":true}]}`)
	goldenCatalogWire(t, "HeraldTypeCatalogReply/nil",
		HeraldTypeCatalogReply{Types: nil},
		`{"types":null}`)

	// --- MyPermissionsReply: scope set/nil, dimensions set/nil, wildcard ---
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
