package soulpurview

import (
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

// TestResolve_Empty_FailClosed is the MAIN security invariant: Purview{} (no
// dimensions, not Unrestricted) -> fail-closed: scope with Empty=true. Handler
// must return EMPTY list, NOT whole fleet. Regression of this test = operator
// sees someone else's hosts.
func TestResolve_Empty_FailClosed(t *testing.T) {
	sc := Resolve(rbac.Purview{})
	if !sc.Empty {
		t.Fatalf("Purview{} → Empty=false (fail-OPEN!); want Empty=true (fail-closed)")
	}
	if sc.Unrestricted {
		t.Fatalf("Purview{} → Unrestricted=true; want false")
	}
	if len(sc.Covens) != 0 {
		t.Fatalf("Purview{} -> Covens=%v; want empty", sc.Covens)
	}
}

// TestResolve_Unrestricted verifies Unrestricted (bare-without-default /
// coven=*) -> whole list without scope filter.
func TestResolve_Unrestricted(t *testing.T) {
	sc := Resolve(rbac.Purview{Unrestricted: true})
	if !sc.Unrestricted {
		t.Fatalf("Unrestricted purview → Unrestricted=false")
	}
	if sc.Empty {
		t.Fatalf("Unrestricted purview -> Empty=true (would return empty instead of all)")
	}
}

// TestResolve_SingleCoven verifies Covens=[prod] -> prod subset, not empty/not all.
func TestResolve_SingleCoven(t *testing.T) {
	sc := Resolve(rbac.Purview{Covens: []string{"prod"}})
	if sc.Empty {
		t.Fatalf("Covens=[prod] -> Empty=true (fail-closed on non-empty scope!)")
	}
	if sc.Unrestricted {
		t.Fatalf("Covens=[prod] -> Unrestricted=true (expanded scope to whole fleet!)")
	}
	if !reflect.DeepEqual(sc.Covens, []string{"prod"}) {
		t.Fatalf("Covens=%v; want [prod]", sc.Covens)
	}
}

// TestResolve_MultiCoven_Union verifies Covens=[prod,staging] -> union (OR
// inside dimension), both covens in scope.
func TestResolve_MultiCoven_Union(t *testing.T) {
	sc := Resolve(rbac.Purview{Covens: []string{"prod", "staging"}})
	if sc.Empty || sc.Unrestricted {
		t.Fatalf("multi-coven -> Empty=%v Unrestricted=%v; want both false", sc.Empty, sc.Unrestricted)
	}
	if !reflect.DeepEqual(sc.Covens, []string{"prod", "staging"}) {
		t.Fatalf("Covens=%v; want [prod staging]", sc.Covens)
	}
}

// TestResolve_UnsupportedDimensions_Partial verifies soulprint/state dimensions
// (without coven/regex and without Unrestricted) are NOT computable by S3b-2a
// yet (page CEL is S3b-2b). Resolve must mark such scope Partial=true so handler
// does not present partial set as complete (otherwise hosts available by
// soulprint would silently disappear). Empty is false here (access exists).
// regex is excluded from Partial because it is computed by keyset filter (see
// regex_test.go).
func TestResolve_UnsupportedDimensions_Partial(t *testing.T) {
	for name, p := range map[string]rbac.Purview{
		"soulprint":       {SoulprintExprs: []string{`soulprint.self.os.family == "debian"`}},
		"state":           {StateExprs: []string{`state.role == "primary"`}},
		"coven+soulprint": {Covens: []string{"prod"}, SoulprintExprs: []string{`soulprint.self.os.family == "debian"`}},
	} {
		sc := Resolve(p)
		if sc.Empty {
			t.Errorf("%s purview -> Empty=true (fail-closed on introduced available dimension)", name)
		}
		if sc.Unrestricted {
			t.Errorf("%s purview -> Unrestricted=true (removed scope entirely)", name)
		}
		if !sc.Partial {
			t.Errorf("%s purview -> Partial=false; want true (S3b-2a does not compute soulprint/state)", name)
		}
	}
}

// TestResolve_CovenOnly_NotPartial verifies pure coven scope is fully covered
// by pilot SQL pushdown -> Partial=false (result complete, does not require S3b-2).
func TestResolve_CovenOnly_NotPartial(t *testing.T) {
	if Resolve(rbac.Purview{Covens: []string{"prod"}}).Partial {
		t.Fatalf("coven-only purview -> Partial=true; want false (coven is fully pushed down)")
	}
	if Resolve(rbac.Purview{Unrestricted: true}).Partial {
		t.Fatalf("unrestricted purview → Partial=true; want false")
	}
}

// TestInScope_Unrestricted verifies Unrestricted scope sees any host, including
// host without covens at all.
func TestInScope_Unrestricted(t *testing.T) {
	sc := Scope{Unrestricted: true}
	if !InScope(sc, "host-01.example.com", []string{"prod"}) {
		t.Fatalf("Unrestricted scope -> host [prod] outside scope; want in scope")
	}
	if !InScope(sc, "host-01.example.com", nil) {
		t.Fatalf("Unrestricted scope -> host without covens outside scope; want in scope")
	}
}

// TestInScope_Empty_FailClosed is the MAIN single-read security invariant:
// Empty scope (fail-closed) allows no host, including with covens. Regression =
// scoped operator without rights reads someone else's host by direct SID.
func TestInScope_Empty_FailClosed(t *testing.T) {
	sc := Scope{Empty: true}
	if InScope(sc, "host-01.example.com", []string{"prod"}) {
		t.Fatalf("Empty scope -> host [prod] in scope (fail-OPEN!); want outside scope")
	}
	if InScope(sc, "host-01.example.com", nil) {
		t.Fatalf("Empty scope -> host without covens in scope (fail-OPEN!); want outside scope")
	}
}

// TestInScope_CovenMatch verifies intersection of host covens and scope.Covens
// is non-empty -> host visible.
func TestInScope_CovenMatch(t *testing.T) {
	sc := Scope{Covens: []string{"prod", "staging"}}
	if !InScope(sc, "host-01.example.com", []string{"staging"}) {
		t.Fatalf("scope [prod staging] intersection host [staging] non-empty -> outside scope; want in scope")
	}
	if !InScope(sc, "host-01.example.com", []string{"dev", "prod"}) {
		t.Fatalf("scope [prod staging] intersection host [dev prod] non-empty -> outside scope; want in scope")
	}
}

// TestInScope_CovenMismatch verifies empty intersection -> host outside scope
// (single-read returns 404, does not reveal someone else's host existence).
func TestInScope_CovenMismatch(t *testing.T) {
	sc := Scope{Covens: []string{"prod"}}
	if InScope(sc, "host-01.example.com", []string{"staging"}) {
		t.Fatalf("scope [prod] intersection host [staging] empty -> in scope; want outside scope")
	}
	if InScope(sc, "host-01.example.com", nil) {
		t.Fatalf("scope [prod] intersection host without covens -> in scope; want outside scope")
	}
}
