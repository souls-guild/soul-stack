package render

import (
	"context"
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Render's half of rescue routing (docs/scenario/settings.md §4, NIM-951): a dropped
// host is the target of an `onfail:` task whose source failed on it, and past the end
// of a run a failure ended only a rescue that reaches a host renders, with no `where:`
// evaluated anywhere.

const rescueScenario = `
name: linear
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: report
    module: core.exec.run
    params: { cmd: "${ register.deploy.stdout }" }
  - name: rollback
    module: core.exec.run
    onfail: [deploy]
    params: { cmd: rollback }
  - name: cleanup
    on: [web]
    block:
      - name: undo
        module: core.exec.run
        onfail: [deploy]
        params: { cmd: undo }
      - name: tidy
        module: core.exec.run
        params: { cmd: tidy }
  - name: canary
    serial: 1
    settings: { serial_scope: block }
    block:
      - name: revert
        module: core.exec.run
        onfail: [deploy]
        params: { cmd: revert }
      - name: probe
        module: core.exec.run
        params: { cmd: "${ register.deploy.stdout }" }
  - name: gated
    where: "register.deploy.stdout == 'ok'"
    block:
      - name: wipe
        module: core.exec.run
        params: { cmd: wipe }
  - name: migrate
    module: core.exec.run
    register: migrate
    params: { cmd: migrate }
  - name: rollback_migrate
    module: core.exec.run
    onfail: [migrate]
    params: { cmd: "${ register.migrate.stdout }" }
  - name: notify
    module: core.exec.run
    onfail: [deploy]
    run_once: true
    where: "register.migrate.stdout == 'ok'"
    params: { cmd: notify }
  - name: pair
    serial: 2
    settings: { serial_scope: block }
    where: "register.migrate.stdout == 'ok'"
    block:
      - name: stage
        module: core.exec.run
        register: stage
        params: { cmd: stage }
      - name: unstage
        module: core.exec.run
        onfail: [stage]
        params: { cmd: unstage }
  - name: outer
    block:
      - name: inner
        where: "register.deploy.stdout == 'ok'"
        block:
          - name: scrub
            module: core.exec.run
            params: { cmd: scrub }
  - name: outer2
    block:
      - name: inner2
        on: [web]
        block:
          - name: undo4
            module: core.exec.run
            onfail: [deploy]
            params: { cmd: undo4 }
`

func rescuePass(t *testing.T, m *config.ScenarioManifest, active int, live, dropped []*topology.HostFacts, reg map[string]map[string]any, rescue *Rescue, destiny ...DestinyResolver) ([]*RenderedTask, []DispatchPlan) {
	t.Helper()
	plan, steps := config.LinearPassage(m.Tasks, m.Settings)
	in := RenderInput{
		Scenario:       m,
		Input:          map[string]any{},
		Incarnation:    IncarnationMeta{ID: "svc"},
		Hosts:          live,
		Dropped:        dropped,
		TaskPassage:    plan.TaskPassage,
		ActivePassage:  active,
		Steps:          steps,
		HeldTargets:    NewHeldTargets(),
		RegisterByHost: reg,
		Rescue:         rescue,
	}
	if len(destiny) > 0 {
		in.Destiny = destiny[0]
	}
	tasks, plans, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render at step %d: %v", active, err)
	}
	return tasks, plans
}

func rescueRegisters() map[string]map[string]any {
	ok := map[string]any{"changed": true, "failed": false, "stdout": "ok"}
	return map[string]map[string]any{
		sidA: {"deploy": map[string]any{"changed": false, "failed": true}},
		sidB: {"deploy": ok, "migrate": ok},
		sidC: {"deploy": ok, "migrate": ok},
	}
}

