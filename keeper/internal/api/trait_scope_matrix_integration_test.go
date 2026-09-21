//go:build integration

// NIM-522 — the trait-scope value rule, stated once and checked against Postgres.
//
// The two agreement guards next to this file (roster ↔ souls-list, incarnation
// list ↔ get) prove that the two READERS of the RBAC boundary answer the same
// thing. They do it end-to-end, through the routes, which is what makes them
// convincing — and also what makes them expensive: each case costs a seeded row,
// a role, a token and two HTTP calls, so they can carry a dozen JSON shapes, not
// fifty.
//
// This test is the same claim at the other end of that trade. It skips the
// routes entirely and puts `rbac.TraitScopeSQL` and `rbac.TraitValues` side by
// side over a wide matrix of jsonb shapes, so the shapes that are awkward to
// seed as a fleet — an empty array, an object with no keys, a list mixing
// scalars with nested containers, a JSON null beside a real value — are covered
// somewhere. Where the agreement guards ask "do the readers agree about this
// host", this asks "do the two renderings of the rule agree about this VALUE".
//
// Three things are asserted per probe, and the third is the one that is easy to
// leave out:
//
//	SQL == Go        — the divergence class itself (NIM-401 / NIM-521).
//	SQL == the rule  — both halves wrong in the same direction is still wrong,
//	Go  == the rule    and comparing them to each other alone would not see it.
//
// Live Postgres is not decoration here. Every expectation below is a claim about
// what jsonb DOES — `->>` over a container yields the container's rendered text,
// `#>> '{}'` over a scalar element yields it unquoted, a number is re-canonicalized
// on the way in (`1e6` reads back `1000000`) while its scale survives
// (`1000000.0` does not become `1000000`), and `jsonb_array_elements` ERRORS on a
// non-array rather than returning no rows. A fake would have to reproduce all of
// that to be worth asking, at which point it is a second implementation of the
// thing under test.
//
// Run:
//
//	cd keeper && SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1 \
//	    go test -tags=integration -race -count=1 ./internal/api/ \
//	    -run TestIntegration_TraitScopeValueMatrix

package api

