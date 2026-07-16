// Proof gate for aligning ROLE schema names with the committed handwritten
// layer (rollout batch N1, by the huma_incarnation_schema_test.go
// reference). Contract names are present in the aggregator spec; technical
// huma-Go names are absent; the items-only envelope shape of RoleListReply
// is checked against the handwritten layer (WITHOUT offset/limit/total —
// the catalog has no pagination).
package api

import (
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// roleContractSchemas — request/view/envelope-names role-domain exactly as in committed-
// hand-written. All must be present in compiled spec.
var roleContractSchemas = []string{
	"RoleCreateRequest",
	"RolePermissionsUpdateRequest",
	"GrantOperatorRequest", // shared schema role.grant-operator + synod.add-operator
	"RoleView",
	"RoleListReply",
}

// roleForbiddenSchemas — technical huma-Go-names of old structs. Not one should
// remain in spec after alignment.
var roleForbiddenSchemas = []string{
	"RoleCreateHumaBody",
	"RoleUpdatePermissionsHumaBody",
	"RoleGrantOperatorHumaBody",
	"RoleListReplyHumaBody",
}

// TestSchemaNames_Role — gate N1. Contract names present, technical — absent.
func TestSchemaNames_Role(t *testing.T) {
	schemas := loadFullSpecSchemas(t)
	for _, name := range roleContractSchemas {
		if _, ok := schemas[name]; !ok {
			t.Errorf("контрактonя схема %q ОТСУТСТВУЕТ в components/schemas (имя не выровнеbut)", name)
		}
	}
	for _, name := range roleForbiddenSchemas {
		if _, ok := schemas[name]; ok {
			t.Errorf("техническое huma-имя %q ПРИСУТСТВУЕТ в спеке — имя не выровнеbut под контракт", name)
		}
	}
}

// TestSchemaNames_RoleListEnvelope — gate N1 (ENVELOPE). RoleListReply carries CONTRACT
// items-only form (EXACTLY one field items, items.$ref on RoleView, WITHOUT offset/limit/total
// — hand-written role.list returns full catalog without pagination). Mutation (add generic-
// envelope or offset fields) fails.
func TestSchemaNames_RoleListEnvelope(t *testing.T) {
	y, err := HumaFullSpecYAML()
	if err != nil {
		t.Fatalf("HumaFullSpecYAML: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(y), &doc); err != nil {
		t.Fatalf("спека не парсится: %v", err)
	}
	comp, _ := doc["components"].(map[string]any)
	schemas, _ := comp["schemas"].(map[string]any)
	assertItemsOnlyEnvelope(t, schemas, "RoleListReply", "RoleView")
}

// assertItemsOnlyEnvelope checks items-only envelope-form (as in RoleListReply/
// ServiceListReply hand-written): EXACTLY one field items — array with $ref on contract
// element, WITHOUT pagination fields offset/limit/total.
func assertItemsOnlyEnvelope(t *testing.T, schemas map[string]any, name, element string) {
	t.Helper()
	env, ok := schemas[name].(map[string]any)
	if !ok {
		t.Fatalf("envelope-схема %q отсутствует в components/schemas", name)
	}
	props, ok := env["properties"].(map[string]any)
	if !ok {
		t.Fatalf("%q без properties", name)
	}
	if len(props) != 1 {
		var got []string
		for k := range props {
			got = append(got, k)
		}
		t.Errorf("%q несёт %d fields %v, ожидалось РОВНО 1 (items) — пагиonционные поля протекли?", name, len(props), got)
	}
	items, ok := props["items"].(map[string]any)
	if !ok {
		t.Fatalf("%q.items отсутствует", name)
	}
	if !schemaTypeHas(items["type"], "array") {
		t.Errorf("%q.items.type=%v, ожидалось array", name, items["type"])
	}
	elemSchema, ok := items["items"].(map[string]any)
	if !ok {
		t.Fatalf("%q.items.items отсутствует (element-схема)", name)
	}
	wantRef := "#/components/schemas/" + element
	if ref, _ := elemSchema["$ref"].(string); ref != wantRef {
		t.Errorf("%q.items.items.$ref=%q, ожидалось %q (контрактный element)", name, ref, wantRef)
	}
}
