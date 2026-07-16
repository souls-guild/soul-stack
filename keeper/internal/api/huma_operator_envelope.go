package api

// Aligning the OPERATOR domain's list-envelope name with the committed hand-written spec (ENVELOPE
// mechanism, handler-native PILOT T5d following the huma_incarnation_envelope.go reference).
//
// PROBLEM. GET /v1/operators carries the type sharedapi.PagedResponse[Operator] in its Body
// directly — huma DefaultSchemaNamer sees the instantiated generic and emits a schema
// "PagedResponseOperator" (the generic brackets collapse into concatenation). The hand-written spec
// (docs/keeper/openapi.yaml) declares the envelope as "OperatorListReply" — the UI expects it.
//
// MECHANISM (structural analog of incarnation-envelope): a named struct operatorListReply with
// the contract offset shape (4 int32 fields items/offset/limit/total WITHOUT cursor fields,
// checked against the hand-written spec OperatorListReply) + items.$ref to the native element Operator +
// RegisterTypeAlias(PagedResponse[Operator] → named) in registerOperatorEnvelopes
// (called from newHumaCadenceAPI). The wire type (the PagedResponse body) does NOT change —
// only the OpenAPI Body schema changes.

import (
	"reflect"

	"github.com/danielgtaylor/huma/v2"

	sharedapi "github.com/souls-guild/soul-stack/shared/api"
)

// operatorListReply — the alias target for the GET /v1/operators envelope schema. The shape is checked against
// the committed hand-written spec (docs/keeper/openapi.yaml → OperatorListReply): EXACTLY 4 int32
// fields (items/offset/limit/total), all required, WITHOUT cursor fields. items.$ref to the
// native element Operator (handler-native PILOT: the wire-DTO is fully native). The type name =
// the contract schema name (huma DefaultSchemaNamer capitalizes → "OperatorListReply").
type operatorListReply struct {
	Items  []Operator `json:"items" doc:"страница операторов"`
	Offset int32      `json:"offset" doc:"offset from start of set"`
	Limit  int32      `json:"limit" doc:"page size"`
	Total  int32      `json:"total" doc:"total number of entries in set"`
}

// registerOperatorEnvelopes registers a huma alias on the registry from the instantiated generic
// sharedapi.PagedResponse[Operator] → the named-struct envelope, so that huma builds the
// list-Body schema under the contract name/shape. Called in newHumaCadenceAPI for
// each assembled huma.API. The wire type (the PagedResponse body) does NOT change.
func registerOperatorEnvelopes(api huma.API) {
	api.OpenAPI().Components.Schemas.RegisterTypeAlias(
		reflect.TypeFor[sharedapi.PagedResponse[Operator]](),
		reflect.TypeFor[operatorListReply](),
	)
}
