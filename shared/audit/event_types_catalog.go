package audit

// AllEventTypes returns every [EventType] keeper can write, ordered by wire value.
//
// The catalog is DERIVED from the constant declarations in event_types.go by
// `make gen-audit-catalog` (see event_types_gen_test.go for why it is generated
// and what goes red when it is not). Adding a constant is the only edit needed —
// regenerating is a mechanical step the gate demands, not a second list to keep.
//
// The Operator API publishes this set as the OpenAPI enum of `AuditEvent.type`
// (NIM-346), which is what lets a client prove it can label every event keeper
// emits.
//
// There is deliberately no membership helper alongside it. Unlike [Source.Valid],
// which the write path enforces before INSERT, the event-type catalog is open by
// design (ADR-022(g) — names arrive as each write-path subsystem is normalized) and
// history outlives it: `audit_log` holds rows written under types since retired, and
// a reader that rejected them would hide the trail rather than validate it. Adding a
// Known() would invite exactly the check that must not exist.
//
// The returned slice is a copy: it backs a published contract enum, so a caller
// sorting or truncating it must not be able to change what anybody else sees.
func AllEventTypes() []EventType {
	out := make([]EventType, len(allEventTypes))
	copy(out, allEventTypes)
	return out
}
