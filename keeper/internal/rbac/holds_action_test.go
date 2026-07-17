package rbac

import (
	"testing"
	"time"
)

// HoldsAction (ADR-047 §d amendment 2026-06-04) is the existence gate for
// read endpoints. These tests prove: the gate sees ALL four Purview
// dimensions as "right exists", bare/`*` → true, no-permission → false,
// Deny → false (forward-compat S2+). This is a different question than
// scope-aware Check: the gate doesn't operate on context.

func TestHoldsAction_BarePermission_True(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "viewer", operators: []string{"archon-a"}, permissions: []string{"soul.list"},
	})
	if !e.HoldsAction("archon-a", "soul", "list") {
		t.Errorf("bare soul.list: HoldsAction = false, want true")
	}
}

func TestHoldsAction_Wildcard_True(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "admin", operators: []string{"archon-root"}, permissions: []string{"*"},
	})
	if !e.HoldsAction("archon-root", "soul", "list") {
		t.Errorf("`*` cluster-admin: HoldsAction = false, want true")
	}
}

// Existence holds for EACH Purview dimension independently — a scoped
// operator of any kind passes the gate (narrowing is done by the handler).
func TestHoldsAction_ScopedEachDimension_True(t *testing.T) {
	cases := []struct {
		name string
		perm string
	}{
		{"coven", "soul.list on coven=prod"},
		{"regex", `soul.list on regex='^web-'`},
		{"soulprint", `soul.list on soulprint='soulprint.self.os.family == "debian"'`},
		{"state", `soul.list on state='state.redis_version == "8.0"'`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := mustEnforcer(t, fixtureRole{
				name: "scoped-" + tc.name, operators: []string{"archon-s"},
				permissions: []string{tc.perm},
			})
			if !e.HoldsAction("archon-s", "soul", "list") {
				t.Errorf("%s-scoped %q: HoldsAction = false, want true (existence sees the dimension)", tc.name, tc.perm)
			}
			// Control invariant: scope-aware Check with a nil context for
			// a scoped permission gives a deny — exactly the false deny
			// HoldsAction was introduced for. If this assert ever fails
			// (Check starts letting scoped permissions through with a nil
			// context), the gate could be simplified — but that's exactly
			// why it exists today.
			if err := e.Check("archon-s", "soul", "list", nil); err == nil {
				t.Errorf("%s-scoped: Check(nil) = nil, expected a false deny (HoldsAction justification)", tc.name)
			}
		})
	}
}

// TestHoldsAction_Revoked_False (ADR-047 G1) is a direct guard on the
// revoked gap: a revoked Archon with an active role of any dimension does
// NOT hold the action through the gate. Existence of the right doesn't
// "outweigh" revoked — otherwise RequireAction would let a revoked operator
// read souls. Mirrors the revoked shortcut in Check (enforcer.go), threaded
// through ResolvePurview→Deny→false.
func TestHoldsAction_Revoked_False(t *testing.T) {
	cases := []struct {
		name string
		perm string
	}{
		{"bare", "soul.list"},
		{"wildcard", "*"},
		{"coven", "soul.list on coven=prod"},
		{"regex", `soul.list on regex='^web-'`},
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
			if e.HoldsAction("archon-fired", "soul", "list") {
				t.Errorf("revoked AID with %q: HoldsAction = true, want false (revoked is cut off before scope)", tc.perm)
			}
			// Control: the same AID WITHOUT revoked holds the action —
			// isolates the revoked effect.
			snap.Revoked = nil
			eOK, err := NewEnforcerFromSnapshot(snap)
			if err != nil {
				t.Fatalf("NewEnforcerFromSnapshot (not revoked): %v", err)
			}
			if !eOK.HoldsAction("archon-fired", "soul", "list") {
				t.Errorf("non-revoked AID with %q: HoldsAction = false, want true (control)", tc.perm)
			}
		})
	}
}

func TestHoldsAction_NoPermission_False(t *testing.T) {
	// Operator with a permission for a DIFFERENT resource.action — for
	// (soul, list) the Purview is empty.
	e := mustEnforcer(t, fixtureRole{
		name: "other", operators: []string{"archon-a"}, permissions: []string{"operator.create"},
	})
	if e.HoldsAction("archon-a", "soul", "list") {
		t.Errorf("no soul.list permission: HoldsAction = true, want false")
	}
}

func TestHoldsAction_UnknownAID_False(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "viewer", operators: []string{"archon-a"}, permissions: []string{"soul.list"},
	})
	if e.HoldsAction("archon-ghost", "soul", "list") {
		t.Errorf("unknown AID: HoldsAction = true, want false")
	}
}

// Deny=true → false (forward-compat S2+). ResolvePurview never sets Deny in
// the coven MVP, so we construct Purview directly and verify the flag is
// honored: existence must not "outweigh" an explicit scope deny.
func TestHoldsAction_Deny_False(t *testing.T) {
	// The check happens at the level of the HoldsAction predicate itself:
	// with Deny=true the result is false regardless of populated dimensions.
	// HoldsAction has no way to inject a Purview (it calls ResolvePurview,
	// which never sets Deny in the coven MVP), so we call the extracted
	// [holdsFromPurview] directly — the same source of truth as HoldsAction's
	// body (a guard on the forward-compat `if p.Deny { return false }`
	// branch, without duplicating the formula).
	denied := Purview{Deny: true, Unrestricted: true, Covens: []string{"prod"}}
	if holdsFromPurview(denied) {
		t.Errorf("Purview{Deny:true,...}: holds = true, want false (forward-compat S2+)")
	}
	// And the converse — without Deny, the same populated dimensions give true.
	allowed := Purview{Covens: []string{"prod"}}
	if !holdsFromPurview(allowed) {
		t.Errorf("Purview{Covens:[prod]}: holds = false, want true")
	}
}
