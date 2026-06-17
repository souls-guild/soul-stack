package rbac

import (
	"reflect"
	"testing"
)

func mustEnforcer(t *testing.T, roles ...fixtureRole) *Enforcer {
	t.Helper()
	e, err := NewEnforcerFromSnapshot(snapshotOf(roles...))
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	return e
}

func TestCovenScope_Wildcard_Unrestricted(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "admin", operators: []string{"archon-a"}, permissions: []string{"*"},
	})
	covens, unrestricted := e.CovenScope("archon-a", "soul", "coven-assign")
	if !unrestricted {
		t.Errorf("unrestricted = false, want true for `*`")
	}
	if covens != nil {
		t.Errorf("covens = %v, want nil for unrestricted", covens)
	}
}

func TestCovenScope_BarePermission_Unrestricted(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "ops", operators: []string{"archon-a"}, permissions: []string{"soul.coven-assign"},
	})
	_, unrestricted := e.CovenScope("archon-a", "soul", "coven-assign")
	if !unrestricted {
		t.Errorf("unrestricted = false, want true for bare permission")
	}
}

func TestCovenScope_CovenSelector_Restricted(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "dev-ops", operators: []string{"archon-a"},
		permissions: []string{"soul.coven-assign on coven=dev,stage"},
	})
	covens, unrestricted := e.CovenScope("archon-a", "soul", "coven-assign")
	if unrestricted {
		t.Errorf("unrestricted = true, want false for coven-selector")
	}
	got := append([]string(nil), covens...)
	if !reflect.DeepEqual(got, []string{"dev", "stage"}) {
		t.Errorf("covens = %v, want [dev stage] (sorted)", got)
	}
}

func TestCovenScope_UnionAcrossRoles(t *testing.T) {
	e := mustEnforcer(t,
		fixtureRole{name: "r1", operators: []string{"archon-a"}, permissions: []string{"soul.coven-assign on coven=dev"}},
		fixtureRole{name: "r2", operators: []string{"archon-a"}, permissions: []string{"soul.coven-assign on coven=stage"}},
	)
	covens, unrestricted := e.CovenScope("archon-a", "soul", "coven-assign")
	if unrestricted {
		t.Errorf("unrestricted = true, want false")
	}
	if !reflect.DeepEqual(covens, []string{"dev", "stage"}) {
		t.Errorf("covens = %v, want [dev stage] (union)", covens)
	}
}

// Право с непустым селектором, но БЕЗ ключа coven (host=) НЕ делает оператора
// unrestricted по coven — симметрия с Permission.Matches (host-only-permission
// не сматчит запрос без host в контексте, значит ограничивает в другом
// измерении). Вклад covens=nil, unrestricted=false («не вправе по coven»).
func TestCovenScope_HostSelector_NotCovenScoped(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "host-ops", operators: []string{"archon-a"},
		permissions: []string{"soul.coven-assign on host=h.example.com"},
	})
	covens, unrestricted := e.CovenScope("archon-a", "soul", "coven-assign")
	if unrestricted {
		t.Errorf("unrestricted = true, want false (host-only selector does not grant any coven)")
	}
	if len(covens) != 0 {
		t.Errorf("covens = %v, want empty (host-only selector contributes no coven)", covens)
	}
}

func TestCovenScope_NoMatchingPermission_Empty(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "viewer", operators: []string{"archon-a"}, permissions: []string{"soul.list"},
	})
	covens, unrestricted := e.CovenScope("archon-a", "soul", "coven-assign")
	if unrestricted {
		t.Errorf("unrestricted = true, want false for non-holder")
	}
	if len(covens) != 0 {
		t.Errorf("covens = %v, want empty for non-holder", covens)
	}
}

func TestCovenScope_UnknownAID_Empty(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "ops", operators: []string{"archon-a"}, permissions: []string{"soul.coven-assign"},
	})
	covens, unrestricted := e.CovenScope("archon-ghost", "soul", "coven-assign")
	if unrestricted || len(covens) != 0 {
		t.Errorf("ghost AID: covens=%v unrestricted=%v, want empty/false", covens, unrestricted)
	}
}

func TestCovenScope_DifferentAction_NotMatched(t *testing.T) {
	// coven-scope для другого действия (soul.list) не должен «протекать» в
	// coven-assign.
	e := mustEnforcer(t, fixtureRole{
		name: "lister", operators: []string{"archon-a"},
		permissions: []string{"soul.list on coven=dev"},
	})
	covens, unrestricted := e.CovenScope("archon-a", "soul", "coven-assign")
	if unrestricted || len(covens) != 0 {
		t.Errorf("covens=%v unrestricted=%v, want empty/false (different action)", covens, unrestricted)
	}
}
