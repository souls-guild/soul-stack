package rbac

import (
	"reflect"
	"sort"
	"testing"
	"time"
)

// findEff looks up an EffectivePermission by (resource, action) in the
// PermissionsOf result. Handy for table-driven checks.
func findEff(perms []EffectivePermission, resource, action string) (EffectivePermission, bool) {
	for _, p := range perms {
		if p.Resource == resource && p.Action == action {
			return p, true
		}
	}
	return EffectivePermission{}, false
}

func TestPermissionsOf_TwoBarePermissions(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name:        "ops",
		operators:   []string{"archon-ops"},
		permissions: []string{"incarnation.run", "soul.list"},
	})

	perms := e.PermissionsOf("archon-ops")
	if len(perms) != 2 {
		t.Fatalf("expected 2 effective-permissions, got %d: %+v", len(perms), perms)
	}
	if _, ok := findEff(perms, "incarnation", "run"); !ok {
		t.Errorf("incarnation.run missing: %+v", perms)
	}
	if _, ok := findEff(perms, "soul", "list"); !ok {
		t.Errorf("soul.list missing: %+v", perms)
	}
	// A bare permission with no role default_scope is unrestricted, no scope labels.
	for _, p := range perms {
		if p.Wildcard {
			t.Errorf("%s.%s should not be wildcard", p.Resource, p.Action)
		}
		if !p.Scope.Unrestricted {
			t.Errorf("%s.%s bare-permission should be unrestricted: %+v", p.Resource, p.Action, p.Scope)
		}
	}
}

func TestPermissionsOf_ClusterAdminWildcard(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "cluster-admin", operators: []string{"archon-root"}, permissions: []string{"*"},
	})

	perms := e.PermissionsOf("archon-root")
	if len(perms) != 1 {
		t.Fatalf("cluster-admin: expected one wildcard marker, got %d: %+v", len(perms), perms)
	}
	if !perms[0].Wildcard {
		t.Errorf("cluster-admin should yield a wildcard marker: %+v", perms[0])
	}
	if perms[0].Resource != "" || perms[0].Action != "" {
		t.Errorf("wildcard marker should not carry resource/action: %+v", perms[0])
	}
}

func TestPermissionsOf_UnknownAID_Empty(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "ops", operators: []string{"archon-ops"}, permissions: []string{"soul.list"},
	})

	if perms := e.PermissionsOf("archon-nobody"); len(perms) != 0 {
		t.Errorf("unknown AID should yield empty, got: %+v", perms)
	}
}

func TestPermissionsOf_Dedup(t *testing.T) {
	// Two roles with the same right → one effective permission.
	e := mustEnforcer(t,
		fixtureRole{name: "a", operators: []string{"archon-dup"}, permissions: []string{"soul.list"}},
		fixtureRole{name: "b", operators: []string{"archon-dup"}, permissions: []string{"soul.list", "incarnation.run"}},
	)

	perms := e.PermissionsOf("archon-dup")
	if len(perms) != 2 {
		t.Fatalf("expected 2 unique permissions (dedup soul.list), got %d: %+v", len(perms), perms)
	}
	count := 0
	for _, p := range perms {
		if p.Resource == "soul" && p.Action == "list" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("soul.list should appear exactly once, appeared %d", count)
	}
}

func TestPermissionsOf_ScopeIncluded(t *testing.T) {
	// Role with default_scope=coven=prod + bare incarnation.run → the
	// effective-permission scope carries covens=[prod], NOT unrestricted.
	e := mustEnforcer(t, fixtureRole{
		name:         "prod-runner",
		operators:    []string{"archon-prod"},
		permissions:  []string{"incarnation.run"},
		defaultScope: "coven=prod",
	})

	perms := e.PermissionsOf("archon-prod")
	p, ok := findEff(perms, "incarnation", "run")
	if !ok {
		t.Fatalf("incarnation.run missing: %+v", perms)
	}
	if p.Scope.Unrestricted {
		t.Errorf("scope with default_scope=coven=prod should not be unrestricted: %+v", p.Scope)
	}
	want := []string{"prod"}
	got := append([]string(nil), covensFromPurview(p.Scope)...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("covens = %v, want %v", got, want)
	}
}

