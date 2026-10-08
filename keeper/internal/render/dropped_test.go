package render

import (
	"context"
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Render's half of a linear run's failure policy (docs/scenario/settings.md §4,
// NIM-948): a dropped host is targeted by nothing, stays in `soulprint.hosts` with
// `dropped: true`, and each plan names the dropped hosts its task would have reached.

const sidC = "c.example.com"

func droppedPass(t *testing.T, m *config.ScenarioManifest, active int, live, dropped []*topology.HostFacts) ([]*RenderedTask, []DispatchPlan) {
	t.Helper()
	plan, steps := config.LinearPassage(m.Tasks, m.Settings)
	tasks, plans, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), RenderInput{
		Scenario:      m,
		Input:         map[string]any{},
		Incarnation:   IncarnationMeta{ID: "svc"},
		Hosts:         live,
		Dropped:       dropped,
		TaskPassage:   plan.TaskPassage,
		ActivePassage: active,
		Steps:         steps,
		HeldTargets:   NewHeldTargets(),
	})
	if err != nil {
		t.Fatalf("Render at step %d: %v", active, err)
	}
	return tasks, plans
}

const droppedScenario = `
name: linear
tasks:
  - name: first
    module: core.exec.run
    params: { cmd: first }
  - name: count
    module: core.exec.run
    params:
      cmd: count
      args:
        - "${ string(size(soulprint.hosts)) }"
        - "${ soulprint.hosts[0].sid }"
        - "${ string(soulprint.hosts[0].dropped) }"
        - "${ string(size(soulprint.hosts.filter(h, !h.dropped))) }"
  - name: tagged
    module: core.exec.run
    on: [web]
    params: { cmd: tagged }
  - name: once
    module: core.exec.run
    run_once: true
    params: { cmd: once }
`

// ★ GUARD (user decision, NIM-948). Host a — first by SID — was dropped. No task
// targets it, soulprint.hosts still lists it first with `dropped: true`, the run's
// live hosts are the rest, and each task names it as not reached when its `on:`
// selects it — but a run_once task, which reaches one live host, does not.
//
// Mutations: leave in.Dropped out of soulprint.hosts → 2 hosts and b first; append
// it after the roster unsorted → b first; let coveredDropped ignore `on:` → a host
// outside the coven counts as not reached.
func TestRender_DroppedHostStaysListedAndIsTargetedByNothing(t *testing.T) {
	m := loadStagedManifest(t, droppedScenario)
	live := []*topology.HostFacts{host(sidB, []string{"svc"}, nil), host(sidC, []string{"svc", "web"}, nil)}
	dropped := []*topology.HostFacts{host(sidA, []string{"svc", "web"}, nil)}

	tasks, plans := droppedPass(t, m, 1, live, dropped)
	count, plan := renderedByName(t, tasks, plans, "count")
	if !reflect.DeepEqual(plan.TargetSIDs, []string{sidB, sidC}) {
		t.Errorf("count targets = %v, want the live hosts", plan.TargetSIDs)
	}
	if !reflect.DeepEqual(plan.NotReached, []string{sidA}) {
		t.Errorf("count not reached = %v, want [%s]", plan.NotReached, sidA)
	}
	args := count.Params.AsMap()["args"]
	if want := []any{"3", sidA, "true", "2"}; !reflect.DeepEqual(args, want) {
		t.Errorf("soulprint.hosts as count sees it = %v, want %v", args, want)
	}

	tasks, plans = droppedPass(t, m, 2, live, []*topology.HostFacts{host(sidA, []string{"svc"}, nil)})
	if _, plan := renderedByName(t, tasks, plans, "tagged"); len(plan.NotReached) != 0 {
		t.Errorf("tagged not reached = %v, want none: the dropped host is outside on: [web]", plan.NotReached)
	}
	tasks, plans = droppedPass(t, m, 3, live, dropped)
	if _, plan := renderedByName(t, tasks, plans, "once"); len(plan.NotReached) != 0 {
		t.Errorf("once not reached = %v, want none: run_once reaches one live host", plan.NotReached)
	}
}

const radiusScenario = `
name: linear
settings:
  on_failure: host
tasks:
  - name: plain
    module: core.exec.run
    params: { cmd: plain }
  - name: strict
    module: core.exec.run
    settings: { on_failure: scenario }
    params: { cmd: strict }
  - name: canary
    serial: 1
    settings: { serial_scope: block }
    block:
      - name: change
        module: core.exec.run
        params: { cmd: change }
      - name: check
        module: core.exec.run
        settings: { on_failure: scenario }
        params: { cmd: check }
  - name: each
    module: core.exec.run
    loop:
      items: "${ ['x', 'y'] }"
    params: { cmd: "${ item }" }
`

// ★ GUARD (settings.md §4). Every task carries the radius its failures are judged by:
// the scenario's, a task's own, a grouped child's own over its block's, and a
// loop's on every iteration. A staged run stamps nothing.
//
// Mutations: stamp a block's range before its children → `check` reads `host`;
// stamp from the step owner instead of the task → the same.
func TestRender_OnFailureStampedPerTask(t *testing.T) {
	m := loadStagedManifest(t, radiusScenario)
	want := map[string]string{"plain": "host", "strict": "scenario", "change": "host", "check": "scenario", "each": "host"}
	got := map[string]string{}
	for step := 0; step < 4; step++ {
		tasks, _ := droppedPass(t, m, step, linearHosts(), nil)
		for _, rt := range tasks {
			if rt.Passage == step && rt.Params != nil {
				if prev, seen := got[rt.Name]; seen && prev != rt.OnFailure {
					t.Errorf("%s: iterations disagree, %s and %s", rt.Name, prev, rt.OnFailure)
				}
				got[rt.Name] = rt.OnFailure
			}
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("on_failure per task = %v, want %v", got, want)
	}

	tasks, _, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), RenderInput{
		Scenario: m, Input: map[string]any{}, Incarnation: IncarnationMeta{ID: "svc"}, Hosts: linearHosts(),
	})
	if err != nil {
		t.Fatalf("staged Render: %v", err)
	}
	for _, rt := range tasks {
		if rt.OnFailure != "" {
			t.Errorf("staged render stamped %s with %q", rt.Name, rt.OnFailure)
		}
	}
}

// ★ GUARD (settings.md §4). Inside a block a child's where: sees the block's share of
// soulprint.hosts — its hosts and the dropped hosts its `on:` selects — not every
// dropped host of the run.
//
// Mutation: leave the run's dropped hosts on the block's scope → the child's
// where: counts three hosts and targets nobody.
func TestRender_BlockChildSeesTheBlocksDroppedHosts(t *testing.T) {
	m := loadStagedManifest(t, `
name: linear
tasks:
  - name: web
    on: [web]
    block:
      - name: tune
        module: core.exec.run
        where: "size(soulprint.hosts) == 2"
        params: { cmd: tune }
`)
	live := []*topology.HostFacts{host(sidB, []string{"web"}, nil), host(sidC, []string{"db"}, nil)}
	dropped := []*topology.HostFacts{host(sidA, []string{"db"}, nil), host("d.example.com", []string{"web"}, nil)}
	tasks, plans := droppedPass(t, m, 0, live, dropped)
	if _, plan := renderedByName(t, tasks, plans, "tune"); !reflect.DeepEqual(plan.TargetSIDs, []string{sidB}) {
		t.Errorf("tune targets = %v, want [%s]: the block's host and its one dropped web host", plan.TargetSIDs, sidB)
	}
}
