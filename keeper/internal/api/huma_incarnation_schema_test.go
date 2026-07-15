// Proof gate aligning incarnation schema names with the committed reference
// (T4b pilot). Assembles the aggregated huma spec ([HumaFullSpecYAML]) and checks that
// incarnation-domain schemas are named EXACTLY per the contract (docs/keeper/openapi.yaml), and
// that technical huma Go names (IncCreateHumaBody etc.) are ABSENT from the spec. A mutation
// (restoring the old struct name) reddens this test.
package api

import (
	"sort"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// incarnationContractSchemas — request/reply/view names of the incarnation domain exactly as in
// the committed reference (docs/keeper/openapi.yaml). All must be present in the assembled
// spec as components/schemas.
var incarnationContractSchemas = []string{
	"IncarnationCreateRequest",
	"IncarnationCreateReply",
	"IncarnationRunRequest",
	"IncarnationRunReply",
	"IncarnationUnlockRequest",
	"IncarnationUnlockReply",
	"IncarnationUpgradeRequest",
	"IncarnationUpgradeReply",
	"IncarnationRerunLastRequest",
	"IncarnationRerunLastReply",
	"IncarnationCheckDriftRequest",
	"IncarnationUpdateHostsRequest",
	"IncarnationSetTraitsRequest",
	"IncarnationSpecHost",
	"IncarnationGetReply",
	"IncarnationDestroyReply",
	"IncarnationStatus",
	"IncarnationListReply",
	"IncarnationHistoryReply",
}

// incarnationForbiddenSchemas — technical huma Go names that DefaultSchemaNamer
// WOULD produce from the old struct names (incCreateHumaBody → IncCreateHumaBody). None
// must remain in the spec after alignment.
var incarnationForbiddenSchemas = []string{
	"IncCreateHumaBody",
	"IncRunHumaBody",
	"IncUnlockHumaBody",
	"IncUpgradeHumaBody",
	"IncRerunHumaBody",
	"IncCheckDriftHumaBody",
	"IncUpdateHostsHumaBody",
	"IncHostHumaBody",
	// Generic names that DefaultSchemaNamer WOULD produce from an unaliased
	// sharedapi.PagedResponse[T] (generic brackets collapse into concatenation):
	// the envelope alias must displace them with contract names.
	"PagedResponseIncarnationGetReply",
	"PagedResponseStateHistoryEntry",
}

// TestSchemaNames_Incarnation — T4b gate. Proves the assembled aggregator spec
// carries incarnation schemas under CONTRACT names and carries no technical huma names.
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

// TestTraitsRelocation_OpenAPI — gate for the Trait relocation per-soul → per-incarnation
// (ADR-060 amend R1): (1) POST /v1/incarnations carries a top-level `traits` field;
// (2) PUT /v1/incarnations/{name}/traits is mounted (operationId setIncarnationTraits);
// (3) per-soul POST /v1/souls/traits is marked deprecated:true. Any rollback reddens it.
func TestTraitsRelocation_OpenAPI(t *testing.T) {
	y, err := HumaFullSpecYAML()
	if err != nil {
		t.Fatalf("HumaFullSpecYAML: %v", err)
	}

	var doc struct {
		Paths      map[string]map[string]yaml.Node `yaml:"paths"`
		Components struct {
			Schemas map[string]struct {
				Properties map[string]yaml.Node `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal([]byte(y), &doc); err != nil {
		t.Fatalf("спека не парсится: %v", err)
	}

	// (1) create-request carries the traits field.
	if _, ok := doc.Components.Schemas["IncarnationCreateRequest"].Properties["traits"]; !ok {
		t.Error("IncarnationCreateRequest без поля traits — top-level create-traits не прокинут")
	}

	// (2) PUT .../traits is mounted.
	put, ok := doc.Paths["/v1/incarnations/{name}/traits"]
	if !ok {
		t.Fatal("путь /v1/incarnations/{name}/traits ОТСУТСТВУЕТ в спеке")
	}
	if _, ok := put["put"]; !ok {
		t.Errorf("у /v1/incarnations/{name}/traits нет PUT-операции: %v", put)
	}
	if !strings.Contains(y, "setIncarnationTraits") {
		t.Error("operationId setIncarnationTraits отсутствует в спеке")
	}

	// (3) per-soul deprecated:true.
	soulTraits, ok := doc.Paths["/v1/souls/traits"]
	if !ok {
		t.Fatal("путь /v1/souls/traits ОТСУТСТВУЕТ в спеке")
	}
	postNode, ok := soulTraits["post"]
	if !ok {
		t.Fatal("/v1/souls/traits без POST-операции")
	}
	var op struct {
		Deprecated bool `yaml:"deprecated"`
	}
	if err := postNode.Decode(&op); err != nil {
		t.Fatalf("decode soul.traits POST: %v", err)
	}
	if !op.Deprecated {
		t.Error("POST /v1/souls/traits НЕ помечен deprecated:true (релокация per-soul → per-incarnation не отражена)")
	}
}

// TestSchemaNames_IncarnationStatusEnum — T4b gate (enum). IncarnationStatus is extracted as a
// named schema with enum values (not inline `type: string`), and the status fields of the reply
// structs reference it via $ref (as in the 3.0.3 reference).
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
	// Contract minimum: the key statuses from the reference are present.
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

	// status fields reference the named schema via $ref (not inline).
	if !strings.Contains(y, "#/components/schemas/IncarnationStatus") {
		t.Error("ни одно поле не ссылается на IncarnationStatus через $ref — статус остался инлайн")
	}
}

// TestSchemaNames_IncarnationEnvelope — T4b gate (ENVELOPE). The list/history envelopes are
// extracted as named schemas under CONTRACT names (IncarnationListReply/Incarnation-
// HistoryReply) with the CONTRACT shape (exactly 4 int32 fields items/offset/limit/total WITHOUT
// cursor fields; items.$ref to the contract element). A mutation (removing the envelope alias)
// reddens: huma emits the generic name PagedResponse* — both assert blocks fail.
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
		schema  string // contract name of the envelope schema
		element string // contract name of the element schema referenced by items.$ref
	}{
		{"IncarnationListReply", "IncarnationGetReply"},
		{"IncarnationHistoryReply", "StateHistoryEntry"},
	}
	for _, c := range cases {
		assertEnvelopeShape(t, schemas, c.schema, c.element)
	}
}

// assertEnvelopeShape checks that the envelope schema carries EXACTLY the contract shape
// of the reference: 4 fields (items/offset/limit/total), offset/limit/total — int32, items —
// an array with $ref to the contract element, WITHOUT cursor fields (next_cursor/total_approximate
// of generic PagedResponse[T] must not leak — otherwise the envelope alias didn't work).
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

	// Exactly 4 fields — cursor fields (next_cursor/total_approximate) must not leak.
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

	// offset/limit/total — integer int32. (huma emits OpenAPI 3.1, type may be
	// the string "integer" — for a non-nullable scalar; nullable would give an array [..,"null"].)
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

	// items — array with $ref to the contract element. (3.1: slice nullable → type
	// [array,null]; schemaTypeHas accepts both notations.)
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

// schemaTypeHas checks that the JSON-Schema `type` node contains want. OpenAPI 3.1 (huma
// default) encodes type as a string ("integer") for non-nullable or an array (["array","null"])
// for nullable — we accept both forms.
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
