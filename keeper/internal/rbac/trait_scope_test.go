package rbac

import "testing"

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
