package render

import (
	"context"
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Render's half of rescue routing (docs/scenario/rescues.md, NIM-951): a dropped
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

// ★ GUARD (rescues.md; ADR-0091 amendment NIM-993, rule 1). `deploy` failed on
// host a, which was dropped under `on_failure: host`; host c was dropped at another
// task. A rescue of an earlier step's source is routed to a and settled SKIPPED,
// unrendered, on the live host b; c stays not reached. An ordinary task reaches no
// dropped host; a rescue inside a block reaches a when the block's `on:` selects it —
// at the top level and nested — and never d, outside it. A source failing at or after
// the rescue's step does not route it. A `run_once:` rescue is routed to the first
// host by SID it fires on, c, its `where:` not evaluated there, and is still settled
// SKIPPED on b: `run_once:` exempts no live host.
//
// Mutations: filter a rescue by the live roster only → a not targeted; route a
// dropped host without the source's failure → c targeted; read a failure of the
// rescue's own step → a targeted when deploy failed at step 2; give a live host no
// skip target in routeUnit → rollback and undo target a alone, notify c alone; let
// `run_once:` exempt the live hosts → notify targets c alone and b gets no flow; leave
// a block's `on:` out of the rescue cover, top-level or nested → undo or undo4
// reaches d.
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
	notify, plan := renderedByName(t, tasks, plans, "notify")
	if !reflect.DeepEqual(plan.TargetSIDs, []string{sidB, sidC}) {
		t.Errorf("notify targets %v, want [b c]: routed to c, the one host it fires on, and settled SKIPPED on the live b", plan.TargetSIDs)
	}
	if got := paramsForHost(notify, sidC).GetFields()["cmd"].GetStringValue(); got != "notify" || notify.FlowBySID[sidC].Skip {
		t.Errorf("notify on c: cmd %q, flow %+v - want it rendered and not skipped", got, notify.FlowBySID[sidC])
	}
	_, paramsOnB := notify.ParamsBySID[sidB]
	_, contextOnB := notify.RenderContextBySID[sidB]
	if notify.FlowBySID[sidB] != unrenderedSkip || paramsOnB || contextOnB {
		t.Errorf("notify on live b: flow %+v, per-host params %v, render context %v - want an unrendered skip, nothing rendered for b", notify.FlowBySID[sidB], paramsOnB, contextOnB)
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

	late := &Rescue{Failed: map[string]map[string]int{sidA: {"deploy": 2}}}
	tasks, plans = rescuePass(t, m, 2, live, dropped, rescueRegisters(), late)
	if _, plan := renderedByName(t, tasks, plans, "rollback"); !reflect.DeepEqual(plan.TargetSIDs, []string{sidB}) {
		t.Errorf("rollback targets %v with deploy failing at its own step, want [b]", plan.TargetSIDs)
	}
}

const mixedRescueScenario = `
name: linear
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: mend
    module: core.exec.run
    onfail: [deploy, verify]
    params: { cmd: mend }
  - name: page
    module: core.exec.run
    onfail: [deploy, verify]
    run_once: true
    params: { cmd: page }
  - name: verify
    module: core.exec.run
    register: verify
    params: { cmd: verify }
`

// ★ GUARD (ADR-0091 amendment NIM-993, rule 1, "Units with no source before them in
// their own message"). A rescue that also names a later step's source has no source in
// its own message, so it is a cross-step unit like one of earlier sources only: routed to
// the dropped host it fires on, rendered there, and settled SKIPPED, unrendered, on every
// live host; the dropped host it does not fire on stays not reached. Under `run_once:`
// it goes to the first host by SID it fires on, and the live host is still settled
// SKIPPED.
//
// Mutation: take a unit by its sources at earlier steps only (config.CrossStepUnits) →
// mend and page are rendered for the live hosts.
func TestRender_AMixedSourceUnitIsACrossStepUnit(t *testing.T) {
	m := loadStagedManifest(t, mixedRescueScenario)
	b, c := host(sidB, []string{"svc"}, nil), host(sidC, []string{"svc"}, nil)
	a, d := host(sidA, []string{"svc"}, nil), host("d.example.com", []string{"svc"}, nil)
	onA := &Rescue{Failed: map[string]map[string]int{sidA: {"deploy": 0}}}

	tasks, plans := rescuePass(t, m, 1, []*topology.HostFacts{b, c}, []*topology.HostFacts{a, d}, rescueRegisters(), onA)
	mend, plan := renderedByName(t, tasks, plans, "mend")
	if !reflect.DeepEqual(plan.TargetSIDs, []string{sidA, sidB, sidC}) || !reflect.DeepEqual(plan.NotReached, []string{"d.example.com"}) {
		t.Errorf("mend targets %v, not reached %v - want [a b c], [d]", plan.TargetSIDs, plan.NotReached)
	}
	if mend.FlowBySID[sidA].Skip || mend.FlowBySID[sidB] != unrenderedSkip || mend.FlowBySID[sidC] != unrenderedSkip {
		t.Errorf("mend flows %+v, want a run, b and c unrendered skips", mend.FlowBySID)
	}

	onC := &Rescue{Failed: map[string]map[string]int{sidC: {"deploy": 0}}}
	tasks, plans = rescuePass(t, m, 2, []*topology.HostFacts{b}, []*topology.HostFacts{a, c}, rescueRegisters(), onC)
	page, plan := renderedByName(t, tasks, plans, "page")
	if !reflect.DeepEqual(plan.TargetSIDs, []string{sidB, sidC}) || page.FlowBySID[sidB] != unrenderedSkip || page.FlowBySID[sidC].Skip {
		t.Errorf("page targets %v, flows %+v - want [b c], b an unrendered skip and c run", plan.TargetSIDs, page.FlowBySID)
	}
}

const groupedRescueScenario = `
name: linear
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: pair
    serial: 4
    settings: { serial_scope: block }
    block:
      - name: stage
        module: core.exec.run
        register: stage
        params: { cmd: stage }
      - name: mend
        module: core.exec.run
        onfail: [stage, deploy]
        params: { cmd: mend }
  - name: once
    serial: 4
    settings: { serial_scope: block }
    block:
      - name: probe
        module: core.exec.run
        register: probe
        params: { cmd: probe }
      - name: page
        module: core.exec.run
        onfail: [probe, deploy]
        run_once: true
        params: { cmd: page }
`

// ★ GUARD (rescues.md). A rescue with a source before it in its own group can fire on a
// live host, inside the message, so it is no cross-step unit and is routed as NIM-951
// routes it. Its live hosts are joined by the dropped hosts it fires on, and the dropped
// host it does not fire on stays not reached; routing it leaves the run's live roster as
// it was; under `run_once:` it goes to the first host by SID it fires on, not the first
// live one.
//
// Mutations: drop the join of the dropped hosts in routeRescue → mend misses a; append
// them to the slice resolveTargets returns (no slices.Clone) → the run's roster, with
// room to spare, becomes [a b]; pick a `run_once:` rescue's host by SID alone → page
// goes to b, where it does not fire; take every outermost unit for a cross-step one
// (config.CrossStepUnits not reading where its sources are set) → mend is settled
// SKIPPED, unrendered, on b and c.
func TestRender_AnInMessageRescueIsRoutedOverItsLiveHosts(t *testing.T) {
	m := loadStagedManifest(t, groupedRescueScenario)
	roster := make([]*topology.HostFacts, 0, 4)
	roster = append(roster, host(sidB, []string{"svc"}, nil), host(sidC, []string{"svc"}, nil))
	a, d := host(sidA, []string{"svc"}, nil), host("d.example.com", []string{"svc"}, nil)
	onA := &Rescue{Failed: map[string]map[string]int{sidA: {"deploy": 0}}}

	tasks, plans := rescuePass(t, m, 1, roster, []*topology.HostFacts{a, d}, rescueRegisters(), onA)
	mend, plan := renderedByName(t, tasks, plans, "mend")
	if !reflect.DeepEqual(plan.TargetSIDs, []string{sidA, sidB, sidC}) || !reflect.DeepEqual(plan.NotReached, []string{"d.example.com"}) {
		t.Errorf("mend targets %v, not reached %v - want [a b c], [d]", plan.TargetSIDs, plan.NotReached)
	}
	if mend.FlowBySID[sidB].Unrendered || mend.FlowBySID[sidC].Unrendered {
		t.Errorf("mend flows %+v, want it rendered for the live b and c", mend.FlowBySID)
	}
	if roster[0].SID != sidB || roster[1].SID != sidC {
		t.Errorf("the run's live roster became [%s %s] while mend was routed, want [b c]", roster[0].SID, roster[1].SID)
	}

	onC := &Rescue{Failed: map[string]map[string]int{sidC: {"deploy": 0}}}
	tasks, plans = rescuePass(t, m, 2, roster[:1], []*topology.HostFacts{a, roster[1]}, rescueRegisters(), onC)
	if _, plan := renderedByName(t, tasks, plans, "page"); !reflect.DeepEqual(plan.TargetSIDs, []string{sidC}) {
		t.Errorf("page targets %v, want [c]: run_once picks the host it fires on, not b, first by SID", plan.TargetSIDs)
	}
}

// ★ GUARD (rescues.md, fork 1 item 4). A failure ended the run at step 0. Past it
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

// ★ GUARD (rescues.md; ADR-0091 amendment NIM-993, rule 1). A rescue inside a
// `run_once:` block reaches the dropped host its source failed on: its cover is the
// `on:` chain, and an enclosing block's `where:` and `run_once:` are not read. Past the
// end, a host that moves from live to dropped — a rescue failed on it under `host` —
// changes neither what a rescue reaches nor the plan indices of the steps that ran. A
// dropped host the rescue does not reach is not recorded as not reached: a `run_once:`
// block covers none.
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

// ★ GUARD (ADR-0091 amendment NIM-993, "What the run goroutine keeps"). A source
// fires a rescue at a step when it failed at an earlier step and was not handled at
// an earlier one: handled at or after a step, it still fires there on every later
// pass, and a later unit is skipped. A nil Rescue fires nothing.
//
// Mutations: read a source handled at the rescue's own step as handled (by > step
// instead of by >= step) → step 2 stops firing; drop the HandledAt check → step 3
// fires; drop the nil guard → FiresAnywhere panics.
func TestRescue_AHandledSourceFiresUpToTheStepThatHandledIt(t *testing.T) {
	r := &Rescue{
		Failed:    map[string]map[string]int{sidA: {"deploy": 0, "migrate": 1}},
		HandledAt: map[string]map[string]int{sidA: {"deploy": 2}},
	}
	for _, tc := range []struct {
		names []string
		step  int
		want  bool
	}{
		{[]string{"deploy"}, 1, true},
		{[]string{"deploy"}, 2, true},
		{[]string{"deploy"}, 3, false},
		{[]string{"deploy", "migrate"}, 3, true},
	} {
		if got := r.fires(sidA, tc.names, tc.step); got != tc.want {
			t.Errorf("fires(a, %v, %d) = %v, want %v", tc.names, tc.step, got, tc.want)
		}
	}
	if !r.FiresAnywhere([]string{"deploy"}, 2) || r.FiresAnywhere([]string{"deploy"}, 3) {
		t.Error("FiresAnywhere(deploy) should fire at step 2 and not at step 3")
	}
	var none *Rescue
	if none.fires(sidA, []string{"deploy"}, 3) || none.FiresAnywhere([]string{"deploy"}, 3) {
		t.Error("a nil Rescue fired")
	}

	m := loadStagedManifest(t, rescueScenario)
	live := []*topology.HostFacts{host(sidB, []string{"svc", "web"}, nil)}
	dropped := []*topology.HostFacts{host(sidA, []string{"svc", "web"}, nil)}
	for handledAt, want := range map[int][]string{1: {sidB}, 2: {sidA, sidB}} {
		rescue := &Rescue{Failed: map[string]map[string]int{sidA: {"deploy": 0}}, HandledAt: map[string]map[string]int{sidA: {"deploy": handledAt}}}
		tasks, plans := rescuePass(t, m, 2, live, dropped, rescueRegisters(), rescue)
		if _, plan := renderedByName(t, tasks, plans, "rollback"); !reflect.DeepEqual(plan.TargetSIDs, want) {
			t.Errorf("deploy handled at step %d: rollback (step 2) targets %v, want %v", handledAt, plan.TargetSIDs, want)
		}
	}
}

const heldShareScenario = `
name: linear
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: pair
    serial: 4
    settings: { serial_scope: block }
    block:
      - name: stage
        module: core.exec.run
        register: stage
        params: { cmd: stage }
      - name: mend
        module: core.exec.run
        onfail: [stage, deploy]
        params: { cmd: mend }
  - name: web only
    on: [web]
    block:
      - name: count
        module: core.exec.run
        where: "soulprint.hosts.filter(h, h.held).size() == 1"
        params: { cmd: count }
`

// ★ GUARD (ADR-0091 amendment NIM-993, rules 2-3). A held host is no live host, and
// render treats it as a dropped one: a rescue with a source in its own group reaches it
// where a source of it fires there, its `where:` not evaluated; an ordinary task names
// it not reached, so the Keeper records the task SKIPPED for it; and inside a block a
// child's `where:` reads the block's share of the held hosts, those the block's `on:`
// selects.
//
// Mutations: route a rescue to the dropped hosts alone (droppedSIDs for notLiveSIDs in
// walkBlockChildren) → mend misses a; leave the held hosts out of a rescue's not
// reached (routeRescue over in.Dropped) → d is not named; leave a block's child the
// run's held hosts (no scoped.HeldHosts in blockChildTargets) → count reads two held
// hosts and reaches nobody.
func TestRender_AHeldHostIsReachedAsADroppedOne(t *testing.T) {
	m := loadStagedManifest(t, heldShareScenario)
	b, c := host(sidB, []string{"svc", "web"}, nil), host(sidC, []string{"svc", "web"}, nil)
	a, d := host(sidA, []string{"svc", "web"}, nil), host("d.example.com", []string{"svc"}, nil)
	onA := &Rescue{Failed: map[string]map[string]int{sidA: {"deploy": 0}}}
	pass := func(active int) ([]*RenderedTask, []DispatchPlan) {
		t.Helper()
		plan, steps := config.LinearPassage(m.Tasks, m.Settings)
		in := RenderInput{
			Scenario: m, Input: map[string]any{}, Incarnation: IncarnationMeta{ID: "svc"},
			Hosts: []*topology.HostFacts{b, c}, HeldHosts: []*topology.HostFacts{a, d},
			TaskPassage: plan.TaskPassage, ActivePassage: active, Steps: steps, HeldTargets: NewHeldTargets(),
			RegisterByHost: rescueRegisters(), Rescue: onA,
		}
		tasks, plans, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), in)
		if err != nil {
			t.Fatalf("Render at step %d: %v", active, err)
		}
		return tasks, plans
	}

	tasks, plans := pass(1)
	if _, plan := renderedByName(t, tasks, plans, "mend"); !reflect.DeepEqual(plan.TargetSIDs, []string{sidA, sidB, sidC}) || !reflect.DeepEqual(plan.NotReached, []string{"d.example.com"}) {
		t.Errorf("mend targets %v, not reached %v - want [a b c], [d]", plan.TargetSIDs, plan.NotReached)
	}
	tasks, plans = pass(2)
	if _, plan := renderedByName(t, tasks, plans, "count"); !reflect.DeepEqual(plan.TargetSIDs, []string{sidB, sidC}) || !reflect.DeepEqual(plan.NotReached, []string{sidA}) {
		t.Errorf("count targets %v, not reached %v - want [b c] reading the block's one held host, and [a]", plan.TargetSIDs, plan.NotReached)
	}
}

