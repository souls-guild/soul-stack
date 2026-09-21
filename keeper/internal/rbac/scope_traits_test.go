package rbac

import (
	"slices"
	"strings"
	"testing"
)

// NIM-521 / NIM-522 — the trait-scope value rule, pinned WITHOUT Docker.
//
// The live matrix next door (api/trait_scope_matrix_integration_test.go) is what
// proves the two halves of the boundary answer the same thing about a real jsonb
// column, and it is the only place that can: every expectation there is a claim
// about what Postgres DOES. But it is gated behind `//go:build integration` and a
// container runtime, so on a machine without one the entire semantics of this
// rule rests on nothing.
//
// This file is the floor under that. It takes Postgres' serializations as
// LITERALS — the integration matrix is what earns the right to write them down —
// and pins, in `go test ./...` with no tags and no daemon:
//
//	the Go half     — which texts a stored value is reachable by;
//	the write half  — which pairs a payload demands, and that the demanded set is
//	                  always a superset of the reachable one (never laxer);
//	the SQL half    — the rendered predicate's shape, one assertion per mutation
//	                  that would silently widen or break it.

// pgTexts are Postgres' own renderings. Each is asserted live in
// TestIntegration_TraitScopeValueMatrix; here they are the input, because the
// contract of [TraitValues] is "you will be fed exactly these bytes".
type traitCase struct {
	name string
	raw  string // as `SELECT ($1::jsonb)::text` returns it
	key  string
	// values — every text a scope value may name to reach this stored value.
	values []string
	// pairs — every text a write of this payload must find granted. nil means
	// "same as values": the symmetric case, which is every payload the API can
	// actually store.
	pairs []string
}

func traitCases() []traitCase {
	return []traitCase{
		{name: "string", raw: `{"tier": "gold"}`, key: "tier", values: []string{"gold"}},
		{name: "string with spaces", raw: `{"tier": "gold plated"}`, key: "tier", values: []string{"gold plated"}},
		{name: "empty string", raw: `{"tier": ""}`, key: "tier", values: []string{""}},

		// Numbers: the token as jsonb stores it, verbatim. Re-deriving any of
		// these from a decoded float64 is the NIM-401/NIM-521 defect — `1000000`
		// and `1000000.0` collapse onto one float, and the last two do not
		// survive float64 at all.
		{name: "integer", raw: `{"asn": 1000000}`, key: "asn", values: []string{"1000000"}},
		{name: "trailing scale kept", raw: `{"asn": 1000000.0}`, key: "asn", values: []string{"1000000.0"}},
		{name: "beyond float64", raw: `{"n": 12345678901234567890}`, key: "n", values: []string{"12345678901234567890"}},
		{name: "small decimal", raw: `{"ratio": 0.0000001}`, key: "ratio", values: []string{"0.0000001"}},
		{name: "negative", raw: `{"delta": -3}`, key: "delta", values: []string{"-3"}},

		{name: "bool true", raw: `{"active": true}`, key: "active", values: []string{"true"}},
		{name: "bool false", raw: `{"active": false}`, key: "active", values: []string{"false"}},

		// A JSON null: `->>` over it is SQL NULL, equal to no value at all — so
		// nothing reaches it. The write half still demands a pair for it, because
		// contributing nothing would wave the write past the gate entirely.
		{name: "null", raw: `{"gone": null}`, key: "gone", values: nil, pairs: []string{"null"}},

		// Arrays: each SCALAR element, and never the array's own text.
		{name: "string list", raw: `{"env": ["prod", "stage"]}`, key: "env", values: []string{"prod", "stage"}},
		{name: "number list", raw: `{"ports": [6379, 6380]}`, key: "ports", values: []string{"6379", "6380"}},
		{name: "bool list", raw: `{"flags": [true, false]}`, key: "flags", values: []string{"true", "false"}},
		{name: "mixed scalar list", raw: `{"m": ["a", 42, true]}`, key: "m", values: []string{"a", "42", "true"}},
		{name: "empty list", raw: `{"e": []}`, key: "e", values: nil, pairs: nil},

		// Objects and anything nested one level deeper than a value: reachable by
		// nothing, demanded as their own text.
		{name: "object", raw: `{"tier": {"k": "gold"}}`, key: "tier",
			values: nil, pairs: []string{`{"k": "gold"}`}},
		{name: "empty object", raw: `{"o": {}}`, key: "o",
			values: nil, pairs: []string{"{}"}},
		{name: "list with nested containers", raw: `{"m": ["a", ["x"], {"k": "v"}, 42]}`, key: "m",
			values: []string{"a", "42"},
			pairs:  []string{"a", `["x"]`, `{"k": "v"}`, "42"}},
		{name: "list with null", raw: `{"m": ["a", null]}`, key: "m",
			values: []string{"a"}, pairs: []string{"a", "null"}},
		{name: "list of only null", raw: `{"m": [null]}`, key: "m",
			values: nil, pairs: []string{"null"}},
	}
}

