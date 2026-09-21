package api

// HUMA-NATIVE wire-DTO of a rule's SUBJECT — the one shape shared by all three
// subject-bearing registries (vigils / decrees / rites, NIM-280). Code-first source of
// the OpenAPI `Subject` schema.
//
// WHY NESTED, and why one type. The subject is one concept with four alternative
// spellings, and two of them are pairs that mean nothing apart (an incarnation is
// service+name; a trait is key+value). Nesting them makes a half-written subject
// unspellable instead of a 422, and it keeps `subject.incarnation` visibly distinct from
// a Decree's top-level `incarnation_name` — the TARGET of the reaction, an entirely
// different field (the DB spells that difference with a `subject_` column prefix).
//
// The type is shared between request bodies and response views deliberately, unlike the
// rest of this package: the constraints are identical on both sides (they come from the
// same [subject.Validate]), so there is no input-422 risk from a documentation-only
// pattern, and a client sees ONE `Subject` schema rather than six near-identical ones.

import (
	"github.com/souls-guild/soul-stack/keeper/internal/subject"
)

// Subject — WHO a Vigil, Decree or Rite applies to. EXACTLY ONE of the four keys is
// present; the service rejects zero or two with 422 ([subject.Validate], symmetric with
// the `*_subject_one_of` CHECKs).
//
// The two label dimensions (coven / trait) read BOTH levels: a rule reaches a host that
// carries the label itself, and every member of an incarnation that carries it. That is
// the point of NIM-280 — "the hosts of this incarnation" became expressible again after
// NIM-281 removed label inheritance.
//
// ⚠ The label namespace is shared, so tagging an incarnation `prod` widens every existing
// `coven: [prod]` SUBJECT to its members with no rule edited. This is targeting only: an
// operator's RBAC scope (`soul.list on coven=prod`) is resolved by a different code path
// and stays "a label on a host", so no labelling ever widens what an ARCHON may do.
type Subject struct {
	Coven       []string            `json:"coven,omitempty" doc:"Coven labels — reaches a host carrying one of them, and every member of an incarnation carrying one"`
	Incarnation *SubjectIncarnation `json:"incarnation,omitempty" doc:"service+name pair — reaches every host that is a member of that incarnation"`
	SID         []string            `json:"sid,omitempty" doc:"exact SIDs — reaches those hosts and nothing else"`
	Trait       *SubjectTrait       `json:"trait,omitempty" doc:"trait key/value — the same two-level reach as coven, on the traits map"`
}

// SubjectIncarnation — the `service.incarnation` address. Both halves are required: an
// incarnation name is only unique WITHIN its service, so the service is part of the
// address and not decoration.
type SubjectIncarnation struct {
	Name    string `json:"name" required:"true" pattern:"^[a-z0-9][a-z0-9-]{0,62}$" doc:"incarnation name (unique within the service)"`
	Service string `json:"service" required:"true" pattern:"^[a-z0-9][a-z0-9-]{0,62}$" doc:"service the incarnation belongs to"`
}

// SubjectTrait — one trait key/value pair. Matching mirrors Postgres: a scalar trait
// matches by its text rendering, a list trait by membership.
type SubjectTrait struct {
	Key   string `json:"key" required:"true" pattern:"^[a-z][a-z0-9_.-]*$" doc:"trait key"`
	Value string `json:"value" required:"true" doc:"trait value; a scalar trait matches by text, a list trait by membership"`
}

// selector converts the wire form into the domain selector. It does NOT validate — an
// empty or over-specified subject reaches [subject.Validate] in the service and comes
// back as a 422 with a diagnostic, so the wire layer never has a second opinion about
// what is legal.
func (s Subject) selector() subject.Selector {
	sel := subject.Selector{SIDs: s.SID, Covens: s.Coven}
	if s.Incarnation != nil {
		sel.Service, sel.Incarnation = s.Incarnation.Service, s.Incarnation.Name
	}
	if s.Trait != nil {
		sel.TraitKey, sel.TraitValue = s.Trait.Key, s.Trait.Value
	}
	return sel
}

// newSubject projects a stored selector back onto the wire. Only the dimension the rule
// was written with is emitted; the other three keys are omitted entirely, so a response
// says what the rule IS rather than which columns happen to be NULL.
func newSubject(sel subject.Selector) Subject {
	out := Subject{SID: sel.SIDs, Coven: sel.Covens}
	if sel.Service != "" || sel.Incarnation != "" {
		out.Incarnation = &SubjectIncarnation{Service: sel.Service, Name: sel.Incarnation}
	}
	if sel.TraitKey != "" || sel.TraitValue != "" {
		out.Trait = &SubjectTrait{Key: sel.TraitKey, Value: sel.TraitValue}
	}
	return out
}
