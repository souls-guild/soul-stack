package api

// Named IncarnationStatus schema ($ref) for components/schemas — the shared truth of the enum set.
//
// huma DefaultSchemaNamer hoists into components/schemas (getsRef=true) ONLY struct types;
// a string-based named type huma always INLINES as `type: string`. The spec (docs/keeper/
// openapi.yaml) declares IncarnationStatus as a separate schema with enum values and references
// it via $ref in every status field — the UI expects exactly the named schema.
//
// handler-native T5d: the native enum type IncarnationStatus (huma_enums.go) implements
// huma.SchemaProvider itself — its Schema() method reads the constants in this file (schemaName/Ref/Enum/
// Description), registers the named schema "IncarnationStatus" and returns a $ref. Reply/get/list/
// unlock Body carry native IncarnationStatus DIRECTLY (fields projected from the domain
// handlers.*View flat strings), so a separate RegisterTypeAlias IncarnationStatus
// → native is no longer needed (there is no IncarnationStatus field in any reflected Body).

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
	"an informational Scry status (ADR-031), NOT blocking: remediation = a regular apply, " +
	"which on success returns the incarnation to `ready`."
