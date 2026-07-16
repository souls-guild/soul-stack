// Evidence gate aligning PUSH-PROVIDER schema names to the committed hand-written spec
// (rollout batch N3, following the huma_operator_schema_test.go pattern). Assembles the
// aggregated huma spec (HumaFullSpecYAML) and checks that push-provider domain schemas are
// named EXACTLY as the contract (docs/keeper/openapi.yaml), and that technical huma-Go names
// (PushProviderCreateHumaBody / PushProviderUpdateHumaBody) are ABSENT.
//
// MECHANISMS for push-provider (checked against the hand-written spec):
//   - REQUEST-RENAME: pushProviderCreateHumaBody → PushProviderCreateRequest;
//     pushProviderUpdateHumaBody → PushProviderUpdateRequest. Applied.
//   - ENUM-ALIAS: NOT applied (domain has no enum fields in the schema).
//   - ENVELOPE: already a named oapi type (PushProviderListReply — a generated struct, NOT
//     generic PagedResponse) → DefaultSchemaNamer produces the contract name itself; no alias
//     needed. We verify the shape: 4-field-offset plain `integer`, items.$ref to PushProvider.
package api

import (
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// pushProviderContractSchemas — request/view/envelope names of the push-provider domain exactly
// as in the committed hand-written spec. All must be present in the assembled spec.
var pushProviderContractSchemas = []string{
	"PushProviderCreateRequest",
	"PushProviderUpdateRequest",
	"PushProvider",
	"PushProviderListReply",
}

// pushProviderForbiddenSchemas — technical huma-Go names of the old structs. None must
// remain in the spec after alignment.
var pushProviderForbiddenSchemas = []string{
	"PushProviderCreateHumaBody",
	"PushProviderUpdateHumaBody",
}

// TestSchemaNames_PushProvider — gate N3. Contract names present, technical ones absent.
func TestSchemaNames_PushProvider(t *testing.T) {
	schemas := loadFullSpecSchemas(t)
	for _, name := range pushProviderContractSchemas {
		if _, ok := schemas[name]; !ok {
			t.Errorf("контрактonя схема %q ОТСУТСТВУЕТ в components/schemas (имя не выровнеbut)", name)
		}
	}
	for _, name := range pushProviderForbiddenSchemas {
		if _, ok := schemas[name]; ok {
			t.Errorf("техническое huma-имя %q ПРИСУТСТВУЕТ в спеке — имя не выровнеbut под контракт", name)
		}
	}
}

// TestSchemaNames_PushProviderEnvelope — gate N3 (ENVELOPE). PushProviderListReply carries
// the CONTRACT 4-field-offset shape (items/offset/limit/total; items.$ref to PushProvider).
// Format-agnostic (hand-written spec — plain `integer`). A mutation (item-only/cursor/wrong $ref) fails it.
func TestSchemaNames_PushProviderEnvelope(t *testing.T) {
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
	assertOffsetEnvelopeNoFormat(t, schemas, "PushProviderListReply", "PushProvider")
}
