package handlers

import (
	"sort"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/herald"
)

func areaNames(areas []eventTypeArea) []string {
	out := make([]string, 0, len(areas))
	for _, a := range areas {
		out = append(out, a.Name)
	}
	return out
}

func pointNames(points []eventTypePoint) []string {
	out := make([]string, 0, len(points))
	for _, p := range points {
		out = append(out, p.Name)
	}
	return out
}

func containsName(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func TestEventTypeCatalog_List(t *testing.T) {
	h := NewEventTypeCatalogHandler(nil)
	resp := h.ListTyped()

	if len(resp.Areas) == 0 {
		t.Fatal("areas пусты")
	}
	if len(resp.PointEvents) == 0 {
		t.Fatal("point_events пусты")
	}

	// Ожидаемые области прогонов в area-glob-форме (ADR-052(b)): каталог отдаёт
	// `<area>.*`, а не голое имя области (голое имя невалидно для подписки).
	for _, want := range []string{"scenario_run.*", "command_run.*", "voyage.*", "cadence.*"} {
		if !containsName(areaNames(resp.Areas), want) {
			t.Errorf("область %q отсутствует в каталоге areas", want)
		}
	}

	// Точечные типы: drift-чек + терминал run_completed (T4a/T4b-подписки).
	for _, want := range []string{"incarnation.drift_checked", "incarnation.run_completed"} {
		if !containsName(pointNames(resp.PointEvents), want) {
			t.Errorf("точечный тип %q отсутствует в каталоге point_events", want)
		}
	}

	// Детерминированный порядок: оба списка отсортированы по name.
	assertSorted(t, "areas", areaNames(resp.Areas))
	assertSorted(t, "point_events", pointNames(resp.PointEvents))
}

func assertSorted(t *testing.T, label string, names []string) {
	t.Helper()
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Errorf("%s не отсортированы или дубль: %q >= %q", label, names[i-1], names[i])
		}
	}
}

// TestEventTypeCatalog_SingleSourceOfTruth — каталог отдаёт РОВНО то, что
// herald-валидатор считает допустимым scope (один источник правды): расширение
// runScope* в herald автоматически отражается в каталоге, рассинхрон невозможен.
func TestEventTypeCatalog_SingleSourceOfTruth(t *testing.T) {
	resp := buildEventTypeCatalog()

	// Каталог отдаёт area-glob `<area>.*`; источник правды herald.RunScopeAreas()
	// — голые имена областей. Сверяем, сняв суффикс `.*` с отданных значений.
	wantAreas := herald.RunScopeAreas()
	gotAreas := areaNames(resp.Areas)
	sort.Strings(gotAreas)
	gotAreaBare := make([]string, len(gotAreas))
	for i, a := range gotAreas {
		gotAreaBare[i] = strings.TrimSuffix(a, ".*")
	}
	if !equalStrings(gotAreaBare, wantAreas) {
		t.Errorf("areas(bare)=%v, herald.RunScopeAreas()=%v", gotAreaBare, wantAreas)
	}

	wantPoints := herald.RunScopePointEvents()
	gotPoints := pointNames(resp.PointEvents)
	sort.Strings(gotPoints)
	if !equalStrings(gotPoints, wantPoints) {
		t.Errorf("point_events=%v, herald.RunScopePointEvents()=%v", gotPoints, wantPoints)
	}

	// КАЖДЫЙ отданный каталогом тип ДОЛЖЕН проходить herald-валидатор as-is —
	// и area-glob, и точечный — без какой-либо доработки потребителем. Это и есть
	// контракт: UI кладёт значения дословно. Прогоняем ровно то, что в выдаче.
	for _, a := range gotAreas {
		if err := herald.ValidateEventTypes([]string{a}); err != nil {
			t.Errorf("area-glob %q отвергнут валидатором: %v", a, err)
		}
	}
	for _, p := range gotPoints {
		if err := herald.ValidateEventTypes([]string{p}); err != nil {
			t.Errorf("точечный тип %q отвергнут валидатором: %v", p, err)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
