package api

// Aligns the name/shape of the SOUL-domain list envelope (CURSOR, 6 fields) with the committed
// hand-written spec + collapses the nested SoulSshTarget onto a single schema (CLASS A) — rollout batch N5
// (ENVELOPE+CLASS-A mechanisms following huma_incarnation_envelope.go / huma_voyage_target.go).
//
// === ENVELOPE (CURSOR, 6 fields) ===
//
// PROBLEM. GET /v1/souls carries in Body the type handlers.SoulListReply — a Go type ALIAS to
// sharedapi.PagedResponse[SoulListEntry] (handlers/soul.go). A Go alias is transparent to
// reflect → huma DefaultSchemaNamer sees the instantiated generic and emits the schema
// "PagedResponseSoulListEntry". The spec (docs/keeper/openapi.yaml :6766) declares the envelope
// as "SoulListReply" — the UI expects it.
//
// ★ DIFFERENCE FROM incarnation/operator/voyage (4-field offset): soul is the ONLY cursor domain.
// The spec :6766 carries SIX fields — items/offset/limit/total + next_cursor (string) +
// total_approximate (boolean) — an offset/keyset hybrid (ADR-047 S3b-2a, the server picks the mode from
// Purview). The named struct soulListReply repeats EXACTLY these 6 fields (NOT the incarnation 4-field shape),
// checked against the spec. items.$ref to the contract element SoulListEntry; required:[items,offset,
// limit,total] (next_cursor/total_approximate — optional, omitempty).
//
// MECHANISM. RegisterTypeAlias(PagedResponse[SoulListEntry] → soulListReply): huma builds
// the list-Body schema under the contract name/shape. The wire body (PagedResponse) does NOT change — the json
// fields are the same (next_cursor/total_approximate omitempty are omitted in offset mode) → golden list
// byte-exact.
//
// === CLASS A (SoulSshTarget shared input↔output) ===
//
// SoulSshTarget — a single api type for ALL consumers: input (PUT ssh-target body, see
// huma_soul_op.go) AND output (SoulSshTargetReply.ssh_target carries the generated SoulSSHTarget).
// aliasSoulSshTarget collapses the OUTPUT onto the same named SoulSshTarget schema as the input. The shapes are
// compatible: api.SoulSshTarget — required:[ssh_port,ssh_user,soul_path] (spec :6394),
// ssh_provider optional; SoulSSHTarget — the same fields (ssh_provider *string omitempty). One
// valid SoulSshTarget schema; the technical SoulSSHTarget (from the generated output type) is displaced.
//
// === REPLY-RENAME via ALIAS (SoulCovenAssignReply) — batch N6 ===
//
// Output drift whose name cannot be aligned by a simple Go-struct rename:
//
//   - SoulCovenAssignReply: the wire body of POST /v1/souls/coven carries the handler type
//     handlers.SoulCovenAssignResponse (custom MarshalJSON XOR label↔labels — the
//     unexported struct could be renamed, but the name handlers.SoulCovenAssignReply is ALREADY taken by the internal
//     container {Body, AuditPayload}). DefaultSchemaNamer emitted "SoulCovenAssignResponse".
//     The spec (:7140) declares the wire body as "SoulCovenAssignReply". MECHANISM (like the service
//     envelope): api named struct soulCovenAssignReply (shape exactly per the spec, matched/changed
//     int32, required:[mode,label,matched,changed,status,dry_run]) + alias
//     handlers.SoulCovenAssignResponse → soulCovenAssignReply. The wire body (custom MarshalJSON)
//     does NOT change — only the OpenAPI schema/name of Body does.
//
// SoulSshTargetReply (PUT ssh-target 200 body) moved to huma-native (huma_soul_reply.go,
// final T5b): Body — native SoulSshTargetReply, the native Body provides the schema itself → the rename alias
// SoulSSHTargetReply → soulSshTargetReply is REMOVED. nested ssh_target — class-A reuse of native
// SoulSshTarget (aliasSoulSshTarget on SoulSSHTarget REMAINS — the input PUT body is still legacy-generated).

import (
	"reflect"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
)

// soulCovenAssignReply — the alias target schema for the POST /v1/souls/coven 200 body. The shape is checked against
// the committed hand-written spec (docs/keeper/openapi.yaml :7140 → SoulCovenAssignReply): mode/label/labels/
// matched/changed/status/dry_run; matched/changed — int32; required:[mode,label,matched,changed,
// status,dry_run] (labels — optional). The type name = the contract schema name (huma DefaultSchemaNamer
// capitalizes the first letter → "SoulCovenAssignReply"). The wire body is serialized by the handler type
// (custom MarshalJSON XOR label↔labels) — here it is only the shape for the schema.
//
// OUTPUT-PATTERN FOR NAMES (batch 5, documentation-only): labels[] — the output echo of the applied replace
// set ← soul.CovenPattern (per-element); the reply is output-only (an alias target, NOT a request Body) →
// no input-422 risk. label (the singular append/remove echo) is NOT tagged: for replace mode
// it is "" (XOR with labels), and a pattern would falsely require a coven on the empty string.
type soulCovenAssignReply struct {
	Mode    string   `json:"mode" doc:"тип операции onд covens[]"`
	Label   string   `json:"label" doc:"применёнonя метка for append/remove (mirror input)"`
	Labels  []string `json:"labels,omitempty" pattern:"^[a-z][a-z0-9]*(-[a-z0-9]+)*$" doc:"applied set tokens for replace (mirror input)"` // ← soul.CovenPattern (per-element, output echo)
	Matched int32    `json:"matched" doc:"number of hosts matching selector ∩ scope"`
	Changed int32    `json:"changed" doc:"number of rows actually changed"`
	Status  string   `json:"status" enum:"completed,partial" doc:"completed — all chunks committed; partial — mid-run failure"`
	DryRun  bool     `json:"dry_run" doc:"dry-run without writing"`
}

