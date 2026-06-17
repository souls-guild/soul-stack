package soulpurview

import (
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

// TestResolve_Empty_FailClosed — ГЛАВНЫЙ security-инвариант: Purview{} (нет ни
// одного измерения, не Unrestricted) → fail-closed: scope с Empty=true. Handler
// обязан отдать ПУСТОЙ список, НЕ весь флот. Регресс этого теста = оператор
// видит чужие хосты.
func TestResolve_Empty_FailClosed(t *testing.T) {
	sc := Resolve(rbac.Purview{})
	if !sc.Empty {
		t.Fatalf("Purview{} → Empty=false (fail-OPEN!); want Empty=true (fail-closed)")
	}
	if sc.Unrestricted {
		t.Fatalf("Purview{} → Unrestricted=true; want false")
	}
	if len(sc.Covens) != 0 {
		t.Fatalf("Purview{} → Covens=%v; want пусто", sc.Covens)
	}
}

// TestResolve_Unrestricted — Unrestricted (bare-без-default / coven=*) → весь
// список без scope-фильтра.
func TestResolve_Unrestricted(t *testing.T) {
	sc := Resolve(rbac.Purview{Unrestricted: true})
	if !sc.Unrestricted {
		t.Fatalf("Unrestricted purview → Unrestricted=false")
	}
	if sc.Empty {
		t.Fatalf("Unrestricted purview → Empty=true (отдал бы пусто вместо всего)")
	}
}

// TestResolve_SingleCoven — Covens=[prod] → подмножество prod, не пусто/не весь.
func TestResolve_SingleCoven(t *testing.T) {
	sc := Resolve(rbac.Purview{Covens: []string{"prod"}})
	if sc.Empty {
		t.Fatalf("Covens=[prod] → Empty=true (fail-closed на непустом scope!)")
	}
	if sc.Unrestricted {
		t.Fatalf("Covens=[prod] → Unrestricted=true (расширил scope до всего флота!)")
	}
	if !reflect.DeepEqual(sc.Covens, []string{"prod"}) {
		t.Fatalf("Covens=%v; want [prod]", sc.Covens)
	}
}

// TestResolve_MultiCoven_Union — Covens=[prod,staging] → union (OR внутри
// измерения), оба коврена в scope.
func TestResolve_MultiCoven_Union(t *testing.T) {
	sc := Resolve(rbac.Purview{Covens: []string{"prod", "staging"}})
	if sc.Empty || sc.Unrestricted {
		t.Fatalf("multi-coven → Empty=%v Unrestricted=%v; want оба false", sc.Empty, sc.Unrestricted)
	}
	if !reflect.DeepEqual(sc.Covens, []string{"prod", "staging"}) {
		t.Fatalf("Covens=%v; want [prod staging]", sc.Covens)
	}
}

// TestResolve_UnsupportedDimensions_Partial — soulprint/state измерения (без
// coven/regex и без Unrestricted) S3b-2a ещё НЕ умеет вычислять (page-CEL —
// S3b-2b). Resolve обязан пометить такой scope Partial=true, чтобы handler не
// выдавал частичный набор за полный (иначе хосты, доступные по soulprint, молча
// исчезнут). Empty при этом false (доступ есть). regex из Partial исключён —
// он вычисляется keyset-фильтром (см. regex_test.go).
func TestResolve_UnsupportedDimensions_Partial(t *testing.T) {
	for name, p := range map[string]rbac.Purview{
		"soulprint":       {SoulprintExprs: []string{`soulprint.self.os.family == "debian"`}},
		"state":           {StateExprs: []string{`state.role == "primary"`}},
		"coven+soulprint": {Covens: []string{"prod"}, SoulprintExprs: []string{`soulprint.self.os.family == "debian"`}},
	} {
		sc := Resolve(p)
		if sc.Empty {
			t.Errorf("%s purview → Empty=true (fail-closed на введённом доступном измерении)", name)
		}
		if sc.Unrestricted {
			t.Errorf("%s purview → Unrestricted=true (снял scope полностью)", name)
		}
		if !sc.Partial {
			t.Errorf("%s purview → Partial=false; want true (S3b-2a не вычисляет soulprint/state)", name)
		}
	}
}

// TestResolve_CovenOnly_NotPartial — чистый coven-scope полностью покрывается
// SQL-pushdown-ом пилота → Partial=false (результат полон, не требует S3b-2).
func TestResolve_CovenOnly_NotPartial(t *testing.T) {
	if Resolve(rbac.Purview{Covens: []string{"prod"}}).Partial {
		t.Fatalf("coven-only purview → Partial=true; want false (coven полностью pushdown-ится)")
	}
	if Resolve(rbac.Purview{Unrestricted: true}).Partial {
		t.Fatalf("unrestricted purview → Partial=true; want false")
	}
}

// TestInScope_Unrestricted — Unrestricted scope видит любой хост, включая хост
// вовсе без covens.
func TestInScope_Unrestricted(t *testing.T) {
	sc := Scope{Unrestricted: true}
	if !InScope(sc, "host-01.example.com", []string{"prod"}) {
		t.Fatalf("Unrestricted scope → хост [prod] вне scope; want в scope")
	}
	if !InScope(sc, "host-01.example.com", nil) {
		t.Fatalf("Unrestricted scope → хост без covens вне scope; want в scope")
	}
}

// TestInScope_Empty_FailClosed — ГЛАВНЫЙ security-инвариант single-read: Empty
// scope (fail-closed) НЕ пускает ни один хост, в т.ч. с covens. Регресс = scoped-
// оператор без прав читает чужой хост по прямому SID.
func TestInScope_Empty_FailClosed(t *testing.T) {
	sc := Scope{Empty: true}
	if InScope(sc, "host-01.example.com", []string{"prod"}) {
		t.Fatalf("Empty scope → хост [prod] в scope (fail-OPEN!); want вне scope")
	}
	if InScope(sc, "host-01.example.com", nil) {
		t.Fatalf("Empty scope → хост без covens в scope (fail-OPEN!); want вне scope")
	}
}

// TestInScope_CovenMatch — пересечение covens хоста и scope.Covens непусто →
// хост виден.
func TestInScope_CovenMatch(t *testing.T) {
	sc := Scope{Covens: []string{"prod", "staging"}}
	if !InScope(sc, "host-01.example.com", []string{"staging"}) {
		t.Fatalf("scope [prod staging] ∩ хост [staging] непусто → вне scope; want в scope")
	}
	if !InScope(sc, "host-01.example.com", []string{"dev", "prod"}) {
		t.Fatalf("scope [prod staging] ∩ хост [dev prod] непусто → вне scope; want в scope")
	}
}

// TestInScope_CovenMismatch — пересечение пусто → хост вне scope (single-read
// вернёт 404, не палит существование чужого хоста).
func TestInScope_CovenMismatch(t *testing.T) {
	sc := Scope{Covens: []string{"prod"}}
	if InScope(sc, "host-01.example.com", []string{"staging"}) {
		t.Fatalf("scope [prod] ∩ хост [staging] пусто → в scope; want вне scope")
	}
	if InScope(sc, "host-01.example.com", nil) {
		t.Fatalf("scope [prod] ∩ хост без covens → в scope; want вне scope")
	}
}
