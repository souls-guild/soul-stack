// Доказательный гейт выравнивания имён CHOIR/VOICE-схем под committed-рукопись (тираж-
// батч N4, по эталону huma_herald_schema_test.go). Собирает агрегированную huma-спеку
// (HumaFullSpecYAML) и проверяет контрактные имена + отсутствие технических huma-Go-имён.
//
// МЕХАНИЗМЫ для choir (сверены с рукописью):
//   - REQUEST-RENAME: choirCreateHumaBody → ChoirCreateRequest (:6123, class C input-
//     only); voiceAddHumaBody → VoiceAddRequest (:6143, class C input-only). Применён.
//   - ENVELOPE: ChoirListReply/VoiceListReply УЖЕ named oapi-типы (ChoirListReply/
//     VoiceListReply — генерёные struct, НЕ generic PagedResponse) → DefaultSchemaNamer
//     даёт контрактное имя сам; alias не нужен. СВЕРЯЕМ форму (4-поля-offset, items.$ref
//     на Choir/Voice).
//   - ENUM-ALIAS / NESTED: не применяются (рукопись standalone enum/shared-nested для
//     choir НЕ объявляет).
package api

import (
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// choirContractSchemas — request/view/envelope-имена choir+voice-домена ровно как в
// committed-рукописи. Все обязаны присутствовать в собранной спеке.
var choirContractSchemas = []string{
	"ChoirCreateRequest",
	"VoiceAddRequest",
	"Choir",
	"Voice",
	"ChoirListReply",
	"VoiceListReply",
}

// choirForbiddenSchemas — технические huma-Go-имена старых input-тел. Ни одно не должно
// остаться в спеке после выравнивания.
var choirForbiddenSchemas = []string{
	"ChoirCreateHumaBody",
	"VoiceAddHumaBody",
}

// TestSchemaNames_Choir — гейт N4. Контрактные имена присутствуют, технические — нет.
func TestSchemaNames_Choir(t *testing.T) {
	schemas := loadFullSpecSchemas(t)
	for _, name := range choirContractSchemas {
		if _, ok := schemas[name]; !ok {
			t.Errorf("контрактная схема %q ОТСУТСТВУЕТ в components/schemas (имя не выровнено)", name)
		}
	}
	for _, name := range choirForbiddenSchemas {
		if _, ok := schemas[name]; ok {
			t.Errorf("техническое huma-имя %q ПРИСУТСТВУЕТ в спеке — имя не выровнено под контракт", name)
		}
	}
}

// TestSchemaNames_ChoirEnvelopes — оба envelope несут КОНТРАКТНУЮ 4-поля-offset форму
// (items/offset/limit/total; items.$ref на Choir/Voice). Format-agnostic (рукопись plain
// `integer`). Мутация (cursor-протечка / item-only / неверный $ref) краснит.
func TestSchemaNames_ChoirEnvelopes(t *testing.T) {
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
	assertOffsetEnvelopeNoFormat(t, schemas, "ChoirListReply", "Choir")
	assertOffsetEnvelopeNoFormat(t, schemas, "VoiceListReply", "Voice")
}

// TestSchemaNames_ChoirRequestShapes — формы request-тел сверены с рукописью:
// ChoirCreateRequest.required=[choir_name] (:6141); VoiceAddRequest.required=[sid] (:6159).
func TestSchemaNames_ChoirRequestShapes(t *testing.T) {
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

	cc, _ := schemas["ChoirCreateRequest"].(map[string]any)
	if cc == nil {
		t.Fatal("ChoirCreateRequest отсутствует")
	}
	assertRequiredExactly(t, cc, "ChoirCreateRequest", "choir_name")

	va, _ := schemas["VoiceAddRequest"].(map[string]any)
	if va == nil {
		t.Fatal("VoiceAddRequest отсутствует")
	}
	assertRequiredExactly(t, va, "VoiceAddRequest", "sid")
}