// soulTraitsAssignReply — the alias target schema for the POST /v1/souls/traits 200 body (ADR-060). The type
// name = the contract schema name (huma DefaultSchemaNamer capitalizes → "SoulTraitsAssignReply").
// keys[] — the output echo of the set of affected trait keys (per-element pattern = soul.TraitKeyPattern);
// trait VALUES are NOT included in the output (secret hygiene, symmetric with audit). matched/changed —
// int32 (the soul envelope-domain convention). The reply is output-only → no input-422 pattern risk.
type soulTraitsAssignReply struct {
	Mode    string   `json:"mode" doc:"режим операции (merge/replace/remove)"`
	Keys    []string `json:"keys" pattern:"^[a-z][a-z0-9]*([_-][a-z0-9]+)*$" doc:"affected trait keys (mirror input)"` // ← soul.TraitKeyPattern (per-element, output echo)
	Matched int32    `json:"matched" doc:"number of hosts matching selector ∩ scope"`
	Changed int32    `json:"changed" doc:"number of rows actually changed"`
	Status  string   `json:"status" enum:"completed,partial" doc:"completed — all chunks committed; partial — mid-run failure"`
	DryRun  bool     `json:"dry_run" doc:"dry-run without writing"`
}

// soulListReply — the alias target schema for the GET /v1/souls envelope (CURSOR, 6 fields). The shape is checked against
// the committed hand-written spec (docs/keeper/openapi.yaml :6766 → SoulListReply): items/offset/limit/total
// (required) + next_cursor (string, optional) + total_approximate (boolean, optional). offset/
// limit/total — int32 (the spec's format:int32). items.$ref to the CONTRACT native element
// SoulListEntry (the same schema the get-Body emits — final T5b, otherwise a huma duplicate-name panic
// between api.SoulListEntry and SoulListEntry). The type name = the contract schema name (huma
// DefaultSchemaNamer capitalizes → "SoulListReply"). The json tags repeat sharedapi.PagedResponse
// (next_cursor/total_approximate omitempty) → the wire does not change.
type soulListReply struct {
	Items            []SoulListEntry `json:"items" doc:"страница реестра souls"`
	Offset           int32           `json:"offset" doc:"offset from start of set (offset-режим)"`
	Limit            int32           `json:"limit" doc:"page size"`
	Total            int32           `json:"total" doc:"общее число записей; зonчимо только в offset-режиме"`
	NextCursor       *string         `json:"next_cursor,omitempty" doc:"opaque keyset-курwithр следующей страницы (keyset-режим); отсутствует в offset-режиме и когда onбор исчерпан"`
	TotalApproximate *bool           `json:"total_approximate,omitempty" doc:"total NOT точен (keyset-режим); в offset-режиме опущеbut"`
}

// registerSoulEnvelopes registers on the registry a huma alias from the instantiated generic
// sharedapi.PagedResponse[SoulListEntry] → named-struct soulListReply (contract name/
// CURSOR shape, 6 fields) + the coven-assign reply-rename alias (batch N6). Called in
// newHumaCadenceAPI for every assembled huma.API. The wire type (the body) does NOT change.
//
// ★ The alias key PagedResponse[SoulListEntry] is untouched (final T5b): the handler list marshals
// exactly this wire type; the element soulListReply.Items []SoulListEntry resolves through the same
// SoulListEntry schema the native get-Body emits (shapes identical → dedup safe).
func registerSoulEnvelopes(api huma.API) {
	schemas := api.OpenAPI().Components.Schemas
	// ★ handler-native T5d: the wire type of list-Body is sharedapi.PagedResponse[handlers.SoulListView]
	// (Go alias handlers.SoulListReply). The SoulListView element schema is reduced through the same alias
	// PagedResponse → soulListReply, whose items.$ref points to the CONTRACT schema SoulListEntry
	// (the same one the native get-Body emits) → dedup is safe, the name/CURSOR shape is stable.
	schemas.RegisterTypeAlias(
		reflect.TypeFor[sharedapi.PagedResponse[handlers.SoulListView]](),
		reflect.TypeFor[soulListReply](),
	)
	// REPLY-RENAME (batch N6): the coven-assign wire-body handler type → the contract name
	// SoulCovenAssignReply. SoulSshTargetReply — native (huma_soul_reply.go), alias removed.
	schemas.RegisterTypeAlias(
		reflect.TypeFor[handlers.SoulCovenAssignResponse](),
		reflect.TypeFor[soulCovenAssignReply](),
	)
	// traits-assign (ADR-060): the wire-body handler type → the contract name SoulTraitsAssignReply.
	schemas.RegisterTypeAlias(
		reflect.TypeFor[handlers.SoulTraitsAssignResponse](),
		reflect.TypeFor[soulTraitsAssignReply](),
	)
}
