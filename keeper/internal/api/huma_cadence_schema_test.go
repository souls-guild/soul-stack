// Доказательный гейт выравнивания имён CADENCE-схем под committed-рукопись (тираж-батч
// N4, по эталону huma_voyage_schema_test.go / huma_herald_schema_test.go). Собирает
// агрегированную huma-спеку (HumaFullSpecYAML) и проверяет, что схемы cadence-домена
// названы ТОЧНО как контракт (docs/keeper/openapi.yaml), а технические huma-Go-имена
// тел/реплик ОТСУТСТВУЮТ.
//
// МЕХАНИЗМЫ для cadence (сверены с рукописью):
//   - REQUEST-RENAME: cadenceCreateHumaBody → CadenceCreateRequest (:7853),
//     cadencePatchHumaBody → CadencePatchRequest (:7973). Применён.
//   - REPLY-RENAME: cadenceCreateReplyHumaBody → CadenceCreateReply (:8051) —
//     контрактное имя 201-тела POST /v1/cadences. Применён.
//   - ENVELOPE (runs): GET /v1/cadences/{id}/runs по рукописи (:2378) ссылается на
//     VoyageListReply (дочерние Voyage переиспользуют Voyage-DTO). handlers.CadenceRunsReply
//     = PagedResponse[voyageDTO] → huma эмитил PagedResponseVoyage; alias generic →
//     VoyageListReply (registerCadenceEnvelopes) сводит runs на ту же named-схему
//     VoyageListReply, что voyage list. Применён.
//   - ENUM-ALIAS: НЕ применяется. Рукопись НЕ объявляет standalone ScheduleKind/
//     OverlapPolicy — оба инлайнятся в CadenceCreateRequest/Cadence (`enum: […]`).
//     Standalone enum-схемы нет → named-схему НЕ создаём.
//   - NESTED (target/notify): сделано в nested-voyage-батче (VoyageTarget/VoyageNotify,
//     shared voyage+cadence). Здесь НЕ трогается; ссылки cadence-input на них проверяет
//     TestSchemaNames_VoyageNested (consumer CadenceCreateRequest, N4-выровнен).
//
// LIST + element (батч N6): GET /v1/cadences — element CadenceDTO→Cadence (:8078) и envelope
// PagedResponseCadenceDTO→CadenceListReply (:8147) выровнены ИМЕНОВАНИЕМ через api-named-struct +
// alias (huma_cadence_envelope.go). ★ target типизирован как $ref VoyageTarget (:8106) ТОЛЬКО в
// схеме: alias подменяет схему, wire-тело cadenceDTO.target=json.RawMessage сериализуется тем же
// путём → golden cadence get/list/patch byte-exact. CadenceDTO/PagedResponseCadenceDTO — теперь
// в forbidden (дрейф вытеснен).
package api

