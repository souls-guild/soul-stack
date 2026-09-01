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
// while sharing one audit payload. Name addresses the row and is never written —
// the identifier is immutable and this endpoint has no way to change it.
type LabelWriteReply[V any] struct {
	Body  V
	Name  string
	Label *string
}

// AuditPayload assembles the audit payload of a label-set route: the identifier
// that was addressed and the caption as it now reads.
//
// `label` is always present, and it is explicitly null when the caption was
// cleared — an omitted key would make "cleared" and "this event predates the
// field" the same record. Only the new value is carried, matching the one other
// free-text mutation in the tree (`synod.updated` records `{name, description}`).
// The caption is operator-written display text, never a secret, so it is stored
// as it reads.
func (r LabelWriteReply[V]) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"name":  r.Name,
		"label": r.Label,
	}
}
