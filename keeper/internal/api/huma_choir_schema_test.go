// Proof gate: alignment of CHOIR/VOICE schema names to the committed hand-written spec
// (rollout batch N4, following huma_herald_schema_test.go). Assembles the aggregated huma
// spec (HumaFullSpecYAML) and checks contract names + absence of technical huma-Go names.
//
// MECHANISMS for choir (verified against the hand-written spec):
//   - REQUEST-RENAME: choirCreateHumaBody → ChoirCreateRequest (:6123, class C input-
//     only); voiceAddHumaBody → VoiceAddRequest (:6143, class C input-only). Applied.
//   - ENVELOPE: ChoirListReply/VoiceListReply are ALREADY named oapi types (generated
//     structs, NOT generic PagedResponse) → DefaultSchemaNamer yields the contract name
//     itself; no alias needed. We VERIFY the shape (4-field offset, items.$ref to
//     Choir/Voice).
//   - ENUM-ALIAS / NESTED: not applied (the hand-written spec declares no standalone
//     enum/shared-nested for choir).
package api

import (
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// choirContractSchemas — request/view/envelope names of the choir+voice domain exactly as
// in the committed hand-written spec. All must be present in the assembled spec.
var choirContractSchemas = []string{
	"ChoirCreateRequest",
	"VoiceAddRequest",
	"Choir",
	"Voice",
	"ChoirListReply",
	"VoiceListReply",
}

// choirForbiddenSchemas — technical huma-Go names of the old input bodies. None may remain
// in the spec after alignment.
var choirForbiddenSchemas = []string{
	"ChoirCreateHumaBody",
	"VoiceAddHumaBody",
}

// TestSchemaNames_Choir — gate N4. Contract names present, technical ones absent.
func TestSchemaNames_Choir(t *testing.T) {
	schemas := loadFullSpecSchemas(t)
	for _, name := range choirContractSchemas {
		if _, ok := schemas[name]; !ok {
			t.Errorf("контрактonя схема %q ОТСУТСТВУЕТ в components/schemas (имя не выровнеbut)", name)
		}
	}
	for _, name := range choirForbiddenSchemas {
		if _, ok := schemas[name]; ok {
			t.Errorf("техническое huma-имя %q ПРИСУТСТВУЕТ в спеке — имя не выровнеbut под контракт", name)
		}
	}
}

// TestSchemaNames_ChoirEnvelopes — both envelopes carry the CONTRACT 4-field offset shape
// (items/offset/limit/total; items.$ref to Choir/Voice). Format-agnostic (hand-written spec
// uses plain `integer`). A mutation (cursor leak / item-only / wrong $ref) turns it red.
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

// TestSchemaNames_ChoirRequestShapes — request-body shapes verified against the hand-written
// spec: ChoirCreateRequest.required=[choir_name] (:6141); VoiceAddRequest.required=[sid] (:6159).
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
