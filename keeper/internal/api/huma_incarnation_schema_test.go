// Доказательный гейт выравнивания имён incarnation-схем под committed-рукопись
// (T4b pilot). Собирает агрегированную huma-спеку ([HumaFullSpecYAML]) и проверяет, что
// схемы incarnation-домена названы ТОЧНО как контракт (docs/keeper/openapi.yaml), а
// технические huma-Go-имена (IncCreateHumaBody и пр.) в спеке ОТСУТСТВУЮТ. Мутация
// (вернуть старое имя структуры) краснит этот тест.
package api

import (
	"sort"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// incarnationContractSchemas — request/reply/view-имена incarnation-домена ровно как в
// committed-рукописи (docs/keeper/openapi.yaml). Все обязаны присутствовать в собранной
// спеке как components/schemas.
var incarnationContractSchemas = []string{
	"IncarnationCreateRequest",
	"IncarnationCreateReply",
	"IncarnationRunRequest",
	"IncarnationRunReply",
	"IncarnationUnlockRequest",
	"IncarnationUnlockReply",
	"IncarnationUpgradeRequest",
	"IncarnationUpgradeReply",
	"IncarnationRerunCreateRequest",
	"IncarnationRerunCreateReply",
	"IncarnationCheckDriftRequest",
	"IncarnationUpdateHostsRequest",
	"IncarnationSpecHost",
	"IncarnationGetReply",
	"IncarnationDestroyReply",
	"IncarnationStatus",
	"IncarnationListReply",
	"IncarnationHistoryReply",
}

// incarnationForbiddenSchemas — технические huma-Go-имена, которые DefaultSchemaNamer
// дал БЫ из старых имён структур (incCreateHumaBody → IncCreateHumaBody). Ни одно не
// должно остаться в спеке после выравнивания.
var incarnationForbiddenSchemas = []string{
	"IncCreateHumaBody",
	"IncRunHumaBody",
	"IncUnlockHumaBody",
	"IncUpgradeHumaBody",
	"IncRerunHumaBody",
	"IncCheckDriftHumaBody",
	"IncUpdateHostsHumaBody",
	"IncHostHumaBody",
	// Generic-имена, которые DefaultSchemaNamer дал БЫ из неаласенного
	// sharedapi.PagedResponse[T] (скобки generic схлопываются в конкатенацию):
	// envelope-alias обязан их вытеснить контрактными именами.
	"PagedResponseIncarnationGetReply",
	"PagedResponseStateHistoryEntry",
}

// TestSchemaNames_Incarnation — гейт T4b. Доказывает, что собранная агрегатор-спека
// несёт incarnation-схемы под КОНТРАКТНЫМИ именами и не несёт технических huma-имён.
func TestSchemaNames_Incarnation(t *testing.T) {
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
	schemas := doc.Components.Schemas

	for _, name := range incarnationContractSchemas {
		if _, ok := schemas[name]; !ok {
			t.Errorf("контрактная схема %q ОТСУТСТВУЕТ в components/schemas (имя не выровнено)", name)
		}
	}
	for _, name := range incarnationForbiddenSchemas {
		if _, ok := schemas[name]; ok {
			t.Errorf("техническое huma-имя %q ПРИСУТСТВУЕТ в спеке — имя не выровнено под контракт", name)
		}
	}
}

// TestSchemaNames_IncarnationStatusEnum — гейт T4b (enum). IncarnationStatus вынесен как
// named-схема с enum-значениями (а не инлайн `type: string`), а status-поля reply-
// структур ссылаются на неё через $ref (как в рукописи 3.0.3).
func TestSchemaNames_IncarnationStatusEnum(t *testing.T) {
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

	statusSchema, ok := schemas["IncarnationStatus"].(map[string]any)
	if !ok {
		t.Fatal("IncarnationStatus не вынесен как named-схема в components/schemas")
	}
	if typ, _ := statusSchema["type"].(string); typ != "string" {
		t.Errorf("IncarnationStatus.type=%q, ожидалось string", typ)
	}
	rawEnum, ok := statusSchema["enum"].([]any)
	if !ok || len(rawEnum) == 0 {
		t.Fatal("IncarnationStatus без enum — выноса как enum не произошло")
	}
	got := map[string]struct{}{}
	for _, v := range rawEnum {
		if s, ok := v.(string); ok {
			got[s] = struct{}{}
		}
	}
	// Контракт-минимум: ключевые статусы из рукописи присутствуют.
	for _, want := range []string{"provisioning", "ready", "applying", "error_locked", "migration_failed", "drift", "destroying", "destroy_failed"} {
		if _, ok := got[want]; !ok {
			var have []string
			for k := range got {
				have = append(have, k)
			}
			sort.Strings(have)
			t.Errorf("enum IncarnationStatus не содержит %q; есть: %v", want, have)
		}
	}

	// status-поля ссылаются на named-схему через $ref (не инлайн).
	if !strings.Contains(y, "#/components/schemas/IncarnationStatus") {
		t.Error("ни одно поле не ссылается на IncarnationStatus через $ref — статус остался инлайн")
	}
}

// TestSchemaNames_IncarnationEnvelope — гейт T4b (ENVELOPE). list/history-envelope
// вынесены как named-схемы под КОНТРАКТНЫМИ именами (IncarnationListReply/Incarnation-
// HistoryReply) с КОНТРАКТНОЙ формой (ровно 4 поля int32 items/offset/limit/total БЕЗ
// cursor-полей; items.$ref на контрактный element). Мутация (убрать envelope-alias)
// краснит: huma эмитит generic-имя PagedResponse* — оба assert-блока падают.
func TestSchemaNames_IncarnationEnvelope(t *testing.T) {
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

	cases := []struct {
		schema  string // контрактное имя envelope-схемы
		element string // контрактное имя element-схемы, на которую ссылается items.$ref
	}{
		{"IncarnationListReply", "IncarnationGetReply"},
		{"IncarnationHistoryReply", "StateHistoryEntry"},
	}
	for _, c := range cases {
		assertEnvelopeShape(t, schemas, c.schema, c.element)
	}
}

// assertEnvelopeShape проверяет, что envelope-схема несёт РОВНО контрактную форму
// рукописи: 4 поля (items/offset/limit/total), offset/limit/total — int32, items —
// массив с $ref на контрактный element, БЕЗ cursor-полей (next_cursor/total_approximate
// generic PagedResponse[T] не должны протечь — иначе envelope-alias не сработал).
func assertEnvelopeShape(t *testing.T, schemas map[string]any, name, element string) {
	t.Helper()

	env, ok := schemas[name].(map[string]any)
	if !ok {
		t.Fatalf("envelope-схема %q отсутствует в components/schemas — envelope-alias не сработал", name)
	}
	props, ok := env["properties"].(map[string]any)
	if !ok {
		t.Fatalf("%q без properties", name)
	}

	// Ровно 4 поля — cursor-поля (next_cursor/total_approximate) не должны протечь.
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

	// offset/limit/total — integer int32. (huma эмитит OpenAPI 3.1, type может быть
	// строкой "integer" — для не-nullable скаляра; nullable дал бы массив [..,"null"].)
	for _, f := range []string{"offset", "limit", "total"} {
		fp, ok := props[f].(map[string]any)
		if !ok {
			continue
		}
		if !schemaTypeHas(fp["type"], "integer") {
			t.Errorf("%q.%s.type=%v, ожидалось integer", name, f, fp["type"])
		}
		if format, _ := fp["format"].(string); format != "int32" {
			t.Errorf("%q.%s.format=%q, ожидалось int32", name, f, format)
		}
	}

	// items — array c $ref на контрактный element. (3.1: slice nullable → type
	// [array,null]; schemaTypeHas принимает обе нотации.)
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

// schemaTypeHas проверяет, что JSON-Schema-узел `type` содержит want. OpenAPI 3.1 (huma-
// дефолт) кодирует type строкой ("integer") для не-nullable либо массивом (["array","null"])
// для nullable — принимаем обе формы.
func schemaTypeHas(raw any, want string) bool {
	switch v := raw.(type) {
	case string:
		return v == want
	case []any:
		for _, e := range v {
			if s, _ := e.(string); s == want {
				return true
			}
		}
	}
	return false
}