func (c traitCase) wantPairs() []string {
	if c.pairs == nil && c.values != nil {
		return c.values
	}
	return c.pairs
}

// The read half: which scope values reach a stored row. An OR set — one match
// makes the row visible — so a text that is here and should not be is a host
// handed to an operator who never held it.
func TestTraitValues_ProjectsPostgresTexts(t *testing.T) {
	for _, c := range traitCases() {
		t.Run(c.name, func(t *testing.T) {
			got := TraitValues([]byte(c.raw))[c.key]
			if !sameSet(got, c.values) {
				t.Errorf("TraitValues(%s)[%q] = %q, want %q\n"+
					"A text that appears here and should not is a row reachable by a scope value "+
					"nobody meant to grant; one that is missing is a row its own operator cannot find.",
					c.raw, c.key, got, c.values)
			}
		})
	}
}

// The write half: which pairs a payload demands be granted. An AND set — every
// one must be inside the operator's trait scope — so a text MISSING here is a
// pair stamped onto a host without authority.
func TestTraitPairTexts_DemandsEveryPair(t *testing.T) {
	for _, c := range traitCases() {
		t.Run(c.name, func(t *testing.T) {
			want := c.wantPairs()
			got := TraitPairTexts([]byte(c.raw))[c.key]
			if !sameSet(got, want) {
				t.Errorf("TraitPairTexts(%s)[%q] = %q, want %q\n"+
					"A pair missing here is one the gate never asks about: the operator stamps it "+
					"onto a host without holding it, and everyone scoped on that pair gains the host.",
					c.raw, c.key, got, want)
			}
		})
	}
}

// The two halves against EACH OTHER, which is the property that actually matters
// and the one neither table above states on its own: the gate must never demand
// LESS than the read side would grant by.
//
// The known-bad this exists for: making TraitPairTexts return nothing for a value
// TraitValues reaches — the shape the write half had before NIM-529, when a list
// contributed only its own text. Each table alone would still pass a change that
// moved a text out of pairs and into values.
func TestTraitPairTexts_IsNeverLaxerThanTraitValues(t *testing.T) {
	for _, c := range traitCases() {
		t.Run(c.name, func(t *testing.T) {
			values := TraitValues([]byte(c.raw))[c.key]
			pairs := TraitPairTexts([]byte(c.raw))[c.key]

			for _, v := range values {
				if !slices.Contains(pairs, v) {
					t.Errorf("%s: a scope value of %q REACHES this row, but writing it demands only %q — "+
						"so an operator who cannot see the row can still create it, and hand it to whoever can",
						c.raw, v, pairs)
				}
			}

			// The difference in the other direction is deliberate, and the two
			// tables above pin exactly what it is: container and null texts, none
			// of which any value renders. It must not be empty where the tables
			// say it is not, or the extra demand has quietly been dropped.
			if len(pairs) < len(values) {
				t.Errorf("%s: the write half demands %d pairs for %d reachable values — it cannot "+
					"demand fewer than the read side grants by", c.raw, len(pairs), len(values))
			}
		})
	}
}

