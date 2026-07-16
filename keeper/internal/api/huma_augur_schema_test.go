// Proof gate for aligning the AUGUR schema names (omens + rites) with the committed
// hand-written spec (rollout batch N2, per the huma_operator_schema_test.go / huma_role_schema_
// test.go reference). Assembles the aggregated huma spec (HumaFullSpecYAML) and checks that
// the augur-domain schemas are named EXACTLY as the contract (docs/keeper/openapi.yaml), and that
// the technical huma-Go names (OmenCreateHumaBody / RiteCreateHumaBody) are ABSENT from the
// spec. The envelope shape is verified per-resource against the hand-written spec: OmenListReply —
// 4-field-offset (int32), RiteListReply — items-only (list-by-omen without pagination).
// A mutation (restoring the old struct name / changing the envelope shape) turns it red.
package api

import (
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// augurContractSchemas — request/view/envelope names of the augur domain exactly as in the
// committed hand-written spec. All must be present in the assembled spec.
var augurContractSchemas = []string{
	"OmenCreateRequest",
	"RiteCreateRequest",
	"OmenView",
	"RiteView",
	"OmenListReply",
	"RiteListReply",
}

// augurForbiddenSchemas — technical huma-Go names that DefaultSchemaNamer WOULD produce
// from the old struct names (omenCreateHumaBody → OmenCreateHumaBody). None must
// remain in the spec after alignment. The source_type enum is inlined into OmenCreateRequest
// (the hand-written spec does NOT emit a standalone enum schema) — there is no separate forbidden name.
var augurForbiddenSchemas = []string{
	"OmenCreateHumaBody",
	"RiteCreateHumaBody",
}

// TestSchemaNames_Augur — gate N2. Contract names present, technical ones absent.
func TestSchemaNames_Augur(t *testing.T) {
	schemas := loadFullSpecSchemas(t)
	for _, name := range augurContractSchemas {
		if _, ok := schemas[name]; !ok {
			t.Errorf("контрактonя схема %q ОТСУТСТВУЕТ в components/schemas (имя не выровнеbut)", name)
		}
	}
	for _, name := range augurForbiddenSchemas {
		if _, ok := schemas[name]; ok {
			t.Errorf("техническое huma-имя %q ПРИСУТСТВУЕТ в спеке — имя не выровнеbut под контракт", name)
		}
	}
}

// TestSchemaNames_AugurEnvelopes — gate N2 (ENVELOPE); the shape is verified per-resource against
// the hand-written spec: OmenListReply — 4-field-offset (int32 items/offset/limit/total),
// RiteListReply — items-only (EXACTLY one items field, list-by-omen without pagination).
// A shape mutation turns it red.
func TestSchemaNames_AugurEnvelopes(t *testing.T) {
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
	assertEnvelopeShape(t, schemas, "OmenListReply", "OmenView")
	assertItemsOnlyEnvelope(t, schemas, "RiteListReply", "RiteView")
}
