package soulpurview

import (
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

// mustExpr parses a boolean scope expression for tests (panics on error).
func mustExpr(t *testing.T, s string) *rbac.ScopeExpr {
	t.Helper()
	e, err := rbac.ParseScopeExpr(s)
	if err != nil {
		t.Fatalf("ParseScopeExpr(%q): %v", s, err)
	}
	return e
}

func exprs(t *testing.T, ss ...string) []*rbac.ScopeExpr {
	out := make([]*rbac.ScopeExpr, len(ss))
	for i, s := range ss {
		out[i] = mustExpr(t, s)
	}
	return out
}

// TestScope_Empty_FailClosed is the MAIN security invariant: Purview{} (no
// predicates, not Unrestricted) → fail-closed. The handler must return an EMPTY
// list, NOT the whole fleet. Regression = an operator sees someone else's hosts.
func TestScope_Empty_FailClosed(t *testing.T) {
	sc := Resolve(rbac.Purview{})
	if !sc.Empty() {
		t.Fatalf("Purview{} → Empty()=false (fail-OPEN!); want true (fail-closed)")
	}
	if sc.Unrestricted() {
		t.Fatalf("Purview{} → Unrestricted()=true; want false")
	}
}

// TestScope_Unrestricted verifies a bare/`*` permission → the whole list without
// a scope filter.
func TestScope_Unrestricted(t *testing.T) {
	sc := Resolve(rbac.Purview{Unrestricted: true})
	if !sc.Unrestricted() {
		t.Fatalf("Unrestricted purview → Unrestricted()=false")
	}
	if sc.Empty() {
		t.Fatalf("Unrestricted purview → Empty()=true (would return empty instead of all)")
	}
}

// TestScope_NonEmpty verifies a predicate scope is neither Empty nor Unrestricted.
func TestScope_NonEmpty(t *testing.T) {
	sc := Resolve(rbac.Purview{Exprs: exprs(t, "coven=prod")})
	if sc.Empty() {
		t.Fatalf("coven=prod → Empty()=true (fail-closed on non-empty scope!)")
	}
	if sc.Unrestricted() {
		t.Fatalf("coven=prod → Unrestricted()=true (expanded to the whole fleet!)")
	}
}

// TestInScope_Unrestricted verifies an Unrestricted scope sees any host,
// including a host with no covens at all.
func TestInScope_Unrestricted(t *testing.T) {
	sc := Resolve(rbac.Purview{Unrestricted: true})
	if !InScope(sc, "host-01.example.com", []string{"prod"}, nil) {
		t.Fatalf("Unrestricted scope → host [prod] outside scope; want in scope")
	}
	if !InScope(sc, "host-01.example.com", nil, nil) {
		t.Fatalf("Unrestricted scope → host without covens outside scope; want in scope")
	}
}

// TestInScope_Empty_FailClosed is the MAIN single-read security invariant: an
// empty scope allows no host, even one carrying covens. Regression = a scoped
// operator with no rights reads someone else's host by direct SID.
func TestInScope_Empty_FailClosed(t *testing.T) {
	sc := Resolve(rbac.Purview{})
	if InScope(sc, "host-01.example.com", []string{"prod"}, nil) {
		t.Fatalf("empty scope → host [prod] in scope (fail-OPEN!); want outside")
	}
}

// TestInScope_CovenMatch verifies a non-empty intersection of host covens and a
// coven predicate → host visible; an empty intersection → hidden.
func TestInScope_CovenMatch(t *testing.T) {
	sc := Resolve(rbac.Purview{Exprs: exprs(t, "coven=prod OR coven=staging")})
	if !InScope(sc, "host-01.example.com", []string{"staging"}, nil) {
		t.Fatalf("coven∈{prod,staging} vs host [staging] → outside; want in scope")
	}
	if InScope(sc, "host-01.example.com", []string{"dev"}, nil) {
		t.Fatalf("coven∈{prod,staging} vs host [dev] → in scope; want outside (404)")
	}
}

// TestInScope_HostGlob verifies the host dimension (`host matches <glob>`) — the
// replacement for the removed regex type. A matching SID is visible; a
// non-matching one is hidden.
func TestInScope_HostGlob(t *testing.T) {
	sc := Resolve(rbac.Purview{Exprs: exprs(t, "host matches web-*")})
	if !InScope(sc, "web-01.example.com", nil, nil) {
		t.Fatalf("host matches web-* vs web-01 → outside; want in scope")
	}
	if InScope(sc, "db-01.example.com", nil, nil) {
		t.Fatalf("host matches web-* vs db-01 → in scope; want outside")
	}
}

// TestInScope_Trait verifies the trait dimension over a Soul's projected traits.
// A matching value → visible; a mismatch or absent traits → hidden (fail-closed).
func TestInScope_Trait(t *testing.T) {
	sc := Resolve(rbac.Purview{Exprs: exprs(t, "trait.tier=gold")})
	if !InScope(sc, "h", nil, map[string][]string{"tier": {"gold"}}) {
		t.Fatalf("trait.tier=gold vs {tier:gold} → outside; want in scope")
	}
	if InScope(sc, "h", nil, map[string][]string{"tier": {"silver"}}) {
		t.Fatalf("trait.tier=gold vs {tier:silver} → in scope; want outside")
	}
	if InScope(sc, "h", nil, nil) {
		t.Fatalf("trait.tier=gold vs no traits → in scope (fail-OPEN!); want outside")
	}
}

// TestTraitsInput verifies the jsonb-traits → ScopeInput projection: a scalar
// becomes a one-element slice, a list becomes many, values are stringified like
// PG's `->>`; an empty/nil map → nil.
func TestTraitsInput(t *testing.T) {
	got := TraitsInput(map[string]any{
		"tier":   "gold",
		"env":    []any{"prod", "stage"},
		"shard":  float64(7),
		"active": true,
	})
	want := map[string][]string{
		"tier":   {"gold"},
		"env":    {"prod", "stage"},
		"shard":  {"7"},
		"active": {"true"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TraitsInput = %v; want %v", got, want)
	}
	if TraitsInput(nil) != nil {
		t.Fatalf("TraitsInput(nil) → non-nil; want nil (trait condition fails closed)")
	}
}
