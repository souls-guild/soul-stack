package api

// Extra emission of the ErrandAccepted schema into the aggregator spec — Class C alignment
// (202 body of exec async-escalation). Following the schema-builder pre-seed pattern (huma
// Registry.Schema registers a struct type even without a referencing field, registry.go :142).
//
// PROBLEM. POST /v1/souls/{sid}/exec carries dual-status 200/202: 200 — ErrandResult
// (sync terminal), 202 — ErrandAccepted (async-escalation + Location). The handler pre-marshals
// both bodies into one json.RawMessage Body (errandExecOutput.Body) to serve different wire
// bodies under one OperationID (huma cannot express per-status-2xx typed bodies with a single
// output type). ErrandResult reaches components via ANOTHER route (errand-list:
// ErrandListReply.Items=[]ErrandResult is typed), while ErrandAccepted is typed NOWHERE by a
// referencing huma field (errand-get also marshals it via json.RawMessage) → the schema was
// not emitted, even though the hand-written spec (docs/keeper/openapi.yaml :7363) declares it
// and the UI expects it.
//
// MECHANISM (schema-builder pre-seed — WIRE-SAFE). We register the api-named struct
// errandAccepted directly via Components.Schemas.Schema(..., allowRef=false): huma builds and
// places the schema in components/schemas under the name "ErrandAccepted" WITHOUT any
// referencing output field. No operation/response changes — the dual-status Body stays
// json.RawMessage, the 202-body wire bytes are identical to legacy (golden errand byte-exact
// intact). ONLY the presence of the schema in components changes.
//
// ★ Shape verified against the hand-written spec :7363: errand_id (ULID pattern) + status
// (enum [running]); required:[errand_id, status]. Type name = contract name (huma
// DefaultSchemaNamer capitalizes → "ErrandAccepted"). This is the same contract shape as the
// generated ErrandAccepted, but with enum/pattern in the schema (the oapi type carries a
// string-enum type without Go constants → huma would give a bare string). The name
// "errandAccepted" in the api layer does not collide (the errand package is imported into the
// api layer only under an alias).

import (
	"reflect"

	"github.com/danielgtaylor/huma/v2"
)

// errandAccepted — source-of-form for the ErrandAccepted schema (202 body of exec / errand-get
// while running). Shape per the committed hand-written spec (docs/keeper/openapi.yaml :7363):
// errand_id (ULID pattern) + status (enum [running]); both required. This is a pure
// schema-builder type: on the wire the handler serializes the 202 body via json.RawMessage,
// this type does NOT participate in serialization.
type errandAccepted struct {
	ErrandID string `json:"errand_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$" doc:"ULID of the started Errand"`
	Status   string `json:"status" enum:"running" doc:"string still running (async-escalation)"`
}

// registerErrandAccepted places the ErrandAccepted schema into components/schemas via
// Registry.Schema (pre-seed without a referencing field). Called in newHumaCadenceAPI for
// each assembled huma.API. Operations/bodies are NOT affected — dual-status exec stays
// json.RawMessage, wire byte-exact intact.
func registerErrandAccepted(api huma.API) {
	api.OpenAPI().Components.Schemas.Schema(reflect.TypeFor[errandAccepted](), false, "ErrandAccepted")
}
