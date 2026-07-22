package api

// Aligns the list/history envelope names of the incarnation domain with the committed
// hand-written spec (T4b pilot, ENVELOPE mechanism — the third mechanism of the pattern after
// request-rename and enum-alias).
//
// PROBLEM. The list/history routes carry in Body the type handlers.IncarnationListReply /
// handlers.IncarnationHistoryReply — a Go type ALIAS to sharedapi.PagedResponse[T]
// (handlers/incarnation_typed.go). A Go alias is transparent to reflect → huma DefaultSchemaNamer
// sees the instantiated generic sharedapi.PagedResponse[IncarnationGetReply] and emits
// the schema "PagedResponseIncarnationGetReply" / "PagedResponseStateHistoryEntry" (the
// generic brackets collapse into name concatenation). The spec (docs/keeper/openapi.yaml) declares
// the envelope as "IncarnationListReply" / "IncarnationHistoryReply" — the UI expects exactly those.
//
// MECHANISM (a structural analog of the enum-alias in huma_incarnation_status.go):
//
//   - A NAMED STRUCT (NOT an alias, NOT generic) in the huma layer: incarnationListReply /
//     incarnationHistoryReply. The Go type name = the contract schema name: huma DefaultSchemaNamer
//     takes reflect.Type.Name() and capitalizes the first letter → unexported
//     incarnationListReply yields exactly "IncarnationListReply" (like the enum incarnationStatus →
//     "IncarnationStatus"). The shape is EXACTLY the spec's contract fields: 4 int32 fields
//     (items/offset/limit/total) with no cursor fields. This is deliberately NARROWER than generic
//     PagedResponse[T] (which also carries next_cursor/total_approximate omitempty — needed by
//     the keyset domain soul, NOT incarnation). items.$ref to the contract native element
//     (IncarnationGetReply / StateHistoryEntry — T5a huma-native reply-DTO).
//   - RegisterTypeAlias (registerIncarnationEnvelopes): on encountering the instantiated
//     generic sharedapi.PagedResponse[<element>] huma substitutes the named-struct schema.
//     huma matches the alias by the reflect.Type instantiation key (registry.go:100) — the Go alias
//     IncarnationXReply is transparent, reflect sees exactly the same instantiation → the schema is built
//     from the envelope. Called in newHumaCadenceAPI (the common factory of all huma.API) next to
//     aliasIncarnationStatus — the wire type (the PagedResponse body) does NOT change, only the
//     OpenAPI schema of Body: contract name + contract shape instead of generic.
//
// DOES NOT TOUCH WIRE: handlers.IncarnationListReply/IncarnationHistoryReply (an alias to
// PagedResponse) and the domain *Typed stay generic; the envelope carries the SAME json fields
// (items/offset/limit/total) → the golden byte-exact list/history does not change.

import (
	"reflect"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
)

// incarnationListReply — the alias target schema for the GET /v1/incarnations envelope. The shape is checked against
// the committed hand-written spec (docs/keeper/openapi.yaml → IncarnationListReply): EXACTLY 4
// int32 fields (items/offset/limit/total), all required, with no cursor fields (cursor belongs to the keyset
// domain soul, not incarnation). items.$ref to the contract native element IncarnationGetReply
// (T5a). The type name = the contract schema name (huma DefaultSchemaNamer capitalizes → "IncarnationListReply").
type incarnationListReply struct {
	Items  []IncarnationGetReply `json:"items" doc:"page of incarnations"`
	Offset int32                 `json:"offset" doc:"offset from start of set"`
	Limit  int32                 `json:"limit" doc:"page size"`
	Total  int32                 `json:"total" doc:"total number of entries in set"`
}

// incarnationHistoryReply — the alias target schema for the GET /v1/incarnations/{name}/history envelope.
// The shape is checked against the committed hand-written spec (docs/keeper/openapi.yaml → IncarnationHistoryReply):
// EXACTLY 4 int32 fields (items/offset/limit/total), all required, with no cursor fields. items.$ref
// to the contract native element StateHistoryEntry (T5a). The type name = the contract schema name.
type incarnationHistoryReply struct {
	Items  []StateHistoryEntry `json:"items" doc:"page of state_history entries"`
	Offset int32               `json:"offset" doc:"offset from start of set"`
	Limit  int32               `json:"limit" doc:"page size"`
	Total  int32               `json:"total" doc:"total number of entries in set"`
}

// incarnationRunsReply — the alias target schema for the GET /v1/incarnations/{name}/runs envelope.
// The same contract shape (4 int32 fields items/offset/limit/total, all required, with no
// cursor fields), items.$ref to the native element RunSummaryEntry. The type name = the contract
// schema name (huma DefaultSchemaNamer capitalizes → "IncarnationRunsReply").
type incarnationRunsReply struct {
	Items  []RunSummaryEntry `json:"items" doc:"page of incarnation runs (apply_runs fold)"`
	Offset int32             `json:"offset" doc:"offset from start of set"`
	Limit  int32             `json:"limit" doc:"page size"`
	Total  int32             `json:"total" doc:"total number of incarnation runs"`
}

// registerIncarnationEnvelopes registers on the registry a huma alias from the instantiated generic
// sharedapi.PagedResponse[<element>] → named-struct envelope, so huma builds the
// list/history/runs Body schema under the contract name and contract shape (4 int32 fields,
// $ref to the contract element). Called in newHumaCadenceAPI for every assembled
// huma.API. The wire type (the PagedResponse body) does NOT change — only the OpenAPI schema does.
func registerIncarnationEnvelopes(api huma.API) {
	schemas := api.OpenAPI().Components.Schemas
	// ★ handler-native T5d: the wire type of list/history Body is PagedResponse[handlers.*View].
	// The View element schema is reduced through the same alias to the native envelope, whose items.$ref
	// points to the CONTRACT schema IncarnationGetReply/StateHistoryEntry (the same one the
	// native get-Body emits) → dedup is safe, the name/4-field shape is stable.
	schemas.RegisterTypeAlias(
		reflect.TypeFor[sharedapi.PagedResponse[handlers.IncarnationGetView]](),
		reflect.TypeFor[incarnationListReply](),
	)
	schemas.RegisterTypeAlias(
		reflect.TypeFor[sharedapi.PagedResponse[handlers.StateHistoryView]](),
		reflect.TypeFor[incarnationHistoryReply](),
	)
	schemas.RegisterTypeAlias(
		reflect.TypeFor[sharedapi.PagedResponse[handlers.RunSummaryView]](),
		reflect.TypeFor[incarnationRunsReply](),
	)
}
