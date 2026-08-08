package rbac

import (
	"bytes"
	"encoding/json"
)

// TraitValues projects a row's `traits` jsonb onto the [ScopeInput.Traits] shape
// (key → the texts a scope value may match), so that an in-Go [Purview.Match]
// answers EXACTLY what the [PurviewSQL] pushdown answers for the same row.
//
// The rule is [TraitScopeSQL]'s, restated in Go — `trait.<key>=<value>` reaches
// a WHOLE value and only a whole value (NIM-522):
//
//	string   → the string
//	number   → its token, VERBATIM
//	bool     → "true" / "false"
//	null     → nothing: `->>` is SQL NULL, which equals no value
//	array    → the text of each SCALAR element, string/number/bool alike — and
//	           NOT the array's own text (`->>` spells it `["prod", "stage"]`,
//	           but that string is a rendering of the container, not a value in
//	           it, so naming it grants nothing)
//	object   → nothing. A scope value is a value, never a key: `trait.tier=k`
//	           does not reach `{"tier": {"k": "gold"}}`.
//
// Anything nested inside a container is likewise nothing: only the elements one
// level down, and only the scalar ones, are values.
//
// raw MUST be Postgres' own serialization of that jsonb (the bytes a `SELECT
// traits` returns), because the texts are taken from it VERBATIM — that is the
// whole point. `->>` emits a number exactly as jsonb stores it, and jsonb keeps
// the numeric token's scale: `1e6` reads back as `1000000`, while `1000000.0`
// stays `1000000.0` and does NOT match a scope value of `1000000`. Re-deriving
// those texts from a decoded map cannot work: `encoding/json` turns a number
// into a float64, and no float formatting recovers the token (`1000000` and
// `1000000.0` collapse onto the same float, and `12345678901234567890` or
// `0.12345678901234567890123` do not survive float64 at all). Slicing the raw
// bytes sidesteps every one of those questions — including how jsonb spaces and
// orders a container it prints — because it copies the answer instead of
// recomputing it.
//
// A nil/empty/unparsable raw yields nil, and a trait condition then fails closed
// (ADR-047), the same as the SQL branch over a NULL traits column.
func TraitValues(raw []byte) map[string][]string {
	var top map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &top) != nil {
		return nil
	}
	out := make(map[string][]string, len(top))
	for key, val := range top {
		if texts := traitTexts(val); len(texts) > 0 {
			out[key] = texts
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// traitTexts returns every text one trait value is matchable by — see
// [TraitValues] for the rule per JSON kind. The value's own text is taken
// verbatim from raw, never re-encoded.
func traitTexts(raw json.RawMessage) []string {
	val := bytes.TrimSpace(raw)
	if len(val) == 0 {
		return nil
	}
	switch val[0] {
	case 'n': // null — `->>` yields SQL NULL, which is equal to nothing.
		return nil

	case '"': // string scalar — `->>` yields it unquoted.
		var s string
		if json.Unmarshal(val, &s) != nil {
			return nil
		}
		return []string{s}

	case '[': // array — each SCALAR element's text; not the array's own text.
		var elems []json.RawMessage
		if json.Unmarshal(val, &elems) != nil {
			return nil
		}
		texts := make([]string, 0, len(elems))
		for _, e := range elems {
			if t, ok := traitScalarText(e); ok {
				texts = append(texts, t)
			}
		}
		return texts

	case '{': // object — nothing: a scope value names a value, never a key.
		return nil

	default: // number or bool — `->>` yields the token as jsonb stores it.
		return []string{string(val)}
	}
}

// traitScalarText returns one array element's text as `e #>> '{}'` renders it,
// and false for a nested container — which is reachable by no scope value, the
// same way a top-level object is.
func traitScalarText(raw json.RawMessage) (string, bool) {
	val := bytes.TrimSpace(raw)
	if len(val) == 0 {
		return "", false
	}
	switch val[0] {
	case '[', '{': // nested container — not a value.
		return "", false
	case 'n': // null — `#>>` is SQL NULL, which equals no value.
		return "", false
	case '"':
		var s string
		if json.Unmarshal(val, &s) != nil {
			return "", false
		}
		return s, true
	default: // number or bool — the token as jsonb stores it.
		return string(val), true
	}
}

// TraitPairTexts projects a trait PAYLOAD's jsonb onto the pairs a write gate
// has to find granted: key → every text that key contributes as a `<key>=<text>`
// pair. It is the WRITE twin of [TraitValues], and the two answer different
// questions, which is why they are two functions:
//
//	[TraitValues]  — read: "which scope values REACH this stored row?" An OR
//	                 set: any one of them matching makes the row visible, so a
//	                 value the rule does not reach contributes NOTHING and the
//	                 row stays invisible.
//	TraitPairTexts — write: "which pairs is this operator stamping onto a host?"
//	                 An AND set: every one of them must be inside the operator's
//	                 trait-scope, because one out-of-scope element grants exactly
//	                 as much as a whole out-of-scope key would. A value the rule
//	                 does not reach must therefore contribute an UNSATISFIABLE
//	                 pair, not nothing — skipping it would wave the write past
//	                 the gate.
//
// Per JSON kind:
//
//	string / number / bool → one pair, the text `->>` yields
//	array                  → one pair per element, each element's own `->>` text
//	null                   → one pair, the token `null`
//	object / nested        → the container's own text, verbatim
//
// The last two lines are where the two sets part, and they part toward refusal.
// [TraitValues] yields NOTHING for a null, an object or a nested container
// (NIM-522 — no scope value reaches one), so what is demanded here is a text the
// read side would never have granted by: the value's own text.
//
// Usually no scope can even carry such a text — a `"` cannot appear inside a
// quoted scope value and a bareword holds only [reScopeExact] characters — but
// `{}` and `[1, 2]` are quotable and `null` is a legal bareword, so
// "unsatisfiable" is NOT the invariant and must not be claimed as one. The true
// invariant is weaker and enough: the demanded set is always a SUPERSET of what
// [TraitValues] reaches for the same value, so the gate is never laxer than the
// read it guards.
//
// The difference is NOT a set of texts nothing renders — do not claim that. `null`
// and `{}` are demanded here for a JSON null and an empty object, and both are also
// what [TraitValues] yields for the STRINGS `"null"` and `"{}"`, so a scope
// `trait.k=null` is satisfiable and reaches the string. What the difference means is
// only this: for THIS value the gate asks for a text the read side will not grant by,
// so holding it lets the operator write a row that this value's own scope then
// reaches by nothing.
//
// None of these payloads is reachable through the API to begin with:
// `soul.ValidTraitValue` refuses a nil and anything but a scalar or a list of
// scalars. If one arrives by another road it is measured, not waved past.
//
// raw MUST be Postgres' own serialization of the payload about to be written —
// what `SELECT $1::jsonb` returns for it — because the texts are taken from it
// VERBATIM. That is the whole point: the gate has to speak about the row that
// WILL exist, and only Postgres knows how it will spell it. jsonb re-canonicalizes
// a number rather than keeping the caller's token: `1e-7` is stored (and read
// back by `->>`) as `0.0000001`, `1e+21` as `1000000000000000000000`. Rendering
// the pair in Go instead cannot reach those texts — `encoding/json` decodes every
// JSON number into float64 and `fmt` then prints `1e-07` / `1.2345678901234567e+19`,
// which no operator's scope ever names, so the write is refused for a pair the
// operator plainly holds (NIM-529).
//
// A nil/empty/unparsable raw yields nil: no pairs, so the gate finds nothing to
// object to — correct, since such a payload writes no trait either.
//
// NOTE what is deliberately NOT a pair here: an array's OWN text. Requiring it
// would refuse every legitimate list write — a scope grants `trait.env=prod`,
// never `trait.env="[\"prod\", \"stage\"]"`. This used to be an asymmetry, because
// the read side DID match a container's own text; NIM-522 settled the fork the
// other way and [TraitValues] no longer does, so the two sides now name the same
// texts for every value the API can store.
func TraitPairTexts(raw []byte) map[string][]string {
	var top map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &top) != nil {
		return nil
	}
	out := make(map[string][]string, len(top))
	for key, val := range top {
		if texts := traitPairTexts(val); len(texts) > 0 {
			out[key] = texts
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// traitPairTexts returns the pairs one trait value contributes — see
// [TraitPairTexts] for the rule per JSON kind. Texts come from raw verbatim,
// never re-encoded.
func traitPairTexts(raw json.RawMessage) []string {
	val := bytes.TrimSpace(raw)
	if len(val) == 0 {
		return nil
	}
	switch val[0] {
	case '"': // string scalar — `->>` yields it unquoted.
		var s string
		if json.Unmarshal(val, &s) != nil {
			return nil
		}
		return []string{s}

	case '[': // array — one pair per element, each as `->>` would yield it.
		var elems []json.RawMessage
		if json.Unmarshal(val, &elems) != nil {
			return []string{string(val)} // unparsable: fail closed on its own text.
		}
		out := make([]string, 0, len(elems))
		for _, e := range elems {
			out = append(out, traitElemText(e))
		}
		return out

	case '{': // object — not writable through the API; fail closed on its text.
		return []string{string(val)}

	default: // number, bool or null — the token as jsonb stores it.
		return []string{string(val)}
	}
}

// traitElemText renders ONE list element as the single pair it contributes. A
// nested container is NOT descended into — it yields its own text, so a list of
// lists is refused rather than flattened into pairs its elements never formed.
func traitElemText(raw json.RawMessage) string {
	val := bytes.TrimSpace(raw)
	if len(val) == 0 {
		return ""
	}
	if val[0] == '"' {
		var s string
		if json.Unmarshal(val, &s) == nil {
			return s
		}
	}
	return string(val)
}