const heldRunOnceScenario = `
name: linear
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: once
    run_once: true
    block:
      - name: c1
        module: core.exec.run
        register: c1
        params: { cmd: c1 }
  - name: rollback
    module: core.exec.run
    onfail: [deploy]
    params: { cmd: rollback }
`

// ★ GUARD (ADR-0091 amendment NIM-993, rule 3, decision on `run_once:`). Under a
// `run_once:` block a held host gets what a dropped host gets there: nothing — the
// block reaches one live host, so it covers no host that is not live, and its child
// names none not reached, so no SKIPPED is recorded and no register is synthesized.
//
// Mutation: let a `run_once:` task cover the hosts that are not live (no RunOnce
// check in coveredDropped) → c1 names a and d not reached.
func TestRender_AHeldHostUnderARunOnceBlockGetsNothing(t *testing.T) {
	m := loadStagedManifest(t, heldRunOnceScenario)
	plan, steps := config.LinearPassage(m.Tasks, m.Settings)
	in := RenderInput{
		Scenario: m, Input: map[string]any{}, Incarnation: IncarnationMeta{ID: "svc"},
		Hosts:       []*topology.HostFacts{host(sidB, []string{"svc"}, nil), host(sidC, []string{"svc"}, nil)},
		HeldHosts:   []*topology.HostFacts{host(sidA, []string{"svc"}, nil)},
		Dropped:     []*topology.HostFacts{host("d.example.com", []string{"svc"}, nil)},
		TaskPassage: plan.TaskPassage, ActivePassage: 1, Steps: steps, HeldTargets: NewHeldTargets(),
		RegisterByHost: rescueRegisters(), Rescue: &Rescue{Failed: map[string]map[string]int{sidA: {"deploy": 0}}},
	}
	tasks, plans, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if _, plan := renderedByName(t, tasks, plans, "c1"); !reflect.DeepEqual(plan.TargetSIDs, []string{sidB}) || len(plan.NotReached) != 0 {
		t.Errorf("c1 targets %v, not reached %v - want [b] and nobody", plan.TargetSIDs, plan.NotReached)
	}
}