// Both halves blind to the same non-answers. A trait condition over any of these
// fails closed (ADR-047), matching the SQL branch over a NULL traits column.
func TestTraitProjections_FailClosed(t *testing.T) {
	blind := []struct {
		name string
		raw  string
	}{
		{"nil", ""},
		{"empty object", "{}"},
		{"not an object", `[1, 2]`},
		{"a bare scalar", `"gold"`},
		{"truncated", `{"tier": "go`},
		{"garbage", "not json at all"},
		{"sql NULL rendered as the word", "null"},
	}
	for _, b := range blind {
		t.Run(b.name, func(t *testing.T) {
			if got := TraitValues([]byte(b.raw)); got != nil {
				t.Errorf("TraitValues(%q) = %v, want nil — a row keeper cannot read must be inside no trait scope", b.raw, got)
			}
			if got := TraitPairTexts([]byte(b.raw)); got != nil {
				t.Errorf("TraitPairTexts(%q) = %v, want nil — such a payload writes no trait, so it may demand none", b.raw, got)
			}
		})
	}
}

// Sibling keys are independent, and an absent key reaches nothing whatever its
// neighbours carry.
func TestTraitValues_KeysAreIndependent(t *testing.T) {
	got := TraitValues([]byte(`{"tier": "gold", "env": ["prod", "stage"], "gone": null, "o": {}}`))

	if !sameSet(got["tier"], []string{"gold"}) || !sameSet(got["env"], []string{"prod", "stage"}) {
		t.Fatalf("TraitValues = %v, want tier=[gold] env=[prod stage]", got)
	}
	if _, ok := got["gone"]; ok {
		t.Errorf("a null key is present in %v — it reaches nothing and must not be offered", got)
	}
	if _, ok := got["o"]; ok {
		t.Errorf("an object key is present in %v — a scope value is a value, never a key", got)
	}
	if v, ok := got["absent"]; ok {
		t.Errorf("an absent key yielded %q — `trait.absent=x` would reach a row that carries no `absent`", v)
	}
}