// ★ GUARD (settings.md §4, fork 1). `deploy` failed on host a, which was dropped
// under `on_failure: host`; host c was dropped at another task. The rescue reaches a —
// and the live host b, whose gate drops it later — but not c, which stays not
// reached; an ordinary task reaches no dropped host; a rescue inside a block reaches
// a when the block's `on:` selects it — at the top level and nested — and never d,
// outside it. A source failing at or after the rescue's step does not route it. A
// `run_once:` rescue goes to the host it fires on, its `where:` not evaluated there.
//
// Mutations: filter a rescue by the live roster only → a not targeted; route a
// dropped host without the source's failure → c targeted; read a failure of the
// rescue's own step → a targeted when deploy failed at step 2; pick a `run_once:`
// rescue's host by SID alone → notify goes to b, where it does not fire; leave a
// block's `on:` out of the rescue cover, top-level or nested → undo or undo4 reaches d;
// append the dropped hosts to the live slice resolveTargets returns → the run's own
// roster, with room to spare, is overwritten by a.
func TestRender_RescueReachesTheDroppedHostItsSourceFailedOn(t *testing.T) {
	m := loadStagedManifest(t, rescueScenario)
	live := []*topology.HostFacts{host(sidB, []string{"svc", "web"}, nil)}
	dropped := []*topology.HostFacts{host(sidA, []string{"svc", "web"}, nil), host(sidC, []string{"svc", "web"}, nil)}
	rescue := &Rescue{Failed: map[string]map[string]int{sidA: {"deploy": 0}}}

	tasks, plans := rescuePass(t, m, 1, live, dropped, rescueRegisters(), rescue)
	if _, plan := renderedByName(t, tasks, plans, "report"); !reflect.DeepEqual(plan.TargetSIDs, []string{sidB}) || !reflect.DeepEqual(plan.NotReached, []string{sidA, sidC}) {
		t.Errorf("report targets %v, not reached %v - want [b], [a c]: an ordinary task reaches no dropped host", plan.TargetSIDs, plan.NotReached)
	}

	tasks, plans = rescuePass(t, m, 2, live, dropped, rescueRegisters(), rescue)
	if _, plan := renderedByName(t, tasks, plans, "rollback"); !reflect.DeepEqual(plan.TargetSIDs, []string{sidA, sidB}) || !reflect.DeepEqual(plan.NotReached, []string{sidC}) {
		t.Errorf("rollback targets %v, not reached %v - want [a b], [c]", plan.TargetSIDs, plan.NotReached)
	}

	tasks, plans = rescuePass(t, m, 3, live, dropped, rescueRegisters(), rescue)
	if _, plan := renderedByName(t, tasks, plans, "undo"); !reflect.DeepEqual(plan.TargetSIDs, []string{sidA, sidB}) {
		t.Errorf("undo targets %v, want [a b]: the block's on: selects a, and deploy failed there", plan.TargetSIDs)
	}
	tasks, plans = rescuePass(t, m, 4, live, dropped, rescueRegisters(), rescue)
	if _, plan := renderedByName(t, tasks, plans, "tidy"); !reflect.DeepEqual(plan.TargetSIDs, []string{sidB}) {
		t.Errorf("tidy targets %v, want [b]", plan.TargetSIDs)
	}

	onC := &Rescue{Failed: map[string]map[string]int{sidC: {"deploy": 0}}}
	tasks, plans = rescuePass(t, m, 9, live, dropped, rescueRegisters(), onC)
	if _, plan := renderedByName(t, tasks, plans, "notify"); !reflect.DeepEqual(plan.TargetSIDs, []string{sidC}) {
		t.Errorf("notify targets %v, want [c]: run_once picks the host it fires on, not b, first by SID", plan.TargetSIDs)
	}

	sidD := "d.example.com"
	both := &Rescue{Failed: map[string]map[string]int{sidA: {"deploy": 0}, sidD: {"deploy": 0}}}
	withD := []*topology.HostFacts{host(sidA, []string{"svc", "web"}, nil), host(sidD, []string{"svc"}, nil)}
	tasks, plans = rescuePass(t, m, 12, live, withD, rescueRegisters(), both)
	for _, name := range []string{"undo", "undo4"} {
		if _, plan := renderedByName(t, tasks, plans, name); !reflect.DeepEqual(plan.TargetSIDs, []string{sidA, sidB}) {
			t.Errorf("%s targets %v, want [a b]: its block's on: [web] does not select d", name, plan.TargetSIDs)
		}
	}
	if _, plan := renderedByName(t, tasks, plans, "rollback"); !reflect.DeepEqual(plan.TargetSIDs, []string{sidA, sidB, sidD}) {
		t.Errorf("rollback targets %v, want [a b d]", plan.TargetSIDs)
	}

	roster := make([]*topology.HostFacts, 0, 4)
	roster = append(roster, host(sidB, []string{"svc", "web"}, nil), host(sidC, []string{"svc", "web"}, nil))
	rescuePass(t, m, 2, roster, []*topology.HostFacts{host(sidA, []string{"svc", "web"}, nil)}, rescueRegisters(), rescue)
	if roster[0].SID != sidB || roster[1].SID != sidC {
		t.Errorf("the run's live roster became [%s %s] while rollback was routed, want [b c]", roster[0].SID, roster[1].SID)
	}

	late := &Rescue{Failed: map[string]map[string]int{sidA: {"deploy": 2}}}
	tasks, plans = rescuePass(t, m, 2, live, dropped, rescueRegisters(), late)
	if _, plan := renderedByName(t, tasks, plans, "rollback"); !reflect.DeepEqual(plan.TargetSIDs, []string{sidB}) {
		t.Errorf("rollback targets %v with deploy failing at its own step, want [b]", plan.TargetSIDs)
	}
}

