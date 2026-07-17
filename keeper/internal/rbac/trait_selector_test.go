package rbac

import (
	"strings"
	"testing"
)

// ADR-047 amendment / ADR-060 item 7 slice 1 — trait selector key: exact
// `key:value` match against incarnation.traits. Parallels state (S2c) /
// soulprint (S2b), but is NOT a CEL predicate — exact equality (like coven):
// Selector["trait"] carries the normalized `key:value` string, the actual
// match happens in the incarnation-list/get resolver (slice 1 item 7).
// Matches fail-closed (the map[string]string context carries no nested
// traits). Slice 1 semantics — an OR dimension of Purview.

// --- Parsing key:value ---

// trait=owner:alice parses into Selector{trait:["owner:alice"]}.
func TestParseSelector_Trait_Simple(t *testing.T) {
	p, err := ParsePermission(`incarnation.run on trait=owner:alice`)
	if err != nil {
		t.Fatalf("ParsePermission: %v", err)
	}
	got := p.Selector["trait"]
	if len(got) != 1 || got[0] != "owner:alice" {
		t.Errorf("Selector[trait] = %v, want [owner:alice]", got)
	}
}

// Dots/hyphens/underscores are allowed in both halves (reSelValue).
func TestParseSelector_Trait_DottedValues(t *testing.T) {
	p, err := ParsePermission(`incarnation.run on trait=namespace:dba-ns_01.x`)
	if err != nil {
		t.Fatalf("ParsePermission: %v", err)
	}
	if got := p.Selector["trait"]; len(got) != 1 || got[0] != "namespace:dba-ns_01.x" {
		t.Errorf("Selector[trait] = %v, want [namespace:dba-ns_01.x]", got)
	}
}

// Without `:` — rejected (the form must be key:value).
func TestParseSelector_Trait_MissingColonRejected(t *testing.T) {
	_, err := ParsePermission(`incarnation.run on trait=owner`)
	if err == nil {
		t.Fatal("ParsePermission(trait=owner): want error (must be key:value), got nil")
	}
}

// More than one `:` — rejected (exactly one separator).
func TestParseSelector_Trait_MultipleColonsRejected(t *testing.T) {
	_, err := ParsePermission(`incarnation.run on trait=owner:a:b`)
	if err == nil {
		t.Fatal("ParsePermission(trait=owner:a:b): want error (exactly one ':'), got nil")
	}
}

// Empty key / empty value — rejected.
func TestParseSelector_Trait_EmptyHalvesRejected(t *testing.T) {
	cases := []string{
		`incarnation.run on trait=:alice`,
		`incarnation.run on trait=owner:`,
		`incarnation.run on trait=:`,
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			if _, err := ParsePermission(in); err == nil {
				t.Fatalf("ParsePermission(%q): want error for empty key/value, got nil", in)
			}
		})
	}
}

// Invalid characters (space) in the value — rejected (scalar-only, reSelValue).
func TestParseSelector_Trait_BadCharsRejected(t *testing.T) {
	_, err := ParsePermission(`incarnation.run on trait=owner:al ice`)
	if err == nil {
		t.Fatal("ParsePermission(trait with a space): want error, got nil")
	}
}

// --- Matches: fail-closed without traits in context ---

// The current map[string]string context carries no nested traits → the
// trait dimension fail-closed denies (the incarnation-list/get resolver
// supplies the real match).
func TestMatches_Trait_FailClosedWithoutTraits(t *testing.T) {
	p, err := ParsePermission(`incarnation.run on trait=owner:alice`)
	if err != nil {
		t.Fatalf("ParsePermission: %v", err)
	}
	if p.Matches("incarnation", "run", map[string]string{"incarnation": "redis-prod", "trait": "owner:alice"}) {
		t.Error("trait-perm without nested traits in context must deny (slice 1 fail-closed)")
	}
	if p.Matches("incarnation", "run", nil) {
		t.Error("trait-perm with nil context must deny")
	}
}

// --- Purview.TraitExprs ---

// ResolvePurview with a trait permission populates Purview.TraitExprs.
func TestResolvePurview_Trait(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "alice-ops", operators: []string{"archon-a"},
		permissions: []string{`incarnation.run on trait=owner:alice`},
	})
	p := e.ResolvePurview("archon-a", "incarnation", "run")
	if p.Unrestricted {
		t.Error("Unrestricted=true, want false (trait-scoped)")
	}
	if len(p.TraitExprs) != 1 || p.TraitExprs[0] != "owner:alice" {
		t.Errorf("TraitExprs = %v, want [owner:alice]", p.TraitExprs)
	}
}

// default_scope=trait is inherited by a bare permission (S1 + trait together).
func TestResolvePurview_Trait_DefaultScopeInherited(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "alice-ops", operators: []string{"archon-a"},
		defaultScope: `trait=owner:alice`,
		permissions:  []string{"incarnation.run"},
	})
	p := e.ResolvePurview("archon-a", "incarnation", "run")
	if p.Unrestricted {
		t.Error("Unrestricted=true, want false (bare inherits trait default_scope)")
	}
	if len(p.TraitExprs) != 1 || p.TraitExprs[0] != "owner:alice" {
		t.Errorf("TraitExprs = %v, want [owner:alice] (default_scope inheritance)", p.TraitExprs)
	}
}

// A trait-only Purview gives HoldsAction=true (the gate sees a scoped role
// with a single trait dimension — otherwise the operator would get a 403 on
// its own list).
func TestHoldsAction_TraitOnly(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "alice-ops", operators: []string{"archon-a"},
		permissions: []string{`incarnation.list on trait=owner:alice`},
	})
	if !e.HoldsAction("archon-a", "incarnation", "list") {
		t.Error("trait-only scope must give HoldsAction=true (gate visibility)")
	}
}

// --- subset: trait = string-equality fail-closed (escalation guard) ---

func TestSubset_Trait_StringEquality(t *testing.T) {
	alice := `incarnation.run on trait=owner:alice`
	bob := `incarnation.run on trait=owner:bob`

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
			gotHeld := strings.Contains(errString(err), "least-privilege")
			if gotHeld != tc.wantHeld {
				t.Fatalf("assertCallerCovers err = %v; held=%v, want %v", err, gotHeld, tc.wantHeld)
			}
		})
	}
}
