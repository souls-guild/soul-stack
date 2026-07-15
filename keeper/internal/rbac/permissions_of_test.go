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
		t.Fatalf("ожидали 2 effective-permission, получили %d: %+v", len(perms), perms)
	}
	if _, ok := findEff(perms, "incarnation", "run"); !ok {
		t.Errorf("incarnation.run отсутствует: %+v", perms)
	}
	if _, ok := findEff(perms, "soul", "list"); !ok {
		t.Errorf("soul.list отсутствует: %+v", perms)
	}
	// A bare permission with no role default_scope is unrestricted, no scope labels.
	for _, p := range perms {
		if p.Wildcard {
			t.Errorf("%s.%s не должен быть wildcard", p.Resource, p.Action)
		}
		if !p.Scope.Unrestricted {
			t.Errorf("%s.%s bare-permission должен быть unrestricted: %+v", p.Resource, p.Action, p.Scope)
		}
	}
}

func TestPermissionsOf_ClusterAdminWildcard(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "cluster-admin", operators: []string{"archon-root"}, permissions: []string{"*"},
	})

	perms := e.PermissionsOf("archon-root")
	if len(perms) != 1 {
		t.Fatalf("cluster-admin: ожидали один wildcard-маркер, получили %d: %+v", len(perms), perms)
	}
	if !perms[0].Wildcard {
		t.Errorf("cluster-admin должен дать wildcard-маркер: %+v", perms[0])
	}
	if perms[0].Resource != "" || perms[0].Action != "" {
		t.Errorf("wildcard-маркер не должен нести resource/action: %+v", perms[0])
	}
}

func TestPermissionsOf_UnknownAID_Empty(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "ops", operators: []string{"archon-ops"}, permissions: []string{"soul.list"},
	})

	if perms := e.PermissionsOf("archon-nobody"); len(perms) != 0 {
		t.Errorf("неизвестный AID должен дать пусто, получили: %+v", perms)
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
		t.Fatalf("ожидали 2 уникальных permission (дедуп soul.list), получили %d: %+v", len(perms), perms)
	}
	count := 0
	for _, p := range perms {
		if p.Resource == "soul" && p.Action == "list" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("soul.list должен встретиться ровно один раз, встретился %d", count)
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
		t.Fatalf("incarnation.run отсутствует: %+v", perms)
	}
	if p.Scope.Unrestricted {
		t.Errorf("scope с default_scope=coven=prod не должен быть unrestricted: %+v", p.Scope)
	}
	want := []string{"prod"}
	got := append([]string(nil), p.Scope.Covens...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("covens = %v, ожидали %v", got, want)
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
				t.Errorf("revoked AID (%s): PermissionsOf = %+v, want пусто", tc.name, perms)
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
			t.Errorf("не-revoked cluster-admin: perms = %+v, want один wildcard-маркер", perms)
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
			t.Fatalf("не-revoked scoped: incarnation.run отсутствует: %+v", perms)
		}
		if p.Scope.Unrestricted {
			t.Errorf("не-revoked scoped: scope не должен быть unrestricted: %+v", p.Scope)
		}
		if !reflect.DeepEqual(p.Scope.Covens, []string{"prod"}) {
			t.Errorf("не-revoked scoped: covens = %v, want [prod]", p.Scope.Covens)
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
			t.Errorf("порядок не детерминирован/дубль: %q >= %q", prev, cur)
		}
	}
}
