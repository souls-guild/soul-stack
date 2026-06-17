// Доказательный гейт выравнивания имён AUGUR-схем (omens + rites) под committed-
// рукопись (тираж-батч N2, по эталону huma_operator_schema_test.go / huma_role_schema_
// test.go). Собирает агрегированную huma-спеку (HumaFullSpecYAML) и проверяет, что
// схемы augur-домена названы ТОЧНО как контракт (docs/keeper/openapi.yaml), а
// технические huma-Go-имена (OmenCreateHumaBody / RiteCreateHumaBody) в спеке
// ОТСУТСТВУЮТ. Форма envelope сверена per-resource с рукописью: OmenListReply —
// 4-поля-offset (int32), RiteListReply — items-only (list-by-omen без пагинации).
// Мутация (вернуть старое имя структуры / поменять форму envelope) краснит.
package api

import (
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// augurContractSchemas — request/view/envelope-имена augur-домена ровно как в committed-
// рукописи. Все обязаны присутствовать в собранной спеке.
var augurContractSchemas = []string{
	"OmenCreateRequest",
	"RiteCreateRequest",
	"OmenView",
	"RiteView",
	"OmenListReply",
	"RiteListReply",
}

// augurForbiddenSchemas — технические huma-Go-имена, которые DefaultSchemaNamer дал БЫ
// из старых имён структур (omenCreateHumaBody → OmenCreateHumaBody). Ни одно не должно
// остаться в спеке после выравнивания. Enum source_type инлайнится в OmenCreateRequest
// (рукопись НЕ выносит standalone enum-схему) — отдельного forbidden-имени нет.
var augurForbiddenSchemas = []string{
	"OmenCreateHumaBody",
	"RiteCreateHumaBody",
}

// TestSchemaNames_Augur — гейт N2. Контрактные имена присутствуют, технические — нет.
func TestSchemaNames_Augur(t *testing.T) {
	schemas := loadFullSpecSchemas(t)
	for _, name := range augurContractSchemas {
		if _, ok := schemas[name]; !ok {
			t.Errorf("контрактная схема %q ОТСУТСТВУЕТ в components/schemas (имя не выровнено)", name)
		}
	}
	for _, name := range augurForbiddenSchemas {
		if _, ok := schemas[name]; ok {
			t.Errorf("техническое huma-имя %q ПРИСУТСТВУЕТ в спеке — имя не выровнено под контракт", name)
		}
	}
}

// TestSchemaNames_AugurEnvelopes — гейт N2 (ENVELOPE), форма сверена per-resource с
// рукописью: OmenListReply — 4-поля-offset (int32 items/offset/limit/total),
// RiteListReply — items-only (РОВНО одно поле items, list-by-omen без пагинации).
// Мутация формы краснит.
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
