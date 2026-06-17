// Доказательный гейт выравнивания имён ORACLE-схем (vigils + decrees) под committed-
// рукопись (тираж-батч N2, по эталону huma_operator_schema_test.go). Собирает
// агрегированную huma-спеку (HumaFullSpecYAML) и проверяет, что схемы oracle-домена
// названы ТОЧНО как контракт (docs/keeper/openapi.yaml), а технические huma-Go-имена
// (VigilCreateHumaBody / DecreeCreateHumaBody) в спеке ОТСУТСТВУЮТ. Форма обоих
// envelope сверена с рукописью: VigilListReply и DecreeListReply — 4-поля-offset
// (int32 items/offset/limit/total). Мутация краснит.
package api

import (
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// oracleContractSchemas — request/view/envelope-имена oracle-домена ровно как в
// committed-рукописи. Все обязаны присутствовать в собранной спеке.
var oracleContractSchemas = []string{
	"VigilCreateRequest",
	"DecreeCreateRequest",
	"VigilView",
	"DecreeView",
	"VigilListReply",
	"DecreeListReply",
}

// oracleForbiddenSchemas — технические huma-Go-имена старых структур. Ни одно не должно
// остаться в спеке после выравнивания.
var oracleForbiddenSchemas = []string{
	"VigilCreateHumaBody",
	"DecreeCreateHumaBody",
}

// TestSchemaNames_Oracle — гейт N2. Контрактные имена присутствуют, технические — нет.
func TestSchemaNames_Oracle(t *testing.T) {
	schemas := loadFullSpecSchemas(t)
	for _, name := range oracleContractSchemas {
		if _, ok := schemas[name]; !ok {
			t.Errorf("контрактная схема %q ОТСУТСТВУЕТ в components/schemas (имя не выровнено)", name)
		}
	}
	for _, name := range oracleForbiddenSchemas {
		if _, ok := schemas[name]; ok {
			t.Errorf("техническое huma-имя %q ПРИСУТСТВУЕТ в спеке — имя не выровнено под контракт", name)
		}
	}
}

// TestSchemaNames_OracleEnvelopes — гейт N2 (ENVELOPE). Оба envelope несут КОНТРАКТНУЮ
// 4-поля-offset форму (int32 items/offset/limit/total; items.$ref на контрактный
// element). Сверено с рукописью VigilListReply/DecreeListReply. Мутация краснит.
func TestSchemaNames_OracleEnvelopes(t *testing.T) {
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
	assertEnvelopeShape(t, schemas, "VigilListReply", "VigilView")
	assertEnvelopeShape(t, schemas, "DecreeListReply", "DecreeView")
}