// The SQL half's shape, pinned per mutation. Each block below names what
// removing it would let through; the exact-text pin at the end catches the
// mutations nobody enumerated.
func TestTraitScopeSQL_ShapePins(t *testing.T) {
	const (
		col  = "s.traits"
		key  = "$1"
		vals = "$2"
	)
	got := TraitScopeSQL(col, key, vals)
	t.Logf("predicate under test:\n%s", got)

	scalarArm, elemArm, found := strings.Cut(got, " OR EXISTS ")
	if !found {
		t.Fatalf("the predicate no longer has two arms — a scalar value and a list element are "+
			"reached differently and both must be present:\n%s", got)
	}

	for _, pin := range []struct {
		arm      string
		armName  string
		fragment string
		mutation string
	}{
		{scalarArm, "scalar arm", "jsonb_typeof(s.traits -> $1) NOT IN ('object', 'array')",
			"without it `->>` over a container yields the container's OWN text, so " +
				`trait.env='["prod", "stage"]' reaches a row no value of which is named — the NIM-522 defect`},
		{scalarArm, "scalar arm", "s.traits ->> $1 = ANY($2)",
			"a scope binds a SET of values; comparing against one drops the rest of the OR"},

		{elemArm, "element arm", "jsonb_array_elements(",
			"without it only whole scalars are reachable and `trait.ports=6379` stops reaching {\"ports\": [6379, 6380]}"},
		{elemArm, "element arm", "CASE WHEN jsonb_typeof(s.traits -> $1) = 'array'",
			"jsonb_array_elements ERRORS on a scalar rather than returning no rows, so without the CASE " +
				"every list under a trait scope 500s instead of filtering"},
		{elemArm, "element arm", "ELSE '[]'::jsonb END",
			"the non-array branch has to be an EMPTY array; anything else feeds rows to the EXISTS"},
		{elemArm, "element arm", "jsonb_typeof(_trait_elem) NOT IN ('object', 'array')",
			"without it a NESTED container's own text becomes a reachable value, so " +
				`trait.m='{"k": "v"}' reaches {"m": ["a", {"k": "v"}]} — one level deeper than a value`},
		{elemArm, "element arm", "_trait_elem #>> '{}' = ANY($2)",
			"`#>> '{}'` is the element's text as Postgres renders it; a cast or `::text` would " +
				"bring a string element back QUOTED and no scope value would ever match it"},
	} {
		if !strings.Contains(pin.arm, pin.fragment) {
			t.Errorf("the %s no longer contains %q — %s\nfull predicate:\n%s",
				pin.armName, pin.fragment, pin.mutation, got)
		}
	}

	// The element arm must unnest the CASE directly. Wrapping anything else in
	// jsonb_array_elements — the row's raw column, a coalesce, a join — is how a
	// scalar reaches it again.
	if !strings.Contains(elemArm, "jsonb_array_elements(CASE WHEN") {
		t.Errorf("jsonb_array_elements no longer unnests the CASE directly:\n%s", got)
	}

	// One rule, one column. Every jsonb reach in the predicate is the row's own
	// traits column under the bound key; a second column here is a label the
	// operator never attached to this row (NIM-281).
	if n := strings.Count(got, col+" -> "+key); n != 3 {
		t.Errorf("the predicate reaches `%s -> %s` %d times, want 3 (scalar typeof, CASE typeof, CASE value):\n%s",
			col, key, n, got)
	}
	if n := strings.Count(got, "s.traits"); n != 4 {
		t.Errorf("the predicate names %s %d times, want 4 — an extra one is a source of rows "+
			"outside this column:\n%s", col, n, got)
	}
	if n := strings.Count(got, vals); n != 2 {
		t.Errorf("the values placeholder appears %d times, want 2 (once per arm) — both arms must be "+
			"bound to the SAME set, or a scope means one thing for scalars and another for elements:\n%s", n, got)
	}

	const want = "((jsonb_typeof(s.traits -> $1) NOT IN ('object', 'array') AND s.traits ->> $1 = ANY($2))" +
		" OR EXISTS (SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(s.traits -> $1) = 'array'" +
		" THEN s.traits -> $1 ELSE '[]'::jsonb END) AS _trait_elem" +
		" WHERE jsonb_typeof(_trait_elem) NOT IN ('object', 'array')" +
		" AND _trait_elem #>> '{}' = ANY($2)))"
	if got != want {
		t.Errorf("the rendered predicate changed.\n got: %s\nwant: %s\n\n"+
			"This is a deliberate pin: [TraitValues] reproduces THIS text's answer in Go, and the two "+
			"drifting apart is the whole of NIM-401/NIM-521. If the change is intended, re-run "+
			"TestIntegration_TraitScopeValueMatrix against a live Postgres, update rbac.TraitValues to "+
			"match, and only then update this string.", got, want)
	}
}

// The predicate must be parenthesised as a unit: it is OR-ed and AND-ed into a
// larger scope expression, and an unbracketed `OR` inside it would bind past its
// own condition and grant across a conjunction.
func TestTraitScopeSQL_IsSelfContained(t *testing.T) {
	got := TraitScopeSQL("s.traits", "$1", "$2")

	if !strings.HasPrefix(got, "(") || !strings.HasSuffix(got, ")") {
		t.Fatalf("the predicate is not bracketed as a unit — its inner OR would escape into the "+
			"surrounding AND and grant where the conjunction refuses:\n%s", got)
	}
	depth := 0
	for i := 0; i < len(got); i++ {
		switch got[i] {
		case '(':
			depth++
		case ')':
			depth--
		}
		if depth == 0 && i < len(got)-1 {
			t.Fatalf("the predicate closes its outermost bracket at byte %d, before its end — "+
				"the tail is outside the unit:\n%s", i, got)
		}
	}
	if depth != 0 {
		t.Fatalf("unbalanced parentheses in:\n%s", got)
	}
}

func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	g := slices.Clone(got)
	w := slices.Clone(want)
	slices.Sort(g)
	slices.Sort(w)
	return slices.Equal(g, w)
}
