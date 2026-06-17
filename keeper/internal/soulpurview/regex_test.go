package soulpurview

import (
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

// TestResolve_RegexOnly_NotPartialNotEmpty — regex-измерение S3b-2a ВЫЧИСЛЯЕТСЯ
// пилотом: Resolve больше НЕ помечает чистый regex-scope как Partial (как было в
// S3b-0) и не схлопывает в Empty. Scope.Regexes несёт паттерны для keyset-eval.
// soulprint/state остаются Partial — их пилот этого среза не вычисляет.
func TestResolve_RegexOnly_NotPartialNotEmpty(t *testing.T) {
	sc := Resolve(rbac.Purview{Regexes: []string{"^web-"}})
	if sc.Empty {
		t.Fatalf("regex-only purview → Empty=true (fail-closed на доступном regex-измерении)")
	}
	if sc.Partial {
		t.Fatalf("regex-only purview → Partial=true; want false (S3b-2a вычисляет regex)")
	}
	if !reflect.DeepEqual(sc.Regexes, []string{"^web-"}) {
		t.Fatalf("Regexes = %v, want [^web-]", sc.Regexes)
	}
}

// TestResolve_CovenPlusRegex_NotPartial — coven+regex: оба измерения вычислимы
// пилотом (coven OR regex) → НЕ Partial. Оба поля заполнены.
func TestResolve_CovenPlusRegex_NotPartial(t *testing.T) {
	sc := Resolve(rbac.Purview{Covens: []string{"prod"}, Regexes: []string{"^db-"}})
	if sc.Empty || sc.Partial {
		t.Fatalf("coven+regex → Empty=%v Partial=%v; want оба false", sc.Empty, sc.Partial)
	}
	if !reflect.DeepEqual(sc.Covens, []string{"prod"}) {
		t.Fatalf("Covens = %v, want [prod]", sc.Covens)
	}
	if !reflect.DeepEqual(sc.Regexes, []string{"^db-"}) {
		t.Fatalf("Regexes = %v, want [^db-]", sc.Regexes)
	}
}

// TestResolve_SoulprintStillPartial — soulprint/state остаются Partial в этом
// срезе (S3b-2b ещё не реализован). Граница среза: regex снят с Partial,
// soulprint/state — нет.
func TestResolve_SoulprintStillPartial(t *testing.T) {
	for name, p := range map[string]rbac.Purview{
		"soulprint":       {SoulprintExprs: []string{`soulprint.self.os.family == "debian"`}},
		"state":           {StateExprs: []string{`state.role == "primary"`}},
		"coven+soulprint": {Covens: []string{"prod"}, SoulprintExprs: []string{`x`}},
		"regex+soulprint": {Regexes: []string{"^web-"}, SoulprintExprs: []string{`x`}},
	} {
		sc := Resolve(p)
		if !sc.Partial {
			t.Errorf("%s → Partial=false; want true (soulprint/state не вычисляются в S3b-2a)", name)
		}
		if sc.Empty {
			t.Errorf("%s → Empty=true (доступ есть)", name)
		}
	}
}

// TestResolve_Keyset_RegexPresent — режимный флаг: scope с regex требует
// keyset-режима (handler выбирает путь по нему). coven-only / unrestricted —
// нет.
func TestResolve_Keyset_RegexPresent(t *testing.T) {
	if !Resolve(rbac.Purview{Regexes: []string{"^web-"}}).NeedsKeyset() {
		t.Error("regex-scope → NeedsKeyset()=false; want true")
	}
	if !Resolve(rbac.Purview{Covens: []string{"prod"}, Regexes: []string{"^db-"}}).NeedsKeyset() {
		t.Error("coven+regex-scope → NeedsKeyset()=false; want true")
	}
	if Resolve(rbac.Purview{Covens: []string{"prod"}}).NeedsKeyset() {
		t.Error("coven-only → NeedsKeyset()=true; want false (offset fast-path)")
	}
	if Resolve(rbac.Purview{Unrestricted: true}).NeedsKeyset() {
		t.Error("unrestricted → NeedsKeyset()=true; want false")
	}
}

// TestCompileScope_BadRegex_FailClosed — битый паттерн в Purview → ошибка
// компиляции (handler скрывает/пусто, НЕ 500). НЕ молчаливое игнорирование
// (иначе оператор увидел бы больше/меньше положенного).
func TestCompileScope_BadRegex_FailClosed(t *testing.T) {
	sc := Scope{Regexes: []string{"^web-", "([unclosed"}}
	if _, err := CompileScope(sc); err == nil {
		t.Fatal("CompileScope с битым паттерном → nil err; want ошибка (fail-closed)")
	}
}

// TestCompileScope_TooLong_FailClosed — паттерн сверх лимита длины (ReDoS-
// страховка: RE2 безопасен по времени, но патологически длинный паттерн
// отвергаем) → ошибка.
func TestCompileScope_TooLong_FailClosed(t *testing.T) {
	long := make([]byte, MaxRegexLen+1)
	for i := range long {
		long[i] = 'a'
	}
	sc := Scope{Regexes: []string{string(long)}}
	if _, err := CompileScope(sc); err == nil {
		t.Fatalf("CompileScope с паттерном длины %d (>%d) → nil err; want ошибка", len(long), MaxRegexLen)
	}
}

// TestVisible_OR_Union — ГЛАВНЫЙ union-инвариант: видимость = covenMatch OR
// regexMatch. Хост, матчащий ЛИШЬ ОДНО измерение, виден. Это OR, не AND.
func TestVisible_OR_Union(t *testing.T) {
	sc := Scope{Covens: []string{"prod"}, Regexes: []string{"^db-"}}
	cs, err := CompileScope(sc)
	if err != nil {
		t.Fatalf("CompileScope: %v", err)
	}

	// Хост в prod, но НЕ db-* → виден по coven-измерению.
	if !cs.Visible("web-01.example.com", []string{"prod"}) {
		t.Error("хост [prod]/web-* → невидим; want видим (coven-измерение)")
	}
	// Хост db-*, но НЕ в prod → виден по regex-измерению.
	if !cs.Visible("db-07.example.com", []string{"staging"}) {
		t.Error("хост [staging]/db-* → невидим; want видим (regex-измерение)")
	}
	// Хост в обоих → виден.
	if !cs.Visible("db-09.example.com", []string{"prod"}) {
		t.Error("хост [prod]/db-* → невидим; want видим (оба измерения)")
	}
}

// TestVisible_UnionSubset_Negative — union ⊆ Purview: хост, не матчащий НИ
// coven НИ regex, скрыт. Регресс = over-show за границу Purview.
func TestVisible_UnionSubset_Negative(t *testing.T) {
	sc := Scope{Covens: []string{"prod"}, Regexes: []string{"^db-"}}
	cs, _ := CompileScope(sc)
	if cs.Visible("web-01.example.com", []string{"staging"}) {
		t.Error("хост [staging]/web-* (ни coven, ни regex) → видим; want скрыт (union ⊆ Purview)")
	}
	if cs.Visible("app-01.example.com", nil) {
		t.Error("хост без covens / не-db → видим; want скрыт")
	}
}

// TestVisible_Unrestricted — unrestricted-scope видит любой хост.
func TestVisible_Unrestricted(t *testing.T) {
	cs, _ := CompileScope(Scope{Unrestricted: true})
	if !cs.Visible("anything.example.com", nil) {
		t.Error("unrestricted → хост невидим")
	}
}

// TestVisible_Empty_FailClosed — Empty-scope не пускает ни один хост.
func TestVisible_Empty_FailClosed(t *testing.T) {
	cs, _ := CompileScope(Scope{Empty: true})
	if cs.Visible("prod-01.example.com", []string{"prod"}) {
		t.Error("Empty-scope → хост видим (fail-OPEN!); want скрыт")
	}
}

// TestInScope_RegexOnly_NowVisible — list↔get КОНСИСТЕНТНОСТЬ (gate-fix):
// regex-only оператор видит хост в List (keyset-eval [CompiledScope.Visible]),
// и теперь InScope даёт ТО ЖЕ — матчащий regex sid виден по GET /{sid} (200, не
// 404). Регресс этого теста = list↔get рассинхрон вернулся (хост в списке, но
// 404 по прямому SID). Перевёрнут из прежнего CurrentlyFalse (S3b-2a coven-only).
func TestInScope_RegexOnly_NowVisible(t *testing.T) {
	sc := Resolve(rbac.Purview{Regexes: []string{"^web-"}})
	// Матчащий regex хост — виден независимо от covens (regex-измерение).
	if !InScope(sc, "web-01.example.com", []string{"any-coven"}) {
		t.Fatal("regex-only scope, sid=web-01 (матчит ^web-) → InScope=false; want true (list↔get консистентны)")
	}
	if !InScope(sc, "web-02.example.com", nil) {
		t.Fatal("regex-only scope, sid=web-02 без covens → InScope=false; want true (regex-измерение)")
	}
	// Не-матчащий regex хост — скрыт (union ⊆ Purview, не over-show).
	if InScope(sc, "db-01.example.com", []string{"any-coven"}) {
		t.Fatal("regex-only scope, sid=db-01 (НЕ матчит ^web-) → InScope=true; want false (вне Purview)")
	}
}

// TestInScope_CovenRegexUnion — single-read OR-union (тот же предикат, что
// [CompiledScope.Visible]): хост виден по coven ИЛИ по regex, не-по-ни-одному —
// скрыт. Симметрично List union (см. TestVisible_OR_Union).
func TestInScope_CovenRegexUnion(t *testing.T) {
	sc := Scope{Covens: []string{"prod"}, Regexes: []string{"^db-"}}
	// в prod, не db-* → виден по coven.
	if !InScope(sc, "web-01.example.com", []string{"prod"}) {
		t.Error("[prod]/web-* → InScope=false; want true (coven-измерение)")
	}
	// db-*, не в prod → виден по regex.
	if !InScope(sc, "db-07.example.com", []string{"staging"}) {
		t.Error("[staging]/db-* → InScope=false; want true (regex-измерение)")
	}
	// ни prod, ни db-* → скрыт.
	if InScope(sc, "web-01.example.com", []string{"staging"}) {
		t.Error("[staging]/web-* (ни coven, ни regex) → InScope=true; want false")
	}
}

// TestInScope_BadRegex_FailClosed — eval-error single-read: битый regex в Purview
// скрывает хост (false), а не палит существование и не паникует. fail-closed,
// симметрично listKeyset CompileScope-error-ветке.
func TestInScope_BadRegex_FailClosed(t *testing.T) {
	sc := Scope{Regexes: []string{"(unclosed"}}
	if InScope(sc, "web-01.example.com", []string{"prod"}) {
		t.Fatal("битый regex в scope → InScope=true (fail-OPEN!); want false (eval-error скрывает)")
	}
}

// TestVisible_RegexAnchoring — RE2 без авто-якоря: `^web-` матчит префикс, но
// НЕ подстроку в середине. Документирует семантику для оператора.
func TestVisible_RegexAnchoring(t *testing.T) {
	cs, _ := CompileScope(Scope{Regexes: []string{"^web-"}})
	if !cs.Visible("web-01.example.com", nil) {
		t.Error("^web- не сматчил web-01")
	}
	if cs.Visible("api-web-01.example.com", nil) {
		t.Error("^web- сматчил api-web-01 (паттерн заякорен в начало, не подстрока)")
	}
}
