package rbac

import (
	"reflect"
	"testing"
)

// ADR-047 S1 — role.default_scope: inheritance/override + default-deny for
// explicitly introduced dimensions, with three exceptions (`*` / bare-no-scope /
// introduced-but-empty). These tests are the semantics spec (TDD-first): they
// pin down the ResolvePurview contract BEFORE the implementation.

// Inheritance: a role with default_scope=coven=prod + bare-permission
// incarnation.run → the permission inherits the role's scope. Not
// unrestricted: a scope was introduced.
func TestResolvePurview_DefaultScope_Inherited(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "prod-ops", operators: []string{"archon-a"},
		defaultScope: "coven=prod",
		permissions:  []string{"incarnation.run"},
	})
	p := e.ResolvePurview("archon-a", "incarnation", "run")
	if p.Unrestricted {
		t.Errorf("Unrestricted=true, want false (bare-perm inherits default_scope)")
	}
	if p.Deny {
		t.Errorf("Deny=true, want false (scope was set non-empty)")
	}
	if !reflect.DeepEqual(p.Covens, []string{"prod"}) {
		t.Errorf("Covens=%v, want [prod] (default_scope inheritance)", p.Covens)
	}
}

// Override: a per-perm selector `on coven=staging` FULLY overrides
// default_scope=coven=prod (not a prod+staging merge — staging only).
func TestResolvePurview_DefaultScope_OverriddenByPerPerm(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "prod-ops", operators: []string{"archon-a"},
		defaultScope: "coven=prod",
		permissions:  []string{"incarnation.run on coven=staging"},
	})
	p := e.ResolvePurview("archon-a", "incarnation", "run")
	if p.Unrestricted {
		t.Errorf("Unrestricted=true, want false")
	}
	if !reflect.DeepEqual(p.Covens, []string{"staging"}) {
		t.Errorf("Covens=%v, want [staging] (override, NOT prod, NOT merge)", p.Covens)
	}
}

// BACKCOMPAT: a role WITHOUT default_scope (NULL) + bare permission →
// unrestricted. NULL default_scope = dimension NOT introduced = existing
// roles keep working.
func TestResolvePurview_NoDefaultScope_BarePermission_Unrestricted(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "ops", operators: []string{"archon-a"},
		permissions: []string{"incarnation.run"},
	})
	p := e.ResolvePurview("archon-a", "incarnation", "run")
	if !p.Unrestricted {
		t.Errorf("Unrestricted=false, want true (BACKCOMPAT: NULL scope + bare-perm)")
	}
	if p.Deny {
		t.Errorf("Deny=true, want false")
	}
}

// A role WITHOUT default_scope + a per-perm selector → behaves like S0
// (covens come from the selector).
func TestResolvePurview_NoDefaultScope_PerPermSelector(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "ops", operators: []string{"archon-a"},
		permissions: []string{"incarnation.run on coven=dev"},
	})
	p := e.ResolvePurview("archon-a", "incarnation", "run")
	if p.Unrestricted {
		t.Errorf("Unrestricted=true, want false (per-perm coven-selector)")
	}
	if !reflect.DeepEqual(p.Covens, []string{"dev"}) {
		t.Errorf("Covens=%v, want [dev]", p.Covens)
	}
}

// CRITICAL: a `*` permission is ALWAYS unrestricted, even in a role with
// default_scope. cluster-admin must not be locked by default-deny — otherwise
// the bootstrap admin locks itself out.
func TestResolvePurview_Wildcard_IgnoresDefaultScope(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "admin", operators: []string{"archon-root"},
		defaultScope: "coven=prod",
		permissions:  []string{"*"},
	})
	// `*` matches ANY (resource, action) — we check an arbitrary one.
	p := e.ResolvePurview("archon-root", "incarnation", "run")
	if !p.Unrestricted {
		t.Errorf("Unrestricted=false, want true (`*` ignores default_scope - cluster-admin is not locked)")
	}
	if p.Deny {
		t.Errorf("Deny=true, want false (`*` is always allow-all)")
	}
	if p.Covens != nil {
		t.Errorf("Covens=%v, want nil (unrestricted)", p.Covens)
	}
}

// A role's default_scope applies ONLY to THAT role's permissions — a request
// for a different (resource, action) that the role doesn't cover gets no scope.
func TestResolvePurview_DefaultScope_OnlyOwnPermissions(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "prod-ops", operators: []string{"archon-a"},
		defaultScope: "coven=prod",
		permissions:  []string{"incarnation.run"},
	})
	// The role does NOT grant soul.coven-assign → no match → empty Purview (not prod).
	p := e.ResolvePurview("archon-a", "soul", "coven-assign")
	if p.Unrestricted {
		t.Errorf("Unrestricted=true, want false (role does not cover soul.coven-assign)")
	}
	if len(p.Covens) != 0 {
		t.Errorf("Covens=%v, want empty (default_scope does not leak onto an unrelated resource)", p.Covens)
	}
}

