package api

// AuditEventType — the enum of `AuditEvent.type`, DERIVED from the audit catalog
// (NIM-346). Third emission mode next to the two in huma_enums.go: an inline enum
// whose value set is not written here at all.
//
// WHY IT IS NOT A CONST BLOCK. Every other enum in the huma layer is a hand-kept
// const block, which works because the sets are small and closed. The audit catalog
// is neither: 138 values today, open by construction (ADR-022(g) adds names as each
// write-path subsystem is normalized), and authored in another module entirely
// ([audit.EventType]). A const block mirroring it would be a second copy of a list
// that grows — the vendored-artifact drift class — so the values come from
// [audit.AllEventTypes] instead, which is generated from the declarations.
//
// WHAT THIS BUYS. `AuditEvent.type` used to be a bare string, so a client rendering
// a human label per event had nothing to check its coverage against: an unlabelled
// new type was invisible until a user saw it (NIM-337), and labels for retired types
// were invisible in the other direction. With the set in the contract, a client can
// assert coverage in both directions against the spec it already vendors.
//
// SCOPE — THE RESPONSE FIELD ONLY. The `?type=` query filter (auditListInput.Types)
// stays a free string deliberately. huma answers an out-of-enum query value with 422,
// so putting the enum there would turn `?type=whatever` from "200, no matches" into a
// rejected request — a wire regression for a filter that legitimately runs against
// historical rows whose type may since have been retired. Narrowing an OUTPUT is safe
// (the server already only emits catalog values); narrowing an INPUT is not.
//
// EMISSION. [huma.SchemaProvider] returning an INLINE schema, not a $ref: huma refs
// only struct kinds (registry.go getsRef), so a string-kinded type is inlined wherever
// it is used, and no named schema is added to the components map. This matches how
// every other enum reaches the spec — the two $ref enums in huma_enums.go are the
// exception, not the rule.

import (
	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// auditEventTypeDescription — the enum's `description` in the spec. States where the
// set comes from so a reader of the committed openapi.yaml is not left guessing why
// 138 values are inlined.
const auditEventTypeDescription = "Audit event type, `<area>.<action>` (docs/naming-rules.md -> Audit-events). " +
	"The enum is the full catalog keeper can write, generated from the audit event-type declarations; " +
	"the catalog is open, so a new release may add values."

// AuditEventType — the wire type of `AuditEvent.type`. `type X string`, so the wire
// bytes and json.Marshal behaviour are identical to the plain string it replaced.
type AuditEventType string

// Schema implements [huma.SchemaProvider]: `type: string` + the full catalog as an
// inline enum. Ordered by wire value ([audit.AllEventTypes] is generated sorted), so
// the committed openapi.yaml does not churn on unrelated edits.
func (AuditEventType) Schema(huma.Registry) *huma.Schema {
	types := audit.AllEventTypes()
	values := make([]any, 0, len(types))
	for _, t := range types {
		values = append(values, string(t))
	}
	return &huma.Schema{
		Type:        huma.TypeString,
		Enum:        values,
		Description: auditEventTypeDescription,
	}
}
