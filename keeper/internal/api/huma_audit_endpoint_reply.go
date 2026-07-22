package api

// HUMA-NATIVE reply-DTO of the AUDIT-ENDPOINT domain (Teardown T5b, rollout following the
// T5a reference huma_incarnation_reply.go). The reply/output Body of GET /v1/audit is a native
// Go struct in package api, NOT emitted by the legacy generator from a hand-written spec. The
// pattern (6 steps) is in the header of huma_incarnation_reply.go. Key points for audit:
//
//   - SHAPE byte-for-byte = the legacy generator's (json tags/omitempty/date-time/nullable categories A-D).
//   - SCHEMA NAME = the contract one (AuditEvent / AuditEventListReply): huma DefaultSchemaNamer
//     takes reflect.Type.Name() → the schema is under the same name the legacy generator produced.
//   - archon_aid/correlation_id — `*string` with omitempty (nil → key omitted); payload —
//     `map[string]interface{}` without omitempty (always an object on the wire); created_at —
//     nanosecond time-wire (the value is truncated to second precision by the handler layer, not
//     the shape); source — enum type AuditEventSource (no alias declared, the hand-written spec inlines
//     `type: string` + enum — the schema is byte-identical for native and legacy, parity with GitRefType).
//   - AuditEventListReply here is NOT a generic envelope (the handler returns a named
//     AuditEventListReply, not sharedapi.PagedResponse), but a plain reply with an items[]AuditEvent
//     field + offset/limit/total. offset/limit/total — int (parity with the legacy generator) → `type: integer`
//     without format (assertOffsetEnvelopeNoFormat). Ported as a top-level reply-DTO.
//
// OUTPUT-PATTERN (documentation-only, NOT runtime validation): huma does NOT validate the
// response body (empirically 200, not 500). id/correlation_id — machine-generated ULID (migration
// 001: audit_id/correlation_id "ULID"); archon_aid ← operator.AIDPattern. Format is for
// client codegen; pattern does not affect json.Marshal (golden byte-exact intact).

import (
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// === top-level reply-DTO (shape 1:1 with the former legacy generator) ===

// AuditEvent — a native audit_log record (element of AuditEventListReply.items). Shape 1:1 with
// the former AuditEvent: archon_aid/correlation_id — `*string` with omitempty; payload — `map`
// without omitempty (always an object); created_at — nanosecond time-wire (the value is truncated
// to seconds by the handler layer); source — native enum type AuditEventSource (huma_enums.go).
type AuditEvent struct {
	ArchonAID     *string                `json:"archon_aid,omitempty" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	CorrelationID *string                `json:"correlation_id,omitempty" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`  // ULID (migration 001)
	CreatedAt     time.Time              `json:"created_at"`
	ID            string                 `json:"id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (migration 001)
	Payload       map[string]interface{} `json:"payload"`
	Source        AuditEventSource       `json:"source"`
	Type          string                 `json:"type"`
}

// AuditEventListReply — the native 200 body of GET /v1/audit (offset-envelope: items/offset/limit/
// total). items — native AuditEvent; offset/limit/total — int (parity with the former legacy generator).
type AuditEventListReply struct {
	Items  []AuditEvent `json:"items"`
	Limit  int          `json:"limit"`
	Offset int          `json:"offset"`
	Total  int          `json:"total"`
}

// === projection of domain handlers.AuditListPage (flat fields) → native wire-DTO ===

// newAuditEvent projects the flat domain handlers.AuditEventView into a native AuditEvent.
// Source — a native enum cast (same underlying string). created_at is already truncated to
// seconds by the handler (byte-exact with the legacy wire).
func newAuditEvent(v handlers.AuditEventView) AuditEvent {
	return AuditEvent{
		ArchonAID:     v.ArchonAID,
		CorrelationID: v.CorrelationID,
		CreatedAt:     v.CreatedAt,
		ID:            v.ID,
		Payload:       v.Payload,
		Source:        AuditEventSource(v.Source),
		Type:          v.Type,
	}
}

// newAuditEventListReply projects the domain handlers.AuditListPage into a native
// AuditEventListReply. Items preserve nil-vs-empty 1:1 (nil → null, [] → []) for byte-exact
// wire (category B ADR-051) — ListTyped yields a non-nil [] (empty feed → `[]`).
func newAuditEventListReply(p handlers.AuditListPage) AuditEventListReply {
	var items []AuditEvent
	if p.Items != nil {
		items = make([]AuditEvent, len(p.Items))
		for i := range p.Items {
			items[i] = newAuditEvent(p.Items[i])
		}
	}
	return AuditEventListReply{
		Items:  items,
		Limit:  p.Limit,
		Offset: p.Offset,
		Total:  p.Total,
	}
}
