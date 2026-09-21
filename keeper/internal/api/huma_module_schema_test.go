// Evidence gate aligning MODULE form-prep schema names to the committed hand-written spec
// (rollout batch N4, following the huma_voyage_schema_test.go pattern). Assembles the
// aggregated huma spec (HumaFullSpecYAML) and checks the contract names of the form-prep request +
// nested chains (class C input-only) + absence of technical huma-Go names.
//
// MECHANISMS for module form-prep (checked against the hand-written spec):
//   - REQUEST-RENAME: moduleFormPrepHumaBody → ModuleFormPrepRequest (:5433).
//   - NESTED class C (input-only, single consumer each):
//     moduleFormPrepSourceHumaBody → ModuleFormPrepSource (:5447, ref only from
//     ModuleFormPrepRequest);
//     moduleFormPrepChoirSourceHumaBody → ModuleFormPrepChoirSource (:5457, ref only
//     from ModuleFormPrepSource).
//     class C = a plain Go-struct rename (no alias — no output consumer).
//
// CATALOG domain (GET /v1/modules, /v1/modules/{name}) — batch N6: handler-local structs
// moduleCatalogResponse/moduleItem renamed to moduleCatalogReply/moduleCatalogItem (a plain
// Go-struct rename, handler-types serialize them) → DefaultSchemaNamer produces the contract
// ModuleCatalogReply (:5424) / ModuleCatalogItem (:5392). Drift names ModuleCatalogResponse/
// ModuleItem are displaced (forbidden).
package api

import (
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// moduleContractSchemas — form-prep request + nested chain exactly as in the committed
// hand-written spec. ModuleFormPrepReply (200 body) is ALREADY a named oapi type, contract by itself.
var moduleContractSchemas = []string{
	"ModuleFormPrepRequest",
	"ModuleFormPrepSource",
	"ModuleFormPrepChoirSource",
	"ModuleFormPrepReply",
	// CATALOG domain (batch N6).
	"ModuleCatalogReply",
	"ModuleCatalogItem",
}

// moduleForbiddenSchemas — technical huma-Go names of the form-prep request + nested + drift names
// of the catalog domain (batch N6).
var moduleForbiddenSchemas = []string{
	"ModuleFormPrepHumaBody",
	"ModuleFormPrepSourceHumaBody",
	"ModuleFormPrepChoirSourceHumaBody",
	"ModuleCatalogResponse", // → ModuleCatalogReply
	"ModuleItem",            // → ModuleCatalogItem
}

// TestSchemaNames_Module — gate N4 (form-prep). Contract names present,
// technical ones absent.
func TestSchemaNames_Module(t *testing.T) {
	schemas := loadFullSpecSchemas(t)
	for _, name := range moduleContractSchemas {
		if _, ok := schemas[name]; !ok {
			t.Errorf("contract schema %q MISSING from components/schemas (name not aligned)", name)
		}
	}
	for _, name := range moduleForbiddenSchemas {
		if _, ok := schemas[name]; ok {
			t.Errorf("technical huma name %q PRESENT in the spec - name not aligned to the contract", name)
		}
	}
}

// TestSchemaNames_ModuleFormPrepNested — nested chain class C: ModuleFormPrepRequest.source
// → $ref ModuleFormPrepSource; ModuleFormPrepSource.choir → $ref ModuleFormPrepChoirSource.
// A mutation (inline instead of $ref / wrong name) fails it — guarantees the chain is assembled
// under the contract names.
func TestSchemaNames_ModuleFormPrepNested(t *testing.T) {
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

	const sourceRef = "#/components/schemas/ModuleFormPrepSource"
	if got := propRef(t, schemas, "ModuleFormPrepRequest", "source"); got != sourceRef {
		t.Errorf("ModuleFormPrepRequest.source -> %q, expected %q", got, sourceRef)
	}
	const choirRef = "#/components/schemas/ModuleFormPrepChoirSource"
	if got := propRef(t, schemas, "ModuleFormPrepSource", "choir"); got != choirRef {
		t.Errorf("ModuleFormPrepSource.choir -> %q, expected %q", got, choirRef)
	}

	// ModuleFormPrepRequest shape checked against the hand-written spec (:5433 — required:[source]).
	req, _ := schemas["ModuleFormPrepRequest"].(map[string]any)
	if req == nil {
		t.Fatal("ModuleFormPrepRequest missing")
	}
	assertRequiredExactly(t, req, "ModuleFormPrepRequest", "source")
}
