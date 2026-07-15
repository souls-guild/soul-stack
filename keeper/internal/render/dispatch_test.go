package render

import (
	"sort"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// TestResolveTargetsRootCovenNoSpecialCasing pins down the `on:` resolve
// BEHAVIOR after removing the special-case branch that dropped the root label
// in resolveCovenList: the root Coven label `${ incarnation.name }` has no
// special handling and flows through the common filterByCovens like any other
// label.
//
// Roster invariant (ADR-008, rosterSQL `WHERE $1 = ANY(coven)`): EVERY host in
// the roster carries the root label. So filtering by `incarnation.name` is a
// no-op (= the whole incarnation), and adding a second label narrows scope via
// ordinary AND-intersection. This test catches a regression if someone
// reintroduces special-casing for the root label (then
// `[incarnation.name, baremetal]` would start including non-baremetal hosts)
// or breaks the roster invariant (then `[incarnation.name]` would stop
// returning everyone).
func TestResolveTargetsRootCovenNoSpecialCasing(t *testing.T) {
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}

	const incName = "svc-prod"
	// Roster invariant: every host carries the root label incName; some also
	// carry baremetal.
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

// TestResolveCovenListRootCovenRetained pins down that the root label stays in
// the resolved list (not dropped): a direct check that the special-casing was
// removed at the resolveCovenList level.
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
