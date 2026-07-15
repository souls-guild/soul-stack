// Evidence gate for the alignment of the HERALD schema names (heralds + tidings —
// multi-resource, one HeraldHandler) with the committed hand-written spec (rollout batch N2,
// modeled on huma_operator_schema_test.go). Assembles the aggregated huma spec
// (HumaFullSpecYAML) and checks that the herald/tiding-domain schemas are named EXACTLY like
// the contract (docs/keeper/openapi.yaml), while the technical huma Go names
// (HeraldCreateHumaBody / TidingUpdateHumaBody) are ABSENT from the spec. The shape of both
// envelopes is checked against the hand-written spec: HeraldListReply and TidingListReply —
// 4-field-offset (items/offset/limit/total).
//
// NOTE on the offset/limit/total format: the hand-written spec declares them as
// `type: integer` WITHOUT `format` (unlike augur/oracle, where it carries an explicit int32).
// The generated HeraldListReply/TidingListReply use Go `int` → huma emits format int64. This
// agrees with the hand-written spec (a plain `integer` covers int64) and does NOT contradict
// the contract. So the envelope assert here is format-agnostic (assertOffsetEnvelopeNoFormat),
// not the strict assertEnvelopeShape (which pins int32, correct only for augur/oracle).
package api

import (
	"sort"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// heraldContractSchemas — the request/view/envelope names of the herald+tiding domain
// exactly as in the committed hand-written spec. All must be present in the assembled spec.
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

// heraldForbiddenSchemas — the technical huma Go names of the old structs. None must remain
// in the spec after alignment. The enum type is inlined into Herald*Request (the hand-written
// spec does NOT emit a standalone enum schema) — there is no separate forbidden name.
var heraldForbiddenSchemas = []string{
	"HeraldCreateHumaBody",
	"HeraldUpdateHumaBody",
	"TidingCreateHumaBody",
	"TidingUpdateHumaBody",
}

// TestSchemaNames_Herald — gate N2. Contract names present, technical ones absent.
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

// TestSchemaNames_HeraldEnvelopes — gate N2 (ENVELOPE). Both envelopes carry the CONTRACT
// 4-field-offset shape (items/offset/limit/total; items.$ref to the contract element).
// Format-agnostic: the hand-written HeraldListReply/TidingListReply carries a plain `integer`
// without an explicit int32 (see the file header). A mutation (item-only / cursor fields /
// wrong $ref) reddens it.
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

// assertOffsetEnvelopeNoFormat — a variant of assertEnvelopeShape for an envelope where the
// hand-written spec declares offset/limit/total as a plain `integer` WITHOUT `format`
// (Herald/Tiding): checks EXACTLY 4 fields (items/offset/limit/total, WITHOUT cursor fields),
// offset/limit/total are integer type (format is not pinned), items is an array with a $ref
// to the contract element.
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
