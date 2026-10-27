package render

import (
	"context"
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// A linear run's hosts as each step saw them (ADR-0091 amendment NIM-993, rules 2-3).

func stepAt(n int) *int { return &n }

func skippedRegister() map[string]any {
	return map[string]any{"changed": false, "failed": false, "timed_out": false, "skipped": true}
}

func sidsOfHosts(hosts []*topology.HostFacts) []string {
	out := []string{}
	for _, h := range hosts {
		out = append(out, h.SID)
	}
	return out
}

// standingRun: host a is held from step 1 and back at step 3, having missed reload;
// host c is dropped from step 2.
func standingRun() (*Standing, map[string]map[string]any) {
	roster := []*topology.HostFacts{host(sidA, []string{"svc"}, nil), host(sidB, []string{"svc"}, nil), host(sidC, []string{"svc"}, nil)}
	return &Standing{
			Roster: roster,
			BySID: map[string]HostStanding{
				sidA: {Periods: []HoldPeriod{{HeldFrom: 1, ReturnedAt: stepAt(3), Synth: map[string]any{"reload": skippedRegister()}}}},
				sidC: {DroppedAt: stepAt(2)},
			},
		}, map[string]map[string]any{
			sidA: {"deploy": map[string]any{"failed": true}},
			sidB: {"deploy": map[string]any{"failed": false}, "reload": map[string]any{"skipped": false}},
		}
}

// ★ GUARD (ADR-0091 amendment NIM-993, "a step reads them as they stood when that
// step was dispatched"). Each step sees its own live, held and dropped hosts: a host
// is live before it is held, held until it returns, and live again after; a dropped
// host stays dropped. A returned host's synthesized registers appear from its return
// on, in register.<name> and register.hosts.<name> alike, beneath its own, and
// nothing the caller passed is written. ForStep over its own result starts from the
// caller's registers. nil Standing changes nothing.
//
// Mutations: lay the synthesized registers before the return (drop the ReturnedAt
// check) → a reads reload at step 0; ignore ReturnedAt in the classification → a
// held at 3;
// write the overlay into the caller's map → the base gains reload; overlay onto the
// current RegisterByHost instead of the base → step 1 after step 3 keeps reload.
func TestForStep_HostsAndRegistersAsEachStepSawThem(t *testing.T) {
	standing, reg := standingRun()
	in := RenderInput{Standing: standing, RegisterByHost: reg}
	for _, tc := range []struct {
		step                int
		live, held, dropped []string
		synthesized         bool
	}{
		{0, []string{sidA, sidB, sidC}, []string{}, []string{}, false},
		{1, []string{sidB, sidC}, []string{sidA}, []string{}, false},
		{2, []string{sidB}, []string{sidA}, []string{sidC}, false},
		{3, []string{sidA, sidB}, []string{}, []string{sidC}, true},
		{7, []string{sidA, sidB}, []string{}, []string{sidC}, true},
	} {
		view := in.ForStep(tc.step)
		if got := sidsOfHosts(view.Hosts); !reflect.DeepEqual(got, tc.live) {
			t.Errorf("step %d: live = %v, want %v", tc.step, got, tc.live)
		}
		if got := sidsOfHosts(view.HeldHosts); !reflect.DeepEqual(got, tc.held) {
			t.Errorf("step %d: held = %v, want %v", tc.step, got, tc.held)
		}
		if got := sidsOfHosts(view.Dropped); !reflect.DeepEqual(got, tc.dropped) {
			t.Errorf("step %d: dropped = %v, want %v", tc.step, got, tc.dropped)
		}
		_, own := hostRegister(view, standing.Roster[0])["reload"]
		_, across := registerHosts(view)["reload"].(map[string]any)[sidA]
		if own != tc.synthesized || across != tc.synthesized {
			t.Errorf("step %d: a's reload in register = %v, in register.hosts = %v, want %v", tc.step, own, across, tc.synthesized)
		}
		if got := hostRegister(view, standing.Roster[0])["deploy"]; !reflect.DeepEqual(got, map[string]any{"failed": true}) {
			t.Errorf("step %d: a's own deploy = %v, want it kept", tc.step, got)
		}
	}
	if _, wrote := reg[sidA]["reload"]; wrote {
		t.Error("ForStep wrote the synthesized register into the caller's map")
	}
	if _, kept := in.ForStep(3).ForStep(1).RegisterByHost[sidA]["reload"]; kept {
		t.Error("ForStep(1) over ForStep(3) kept a register synthesized from step 3")
	}

	bare := RenderInput{Hosts: standing.Roster[:2], Dropped: standing.Roster[2:], RegisterByHost: reg}
	if got := bare.ForStep(2); !reflect.DeepEqual(got, bare) {
		t.Errorf("ForStep without Standing changed the input: %+v", got)
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, rules 2-3: a returned host can be held
// again). Each step reads the hold period it falls in: a held from 1, back at 3,
// held again from 5, back at 7; c held from 2, back at 3, held again from 4 and
// dropped at 6 while still held. A period's synthesized registers appear from its own
// return on and stay through a later hold or drop; a period returned after the step
// shows none, on every pass.
//
// Mutations: read only the last period → a live at 1 and without r1 at 3; lay every
// period's registers once the first has returned → a reads r2 at 3; let a period's
// hold win over the drop → c held at 6; lay the registers on live hosts only → a
// without r1 at 5.
func TestForStep_EachHoldPeriodAtItsOwnSteps(t *testing.T) {
	roster := []*topology.HostFacts{host(sidA, []string{"svc"}, nil), host(sidB, []string{"svc"}, nil), host(sidC, []string{"svc"}, nil)}
	synth := func(name string) map[string]any { return map[string]any{name: skippedRegister()} }
	standing := &Standing{Roster: roster, BySID: map[string]HostStanding{
		sidA: {Periods: []HoldPeriod{
			{HeldFrom: 1, ReturnedAt: stepAt(3), Synth: synth("r1")},
			{HeldFrom: 5, ReturnedAt: stepAt(7), Synth: synth("r2")},
		}},
		sidC: {Periods: []HoldPeriod{
			{HeldFrom: 2, ReturnedAt: stepAt(3), Synth: synth("c1")},
			{HeldFrom: 4, Synth: synth("c2")},
		}, DroppedAt: stepAt(6)},
	}}
	reg := map[string]map[string]any{sidA: {"deploy": map[string]any{"failed": true}}}
	in := RenderInput{Standing: standing, RegisterByHost: reg}
	synthesized := func(view RenderInput, sid string) []string {
		out := []string{}
		for _, name := range []string{"r1", "r2", "c1", "c2"} {
			if _, ok := view.RegisterByHost[sid][name]; ok {
				out = append(out, name)
			}
		}
		return out
	}
	for _, tc := range []struct {
		step                int
		live, held, dropped []string
		a, c                []string
	}{
		{0, []string{sidA, sidB, sidC}, []string{}, []string{}, []string{}, []string{}},
		{1, []string{sidB, sidC}, []string{sidA}, []string{}, []string{}, []string{}},
		{2, []string{sidB}, []string{sidA, sidC}, []string{}, []string{}, []string{}},
		{3, []string{sidA, sidB, sidC}, []string{}, []string{}, []string{"r1"}, []string{"c1"}},
		{4, []string{sidA, sidB}, []string{sidC}, []string{}, []string{"r1"}, []string{"c1"}},
		{5, []string{sidB}, []string{sidA, sidC}, []string{}, []string{"r1"}, []string{"c1"}},
		{6, []string{sidB}, []string{sidA}, []string{sidC}, []string{"r1"}, []string{"c1"}},
		{7, []string{sidA, sidB}, []string{}, []string{sidC}, []string{"r1", "r2"}, []string{"c1"}},
		{9, []string{sidA, sidB}, []string{}, []string{sidC}, []string{"r1", "r2"}, []string{"c1"}},
	} {
		view := in.ForStep(tc.step)
		if got := sidsOfHosts(view.Hosts); !reflect.DeepEqual(got, tc.live) {
			t.Errorf("step %d: live = %v, want %v", tc.step, got, tc.live)
		}
		if got := sidsOfHosts(view.HeldHosts); !reflect.DeepEqual(got, tc.held) {
			t.Errorf("step %d: held = %v, want %v", tc.step, got, tc.held)
		}
		if got := sidsOfHosts(view.Dropped); !reflect.DeepEqual(got, tc.dropped) {
			t.Errorf("step %d: dropped = %v, want %v", tc.step, got, tc.dropped)
		}
		if got := synthesized(view, sidA); !reflect.DeepEqual(got, tc.a) {
			t.Errorf("step %d: a's synthesized registers = %v, want %v", tc.step, got, tc.a)
		}
		if got := synthesized(view, sidC); !reflect.DeepEqual(got, tc.c) {
			t.Errorf("step %d: c's synthesized registers = %v, want %v", tc.step, got, tc.c)
		}
		if got := view.RegisterByHost[sidA]["deploy"]; !reflect.DeepEqual(got, map[string]any{"failed": true}) {
			t.Errorf("step %d: a's own deploy = %v, want it kept", tc.step, got)
		}
	}
	if got := synthesized(in.ForStep(7).ForStep(3), sidA); !reflect.DeepEqual(got, []string{"r1"}) {
		t.Errorf("ForStep(3) over ForStep(7): a's synthesized registers = %v, want [r1]", got)
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, "What the run goroutine keeps": a drop cannot
// return the host). Only a return lays a period's registers. a returns at 3, is held
// again from 4, dropped at 6 and would have returned at 7; b is dropped at the step it
// would have returned at; c's first period is still open when its second returns at
// 5, input Periods never holds, so that case guards the defensive stop. Neither a
// return at or after the drop nor one past an open period lays anything.
//
// Mutations: drop the DroppedAt check in synthAt → a reads a2 at 7, b reads b1 at 4;
// drop the stop at an open period → c reads c2 at 5.
func TestForStep_OnlyAReturnLaysAPeriodsRegisters(t *testing.T) {
	roster := []*topology.HostFacts{host(sidA, []string{"svc"}, nil), host(sidB, []string{"svc"}, nil), host(sidC, []string{"svc"}, nil)}
	synth := func(name string) map[string]any { return map[string]any{name: skippedRegister()} }
	in := RenderInput{Standing: &Standing{Roster: roster, BySID: map[string]HostStanding{
		sidA: {Periods: []HoldPeriod{
			{HeldFrom: 1, ReturnedAt: stepAt(3), Synth: synth("a1")},
			{HeldFrom: 4, ReturnedAt: stepAt(7), Synth: synth("a2")},
		}, DroppedAt: stepAt(6)},
		sidB: {Periods: []HoldPeriod{{HeldFrom: 1, ReturnedAt: stepAt(4), Synth: synth("b1")}}, DroppedAt: stepAt(4)},
		sidC: {Periods: []HoldPeriod{
			{HeldFrom: 1, Synth: synth("c1")},
			{HeldFrom: 3, ReturnedAt: stepAt(5), Synth: synth("c2")},
		}},
	}}}
	for _, tc := range []struct {
		step    int
		a, b, c []string
	}{
		{3, []string{"a1"}, nil, nil},
		{4, []string{"a1"}, nil, nil},
		{5, []string{"a1"}, nil, nil},
		{7, []string{"a1"}, nil, nil},
		{9, []string{"a1"}, nil, nil},
	} {
		view := in.ForStep(tc.step)
		for sid, want := range map[string][]string{sidA: tc.a, sidB: tc.b, sidC: tc.c} {
			var got []string
			for _, name := range []string{"a1", "a2", "b1", "c1", "c2"} {
				if _, ok := view.RegisterByHost[sid][name]; ok {
					got = append(got, name)
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("step %d: %s's synthesized registers = %v, want %v", tc.step, sid, got, want)
			}
		}
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 2). Every soulprint.hosts entry carries
// `held`, false by default; a held host is listed in SID order with `held: true` and
// `dropped: false`.
//
// Mutations: leave HeldHosts out of runHosts → two entries; set `held` from the
// dropped set → a reads false, c true; drop the key → absent.
func TestSoulprintHosts_HeldKey(t *testing.T) {
	a, b, c := host(sidA, nil, nil), host(sidB, nil, nil), host(sidC, nil, nil)
	got := soulprintHosts(RenderInput{Hosts: []*topology.HostFacts{b}, HeldHosts: []*topology.HostFacts{a}, Dropped: []*topology.HostFacts{c}})
	want := map[string][2]bool{sidA: {true, false}, sidB: {false, false}, sidC: {false, true}}
	if len(got) != 3 {
		t.Fatalf("soulprint.hosts = %v, want three hosts", got)
	}
	for i, sid := range []string{sidA, sidB, sidC} {
		if got[i]["sid"] != sid || got[i]["held"] != want[sid][0] || got[i]["dropped"] != want[sid][1] {
			t.Errorf("soulprint.hosts[%d] = sid %v held %v dropped %v, want %s held %v dropped %v",
				i, got[i]["sid"], got[i]["held"], got[i]["dropped"], sid, want[sid][0], want[sid][1])
		}
	}
	for _, entry := range soulprintHosts(RenderInput{Hosts: []*topology.HostFacts{a, b}}) {
		if held, ok := entry["held"]; !ok || held != false {
			t.Errorf("%v: held = %v (present %v), want false", entry["sid"], held, ok)
		}
	}
}

const standingScenario = `
name: linear
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: reload
    module: core.exec.run
    register: reload
    params: { cmd: reload }
  - name: roll
    block:
      - name: one
        module: core.exec.run
        params:
          cmd: one
          args:
            - "${ string(size(soulprint.hosts)) }"
            - "${ string(size(soulprint.hosts.filter(h, h.held))) }"
            - "${ string(size(soulprint.hosts.filter(h, h.dropped))) }"
      - name: two
        module: core.exec.run
        params: { cmd: two }
  - name: warm
    module: core.exec.run
    when: register.reload.skipped
    params: { cmd: "${ string(register.reload.skipped) }" }
`

// standingPlan is standingScenario with a keeper capture at its end reading
// register.hosts.reload. Steps: deploy 0, reload 1, one 2, two 3, warm 4, capture 5.
func standingPlan(t *testing.T) *config.ScenarioManifest {
	t.Helper()
	m := loadStagedManifest(t, standingScenario)
	m.Tasks = append(m.Tasks, config.Task{Name: "capture", Module: &config.ModuleTask{
		Module: "core.state.set",
		Params: map[string]any{"field": "reloads", "value": "${ register.hosts.reload }"},
	}})
	return m
}

// standingPass renders m at step active, its last when active is negative.
func standingPass(t *testing.T, m *config.ScenarioManifest, active int, in RenderInput) ([]*RenderedTask, []DispatchPlan) {
	t.Helper()
	plan, steps := config.LinearPassage(m.Tasks, m.Settings)
	if active < 0 {
		active = plan.Count - 1
	}
	in.Scenario, in.Input, in.Incarnation = m, map[string]any{}, IncarnationMeta{ID: "svc"}
	in.TaskPassage, in.ActivePassage, in.Steps, in.HeldTargets = plan.TaskPassage, active, steps, NewHeldTargets()
	tasks, plans, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return tasks, plans
}

// ★ GUARD (ADR-0091 amendment NIM-993, rules 2-3, fork 3). A pass renders each step
// as it stood: a held host is reached by nothing, named not reached as a dropped host
// is, and listed `held`; a host dropped later is still reached at the steps before; a
// default block's child reaches the block's hosts live at its own step and names the
// hosts held or dropped by then as not reached; a returned host gets the later steps,
// reading what it missed as skipped, in its params and in a `when:` the Keeper
// settles, and a keeper capture sees that register across hosts.
//
// Mutations: render every step with the pass's own roster (no ForStep in Render) →
// warm renders over a without its synthesized reload and the render fails; leave a
// default block child on the block's
// hosts (no narrowing in atChildStep) → two reaches c; keep the block's droppedCover
// for its children → two names only a not reached; cover the dropped hosts alone
// (coveredAway without the held) → reload names nobody not reached; settle the flow
// over the pass's own registers (no ForStep for settleFlow) → warm fails on a.
func TestRender_EachStepRendersItsOwnStanding(t *testing.T) {
	standing, _ := standingRun()
	reg := map[string]map[string]any{
		sidA: {"deploy": map[string]any{"changed": false, "failed": true}},
		sidB: {"deploy": map[string]any{"changed": true, "failed": false}, "reload": map[string]any{"changed": true, "failed": false, "skipped": false}},
		sidC: {"deploy": map[string]any{"changed": true, "failed": false}, "reload": map[string]any{"changed": true, "failed": false, "skipped": false}},
	}
	standing.BySID = map[string]HostStanding{
		sidA: {Periods: []HoldPeriod{{HeldFrom: 1, ReturnedAt: stepAt(4), Synth: map[string]any{"reload": skippedRegister()}}}},
		sidC: {DroppedAt: stepAt(3)},
	}
	m := standingPlan(t)
	in := RenderInput{Hosts: standing.Roster, Standing: standing, RegisterByHost: reg}
	tasks, plans := standingPass(t, m, -1, in)

	for _, tc := range []struct {
		name                string
		targets, notReached []string
	}{
		{"reload", []string{sidB, sidC}, []string{sidA}},
		{"one", []string{sidB, sidC}, []string{sidA}},
		{"two", []string{sidB}, []string{sidA, sidC}},
		{"warm", []string{sidA, sidB}, []string{sidC}},
	} {
		_, plan := renderedByName(t, tasks, plans, tc.name)
		if !reflect.DeepEqual(plan.TargetSIDs, tc.targets) || !reflect.DeepEqual(plan.NotReached, tc.notReached) {
			t.Errorf("%s: targets %v not reached %v, want %v and %v", tc.name, plan.TargetSIDs, plan.NotReached, tc.targets, tc.notReached)
		}
	}
	one, _ := renderedByName(t, tasks, plans, "one")
	if got, want := one.Params.AsMap()["args"], []any{"3", "1", "0"}; !reflect.DeepEqual(got, want) {
		t.Errorf("soulprint.hosts at step 2 (size, held, dropped) = %v, want %v", got, want)
	}
	warm, _ := renderedByName(t, tasks, plans, "warm")
	for sid, want := range map[string]string{sidA: "true", sidB: "false"} {
		if got := paramsForHost(warm, sid).AsMap()["cmd"]; got != want {
			t.Errorf("warm on %s reads reload.skipped %v, want %s", sid, got, want)
		}
	}
	capture, _ := renderedByName(t, tasks, plans, "capture")
	reloads, _ := capture.Params.AsMap()["value"].(map[string]any)
	if a, _ := reloads[sidA].(map[string]any); len(reloads) != 3 || a["skipped"] != true {
		t.Errorf("register.hosts.reload = %v, want all three hosts, a's synthesized as skipped", reloads)
	}

	tasks, plans = standingPass(t, m, 4, in)
	warm, _ = renderedByName(t, tasks, plans, "warm")
	if a, b := warm.FlowBySID[sidA], warm.FlowBySID[sidB]; a.Failure != nil || a.Skip || !b.Skip {
		t.Errorf("warm's when: settled a %+v and b %+v, want a sent on its synthesized skip and b skipped", a, b)
	}
}

// ★ GUARD (inert while nothing failed). A Standing in which no host's standing
// changed renders exactly what no Standing renders.
//
// Mutation: leave Hosts unset in ForStep → warm renders over no host and fails.
func TestRender_AStandingWithNothingInItChangesNothing(t *testing.T) {
	m := loadStagedManifest(t, standingScenario)
	reg := map[string]map[string]any{
		sidA: {"reload": map[string]any{"skipped": false}},
		sidB: {"reload": map[string]any{"skipped": false}},
	}
	without, withoutPlans := standingPass(t, m, -1, RenderInput{Hosts: linearHosts(), RegisterByHost: reg})
	with, withPlans := standingPass(t, m, -1, RenderInput{Hosts: linearHosts(), RegisterByHost: reg, Standing: &Standing{Roster: linearHosts()}})
	if !reflect.DeepEqual(without, with) || !reflect.DeepEqual(withoutPlans, withPlans) {
		t.Errorf("an empty Standing changed the render:\nwithout %+v\nwith    %+v", withoutPlans, withPlans)
	}
}
