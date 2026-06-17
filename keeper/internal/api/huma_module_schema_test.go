// Доказательный гейт выравнивания имён MODULE form-prep-схем под committed-рукопись
// (тираж-батч N4, по эталону huma_voyage_schema_test.go). Собирает агрегированную
// huma-спеку (HumaFullSpecYAML) и проверяет контрактные имена form-prep request +
// nested-цепочки (class C input-only) + отсутствие технических huma-Go-имён.
//
// МЕХАНИЗМЫ для module form-prep (сверены с рукописью):
//   - REQUEST-RENAME: moduleFormPrepHumaBody → ModuleFormPrepRequest (:5433).
//   - NESTED class C (input-only, single consumer каждая):
//     moduleFormPrepSourceHumaBody → ModuleFormPrepSource (:5447, ref только из
//     ModuleFormPrepRequest);
//     moduleFormPrepChoirSourceHumaBody → ModuleFormPrepChoirSource (:5457, ref только
//     из ModuleFormPrepSource).
//     class C = обычный rename Go-структуры (БЕЗ alias — нет output-потребителя).
//
// CATALOG-домен (GET /v1/modules, /v1/modules/{name}) — батч N6: handler-local-структуры
// moduleCatalogResponse/moduleItem переименованы в moduleCatalogReply/moduleCatalogItem (обычный
// rename Go-структур, handler-types сериализуют их) → DefaultSchemaNamer даёт контрактные
// ModuleCatalogReply (:5424) / ModuleCatalogItem (:5392). Дрейф-имена ModuleCatalogResponse/
// ModuleItem вытеснены (forbidden).
package api

import (
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// moduleContractSchemas — form-prep request + nested-цепочка ровно как в committed-
// рукописи. ModuleFormPrepReply (200-тело) — УЖЕ named oapi-тип, контрактен сам.
var moduleContractSchemas = []string{
	"ModuleFormPrepRequest",
	"ModuleFormPrepSource",
	"ModuleFormPrepChoirSource",
	"ModuleFormPrepReply",
	// CATALOG-домен (батч N6).
	"ModuleCatalogReply",
	"ModuleCatalogItem",
}

// moduleForbiddenSchemas — технические huma-Go-имена form-prep request + nested + дрейф-имена
// catalog-домена (батч N6).
var moduleForbiddenSchemas = []string{
	"ModuleFormPrepHumaBody",
	"ModuleFormPrepSourceHumaBody",
	"ModuleFormPrepChoirSourceHumaBody",
	"ModuleCatalogResponse", // → ModuleCatalogReply
	"ModuleItem",            // → ModuleCatalogItem
}

// TestSchemaNames_Module — гейт N4 (form-prep). Контрактные имена присутствуют,
// технические — нет.
func TestSchemaNames_Module(t *testing.T) {
	schemas := loadFullSpecSchemas(t)
	for _, name := range moduleContractSchemas {
		if _, ok := schemas[name]; !ok {
			t.Errorf("контрактная схема %q ОТСУТСТВУЕТ в components/schemas (имя не выровнено)", name)
		}
	}
	for _, name := range moduleForbiddenSchemas {
		if _, ok := schemas[name]; ok {
			t.Errorf("техническое huma-имя %q ПРИСУТСТВУЕТ в спеке — имя не выровнено под контракт", name)
		}
	}
}

// TestSchemaNames_ModuleFormPrepNested — nested-цепочка class C: ModuleFormPrepRequest.source
// → $ref ModuleFormPrepSource; ModuleFormPrepSource.choir → $ref ModuleFormPrepChoirSource.
// Мутация (инлайн вместо $ref / неверное имя) краснит — гарантирует, что цепочка собрана
// под контрактными именами.
func TestSchemaNames_ModuleFormPrepNested(t *testing.T) {
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

	const sourceRef = "#/components/schemas/ModuleFormPrepSource"
	if got := propRef(t, schemas, "ModuleFormPrepRequest", "source"); got != sourceRef {
		t.Errorf("ModuleFormPrepRequest.source → %q, ожидался %q", got, sourceRef)
	}
	const choirRef = "#/components/schemas/ModuleFormPrepChoirSource"
	if got := propRef(t, schemas, "ModuleFormPrepSource", "choir"); got != choirRef {
		t.Errorf("ModuleFormPrepSource.choir → %q, ожидался %q", got, choirRef)
	}

	// Форма ModuleFormPrepRequest сверена с рукописью (:5433 — required:[source]).
	req, _ := schemas["ModuleFormPrepRequest"].(map[string]any)
	if req == nil {
		t.Fatal("ModuleFormPrepRequest отсутствует")
	}
	assertRequiredExactly(t, req, "ModuleFormPrepRequest", "source")
}
