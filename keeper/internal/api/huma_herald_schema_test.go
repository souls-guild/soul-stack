// Доказательный гейт выравнивания имён HERALD-схем (heralds + tidings — multi-resource,
// один HeraldHandler) под committed-рукопись (тираж-батч N2, по эталону huma_operator_
// schema_test.go). Собирает агрегированную huma-спеку (HumaFullSpecYAML) и проверяет,
// что схемы herald/tiding-домена названы ТОЧНО как контракт (docs/keeper/openapi.yaml),
// а технические huma-Go-имена (HeraldCreateHumaBody / TidingUpdateHumaBody) в спеке
// ОТСУТСТВУЮТ. Форма обоих envelope сверена с рукописью: HeraldListReply и TidingListReply —
// 4-поля-offset (items/offset/limit/total).
//
// ВНИМАНИЕ к формату offset/limit/total: рукопись объявляет их как `type: integer` БЕЗ
// `format` (в отличие от augur/oracle, где рукопись несёт явный int32). Сгенерированные
// HeraldListReply/TidingListReply используют Go-`int` → huma эмитит format int64.
// Это согласуется с рукописью (плоский `integer` покрывает int64) и НЕ противоречит
// контракту. Поэтому envelope-assert здесь format-agnostic (assertOffsetEnvelopeNoFormat),
// а не строгий assertEnvelopeShape (тот пинит int32, верный лишь для augur/oracle).
package api

import (
	"sort"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// heraldContractSchemas — request/view/envelope-имена herald+tiding-домена ровно как в
// committed-рукописи. Все обязаны присутствовать в собранной спеке.
var heraldContractSchemas = []string{
	"HeraldCreateRequest",
	"HeraldUpdateRequest",
	"TidingCreateRequest",
	"TidingUpdateRequest",
	"Herald",
	"Tiding",
	"HeraldListReply",
	"TidingListReply",
}

// heraldForbiddenSchemas — технические huma-Go-имена старых структур. Ни одно не должно
// остаться в спеке после выравнивания. Enum type инлайнится в Herald*Request (рукопись
// НЕ выносит standalone enum-схему) — отдельного forbidden-имени нет.
var heraldForbiddenSchemas = []string{
	"HeraldCreateHumaBody",
	"HeraldUpdateHumaBody",
	"TidingCreateHumaBody",
	"TidingUpdateHumaBody",
}

// TestSchemaNames_Herald — гейт N2. Контрактные имена присутствуют, технические — нет.
func TestSchemaNames_Herald(t *testing.T) {
	schemas := loadFullSpecSchemas(t)
	for _, name := range heraldContractSchemas {
		if _, ok := schemas[name]; !ok {
			t.Errorf("контрактная схема %q ОТСУТСТВУЕТ в components/schemas (имя не выровнено)", name)
		}
	}
	for _, name := range heraldForbiddenSchemas {
		if _, ok := schemas[name]; ok {
			t.Errorf("техническое huma-имя %q ПРИСУТСТВУЕТ в спеке — имя не выровнено под контракт", name)
		}
	}
}

// TestSchemaNames_HeraldEnvelopes — гейт N2 (ENVELOPE). Оба envelope несут КОНТРАКТНУЮ
// 4-поля-offset форму (items/offset/limit/total; items.$ref на контрактный element).
// Format-agnostic: рукопись HeraldListReply/TidingListReply несёт plain `integer` без
// явного int32 (см. шапку файла). Мутация (item-only / cursor-поля / неверный $ref)
// краснит.
func TestSchemaNames_HeraldEnvelopes(t *testing.T) {
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
	assertOffsetEnvelopeNoFormat(t, schemas, "HeraldListReply", "Herald")
	assertOffsetEnvelopeNoFormat(t, schemas, "TidingListReply", "Tiding")
}

// assertOffsetEnvelopeNoFormat — вариант assertEnvelopeShape для envelope, где рукопись
// объявляет offset/limit/total как plain `integer` БЕЗ `format` (Herald/Tiding):
// проверяет РОВНО 4 поля (items/offset/limit/total, БЕЗ cursor-полей), offset/limit/total
// — integer-тип (format не пинится), items — массив с $ref на контрактный element.
func assertOffsetEnvelopeNoFormat(t *testing.T, schemas map[string]any, name, element string) {
	t.Helper()

	env, ok := schemas[name].(map[string]any)
	if !ok {
		t.Fatalf("envelope-схема %q отсутствует в components/schemas", name)
	}
	props, ok := env["properties"].(map[string]any)
	if !ok {
		t.Fatalf("%q без properties", name)
	}

	wantFields := map[string]struct{}{"items": {}, "offset": {}, "limit": {}, "total": {}}
	if len(props) != len(wantFields) {
		var got []string
		for k := range props {
			got = append(got, k)
		}
		sort.Strings(got)
		t.Errorf("%q несёт %d полей %v, ожидалось ровно 4 (items/offset/limit/total) — cursor-поля протекли?", name, len(props), got)
	}
	for f := range wantFields {
		if _, ok := props[f]; !ok {
			t.Errorf("%q не содержит контрактного поля %q", name, f)
		}
	}

	for _, f := range []string{"offset", "limit", "total"} {
		fp, ok := props[f].(map[string]any)
		if !ok {
			continue
		}
		if !schemaTypeHas(fp["type"], "integer") {
			t.Errorf("%q.%s.type=%v, ожидалось integer", name, f, fp["type"])
		}
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
