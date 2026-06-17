package rbac

import (
	"reflect"
	"sort"
	"testing"
	"time"
)

// findEff — ищет EffectivePermission по (resource, action) в результате
// PermissionsOf. Удобный для табличных проверок.
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
	// bare-permission без default_scope роли → unrestricted, без scope-меток.
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
	// Две роли с одинаковым правом → один effective-permission.
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
	// Роль с default_scope=coven=prod + bare incarnation.run → в effective-
	// permission scope несёт covens=[prod], НЕ unrestricted.
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

// TestPermissionsOf_Revoked_Empty (ADR-047 G1) — ревокнутый Архонт получает
// пустой список прав на `GET /v1/me/permissions` независимо от ролей. Это
// зеркало revoked-shortcut в Check/ResolvePurview: до фикса PermissionsOf имел
// раннюю ветку `IsWildcard → [{Wildcard:true}]` ПЕРЕД revoked-проверкой, поэтому
// revoked cluster-admin (`*`) видел свой бывший wildcard-маркер. Guard ловит
// регресс этого revoked-shortcut для wildcard И scoped операторов.
func TestPermissionsOf_Revoked_Empty(t *testing.T) {
	cases := []struct {
		name        string
		permissions []string
		// defaultScope — ненулевой для scoped-кейса (право ограничено coven-scope,
		// но revoked всё равно режет до пустоты).
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

// TestPermissionsOf_NotRevoked_HappyPath — контроль: те же роли БЕЗ revoked
// отдают реальные права (фикс revoked-ветки не сломал happy-path). Wildcard →
// маркер, scoped → реальный scope.
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
	// Стабильный порядок — UI/тестам нужна детерминированность (как каталог).
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
