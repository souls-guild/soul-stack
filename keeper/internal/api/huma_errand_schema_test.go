// Доказательный гейт выравнивания имён ERRAND-схем под committed-рукопись (тираж-батч
// N3, по эталону huma_operator_schema_test.go / huma_herald_schema_test.go). Собирает
// агрегированную huma-спеку (HumaFullSpecYAML) и проверяет, что схемы errand-домена
// названы ТОЧНО как контракт (docs/keeper/openapi.yaml).
//
// МЕХАНИЗМЫ для errand (сверены с рукописью):
//   - REQUEST-RENAME: НЕ применяется. list/get/cancel — query/path-only роуты БЕЗ тела
//     запроса; huma-input-структур с Body нет → переименовывать нечего, технических
//     *HumaBody-имён в спеке быть не может.
//   - ENUM-ALIAS: НЕ применяется. Рукопись НЕ объявляет standalone ErrandStatus в
//     components/schemas — статус инлайнится в ErrandResult (`type: string` + enum).
//   - ENVELOPE: уже named oapi-тип (ErrandListReply — генерёная struct, НЕ generic
//     PagedResponse) → DefaultSchemaNamer даёт контрактное имя сам; alias-механизм не
//     нужен. Здесь лишь СВЕРЯЕМ форму: 4-поля-offset с plain `integer` (рукопись несёт
//     `type: integer` БЕЗ format → format-agnostic assert; ErrandListReply на Go-int
//     → huma эмитит int64). items.$ref на контрактный ErrandResult.
package api

import (
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// errandContractSchemas — envelope/element-имена errand-домена ровно как в committed-
// рукописи. Все обязаны присутствовать в собранной спеке.
var errandContractSchemas = []string{
	"ErrandListReply",
	"ErrandResult",
	// Class C доэмиссия: ErrandAccepted (202-тело async-escalation exec / errand-get
	// running) маршалится через json.RawMessage → НЕ типизирован ссылающимся huma-полем
	// → схема не эмитилась. Pre-seed schema-builder (huma_errand_accepted.go) кладёт её.
	"ErrandAccepted",
}

// TestSchemaNames_Errand — гейт N3. Контрактные имена envelope/element присутствуют.
// (forbidden-набор пуст: у errand нет request-тел → технических *HumaBody-имён не бывает.)
func TestSchemaNames_Errand(t *testing.T) {
	schemas := loadFullSpecSchemas(t)
	for _, name := range errandContractSchemas {
		if _, ok := schemas[name]; !ok {
			t.Errorf("контрактная схема %q ОТСУТСТВУЕТ в components/schemas (имя не выровнено)", name)
		}
	}
}

// TestSchemaNames_ErrandEnvelope — гейт N3 (ENVELOPE). ErrandListReply несёт КОНТРАКТНУЮ
// 4-поля-offset форму (items/offset/limit/total; items.$ref на ErrandResult). Format-
// agnostic: рукопись объявляет offset/limit/total как plain `integer` без явного int32.
// Мутация (item-only / cursor-поля / неверный $ref) краснит.
func TestSchemaNames_ErrandEnvelope(t *testing.T) {
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
	assertOffsetEnvelopeNoFormat(t, schemas, "ErrandListReply", "ErrandResult")
}

// TestSchemaNames_ErrandAccepted — гейт Class C (доэмиссия 202-тела). ErrandAccepted
// присутствует в спеке с контрактной формой (рукопись :7363): errand_id + status
// (enum [running]); required:[errand_id, status]. Мутация (убрать registerErrandAccepted →
// схема исчезает, dual-status 202 без типизированного тела) краснит.
func TestSchemaNames_ErrandAccepted(t *testing.T) {
	_, doc := loadFullSpecDoc(t)
	comp, _ := doc["components"].(map[string]any)
	schemas, _ := comp["schemas"].(map[string]any)

	acc, ok := schemas["ErrandAccepted"].(map[string]any)
	if !ok {
		t.Fatal("ErrandAccepted отсутствует в components/schemas — pre-seed не сработал")
	}
	assertRequiredExactly(t, acc, "ErrandAccepted", "errand_id", "status")
	assertProps(t, acc, "ErrandAccepted", "errand_id", "status")

	// status — string-enum ровно [running] (контракт :7372).
	props, _ := acc["properties"].(map[string]any)
	status, _ := props["status"].(map[string]any)
	if status == nil {
		t.Fatal("ErrandAccepted.status отсутствует")
	}
	rawEnum, _ := status["enum"].([]any)
	if len(rawEnum) != 1 || rawEnum[0] != "running" {
		t.Errorf("ErrandAccepted.status.enum=%v, ожидался ровно [running]", rawEnum)
	}
}
