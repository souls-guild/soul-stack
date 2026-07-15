// A proof gate that OPERATOR schema names are aligned with the committed hand-written
// spec (rollout batch N1, following the huma_incarnation_schema_test.go reference
// pattern). Assembles the aggregated huma spec (HumaFullSpecYAML) and checks that the
// operator-domain schemas are named EXACTLY as the contract (docs/keeper/openapi.yaml),
// and that the technical huma-Go names (operatorCreateHumaBody / PagedResponseOperator)
// are ABSENT from the spec. A mutation (restore the old struct name / drop the
// envelope alias) turns this test red.
package api

import (
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// operatorContractSchemas — the request/reply/view/envelope names of the operator
// domain exactly as in the committed hand-written spec. All must be present in the
// assembled spec.
var operatorContractSchemas = []string{
	"OperatorCreateRequest",
	"OperatorCreateReply",
	"OperatorRevokeRequest",
	"Operator",
	"OperatorListReply",
	"IssueTokenReply",
}

// operatorForbiddenSchemas — the technical huma-Go names that DefaultSchemaNamer
// WOULD produce from the old struct names / an un-aliased generic
// PagedResponse[Operator]. None must remain in the spec after alignment.
var operatorForbiddenSchemas = []string{
	"OperatorCreateHumaBody",
	"OperatorRevokeHumaBody",
	"PagedResponseOperator",
}

// TestSchemaNames_Operator — the N1 gate. Proves the assembled aggregator spec
// carries the operator schemas under CONTRACT names and carries no technical huma
// names.
func TestSchemaNames_Operator(t *testing.T) {
	schemas := loadFullSpecSchemas(t)
	for _, name := range operatorContractSchemas {
		if _, ok := schemas[name]; !ok {
			t.Errorf("контрактная схема %q ОТСУТСТВУЕТ в components/schemas (имя не выровнено)", name)
		}
	}
	for _, name := range operatorForbiddenSchemas {
		if _, ok := schemas[name]; ok {
			t.Errorf("техническое huma-имя %q ПРИСУТСТВУЕТ в спеке — имя не выровнено под контракт", name)
		}
	}
}

// TestSchemaNames_OperatorEnvelope — the N1 gate (ENVELOPE). OperatorListReply is
// surfaced as a named schema under the CONTRACT name with the CONTRACT offset shape
// (exactly 4 int32 fields items/offset/limit/total with NO cursor fields; items.$ref
// to the contract Operator). A mutation (remove registerOperatorEnvelopes) turns it
// red: huma emits the generic name PagedResponseOperator.
func TestSchemaNames_OperatorEnvelope(t *testing.T) {
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
	assertEnvelopeShape(t, schemas, "OperatorListReply", "Operator")
}

// loadFullSpecSchemas assembles the aggregator spec and returns its
// components/schemas map (a shared helper for the per-domain schema gates of rollout
// N1).
func loadFullSpecSchemas(t *testing.T) map[string]yaml.Node {
	t.Helper()
	y, err := HumaFullSpecYAML()
	if err != nil {
		t.Fatalf("HumaFullSpecYAML: %v", err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]yaml.Node `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal([]byte(y), &doc); err != nil {
		t.Fatalf("спека не парсится: %v", err)
	}
	return doc.Components.Schemas
}
