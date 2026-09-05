package api

// Named-schema emission for the three contract enums that the UI references by $ref.
//
// THE ENUM TYPES THEMSELVES ARE NOT HERE. The catalog — every `type <Name> string` of the
// contract plus its const block — moved to shared/api/wire (NIM-776), because soulctl and the
// e2e/load harnesses read these values too and an internal/ package forced each of them to
// retype the set. Aliases in huma_wire_alias.go keep this package naming them unqualified.
//
// WHAT STAYED. Emission is server-side and stays here. Two modes, unchanged:
//
//	(a) INLINE enum (the vast majority): the property declares the enum INLINE
//	    (`type: string` + `enum:` from the struct tag), huma inlines the string-named type →
//	    there is NO separate named schema. Nothing to do — a wire.* enum type carries no
//	    SchemaProvider, which is exactly what mode (a) requires.
//	(b) NAMED schema ($ref): only SoulStatus / SoulTransport / IncarnationStatus. The UI
//	    references them by $ref, so a standalone schema has to be registered.
//
// ★ WHY MODE (b) IS A SHIM AND NOT A METHOD. huma reaches a named schema through
// huma.SchemaProvider, which is a METHOD on the type — and Go allows a method only in the
// package that declares the type. With the enum in shared/api/wire the method cannot follow it
// there without dragging huma into shared/ and, through it, into soul (ADR-011 forbids that
// direction). So each of the three keeps a keeper-local shim string type carrying the
// SchemaProvider, and registerContractEnums points huma at the shim whenever it reflects the
// wire type. huma resolves the registry alias BEFORE it looks for a SchemaProvider
// (mapRegistry.Schema), so the emitted schema is the one the shim returns — byte-identical to
// what the method on the enum used to produce.
//
// ★ SCHEMA COUNT IS STABLE (159 schemas). The shims are string-kind, so they never take a name
// of their own from DefaultSchemaNamer; they return the hard $ref built from the
// schemaName/Ref/Enum/Description constants in huma_soul_status.go and
// huma_incarnation_status.go, which stay the shared truth.
//
// A third mode lives OUTSIDE this file: AuditEventType (huma_audit_event_type.go) is inline like
// (a), but its 138 values are DERIVED from the audit catalog rather than declared as a const
// block — the set is open and authored in another module, so a copy would drift. It stays in
// this package for that reason.
//
// NOTE ON SOULSTATUS. The wire.SoulStatus const block carries 4 values (connected/disconnected/
// expired/pending) — the trimmed contract set; the named schema below carries the FULL domain
// set from internal/soul (6 values). This is a pre-existing content drift (see
// huma_soul_status.go), NOT a naming one. huma_enums_test.go checks the const set (4); the named
// schema's enum set is a separate invariant.

import (
	"reflect"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/shared/api/wire"
)

// soulStatusSchema / soulTransportSchema / incarnationStatusSchema — the keeper-local
// SchemaProvider carriers for the three named enums. They are never marshaled and never appear
// in a Body: registerContractEnums points the registry at them, and huma asks THEM for the
// schema of the corresponding wire type.
type (
	soulStatusSchema        string
	soulTransportSchema     string
	incarnationStatusSchema string
)

// Schema implements huma.SchemaProvider: registers the named schema "SoulStatus" (string+enum,
// the full domain set) and returns a $ref. Idempotent. The wire type (string) does NOT change.
func (soulStatusSchema) Schema(r huma.Registry) *huma.Schema {
	if _, ok := r.Map()[soulStatusSchemaName]; !ok {
		r.Map()[soulStatusSchemaName] = &huma.Schema{
			Type:        huma.TypeString,
			Enum:        soulStatusEnum,
			Description: soulStatusDescription,
		}
	}
	return &huma.Schema{Ref: soulStatusSchemaRef}
}

// Schema implements huma.SchemaProvider: registers the named schema "SoulTransport" and returns
// a $ref. Idempotent.
func (soulTransportSchema) Schema(r huma.Registry) *huma.Schema {
	if _, ok := r.Map()[soulTransportSchemaName]; !ok {
		r.Map()[soulTransportSchemaName] = &huma.Schema{
			Type:        huma.TypeString,
			Enum:        soulTransportEnum,
			Description: soulTransportDescription,
		}
	}
	return &huma.Schema{Ref: soulTransportSchemaRef}
}

// Schema implements huma.SchemaProvider: registers the named schema "IncarnationStatus" and
// returns a $ref. Idempotent.
func (incarnationStatusSchema) Schema(r huma.Registry) *huma.Schema {
	if _, ok := r.Map()[incarnationStatusSchemaName]; !ok {
		r.Map()[incarnationStatusSchemaName] = &huma.Schema{
			Type:        huma.TypeString,
			Enum:        incarnationStatusEnum,
			Description: incarnationStatusDescription,
		}
	}
	return &huma.Schema{Ref: incarnationStatusSchemaRef}
}

// registerContractEnums points the registry of one huma.API at the three shims, so a field
// typed wire.SoulStatus / wire.SoulTransport / wire.IncarnationStatus emits the named schema
// with a $ref instead of a bare `type: string`. There is ONE place to call it from:
// newHumaCadenceAPI is the only site in the repository that constructs a huma.API
// (humachi.New), and humaDumpSpec goes through it too. A second factory that skipped this call
// would be a silent contract change rather than a compile error — openapi_drift_test.go is what
// would catch it.
func registerContractEnums(api huma.API) {
	schemas := api.OpenAPI().Components.Schemas
	schemas.RegisterTypeAlias(reflect.TypeFor[wire.SoulStatus](), reflect.TypeFor[soulStatusSchema]())
	schemas.RegisterTypeAlias(reflect.TypeFor[wire.SoulTransport](), reflect.TypeFor[soulTransportSchema]())
	schemas.RegisterTypeAlias(reflect.TypeFor[wire.IncarnationStatus](), reflect.TypeFor[incarnationStatusSchema]())
}
