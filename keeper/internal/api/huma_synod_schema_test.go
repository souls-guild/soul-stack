// Proof gate that SYNOD schema names align with the committed hand-written spec
// (rollout batch N1). Contract names are present; technical huma-Go names are absent;
// add-operator carries the SHARED GrantOperatorRequest schema (like role.grant-operator);
// the items-only shape of SynodListReply is checked against the hand-written spec.
package api

import (
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// synodContractSchemas — request/view/envelope names of the synod domain exactly as in the
// committed hand-written spec. add-operator is described by the same GrantOperatorRequest as role.grant-operator.
var synodContractSchemas = []string{
	"SynodCreateRequest",
	"SynodUpdateRequest",
	"SynodGrantRoleRequest",
	"GrantOperatorRequest", // synod.add-operator + role.grant-operator (shared)
	"SynodView",
	"SynodListReply",
}

// synodForbiddenSchemas — technical huma-Go names of the old structs.
var synodForbiddenSchemas = []string{
	"SynodCreateHumaBody",
	"SynodUpdateHumaBody",
	"SynodAddOperatorHumaBody",
	"SynodGrantRoleHumaBody",
}

// TestSchemaNames_Synod — gate N1. Contract names present, technical ones absent.
func TestSchemaNames_Synod(t *testing.T) {
	schemas := loadFullSpecSchemas(t)
	for _, name := range synodContractSchemas {
		if _, ok := schemas[name]; !ok {
			t.Errorf("contract schema %q MISSING from components/schemas (name not aligned)", name)
		}
	}
	for _, name := range synodForbiddenSchemas {
		if _, ok := schemas[name]; ok {
			t.Errorf("technical huma name %q PRESENT in the spec - name not aligned to the contract", name)
		}
	}
}

// TestSchemaNames_SynodListEnvelope — gate N1 (ENVELOPE). SynodListReply (already an oapi type
// with a contract name) carries the items-only shape (items.$ref to SynodView, no pagination).
func TestSchemaNames_SynodListEnvelope(t *testing.T) {
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
	assertItemsOnlyEnvelope(t, schemas, "SynodListReply", "SynodView")
}
