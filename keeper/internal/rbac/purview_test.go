package rbac

import (
	"reflect"
	"testing"
	"time"
)

// ResolvePurview (ADR-047 S0) is a generalization of CovenScope into
// Purview. These tests pin down that observable behavior does NOT change:
// every CovenScope scenario has an equivalent Purview result, and CovenScope
// becomes a thin (covens, unrestricted) projection of ResolvePurview.

func TestResolvePurview_Wildcard_Unrestricted(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "admin", operators: []string{"archon-a"}, permissions: []string{"*"},
	})
	p := e.ResolvePurview("archon-a", "soul", "coven-assign")
	if !p.Unrestricted {
		t.Errorf("Unrestricted = false, want true for `*`")
	}
	if p.Deny {
		t.Errorf("Deny = true, want false for `*`")
	}
	if covensFromPurview(p) != nil {
		t.Errorf("Covens = %v, want nil for unrestricted", covensFromPurview(p))
	}
}

func TestResolvePurview_BarePermission_Unrestricted(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "ops", operators: []string{"archon-a"}, permissions: []string{"soul.coven-assign"},
	})
	p := e.ResolvePurview("archon-a", "soul", "coven-assign")
	if !p.Unrestricted {
		t.Errorf("Unrestricted = false, want true for bare permission")
	}
	if p.Deny {
		t.Errorf("Deny = true, want false for bare permission")
	}
}

func TestResolvePurview_CovenSelector_Restricted(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "dev-ops", operators: []string{"archon-a"},
		permissions: []string{"soul.coven-assign on coven=dev,stage"},
	})
	p := e.ResolvePurview("archon-a", "soul", "coven-assign")
	if p.Unrestricted {
		t.Errorf("Unrestricted = true, want false for coven-selector")
	}
	if !reflect.DeepEqual(covensFromPurview(p), []string{"dev", "stage"}) {
		t.Errorf("Covens = %v, want [dev stage] (sorted)", covensFromPurview(p))
	}
}

// TestResolvePurview_Revoked_Deny (ADR-047 G1) — a revoked Archon with an
// active role of ANY dimension gets Purview{Deny:true} with empty fields.
// This is the single point of revoked-aware resolution: gate
// (HoldsAction→false), single-read (soulpurview.Resolve→Empty→404), InScope
// (Deny→false) — all derive from here. Mirrors the revoked-shortcut in
// Check (enforcer.go).
func TestResolvePurview_Revoked_Deny(t *testing.T) {
	cases := []struct {
		name string
		perm string
	}{
		{"bare", "soul.list"},
		{"wildcard", "*"},
		{"coven", "soul.list on coven=prod"},
		{"host", "soul.list on host matches web-*"},
		{"trait", "soul.list on trait.owner=dba"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := snapshotOf(fixtureRole{
				name: "active", operators: []string{"archon-fired"}, permissions: []string{tc.perm},
			})
			snap.Revoked = map[string]time.Time{"archon-fired": time.Now()}
			e, err := NewEnforcerFromSnapshot(snap)
			if err != nil {
				t.Fatalf("NewEnforcerFromSnapshot: %v", err)
			}
			p := e.ResolvePurview("archon-fired", "soul", "list")
			if !p.Deny {
				t.Errorf("revoked AID with %q: Deny = false, want true", tc.perm)
			}
			if p.Unrestricted {
				t.Errorf("revoked AID with %q: Unrestricted = true, want false (revoked is not unrestricted)", tc.perm)
			}
			if len(p.Exprs) != 0 {
				t.Errorf("revoked AID with %q: scope predicates are not empty (%+v), want none", tc.perm, p)
			}
		})
	}
}

func TestResolvePurview_UnionAcrossRoles(t *testing.T) {
	e := mustEnforcer(t,
		fixtureRole{name: "r1", operators: []string{"archon-a"}, permissions: []string{"soul.coven-assign on coven=dev"}},
		fixtureRole{name: "r2", operators: []string{"archon-a"}, permissions: []string{"soul.coven-assign on coven=stage"}},
	)
	p := e.ResolvePurview("archon-a", "soul", "coven-assign")
	if p.Unrestricted {
		t.Errorf("Unrestricted = true, want false")
	}
	if !reflect.DeepEqual(covensFromPurview(p), []string{"dev", "stage"}) {
		t.Errorf("Covens = %v, want [dev stage] (union)", covensFromPurview(p))
	}
}

