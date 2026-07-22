package api

// Emitting the SoulStatus and SoulTransport enums into components/schemas as named schemas with
// $ref (rollout batch N5, ENUM mechanism modeled on huma_incarnation_status.go).
//
// PROBLEM. huma's DefaultSchemaNamer emits into components/schemas (getsRef=true) ONLY struct
// types; a string-based named type (SoulStatus / SoulTransport) huma always INLINES as
// `type: string` WITHOUT $ref. The hand-written spec (docs/keeper/openapi.yaml :4198/:4207)
// declares SoulTransport and SoulStatus as separate schemas with enum values and references them
// via $ref (SoulListEntry.status/.transport, SoulCreateReply.status/.transport,
// SoulCovenAssignSelector.status) — the UI expects exactly named schemas.
//
// MECHANISM (huma-only, no edits to the generated oapi package): for each enum — a string type in
// the api package with huma.SchemaProvider that registers the named schema and returns a $ref to
// it, + a RegisterTypeAlias of the domain oapi type to our SchemaProvider. The wire type (string)
// does NOT change, only the OpenAPI schema: status/transport become $ref instead of inline
// `type: string`. Registration is idempotent. Both aliases are called in newHumaCadenceAPI (the
// shared factory for all huma.API).
//
// ENUM SET. The values come from the DOMAIN truth (internal/soul: Status*/Transport*,
// validStatus/validTransport) — the same 6 statuses / 2 transports the code already emits inline
// on status fields and validates in the domain. The hand-written spec :4207 declares SoulStatus
// with a trimmed set (pending/connected/disconnected/expired — without revoked/destroyed) — this
// is a pre-existing content drift in the hand-written spec, NOT a naming one (see report N5); the
// named schema carries the full domain set, like the existing inline schema.

import (
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// soulStatusSchemaName / soulTransportSchemaName — the contract names of the named schemas (from
// the hand-written spec; the UI references them by $ref).
const (
	soulStatusSchemaName    = "SoulStatus"
	soulTransportSchemaName = "SoulTransport"
)

const (
	soulStatusSchemaRef    = "#/components/schemas/" + soulStatusSchemaName
	soulTransportSchemaRef = "#/components/schemas/" + soulTransportSchemaName
)

// soulStatusEnum — Soul statuses in the registry. The set is the domain truth
// (internal/soul.validStatus): 6 values. The hand-written spec :4207 carries a trimmed set — the
// named schema follows the domain (see the header).
var soulStatusEnum = []any{
	string(soul.StatusPending),
	string(soul.StatusConnected),
	string(soul.StatusDisconnected),
	string(soul.StatusRevoked),
	string(soul.StatusExpired),
	string(soul.StatusDestroyed),
}

// soulTransportEnum — the configuration delivery method. The set is the domain truth
// (internal/soul.validTransport): agent / ssh.
var soulTransportEnum = []any{
	string(soul.TransportAgent),
	string(soul.TransportSSH),
}

const soulStatusDescription = "Soul status in the registry."

const soulTransportDescription = "Configuration delivery method. agent — soul daemon on top of " +
	"mTLS gRPC stream; ssh — agentless push."

	// SchemaProvider targets — the NATIVE enum types SoulStatus / SoulTransport (huma_enums.go,
	// T5d-2c-full Phase 1). The native enum types implement huma.SchemaProvider themselves (emitting
	// the named schemas "SoulStatus"/"SoulTransport" with the domain enum set and $ref). The
	// schemaName/Ref/Enum/Description constants above are the shared truth read by the native types'
	// Schema() methods.
	//
	// handler-native T5d: reply/get/list Body carry native SoulStatus/SoulTransport DIRECTLY (fields
	// are projected from the domain Soul*View flat strings), so a separate RegisterTypeAlias
	// SoulStatus → native is no longer needed (there is not a single SoulStatus field in the
	// reflected Body).
