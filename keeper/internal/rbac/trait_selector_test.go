package rbac

import (
	"errors"
	"testing"
)

// ADR-047 amendment / ADR-060 item 7 — trait scope dimension (NIM-128 boolean
// scope): exact `trait.<key>=value` match against incarnation.traits. Like
// coven, it is an exact-equality dimension (not a CEL predicate); the actual
// match happens in the incarnation-list/get resolver over a full ScopeInput.
// The flat map[string]string request context carries no nested traits, so a
// trait condition fails closed on the Permission.Matches path. A trait scope is
// one predicate in the Purview OR-set.

// --- Parsing trait.<key>=value ---

// trait.owner=alice parses into a single trait leaf condition.
func TestParseScope_Trait_Simple(t *testing.T) {
	p, err := ParsePermission(`incarnation.run on trait.owner=alice`)
	if err != nil {
		t.Fatalf("ParsePermission: %v", err)
	}
	if p.Scope == nil || p.Scope.String() != "trait.owner=alice" {
		t.Errorf("Scope = %v, want trait.owner=alice", p.Scope)
	}
}

// Dots/hyphens/underscores are allowed in the value (reScopeExact).
func TestParseScope_Trait_DottedValues(t *testing.T) {
	p, err := ParsePermission(`incarnation.run on trait.namespace=dba-ns_01.x`)
	if err != nil {
		t.Fatalf("ParsePermission: %v", err)
	}
	if p.Scope == nil || p.Scope.String() != "trait.namespace=dba-ns_01.x" {
		t.Errorf("Scope = %v, want trait.namespace=dba-ns_01.x", p.Scope)
	}
}

// The bare `trait=` form is gone — a trait condition MUST name its key as
// `trait.<key>=`. Bare `trait` is rejected as an unknown dimension.
func TestParseScope_Trait_BareFormRejected(t *testing.T) {
	if _, err := ParsePermission(`incarnation.run on trait=owner`); err == nil {
		t.Fatal("ParsePermission(trait=owner): want error (bare trait form gone), got nil")
	}
}

// An unquoted value with an out-of-class char (`:`) is rejected — it must be
// quoted (exact class [A-Za-z0-9_.-]).
func TestParseScope_Trait_BadCharsRejected(t *testing.T) {
	if _, err := ParsePermission(`incarnation.run on trait.owner=a:b`); err == nil {
		t.Fatal("ParsePermission(trait.owner=a:b): want error (`:` needs quoting), got nil")
	}
	if _, err := ParsePermission(`incarnation.run on trait.owner=al ice`); err == nil {
		t.Fatal("ParsePermission(trait.owner with a space): want error, got nil")
	}
}

// Empty key / empty value — rejected.
func TestParseScope_Trait_EmptyHalvesRejected(t *testing.T) {
	cases := []string{
		`incarnation.run on trait.=alice`,
		`incarnation.run on trait.owner=`,
		`incarnation.run on trait.=`,
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			if _, err := ParsePermission(in); err == nil {
				t.Fatalf("ParsePermission(%q): want error for empty key/value, got nil", in)
			}
		})
	}
}

// --- Matches: fail-closed without traits in context ---

// The flat map[string]string context carries no nested traits → the trait
// dimension fails closed (the incarnation-list/get resolver supplies the real
// match over a full ScopeInput).
func TestMatches_Trait_FailClosedWithoutTraits(t *testing.T) {
	p, err := ParsePermission(`incarnation.run on trait.owner=alice`)
	if err != nil {
		t.Fatalf("ParsePermission: %v", err)
	}
	if p.Matches("incarnation", "run", map[string]string{"incarnation": "redis-prod"}) {
		t.Error("trait-perm without nested traits in context must deny (fail-closed)")
	}
	if p.Matches("incarnation", "run", nil) {
		t.Error("trait-perm with nil context must deny")
	}
}

// --- Purview trait predicate ---

// ResolvePurview with a trait permission carries the trait predicate in Exprs.
func TestResolvePurview_Trait(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "alice-ops", operators: []string{"archon-a"},
		permissions: []string{`incarnation.run on trait.owner=alice`},
	})
	p := e.ResolvePurview("archon-a", "incarnation", "run")
	if p.Unrestricted {
		t.Error("Unrestricted=true, want false (trait-scoped)")
	}
	if len(p.Exprs) != 1 || p.Exprs[0].String() != "trait.owner=alice" {
		t.Errorf("Exprs = %v, want [trait.owner=alice]", p.Exprs)
	}
}

// default_scope=trait is inherited by a bare permission (S1 + trait together).
func TestResolvePurview_Trait_DefaultScopeInherited(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "alice-ops", operators: []string{"archon-a"},
		defaultScope: `trait.owner=alice`,
		permissions:  []string{"incarnation.run"},
	})
	p := e.ResolvePurview("archon-a", "incarnation", "run")
	if p.Unrestricted {
		t.Error("Unrestricted=true, want false (bare inherits trait default_scope)")
	}
	if len(p.Exprs) != 1 || p.Exprs[0].String() != "trait.owner=alice" {
		t.Errorf("Exprs = %v, want [trait.owner=alice] (default_scope inheritance)", p.Exprs)
	}
}

// A trait-only Purview gives HoldsAction=true (the gate sees a scoped role with
// a single trait dimension — otherwise the operator would get a 403 on its own
// list).
func TestHoldsAction_TraitOnly(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "alice-ops", operators: []string{"archon-a"},
		permissions: []string{`incarnation.list on trait.owner=alice`},
	})
	if !e.HoldsAction("archon-a", "incarnation", "list") {
		t.Error("trait-only scope must give HoldsAction=true (gate visibility)")
	}
}

// --- subset: trait = string-equality fail-closed (escalation guard) ---

func TestSubset_Trait_StringEquality(t *testing.T) {
	alice := `incarnation.run on trait.owner=alice`
	bob := `incarnation.run on trait.owner=bob`

	tests := []struct {
		name        string
		callerRaws  []string
		grantedRaws []string
		wantHeld    bool // true → ErrPermissionNotHeld (grant denied)
	}{
		{
			name:        "identical trait pair -> grant ok",
			callerRaws:  []string{alice},
			grantedRaws: []string{alice},
			wantHeld:    false,
		},
		{
			name:        "different trait pair -> DENY (fail-closed, escalation-guard)",
			callerRaws:  []string{alice},
			grantedRaws: []string{bob},
			wantHeld:    true,
		},
		{
			name:        "caller with * grants any trait",
			callerRaws:  []string{"*"},
			grantedRaws: []string{alice},
			wantHeld:    false,
		},
		{
			name:        "caller without trait-scope (bare) grants trait -> ok (bare covers)",
			callerRaws:  []string{"incarnation.run"},
			grantedRaws: []string{alice},
			wantHeld:    false,
		},
		{
			name:        "caller with trait grants bare -> DENY (bare is wider than caller trait-scope)",
			callerRaws:  []string{alice},
			grantedRaws: []string{"incarnation.run"},
			wantHeld:    true,
		},
		{
			name:        "caller with trait grants coven (different dimension) -> DENY",
			callerRaws:  []string{alice},
			grantedRaws: []string{"incarnation.run on coven=prod"},
			wantHeld:    true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			caller := mustParse(t, tc.callerRaws...)
			required := mustParse(t, tc.grantedRaws...)
			err := assertCallerCovers(caller, required)
			gotHeld := errors.Is(err, ErrPermissionNotHeld)
			if gotHeld != tc.wantHeld {
				t.Fatalf("assertCallerCovers err = %v; held=%v, want %v", err, gotHeld, tc.wantHeld)
			}
		})
	}
}
