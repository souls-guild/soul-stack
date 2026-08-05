package mcp

// MCP wire form of a rule's SUBJECT — the one shape shared by all three
// subject-bearing registries (vigils / decrees / rites, NIM-280), mirroring the REST
// `Subject` object field-for-field so an operator can move between the two surfaces
// without relearning the vocabulary.
//
// It is nested for the same reason it is nested in REST: two of the four dimensions
// are pairs that mean nothing half-written (an incarnation is service+name, a trait is
// key+value), and nesting makes the half-written form unspellable rather than a
// deferred validation error. `subject.incarnation` also stays visibly distinct from a
// Decree's top-level `incarnation_name` — the reaction's TARGET, the opposite end of
// the rule.

import (
	"github.com/souls-guild/soul-stack/keeper/internal/subject"
)

// subjectPayload — WHO a Vigil, Decree or Rite applies to; used for both tool
// arguments and tool output (the constraints are the same in both directions, so one
// type keeps them from drifting). EXACTLY ONE of the four keys is populated; zero or
// two comes back from the Service as validation-failed ([subject.Validate]).
//
// The two label dimensions read BOTH levels: the rule reaches a host carrying the
// label itself, and every member of an incarnation carrying it. Targeting only — an
// operator's RBAC scope is resolved elsewhere and stays "a label on a host", so
// labelling an incarnation never widens what an ARCHON may do.
type subjectPayload struct {
	Coven       []string             `json:"coven,omitempty"`
	Incarnation *subjectIncarnation  `json:"incarnation,omitempty"`
	SID         []string             `json:"sid,omitempty"`
	Trait       *subjectTraitPayload `json:"trait,omitempty"`
}

// subjectIncarnation — the `service.incarnation` address. Both halves are required:
// an incarnation name is unique only WITHIN its service.
type subjectIncarnation struct {
	Service string `json:"service"`
	Name    string `json:"name"`
}

// subjectTraitPayload — one trait key/value pair. A scalar trait matches by its text
// rendering, a list trait by membership (mirroring Postgres).
type subjectTraitPayload struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// selector converts the tool arguments into the domain selector. It deliberately does
// NOT validate: an empty or over-specified subject reaches [subject.Validate] in the
// Service and comes back as one diagnostic, so the transport never holds a second
// opinion about what is legal.
func (s subjectPayload) selector() subject.Selector {
	sel := subject.Selector{SIDs: s.SID, Covens: s.Coven}
	if s.Incarnation != nil {
		sel.Service, sel.Incarnation = s.Incarnation.Service, s.Incarnation.Name
	}
	if s.Trait != nil {
		sel.TraitKey, sel.TraitValue = s.Trait.Key, s.Trait.Value
	}
	return sel
}

// toSubjectPayload projects a stored selector back onto the wire. Only the dimension
// the rule was written with is emitted, so the output says what the rule IS rather
// than which columns happen to be NULL.
func toSubjectPayload(sel subject.Selector) subjectPayload {
	out := subjectPayload{SID: sel.SIDs, Coven: sel.Covens}
	if sel.Service != "" || sel.Incarnation != "" {
		out.Incarnation = &subjectIncarnation{Service: sel.Service, Name: sel.Incarnation}
	}
	if sel.TraitKey != "" || sel.TraitValue != "" {
		out.Trait = &subjectTraitPayload{Key: sel.TraitKey, Value: sel.TraitValue}
	}
	return out
}

// schemaSubjectProperty — the JSON Schema fragment for the `subject` property, spliced
// into every Vigil / Decree / Rite tool schema (input and output alike). One literal
// so the six schemas cannot drift; the exactly-one-of rule is stated in the description
// rather than as `oneOf`, because a hand-written `oneOf` over four optional keys reads
// far worse in a tool listing than the sentence does, and the Service enforces it either
// way.
const schemaSubjectProperty = `"subject":{"type":"object","additionalProperties":false,` +
	`"description":"WHO the rule applies to - exactly one of sid / incarnation / coven / trait. ` +
	`A coven or trait reaches a host carrying the label AND every member of an incarnation carrying it; ` +
	`an incarnation reaches its members; a sid reaches exactly those hosts.",` +
	`"properties":{` +
	`"sid":{"type":"array","items":{"type":"string"},"description":"Exact SIDs."},` +
	`"incarnation":{"type":"object","additionalProperties":false,"required":["service","name"],` +
	`"description":"service+name pair; the name is unique only within the service.",` +
	`"properties":{` +
	`"service":{"type":"string","pattern":"^[a-z0-9][a-z0-9-]{0,62}$"},` +
	`"name":{"type":"string","pattern":"^[a-z0-9][a-z0-9-]{0,62}$"}}},` +
	`"coven":{"type":"array","items":{"type":"string"},"description":"Coven labels; any one of them matches."},` +
	`"trait":{"type":"object","additionalProperties":false,"required":["key","value"],` +
	`"description":"Trait key/value; a scalar trait matches by text, a list trait by membership.",` +
	`"properties":{` +
	`"key":{"type":"string","pattern":"^[a-z][a-z0-9_.-]*$"},` +
	`"value":{"type":"string"}}}}}`