// Union across an operator's roles: one role inherits default_scope=prod,
// the other has a per-perm coven=staging. Result: union [prod, staging].
func TestResolvePurview_UnionAcrossRoles_WithDefaultScope(t *testing.T) {
	e := mustEnforcer(t,
		fixtureRole{
			name: "prod-ops", operators: []string{"archon-a"},
			defaultScope: "coven=prod",
			permissions:  []string{"incarnation.run"},
		},
		fixtureRole{
			name: "stage-ops", operators: []string{"archon-a"},
			permissions: []string{"incarnation.run on coven=staging"},
		},
	)
	p := e.ResolvePurview("archon-a", "incarnation", "run")
	if p.Unrestricted {
		t.Errorf("Unrestricted=true, want false")
	}
	if !reflect.DeepEqual(p.Covens, []string{"prod", "staging"}) {
		t.Errorf("Covens=%v, want [prod staging] (union default_scope + per-perm)", p.Covens)
	}
}

// Union "unrestricted wins": one role is scoped (default_scope=prod), the
// other is bare-without-scope (unrestricted). At least one unrestricted →
// the result is unrestricted.
func TestResolvePurview_UnionAcrossRoles_UnrestrictedWins(t *testing.T) {
	e := mustEnforcer(t,
		fixtureRole{
			name: "prod-ops", operators: []string{"archon-a"},
			defaultScope: "coven=prod",
			permissions:  []string{"incarnation.run"},
		},
		fixtureRole{
			name: "free-ops", operators: []string{"archon-a"},
			permissions: []string{"incarnation.run"},
		},
	)
	p := e.ResolvePurview("archon-a", "incarnation", "run")
	if !p.Unrestricted {
		t.Errorf("Unrestricted=false, want true (a single bare-scope-less role -> unrestricted wins)")
	}
}

// A default_scope with multi-coven values is inherited whole (union within
// the dimension).
func TestResolvePurview_DefaultScope_MultiCoven(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "multi", operators: []string{"archon-a"},
		defaultScope: "coven=prod,stage",
		permissions:  []string{"incarnation.run"},
	})
	p := e.ResolvePurview("archon-a", "incarnation", "run")
	if !reflect.DeepEqual(p.Covens, []string{"prod", "stage"}) {
		t.Errorf("Covens=%v, want [prod stage] (multi-coven default_scope)", p.Covens)
	}
}

// BACKCOMPAT equivalence: with default_scope=NULL on ALL roles, ResolvePurview
// must behave exactly like S0 (the CovenScope projection). The central
// regression check: S1 doesn't break existing roles.
func TestResolvePurview_NoDefaultScope_EquivalentToS0(t *testing.T) {
	e := mustEnforcer(t,
		fixtureRole{name: "admin", operators: []string{"archon-wild"}, permissions: []string{"*"}},
		fixtureRole{name: "bare", operators: []string{"archon-bare"}, permissions: []string{"soul.coven-assign"}},
		fixtureRole{name: "scoped", operators: []string{"archon-scoped"}, permissions: []string{"soul.coven-assign on coven=dev,stage"}},
		fixtureRole{name: "host", operators: []string{"archon-host"}, permissions: []string{"soul.coven-assign on host=h.example.com"}},
		fixtureRole{name: "viewer", operators: []string{"archon-view"}, permissions: []string{"soul.list"}},
	)
	cases := []struct {
		aid         string
		wantUnrestr bool
		wantCovens  []string
	}{
		{"archon-wild", true, nil},
		{"archon-bare", true, nil},
		{"archon-scoped", false, []string{"dev", "stage"}},
		{"archon-host", false, nil},
		{"archon-view", false, nil},
		{"archon-ghost", false, nil},
	}
	for _, c := range cases {
		p := e.ResolvePurview(c.aid, "soul", "coven-assign")
		if p.Unrestricted != c.wantUnrestr {
			t.Errorf("aid=%s: Unrestricted=%v, want %v", c.aid, p.Unrestricted, c.wantUnrestr)
		}
		if !reflect.DeepEqual(p.Covens, c.wantCovens) {
			t.Errorf("aid=%s: Covens=%v, want %v", c.aid, p.Covens, c.wantCovens)
		}
	}
}
