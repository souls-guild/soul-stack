package rbac

import (
	"reflect"
	"testing"
)

// ADR-047 S1 — role.default_scope: наследование/override + default-deny по
// явно введённым измерениям с тремя исключениями (`*` / bare-без-scope /
// введённое-пустое). Эти тесты — спецификация семантики (TDD-first), они
// фиксируют контракт ResolvePurview ДО реализации.

// Наследование: роль с default_scope=coven=prod + bare-permission incarnation.run
// → permission наследует scope роли. Не unrestricted: scope введён.
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
		t.Errorf("Deny=true, want false (scope введён непустой)")
	}
	if !reflect.DeepEqual(p.Covens, []string{"prod"}) {
		t.Errorf("Covens=%v, want [prod] (наследование default_scope)", p.Covens)
	}
}

// Override: per-perm-селектор `on coven=staging` ПОЛНОСТЬЮ переопределяет
// default_scope=coven=prod (не мерж prod+staging → только staging).
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
		t.Errorf("Covens=%v, want [staging] (override, НЕ prod, НЕ merge)", p.Covens)
	}
}

// BACKCOMPAT: роль БЕЗ default_scope (NULL) + bare-permission → unrestricted.
// NULL default_scope = измерение НЕ введено = существующие роли не ломаются.
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

// Роль БЕЗ default_scope + per-perm-селектор → как S0 (covens из селектора).
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

// КРИТИЧНЫЙ: `*`-permission ВСЕГДА unrestricted, даже в роли с default_scope.
// cluster-admin не залочен default-deny — иначе bootstrap-админ запирается.
func TestResolvePurview_Wildcard_IgnoresDefaultScope(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "admin", operators: []string{"archon-root"},
		defaultScope: "coven=prod",
		permissions:  []string{"*"},
	})
	// `*` матчит ЛЮБОЙ (resource, action) — проверяем на произвольном.
	p := e.ResolvePurview("archon-root", "incarnation", "run")
	if !p.Unrestricted {
		t.Errorf("Unrestricted=false, want true (`*` игнорирует default_scope — cluster-admin не залочен)")
	}
	if p.Deny {
		t.Errorf("Deny=true, want false (`*` всегда allow-all)")
	}
	if p.Covens != nil {
		t.Errorf("Covens=%v, want nil (unrestricted)", p.Covens)
	}
}

// default_scope роли применяется ТОЛЬКО к permissions ЭТОЙ роли — запрос на
// другой (resource, action), которого роль не покрывает, scope не получает.
func TestResolvePurview_DefaultScope_OnlyOwnPermissions(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "prod-ops", operators: []string{"archon-a"},
		defaultScope: "coven=prod",
		permissions:  []string{"incarnation.run"},
	})
	// Роль НЕ даёт soul.coven-assign → не матчит → пустой Purview (не prod).
	p := e.ResolvePurview("archon-a", "soul", "coven-assign")
	if p.Unrestricted {
		t.Errorf("Unrestricted=true, want false (роль не покрывает soul.coven-assign)")
	}
	if len(p.Covens) != 0 {
		t.Errorf("Covens=%v, want empty (default_scope не утекает на чужой resource)", p.Covens)
	}
}

// Union по ролям оператора: одна роль наследует default_scope=prod, другая —
// per-perm coven=staging. Итог — union [prod, staging].
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

// Union «unrestricted побеждает»: одна роль scoped (default_scope=prod), другая
// bare-без-scope (unrestricted). Хотя бы одна unrestricted → итог unrestricted.
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
		t.Errorf("Unrestricted=false, want true (одна роль bare-без-scope → unrestricted побеждает)")
	}
}

// default_scope с multi-coven значениями наследуется целиком (union внутри
// измерения).
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

// BACKCOMPAT-эквивалентность: с default_scope=NULL во ВСЕХ ролях ResolvePurview
// обязан вести себя ровно как S0 (CovenScope-проекция). Центральный регресс —
// S1 не ломает существующие роли.
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
