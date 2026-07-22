// Evidence gate for aligning ORACLE schema names (vigils + decrees) to the committed
// hand-written spec (rollout batch N2, following huma_operator_schema_test.go). It assembles
// the aggregated huma spec (HumaFullSpecYAML) and checks that the oracle-domain schemas are
// named EXACTLY as the contract (docs/keeper/openapi.yaml), while the technical huma Go names
// (VigilCreateHumaBody / DecreeCreateHumaBody) are ABSENT from the spec. The shape of both
// envelopes is checked against the hand-written spec: VigilListReply and DecreeListReply are
// 4-field-offset (int32 items/offset/limit/total). A mutation reddens.
package api

import (
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// oracleContractSchemas — request/view/envelope names of the oracle domain exactly as in
// the committed hand-written spec. All must be present in the assembled spec.
var oracleContractSchemas = []string{
	"VigilCreateRequest",
	"DecreeCreateRequest",
	"VigilView",
	"DecreeView",
	"VigilListReply",
	"DecreeListReply",
}

// oracleForbiddenSchemas — technical huma Go names of the old structs. None must
// remain in the spec after the alignment.
var oracleForbiddenSchemas = []string{
	"VigilCreateHumaBody",
	"DecreeCreateHumaBody",
}

// TestSchemaNames_Oracle — gate N2. Contract names present, technical names absent.
func TestSchemaNames_Oracle(t *testing.T) {
	schemas := loadFullSpecSchemas(t)
	for _, name := range oracleContractSchemas {
		if _, ok := schemas[name]; !ok {
			t.Errorf("contract schema %q MISSING from components/schemas (name not aligned)", name)
		}
	}
	for _, name := range oracleForbiddenSchemas {
		if _, ok := schemas[name]; ok {
			t.Errorf("technical huma name %q PRESENT in the spec - name not aligned to the contract", name)
		}
	}
}

// TestSchemaNames_OracleEnvelopes — gate N2 (ENVELOPE). Both envelopes carry the CONTRACT
// 4-field-offset shape (int32 items/offset/limit/total; items.$ref to the contract
// element). Checked against hand-written VigilListReply/DecreeListReply. A mutation reddens.
func TestSchemaNames_OracleEnvelopes(t *testing.T) {
	y, err := HumaFullSpecYAML()
	if err != nil {
		t.Fatalf("HumaFullSpecYAML: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(y), &doc); err != nil {
		t.Fatalf("spec does not parse: %v", err)
	}
	comp, _ := doc["components"].(map[string]any)
	schemas, _ := comp["schemas"].(map[string]any)
	assertEnvelopeShape(t, schemas, "VigilListReply", "VigilView")
	assertEnvelopeShape(t, schemas, "DecreeListReply", "DecreeView")
}
