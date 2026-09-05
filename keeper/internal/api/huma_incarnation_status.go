package api

// Named IncarnationStatus schema ($ref) for components/schemas — the shared truth of the enum set.
//
// huma DefaultSchemaNamer hoists into components/schemas (getsRef=true) ONLY struct types;
// a string-based named type huma always INLINES as `type: string`. The spec (docs/keeper/
// openapi.yaml) declares IncarnationStatus as a separate schema with enum values and references
// it via $ref in every status field — the UI expects exactly the named schema.
//
// The enum TYPE is wire.IncarnationStatus (shared/api/wire/enums.go, NIM-776); the reply/get/list/
// unlock Body carry it directly, projected from the domain handlers.*View flat strings. It carries
// no Schema() method, because huma.SchemaProvider is a METHOD and Go allows one only in the
// declaring package — putting it there would drag huma into shared/ and, through it, into soul.
// So the SchemaProvider lives on the keeper-local shim incarnationStatusSchema
// (huma_enums.go), whose Schema() reads the constants in this file, and registerContractEnums
// points the registry at it with RegisterTypeAlias. huma resolves a registry alias BEFORE it
// looks for a SchemaProvider (mapRegistry.Schema), so the emitted schema is unchanged.

// incarnationStatusSchemaName — the name of the named schema in components/schemas (the contract name
// from the spec; the UI references it by $ref).
const incarnationStatusSchemaName = "IncarnationStatus"

// incarnationStatusSchemaRef — the standard huma component-schema prefix (huma.DefaultConfig
// configures the registry with this prefix) + the name. Returned from SchemaProvider as a $ref.
const incarnationStatusSchemaRef = "#/components/schemas/" + incarnationStatusSchemaName

// incarnationStatusEnum — the allowed status values of a runtime instance (ADR-009/031/
// S-D). Order and contents follow the committed hand-written spec docs/keeper/openapi.yaml
// (IncarnationStatus.enum), which is authoritative for the OpenAPI contract. `provisioning` —
// a post-MVP catalog value (see internal/incarnation.Status: not there yet, but
// the contract already reserves it).
var incarnationStatusEnum = []any{
	"provisioning",
	"ready",
	"applying",
	"error_locked",
	"migration_failed",
	"drift",
	"destroying",
	"destroy_failed",
}

// incarnationStatusDescription — the schema description (parity with the spec).
const incarnationStatusDescription = "Runtime instance status. In proto the constants have " +
	"a family-prefix (INCARNATION_STATUS_READY), in the JSON API - short forms. `drift` - " +
	"informational, NOT blocking: the DB state is ahead of the hosts after a legacy " +
	"upgrade (ADR-031(d)); remediation = a regular apply, which on success returns the " +
	"incarnation to `ready`."
