// Proof gate: alignment of VOYAGE schema names to the committed hand-written spec (rollout
// batch N3, following huma_operator_schema_test.go). Assembles the aggregated huma spec
// (HumaFullSpecYAML) and checks that voyage-domain schemas are named EXACTLY as the contract
// (docs/keeper/openapi.yaml), and that the technical huma-Go name VoyageCreateHumaBody is ABSENT.
//
// MECHANISMS for voyage (verified against the hand-written spec):
//   - REQUEST-RENAME: voyageCreateHumaBody → VoyageCreateRequest (contract name of the
//     POST /v1/voyages body; same body — preview). Applied.
//   - ENUM-ALIAS: NOT applied. The hand-written spec declares no standalone VoyageKind/
//     VoyageStatus in components/schemas — kind is inlined in VoyageCreateRequest (`enum:
//     [scenario, command]`), statuses in Voyage/VoyageTargetEntry. No standalone enum schema
//     → we do NOT create a named schema.
//   - ENVELOPE: already a named oapi type (VoyageListReply — generated struct, NOT generic
//     PagedResponse) → DefaultSchemaNamer yields the contract name itself; no alias needed.
//     We VERIFY the shape: 4-field offset plain `integer`, items.$ref to Voyage.
//
// NESTED ALIGNMENT (target/notify): nested shared forms are collapsed into SINGLE Go types
// api.VoyageTarget/api.VoyageNotify (huma_voyage_target.go), shared by voyage+cadence. The
// spec has exactly ONE VoyageTarget (input voyage+cadence + output voyage, single native type)
// and ONE VoyageNotify (input voyage+cadence, no output); the technical VoyageTargetHumaBody/
// CadenceTargetHumaBody/VoyageNotifyHumaBody/CadenceNotifyHumaBody are ABSENT. Gate —
// TestSchemaNames_VoyageNested.
package api

import (
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// voyageContractSchemas — request/view/envelope names of the voyage domain exactly as in the
// committed hand-written spec. All must be present in the assembled spec.
var voyageContractSchemas = []string{
	"VoyageCreateRequest",
	"Voyage",
	"VoyageListReply",
	// Nested shared forms (one for both domains, hand-written spec :7455/:7612).
	"VoyageTarget",
	"VoyageNotify",
}

// voyageForbiddenSchemas — technical huma-Go name of the old body struct. Must not remain.
// The hand-written spec declares no standalone enum (VoyageKind/VoyageStatus) — there is no
// separate forbidden name for them (kind/status are inlined).
var voyageForbiddenSchemas = []string{
	"VoyageCreateHumaBody",
	// Technical huma-Go names of the collapsed nested forms — after nested alignment they
	// are gone (one VoyageTarget/VoyageNotify for both domains).
	"VoyageTargetHumaBody",
	"CadenceTargetHumaBody",
	"VoyageNotifyHumaBody",
	"CadenceNotifyHumaBody",
}

// TestSchemaNames_Voyage — gate N3. Contract names present, technical one absent.
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

// TestSchemaNames_VoyageEnvelope — gate N3 (ENVELOPE). VoyageListReply carries the CONTRACT
// 4-field offset shape (items/offset/limit/total; items.$ref to Voyage). Format-agnostic
// (hand-written spec — plain `integer`). A mutation (item-only/cursor/wrong $ref) turns it red.
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

// voyageTargetConsumers — schemas whose `target` field MUST reference
// #/components/schemas/VoyageTarget. Covers input(voyage+cadence) AND output(voyage).
// The cadence input-body name is aligned to the hand-written spec (CadenceCreateRequest, N4).
//
// CadenceDTO (output cadence) is NOT in the list: its target field is a pre-existing
// json.RawMessage (free-form `{}` in the spec, not $ref) from BEFORE this alignment; typing
// the cadence output target as VoyageTarget (and the related CadenceDTO→Cadence rename) is a
// separate task, not nested collapsing of technical duplicates (see N4 report: blocked for N5).
var voyageTargetConsumers = []string{
	"VoyageCreateRequest",  // input voyage (create+preview — one schema)
	"CadenceCreateRequest", // input cadence (N4)
	"Voyage",               // output voyage (via alias VoyageTarget→VoyageTarget)
}

// voyageNotifyConsumers — schemas whose `notify[]` field MUST reference VoyageNotify
// (class B — input only, no output consumer).
var voyageNotifyConsumers = []string{
	"VoyageCreateRequest",
	"CadenceCreateRequest", // N4
}

// TestSchemaNames_VoyageNested — gate for NESTED alignment. Proves:
//   - exactly ONE VoyageTarget and ONE VoyageNotify in the aggregator spec (schema key is
//     unique — presence of the contract name itself is checked by TestSchemaNames_Voyage;
//     here — the REFERENCES to it from all consumers);
//   - input(voyage+cadence) AND output(voyage) reference VoyageTarget; input(voyage+cadence)
//     references VoyageNotify;
//   - technical names (Voyage/CadenceTargetHumaBody, …) are absent (voyageForbiddenSchemas in
//     TestSchemaNames_Voyage);
//   - VoyageTarget/VoyageNotify shape verified against the hand-written spec (required set).
//
// A mutation (split target into a per-domain type / drop the alias / change the required set) turns it red.
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

	// (1) VoyageTarget — consumers reference the single schema.
	for _, name := range voyageTargetConsumers {
		if got := propRef(t, schemas, name, "target"); got != targetRef {
			t.Errorf("%s.target → %q, ожидался %q (target не сведён на единую VoyageTarget)", name, got, targetRef)
		}
	}
	// (2) VoyageNotify — input consumers reference the single schema (notify is an array).
	for _, name := range voyageNotifyConsumers {
		if got := propItemsRef(t, schemas, name, "notify"); got != notifyRef {
			t.Errorf("%s.notify[] → %q, ожидался %q (notify не сведён на единую VoyageNotify)", name, got, notifyRef)
		}
	}

	// (3) VoyageTarget shape verified against the hand-written spec (:7455 — no required; 5 optional fields).
	tgt, _ := schemas["VoyageTarget"].(map[string]any)
	if tgt == nil {
		t.Fatal("VoyageTarget отсутствует в components.schemas")
	}
	if req, ok := tgt["required"]; ok {
		t.Errorf("VoyageTarget.required=%v — рукопись :7455 НЕ объявляет required (все поля optional)", req)
	}
	assertProps(t, tgt, "VoyageTarget", "incarnations", "service", "sids", "where", "coven")

	// (4) VoyageNotify shape verified against the hand-written spec (:7612 — required:[herald]).
	ntf, _ := schemas["VoyageNotify"].(map[string]any)
	if ntf == nil {
		t.Fatal("VoyageNotify отсутствует в components.schemas")
	}
	assertRequiredExactly(t, ntf, "VoyageNotify", "herald")
	assertProps(t, ntf, "VoyageNotify", "herald", "on", "only_failures", "only_changes", "annotations", "projection")
}

// propRef extracts $ref from a scalar field of schema schemaName (target: {$ref: …}).
func propRef(t *testing.T, schemas map[string]any, schemaName, field string) string {
	t.Helper()
	prop := schemaProp(t, schemas, schemaName, field)
	ref, _ := prop["$ref"].(string)
	return ref
}

// propItemsRef extracts $ref from the elements of the array field `field` (notify: {items: {$ref: …}}).
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

// schemaProp extracts the map of field `field` from the properties of schema schemaName.
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

// assertProps checks that the schema's properties contain exactly the expected set of fields.
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

// assertRequiredExactly checks that the schema's required is exactly the expected set.
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
