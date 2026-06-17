// Доказательный гейт выравнивания имён PUSH-PROVIDER-схем под committed-рукопись (тираж-
// батч N3, по эталону huma_operator_schema_test.go). Собирает агрегированную huma-спеку
// (HumaFullSpecYAML) и проверяет, что схемы push-provider-домена названы ТОЧНО как
// контракт (docs/keeper/openapi.yaml), а технические huma-Go-имена (PushProviderCreate-
// HumaBody / PushProviderUpdateHumaBody) ОТСУТСТВУЮТ.
//
// МЕХАНИЗМЫ для push-provider (сверены с рукописью):
//   - REQUEST-RENAME: pushProviderCreateHumaBody → PushProviderCreateRequest;
//     pushProviderUpdateHumaBody → PushProviderUpdateRequest. Применён.
//   - ENUM-ALIAS: НЕ применяется (домен без enum-полей в схеме).
//   - ENVELOPE: уже named oapi-тип (PushProviderListReply — генерёная struct, НЕ
//     generic PagedResponse) → DefaultSchemaNamer даёт контрактное имя сам; alias не
//     нужен. СВЕРЯЕМ форму: 4-поля-offset plain `integer`, items.$ref на PushProvider.
package api

import (
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// pushProviderContractSchemas — request/view/envelope-имена push-provider-домена ровно
// как в committed-рукописи. Все обязаны присутствовать в собранной спеке.
var pushProviderContractSchemas = []string{
	"PushProviderCreateRequest",
	"PushProviderUpdateRequest",
	"PushProvider",
	"PushProviderListReply",
}

// pushProviderForbiddenSchemas — технические huma-Go-имена старых структур. Ни одно не
// должно остаться в спеке после выравнивания.
var pushProviderForbiddenSchemas = []string{
	"PushProviderCreateHumaBody",
	"PushProviderUpdateHumaBody",
}

// TestSchemaNames_PushProvider — гейт N3. Контрактные имена присутствуют, технические — нет.
func TestSchemaNames_PushProvider(t *testing.T) {
	schemas := loadFullSpecSchemas(t)
	for _, name := range pushProviderContractSchemas {
		if _, ok := schemas[name]; !ok {
			t.Errorf("контрактная схема %q ОТСУТСТВУЕТ в components/schemas (имя не выровнено)", name)
		}
	}
	for _, name := range pushProviderForbiddenSchemas {
		if _, ok := schemas[name]; ok {
			t.Errorf("техническое huma-имя %q ПРИСУТСТВУЕТ в спеке — имя не выровнено под контракт", name)
		}
	}
}

// TestSchemaNames_PushProviderEnvelope — гейт N3 (ENVELOPE). PushProviderListReply несёт
// КОНТРАКТНУЮ 4-поля-offset форму (items/offset/limit/total; items.$ref на PushProvider).
// Format-agnostic (рукопись — plain `integer`). Мутация (item-only/cursor/неверный $ref) краснит.
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