// A host-only selector doesn't make the operator unrestricted by coven and
// doesn't grant any coven label — current CovenScope behavior (covens=nil,
// unrestricted=false).
func TestResolvePurview_HostSelector_NotCovenScoped(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "host-ops", operators: []string{"archon-a"},
		permissions: []string{"soul.coven-assign on host=h.example.com"},
	})
	p := e.ResolvePurview("archon-a", "soul", "coven-assign")
	if p.Unrestricted {
		t.Errorf("Unrestricted = true, want false (host-only selector)")
	}
	if len(covensFromPurview(p)) != 0 {
		t.Errorf("Covens = %v, want empty (host-only selector)", covensFromPurview(p))
	}
}

// A known AID without a matching role — current CovenScope returns
// (nil, false). S0 does NOT change the semantics to Deny=true: pure refactor.
func TestResolvePurview_NoMatchingPermission_NotUnrestricted(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "viewer", operators: []string{"archon-a"}, permissions: []string{"soul.list"},
	})
	p := e.ResolvePurview("archon-a", "soul", "coven-assign")
	if p.Unrestricted {
		t.Errorf("Unrestricted = true, want false for non-holder")
	}
	if len(covensFromPurview(p)) != 0 {
		t.Errorf("Covens = %v, want empty for non-holder", covensFromPurview(p))
	}
}

// Unknown AID — CovenScope currently returns (nil, false); the projection
// through ResolvePurview must be equivalent.
func TestResolvePurview_UnknownAID_Empty(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "ops", operators: []string{"archon-a"}, permissions: []string{"soul.coven-assign"},
	})
	p := e.ResolvePurview("archon-ghost", "soul", "coven-assign")
	if p.Unrestricted || len(covensFromPurview(p)) != 0 {
		t.Errorf("ghost AID: Covens=%v Unrestricted=%v, want empty/false", covensFromPurview(p), p.Unrestricted)
	}
}

// A coven-only permission yields a scope predicate that references ONLY the
// coven dimension — resolving it must not accidentally introduce constraints on
// other dimensions (host/service/incarnation/trait). NIM-128 boolean scope.
func TestResolvePurview_CovenScope_OnlyCovenDimension(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "dev-ops", operators: []string{"archon-a"},
		permissions: []string{"soul.coven-assign on coven=dev"},
	})
	p := e.ResolvePurview("archon-a", "soul", "coven-assign")
	if len(p.Exprs) != 1 {
		t.Fatalf("Exprs = %+v, want exactly one predicate", p.Exprs)
	}
	dims := scopeDimsUsed(p.Exprs[0])
	if len(dims) != 1 {
		t.Errorf("scope references dimensions %v, want only {coven}", sortedSet(dims))
	}
	if _, ok := dims[dimCoven]; !ok {
		t.Errorf("scope does not reference coven: %v", sortedSet(dims))
	}
}

// Equivalence: CovenScope == the ResolvePurview projection onto
// (Covens, Unrestricted) across all characteristic scenarios. This is the
// central S0 regression test — the refactor doesn't change a single
// coven-scope decision.
func TestCovenScope_EquivalentToResolvePurviewProjection(t *testing.T) {
	e := mustEnforcer(t,
		fixtureRole{name: "admin", operators: []string{"archon-wild"}, permissions: []string{"*"}},
		fixtureRole{name: "bare", operators: []string{"archon-bare"}, permissions: []string{"soul.coven-assign"}},
		fixtureRole{name: "scoped", operators: []string{"archon-scoped"}, permissions: []string{"soul.coven-assign on coven=dev,stage"}},
		fixtureRole{name: "host", operators: []string{"archon-host"}, permissions: []string{"soul.coven-assign on host=h.example.com"}},
		fixtureRole{name: "viewer", operators: []string{"archon-view"}, permissions: []string{"soul.list"}},
		fixtureRole{name: "u1", operators: []string{"archon-union"}, permissions: []string{"soul.coven-assign on coven=dev"}},
		fixtureRole{name: "u2", operators: []string{"archon-union"}, permissions: []string{"soul.coven-assign on coven=stage"}},
	)
	aids := []string{"archon-wild", "archon-bare", "archon-scoped", "archon-host", "archon-view", "archon-union", "archon-ghost"}
	for _, aid := range aids {
		covens, unrestricted := e.CovenScope(aid, "soul", "coven-assign")
		p := e.ResolvePurview(aid, "soul", "coven-assign")
		if unrestricted != p.Unrestricted {
			t.Errorf("aid=%s: CovenScope.unrestricted=%v != Purview.Unrestricted=%v", aid, unrestricted, p.Unrestricted)
		}
		if !reflect.DeepEqual(covens, covensFromPurview(p)) {
			t.Errorf("aid=%s: CovenScope.covens=%v != Purview.Covens=%v", aid, covens, covensFromPurview(p))
		}
	}
}
