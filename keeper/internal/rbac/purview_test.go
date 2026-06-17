package rbac

import (
	"reflect"
	"testing"
	"time"
)

// ResolvePurview (ADR-047 S0) — обобщение CovenScope в Purview. Эти тесты
// фиксируют, что наблюдаемое поведение НЕ меняется: каждый сценарий
// CovenScope имеет эквивалентный Purview-результат, а CovenScope становится
// тонкой проекцией (covens, unrestricted) ResolvePurview.

func TestResolvePurview_Wildcard_Unrestricted(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "admin", operators: []string{"archon-a"}, permissions: []string{"*"},
	})
	p := e.ResolvePurview("archon-a", "soul", "coven-assign")
	if !p.Unrestricted {
		t.Errorf("Unrestricted = false, want true for `*`")
	}
	if p.Deny {
		t.Errorf("Deny = true, want false for `*`")
	}
	if p.Covens != nil {
		t.Errorf("Covens = %v, want nil for unrestricted", p.Covens)
	}
}

func TestResolvePurview_BarePermission_Unrestricted(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "ops", operators: []string{"archon-a"}, permissions: []string{"soul.coven-assign"},
	})
	p := e.ResolvePurview("archon-a", "soul", "coven-assign")
	if !p.Unrestricted {
		t.Errorf("Unrestricted = false, want true for bare permission")
	}
	if p.Deny {
		t.Errorf("Deny = true, want false for bare permission")
	}
}

func TestResolvePurview_CovenSelector_Restricted(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "dev-ops", operators: []string{"archon-a"},
		permissions: []string{"soul.coven-assign on coven=dev,stage"},
	})
	p := e.ResolvePurview("archon-a", "soul", "coven-assign")
	if p.Unrestricted {
		t.Errorf("Unrestricted = true, want false for coven-selector")
	}
	if !reflect.DeepEqual(p.Covens, []string{"dev", "stage"}) {
		t.Errorf("Covens = %v, want [dev stage] (sorted)", p.Covens)
	}
}

// TestResolvePurview_Revoked_Deny (ADR-047 G1) — ревокнутый Архонт с активной
// ролью ЛЮБОГО измерения получает Purview{Deny:true} с пустыми полями. Это
// единая точка revoked-aware-резолва: gate (HoldsAction→false), single-read
// (soulpurview.Resolve→Empty→404), InScope (Deny→false) — все деривируются
// отсюда. Зеркало revoked-shortcut в Check (enforcer.go).
func TestResolvePurview_Revoked_Deny(t *testing.T) {
	cases := []struct {
		name string
		perm string
	}{
		{"bare", "soul.list"},
		{"wildcard", "*"},
		{"coven", "soul.list on coven=prod"},
		{"regex", `soul.list on regex='^web-'`},
		{"state", `soul.list on state='state.redis_version == "8.0"'`},
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
			p := e.ResolvePurview("archon-fired", "soul", "list")
			if !p.Deny {
				t.Errorf("revoked AID с %q: Deny = false, want true", tc.perm)
			}
			if p.Unrestricted {
				t.Errorf("revoked AID с %q: Unrestricted = true, want false (revoked не unrestricted)", tc.perm)
			}
			if p.Covens != nil || p.Regexes != nil || p.SoulprintExprs != nil || p.StateExprs != nil {
				t.Errorf("revoked AID с %q: измерения не пусты (%+v), want все nil", tc.perm, p)
			}
		})
	}
}

func TestResolvePurview_UnionAcrossRoles(t *testing.T) {
	e := mustEnforcer(t,
		fixtureRole{name: "r1", operators: []string{"archon-a"}, permissions: []string{"soul.coven-assign on coven=dev"}},
		fixtureRole{name: "r2", operators: []string{"archon-a"}, permissions: []string{"soul.coven-assign on coven=stage"}},
	)
	p := e.ResolvePurview("archon-a", "soul", "coven-assign")
	if p.Unrestricted {
		t.Errorf("Unrestricted = true, want false")
	}
	if !reflect.DeepEqual(p.Covens, []string{"dev", "stage"}) {
		t.Errorf("Covens = %v, want [dev stage] (union)", p.Covens)
	}
}

