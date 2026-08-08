package rbac

import (
	"strings"
	"testing"
)

// Guard tests for the scope predicate's label resolution (NIM-281). A host's
// visibility resolves over the labels it carries ITSELF and nothing else: a coven
// tag or a trait pair grants only where an operator attached it, and belonging to
// an incarnation attaches nothing. Reaching an incarnation's members is a
// membership question, spelled `incarnation=<name>`.
//
// These guards are written against the SQL text on purpose. Inheritance was
// invisible from the outside — it widened the matched set without erroring — so
// the only thing that catches it coming back is the predicate itself.

// soulCols mirrors soulpurview.Columns without importing it (soulpurview imports
// this package). A drift between the two is caught by soulpurview's own tests.
var soulCols = ScopeColumns{
	Coven:  "souls.coven",
	Host:   "souls.sid",
	Traits: "souls.traits",
}

func TestPurviewSQL_CovenIsOwnColumnOnly(t *testing.T) {
	p := Purview{Exprs: []*ScopeExpr{mustExpr(t, "coven=dba")}}

	sql, args, _ := PurviewSQL(p, soulCols, 1)

	if !strings.Contains(sql, "souls.coven && $1::text[]") {
		t.Errorf("own coven predicate missing from:\n%s", sql)
	}
	if strings.Contains(sql, "incarnation_membership") {
		t.Fatalf("coven scope reaches the host's incarnations — a label an operator never attached to the host would grant access:\n%s", sql)
	}
	if strings.Contains(sql, "i.name") {
		t.Fatalf("an incarnation's name is being matched as a coven tag:\n%s", sql)
	}
	if len(args) != 1 {
		t.Errorf("args = %v, want the value bound once", args)
	}
}

func TestPurviewSQL_TraitIsOwnColumnOnly(t *testing.T) {
	p := Purview{Exprs: []*ScopeExpr{mustExpr(t, "trait.owner=dba")}}

	sql, _, _ := PurviewSQL(p, soulCols, 1)

	if !strings.Contains(sql, "souls.traits ->>") {
		t.Errorf("own trait predicate missing from:\n%s", sql)
	}
	if strings.Contains(sql, "i.traits") || strings.Contains(sql, "incarnation_membership") {
		t.Fatalf("trait scope reaches the host's incarnations — `owner=dba` on an incarnation would expose hosts nobody labelled:\n%s", sql)
	}
	// The predicate DOES carry a subquery since NIM-522 (walking a list value's
	// elements needs `jsonb_array_elements`), so "no subquery" is no longer the
	// invariant — "no TABLE" is. Every FROM has to unnest the row's own column;
	// a FROM naming anything else is a source of rows the operator never labelled.
	//
	// The count is asserted first and exactly. A loop over what fromSources finds
	// says nothing when it finds nothing: rename the CTE, reshape the predicate,
	// or break the parser, and every check below passes over an empty list.
	srcs := fromSources(sql)
	if len(srcs) != 1 {
		t.Fatalf("found %d FROM sources in the trait predicate, want exactly 1 (the jsonb_array_elements "+
			"over the row's own column). Zero means the checks below range over nothing and this guard "+
			"gates nothing; more than one is a second source of rows.\nsources: %q\n%s", len(srcs), srcs, sql)
	}
	for _, src := range srcs {
		if !strings.HasPrefix(src, "jsonb_array_elements(") {
			t.Fatalf("trait predicate reads from %q — it must only unnest the row's own traits column:\n%s", src, sql)
		}
		if !strings.Contains(src, soulCols.Traits) {
			t.Fatalf("trait predicate unnests %q rather than %s — that is not the row's own column:\n%s", src, soulCols.Traits, sql)
		}
	}
}

// fromSources returns what each FROM in sql reads, so a guard can state "no
// table" instead of the weaker "no subquery". Nested parens are balanced so a
// `jsonb_array_elements(CASE … END)` comes back whole rather than clipped at
// its first `)`.
func fromSources(sql string) []string {
	var out []string
	for rest := sql; ; {
		i := strings.Index(rest, "FROM ")
		if i < 0 {
			return out
		}
		rest = rest[i+len("FROM "):]
		depth, end := 0, len(rest)
		for j := 0; j < len(rest); j++ {
			switch rest[j] {
			case '(':
				depth++
			case ')':
				if depth--; depth < 0 {
					end = j
				}
			case ' ':
				if depth == 0 && j > 0 {
					end = j
				}
			}
			if end != len(rest) {
				break
			}
		}
		out = append(out, rest[:end])
		rest = rest[end:]
	}
}

// The incarnation table carries its labels directly, and always did — the guard
// stays to pin that its predicate never grows a membership join either.
func TestPurviewSQL_IncarnationScopeIsPlainColumns(t *testing.T) {
	incCols := ScopeColumns{Coven: "covens", Incarnation: "name", Traits: "traits"}
	p := Purview{Exprs: []*ScopeExpr{mustExpr(t, "trait.owner=dba")}}

	sql, _, _ := PurviewSQL(p, incCols, 1)

	if strings.Contains(sql, "incarnation_membership") {
		t.Fatalf("incarnation scope joined membership — it holds its own labels:\n%s", sql)
	}
}

// CovenScopeSQL is rendered by four call sites (the RBAC pushdown, the souls list
// filter, the bulk selector and the bulk scope gate, NIM-250). Pinning its shape
// here pins all four: what an operator can find, what a bulk call selects and what
// authorizes that call cannot drift into three different answers about one host.
func TestCovenScopeSQL_IsPlainOverlap(t *testing.T) {
	got := CovenScopeSQL("souls.coven", "$7")

	if got != "souls.coven && $7::text[]" {
		t.Fatalf("CovenScopeSQL = %q, want a plain array overlap on the row's own column", got)
	}
}

func mustExpr(t *testing.T, s string) *ScopeExpr {
	t.Helper()
	e, err := ParseScopeExpr(s)
	if err != nil {
		t.Fatalf("ParseScopeExpr(%q): %v", s, err)
	}
	return e
}