const failedUnitScenario = `
name: linear
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: roll back
    onfail: [deploy]
    block:
      - name: r1
        module: core.exec.run
        register: r1
        params: { cmd: r1 }
      - name: r2
        module: core.exec.run
        params: { cmd: "${ register.r1.stdout }" }
`

// ★ GUARD (ADR-0091 amendment NIM-993, rule 1). A cross-step unit's task is routed to
// no host on which an earlier task of its unit failed, and nothing of it is evaluated
// for that host: `r1` failed on the held host a, so `r2`, whose params read the output
// `r1` never produced there, names a not reached and renders. On the held host c,
// where the unit did not fail, `r2` goes as before. A unit failure at the task's own
// step is not read, so a step renders the same on every pass.
//
// Mutations: route a unit's task whatever its unit did (no unitFailed case in
// routeUnit) → the render fails on `register.r1.stdout` for a; read a failure at the
// task's own step (Rescue.unitFailed with at <= step) → a is not reached in the second
// case.
func TestRender_AUnitTaskSkipsTheHostItsUnitFailedOn(t *testing.T) {
	m := loadStagedManifest(t, failedUnitScenario)
	plan, steps := config.LinearPassage(m.Tasks, m.Settings)
	ok := map[string]any{"changed": true, "failed": false, "stdout": "ok"}
	broke := map[string]any{"changed": false, "failed": true}
	for _, tc := range []struct {
		name                string
		failedAt            int
		r1OnA               map[string]any
		targets, notReached []string
	}{
		{name: "the unit failed at an earlier step", failedAt: 1, r1OnA: broke, targets: []string{sidB, sidC}, notReached: []string{sidA}},
		{name: "a unit failure of the task's own step", failedAt: 2, r1OnA: ok, targets: []string{sidA, sidB, sidC}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := RenderInput{
				Scenario: m, Input: map[string]any{}, Incarnation: IncarnationMeta{ID: "svc"},
				Hosts:       []*topology.HostFacts{host(sidB, []string{"svc"}, nil)},
				HeldHosts:   []*topology.HostFacts{host(sidA, []string{"svc"}, nil), host(sidC, []string{"svc"}, nil)},
				TaskPassage: plan.TaskPassage, ActivePassage: 2, Steps: steps, HeldTargets: NewHeldTargets(),
				RegisterByHost: map[string]map[string]any{
					sidA: {"deploy": broke, "r1": tc.r1OnA},
					sidB: {"deploy": ok},
					sidC: {"deploy": broke, "r1": ok},
				},
				Rescue: &Rescue{
					Failed:      map[string]map[string]int{sidA: {"deploy": 0}, sidC: {"deploy": 0}},
					FailedUnits: map[string]map[string]int{sidA: {"1": tc.failedAt}},
				},
			}
			tasks, plans, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), in)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			_, r2 := renderedByName(t, tasks, plans, "r2")
			if !reflect.DeepEqual(r2.TargetSIDs, tc.targets) || !reflect.DeepEqual(r2.NotReached, tc.notReached) {
				t.Errorf("r2 targets %v, not reached %v - want %v, %v", r2.TargetSIDs, r2.NotReached, tc.targets, tc.notReached)
			}
		})
	}
}

