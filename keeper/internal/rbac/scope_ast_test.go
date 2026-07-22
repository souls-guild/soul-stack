package rbac

import (
	"strings"
	"testing"
)

func TestParseScopeExpr_CanonicalRoundTrip(t *testing.T) {
	cases := []struct{ in, want string }{
		{"coven=a", "coven=a"},
		{"coven=a,b", "coven in (a, b)"}, // old flat form → canonical in-list
		{"coven in (a, b)", "coven in (a, b)"},
		{"host matches redis-*", "host matches redis-*"},
		{`host matches "redis-*"`, "host matches redis-*"},
		{"incarnation matches redis-*", "incarnation matches redis-*"}, // NIM-128 amend: glob on incarnation
		{"incarnation in (a, b)", "incarnation in (a, b)"},
		{"trait.owner=dba", "trait.owner=dba"},
		{"coven=a AND host matches web-*", "coven=a AND host matches web-*"},
		{"coven=a OR coven=b", "coven=a OR coven=b"},
		{"coven=a AND (trait.owner=dba OR trait.owner=platform)",
			"coven=a AND (trait.owner=dba OR trait.owner=platform)"},
		{"coven=a AND coven=b OR coven=c", "(coven=a AND coven=b) OR coven=c"}, // AND>OR, canonical parens
	}
	for _, c := range cases {
		e, err := ParseScopeExpr(c.in)
		if err != nil {
			t.Errorf("ParseScopeExpr(%q): %v", c.in, err)
			continue
		}
		if got := e.String(); got != c.want {
			t.Errorf("ParseScopeExpr(%q).String() = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseScopeExpr_Precedence(t *testing.T) {
	// coven=a OR coven=b AND coven=c  ==  coven=a OR (coven=b AND coven=c)
	e, err := ParseScopeExpr("coven=a OR coven=b AND coven=c")
	if err != nil {
		t.Fatal(err)
	}
	if e.Op != OpOr || len(e.Children) != 2 {
		t.Fatalf("top not OR/2: op=%d n=%d", e.Op, len(e.Children))
	}
	if e.Children[1].Op != OpAnd {
		t.Errorf("second child not AND: %d", e.Children[1].Op)
	}
}

func TestParseScopeExpr_DNF(t *testing.T) {
	// (coven=a OR coven=b) AND host matches web-*
	//   → [ {coven=a, host web-*}, {coven=b, host web-*} ]
	e, err := ParseScopeExpr("(coven=a OR coven=b) AND host matches web-*")
	if err != nil {
		t.Fatal(err)
	}
	dnf, err := toDNF(e)
	if err != nil {
		t.Fatal(err)
	}
	if len(dnf) != 2 {
		t.Fatalf("DNF conjuncts = %d, want 2", len(dnf))
	}
	for _, conj := range dnf {
		if len(conj) != 2 {
			t.Errorf("conjunct len = %d, want 2 (%v)", len(conj), conj)
		}
	}
}

func TestParseScopeExpr_Errors(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "empty expression"},
		{"namespace=x", "unknown dimension"},
		{"coven", "expected '='"},
		{"coven=", "expected a value"},
		{"regex=x", "unknown dimension"},
		{"soulprint=x", "unknown dimension"},
		{"state=x", "unknown dimension"},
		{"coven matches x", "only valid for host or incarnation"},
		{"service matches x", "only valid for host or incarnation"},
		{"coven=a AND", "expected a condition"},
		{"(coven=a", "expected ')'"},
		{`host matches "a`, "unterminated"},
		{"trait=owner", "unknown dimension"}, // bare `trait=` form gone (use trait.<key>=)
	}
	for _, c := range cases {
		_, err := ParseScopeExpr(c.in)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("ParseScopeExpr(%q) err = %v, want substring %q", c.in, err, c.want)
		}
	}
}

func TestParseScopeExpr_Caps(t *testing.T) {
	var many []string
	for i := 0; i < maxScopeAtoms+1; i++ {
		many = append(many, "coven=x")
	}
	if _, err := ParseScopeExpr(strings.Join(many, " AND ")); err == nil ||
		!strings.Contains(err.Error(), "too many conditions") {
		t.Errorf("atom cap not enforced: %v", err)
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		glob, target string
		want         bool
	}{
		{"redis-*", "redis-01", true},
		{"redis-*", "web-01", false},
		{"web-?", "web-1", true},
		{"web-?", "web-12", false},
		{"*-prod", "redis-prod", true},
		{"exact", "exact", true},
		{"exact", "exacto", false},
	}
	for _, c := range cases {
		if got := globMatch(c.glob, c.target); got != c.want {
			t.Errorf("globMatch(%q,%q) = %v, want %v", c.glob, c.target, got, c.want)
		}
	}
}

func TestGlobToSQLLike(t *testing.T) {
	cases := []struct{ glob, want string }{
		{"redis-*", "redis-%"},
		{"web-?", "web-_"},
		{"a_b", `a\_b`},
		{"100%", `100\%`},
	}
	for _, c := range cases {
		if got := globToSQLLike(c.glob); got != c.want {
			t.Errorf("globToSQLLike(%q) = %q, want %q", c.glob, got, c.want)
		}
	}
}
