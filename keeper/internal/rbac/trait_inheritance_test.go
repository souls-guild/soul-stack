package rbac

import (
	"strings"
	"testing"
)

// Guard tests for label inheritance in the scope predicate (ADR-080). A host's
// visibility must resolve over its own labels AND those of the incarnations it
// belongs to — the incarnation is the recommended place to attach a label, so a
// predicate that only reads the host's own columns would make it grant nothing.

// soulCols mirrors soulpurview.Columns without importing it (soulpurview imports
// this package). A drift between the two is caught by soulpurview's own tests.
var soulCols = ScopeColumns{
	Coven:         "souls.coven",
	Host:          "souls.sid",
	Traits:        "souls.traits",
	MembershipSID: "souls.sid",
}

func TestPurviewSQL_CovenReachesInheritedLabels(t *testing.T) {
	p := Purview{Exprs: []*ScopeExpr{mustExpr(t, "coven=dba")}}

	sql, args, _ := PurviewSQL(p, soulCols, 1)

	if !strings.Contains(sql, "souls.coven && $1::text[]") {
		t.Errorf("own coven predicate missing from:\n%s", sql)
	}
	if !strings.Contains(sql, "incarnation_membership m") {
		t.Fatalf("coven scope does not reach the host's incarnations — an incarnation label would grant nothing:\n%s", sql)
	}
	// The incarnation's NAME counts as one of its coven tags, matching the
	// incarnation-side resolver (`covens && $x OR name = ANY($x)`).
	if !strings.Contains(sql, "i.name = ANY($1::text[])") {
		t.Errorf("incarnation name not treated as a coven tag:\n%s", sql)
	}
	if len(args) != 1 {
		t.Errorf("args = %v, want the value bound once and reused by placeholder", args)
	}
}

func TestPurviewSQL_TraitReachesInheritedLabels(t *testing.T) {
	p := Purview{Exprs: []*ScopeExpr{mustExpr(t, "trait.owner=dba")}}

	sql, _, _ := PurviewSQL(p, soulCols, 1)

	if !strings.Contains(sql, "souls.traits ->>") {
		t.Errorf("own trait predicate missing from:\n%s", sql)
	}
	if !strings.Contains(sql, "i.traits ->>") {
		t.Fatalf("trait scope does not reach the host's incarnations — `owner=dba` on an incarnation would not expose its hosts:\n%s", sql)
	}
	if !strings.Contains(sql, "OR EXISTS") {
		t.Errorf("own and inherited must be OR-ed (either grants), got:\n%s", sql)
	}
}

// The membership correlation must be table-qualified: the subquery aliases
// `incarnation_membership m`, so a bare `sid` would bind there and correlate
// every host to itself — every host would match every trait.
func TestPurviewSQL_MembershipCorrelationIsQualified(t *testing.T) {
	p := Purview{Exprs: []*ScopeExpr{mustExpr(t, "trait.owner=dba")}}

	sql, _, _ := PurviewSQL(p, soulCols, 1)

	if !strings.Contains(sql, "WHERE m.sid = souls.sid") {
		t.Fatalf("membership correlation is not qualified to the outer table:\n%s", sql)
	}
}

// The incarnation table carries its labels directly (no membership column), so
// its predicate must stay a plain column comparison.
func TestPurviewSQL_NoMembershipColumn_NoInheritance(t *testing.T) {
	incCols := ScopeColumns{Coven: "covens", Incarnation: "name", Traits: "traits"}
	p := Purview{Exprs: []*ScopeExpr{mustExpr(t, "trait.owner=dba")}}

	sql, _, _ := PurviewSQL(p, incCols, 1)

	if strings.Contains(sql, "incarnation_membership") {
		t.Fatalf("incarnation scope joined membership — it holds its own labels:\n%s", sql)
	}
}

// Gate (b) of the per-soul trait write: only pairs the operator itself is scoped
// on may be attached.
func TestTraitScope_PureTraitDisjunctsOnly(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{name: "dba-traits", operators: []string{"archon-dba"}, permissions: []string{"soul.traits-assign on trait.owner=dba"}})

	pairs, unrestricted := e.TraitScope("archon-dba", "soul", "traits-assign")

	if unrestricted {
		t.Fatal("scoped operator reported unrestricted — gate (b) would admit any pair")
	}
	if got := pairs["owner"]; len(got) != 1 || got[0] != "dba" {
		t.Fatalf("trait scope = %v, want owner=[dba]", pairs)
	}
}

// A disjunct mixing trait with another dimension narrows below "any host with
// that pair", so projecting it to the bare pair would over-permit the write.
func TestTraitScope_MixedDisjunctDropped(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{name: "mixed", operators: []string{"archon-mix"}, permissions: []string{"soul.traits-assign on trait.owner=dba AND coven=prod"}})

	pairs, unrestricted := e.TraitScope("archon-mix", "soul", "traits-assign")

	if unrestricted {
		t.Fatal("mixed-dimension scope reported unrestricted")
	}
	if len(pairs) != 0 {
		t.Fatalf("trait scope = %v, want empty (a mixed disjunct must not project to a bare pair)", pairs)
	}
}

func TestTraitScope_Wildcard_Unrestricted(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{name: "admin", operators: []string{"archon-root"}, permissions: []string{"*"}})

	if _, unrestricted := e.TraitScope("archon-root", "soul", "traits-assign"); !unrestricted {
		t.Fatal("cluster-admin is not unrestricted for trait scope")
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
