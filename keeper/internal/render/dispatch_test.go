package render

import (
	"sort"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// TestResolveTargets_IncarnationNameNotACoven pins down the `on:` resolve
// BEHAVIOR after ADR-008 amendment 2026-07-17/NIM-124: incarnation.name is NOT a
// Coven. Membership is a first-class relation, the roster (in.Hosts) is already
// membership-scoped, and hosts carry only real stable tags in Coven.
//
//   - omitted on: → exactly the members (the whole roster), regardless of what
//     each host carries in Coven (no name-coven filter);
//   - on: [real-coven] → AND-narrowing over stable tags;
//   - on: containing ${ incarnation.name } → validation error (fail-closed).
//
// This catches a regression if someone reintroduces the name-as-coven filter
// (then `on: [incarnation.name]` would resolve instead of erroring) or lets a
// host be selected because its Coven literally contains the incarnation name.
func TestResolveTargets_IncarnationNameNotACoven(t *testing.T) {
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}

	const incName = "svc-prod"
	// Members carry only real stable tags now (no incName in Coven). One host
	// deliberately carries a coven literally equal to the incarnation name to
	// prove it is NOT special-cased on the resolve path (it's an ordinary tag,
	// and on: [incName] still errors regardless).
	in := RenderInput{
		Incarnation: IncarnationMeta{Name: incName},
		Hosts: []*topology.HostFacts{
			{SID: "bm-1.example.com", Coven: []string{"baremetal"}},
			{SID: "bm-2.example.com", Coven: []string{"baremetal"}},
			{SID: "vm-1.example.com", Coven: []string{incName}},
		},
	}

	okCases := []struct {
		name string
		on   any
		want []string
	}{
		{
			name: "on omitted -> exactly the members",
			on:   nil,
			want: []string{"bm-1.example.com", "bm-2.example.com", "vm-1.example.com"},
		},
		{
			name: "on: [baremetal] -> AND narrows to baremetal members",
			on:   []any{"baremetal"},
			want: []string{"bm-1.example.com", "bm-2.example.com"},
		},
	}
	for _, tc := range okCases {
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

	errCases := []struct {
		name string
		on   any
	}{
		{name: "on: [incarnation.name] -> error", on: []any{"${ incarnation.name }"}},
		{name: "on: [incarnation.name, baremetal] -> error", on: []any{"${ incarnation.name }", "baremetal"}},
		{name: "on: [literal name] -> error", on: []any{incName}},
	}
	for _, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveTargets(engine, in, config.Task{On: tc.on})
			if err == nil {
				t.Fatalf("resolveTargets: expected a validation error (incarnation.name is not a Coven), got nil")
			}
		})
	}
}

// TestResolveCovenList_IncarnationNameRejected pins down that resolveCovenList
// rejects an element resolving to the incarnation name (ADR-008 amendment
// 2026-07-17/NIM-124) and passes through real stable tags unchanged.
func TestResolveCovenList_IncarnationNameRejected(t *testing.T) {
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}

	const incName = "svc-prod"
	in := RenderInput{Incarnation: IncarnationMeta{Name: incName}}

	// Real stable tags pass through unchanged.
	got, err := resolveCovenList(engine, in, []any{"baremetal", "eu-west"})
	if err != nil {
		t.Fatalf("resolveCovenList: %v", err)
	}
	if diff := strDiff(got, []string{"baremetal", "eu-west"}); diff != "" {
		t.Fatalf("coven list mismatch: %s", diff)
	}

	// The incarnation name (via CEL or as a literal) is rejected.
	for _, on := range [][]any{{"${ incarnation.name }", "baremetal"}, {incName}} {
		if _, err := resolveCovenList(engine, in, on); err == nil {
			t.Fatalf("resolveCovenList(%v): expected a validation error (incarnation.name is not a Coven), got nil", on)
		}
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