const failedMixedUnitScenario = `
name: linear
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: roll back
    onfail: [deploy, verify]
    block:
      - name: r1
        module: core.exec.run
        register: r1
        params: { cmd: r1 }
      - name: r2
        module: core.exec.run
        params: { cmd: "${ register.r1.stdout }" }
  - name: verify
    module: core.exec.run
    register: verify
    params: { cmd: verify }
`

// ★ GUARD (ADR-0091 amendment NIM-993, rule 1). Past the end of the run, a unit's task
// whose unit failed on every host its sources fire on is routed to none and stays a
// placeholder, unrendered; its plan still names those hosts not reached, so each is
// recorded SKIPPED, as it is where the task is routed to another host. That holds for
// a unit of earlier sources only and for one that also names a later step's source:
// both are cross-step units.
//
// Mutations: leave routeUnit's notReached off the placeholder (no stampNotReached in
// unitStaticWhen's past-end branch) → neither r2 names a host not reached; route a
// unit's task whatever its unit did (no unitFailed case in routeUnit) → both r2's
// params are rendered for a, and Render fails on `register.r1.stdout`.
func TestRender_PastTheEndAUnitPlaceholderNamesTheHostItsUnitFailedOn(t *testing.T) {
	broke := map[string]any{"changed": false, "failed": true}
	for _, tc := range []struct{ name, scenario string }{
		{"a cross-step unit", failedUnitScenario},
		{"a unit naming a later source too", failedMixedUnitScenario},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := loadStagedManifest(t, tc.scenario)
			plan, steps := config.LinearPassage(m.Tasks, m.Settings)
			in := RenderInput{
				Scenario: m, Input: map[string]any{}, Incarnation: IncarnationMeta{ID: "svc"},
				Hosts:       []*topology.HostFacts{host(sidA, []string{"svc"}, nil), host(sidB, []string{"svc"}, nil)},
				TaskPassage: plan.TaskPassage, ActivePassage: 2, Steps: steps, HeldTargets: NewHeldTargets(),
				RegisterByHost: map[string]map[string]any{
					sidA: {"deploy": broke, "r1": broke},
					sidB: {"deploy": map[string]any{"changed": true, "failed": false}},
				},
				Rescue: &Rescue{
					Ended: true, EndedAt: 0,
					Failed:      map[string]map[string]int{sidA: {"deploy": 0}},
					FailedUnits: map[string]map[string]int{sidA: {"1": 1}},
				},
			}
			tasks, plans, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), in)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			rt, r2 := renderedByName(t, tasks, plans, "r2")
			if rt.Params != nil || len(r2.TargetSIDs) != 0 || !reflect.DeepEqual(r2.NotReached, []string{sidA}) {
				t.Errorf("r2: params %v, targets %v, not reached %v - want a placeholder naming [a] not reached", rt.Params, r2.TargetSIDs, r2.NotReached)
			}
		})
	}
}