// Host-only селектор не делает оператора unrestricted по coven и не даёт ни
// одной coven-метки — текущее поведение CovenScope (covens=nil, unrestricted=false).
func TestResolvePurview_HostSelector_NotCovenScoped(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "host-ops", operators: []string{"archon-a"},
		permissions: []string{"soul.coven-assign on host=h.example.com"},
	})
	p := e.ResolvePurview("archon-a", "soul", "coven-assign")
	if p.Unrestricted {
		t.Errorf("Unrestricted = true, want false (host-only selector)")
	}
	if len(p.Covens) != 0 {
		t.Errorf("Covens = %v, want empty (host-only selector)", p.Covens)
	}
}

// AID без подходящей роли (но известный) — текущий CovenScope отдаёт
// (nil, false). В S0 НЕ меняем семантику на Deny=true: pure refactor.
func TestResolvePurview_NoMatchingPermission_NotUnrestricted(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "viewer", operators: []string{"archon-a"}, permissions: []string{"soul.list"},
	})
	p := e.ResolvePurview("archon-a", "soul", "coven-assign")
	if p.Unrestricted {
		t.Errorf("Unrestricted = true, want false for non-holder")
	}
	if len(p.Covens) != 0 {
		t.Errorf("Covens = %v, want empty for non-holder", p.Covens)
	}
}

// Неизвестный AID — CovenScope сейчас отдаёт (nil, false); проекция через
// ResolvePurview обязана быть эквивалентна.
func TestResolvePurview_UnknownAID_Empty(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "ops", operators: []string{"archon-a"}, permissions: []string{"soul.coven-assign"},
	})
	p := e.ResolvePurview("archon-ghost", "soul", "coven-assign")
	if p.Unrestricted || len(p.Covens) != 0 {
		t.Errorf("ghost AID: Covens=%v Unrestricted=%v, want empty/false", p.Covens, p.Unrestricted)
	}
}

// Заготовки S2-измерений (regexes/soulprint/state) в S0 ВСЕГДА пустые —
// доказательство, что S0 их не заполняет и S2 будет additive.
func TestResolvePurview_S2Dimensions_EmptyInS0(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "dev-ops", operators: []string{"archon-a"},
		permissions: []string{"soul.coven-assign on coven=dev"},
	})
	p := e.ResolvePurview("archon-a", "soul", "coven-assign")
	if len(p.Regexes) != 0 || len(p.SoulprintExprs) != 0 || len(p.StateExprs) != 0 {
		t.Errorf("S2-dimensions non-empty in S0: regexes=%v soulprint=%v state=%v",
			p.Regexes, p.SoulprintExprs, p.StateExprs)
	}
}

// Эквивалентность: CovenScope == проекция ResolvePurview на (Covens, Unrestricted)
// по всем характерным сценариям. Это центральный регресс-тест S0 —
// рефакторинг не меняет ни одного решения по coven-scope.
func TestCovenScope_EquivalentToResolvePurviewProjection(t *testing.T) {
	e := mustEnforcer(t,
		fixtureRole{name: "admin", operators: []string{"archon-wild"}, permissions: []string{"*"}},
		fixtureRole{name: "bare", operators: []string{"archon-bare"}, permissions: []string{"soul.coven-assign"}},
		fixtureRole{name: "scoped", operators: []string{"archon-scoped"}, permissions: []string{"soul.coven-assign on coven=dev,stage"}},
		fixtureRole{name: "host", operators: []string{"archon-host"}, permissions: []string{"soul.coven-assign on host=h.example.com"}},
		fixtureRole{name: "viewer", operators: []string{"archon-view"}, permissions: []string{"soul.list"}},
		fixtureRole{name: "u1", operators: []string{"archon-union"}, permissions: []string{"soul.coven-assign on coven=dev"}},
		fixtureRole{name: "u2", operators: []string{"archon-union"}, permissions: []string{"soul.coven-assign on coven=stage"}},
	)
	aids := []string{"archon-wild", "archon-bare", "archon-scoped", "archon-host", "archon-view", "archon-union", "archon-ghost"}
	for _, aid := range aids {
		covens, unrestricted := e.CovenScope(aid, "soul", "coven-assign")
		p := e.ResolvePurview(aid, "soul", "coven-assign")
		if unrestricted != p.Unrestricted {
			t.Errorf("aid=%s: CovenScope.unrestricted=%v != Purview.Unrestricted=%v", aid, unrestricted, p.Unrestricted)
		}
		if !reflect.DeepEqual(covens, p.Covens) {
			t.Errorf("aid=%s: CovenScope.covens=%v != Purview.Covens=%v", aid, covens, p.Covens)
		}
	}
}
