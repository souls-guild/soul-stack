// Proof gate for aligning CADENCE schema names to the committed hand-written spec (rollout batch
// N4, following huma_voyage_schema_test.go / huma_herald_schema_test.go). Builds the
// aggregated huma spec (HumaFullSpecYAML) and checks that the cadence-domain schemas are
// named EXACTLY as the contract (docs/keeper/openapi.yaml), and that the technical huma-Go names
// of bodies/replies are ABSENT.
//
// MECHANISMS for cadence (verified against the hand-written spec):
//   - REQUEST-RENAME: cadenceCreateHumaBody → CadenceCreateRequest (:7853),
//     cadencePatchHumaBody → CadencePatchRequest (:7973). Applied.
//   - REPLY-RENAME: cadenceCreateReplyHumaBody → CadenceCreateReply (:8051) —
//     the contract name of the POST /v1/cadences 201 body. Applied.
//   - ENVELOPE (runs): GET /v1/cadences/{id}/runs per the hand-written spec (:2378) references
//     VoyageListReply (child Voyages reuse the Voyage DTO). handlers.CadenceRunsReply
//     = PagedResponse[voyageDTO] → huma emitted PagedResponseVoyage; the generic alias →
//     VoyageListReply (registerCadenceEnvelopes) reduces runs to the same named schema
//     VoyageListReply as voyage list. Applied.
//   - ENUM-ALIAS: NOT applied. The hand-written spec does NOT declare standalone ScheduleKind/
//     OverlapPolicy — both are inlined into CadenceCreateRequest/Cadence (`enum: […]`).
//     There is no standalone enum schema → we do NOT create a named schema.
//   - NESTED (target/notify): done in the nested-voyage batch (VoyageTarget/VoyageNotify,
//     shared voyage+cadence). NOT touched here; the cadence-input references to them are checked by
//     TestSchemaNames_VoyageNested (consumer CadenceCreateRequest, N4-aligned).
//
// LIST + element (batch N6): GET /v1/cadences — element CadenceDTO→Cadence (:8078) and envelope
// PagedResponseCadenceDTO→CadenceListReply (:8147) aligned by NAMING via an api-named-struct +
// alias (huma_cadence_envelope.go). ★ target is typed as $ref VoyageTarget (:8106) ONLY in the
// schema: the alias substitutes the schema, the wire body cadenceDTO.target=json.RawMessage serializes the
// same way → golden cadence get/list/patch byte-exact. CadenceDTO/PagedResponseCadenceDTO — now
// in forbidden (drift displaced).
package api

import (
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// cadenceContractSchemas — the request/reply names of the cadence domain exactly as in the committed
// hand-written spec. All must be present in the assembled spec.
var cadenceContractSchemas = []string{
	"CadenceCreateRequest",
	"CadencePatchRequest",
	"CadenceCreateReply",
	// the runs envelope is reduced to VoyageListReply (hand-written spec :2378); VoyageListReply itself
	// is present from the voyage domain — here we pin it as the contract for runs.
	"VoyageListReply",
	// LIST + element (batch N6).
	"Cadence",
	"CadenceListReply",
}

// cadenceForbiddenSchemas — the technical huma-Go names of the aligned bodies/replies + generic
// envelopes. None should remain.
var cadenceForbiddenSchemas = []string{
	"CadenceCreateHumaBody",
	"CadencePatchHumaBody",
	"CadenceCreateReplyHumaBody",
	"PagedResponseVoyage",     // runs envelope reduced to VoyageListReply
	"CadenceDTO",              // → Cadence (batch N6)
	"PagedResponseCadenceDTO", // → CadenceListReply (batch N6)
}

// TestSchemaNames_Cadence — gate N4. Contract names present, technical ones absent.
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

// TestSchemaNames_CadenceCreateRequestShape — the CadenceCreateRequest shape verified against the
// hand-written spec (:7853): required name/schedule_kind/overlap_policy/kind/target; target —
// $ref to VoyageTarget (nested alignment); notify[] — $ref to VoyageNotify. A mutation
// (losing required / scattering target into a per-domain type) turns it red.
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

// TestSchemaNames_CadenceRunsEnvelope — the runs response (GET /v1/cadences/{id}/runs) reduced
// to the named schema VoyageListReply with the contract 4-field offset shape (items.$ref to Voyage;
// WITHOUT cursor fields). Format-agnostic (hand-written spec plain `integer`). A mutation (cursor leak /
// item-only / wrong $ref) turns it red — guarantees the generic PagedResponseVoyage did not
// return.
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

// TestSchemaNames_CadenceListEnvelope — gate N6 (LIST + element). GET /v1/cadences reduced to
// the named schema CadenceListReply (4-field offset; items.$ref to element Cadence). Element Cadence
// carries the hand-written spec's required set (:8145) and target=$ref VoyageTarget (:8106 — typed, NOT
// free-form `{}`). A mutation (free-form target / item-only / wrong required set) turns it red —
// guarantees the generic PagedResponseCadenceDTO/CadenceDTO did not return.
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
