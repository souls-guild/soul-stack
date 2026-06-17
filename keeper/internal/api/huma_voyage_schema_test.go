// Доказательный гейт выравнивания имён VOYAGE-схем под committed-рукопись (тираж-батч N3,
// по эталону huma_operator_schema_test.go). Собирает агрегированную huma-спеку
// (HumaFullSpecYAML) и проверяет, что схемы voyage-домена названы ТОЧНО как контракт
// (docs/keeper/openapi.yaml), а техническое huma-Go-имя VoyageCreateHumaBody ОТСУТСТВУЕТ.
//
// МЕХАНИЗМЫ для voyage (сверены с рукописью):
//   - REQUEST-RENAME: voyageCreateHumaBody → VoyageCreateRequest (контрактное имя тела
//     POST /v1/voyages; то же тело — preview). Применён.
//   - ENUM-ALIAS: НЕ применяется. Рукопись НЕ объявляет standalone VoyageKind/VoyageStatus
//     в components/schemas — kind инлайнится в VoyageCreateRequest (`enum: [scenario,
//     command]`), статусы — в Voyage/VoyageTargetEntry. Standalone enum-схемы нет → named-
//     схему НЕ создаём.
//   - ENVELOPE: уже named oapi-тип (VoyageListReply — генерёная struct, НЕ generic
//     PagedResponse) → DefaultSchemaNamer даёт контрактное имя сам; alias не нужен.
//     СВЕРЯЕМ форму: 4-поля-offset plain `integer`, items.$ref на Voyage.
//
// NESTED-ВЫРАВНИВАНИЕ (target/notify): вложенные shared-формы сведены в ЕДИНЫЕ Go-типы
// api.VoyageTarget/api.VoyageNotify (huma_voyage_target.go), shared voyage+cadence. В спеке
// — ровно ОДНА VoyageTarget (input voyage+cadence + output voyage, единый native-тип)
// и ОДНА VoyageNotify (input voyage+cadence, без output);
// технические VoyageTargetHumaBody/CadenceTargetHumaBody/VoyageNotifyHumaBody/CadenceNotify-
// HumaBody ОТСУТСТВУЮТ. Гейт — TestSchemaNames_VoyageNested.
package api

