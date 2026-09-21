package statepredicate

import (
	"context"
	"errors"
	"testing"
)

// Slice S2 (ResolveIncarnations): resolver filters incarnation set by state
// predicate. Architect perf strategy is two-stage pushdown: SQL pushdown
// (BaseFilter service/coven narrows set BEFORE CEL) -> page-by-page CEL eval
// (lister streams narrowed set by pages, Matches is applied per page, whole set
// is not materialized in memory at once). TDD-first: tests pin contract BEFORE
// implementation (red).
//
// Resolver does NOT know SQL: list access is encapsulated in
// IncarnationStateLister, consumer provides adapter over incarnation.SelectAll
// (pushdown+pagination there), here it is a mock.

// fakeLister is IncarnationStateLister mock. It remembers received BaseFilter
// (check that pushdown is really passed down) and streams prebuilt Stated pages
// (simulation of already narrowed SQL result drained page-by-page).
type fakeLister struct {
	gotBase BaseFilter
	called  int // number of ListStatePages calls
	yields  int // number of yielded pages (yield calls)
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

// onePage is a helper: one-page set (most cases).
func onePage(items ...Stated) [][]Stated { return [][]Stated{items} }

func names(t *testing.T, r Resolver, l IncarnationStateLister, predicate string, base BaseFilter) []string {
	t.Helper()
	out, err := r.ResolveIncarnations(context.Background(), predicate, base, l)
	if err != nil {
		t.Fatalf("ResolveIncarnations(%q): %v", predicate, err)
	}
	return out
}

// --- CEL cuts non-matches: only incarnations with redis_version 8.0 ---

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
		t.Fatalf("got %v, want %d names (redis-a, redis-c)", got, len(want))
	}
	for _, n := range got {
		if !want[n] {
			t.Errorf("extra incarnation in selection %q", n)
		}
	}
}

// --- numeric/in CEL predicates on narrowed set ---

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
			t.Errorf("in-list: extra incarnation %q", n)
		}
	}
}

// --- pushdown: BaseFilter (service/coven) is passed to lister BEFORE CEL ---

func TestResolveIncarnations_BasePushdown(t *testing.T) {
	r := newResolver(t)
	l := &fakeLister{pages: onePage(
		Stated{Name: "redis-a", State: map[string]any{"redis_version": "8.0"}},
	)}

	base := BaseFilter{Service: "redis", Coven: "prod"}
	_ = names(t, r, l, `state.redis_version == "8.0"`, base)

	if l.called != 1 {
		t.Fatalf("lister called %d times, want 1", l.called)
	}
	// BaseFilter contains slice (Covens), compare fields element-wise.
	if l.gotBase.Service != base.Service || l.gotBase.Coven != base.Coven {
		t.Errorf("lister got base=%+v, want %+v (pushdown service/coven BEFORE CEL)", l.gotBase, base)
	}
}

// --- page-by-page: set larger than page -> all pages walked, nothing lost ---
//
// Key invariant of architect strategy: resolver does not materialize whole set,
// but runs Matches per page. Test gives three pages with their own matches on
// each; all must land in result, no page is skipped.
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
		t.Fatalf("walked %d pages, want 3 (all pages drained)", l.yields)
	}
	want := map[string]bool{"p1-hit": true, "p2-hit": true, "p3-hit": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want one hit per page (p1-hit, p2-hit, p3-hit)", got)
	}
	for _, n := range got {
		if !want[n] {
			t.Errorf("extra/lost incarnation in selection %q", n)
		}
	}
}

// --- no-such-key: incarnation without needed state field is excluded (fail-closed) ---

func TestResolveIncarnations_NoSuchKey(t *testing.T) {
	r := newResolver(t)
	l := &fakeLister{pages: onePage(
		Stated{Name: "has", State: map[string]any{"redis_version": "8.0"}},
		Stated{Name: "missing", State: map[string]any{"other": 1}}, // no redis_version
		Stated{Name: "nilstate", State: nil},                       // entirely empty state
	)}

	got := names(t, r, l, `state.redis_version == "8.0"`, BaseFilter{})

	if len(got) != 1 || got[0] != "has" {
		t.Fatalf("got %v, want [has] (no-such-key -> not in selection, fail-closed)", got)
	}
}

