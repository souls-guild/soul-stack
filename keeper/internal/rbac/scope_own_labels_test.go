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
	if strings.Contains(sql, "EXISTS") {
		t.Fatalf("trait predicate carries a correlated subquery; it must be a plain column comparison:\n%s", sql)
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