import (
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// cadenceContractSchemas — request/reply-имена cadence-домена ровно как в committed-
// рукописи. Все обязаны присутствовать в собранной спеке.
var cadenceContractSchemas = []string{
	"CadenceCreateRequest",
	"CadencePatchRequest",
	"CadenceCreateReply",
	// runs-envelope сведён на VoyageListReply (рукопись :2378); сам VoyageListReply
	// присутствует от voyage-домена — здесь фиксируем как контрактный для runs.
	"VoyageListReply",
	// LIST + element (батч N6).
	"Cadence",
	"CadenceListReply",
}

// cadenceForbiddenSchemas — технические huma-Go-имена выровненных тел/реплик + generic-
// envelope-ы. Ни одно не должно остаться.
var cadenceForbiddenSchemas = []string{
	"CadenceCreateHumaBody",
	"CadencePatchHumaBody",
	"CadenceCreateReplyHumaBody",
	"PagedResponseVoyage",     // runs-envelope сведён на VoyageListReply
	"CadenceDTO",              // → Cadence (батч N6)
	"PagedResponseCadenceDTO", // → CadenceListReply (батч N6)
}

// TestSchemaNames_Cadence — гейт N4. Контрактные имена присутствуют, технические — нет.
func TestSchemaNames_Cadence(t *testing.T) {
	schemas := loadFullSpecSchemas(t)
	for _, name := range cadenceContractSchemas {
		if _, ok := schemas[name]; !ok {
			t.Errorf("контрактная схема %q ОТСУТСТВУЕТ в components/schemas (имя не выровнено)", name)
		}
	}
	for _, name := range cadenceForbiddenSchemas {
		if _, ok := schemas[name]; ok {
			t.Errorf("техническое huma-имя %q ПРИСУТСТВУЕТ в спеке — имя не выровнено под контракт", name)
		}
	}
}

// TestSchemaNames_CadenceCreateRequestShape — форма CadenceCreateRequest сверена с
// рукописью (:7853): required name/schedule_kind/overlap_policy/kind/target; target —
// $ref на VoyageTarget (nested-выравнивание); notify[] — $ref на VoyageNotify. Мутация
// (потеря required / рассыпание target на per-домен тип) краснит.
func TestSchemaNames_CadenceCreateRequestShape(t *testing.T) {
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

	req, _ := schemas["CadenceCreateRequest"].(map[string]any)
	if req == nil {
		t.Fatal("CadenceCreateRequest отсутствует в components.schemas")
	}
	assertRequiredExactly(t, req, "CadenceCreateRequest",
		"name", "schedule_kind", "overlap_policy", "kind", "target")

	const targetRef = "#/components/schemas/VoyageTarget"
	if got := propRef(t, schemas, "CadenceCreateRequest", "target"); got != targetRef {
		t.Errorf("CadenceCreateRequest.target → %q, ожидался %q", got, targetRef)
	}
	const notifyRef = "#/components/schemas/VoyageNotify"
	if got := propItemsRef(t, schemas, "CadenceCreateRequest", "notify"); got != notifyRef {
		t.Errorf("CadenceCreateRequest.notify[] → %q, ожидался %q", got, notifyRef)
	}
}

// TestSchemaNames_CadenceRunsEnvelope — runs-response (GET /v1/cadences/{id}/runs) сведён
// на named-схему VoyageListReply с контрактной 4-поля-offset формой (items.$ref на Voyage;
// БЕЗ cursor-полей). Format-agnostic (рукопись plain `integer`). Мутация (cursor-протечка /
// item-only / неверный $ref) краснит — гарантирует, что generic PagedResponseVoyage не
// вернулся.
func TestSchemaNames_CadenceRunsEnvelope(t *testing.T) {
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
	assertOffsetEnvelopeNoFormat(t, schemas, "VoyageListReply", "Voyage")
}

// TestSchemaNames_CadenceListEnvelope — гейт N6 (LIST + element). GET /v1/cadences сведён на
// named-схему CadenceListReply (4-поля offset; items.$ref на element Cadence). Element Cadence
// несёт required-набор рукописи (:8145) и target=$ref VoyageTarget (:8106 — типизирован, НЕ
// free-form `{}`). Мутация (free-form target / item-only / неверный required-набор) краснит —
// гарантирует, что generic PagedResponseCadenceDTO/CadenceDTO не вернулись.
func TestSchemaNames_CadenceListEnvelope(t *testing.T) {
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

	assertOffsetEnvelopeNoFormat(t, schemas, "CadenceListReply", "Cadence")

	cad, _ := schemas["Cadence"].(map[string]any)
	if cad == nil {
		t.Fatal("Cadence отсутствует в components.schemas — element-alias не сработал")
	}
	assertRequiredExactly(t, cad, "Cadence",
		"cadence_id", "name", "enabled", "schedule_kind", "overlap_policy",
		"kind", "created_by_aid", "created_at", "updated_at")

	const targetRef = "#/components/schemas/VoyageTarget"
	if got := propRef(t, schemas, "Cadence", "target"); got != targetRef {
		t.Errorf("Cadence.target → %q, ожидался %q (target не типизирован на VoyageTarget — alias не сработал / free-form остался)", got, targetRef)
	}
}