import (
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// voyageContractSchemas — request/view/envelope-имена voyage-домена ровно как в committed-
// рукописи. Все обязаны присутствовать в собранной спеке.
var voyageContractSchemas = []string{
	"VoyageCreateRequest",
	"Voyage",
	"VoyageListReply",
	// Вложенные shared-формы (одна на оба домена, рукопись :7455/:7612).
	"VoyageTarget",
	"VoyageNotify",
}

// voyageForbiddenSchemas — техническое huma-Go-имя старой структуры тела. Не должно
// остаться. Standalone-enum (VoyageKind/VoyageStatus) рукопись НЕ объявляет — отдельного
// forbidden-имени для них нет (kind/status инлайнятся).
var voyageForbiddenSchemas = []string{
	"VoyageCreateHumaBody",
	// Технические huma-Go-имена схлопнутых вложенных форм — после nested-выравнивания
	// их нет (одна VoyageTarget/VoyageNotify на оба домена).
	"VoyageTargetHumaBody",
	"CadenceTargetHumaBody",
	"VoyageNotifyHumaBody",
	"CadenceNotifyHumaBody",
}

// TestSchemaNames_Voyage — гейт N3. Контрактные имена присутствуют, техническое — нет.
func TestSchemaNames_Voyage(t *testing.T) {
	schemas := loadFullSpecSchemas(t)
	for _, name := range voyageContractSchemas {
		if _, ok := schemas[name]; !ok {
			t.Errorf("контрактная схема %q ОТСУТСТВУЕТ в components/schemas (имя не выровнено)", name)
		}
	}
	for _, name := range voyageForbiddenSchemas {
		if _, ok := schemas[name]; ok {
			t.Errorf("техническое huma-имя %q ПРИСУТСТВУЕТ в спеке — имя не выровнено под контракт", name)
		}
	}
}

// TestSchemaNames_VoyageEnvelope — гейт N3 (ENVELOPE). VoyageListReply несёт КОНТРАКТНУЮ
// 4-поля-offset форму (items/offset/limit/total; items.$ref на Voyage). Format-agnostic
// (рукопись — plain `integer`). Мутация (item-only/cursor/неверный $ref) краснит.
func TestSchemaNames_VoyageEnvelope(t *testing.T) {
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

// voyageTargetConsumers — схемы, поле `target` которых ОБЯЗАНО ссылаться на
// #/components/schemas/VoyageTarget. Покрывает input(voyage+cadence) И output(voyage).
// Имя cadence input-тела выровнено под рукопись (CadenceCreateRequest, N4).
//
// CadenceDTO (output cadence) В СПИСКЕ НЕТ: его поле target — pre-existing json.RawMessage
// (free-form `{}` в спеке, не $ref) ещё ДО этого выравнивания; типизация cadence output-
// target под VoyageTarget (и связанный rename CadenceDTO→Cadence) — отдельная задача, не
// nested-схлопывание technical-дублей (см. N4-отчёт: blocked для N5).
var voyageTargetConsumers = []string{
	"VoyageCreateRequest",  // input voyage (create+preview — одна схема)
	"CadenceCreateRequest", // input cadence (N4)
	"Voyage",               // output voyage (через alias VoyageTarget→VoyageTarget)
}

// voyageNotifyConsumers — схемы, поле `notify[]` которых ОБЯЗАНО ссылаться на VoyageNotify
// (класс B — только input, output-потребителя нет).
var voyageNotifyConsumers = []string{
	"VoyageCreateRequest",
	"CadenceCreateRequest", // N4
}

// TestSchemaNames_VoyageNested — гейт NESTED-выравнивания. Доказывает:
//   - ровно ОДНА VoyageTarget и ОДНА VoyageNotify в агрегатор-спеке (ключ схемы уникален —
//     присутствие именно контрактного имени проверяет TestSchemaNames_Voyage; здесь —
//     ССЫЛКИ на него от всех потребителей);
//   - input(voyage+cadence) И output(voyage) ссылаются на VoyageTarget; input(voyage+
//     cadence) ссылается на VoyageNotify;
//   - технические имена (Voyage/CadenceTargetHumaBody, …) отсутствуют (voyageForbidden-
//     Schemas в TestSchemaNames_Voyage);
//   - форма VoyageTarget/VoyageNotify сверена с рукописью (required-набор).
//
// Мутация (рассыпать target на per-домен тип / убрать alias / сменить required-набор) краснит.
func TestSchemaNames_VoyageNested(t *testing.T) {
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

	const targetRef = "#/components/schemas/VoyageTarget"
	const notifyRef = "#/components/schemas/VoyageNotify"

	// (1) VoyageTarget — потребители ссылаются на единую схему.
	for _, name := range voyageTargetConsumers {
		if got := propRef(t, schemas, name, "target"); got != targetRef {
			t.Errorf("%s.target → %q, ожидался %q (target не сведён на единую VoyageTarget)", name, got, targetRef)
		}
	}
	// (2) VoyageNotify — input-потребители ссылаются на единую схему (notify — массив).
	for _, name := range voyageNotifyConsumers {
		if got := propItemsRef(t, schemas, name, "notify"); got != notifyRef {
			t.Errorf("%s.notify[] → %q, ожидался %q (notify не сведён на единую VoyageNotify)", name, got, notifyRef)
		}
	}

	// (3) Форма VoyageTarget сверена с рукописью (:7455 — required НЕТ; 5 optional-полей).
	tgt, _ := schemas["VoyageTarget"].(map[string]any)
	if tgt == nil {
		t.Fatal("VoyageTarget отсутствует в components.schemas")
	}
	if req, ok := tgt["required"]; ok {
		t.Errorf("VoyageTarget.required=%v — рукопись :7455 НЕ объявляет required (все поля optional)", req)
	}
	assertProps(t, tgt, "VoyageTarget", "incarnations", "service", "sids", "where", "coven")

	// (4) Форма VoyageNotify сверена с рукописью (:7612 — required:[herald]).
	ntf, _ := schemas["VoyageNotify"].(map[string]any)
	if ntf == nil {
		t.Fatal("VoyageNotify отсутствует в components.schemas")
	}
	assertRequiredExactly(t, ntf, "VoyageNotify", "herald")
	assertProps(t, ntf, "VoyageNotify", "herald", "on", "only_failures", "only_changes", "annotations", "projection")
}

// propRef достаёт $ref у скалярного поля name схемы schemaName (target: {$ref: …}).
func propRef(t *testing.T, schemas map[string]any, schemaName, field string) string {
	t.Helper()
	prop := schemaProp(t, schemas, schemaName, field)
	ref, _ := prop["$ref"].(string)
	return ref
}

// propItemsRef достаёт $ref у элементов массива-поля field (notify: {items: {$ref: …}}).
func propItemsRef(t *testing.T, schemas map[string]any, schemaName, field string) string {
	t.Helper()
	prop := schemaProp(t, schemas, schemaName, field)
	items, _ := prop["items"].(map[string]any)
	if items == nil {
		t.Errorf("%s.%s — не массив (items отсутствует)", schemaName, field)
		return ""
	}
	ref, _ := items["$ref"].(string)
	return ref
}

// schemaProp достаёт map поля field из properties схемы schemaName.
func schemaProp(t *testing.T, schemas map[string]any, schemaName, field string) map[string]any {
	t.Helper()
	sch, _ := schemas[schemaName].(map[string]any)
	if sch == nil {
		t.Fatalf("схема %q отсутствует", schemaName)
	}
	props, _ := sch["properties"].(map[string]any)
	prop, _ := props[field].(map[string]any)
	if prop == nil {
		t.Fatalf("%s.%s отсутствует в properties", schemaName, field)
	}
	return prop
}

// assertProps проверяет, что в properties схемы присутствует ровно ожидаемый набор полей.
func assertProps(t *testing.T, sch map[string]any, name string, want ...string) {
	t.Helper()
	props, _ := sch["properties"].(map[string]any)
	if len(props) != len(want) {
		t.Errorf("%s: %d полей, ожидалось %d (%v)", name, len(props), len(want), want)
	}
	for _, f := range want {
		if _, ok := props[f]; !ok {
			t.Errorf("%s: поле %q отсутствует", name, f)
		}
	}
}

// assertRequiredExactly проверяет, что required схемы — ровно ожидаемый набор.
func assertRequiredExactly(t *testing.T, sch map[string]any, name string, want ...string) {
	t.Helper()
	raw, _ := sch["required"].([]any)
	got := map[string]bool{}
	for _, r := range raw {
		if s, ok := r.(string); ok {
			got[s] = true
		}
	}
	if len(got) != len(want) {
		t.Errorf("%s.required=%v, ожидалось %v", name, raw, want)
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("%s.required не содержит %q", name, w)
		}
	}
}
