// Доказательный гейт выравнивания имён SYNOD-схем под committed-рукопись (тираж-батч
// N1). Контрактные имена присутствуют; технические huma-Go-имена отсутствуют;
// add-operator несёт ОБЩУЮ схему GrantOperatorRequest (как role.grant-operator);
// items-only форма SynodListReply сверена с рукописью.
package api

import (
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// synodContractSchemas — request/view/envelope-имена synod-домена ровно как в committed-
// рукописи. add-operator описан той же GrantOperatorRequest, что role.grant-operator.
var synodContractSchemas = []string{
	"SynodCreateRequest",
	"SynodUpdateRequest",
	"SynodGrantRoleRequest",
	"GrantOperatorRequest", // synod.add-operator + role.grant-operator (общая)
	"SynodView",
	"SynodListReply",
}

// synodForbiddenSchemas — технические huma-Go-имена старых структур.
var synodForbiddenSchemas = []string{
	"SynodCreateHumaBody",
	"SynodUpdateHumaBody",
	"SynodAddOperatorHumaBody",
	"SynodGrantRoleHumaBody",
}

// TestSchemaNames_Synod — гейт N1. Контрактные имена присутствуют, технические — нет.
func TestSchemaNames_Synod(t *testing.T) {
	schemas := loadFullSpecSchemas(t)
	for _, name := range synodContractSchemas {
		if _, ok := schemas[name]; !ok {
			t.Errorf("контрактная схема %q ОТСУТСТВУЕТ в components/schemas (имя не выровнено)", name)
		}
	}
	for _, name := range synodForbiddenSchemas {
		if _, ok := schemas[name]; ok {
			t.Errorf("техническое huma-имя %q ПРИСУТСТВУЕТ в спеке — имя не выровнено под контракт", name)
		}
	}
}

// TestSchemaNames_SynodListEnvelope — гейт N1 (ENVELOPE). SynodListReply (уже oapi-тип с
// контрактным именем) несёт items-only форму (items.$ref на SynodView, БЕЗ пагинации).
func TestSchemaNames_SynodListEnvelope(t *testing.T) {
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
	assertItemsOnlyEnvelope(t, schemas, "SynodListReply", "SynodView")
}
