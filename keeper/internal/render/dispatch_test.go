package render

import (
	"context"
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

// TestIsKeeperTask_SideFollowsTheModuleAddress — NIM-749: the routing decision is
// read off the module, and a task that says nothing about its side is routed by
// its address alone.
//
// The four cases are the whole rule. A keeper-side core address with no `on:` is
// the new ordinary form; a Soul-side core address is untouched however it is
// written; a keeper-side address that still carries the (now redundant) literal
// keeps routing where it always did; and a PLUGIN address with the literal stays
// keeper-side, which is the half NIM-688 has to close before the key can go.
func TestIsKeeperTask_SideFollowsTheModuleAddress(t *testing.T) {
	mod := func(addr string) *config.ModuleTask {
		return &config.ModuleTask{Module: addr, Params: map[string]any{}}
	}
	for name, tc := range map[string]struct {
		task config.Task
		want bool
	}{
		"keeper-side core, no on:":        {config.Task{Module: mod("core.state.set")}, true},
		"keeper-side core, on: keeper":    {config.Task{On: "keeper", Module: mod("core.cloud.created")}, true},
		"soul-side core, no on:":          {config.Task{Module: mod("core.pkg.present")}, false},
		"soul-side core, on: a coven":     {config.Task{On: []any{"primary"}, Module: mod("core.exec.run")}, false},
		"plugin address, on: keeper":      {config.Task{On: "keeper", Module: mod("wb-cloud.vm.created")}, true},
		"plugin address, no on:":          {config.Task{Module: mod("wb-cloud.vm.created")}, false},
		"block task carries no module":    {config.Task{Block: &config.BlockTask{}}, false},
		"block task with the on: literal": {config.Task{On: "keeper", Block: &config.BlockTask{}}, true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := IsKeeperTask(tc.task); got != tc.want {
				t.Fatalf("IsKeeperTask = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRender_KeeperSideModuleWithoutOn_RoutesToTheKeeper — the derivation reaching
// the plan, not just the predicate. A capture written the way an author writes one
// now (no `on:` anywhere) must land on the keeper target and never touch the
// roster: routed Soul-side it would be dispatched to a host that has no such
// module, and the run would die there.
func TestRender_KeeperSideModuleWithoutOn_RoutesToTheKeeper(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "k",
		Tasks: []config.Task{
			{Name: "capture", Module: &config.ModuleTask{Module: "core.state.set", Params: map[string]any{
				"field": "owner",
				"value": "${ input.owner }",
			}}},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "svc"},
		Input:       map[string]any{"owner": "alice"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(plans) != 1 || !plans[0].Keeper {
		t.Fatalf("plans = %+v, want one plan with Keeper=true", plans)
	}
	if len(plans[0].TargetSIDs) != 1 || plans[0].TargetSIDs[0] != KeeperTargetSID {
		t.Fatalf("TargetSIDs = %v, want [%q] — a keeper task has no host roster", plans[0].TargetSIDs, KeeperTargetSID)
	}
	if got := tasks[0].Params.GetFields()["value"].GetStringValue(); got != "alice" {
		t.Fatalf("params.value = %q, want alice (rendered in the keeper context)", got)
	}
}
