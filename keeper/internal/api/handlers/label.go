package handlers

import (
	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
)

// The label mutation, stated once for all ten registries (ADR-0085, NIM-728).
//
// Every registry that carries an identifier now also carries a display caption,
// and every one of them mutates it the same way: PUT /v1/<collection>/{name}/label
// with a body of exactly one optional field, permission <resource>.label-set,
// audit <resource>.label_changed. The shapes below are that contract, written
// once — ten copies of a two-field struct would be ten places for the payload
// key or the null-handling to drift.
//
// A dedicated sub-resource rather than a general PATCH on the row: the
// identifier in the path is not touched, and several of these registries are
// deliberately immutable otherwise (`providers`, `profiles` — "changing
// parameters = delete+create"), so a PATCH on the row would advertise an
// editability they do not have.

// LabelSetInput — NATIVE request form of PUT /v1/<collection>/{name}/label.
//
// One field, and it is a pointer on purpose. A body of `{"label": null}` (or
// `{}`) CLEARS the caption, and the consumer falls back to showing the
// identifier; a body of `{"label": "Redis — Billing (prod)"}` sets it. There is
// no third state, because there is nothing else on this endpoint to leave
// unchanged.
type LabelSetInput struct {
	Label *string
}

// LabelWriteReply — result of a *SetLabelTyped: the 200 body (the row as it now
// reads, in that registry's own view type) plus the audit fields.
//
// Generic over the view because the ten registries return ten different rows
// while sharing one audit payload. ID addresses the row and is never written —
// the identifier is immutable and this endpoint has no way to change it.
type LabelWriteReply[V any] struct {
	Body V
	ID   string
	// Label is the caption as it now reads; Previous is what the row held before
	// the write. Both nil-able, and a nil means the caption was absent on that
	// side of the transition rather than that it is unknown — the pair is read
	// straight off the UPDATE, so it always describes one real change.
	Label    *string
	Previous *string
}

// AuditPayload assembles the audit payload of a label-set route: the identifier
// that was addressed, and the caption on both sides of the change.
//
// `{id, old_label, new_label}`, following `incarnation.traits_changed`
// (`{name, old_keys, new_keys}`) — the event this family is named after. A
// caption is display text and the trail can afford to carry it whole, so unlike
// traits (which record KEYS only, because a value may be sensitive) both values
// are recorded verbatim.
//
// Both are always present and explicitly null where the caption was absent: an
// omitted key would make "there was no caption" and "this event predates the
// field" the same record. The pair comes from a single UPDATE ... RETURNING, so
// it always describes a transition that actually happened — a read-then-write
// could interleave with a concurrent edit and report one that did not.
func (r LabelWriteReply[V]) AuditPayload() middleware.AuditPayload {
	return LabelAuditPayload(r.ID, r.Previous, r.Label)
}

// LabelAuditPayload builds the `<resource>.label_changed` payload from the three
// values it carries.
//
// Split out of the method above because not every label route can BE that
// method: `incarnation.label-set` assembles its own audit event inline
// (incarnation_typed.go) rather than returning a [LabelWriteReply], and its MCP
// twin writes the payload directly. Those two were the exceptions that let the
// key drift — the shared struct was renamed to `id` ([ADR-0085], NIM-729) and
// the two hand-written copies were not, so nine registries wrote `id` and one
// wrote `name` for the same event family.
//
// A function all three call makes the parity a consequence instead of a rule
// somebody has to remember.
func LabelAuditPayload(id string, previous, current *string) middleware.AuditPayload {
	return middleware.AuditPayload{
		"id":        id,
		"old_label": previous,
		"new_label": current,
	}
}
