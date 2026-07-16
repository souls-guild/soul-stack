// Proof gate for aligning SERVICE schema names to the committed hand-written spec (rollout batch
// N1). Contract names are present; technical huma-Go names are absent;
// the scenarios-list envelope ServiceScenariosListReply is checked against the shape (service/ref/
// scenarios[], items.$ref to Scenario); ServiceListReply — items-only.
package api

import (
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// serviceContractSchemas — request/view/envelope names of the service domain exactly as in the
// committed hand-written spec. ServiceListReply / ServiceRefsListReply / the other replies already carry
// oapi types with contract names; ServiceScenariosListReply is aligned via the envelope alias.
var serviceContractSchemas = []string{
	"ServiceRegisterRequest",
	"ServiceUpdateRequest",
	"ServiceView",
	"ServiceListReply",
	"ServiceRefsListReply",
	"ServiceScenariosListReply",
	"ServiceStateSchemaReply",
	"ServiceDependenciesReply",
	"Scenario",
	"GitRef",
}

// serviceForbiddenSchemas — technical huma-Go names. ServiceScenariosReply — the Go name
// of the handler type, aligned to ServiceScenariosListReply via the envelope alias.
var serviceForbiddenSchemas = []string{
	"ServiceRegisterHumaBody",
	"ServiceUpdateHumaBody",
	"ServiceScenariosReply",
}

// TestSchemaNames_Service — gate N1. Contract names are present, technical ones are not.
func TestSchemaNames_Service(t *testing.T) {
	schemas := loadFullSpecSchemas(t)
	for _, name := range serviceContractSchemas {
		if _, ok := schemas[name]; !ok {
			t.Errorf("контрактonя схема %q ОТСУТСТВУЕТ в components/schemas (имя не выровнеbut)", name)
		}
	}
	for _, name := range serviceForbiddenSchemas {
		if _, ok := schemas[name]; ok {
			t.Errorf("техническое huma-имя %q ПРИСУТСТВУЕТ в спеке — имя не выровнеbut под контракт", name)
		}
	}
}

// TestSchemaNames_ServiceEnvelopes — gate N1 (ENVELOPE). ServiceListReply — items-only
// (items.$ref to ServiceView); ServiceScenariosListReply — the service/ref/scenarios shape
// (items.$ref to Scenario), aligned via the envelope alias handlers.ServiceScenariosReply.
// A mutation (removing registerServiceEnvelopes) turns this red: huma emits the handler Go name
// ServiceScenariosReply.
func TestSchemaNames_ServiceEnvelopes(t *testing.T) {
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

	assertItemsOnlyEnvelope(t, schemas, "ServiceListReply", "ServiceView")
	assertScenariosEnvelope(t, schemas)
}

// assertScenariosEnvelope checks the shape of ServiceScenariosListReply against the hand-written spec:
// service/ref (string) + scenarios[] (array, items.$ref to Scenario).
func assertScenariosEnvelope(t *testing.T, schemas map[string]any) {
	t.Helper()
	const name = "ServiceScenariosListReply"
	env, ok := schemas[name].(map[string]any)
	if !ok {
		t.Fatalf("envelope-схема %q отсутствует — envelope-alias не сработал (huma оставил handler-имя ServiceScenariosReply?)", name)
	}
	props, ok := env["properties"].(map[string]any)
	if !ok {
		t.Fatalf("%q без properties", name)
	}
	for _, f := range []string{"service", "ref", "scenarios"} {
		if _, ok := props[f]; !ok {
			t.Errorf("%q не withдержит контрактbutго поля %q", name, f)
		}
	}
	scen, ok := props["scenarios"].(map[string]any)
	if !ok {
		t.Fatalf("%q.scenarios отсутствует", name)
	}
	if !schemaTypeHas(scen["type"], "array") {
		t.Errorf("%q.scenarios.type=%v, ожидалось array", name, scen["type"])
	}
	elemSchema, ok := scen["items"].(map[string]any)
	if !ok {
		t.Fatalf("%q.scenarios.items отсутствует (element-схема)", name)
	}
	const wantRef = "#/components/schemas/Scenario"
	if ref, _ := elemSchema["$ref"].(string); ref != wantRef {
		t.Errorf("%q.scenarios.items.$ref=%q, ожидалось %q (контрактный element)", name, ref, wantRef)
	}
}