// TestPermissionsOf_Revoked_Empty (ADR-047 G1) — a revoked Archon gets an
// empty rights list from `GET /v1/me/permissions` regardless of roles. This
// mirrors the revoked-shortcut in Check/ResolvePurview: before the fix,
// PermissionsOf had an early `IsWildcard → [{Wildcard:true}]` branch BEFORE
// the revoked check, so a revoked cluster-admin (`*`) still saw its former
// wildcard marker. This guard catches a regression of that revoked-shortcut
// for both wildcard and scoped operators.
func TestPermissionsOf_Revoked_Empty(t *testing.T) {
	cases := []struct {
		name        string
		permissions []string
		// defaultScope is non-empty for the scoped case (the right is
		// restricted to a coven scope, but revoked still cuts it to empty).
		defaultScope string
	}{
		{name: "wildcard", permissions: []string{"*"}},
		{name: "scoped-coven", permissions: []string{"soul.list"}, defaultScope: "coven=prod"},
		{name: "scoped-perm-selector", permissions: []string{"incarnation.run on coven=dev,stage"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := snapshotOf(fixtureRole{
				name:         "active",
				operators:    []string{"archon-fired"},
				permissions:  tc.permissions,
				defaultScope: tc.defaultScope,
			})
			snap.Revoked = map[string]time.Time{"archon-fired": time.Now()}
			e, err := NewEnforcerFromSnapshot(snap)
			if err != nil {
				t.Fatalf("NewEnforcerFromSnapshot: %v", err)
			}
			if perms := e.PermissionsOf("archon-fired"); len(perms) != 0 {
				t.Errorf("revoked AID (%s): PermissionsOf = %+v, want empty", tc.name, perms)
			}
		})
	}
}

// TestPermissionsOf_NotRevoked_HappyPath is the control: the same roles
// WITHOUT revoked yield real rights (the revoked-branch fix didn't break the
// happy path). Wildcard → marker, scoped → real scope.
func TestPermissionsOf_NotRevoked_HappyPath(t *testing.T) {
	t.Run("wildcard", func(t *testing.T) {
		e := mustEnforcer(t, fixtureRole{
			name: "cluster-admin", operators: []string{"archon-root"}, permissions: []string{"*"},
		})
		perms := e.PermissionsOf("archon-root")
		if len(perms) != 1 || !perms[0].Wildcard {
			t.Errorf("non-revoked cluster-admin: perms = %+v, want one wildcard marker", perms)
		}
	})
	t.Run("scoped", func(t *testing.T) {
		e := mustEnforcer(t, fixtureRole{
			name: "prod-runner", operators: []string{"archon-prod"},
			permissions: []string{"incarnation.run"}, defaultScope: "coven=prod",
		})
		perms := e.PermissionsOf("archon-prod")
		p, ok := findEff(perms, "incarnation", "run")
		if !ok {
			t.Fatalf("non-revoked scoped: incarnation.run missing: %+v", perms)
		}
		if p.Scope.Unrestricted {
			t.Errorf("non-revoked scoped: scope should not be unrestricted: %+v", p.Scope)
		}
		if got := covensFromPurview(p.Scope); !reflect.DeepEqual(got, []string{"prod"}) {
			t.Errorf("non-revoked scoped: covens = %v, want [prod]", got)
		}
	})
}

func TestPermissionsOf_DeterministicOrder(t *testing.T) {
	// Stable order — UI/tests need determinism (like the catalog).
	e := mustEnforcer(t, fixtureRole{
		name:        "ops",
		operators:   []string{"archon-ops"},
		permissions: []string{"soul.list", "incarnation.run", "audit.read"},
	})

	perms := e.PermissionsOf("archon-ops")
	for i := 1; i < len(perms); i++ {
		prev := perms[i-1].Resource + "." + perms[i-1].Action
		cur := perms[i].Resource + "." + perms[i].Action
		if prev >= cur {
			t.Errorf("order not deterministic/duplicate: %q >= %q", prev, cur)
		}
	}
}