// ★ GUARD (settings.md §4, fork 1 item 4). A failure ended the run at step 0. Past it
// a rescue goes only to the hosts it fires on, its `where:` not evaluated, and
// nothing else renders: `report`, `probe`, the block `gated` and the nested block
// `inner` read a field the failed deploy never produced on host a; `rollback_migrate`
// reads the register of `migrate`, which never ran; `unstage` rescues a sibling that
// stays a placeholder. Rendered, any of them would fail the run before the rescues
// are reached. The step that ended the run renders as it did.
//
// Mutations: render every task after the end → Render fails on register.deploy.stdout;
// evaluate a block's own `where:` past the end → it fails on `gated`, or on `inner`
// for a nested one; render a rescue that reaches no host → it fails on
// register.migrate; keep the live hosts a rescue does not fire on → rollback targets
// b too.
func TestRender_AfterTheRunEndedOnlyRescuesRender(t *testing.T) {
	m := loadStagedManifest(t, rescueScenario)
	live := []*topology.HostFacts{host(sidA, []string{"svc", "web"}, nil), host(sidB, []string{"svc", "web"}, nil)}
	ended := &Rescue{Ended: true, EndedAt: 0, Failed: map[string]map[string]int{sidA: {"deploy": 0}}}

	tasks, plans := rescuePass(t, m, 11, live, nil, rescueRegisters(), ended)
	for _, name := range []string{"report", "tidy", "probe", "wipe", "migrate", "rollback_migrate", "stage", "unstage", "scrub"} {
		if rt, plan := renderedByName(t, tasks, plans, name); rt.Params != nil || len(plan.TargetSIDs) != 0 {
			t.Errorf("%s past the end: params %v, targets %v - want a placeholder", name, rt.Params, plan.TargetSIDs)
		}
	}
	for _, name := range []string{"rollback", "undo", "revert", "notify"} {
		if rt, plan := renderedByName(t, tasks, plans, name); rt.Params == nil || !reflect.DeepEqual(plan.TargetSIDs, []string{sidA}) {
			t.Errorf("rescue %s past the end: params %v, targets %v - want rendered for [a]", name, rt.Params, plan.TargetSIDs)
		}
	}
	if rt, _ := renderedByName(t, tasks, plans, "deploy"); rt.Params == nil {
		t.Error("deploy, the step that ended the run, was not rendered again")
	}
}

