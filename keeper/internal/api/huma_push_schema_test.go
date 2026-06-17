// Доказательный гейт выравнивания имён PUSH-схем под committed-рукопись (тираж-батч N3,
// по эталону huma_operator_schema_test.go). Собирает агрегированную huma-спеку
// (HumaFullSpecYAML) и проверяет, что схемы push-домена названы ТОЧНО как контракт
// (docs/keeper/openapi.yaml), а техническое huma-Go-имя PushApplyHumaBody ОТСУТСТВУЕТ.
//
// МЕХАНИЗМЫ для push (сверены с рукописью):
//   - REQUEST-RENAME: pushApplyHumaBody → PushApplyRequest (контрактное имя тела
//     POST /v1/push/apply). Применён.
//   - ENUM-ALIAS: НЕ применяется. Рукопись НЕ объявляет standalone PushRunStatus —
//     статус инлайнится в PushRunListEntry (`type: string` + enum).
//   - ENVELOPE: уже named oapi-тип (PushRunListReply — генерёная struct, НЕ generic
//     PagedResponse) → DefaultSchemaNamer даёт контрактное имя сам; alias-механизм не
//     нужен. Здесь лишь СВЕРЯЕМ форму: 4-поля-offset с plain `integer` (рукопись — `type:
//     integer` без format → format-agnostic). items.$ref на контрактный PushRunListEntry.
package api

import (
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// pushContractSchemas — request/envelope/element-имена push-домена ровно как в committed-
// рукописи. Все обязаны присутствовать в собранной спеке.
var pushContractSchemas = []string{
	"PushApplyRequest",
	"PushApplyReply",
	"PushApplyView",
	"PushRunListReply",
	"PushRunListEntry",
}

// pushForbiddenSchemas — техническое huma-Go-имя, которое DefaultSchemaNamer дал БЫ из
// старого имени структуры (pushApplyHumaBody → PushApplyHumaBody). Не должно остаться.
var pushForbiddenSchemas = []string{
	"PushApplyHumaBody",
}

// TestSchemaNames_Push — гейт N3. Контрактные имена присутствуют, техническое — нет.
func TestSchemaNames_Push(t *testing.T) {
	schemas := loadFullSpecSchemas(t)
	for _, name := range pushContractSchemas {
		if _, ok := schemas[name]; !ok {
			t.Errorf("контрактная схема %q ОТСУТСТВУЕТ в components/schemas (имя не выровнено)", name)
		}
	}
	for _, name := range pushForbiddenSchemas {
		if _, ok := schemas[name]; ok {
			t.Errorf("техническое huma-имя %q ПРИСУТСТВУЕТ в спеке — имя не выровнено под контракт", name)
		}
	}
}

// TestSchemaNames_PushRunsEnvelope — гейт N3 (ENVELOPE). PushRunListReply несёт
// КОНТРАКТНУЮ 4-поля-offset форму (items/offset/limit/total; items.$ref на PushRunListEntry).
// Format-agnostic (рукопись — plain `integer`). Мутация (item-only/cursor/неверный $ref) краснит.
func TestSchemaNames_PushRunsEnvelope(t *testing.T) {
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
	assertOffsetEnvelopeNoFormat(t, schemas, "PushRunListReply", "PushRunListEntry")
}
