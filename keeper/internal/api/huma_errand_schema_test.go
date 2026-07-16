// A proof gate aligning the ERRAND schema names to the committed hand-written spec (rollout
// batch N3, following the huma_operator_schema_test.go / huma_herald_schema_test.go reference).
// Assembles the aggregated huma spec (HumaFullSpecYAML) and checks that the errand-domain
// schemas are named EXACTLY as the contract (docs/keeper/openapi.yaml).
//
// MECHANISMS for errand (reconciled with the hand-written spec):
//   - REQUEST-RENAME: NOT applied. list/get/cancel — query/path-only routes with NO request
//     body; there are no huma-input structs with a Body → nothing to rename, there can be no
//     technical *HumaBody names in the spec.
//   - ENUM-ALIAS: NOT applied. The hand-written spec does NOT declare a standalone ErrandStatus in
//     components/schemas — the status is inlined into ErrandResult (`type: string` + enum).
//   - ENVELOPE: already a named oapi type (ErrandListReply — a generated struct, NOT a generic
//     PagedResponse) → DefaultSchemaNamer produces the contract name itself; the alias mechanism is
//     not needed. Here we only VERIFY the shape: 4-field-offset with a plain `integer` (the hand-written
//     spec carries `type: integer` WITHOUT format → a format-agnostic assert; ErrandListReply on a Go int
//     → huma emits int64). items.$ref to the contract ErrandResult.
package api

import (
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// errandContractSchemas — envelope/element names of the errand domain exactly as in the
// committed hand-written spec. All must be present in the assembled spec.
var errandContractSchemas = []string{
	"ErrandListReply",
	"ErrandResult",
	// Class C late emission: ErrandAccepted (202 body of async-escalation exec / errand-get
	// running) is marshaled via json.RawMessage → NOT typed by any referencing huma field
	// → the schema was not emitted. A pre-seed schema-builder (huma_errand_accepted.go) adds it.
	"ErrandAccepted",
}

// TestSchemaNames_Errand — gate N3. The contract envelope/element names are present.
// (the forbidden set is empty: errand has no request bodies → no technical *HumaBody names.)
func TestSchemaNames_Errand(t *testing.T) {
	schemas := loadFullSpecSchemas(t)
	for _, name := range errandContractSchemas {
		if _, ok := schemas[name]; !ok {
			t.Errorf("контрактonя схема %q ОТСУТСТВУЕТ в components/schemas (имя не выровнеbut)", name)
		}
	}
}

// TestSchemaNames_ErrandEnvelope — gate N3 (ENVELOPE). ErrandListReply carries the CONTRACT
// 4-field-offset shape (items/offset/limit/total; items.$ref to ErrandResult). Format-
// agnostic: the hand-written spec declares offset/limit/total as a plain `integer` without an explicit int32.
// A mutation (item-only / cursor fields / a wrong $ref) turns it red.
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

// TestSchemaNames_ErrandAccepted — gate Class C (late emission of the 202 body). ErrandAccepted
// is present in the spec with the contract shape (hand-written spec :7363): errand_id + status
// (enum [running]); required:[errand_id, status]. A mutation (remove registerErrandAccepted →
// the schema disappears, a dual-status 202 without a typed body) turns it red.
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

	// status — a string enum of exactly [running] (contract :7372).
	props, _ := acc["properties"].(map[string]any)
	status, _ := props["status"].(map[string]any)
	if status == nil {
		t.Fatal("ErrandAccepted.status отсутствует")
	}
	rawEnum, _ := status["enum"].([]any)
	if len(rawEnum) != 1 || rawEnum[0] != "running" {
		t.Errorf("ErrandAccepted.status.enum=%v, ожидался ровbut [running]", rawEnum)
	}
}
