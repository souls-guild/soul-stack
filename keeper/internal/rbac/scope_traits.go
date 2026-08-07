package rbac

import (
	"bytes"
	"encoding/json"
)

// TraitValues projects a row's `traits` jsonb onto the [ScopeInput.Traits] shape
// (key → the texts a scope value may match), so that an in-Go [Purview.Match]
// answers EXACTLY what the [PurviewSQL] pushdown answers for the same row.
//
// The trait dimension renders to SQL as
//
//	<traits> ->> <key> = ANY(<values>)  OR  <traits> -> <key> ?| <values>
//
// and the two branches reach different things, so the projection must reproduce
// both — a value is matchable when it is what `->>` yields OR what `?|` finds:
//
//	string   → the string                      (`->>`)
//	number   → its token, VERBATIM              (`->>`)
//	bool     → "true" / "false"                 (`->>`)
//	null     → nothing: `->>` is SQL NULL, which equals no value
//	array    → the array's own text, PLUS its STRING elements (`?|` skips
//	           numbers and booleans inside an array — `[6379]` is unreachable
//	           by `trait.ports=6379`)
//	object   → the object's own text, PLUS its TOP-LEVEL KEYS (`?|` over an
//	           object matches keys, not values)
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

	case '[': // array — its own text, plus the string elements `?|` reaches.
		texts := []string{string(val)}
		var elems []json.RawMessage
		if json.Unmarshal(val, &elems) != nil {
			return texts
		}
		for _, e := range elems {
			e = bytes.TrimSpace(e)
			if len(e) == 0 || e[0] != '"' {
				continue // `?|` matches STRING elements only.
			}
			var s string
			if json.Unmarshal(e, &s) == nil {
				texts = append(texts, s)
			}
		}
		return texts

	case '{': // object — its own text, plus the top-level keys `?|` reaches.
		texts := []string{string(val)}
		var obj map[string]json.RawMessage
		if json.Unmarshal(val, &obj) != nil {
			return texts
		}
		for k := range obj {
			texts = append(texts, k)
		}
		return texts

	default: // number or bool — `->>` yields the token as jsonb stores it.
		return []string{string(val)}
	}
}
