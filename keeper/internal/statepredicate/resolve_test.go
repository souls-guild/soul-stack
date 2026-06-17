package statepredicate

import (
	"context"
	"errors"
	"testing"
)

// Слайс S2 (ResolveIncarnations): резолвер фильтрует множество инкарнаций по
// state-предикату. Перф-стратегия architect — двухступенчатый pushdown:
// SQL-pushdown (BaseFilter service/coven сужает множество ДО CEL) → page-by-page
// CEL-eval (lister стримит сужённый набор страницами, Matches применяется
// per-page, весь набор разом в память не материализуется). TDD-first: тесты
// фиксируют контракт ДО реализации (red).
//
// Резолвер НЕ знает SQL: list-доступ инкапсулирован в IncarnationStateLister,
// потребитель даёт адаптер над incarnation.SelectAll (pushdown+пагинация там),
// тут — мок.

// fakeLister — мок IncarnationStateLister. Запоминает полученный BaseFilter
// (проверка, что pushdown реально пробрасывается вниз) и стримит заранее
// заготовленные страницы Stated (имитация уже-сужённого SQL-результата,
// дренируемого page-by-page).
type fakeLister struct {
	gotBase BaseFilter
	called  int // число вызовов ListStatePages
	yields  int // число отданных страниц (yield-вызовов)
	pages   [][]Stated
	err     error
}

func (f *fakeLister) ListStatePages(_ context.Context, base BaseFilter, yield func(page []Stated) error) error {
	f.gotBase = base
	f.called++
	if f.err != nil {
		return f.err
	}
	for _, p := range f.pages {
		f.yields++
		if err := yield(p); err != nil {
			return err
		}
	}
	return nil
}

// onePage — хелпер: один-страничный набор (большинство кейсов).
func onePage(items ...Stated) [][]Stated { return [][]Stated{items} }

func names(t *testing.T, r Resolver, l IncarnationStateLister, predicate string, base BaseFilter) []string {
	t.Helper()
	out, err := r.ResolveIncarnations(context.Background(), predicate, base, l)
	if err != nil {
		t.Fatalf("ResolveIncarnations(%q): %v", predicate, err)
	}
	return out
}

// --- CEL отсекает несовпавшие: только инкарнации с redis_version 8.0 ---

func TestResolveIncarnations_CELFilters(t *testing.T) {
	r := newResolver(t)
	l := &fakeLister{pages: onePage(
		Stated{Name: "redis-a", State: map[string]any{"redis_version": "8.0"}},
		Stated{Name: "redis-b", State: map[string]any{"redis_version": "7.4"}},
		Stated{Name: "redis-c", State: map[string]any{"redis_version": "8.0"}},
	)}

	got := names(t, r, l, `state.redis_version == "8.0"`, BaseFilter{Service: "redis"})

	want := map[string]bool{"redis-a": true, "redis-c": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %d имён (redis-a, redis-c)", got, len(want))
	}
	for _, n := range got {
		if !want[n] {
			t.Errorf("в выборке лишняя инкарнация %q", n)
		}
	}
}

// --- numeric/in CEL-предикаты на сужённом наборе ---

func TestResolveIncarnations_NumericAndIn(t *testing.T) {
	r := newResolver(t)
	l := &fakeLister{pages: onePage(
		Stated{Name: "big", State: map[string]any{"memory_mb": float64(2000)}},
		Stated{Name: "small", State: map[string]any{"memory_mb": float64(500)}},
	)}
	got := names(t, r, l, `state.memory_mb > 1000`, BaseFilter{})
	if len(got) != 1 || got[0] != "big" {
		t.Fatalf("numeric: got %v, want [big]", got)
	}

	l2 := &fakeLister{pages: onePage(
		Stated{Name: "v80", State: map[string]any{"redis_version": "8.0"}},
		Stated{Name: "v74", State: map[string]any{"redis_version": "7.4"}},
		Stated{Name: "v81", State: map[string]any{"redis_version": "8.1"}},
	)}
	got2 := names(t, r, l2, `state.redis_version in ["8.0","8.1"]`, BaseFilter{})
	want := map[string]bool{"v80": true, "v81": true}
	if len(got2) != len(want) {
		t.Fatalf("in-list: got %v, want v80,v81", got2)
	}
	for _, n := range got2 {
		if !want[n] {
			t.Errorf("in-list: лишняя инкарнация %q", n)
		}
	}
}

// --- pushdown: BaseFilter (service/coven) пробрасывается в lister ДО CEL ---

func TestResolveIncarnations_BasePushdown(t *testing.T) {
	r := newResolver(t)
	l := &fakeLister{pages: onePage(
		Stated{Name: "redis-a", State: map[string]any{"redis_version": "8.0"}},
	)}

	base := BaseFilter{Service: "redis", Coven: "prod"}
	_ = names(t, r, l, `state.redis_version == "8.0"`, base)

	if l.called != 1 {
		t.Fatalf("lister вызван %d раз, want 1", l.called)
	}
	// BaseFilter содержит slice (Covens) — сравниваем поля поэлементно.
	if l.gotBase.Service != base.Service || l.gotBase.Coven != base.Coven {
		t.Errorf("lister получил base=%+v, want %+v (pushdown service/coven ДО CEL)", l.gotBase, base)
	}
}