import (
	"context"
	"fmt"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

// traitMatrixProbe — one scope value against one stored trait, plus what the
// NIM-522 rule says the answer is. The rule, restated: a scope value names a
// WHOLE value — a scalar, or one SCALAR element of an array. Never a key, never
// a container's own rendered text, never anything nested deeper than one level.
type traitMatrixProbe struct {
	value string
	want  bool
	why   string
}

type traitMatrixFixture struct {
	traits string
	key    string
	probes []traitMatrixProbe
}

func traitMatrixFixtures() []traitMatrixFixture {
	return []traitMatrixFixture{
		{`{"tier":"gold"}`, "tier", []traitMatrixProbe{
			{"gold", true, "the plain case"},
			{"silver", false, "a different string"},
			{"", false, "the empty scope value is a value like any other, and no trait carries it"},
		}},
		{`{"tier":"gold"}`, "other", []traitMatrixProbe{
			{"gold", false, "an absent key reaches nothing, whatever the sibling keys carry"},
		}},

		// Numbers — the NIM-401/NIM-521 half. jsonb keeps the token, and a Go
		// reader that re-derives it from a float64 cannot reproduce any of these.
		{`{"asn":1000000}`, "asn", []traitMatrixProbe{
			{"1000000", true, "the token as stored"},
			{"1e+06", false, "what fmt prints for the decoded float64 — the NIM-521 defect, spelled out"},
			{"1000000.0", false, "a different token, and jsonb keeps the difference"},
		}},
		{`{"asn":1e6}`, "asn", []traitMatrixProbe{
			{"1000000", true, "jsonb re-canonicalizes on the way IN, so this is the same value as above"},
			{"1e6", false, "the caller's spelling is not what is stored"},
		}},
		{`{"asn":1000000.0}`, "asn", []traitMatrixProbe{
			{"1000000.0", true, "the scale survives"},
			{"1000000", false, "and is therefore load-bearing: this is a different value"},
		}},
		{`{"asn":12345678901234567890}`, "asn", []traitMatrixProbe{
			{"12345678901234567890", true, "outside float64 entirely — unreachable by any re-derivation"},
		}},
		{`{"ratio":1e-7}`, "ratio", []traitMatrixProbe{
			{"0.0000001", true, "what jsonb stores"},
			{"1e-07", false, "what fmt prints"},
			{"1e-7", false, "what encoding/json prints"},
		}},

		{`{"active":true}`, "active", []traitMatrixProbe{
			{"true", true, "a bool renders as its `->>` text"},
			{"false", false, "and the other one is a different value"},
		}},

		// JSON null — `->>` is SQL NULL, and SQL NULL equals nothing, including
		// the string "null". Fail-closed by construction rather than by a branch.
		{`{"gone":null}`, "gone", []traitMatrixProbe{
			{"null", false, "the word `null` is a string; the stored value is not"},
			{"", false, "nor is it the empty string"},
		}},

		// Arrays — each SCALAR element, never the array's own text.
		{`{"env":["prod","stage"]}`, "env", []traitMatrixProbe{
			{"prod", true, "an element is a value"},
			{"stage", true, "every element, not just the first"},
			{"dev", false, "an element that is not there"},
			{`["prod", "stage"]`, false, "the container's own text renders the container; it names no value in it"},
		}},
		{`{"ports":[6379,6380]}`, "ports", []traitMatrixProbe{
			{"6379", true, "NIM-522's new yes: a NUMERIC element is a value like any other"},
			{"6380", true, "the same, and it is the last element"},
			{"6381", false, "a number that is not in the list"},
			{`[6379, 6380]`, false, "NIM-522's other new no — and this text was the ONLY way to address this row before"},
		}},
		{`{"flags":[true,false]}`, "flags", []traitMatrixProbe{
			{"true", true, "a BOOL element is a value too"},
			{"false", true, "including the false one, which is not a special case"},
		}},
		{`{"e":[]}`, "e", []traitMatrixProbe{
			{"", false, "an empty array contributes no element"},
			{"[]", false, "and its own text is still a container's text"},
		}},

		// Objects — a key is not a value, and neither is what it maps to.
		{`{"tier":{"k":"gold"}}`, "tier", []traitMatrixProbe{
			{"k", false, "NIM-522's new no: `?|` matched an object's KEYS, which handed the row to anyone who could name one"},
			{"gold", false, "the value under the key is one level too deep to be a value of `tier`"},
			{`{"k": "gold"}`, false, "and the object's own text renders the object"},
		}},
		{`{"o":{}}`, "o", []traitMatrixProbe{
			{"", false, "an empty object has no keys to have been reachable by, before or now"},
			{"{}", false, "its own text is a container's text"},
		}},
		{`{}`, "any", []traitMatrixProbe{
			{"x", false, "an empty traits object reaches nothing"},
		}},

		// The shape that separates "walk the elements" from "walk everything
		// underneath": containers nested inside an array, next to real scalars.
		{`{"m":["a",["x"],{"k":"v"},42]}`, "m", []traitMatrixProbe{
			{"a", true, "a scalar sibling is still a value — giving up on the list at the first container would lose it"},
			{"42", true, "and so is one that comes AFTER both containers"},
			{"x", false, "one level deeper than a value"},
			{"k", false, "a key of a nested object is not a value either"},
			{"v", false, "nor what that key maps to"},
			{`["x"]`, false, "the nested array's own text is a container's text"},
			{`{"k": "v"}`, false, "and so is the nested object's"},
		}},
		{`{"m":[null]}`, "m", []traitMatrixProbe{
			{"null", false, "`#>>` over a null element is SQL NULL, equal to nothing"},
			{"", false, "and not the empty string either"},
		}},
		{`{"m":["a",null]}`, "m", []traitMatrixProbe{
			{"a", true, "a null element does not poison its siblings"},
			{"null", false, "and is still reachable by nothing"},
		}},
	}
}

func TestIntegration_TraitScopeValueMatrix(t *testing.T) {
	ctx := context.Background()
	pred := rbac.TraitScopeSQL("s.traits", "$1", "$2")
	query := fmt.Sprintf("SELECT %s FROM (SELECT $3::jsonb AS traits) s", pred)
	t.Logf("predicate under test:\n%s", pred)

	// A NULL traits column, first and separately: the whole predicate has to be
	// not-TRUE. `jsonb_typeof(NULL)` is NULL, so both arms go three-valued, and
	// the scope AST has no NOT to turn that back into a grant — but the CASE has
	// to hold too, because `jsonb_array_elements(NULL)` would otherwise decide it.
	var nullHit *bool
	if err := integrationPool.QueryRow(ctx,
		fmt.Sprintf("SELECT %s FROM (SELECT NULL::jsonb AS traits) s", pred),
		"k", []string{"v"}).Scan(&nullHit); err != nil {
		t.Fatalf("NULL traits column: %v — the predicate did not even evaluate, which on a real "+
			"table would be a 500 on every list under a trait scope", err)
	}
	if nullHit != nil && *nullHit {
		t.Errorf("a NULL traits column matched trait.k=v — fail-OPEN: a host nobody labelled is inside a scope")
	}
	if got := rbac.TraitValues(nil); got != nil {
		t.Errorf("rbac.TraitValues(nil) = %v, want nil — the Go half must be as blind as the SQL half", got)
	}

	for _, f := range traitMatrixFixtures() {
		t.Run(f.traits+"/"+f.key, func(t *testing.T) {
			// The Go half is contractually fed POSTGRES' serialization, not the
			// caller's spelling — that is the whole NIM-521 fix — so canonicalize
			// through the same jsonb the SQL half will read.
			var canon string
			if err := integrationPool.QueryRow(ctx, `SELECT ($1::jsonb)::text`, f.traits).Scan(&canon); err != nil {
				t.Fatalf("canonicalize %s: %v", f.traits, err)
			}
			goTexts := rbac.TraitValues([]byte(canon))

			for _, p := range f.probes {
				var sqlHit *bool
				if err := integrationPool.QueryRow(ctx, query, f.key, []string{p.value}, f.traits).Scan(&sqlHit); err != nil {
					t.Fatalf("%s / trait.%s=%q: %v", f.traits, f.key, p.value, err)
				}
				sqlAns := sqlHit != nil && *sqlHit

				goAns := false
				for _, txt := range goTexts[f.key] {
					if txt == p.value {
						goAns = true
						break
					}
				}

				switch {
				case sqlAns != goAns:
					t.Errorf("DIVERGENCE — %s, scope trait.%s=%q: the SQL pushdown says %v and the in-Go "+
						"match says %v. The same operator therefore sees this row in a list and not in a "+
						"single read, or the reverse. (Postgres stores it as %s; rbac.TraitValues offers %q.) %s",
						f.traits, f.key, p.value, sqlAns, goAns, canon, goTexts[f.key], p.why)
				case sqlAns != p.want:
					t.Errorf("BOTH HALVES WRONG — %s, scope trait.%s=%q: both answer %v, the rule says %v. "+
						"Agreeing with each other is not the same as being right; this is the case a "+
						"reader-vs-reader assertion alone cannot see. (Postgres stores it as %s; "+
						"rbac.TraitValues offers %q.) %s",
						f.traits, f.key, p.value, sqlAns, p.want, canon, goTexts[f.key], p.why)
				}
			}
		})
	}

	// `= ANY` binds a SET of values: a scope naming two must reach a row carrying
	// either. Asserted once rather than per-shape — the OR is in the binding, not
	// in the per-value rule the matrix above ranges over.
	var multi *bool
	if err := integrationPool.QueryRow(ctx, query,
		"ports", []string{"9999", "6380"}, `{"ports":[6379,6380]}`).Scan(&multi); err != nil {
		t.Fatalf("two-value scope: %v", err)
	}
	if multi == nil || !*multi {
		t.Errorf("a scope bound to {9999, 6380} missed a row carrying 6380 — `= ANY` stopped being an OR")
	}
}
