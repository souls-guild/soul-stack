// Доказательный гейт выравнивания имён ROLE-схем под committed-рукопись (тираж-батч
// N1, по эталону huma_incarnation_schema_test.go). Контрактные имена присутствуют в
// агрегатор-спеке; технические huma-Go-имена отсутствуют; форма items-only envelope
// RoleListReply сверена с рукописью (БЕЗ offset/limit/total — каталог без пагинации).
package api

import (
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// roleContractSchemas — request/view/envelope-имена role-домена ровно как в committed-
// рукописи. Все обязаны присутствовать в собранной спеке.
var roleContractSchemas = []string{
	"RoleCreateRequest",
	"RolePermissionsUpdateRequest",
	"GrantOperatorRequest", // общая схема role.grant-operator + synod.add-operator
	"RoleView",
	"RoleListReply",
}

// roleForbiddenSchemas — технические huma-Go-имена старых структур. Ни одно не должно
// остаться в спеке после выравнивания.
var roleForbiddenSchemas = []string{
	"RoleCreateHumaBody",
	"RoleUpdatePermissionsHumaBody",
	"RoleGrantOperatorHumaBody",
	"RoleListReplyHumaBody",
}

// TestSchemaNames_Role — гейт N1. Контрактные имена присутствуют, технические — нет.
func TestSchemaNames_Role(t *testing.T) {
	schemas := loadFullSpecSchemas(t)
	for _, name := range roleContractSchemas {
		if _, ok := schemas[name]; !ok {
			t.Errorf("контрактная схема %q ОТСУТСТВУЕТ в components/schemas (имя не выровнено)", name)
		}
	}
	for _, name := range roleForbiddenSchemas {
		if _, ok := schemas[name]; ok {
			t.Errorf("техническое huma-имя %q ПРИСУТСТВУЕТ в спеке — имя не выровнено под контракт", name)
		}
	}
}

// TestSchemaNames_RoleListEnvelope — гейт N1 (ENVELOPE). RoleListReply несёт КОНТРАКТНУЮ
// items-only форму (РОВНО одно поле items, items.$ref на RoleView, БЕЗ offset/limit/total
// — рукопись role.list отдаёт весь каталог без пагинации). Мутация (добавить generic-
// envelope или offset-поля) краснит.
func TestSchemaNames_RoleListEnvelope(t *testing.T) {
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
	assertItemsOnlyEnvelope(t, schemas, "RoleListReply", "RoleView")
}

// assertItemsOnlyEnvelope проверяет items-only envelope-форму (как в RoleListReply/
// ServiceListReply рукописи): РОВНО одно поле items — массив с $ref на контрактный
// element, БЕЗ пагинационных полей offset/limit/total.
func assertItemsOnlyEnvelope(t *testing.T, schemas map[string]any, name, element string) {
	t.Helper()
	env, ok := schemas[name].(map[string]any)
	if !ok {
		t.Fatalf("envelope-схема %q отсутствует в components/schemas", name)
	}
	props, ok := env["properties"].(map[string]any)
	if !ok {
		t.Fatalf("%q без properties", name)
	}
	if len(props) != 1 {
		var got []string
		for k := range props {
			got = append(got, k)
		}
		t.Errorf("%q несёт %d полей %v, ожидалось РОВНО 1 (items) — пагинационные поля протекли?", name, len(props), got)
	}
	items, ok := props["items"].(map[string]any)
	if !ok {
		t.Fatalf("%q.items отсутствует", name)
	}
	if !schemaTypeHas(items["type"], "array") {
		t.Errorf("%q.items.type=%v, ожидалось array", name, items["type"])
	}
	elemSchema, ok := items["items"].(map[string]any)
	if !ok {
		t.Fatalf("%q.items.items отсутствует (element-схема)", name)
	}
	wantRef := "#/components/schemas/" + element
	if ref, _ := elemSchema["$ref"].(string); ref != wantRef {
		t.Errorf("%q.items.items.$ref=%q, ожидалось %q (контрактный element)", name, ref, wantRef)
	}
}
