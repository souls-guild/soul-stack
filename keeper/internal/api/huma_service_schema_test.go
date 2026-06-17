// Доказательный гейт выравнивания имён SERVICE-схем под committed-рукопись (тираж-батч
// N1). Контрактные имена присутствуют; технические huma-Go-имена отсутствуют;
// scenarios-list-envelope ServiceScenariosListReply сверен по форме (service/ref/
// scenarios[], items.$ref на Scenario); ServiceListReply — items-only.
package api

import (
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// serviceContractSchemas — request/view/envelope-имена service-домена ровно как в
// committed-рукописи. ServiceListReply / ServiceRefsListReply / прочие reply уже несут
// oapi-типы с контрактными именами; ServiceScenariosListReply выровнен envelope-alias-ом.
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

// serviceForbiddenSchemas — технические huma-Go-имена. ServiceScenariosReply — Go-имя
// handler-типа, выровненное в ServiceScenariosListReply через envelope-alias.
var serviceForbiddenSchemas = []string{
	"ServiceRegisterHumaBody",
	"ServiceUpdateHumaBody",
	"ServiceScenariosReply",
}

// TestSchemaNames_Service — гейт N1. Контрактные имена присутствуют, технические — нет.
func TestSchemaNames_Service(t *testing.T) {
	schemas := loadFullSpecSchemas(t)
	for _, name := range serviceContractSchemas {
		if _, ok := schemas[name]; !ok {
			t.Errorf("контрактная схема %q ОТСУТСТВУЕТ в components/schemas (имя не выровнено)", name)
		}
	}
	for _, name := range serviceForbiddenSchemas {
		if _, ok := schemas[name]; ok {
			t.Errorf("техническое huma-имя %q ПРИСУТСТВУЕТ в спеке — имя не выровнено под контракт", name)
		}
	}
}

// TestSchemaNames_ServiceEnvelopes — гейт N1 (ENVELOPE). ServiceListReply — items-only
// (items.$ref на ServiceView); ServiceScenariosListReply — service/ref/scenarios форма
// (items.$ref на Scenario), выровнен через envelope-alias handlers.ServiceScenariosReply.
// Мутация (снять registerServiceEnvelopes) краснит: huma эмитит handler-Go-имя
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

// assertScenariosEnvelope проверяет форму ServiceScenariosListReply по рукописи:
// service/ref (string) + scenarios[] (array, items.$ref на Scenario).
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
			t.Errorf("%q не содержит контрактного поля %q", name, f)
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
