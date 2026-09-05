// Push-Provider bodies (ADR-032 amendment 2026-05-26, S7-2). A Push-Provider
// holds per-provider env-payload params for the push-flow SSH plugin - NOT a
// Cloud Provider, which is a different entity with its own tables and
// permission scopes.
//
//   - params is `map[string]interface{}` WITHOUT omitempty: keeper's handler
//     normalises nil to {}, so it is always an object on the wire.
//   - updated_by_aid is `*string` WITH omitempty - nil omits the key.
//   - created_at / updated_at are nanosecond time-wire.
//   - PushProviderListReply is a plain offset envelope, not the generic
//     PagedResponse: offset/limit/total are int and emit `type: integer` with
//     no format.
//
// This is the domain NIM-729 broke and NIM-776 exists because of: the identifier
// is `id`, and soulctl spent that window sending `name` into a body whose
// schema had become `id` + additionalProperties:false.

package wire

import (
	"time"
)

// PushProvider — native push_providers registry record (POST 201 / GET 200 / PUT 200 /
// list element). Shape 1:1 with PushProvider: params — `map` without omitempty (handler yields
// {} on nil); updated_by_aid — `*string` with omitempty; created_at/updated_at —
// nanosecond time-wire.
// OUTPUT-PATTERN (documentation-only, NOT runtime validation): huma does NOT validate
// the response body (empirically 200, not 500). created_by_aid/updated_by_aid ←
// operator.AIDPattern (format for client codegen); the pattern does not affect json.Marshal.
type PushProvider struct {
	CreatedAt    time.Time `json:"created_at"`
	CreatedByAID string    `json:"created_by_aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	// Label — the display caption (ADR-0085), free text and mutable via
	// PUT /v1/push-providers/{id}/label. Absent means the row carries none and
	// the consumer shows `name`.
	Label        *string                `json:"label,omitempty"`
	ID           string                 `json:"id"`
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

// PushProviderCreateRequest — the Go shape of the POST /v1/push-providers body (code-first
// source of the schema AND validation). name + optional params (opaque map; sensitive
// keys — vault-refs). additionalProperties in params:true (opaque payload), but at the
// TOP level of the body — false (the huma default) → an unknown body field → 400. The
// format of name and the sensitive-invariant of params — domain validation in
// CreateTyped (422). The struct name = the contract schema name (huma DefaultSchemaNamer
// takes reflect.Type.Name()) — aligned to the committed hand-written spec (rollout batch
// N3). The register-func projects into the native handlers.PushProviderCreateInput.
type PushProviderCreateRequest struct {
	ID string `json:"id" required:"true" pattern:"^[a-z][a-z0-9-]{0,62}$" doc:"Push Provider id (= plugins.ssh_providers[].name)"`
	// label is the optional display caption (ADR-0085): free text, changed later
	// by PUT /v1/push-providers/{id}/label. No pattern — capitals and spaces
	// are the point, and the letter-first rule on `name` exists because the NAME
	// becomes an env-var name, which the caption never does.
	Label  *string        `json:"label,omitempty" doc:"Display caption: free text, may carry capitals and spaces (ADR-0085). Omitted means consumers show the name instead. Never used to derive a Vault path, an RBAC scope, a snapshot directory or a CEL root"`
	Params map[string]any `json:"params,omitempty" doc:"opaque params; sensitive — vault-refs (values are not logged)"`
}

// PushProviderUpdateRequest — the Go shape of the PUT /v1/push-providers/{id} body
// (replace semantics: params fully replaces the existing set). Params is
// required:"true" (PUT sends the full new set; an empty {} is legitimate — it clears
// params). NOT Optional[T]: read-modify-write on the client, no presence distinction.
// The struct name = the contract schema name (huma DefaultSchemaNamer) — aligned to
// the committed hand-written spec (rollout batch N3). The register-func projects into
// the native handlers.PushProviderUpdateInput.
type PushProviderUpdateRequest struct {
	Params map[string]any `json:"params" required:"true" doc:"full new set of params (replace); sensitive — vault-refs"`
}
