package rbac

import (
	"testing"
	"time"
)

// HoldsAction (ADR-047 §г amendment 2026-06-04) — existence-gate read-эндпоинтов.
// Тесты доказывают: gate видит ВСЕ четыре измерения Purview как «существование
// права», bare/`*` → true, no-permission → false, Deny → false (forward-compat
// S2+). Это другой вопрос, чем scope-aware Check: gate не оперирует контекстом.

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

// Existence держится по КАЖДОМУ измерению Purview по отдельности —
// scoped-оператор любого вида проходит gate (сужение делает handler).
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
				t.Errorf("%s-scoped %q: HoldsAction = false, want true (existence видит измерение)", tc.name, tc.perm)
			}
			// Контрольный инвариант: scope-aware Check с nil-контекстом для
			// scoped-permission даёт deny — ровно тот ложный deny, ради
			// которого введён HoldsAction. Если этот assert когда-нибудь
			// упадёт (Check начнёт пускать scoped при nil-контексте), gate
			// можно будет упростить — но пока он именно зачем нужен.
			if err := e.Check("archon-s", "soul", "list", nil); err == nil {
				t.Errorf("%s-scoped: Check(nil) = nil, ожидался ложный deny (обоснование HoldsAction)", tc.name)
			}
		})
	}
}

// TestHoldsAction_Revoked_False (ADR-047 G1) — прямой guard на revoked-gap:
// ревокнутый Архонт с активной ролью любого измерения НЕ держит действие через
// gate. Существование права не «перевешивает» revoked — иначе RequireAction
// пустил бы revoked-оператора к read-souls. Зеркало revoked-shortcut в Check
// (enforcer.go), проведённое через ResolvePurview→Deny→false.
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
				t.Errorf("revoked AID с %q: HoldsAction = true, want false (revoked отрезается до scope)", tc.perm)
			}
			// Контроль: тот же AID БЕЗ revoked держит действие — изоляция эффекта revoked.
			snap.Revoked = nil
			eOK, err := NewEnforcerFromSnapshot(snap)
			if err != nil {
				t.Fatalf("NewEnforcerFromSnapshot (not revoked): %v", err)
			}
			if !eOK.HoldsAction("archon-fired", "soul", "list") {
				t.Errorf("НЕ-revoked AID с %q: HoldsAction = false, want true (контроль)", tc.perm)
			}
		})
	}
}

func TestHoldsAction_NoPermission_False(t *testing.T) {
	// Оператор с правом на ДРУГОЙ resource.action — для (soul, list) Purview пуст.
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

// Deny=true → false (forward-compat S2+). ResolvePurview в coven-MVP Deny
// никогда не выставляет, поэтому конструируем Purview напрямую и проверяем,
// что флаг учитывается: existence не должно «перевешивать» явный scope-deny.
func TestHoldsAction_Deny_False(t *testing.T) {
	// Проверка идёт на уровне самого предиката HoldsAction: при Deny=true
	// результат false независимо от заполненных измерений. У HoldsAction нет
	// инъекции Purview (он зовёт ResolvePurview, а тот в coven-MVP Deny не
	// выставляет), поэтому вызываем напрямую вынесенную [holdsFromPurview] —
	// тот же источник правды, что и тело HoldsAction (guard на forward-compat-
	// ветку `if p.Deny { return false }`, без дубликата формулы).
	denied := Purview{Deny: true, Unrestricted: true, Covens: []string{"prod"}}
	if holdsFromPurview(denied) {
		t.Errorf("Purview{Deny:true,...}: holds = true, want false (forward-compat S2+)")
	}
	// И обратное — без Deny те же заполненные измерения дают true.
	allowed := Purview{Covens: []string{"prod"}}
	if !holdsFromPurview(allowed) {
		t.Errorf("Purview{Covens:[prod]}: holds = false, want true")
	}
}
