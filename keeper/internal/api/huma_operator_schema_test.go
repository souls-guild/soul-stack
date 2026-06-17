// Доказательный гейт выравнивания имён OPERATOR-схем под committed-рукопись (тираж-
// батч N1, по эталону huma_incarnation_schema_test.go). Собирает агрегированную
// huma-спеку (HumaFullSpecYAML) и проверяет, что схемы operator-домена названы ТОЧНО
// как контракт (docs/keeper/openapi.yaml), а технические huma-Go-имена
// (operatorCreateHumaBody / PagedResponseOperator) в спеке ОТСУТСТВУЮТ. Мутация
// (вернуть старое имя структуры / снять envelope-alias) краснит этот тест.
package api

import (
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// operatorContractSchemas — request/reply/view/envelope-имена operator-домена ровно
// как в committed-рукописи. Все обязаны присутствовать в собранной спеке.
var operatorContractSchemas = []string{
	"OperatorCreateRequest",
	"OperatorCreateReply",
	"OperatorRevokeRequest",
	"Operator",
	"OperatorListReply",
	"IssueTokenReply",
}

// operatorForbiddenSchemas — технические huma-Go-имена, которые DefaultSchemaNamer дал
// БЫ из старых имён структур / неаласенного generic PagedResponse[Operator]. Ни
// одно не должно остаться в спеке после выравнивания.
var operatorForbiddenSchemas = []string{
	"OperatorCreateHumaBody",
	"OperatorRevokeHumaBody",
	"PagedResponseOperator",
}

// TestSchemaNames_Operator — гейт N1. Доказывает, что собранная агрегатор-спека несёт
// operator-схемы под КОНТРАКТНЫМИ именами и не несёт технических huma-имён.
func TestSchemaNames_Operator(t *testing.T) {
	schemas := loadFullSpecSchemas(t)
	for _, name := range operatorContractSchemas {
		if _, ok := schemas[name]; !ok {
			t.Errorf("контрактная схема %q ОТСУТСТВУЕТ в components/schemas (имя не выровнено)", name)
		}
	}
	for _, name := range operatorForbiddenSchemas {
		if _, ok := schemas[name]; ok {
			t.Errorf("техническое huma-имя %q ПРИСУТСТВУЕТ в спеке — имя не выровнено под контракт", name)
		}
	}
}

// TestSchemaNames_OperatorEnvelope — гейт N1 (ENVELOPE). OperatorListReply вынесен как
// named-схема под КОНТРАКТНЫМ именем с КОНТРАКТНОЙ offset-формой (ровно 4 поля int32
// items/offset/limit/total БЕЗ cursor-полей; items.$ref на контрактный Operator). Мутация
// (убрать registerOperatorEnvelopes) краснит: huma эмитит generic-имя PagedResponseOperator.
func TestSchemaNames_OperatorEnvelope(t *testing.T) {
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
	assertEnvelopeShape(t, schemas, "OperatorListReply", "Operator")
}

// loadFullSpecSchemas собирает агрегатор-спеку и возвращает её components/schemas-карту
// (общий хелпер per-доменных schema-гейтов тиража N1).
func loadFullSpecSchemas(t *testing.T) map[string]yaml.Node {
	t.Helper()
	y, err := HumaFullSpecYAML()
	if err != nil {
		t.Fatalf("HumaFullSpecYAML: %v", err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]yaml.Node `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal([]byte(y), &doc); err != nil {
		t.Fatalf("спека не парсится: %v", err)
	}
	return doc.Components.Schemas
}
