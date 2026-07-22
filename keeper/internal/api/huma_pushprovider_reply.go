package api

// HUMA-NATIVE reply-DTO for the PUSH-PROVIDER domain (handler-native T5d-2c-full). The reply/output Body
// of huma operations is a native Go struct in package api, no legacy generator. The register func
// (huma_pushprovider.go) projects the handler's flat domain views (PushProviderView)
// directly INTO THESE types — there are no more legacy-generator→native converters. Key points for push-provider:
//
//   - SHAPE byte-for-byte = the former legacy generator (json tags/omitempty/date-time/nullable categories A-D).
//   - SCHEMA NAME = contractual (PushProvider / PushProviderListReply): huma's
//     DefaultSchemaNamer takes reflect.Type.Name() → schema under the same name the
//     former legacy generator produced.
//   - params — `map[string]interface{}` without omitempty: the handler normalizes nil→{}
//     (toPushProviderView), always an object on the wire. updated_by_aid — `*string` with omitempty
//     (nil → key omitted). created_at/updated_at — nanosecond time-wire (.UTC() without
//     Truncate; the value comes from the handler layer, not the shape).
//   - PushProviderListReply — NOT a generic envelope (not sharedapi.PagedResponse) but a plain
//     reply with items[]PushProvider + offset/limit/total (int) → `type: integer` without
//     format (assertOffsetEnvelopeNoFormat). Top-level reply-DTO, not via an alias.

import (
	"time"
)

// === top-level reply-DTO (shape 1:1 with the former legacy generator shape) ===

// PushProvider — native push_providers registry record (POST 201 / GET 200 / PUT 200 /
// list element). Shape 1:1 with PushProvider: params — `map` without omitempty (handler yields
// {} on nil); updated_by_aid — `*string` with omitempty; created_at/updated_at —
// nanosecond time-wire.
// OUTPUT-PATTERN (documentation-only, NOT runtime validation): huma does NOT validate
// the response body (empirically 200, not 500). created_by_aid/updated_by_aid ←
// operator.AIDPattern (format for client codegen); the pattern does not affect json.Marshal.
type PushProvider struct {
	CreatedAt    time.Time              `json:"created_at"`
	CreatedByAID string                 `json:"created_by_aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	Name         string                 `json:"name"`
	Params       map[string]interface{} `json:"params"`
	UpdatedAt    time.Time              `json:"updated_at"`
	UpdatedByAID *string                `json:"updated_by_aid,omitempty" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
}

// PushProviderListReply — native 200 body of GET /v1/push-providers (offset envelope:
// items/offset/limit/total). items — native PushProvider; offset/limit/total — int (parity
// with the legacy generator). Shape 1:1 with PushProviderListReply.
type PushProviderListReply struct {
	Items  []PushProvider `json:"items"`
	Limit  int            `json:"limit"`
	Offset int            `json:"offset"`
	Total  int            `json:"total"`
}