const rescueInRunOnceBlock = `
name: linear
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: fix
    run_once: true
    block:
      - name: undo
        onfail: [deploy]
        apply: { destiny: d }
      - name: undo2
        module: core.exec.run
        onfail: [deploy]
        params: { cmd: undo2 }
      - name: after
        module: core.exec.run
        params: { cmd: after }
  - name: fix2
    run_once: true
    block:
      - name: deploy2
        module: core.exec.run
        register: deploy2
        params: { cmd: deploy2 }
      - name: undo3
        module: core.exec.run
        onfail: [deploy2]
        params: { cmd: undo3 }
`

// ★ GUARD (settings.md §4, fork 1). A rescue inside a `run_once:` block reaches the
// dropped host its source failed on: the block's `run_once:` narrows its live hosts,
// not the hosts a rescue may reach. Past the end, a host that moves from live to
// dropped — a rescue failed on it under `host` — changes neither what a rescue
// reaches nor the plan indices of the steps that ran. A dropped host the rescue does
// not reach is not recorded as not reached: a `run_once:` block covers none.
//
// Mutations: build the rescue cover from the block's not-reached dropped hosts, nil
// under `run_once:` → undo3 misses a, and past the end the applier rescue `undo`
// collapses to a placeholder once a is dropped, moving undo2's index; take the
// not-reached hosts from the rescue cover → c is recorded not reached by undo3.
func TestRender_RescueInARunOnceBlockAndStableIndices(t *testing.T) {
	m := loadStagedManifest(t, rescueInRunOnceBlock)
	a, b := host(sidA, []string{"svc"}, nil), host(sidB, []string{"svc"}, nil)
	d := staticResolver{&ResolvedDestiny{Name: "d", Tasks: []config.Task{moduleTask("d1", "core.exec.run"), moduleTask("d2", "core.exec.run")}}}

	c := host(sidC, []string{"svc"}, nil)
	goesOn := &Rescue{Failed: map[string]map[string]int{sidA: {"deploy2": 4}}}
	tasks, plans := rescuePass(t, m, 5, []*topology.HostFacts{b}, []*topology.HostFacts{a, c}, rescueRegisters(), goesOn, d)
	if _, plan := renderedByName(t, tasks, plans, "undo3"); !reflect.DeepEqual(plan.TargetSIDs, []string{sidA, sidB}) || len(plan.NotReached) != 0 {
		t.Errorf("undo3 targets %v, not reached %v - want [a b] and none: a failed deploy2, and a run_once block covers no dropped host it does not reach", plan.TargetSIDs, plan.NotReached)
	}

	ended := &Rescue{Ended: true, EndedAt: 0, Failed: map[string]map[string]int{sidA: {"deploy": 0}}}
	indices := func(tasks []*RenderedTask) map[string][]int {
		out := map[string][]int{}
		for _, rt := range tasks {
			out[rt.Name] = append(out[rt.Name], rt.Index)
		}
		return out
	}
	before, _ := rescuePass(t, m, 2, []*topology.HostFacts{a, b}, nil, rescueRegisters(), ended, d)
	after, plans := rescuePass(t, m, 3, []*topology.HostFacts{b}, []*topology.HostFacts{a}, rescueRegisters(), ended, d)
	for _, name := range []string{"deploy", "d1", "d2", "undo", "undo2"} {
		if got, want := indices(after)[name], indices(before)[name]; !reflect.DeepEqual(got, want) {
			t.Errorf("%s plan indices moved from %v to %v after host a was dropped past the end", name, want, got)
		}
	}
	if _, plan := renderedByName(t, after, plans, "undo2"); !reflect.DeepEqual(plan.TargetSIDs, []string{sidA}) {
		t.Errorf("undo2 targets %v after a was dropped, want [a]", plan.TargetSIDs)
	}
}
