package handlers

// NIM-128: incarnation trait-scope is evaluated by the unified resolver
// (rbac.TraitValues → rbac.EvalScope), replacing the former scalar-only
// traitScalarEquals. A jsonb trait value projects into rbac.ScopeInput.Traits:
// scalars become a one-element slice; LIST values contribute each element
// (so a `trait.env=prod` scope now matches a `{env:[prod,stage]}` label —
// List and Get agree, resolving the former List↔Get divergence).
//
// NIM-521: the projection input is the RAW jsonb, not the decoded map. The
// decoded map has already lost the number token the SQL half of the SAME
// boundary compares against, so a map-based projection made Get disagree with
// List on any label the two render differently. The cases below are written as
// the bytes Postgres returns, which is what the handler now feeds.

import (
	"slices"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

// traitVisible mirrors the incarnation Get/List scope check: build the scope
// input from an incarnation's RAW traits jsonb and evaluate a single
// `trait.<key>=value` predicate against it.
func traitVisible(traitsJSON string, key, value string) bool {
	return traitVisibleExpr(traitsJSON, "trait."+key+"="+value)
}

// traitVisibleExpr is traitVisible for a scope whose VALUE needs its own
// spelling — a quoted one, say. It takes the predicate verbatim so a test can
// name a text no bareword could carry.
func traitVisibleExpr(traitsJSON, predicate string) bool {
	expr, err := rbac.ParseScopeExpr(predicate)
	if err != nil {
		panic(err)
	}
	return rbac.EvalScope(expr, rbac.ScopeInput{Traits: rbac.TraitValues([]byte(traitsJSON))})
}

func TestTraitScope_Table(t *testing.T) {
	tests := []struct {
		name   string
		traits string
		key    string
		value  string
		want   bool
	}{
		{"string hit", `{"owner": "alice"}`, "owner", "alice", true},
		{"string miss", `{"owner": "alice"}`, "owner", "bob", false},
		{"integer hit", `{"shard": 3}`, "shard", "3", true},
		{"bool hit", `{"managed": true}`, "managed", "true", true},
		{"missing key → miss", `{"owner": "alice"}`, "team", "dba", false},
		{"nil traits → miss", ``, "owner", "alice", false},
		// NIM-128: a list value matches on any element (List↔Get consistent).
		{"list-Trait hit (element match)", `{"env": ["prod", "stage"]}`, "env", "prod", true},
		{"list-Trait miss (element absent)", `{"env": ["prod", "stage"]}`, "env", "dev", false},
		// NIM-521: the token Postgres stores, not a re-formatted float. A decoded
		// map rendered these as "1e+06" / "1e-07" / a rounded float, so Get hid an
		// incarnation the List had just shown.
		{"large integer keeps its token", `{"asn": 1000000}`, "asn", "1000000", true},
		{"integer is not a scaled decimal", `{"asn": 1000000}`, "asn", "1000000.0", false},
		{"scale is preserved, not normalized", `{"asn": 1000000.0}`, "asn", "1000000.0", true},
		{"small fraction keeps its token", `{"ratio": 0.0000001}`, "ratio", "0.0000001", true},
		{"beyond float64 precision survives", `{"wide": 12345678901234567890}`, "wide", "12345678901234567890", true},
		// NIM-522: a scope value names a WHOLE value. A list element is one,
		// whatever its JSON type — the old `?|` arm reached string elements only,
		// so `trait.ports=6379` was denied on a host that plainly carries 6379.
		{"numeric list element IS addressable", `{"ports": [6379, 6380]}`, "ports", "6379", true},
		{"bool list element IS addressable", `{"flags": [true, false]}`, "flags", "true", true},
		{"numeric list element miss", `{"ports": [6379, 6380]}`, "ports", "6381", false},
		// NIM-522: an object's KEY is not a value — the old `?|` arm matched keys,
		// so `trait.tier=k` granted access to every host whose `tier` object had a
		// `k` key, whatever it mapped to.
		{"object key is not a value", `{"tier": {"k": "gold"}}`, "tier", "k", false},
		{"object value is not reachable either", `{"tier": {"k": "gold"}}`, "tier", "gold", false},
		// NIM-522: a nested container inside a list is not a value either.
		{"nested list element is not a value", `{"m": ["a", ["x"]]}`, "m", "x", false},
		{"scalar sibling of a nested element still matches", `{"m": ["a", ["x"]]}`, "m", "a", true},
		// `->>` over a JSON null is SQL NULL — equal to nothing (fail-closed).
		{"null value matches nothing", `{"gone": null}`, "gone", "null", false},
		{"null list element matches nothing", `{"m": [null]}`, "m", "null", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := traitVisible(tc.traits, tc.key, tc.value); got != tc.want {
				t.Errorf("traitVisible(%s, %q, %q) = %v, want %v", tc.traits, tc.key, tc.value, got, tc.want)
			}
		})
	}
}

// TestTraitScope_ContainerTextIsNotAValue pins the other half of NIM-522: the
// text Postgres prints FOR a container is a rendering of the container, not a
// value inside it, so naming that text grants nothing.
//
// Only the numeric list is expressible as a scope value and therefore testable
// through the parser — `["prod", "stage"]` carries `"`, which no scope value can
// (a quoted value ends at the first `"`, a bareword cannot hold one at all).
// That asymmetry is exactly why the fork was decided rather than documented: an
// operator could address a numeric list whole and a string list not at all,
// while neither list's ELEMENTS were uniformly addressable.
func TestTraitScope_ContainerTextIsNotAValue(t *testing.T) {
	if traitVisibleExpr(`{"ports": [6379, 6380]}`, `trait.ports="[6379, 6380]"`) {
		t.Errorf(`trait.ports="[6379, 6380]" still matches the array's own text; want no access (NIM-522)`)
	}
	// The string list's container text is unreachable through the parser, so it
	// is asserted where it lives: the projection must not offer it as a value.
	//
	// The exact set is pinned first, and that is not belt-and-braces: a loop that
	// only rejects a `[`-bearing text passes just as happily over an EMPTY
	// projection, so a TraitValues that gave up on lists entirely would satisfy
	// the check below while destroying every list scope. Assert what must be
	// there before asserting what must not.
	got := rbac.TraitValues([]byte(`{"env": ["prod", "stage"]}`))["env"]
	if want := []string{"prod", "stage"}; !slices.Equal(got, want) {
		t.Fatalf("TraitValues offers %q for env, want exactly %q — the elements themselves (NIM-522)", got, want)
	}
	for _, text := range got {
		if strings.ContainsAny(text, "[]") {
			t.Errorf("TraitValues offers %q as a value for env; want only the elements (NIM-522)", text)
		}
	}
}
