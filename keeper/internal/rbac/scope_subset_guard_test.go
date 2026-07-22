package rbac

import "testing"

// NIM-128 least-privilege containment guard tests (ADR-047 §C). Every DENY case
// is an escalation attempt that MUST be refused (fail-closed): the boolean
// subset check may never let a caller grant a right wider than its own.

func mustPerm(t *testing.T, s string) Permission {
	t.Helper()
	p, err := ParsePermission(s)
	if err != nil {
		t.Fatalf("ParsePermission(%q): %v", s, err)
	}
	return p
}

// holds builds caller permissions from strings and asks whether the caller may
// grant req.
func holds(t *testing.T, caller []string, req string) bool {
	t.Helper()
	var cp []Permission
	for _, c := range caller {
		cp = append(cp, mustPerm(t, c))
	}
	return callerHolds(cp, mustPerm(t, req))
}

func TestSubset_Guard(t *testing.T) {
	cases := []struct {
		name   string
		caller []string
		req    string
		want   bool // true = grant allowed
	}{
		// 1. OR-widening by value.
		{"or-widen-deny", []string{"incarnation.list on coven=a"},
			"incarnation.list on coven=a OR coven=b", false},

		// 2. Dropping a dimension constraint (dimension reset).
		{"dim-reset-deny", []string{"incarnation.list on coven=a AND host matches web-*"},
			"incarnation.list on coven=a", false},
		{"dim-narrow-allow", []string{"incarnation.list on coven=a AND host matches web-*"},
			"incarnation.list on coven=a AND host matches web-* AND trait.owner=x", true},

		// 3. Nested group.
		{"nested-deny", []string{"incarnation.list on coven in (a,b)"},
			"incarnation.list on coven=a OR (coven=c AND host=h)", false},
		{"nested-allow", []string{"incarnation.list on coven in (a,b)"},
			"incarnation.list on coven=a OR (coven=b AND host=h)", true},

		// 4. Trait.
		{"trait-or-deny", []string{"incarnation.list on trait.owner=dba"},
			"incarnation.list on trait.owner=dba OR trait.owner=platform", false},
		{"trait-narrow-allow", []string{"incarnation.list on trait.owner=dba"},
			"incarnation.list on trait.owner=dba AND coven=a", true},

		// 5. Host glob.
		{"glob-narrow-deny", []string{"incarnation.list on host matches web-*"},
			"incarnation.list on host matches web-prod-*", false},
		{"glob-exact-allow", []string{"incarnation.list on host matches web-*"},
			"incarnation.list on host=web-01", true},
		{"glob-other-deny", []string{"incarnation.list on host matches web-*"},
			"incarnation.list on host matches db-*", false},

		// 5b. Incarnation glob (NIM-128 amend) — same atom logic as host.
		{"inc-glob-narrow-deny", []string{"incarnation.list on incarnation matches prod-*"},
			"incarnation.list on incarnation matches prod-web-*", false},
		{"inc-glob-exact-allow", []string{"incarnation.list on incarnation matches prod-*"},
			"incarnation.list on incarnation=prod-01", true},

		// 6. Free dimension in caller.
		{"free-dim-deny", []string{"incarnation.list on host matches web-*"},
			"incarnation.list on coven=a", false},

		// 7. In-list subset.
		{"inlist-subset-allow", []string{"incarnation.list on coven in (a,b,c)"},
			"incarnation.list on coven in (a,b)", true},
		{"inlist-super-deny", []string{"incarnation.list on coven in (a,b)"},
			"incarnation.list on coven in (a,d)", false},

		// 8. Backward-compat union of points (caller holds values separately).
		{"union-points-allow",
			[]string{"incarnation.list on coven=a", "incarnation.list on coven=b"},
			"incarnation.list on coven in (a,b)", true},

		// 9. Unrestricted caller.
		{"wildcard-covers-all", []string{"*"},
			"incarnation.list on coven=a AND host matches x", true},
		{"bare-covers-scoped", []string{"incarnation.list"},
			"incarnation.list on coven=a", true},

		// 10. Wildcard required.
		{"grant-wildcard-deny", []string{"incarnation.list"}, "*", false},
		{"wildcard-grants-wildcard", []string{"*"}, "*", true},

		// 11. Scoped wildcard `* on <scope>` (NIM-128 amendment).
		{"bare-star-grants-scoped-star", []string{"*"}, "* on coven=a", true},
		{"scoped-star-cannot-grant-bare-star", []string{"* on coven=a"}, "*", false},
		{"scoped-star-subset-allow", []string{"* on coven in (a,b)"}, "* on coven=a", true},
		{"scoped-star-super-deny", []string{"* on coven=a"}, "* on coven=b", false},
		{"scoped-star-narrower-allow", []string{"* on coven=a"},
			"* on coven=a AND host matches web-*", true},
		{"scoped-star-covers-action-in-scope", []string{"* on coven=a"},
			"incarnation.list on coven=a", true},
		{"scoped-star-denies-action-out-of-scope", []string{"* on coven=a"},
			"incarnation.list on coven=b", false},
		{"scoped-star-not-unrestricted-on-action", []string{"* on coven=a"},
			"incarnation.list", false},

		// bare grant needs unrestricted caller.
		{"bare-grant-scoped-caller-deny",
			[]string{"incarnation.list on coven=a"}, "incarnation.list", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := holds(t, c.caller, c.req); got != c.want {
				t.Errorf("holds(%v, %q) = %v, want %v", c.caller, c.req, got, c.want)
			}
		})
	}
}

// TestScopedWildcard_Matches — a scoped `* on <scope>` (NIM-128) is bounded and
// enforced against the request context; a bare `*` stays cluster-admin.
func TestScopedWildcard_Matches(t *testing.T) {
	p := mustPerm(t, "* on coven=payments")
	if !p.IsWildcard || p.Scope == nil {
		t.Fatalf("expected a scoped wildcard, got %+v", p)
	}
	if !p.Matches("incarnation", "run", map[string]string{"coven": "payments"}) {
		t.Errorf("scoped * should match coven=payments")
	}
	if p.Matches("incarnation", "run", map[string]string{"coven": "fraud"}) {
		t.Errorf("scoped * must NOT match coven=fraud (escalation)")
	}
	if p.Matches("operator", "create", nil) {
		t.Errorf("scoped * must NOT match a cluster-level op with no coven context")
	}
	bare := mustPerm(t, "*")
	if !bare.Matches("operator", "create", nil) {
		t.Errorf("bare * must match everything (cluster-admin)")
	}
}

// TestSubset_RemovedTypeInGrant — a required permission that names a removed
// selector type fails to parse (fail-closed at the input boundary, never
// reaching the subset check as a silent widening).
func TestSubset_RemovedTypeInGrant(t *testing.T) {
	for _, s := range []string{
		"incarnation.list on regex=x",
		"incarnation.list on soulprint=x",
		"incarnation.list on state=x",
	} {
		if _, err := ParsePermission(s); err == nil {
			t.Errorf("ParsePermission(%q) should fail (removed selector type)", s)
		}
	}
}