// --- page-by-page: набор больше страницы → все страницы обойдены, ничего не потеряно ---
//
// Ключевой инвариант стратегии architect: резолвер не материализует весь набор,
// а прогоняет Matches per-page. Тест даёт три страницы со «своими» матчами на
// каждой — все должны попасть в результат, ни одна страница не пропущена.
func TestResolveIncarnations_PageByPage(t *testing.T) {
	r := newResolver(t)
	l := &fakeLister{pages: [][]Stated{
		{
			{Name: "p1-hit", State: map[string]any{"redis_version": "8.0"}},
			{Name: "p1-miss", State: map[string]any{"redis_version": "7.0"}},
		},
		{
			{Name: "p2-hit", State: map[string]any{"redis_version": "8.0"}},
		},
		{
			{Name: "p3-miss", State: map[string]any{"redis_version": "6.0"}},
			{Name: "p3-hit", State: map[string]any{"redis_version": "8.0"}},
		},
	}}

	got := names(t, r, l, `state.redis_version == "8.0"`, BaseFilter{})

	if l.yields != 3 {
		t.Fatalf("обойдено %d страниц, want 3 (все страницы дренированы)", l.yields)
	}
	want := map[string]bool{"p1-hit": true, "p2-hit": true, "p3-hit": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want по одному хиту со страницы (p1-hit, p2-hit, p3-hit)", got)
	}
	for _, n := range got {
		if !want[n] {
			t.Errorf("в выборке лишняя/потерянная инкарнация %q", n)
		}
	}
}

// --- no-such-key: инкарнация без нужного state-поля не попадает (fail-closed) ---

func TestResolveIncarnations_NoSuchKey(t *testing.T) {
	r := newResolver(t)
	l := &fakeLister{pages: onePage(
		Stated{Name: "has", State: map[string]any{"redis_version": "8.0"}},
		Stated{Name: "missing", State: map[string]any{"other": 1}}, // нет redis_version
		Stated{Name: "nilstate", State: nil},                       // вовсе пустой state
	)}

	got := names(t, r, l, `state.redis_version == "8.0"`, BaseFilter{})

	if len(got) != 1 || got[0] != "has" {
		t.Fatalf("got %v, want [has] (no-such-key → не в выборке, fail-closed)", got)
	}
}

// --- пустой predicate → ошибка (консистентно с Compile/Matches; match-all не делаем) ---

func TestResolveIncarnations_EmptyPredicate(t *testing.T) {
	r := newResolver(t)
	l := &fakeLister{pages: onePage(Stated{Name: "x", State: map[string]any{"k": 1}})}

	if _, err := r.ResolveIncarnations(context.Background(), "", BaseFilter{}, l); err == nil {
		t.Fatal("пустой predicate: want error (без случайного match-all)")
	}
	if _, err := r.ResolveIncarnations(context.Background(), "   ", BaseFilter{}, l); err == nil {
		t.Fatal("blank predicate: want error")
	}
	// Битый/пустой предикат отсекается ДО обхода набора — lister не должен быть тронут.
	if l.called != 0 {
		t.Errorf("при невалидном predicate lister вызван %d раз, want 0 (reject до обхода)", l.called)
	}
}

// --- битый predicate → ошибка (Compile reject ДО обхода набора) ---

func TestResolveIncarnations_BrokenPredicate(t *testing.T) {
	r := newResolver(t)
	l := &fakeLister{pages: onePage(Stated{Name: "x", State: map[string]any{"k": 1}})}

	if _, err := r.ResolveIncarnations(context.Background(), `state.k ==`, BaseFilter{}, l); err == nil {
		t.Fatal("битый predicate: want compile error")
	}
	if l.called != 0 {
		t.Errorf("битый predicate: lister вызван %d раз, want 0", l.called)
	}
}

// --- sandbox predicate (vault/now/register/...) → ошибка ---

func TestResolveIncarnations_SandboxRejected(t *testing.T) {
	r := newResolver(t)
	l := &fakeLister{pages: onePage(Stated{Name: "x", State: map[string]any{"k": 1}})}

	if _, err := r.ResolveIncarnations(context.Background(), `vault("secret/x") == "y"`, BaseFilter{}, l); err == nil {
		t.Fatal("sandbox predicate: want compile/sandbox error")
	}
	if l.called != 0 {
		t.Errorf("sandbox predicate: lister вызван %d раз, want 0", l.called)
	}
}

// --- не-bool predicate, всплывающий на полном state → ошибка (предикат обязан быть булевым) ---
//
// На пустом state (Compile-валидация) no-such-key маскирует не-bool, поэтому
// not-bool результат всплывает только при eval per-incarnation на странице. Обход
// прерывается с ошибкой — fail-closed по автору предиката, не по данным.
func TestResolveIncarnations_NonBoolRejected(t *testing.T) {
	r := newResolver(t)
	l := &fakeLister{pages: onePage(Stated{Name: "x", State: map[string]any{"redis_version": "8.0"}})}

	if _, err := r.ResolveIncarnations(context.Background(), `state.redis_version`, BaseFilter{}, l); err == nil {
		t.Fatal("не-bool predicate: want error (не fail-closed)")
	}
}

// --- ошибка lister пробрасывается ---

func TestResolveIncarnations_ListerError(t *testing.T) {
	r := newResolver(t)
	sentinel := errors.New("db down")
	l := &fakeLister{err: sentinel}

	_, err := r.ResolveIncarnations(context.Background(), `state.k == 1`, BaseFilter{}, l)
	if !errors.Is(err, sentinel) {
		t.Fatalf("ошибка lister должна проброситься: got %v", err)
	}
}

// --- пустой набор из lister → пустая выборка (не паника, не nil-разыменование) ---

func TestResolveIncarnations_EmptySet(t *testing.T) {
	r := newResolver(t)
	l := &fakeLister{pages: nil}

	got := names(t, r, l, `state.k == 1`, BaseFilter{})
	if len(got) != 0 {
		t.Fatalf("пустой набор: got %v, want []", got)
	}
}
