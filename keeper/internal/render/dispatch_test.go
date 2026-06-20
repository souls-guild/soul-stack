package render

import (
	"sort"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// TestResolveTargetsRootCovenNoSpecialCasing фиксирует ПОВЕДЕНИЕ резолва `on:`
// после удаления спец-ветки drop корневой метки в resolveCovenList: корневая
// Coven-метка `${ incarnation.name }` НЕ имеет особой обработки и проходит через
// общий filterByCovens наравне с прочими метками.
//
// Roster-инвариант (ADR-008, rosterSQL `WHERE $1 = ANY(coven)`): КАЖДЫЙ хост
// roster-а несёт корневую метку. Поэтому фильтрация по `incarnation.name` —
// no-op (= весь incarnation), а добавление второй метки сужает scope обычным
// AND-пересечением. Тест ловит регресс, если кто-то вернёт спец-обработку
// корневой метки (тогда кейс `[incarnation.name, baremetal]` начнёт включать
// non-baremetal хосты) или сломает roster-инвариант (тогда `[incarnation.name]`
// перестанет возвращать всех).
func TestResolveTargetsRootCovenNoSpecialCasing(t *testing.T) {
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}

	const incName = "svc-prod"
	// Roster-инвариант: у всех хостов есть корневая метка incName; часть несёт
	// ещё и baremetal.
	in := RenderInput{
		Incarnation: IncarnationMeta{Name: incName},
		Hosts: []*topology.HostFacts{
			{SID: "bm-1.example.com", Coven: []string{incName, "baremetal"}},
			{SID: "bm-2.example.com", Coven: []string{incName, "baremetal"}},
			{SID: "vm-1.example.com", Coven: []string{incName}},
		},
	}

	cases := []struct {
		name string
		on   any
		want []string
	}{
		{
			name: "root coven only → весь incarnation",
			on:   []any{"${ incarnation.name }"},
			want: []string{"bm-1.example.com", "bm-2.example.com", "vm-1.example.com"},
		},
		{
			name: "root coven + baremetal → AND сужает до baremetal",
			on:   []any{"${ incarnation.name }", "baremetal"},
			want: []string{"bm-1.example.com", "bm-2.example.com"},
		},
		{
			name: "baremetal only → контроль, только baremetal",
			on:   []any{"baremetal"},
			want: []string{"bm-1.example.com", "bm-2.example.com"},
		},
		{
			name: "on опущен → весь incarnation",
			on:   nil,
			want: []string{"bm-1.example.com", "bm-2.example.com", "vm-1.example.com"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveTargets(engine, in, config.Task{On: tc.on})
			if err != nil {
				t.Fatalf("resolveTargets: %v", err)
			}
			if diff := sidDiff(got, tc.want); diff != "" {
				t.Fatalf("targets mismatch: %s", diff)
			}
		})
	}
}

// TestResolveCovenListRootCovenRetained фиксирует, что корневая метка остаётся в
// списке резолва (а не отбрасывается): прямая проверка снятия спец-обработки на
// уровне resolveCovenList.
func TestResolveCovenListRootCovenRetained(t *testing.T) {
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}

	const incName = "svc-prod"
	in := RenderInput{Incarnation: IncarnationMeta{Name: incName}}

	got, err := resolveCovenList(engine, in, []any{"${ incarnation.name }", "baremetal"})
	if err != nil {
		t.Fatalf("resolveCovenList: %v", err)
	}
	want := []string{incName, "baremetal"}
	if diff := strDiff(got, want); diff != "" {
		t.Fatalf("coven list mismatch (root coven must be retained): %s", diff)
	}
}

func sidDiff(hosts []*topology.HostFacts, want []string) string {
	got := sidsOf(hosts)
	return strDiff(got, want)
}

func strDiff(got, want []string) string {
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	if len(g) != len(w) {
		return "got " + join(g) + ", want " + join(w)
	}
	for i := range g {
		if g[i] != w[i] {
			return "got " + join(g) + ", want " + join(w)
		}
	}
	return ""
}

func join(s []string) string {
	out := "["
	for i, v := range s {
		if i > 0 {
			out += " "
		}
		out += v
	}
	return out + "]"
}