// --- blank predicate -> error (consistent with Compile/Matches; no match-all) ---

func TestResolveIncarnations_EmptyPredicate(t *testing.T) {
	r := newResolver(t)
	l := &fakeLister{pages: onePage(Stated{Name: "x", State: map[string]any{"k": 1}})}

	if _, err := r.ResolveIncarnations(context.Background(), "", BaseFilter{}, l); err == nil {
		t.Fatal("blank predicate: want error (no accidental match-all)")
	}
	if _, err := r.ResolveIncarnations(context.Background(), "   ", BaseFilter{}, l); err == nil {
		t.Fatal("blank predicate: want error")
	}
	// Broken/blank predicate is cut BEFORE walking the set; lister must not be touched.
	if l.called != 0 {
		t.Errorf("with invalid predicate lister called %d times, want 0 (reject before walk)", l.called)
	}
}

// --- broken predicate -> error (Compile reject BEFORE walking set) ---

func TestResolveIncarnations_BrokenPredicate(t *testing.T) {
	r := newResolver(t)
	l := &fakeLister{pages: onePage(Stated{Name: "x", State: map[string]any{"k": 1}})}

	if _, err := r.ResolveIncarnations(context.Background(), `state.k ==`, BaseFilter{}, l); err == nil {
		t.Fatal("broken predicate: want compile error")
	}
	if l.called != 0 {
		t.Errorf("broken predicate: lister called %d times, want 0", l.called)
	}
}

// --- sandbox predicate (vault/now/register/...) -> error ---

func TestResolveIncarnations_SandboxRejected(t *testing.T) {
	r := newResolver(t)
	l := &fakeLister{pages: onePage(Stated{Name: "x", State: map[string]any{"k": 1}})}

	if _, err := r.ResolveIncarnations(context.Background(), `vault("secret/x") == "y"`, BaseFilter{}, l); err == nil {
		t.Fatal("sandbox predicate: want compile/sandbox error")
	}
	if l.called != 0 {
		t.Errorf("sandbox predicate: lister called %d times, want 0", l.called)
	}
}

// --- non-bool predicate surfacing on full state -> error (predicate must be boolean) ---
//
// On empty state (Compile validation), no-such-key masks non-bool, so not-bool
// result surfaces only during eval per incarnation on page. Walk interrupts with
// error: fail-closed for predicate author, not for data.
func TestResolveIncarnations_NonBoolRejected(t *testing.T) {
	r := newResolver(t)
	l := &fakeLister{pages: onePage(Stated{Name: "x", State: map[string]any{"redis_version": "8.0"}})}

	if _, err := r.ResolveIncarnations(context.Background(), `state.redis_version`, BaseFilter{}, l); err == nil {
		t.Fatal("non-bool predicate: want error (not fail-closed)")
	}
}

// --- lister error propagates ---

func TestResolveIncarnations_ListerError(t *testing.T) {
	r := newResolver(t)
	sentinel := errors.New("db down")
	l := &fakeLister{err: sentinel}

	_, err := r.ResolveIncarnations(context.Background(), `state.k == 1`, BaseFilter{}, l)
	if !errors.Is(err, sentinel) {
		t.Fatalf("lister error should propagate: got %v", err)
	}
}

// --- empty set from lister -> empty selection (not panic, not nil dereference) ---

func TestResolveIncarnations_EmptySet(t *testing.T) {
	r := newResolver(t)
	l := &fakeLister{pages: nil}

	got := names(t, r, l, `state.k == 1`, BaseFilter{})
	if len(got) != 0 {
		t.Fatalf("empty set: got %v, want []", got)
	}
}
