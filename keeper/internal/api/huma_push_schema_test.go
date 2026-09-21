// Evidence gate for aligning PUSH schema names to the committed hand-written spec (rollout
// batch N3, following huma_operator_schema_test.go). It assembles the aggregated huma spec
// (HumaFullSpecYAML) and checks that the push-domain schemas are named EXACTLY as the contract
// (docs/keeper/openapi.yaml), while the technical huma Go name PushApplyHumaBody is ABSENT.
//
// MECHANISMS for push (checked against the hand-written spec):
//   - REQUEST-RENAME: pushApplyHumaBody → PushApplyRequest (the contract name of the
//     POST /v1/push/apply body). Applied.
//   - ENUM-ALIAS: NOT applied. The hand-written spec does NOT declare a standalone
//     PushRunStatus — the status is inlined in PushRunListEntry (`type: string` + enum).
//   - ENVELOPE: already a named oapi type (PushRunListReply — a generated struct, NOT a
//     generic PagedResponse) → DefaultSchemaNamer gives the contract name on its own; the
//     alias mechanism isn't needed. Here we only CHECK the shape: 4-field-offset with plain
//     `integer` (the hand-written spec uses `type: integer` without format →
//     format-agnostic). items.$ref points to the contract PushRunListEntry.
package api

import (
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// pushContractSchemas — request/envelope/element names of the push domain exactly as in the
// committed hand-written spec. All must be present in the assembled spec.
var pushContractSchemas = []string{
	"PushApplyRequest",
	"PushApplyReply",
	"PushApplyView",
	"PushRunListReply",
	"PushRunListEntry",
}

// pushForbiddenSchemas — the technical huma Go name that DefaultSchemaNamer WOULD give from
// the old struct name (pushApplyHumaBody → PushApplyHumaBody). Must not remain.
var pushForbiddenSchemas = []string{
	"PushApplyHumaBody",
}

// TestSchemaNames_Push — gate N3. Contract names present, technical name absent.
func TestSchemaNames_Push(t *testing.T) {
	schemas := loadFullSpecSchemas(t)
	for _, name := range pushContractSchemas {
		if _, ok := schemas[name]; !ok {
			t.Errorf("contract schema %q MISSING from components/schemas (name not aligned)", name)
		}
	}
	for _, name := range pushForbiddenSchemas {
		if _, ok := schemas[name]; ok {
			t.Errorf("technical huma name %q PRESENT in spec - name not aligned with contract", name)
		}
	}
}

// TestSchemaNames_PushRunsEnvelope — gate N3 (ENVELOPE). PushRunListReply carries the
// CONTRACT 4-field-offset shape (items/offset/limit/total; items.$ref to PushRunListEntry).
// Format-agnostic (the hand-written spec uses plain `integer`). A mutation (item-only/cursor/
// wrong $ref) reddens the test.
func TestSchemaNames_PushRunsEnvelope(t *testing.T) {
	y, err := HumaFullSpecYAML()
	if err != nil {
		t.Fatalf("HumaFullSpecYAML: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(y), &doc); err != nil {
		t.Fatalf("spec does not parse: %v", err)
	}
	comp, _ := doc["components"].(map[string]any)
	schemas, _ := comp["schemas"].(map[string]any)
	assertOffsetEnvelopeNoFormat(t, schemas, "PushRunListReply", "PushRunListEntry")
}
