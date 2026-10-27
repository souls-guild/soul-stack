package render

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Cross-step rescue units on live hosts (ADR-0091 amendment NIM-993, rule 1): a unit
// with no source before it in its own message is settled SKIPPED on every live host its
// `on:` chain selects, with nothing of it evaluated there but the plan's layout.

const unitScenario = `
name: linear
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: reload
    module: core.exec.run
    params: { cmd: reload }
  - name: roll back
    onfail: [deploy]
    where: "register.deploy.stdout == 'ok'"
    block:
      - name: restore
        module: core.exec.run
        params: { cmd: rollback }
      - name: check
        module: core.exec.run
        register: health
        failed_when: "false"
        params: { cmd: healthcheck }
      - name: report
        module: core.exec.run
        params: { cmd: "${ register.health.stdout }" }
      - name: log
        module: core.exec.run
        vars: { out: "${ register.health.stdout }" }
        when: "incarnation.id == 'svc'"
        params: { cmd: "${ vars.out }" }
      - name: undo late
        module: core.exec.run
        onfail: [late]
        where: "register.health.stdout == 'down'"
        params: { cmd: undo }
      - name: not handled
        module: core.task.failed
        when: register.health.exit_code != 0
        params: {}
  - name: each
    module: core.exec.run
    onfail: [deploy]
    vars: { out: "${ register.health.stdout }" }
    when: "incarnation.id == 'svc'"
    loop:
      items: "${ ['x', 'y'] }"
    params: { cmd: "${ vars.out } ${ item }" }
  - name: notify
    module: core.exec.run
    onfail: [deploy]
    when: register.health.exit_code != 0
    params: { cmd: "${ register.health.stdout }" }
  - name: page
    module: core.exec.run
    onfail: [deploy]
    vars: { out: "${ register.health.stdout }" }
    when: "incarnation.id == 'svc'"
    params: { cmd: "${ vars.out }" }
  - name: wrap
    block:
      - name: inner undo
        onfail: [deploy]
        where: "register.health.stdout == 'down'"
        block:
          - name: scrub
            module: core.exec.run
            params: { cmd: scrub }
  - name: warm
    module: core.exec.run
    register: late
    params: { cmd: warm }
`

func unitPass(t *testing.T, m *config.ScenarioManifest, active int, in RenderInput) ([]*RenderedTask, []DispatchPlan) {
	t.Helper()
	tasks, plans, err := unitRender(t, m, active, in)
	if err != nil {
		t.Fatalf("Render at step %d: %v", active, err)
	}
	return tasks, plans
}

func unitRender(t *testing.T, m *config.ScenarioManifest, active int, in RenderInput) ([]*RenderedTask, []DispatchPlan, error) {
	t.Helper()
	plan, steps := config.LinearPassage(m.Tasks, m.Settings)
	in.Scenario, in.Input, in.Incarnation = m, map[string]any{}, IncarnationMeta{ID: "svc"}
	in.TaskPassage, in.ActivePassage, in.Steps, in.HeldTargets = plan.TaskPassage, active, steps, NewHeldTargets()
	return NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), in)
}

func planOf(plans []DispatchPlan, rt *RenderedTask) DispatchPlan {
	for _, pl := range plans {
		if pl.TaskIndex == rt.Index {
			return pl
		}
	}
	return DispatchPlan{}
}

func indicesByName(tasks []*RenderedTask) map[string][]int {
	out := map[string][]int{}
	for _, rt := range tasks {
		out[rt.Name] = append(out[rt.Name], rt.Index)
	}
	return out
}

var unrenderedSkip = HostFlow{Skip: true, Unrendered: true}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 1). On a linear pass with nothing failed,
// the children of the block unit `roll back`, the `loop:` unit `each`, the task units
// `notify` and `page`, and the child of the nested block unit `inner undo` are settled
// SKIPPED on both live hosts, unrendered: none of their params, `vars:` or `when:`,
// nor a block unit's own `where:`, is evaluated — each reads a register no live host
// has, and evaluated it would end the run or fail the host. A static `when:` beside a
// `vars:` entry reading the unit's own output is not evaluated there either (`log`,
// `page`); a `loop:`'s is, host-free, since it fixes the loop's places (`each` keeps
// two). `undo late` also names a later step's source, which makes the block unit around
// it, the outermost, a unit of `late` too: a dropped a is reached once. Once host a is
// held with deploy failed there, every unit goes to a, rendered, beside the live b, and
// the plan indices do not move.
//
// Mutations: route a block's unit child as any other (crossStepUnit read false in
// walkBlockChildren) → Render fails on register.health in report; route a nested block
// unit as any nested block → it fails on inner undo's where:; resolve a block unit's
// hosts as any block's → it fails on the block's where:; evaluate a
// unit's static `when:` before routing it, at the top level or in a block → it fails on
// register.health in page, each or log; settle a skip host like a rendered one in
// settleFlow → restore carries no skip for a and b; render a unit nothing was routed
// to, a task or a `loop:` → Render fails on register.health in notify or each.
func TestRender_AUnitIsNotEvaluatedOnALiveHost(t *testing.T) {
	m := loadStagedManifest(t, unitScenario)
	hosts := []*topology.HostFacts{host(sidA, []string{"svc"}, nil), host(sidB, []string{"svc"}, nil)}
	down := map[string]any{"health": map[string]any{"stdout": "down", "exit_code": 1}}
	tasks, plans := unitPass(t, m, 6, RenderInput{
		Hosts:          hosts[1:],
		Dropped:        hosts[:1],
		Rescue:         &Rescue{Failed: map[string]map[string]int{sidA: {"deploy": 0}}},
		RegisterByHost: map[string]map[string]any{sidA: down, sidB: down},
	})
	rt, plan := renderedByName(t, tasks, plans, "undo late")
	if !reflect.DeepEqual(plan.TargetSIDs, []string{sidA, sidB}) || rt.FlowBySID[sidA].Skip || rt.FlowBySID[sidB] != unrenderedSkip {
		t.Errorf("undo late with a dropped: targets %v, flows %+v - want [a b], a run and b an unrendered skip", plan.TargetSIDs, rt.FlowBySID)
	}

	steps := map[string]int{
		"restore": 2, "check": 3, "report": 4, "log": 5, "undo late": 6, "not handled": 7,
		"each": 8, "notify": 9, "page": 10, "scrub": 11,
	}
	var layout []*RenderedTask
	for name, step := range steps {
		tasks, plans := unitPass(t, m, step, RenderInput{Hosts: hosts})
		var seen int
		for _, rt := range tasks {
			if rt.Name != name {
				continue
			}
			seen++
			if rt.Params != nil || rt.ParamsBySID != nil {
				t.Errorf("%s at its step: params %v, per host %v - want none rendered", name, rt.Params, rt.ParamsBySID)
			}
			if got := planOf(plans, rt).TargetSIDs; !reflect.DeepEqual(got, []string{sidA, sidB}) {
				t.Errorf("%s targets %v, want [a b]: a live host is settled SKIPPED, so it is a target", name, got)
			}
			for _, sid := range []string{sidA, sidB} {
				if rt.FlowBySID[sid] != unrenderedSkip {
					t.Errorf("%s on %s: flow %+v, want an unrendered skip", name, sid, rt.FlowBySID[sid])
				}
			}
		}
		if want := map[bool]int{true: 2, false: 1}[name == "each"]; seen != want {
			t.Errorf("%s rendered %d times at its step, want %d", name, seen, want)
		}
		if name == "each" {
			layout = tasks
		}
	}
	tasks, plans = unitPass(t, m, 12, RenderInput{Hosts: hosts})
	if rt, _ := renderedByName(t, tasks, plans, "warm"); rt.Params == nil {
		t.Error("warm, an ordinary task, was not rendered")
	}

	held := RenderInput{
		Hosts:          hosts[1:],
		HeldHosts:      hosts[:1],
		Rescue:         &Rescue{Failed: map[string]map[string]int{sidA: {"deploy": 0}}},
		RegisterByHost: map[string]map[string]any{sidA: down},
	}
	tasks, plans = unitPass(t, m, 4, held)
	rt, plan = renderedByName(t, tasks, plans, "report")
	if got := rt.Params.GetFields()["cmd"].GetStringValue(); got != "down" || !reflect.DeepEqual(plan.TargetSIDs, []string{sidA, sidB}) {
		t.Errorf("report for held a: cmd %q, targets %v - want down, [a b]", got, plan.TargetSIDs)
	}
	if rt.FlowBySID[sidA].Skip || rt.FlowBySID[sidB] != unrenderedSkip {
		t.Errorf("report flows %+v, want a run and b an unrendered skip", rt.FlowBySID)
	}
	tasks, plans = unitPass(t, m, 8, held)
	if got, want := indicesByName(tasks), indicesByName(layout); !reflect.DeepEqual(got, want) {
		t.Errorf("plan indices with a unit rendered for a = %v, laid out = %v", got, want)
	}
	for _, rt := range tasks {
		if rt.Name == "each" && rt.Params == nil {
			t.Errorf("each[%d] was not rendered for held a", rt.Index)
		}
	}
	if rt, _ := renderedByName(t, tasks, plans, "log"); rt.Params.GetFields()["cmd"].GetStringValue() != "down" {
		t.Errorf("log for held a: params %v, want cmd down", rt.Params)
	}
}

// unitDestinyD is the destiny of the `apply:` unit: one per-host input (addr), one
// host-invariant (mode), a `vars.yml` local of each kind, a `validate:` rule and a
// `required:` on the per-host input, and every construct whose layout a pass must
// keep — a `loop:`, an include group kept and one dropped by a host-invariant
// `when:`, a block, a task a static `when:` skips, and three `loop:`s beside a `vars:`
// entry reading the per-host input whose static `when:` gives them their places: three
// when false, two when true, since `loop.when` filters only a loop that runs. One is a
// block child, which only a false `when:` keeps from the guard.
func unitDestinyD() *ResolvedDestiny {
	return &ResolvedDestiny{
		Name: "d",
		Input: config.InputSchemaMap{
			"addr": {Type: "string", Required: true},
			"mode": {Type: "string", Required: true},
			"port": {Type: "integer", Default: 6379},
		},
		Validate: []config.ValidateRule{
			{That: "input.addr.startsWith('10.')", Message: "addr is a private address"},
			{That: "input.mode == 'fast'", Message: "mode is fast"},
		},
		Vars: map[string]any{"target": "${ input.addr }", "flavour": "${ input.mode }"},
		Tasks: []config.Task{
			runTask("cfg", func(t *config.Task) { t.Module.Params = map[string]any{"cmd": "${ vars.target }"} }),
			runTask("spin", func(t *config.Task) {
				t.Loop = &config.LoopSpec{Items: "${ [input.mode, 'x'] }"}
				t.Module.Params = map[string]any{"cmd": "${ item }"}
			}),
			runTask("kept", func(t *config.Task) { t.IncludeGroupID, t.IncludeWhen = 1, "vars.flavour == 'fast'" }),
			runTask("dropped", func(t *config.Task) { t.IncludeGroupID, t.IncludeWhen = 2, "input.mode == 'slow'" }),
			{Name: "grp", Block: &config.BlockTask{Block: []config.Task{
				runTask("inner", func(t *config.Task) { t.Module.Params = map[string]any{"cmd": "${ vars.target }"} }),
				runTask("burst", fanOut("input.mode == 'slow'")),
			}}},
			runTask("off", func(t *config.Task) { t.When = "input.mode == 'slow'" }),
			runTask("fan", fanOut("input.mode == 'fast'")),
			runTask("fan off", fanOut("input.mode == 'slow'")),
		},
	}
}

func fanOut(when string) func(*config.Task) {
	return func(t *config.Task) {
		t.Loop = &config.LoopSpec{Items: "${ ['p', 'q', 'r'] }", When: "item != 'q'"}
		t.When, t.Vars = when, map[string]any{"at": "${ input.addr }"}
		t.Module.Params = map[string]any{"cmd": "${ item } ${ vars.at }"}
	}
}

const applyUnitScenario = `
name: linear
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: undo
    onfail: [deploy]
    register: undo
    vars: { ip: "${ soulprint.self.network.primary_ip }" }
    apply:
      destiny: d
      input:
        addr: "${ vars.ip }"
        mode: fast
  - name: after
    module: core.exec.run
    params: { cmd: after }
`

// ★ GUARD (ADR-0091 amendment NIM-993, rule 1). An `apply:` unit with nothing failed
// is laid out over its host-invariant inputs alone: addr, which reads soulprint.self
// through the applier's `vars:`, is never rendered, nor is the destiny local reading
// it, and the contract rules on addr are skipped. A `loop:`'s static `when:` is decided
// host-free, its `vars:` reading addr left out. Every destiny task is a skip target of
// both live hosts, unrendered; the terminal is sent to them, and the dropped include
// group stays dropped. Rendered for a held host, the destiny takes the same plan
// indices, and the live host still gets nothing of it rendered. A block child whose
// static `when:` reads a `vars:` entry over addr is refused by name, laid out as routed
// (ADR-0082 amendment 2026-10-15, point 7; user, 2026-10-27).
//
// Mutations: render every apply input over the synthetic host (no
// hostInvariantApplier) → Render fails on soulprint.self.network; drop
// config.WithoutInputs → addr is required; resolve every vars.yml local → it fails on
// input.addr; render a laid-out destiny task, at the top level or in a block → it fails
// on vars.target; resolve a laid-out loop's every `vars:` entry, at the top level or in
// a block → it fails on input.addr in fan or burst; lay a loop out without its static
// `when:` → fan off takes two places; lay a block's loop child out as a module child →
// burst takes one; leave the destiny's tasks without skip targets → cfg targets
// nothing; target the terminal by the routed hosts only → it misses a and b; let
// layoutGuard pass a task with no DSL refusal unguarded → gated is laid out.
func TestRender_AnApplyUnitLaysOutItsDestinyAlone(t *testing.T) {
	m := loadStagedManifest(t, applyUnitScenario)
	ip := func(addr string) map[string]any {
		return map[string]any{"network": map[string]any{"primary_ip": addr}}
	}
	hosts := []*topology.HostFacts{host(sidA, []string{"svc"}, ip("10.0.0.1")), host(sidB, []string{"svc"}, ip("10.0.0.2"))}
	destiny := destinyByName{"d": unitDestinyD()}

	tasks, plans := unitPass(t, m, 1, RenderInput{Hosts: hosts, Destiny: destiny})
	for _, rt := range tasks {
		if rt.Passage != 1 || rt.Name == "undo" {
			continue
		}
		pl := planOf(plans, rt)
		if !reflect.DeepEqual(pl.TargetSIDs, []string{sidA, sidB}) {
			t.Errorf("%s targets %v, want [a b]", rt.Name, pl.TargetSIDs)
		}
		if rt.Name == "applier-register undo" {
			if len(rt.FlowBySID) != 0 {
				t.Errorf("the terminal is settled %v, want it sent", rt.FlowBySID)
			}
			continue
		}
		if rt.Params != nil || rt.FlowBySID[sidA] != unrenderedSkip || rt.FlowBySID[sidB] != unrenderedSkip {
			t.Errorf("%s laid out: params %v, flows %+v - want nothing rendered, an unrendered skip on a and b", rt.Name, rt.Params, rt.FlowBySID)
		}
	}
	layout := indicesByName(tasks)
	if _, ok := layout["dropped"]; ok {
		t.Error("the include group whose when: is false was laid out")
	}
	if len(layout["fan"]) != 2 || len(layout["fan off"]) != 3 || len(layout["burst"]) != 3 {
		t.Errorf("fan laid out in %v places, fan off in %v, burst in %v - want 2, 3 and 3, as their static when: gives", layout["fan"], layout["fan off"], layout["burst"])
	}

	tasks, plans = unitPass(t, m, 1, RenderInput{
		Hosts:     hosts[1:],
		HeldHosts: hosts[:1],
		Destiny:   destiny,
		Rescue:    &Rescue{Failed: map[string]map[string]int{sidA: {"deploy": 0}}},
	})
	if got := indicesByName(tasks); !reflect.DeepEqual(got, layout) {
		t.Errorf("plan indices rendered for held a = %v, laid out = %v", got, layout)
	}
	cfg, pl := renderedByName(t, tasks, plans, "cfg")
	if got := paramsForHost(cfg, sidA).GetFields()["cmd"].GetStringValue(); got != "10.0.0.1" || !reflect.DeepEqual(pl.TargetSIDs, []string{sidA, sidB}) {
		t.Errorf("cfg for held a: cmd %q, targets %v - want 10.0.0.1, [a b]", got, pl.TargetSIDs)
	}
	if cfg.FlowBySID[sidB] != unrenderedSkip || len(paramsForHost(cfg, sidB).GetFields()) != 0 {
		t.Errorf("cfg on live b: flow %+v, params %v - want an unrendered skip with no params", cfg.FlowBySID[sidB], paramsForHost(cfg, sidB))
	}

	gated := unitDestinyD()
	grp := gated.Tasks[4].Block
	grp.Block = slices.Insert(grp.Block, 1, runTask("gated", func(t *config.Task) {
		t.When, t.Vars = "vars.v == 'x'", map[string]any{"v": "${ input.addr }"}
	}))
	for pass, in := range map[string]RenderInput{
		"laid out":            {Hosts: hosts, Destiny: destinyByName{"d": gated}},
		"rendered for held a": heldA(RenderInput{Destiny: destinyByName{"d": gated}}),
	} {
		if _, _, err := unitRender(t, m, 1, in); !refusedByName(err, "vars.v ") {
			t.Errorf("gated %s: err %v, want its when: refused by name over vars.v", pass, err)
		}
	}
}

// refusedByName reports whether err is NIM-1014's refusal of a destiny static `when:`
// over a per-host name (ADR-0082 amendment 2026-10-15, point 7), its Reference starting
// with reference.
func refusedByName(err error, reference string) bool {
	var hs *cel.ErrHostOutOfScope
	return errors.As(err, &hs) && strings.HasPrefix(hs.Reference, reference)
}

const groupedUnitScenario = `
name: linear
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: canary
    serial: 2
    settings: { serial_scope: block }
    block:
      - name: undo
        module: core.exec.run
        onfail: [deploy]
        params: { cmd: "undo ${ soulprint.self.network.primary_ip }" }
      - name: tidy
        module: core.exec.run
        params: { cmd: tidy }
  - name: pair
    serial: 2
    settings: { serial_scope: block }
    block:
      - name: stage
        module: core.exec.run
        register: stage
        params: { cmd: stage }
      - name: unstage
        module: core.exec.run
        onfail: [stage]
        params: { cmd: "unstage ${ soulprint.self.network.primary_ip }" }
  - name: ahead
    serial: 2
    settings: { serial_scope: block }
    block:
      - name: unready
        module: core.exec.run
        onfail: [ready]
        params: { cmd: "unready ${ soulprint.self.network.primary_ip }" }
      - name: ready
        module: core.exec.run
        register: ready
        params: { cmd: ready }
`

// ★ GUARD (ADR-0091 amendment NIM-993, rule 1). Inside a group a unit still travels
// to a live host, as `when: false` beside its siblings — with empty params, not the
// rendering of a host it was routed to. A unit whose source is a sibling before it in
// its own group is no cross-step unit: it can fire inside the message, and renders for
// every host as before. One whose only such sibling comes after it (`unready`, a forward
// `onfail:`) cannot fire there, since the Soul runs it first: it is a cross-step unit,
// and travels to every live host as an unrendered skip.
//
// Mutations: answer an unrendered host with Params in paramsForHost → b is sent
// "undo 10.0.0.1"; take every outermost unit for a cross-step one
// (config.CrossStepUnits not reading where its sources are set) → unstage renders
// nothing; count an in-span source wherever it stands in its group (no s.order < unit
// in config.sourcesOutside) → unready renders for a and b.
func TestRender_AGroupedUnitTravelsUnrendered(t *testing.T) {
	m := loadStagedManifest(t, groupedUnitScenario)
	ip := func(addr string) map[string]any {
		return map[string]any{"network": map[string]any{"primary_ip": addr}}
	}
	a, b, c := host(sidA, []string{"svc"}, ip("10.0.0.1")), host(sidB, []string{"svc"}, ip("10.0.0.2")), host(sidC, []string{"svc"}, ip("10.0.0.3"))
	tasks, plans := unitPass(t, m, 1, RenderInput{
		Hosts:   []*topology.HostFacts{b},
		Dropped: []*topology.HostFacts{a, c},
		Rescue:  &Rescue{Failed: map[string]map[string]int{sidA: {"deploy": 0}, sidC: {"deploy": 0}}},
	})
	undo, pl := renderedByName(t, tasks, plans, "undo")
	if !undo.Grouped || !reflect.DeepEqual(pl.TargetSIDs, []string{sidA, sidB, sidC}) {
		t.Fatalf("undo: grouped %v, targets %v - want a grouped child for [a b c]", undo.Grouped, pl.TargetSIDs)
	}
	wire := ToProtoTasksForHost([]*RenderedTask{undo}, sidB)[0]
	if len(wire.GetParams().GetFields()) != 0 || wire.GetWhen() != skippedWhen {
		t.Errorf("undo sent to live b: params %v, when %q - want no params, when false", wire.GetParams(), wire.GetWhen())
	}
	if got := ToProtoTasksForHost([]*RenderedTask{undo}, sidC)[0].GetParams().GetFields()["cmd"].GetStringValue(); got != "undo 10.0.0.3" {
		t.Errorf("undo sent to c = %q, want its own rendering", got)
	}

	tasks, plans = unitPass(t, m, 2, RenderInput{Hosts: []*topology.HostFacts{a, b}})
	unstage, pl := renderedByName(t, tasks, plans, "unstage")
	if unstage.Params == nil || !reflect.DeepEqual(pl.TargetSIDs, []string{sidA, sidB}) || unstage.FlowBySID[sidA].Unrendered {
		t.Errorf("unstage: params %v, targets %v, flows %+v - want it rendered for [a b]", unstage.Params, pl.TargetSIDs, unstage.FlowBySID)
	}

	tasks, plans = unitPass(t, m, 3, RenderInput{Hosts: []*topology.HostFacts{a, b}})
	unready, pl := renderedByName(t, tasks, plans, "unready")
	if !reflect.DeepEqual(pl.TargetSIDs, []string{sidA, sidB}) || unready.FlowBySID[sidA] != unrenderedSkip || unready.FlowBySID[sidB] != unrenderedSkip {
		t.Errorf("unready: targets %v, flows %+v - want an unrendered skip for [a b]", pl.TargetSIDs, unready.FlowBySID)
	}
}

func unitHosts() []*topology.HostFacts {
	ip := func(addr string) map[string]any {
		return map[string]any{"network": map[string]any{"primary_ip": addr}}
	}
	return []*topology.HostFacts{host(sidA, []string{"svc"}, ip("10.0.0.1")), host(sidB, []string{"svc"}, ip("10.0.0.2"))}
}

func applyUnitDestiny(tasks ...config.Task) destinyByName {
	return destinyByName{"d": &ResolvedDestiny{
		Name:  "d",
		Input: config.InputSchemaMap{"addr": {Type: "string"}, "mode": {Type: "string"}},
		Tasks: tasks,
	}}
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 1; ADR-012(d)). A laid-out destiny is held
// to the DSL boundary a rendered one is: a task's `run_once:` and a block child's
// `where:` are refused before anything is routed to the unit, on a pass where nothing
// failed — under a static `when:` that is true too, the block's AND-ed into its child,
// under one reading a per-host input, directly or through a var, which no host-free
// decision settles — `has()` of that var included, false with the var left out — and
// under one reading `incarnation.host_count`, directly or through a var, which reads 0
// there and is true once a host is routed. A `loop:` is held to it the same way, though
// its `when:` gives its places: a loop's `run_once:` is refused under a `when:` reading
// a per-host var, which a destiny layout with no refusal refuses by name, or reading
// `incarnation.host_count`, false there, and a block's loop child under the first. A
// task whose static `when:` is false is not guarded there, at the top level or in a
// block, as it is not rendered: an inactive branch's DSL never blocks the active one,
// laid out or rendered for a held host.
//
// Mutations: lay a destiny task out before guardDestinyTask → bad's run_once is
// accepted; lay a block child out before guardDestinyBlockChild → bad's where is
// accepted; spare every laid-out task with a static `when:` the guard, at the top level
// or in a block → the true-when rows are accepted; read a `when:` the host-free
// decision fails on as false → "run_once, per-host when" is accepted; read an
// undecidable `when:` as false in layoutGuard, as a scenario layout reads it → the five
// per-host var rows are accepted; decide it there over the vars left out → the `has()` row is
// accepted; drop the whenReadsHostCount check from layoutGuard → the four host_count
// rows are accepted; read the `when:` alone there, not its vars → "host_count through a
// var" is accepted; lay a loop out without its guard's refusal (none passed to
// layoutStaticWhen), from renderApplyDestiny or walkBlockChildren → the loop rows or
// the block loop row are accepted; guard a laid-out task whatever its `when:`, at the
// top level or in a block, or keep every laid-out loop's refusal → diagnose, probe or
// spin off is refused.
func TestRender_ALaidOutDestinyIsGuarded(t *testing.T) {
	m := loadStagedManifest(t, applyUnitScenario)
	hosts := unitHosts()
	for name, task := range map[string]config.Task{
		"run_once":                runTask("bad", func(t *config.Task) { t.RunOnce = true }),
		"block where":             {Name: "grp", Block: &config.BlockTask{Block: []config.Task{runTask("bad", func(t *config.Task) { t.Where = "true" })}}},
		"run_once, true when":     runTask("bad", func(t *config.Task) { t.RunOnce, t.When = true, "input.mode == 'fast'" }),
		"run_once, per-host when": runTask("bad", func(t *config.Task) { t.RunOnce, t.When = true, "input.addr == '10.0.0.1'" }),
		"block where, true when": {Name: "grp", Block: &config.BlockTask{Block: []config.Task{
			runTask("bad", func(t *config.Task) { t.Where, t.When = "true", "input.mode == 'fast'" }),
		}}},
		"block where, the block's true when": {Name: "grp", When: "input.mode == 'fast'", Block: &config.BlockTask{Block: []config.Task{
			runTask("bad", func(t *config.Task) { t.Where = "true" }),
		}}},
		"run_once, host_count when": runTask("bad", func(t *config.Task) { t.RunOnce, t.When = true, "incarnation.host_count > 0" }),
		"run_once, host_count through a var": runTask("bad", func(t *config.Task) {
			t.RunOnce, t.Vars, t.When = true, map[string]any{"n": "${ incarnation.host_count }", "m": "${ vars.n }"}, "vars.m > 0"
		}),
		"block where, host_count when": {Name: "grp", Block: &config.BlockTask{Block: []config.Task{
			runTask("bad", func(t *config.Task) { t.Where, t.When = "true", "incarnation.host_count > 0" }),
		}}},
		"run_once, when through a per-host var": runTask("bad", func(t *config.Task) {
			t.RunOnce, t.Vars, t.When = true, map[string]any{"v": "${ input.addr }"}, "vars.v == '10.0.0.1'"
		}),
		"run_once, has() of a per-host var": runTask("bad", func(t *config.Task) {
			t.RunOnce, t.Vars, t.When = true, map[string]any{"v": "${ input.addr }"}, "has(vars.v)"
		}),
		"block where, when through a per-host var": {Name: "grp", Block: &config.BlockTask{Block: []config.Task{
			runTask("bad", func(t *config.Task) {
				t.Where, t.Vars, t.When = "true", map[string]any{"v": "${ input.addr }"}, "vars.v == '10.0.0.1'"
			}),
		}}},
		"loop run_once, when through a per-host var": runTask("bad", func(t *config.Task) { fanOut(perHostWhen)(t); t.RunOnce = true }),
		"loop run_once, host_count when": runTask("bad", func(t *config.Task) {
			fanOut("incarnation.host_count > 0")(t)
			t.RunOnce = true
		}),
		"block loop child, when through a per-host var": {Name: "grp", Block: &config.BlockTask{Block: []config.Task{runTask("bad", fanOut(perHostWhen))}}},
	} {
		if _, _, err := unitRender(t, m, 1, RenderInput{Hosts: hosts, Destiny: applyUnitDestiny(task)}); !errors.Is(err, ErrUnsupportedDSL) {
			t.Errorf("%s laid out: err %v, want %v", name, err, ErrUnsupportedDSL)
		}
	}

	gated := applyUnitDestiny(
		runTask("diagnose", func(t *config.Task) { t.RunOnce, t.When = true, "input.mode == 'slow'" }),
		config.Task{Name: "grp", Block: &config.BlockTask{Block: []config.Task{
			runTask("probe", func(t *config.Task) { t.Where, t.When = "true", "input.mode == 'slow'" }),
		}}},
		runTask("spin off", func(t *config.Task) { fanOut("input.mode == 'slow'")(t); t.RunOnce = true }),
	)
	for name, in := range map[string]RenderInput{
		"laid out":            {Hosts: hosts, Destiny: gated},
		"rendered for a held": {Hosts: hosts[1:], HeldHosts: hosts[:1], Destiny: gated, Rescue: &Rescue{Failed: map[string]map[string]int{sidA: {"deploy": 0}}}},
	} {
		if _, _, err := unitRender(t, m, 1, in); err != nil {
			t.Errorf("an inactive branch %s: %v, want it accepted", name, err)
		}
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 1). A laid-out destiny counts the hosts
// routed to its unit: `incarnation.host_count` reads 0 there, not the two live hosts it
// is settled SKIPPED on, and 1 once a held host is routed to it.
//
// Mutation: give the destiny pass its skip hosts as its hosts → host_count reads 2 and
// neither task is laid out.
func TestRender_ALaidOutDestinyCountsTheRoutedHosts(t *testing.T) {
	m := loadStagedManifest(t, applyUnitScenario)
	hosts := unitHosts()
	destiny := applyUnitDestiny(
		runTask("none routed", func(t *config.Task) { t.IncludeGroupID, t.IncludeWhen = 1, "incarnation.host_count == 0" }),
		runTask("one routed", func(t *config.Task) { t.IncludeGroupID, t.IncludeWhen = 2, "incarnation.host_count == 1" }),
	)
	for want, in := range map[string]RenderInput{
		"none routed": {Hosts: hosts, Destiny: destiny},
		"one routed":  {Hosts: hosts[1:], HeldHosts: hosts[:1], Destiny: destiny, Rescue: &Rescue{Failed: map[string]map[string]int{sidA: {"deploy": 0}}}},
	} {
		tasks, _ := unitPass(t, m, 1, in)
		got := indicesByName(tasks)
		if _, ok := got[want]; !ok || len(got["none routed"])+len(got["one routed"]) != 1 {
			t.Errorf("laid out %v, want %s alone", got, want)
		}
	}
}

const perHostWhen = "vars.at == 'eu'"

const perHostWhenScenario = `
name: linear
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: zone
    module: core.exec.run
    onfail: [deploy]
    vars: { at: "${ soulprint.self.network.primary_ip }" }
    when: "vars.at == 'eu'"
    loop:
      items: "${ ['p', 'q', 'r'] }"
      when: "item != 'q'"
    params: { cmd: "${ item } ${ vars.at }" }
  - name: probe zone
    module: core.exec.run
    onfail: [deploy]
    vars: { at: "${ register.deploy.stdout }" }
    when: "vars.at == 'eu'"
    loop:
      items: "${ ['p', 'q', 'r'] }"
      when: "item != 'q'"
    params: { cmd: "${ item }" }
  - name: undo
    onfail: [deploy]
    register: undo
    vars: { at: "${ soulprint.self.network.primary_ip }" }
    when: "vars.at == 'eu'"
    apply:
      destiny: d
      input: { addr: "${ vars.at }", mode: fast }
`

// unitAlone is m with deploy and its i-th task alone, that task's `when:` replaced
// unless when is empty.
func unitAlone(m *config.ScenarioManifest, i int, when string) *config.ScenarioManifest {
	out := *m
	task := m.Tasks[i]
	if when != "" {
		task.When = when
	}
	out.Tasks = []config.Task{m.Tasks[0], task}
	return &out
}

// heldA is in with host a held, deploy failed there, and b live.
func heldA(in RenderInput) RenderInput {
	hosts := unitHosts()
	in.Hosts, in.HeldHosts = hosts[1:], hosts[:1]
	in.Rescue = &Rescue{Failed: map[string]map[string]int{sidA: {"deploy": 0}}}
	in.RegisterByHost = map[string]map[string]any{sidA: {"deploy": map[string]any{"stdout": "failed"}}}
	return in
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 1; user, 2026-10-20 and 2026-10-27). Laid
// out, a scenario unit's static `when:` that reads a `vars:` entry reading a per-host
// name — `soulprint.self`, a register the host keeps — cannot be decided over no host
// and counts as false. On a pass where nothing failed a scenario `loop:` unit and an
// `apply:` unit are laid out static-false, with no render error: a loop one placeholder
// per item, its `loop.when` not read, settled SKIPPED unrendered on both live hosts;
// the applier one placeholder that targets no host, its destiny not laid out. With the
// `when:` false on the held host too, the routed pass gives the same plan indices. A
// task its DSL guard refuses keeps the refusal instead
// (TestRender_ALaidOutDestinyIsGuarded).
//
// In a destiny the same `when:`, over an entry reading an `apply: input:` rendered per
// host, is refused by name in both passes, as NIM-1014 refuses it in every destiny pass
// (ADR-0082 amendment 2026-10-15, point 7): a `loop:`, a module task, and a block's
// module child; so is a module task's `when:` reading such an input directly, or a
// `vars.yml` local over one. Nothing counts it false. A block's `loop:` child is refused
// by its DSL guard laid out (TestRender_ALaidOutDestinyIsGuarded).
//
// The two decisions leave one gap: an `apply:` unit counted false lays out no destiny, so
// a destiny `when:` over a per-host input is accepted laid out and refused by name only
// routed to a host where the applier's `when:` is true.
//
// Mutations: decide the `when:` over the entries left out, as before → Render fails on
// no such key at; read an undecidable `when:` as true → zone takes two places and undo
// lays its destiny out; let layoutGuard pass a task with no DSL refusal unguarded →
// every destiny row is accepted laid out; refuse in layoutGuard every `when:` the layout
// leaves an entry of out, at the scenario level too → the scenario rows are refused;
// clear in.hostVariantInputs before layoutGuard hands the task to guardStaticWhenHost →
// the direct-input row (and the task-var rows) are accepted laid out; clear in.destinyVars
// and in.hostVariantVars there, leaving the task rung's names alone → the vars.yml-local
// row is accepted laid out; lay out the destiny of an `apply:` unit no host is routed to
// whatever its `when:` (unitStaticWhen returning before layoutStaticWhen for every task
// but a `loop:`) → the gap row is refused laid out.
func TestRender_ALaidOutWhenReadingAPerHostVarIsFalse(t *testing.T) {
	scenario := loadStagedManifest(t, perHostWhenScenario)
	applier := loadStagedManifest(t, applyUnitScenario)
	perHost := func(t *config.Task) { t.When, t.Vars = perHostWhen, map[string]any{"at": "${ input.addr }"} }
	directly := applyUnitDestiny(runTask("zone", func(t *config.Task) { t.When = "input.addr == 'x'" }))
	viaLocal := applyUnitDestiny(runTask("zone", func(t *config.Task) { t.When = "vars.target == 'x'" }))
	viaLocal["d"].Vars = map[string]any{"target": "${ input.addr }"}
	for label, tc := range map[string]struct {
		destiny   destinyByName
		reference string
	}{
		"a destiny loop":                                               {applyUnitDestiny(runTask("zone", fanOut(perHostWhen))), "vars.at "},
		"a destiny module task":                                        {applyUnitDestiny(runTask("zone", perHost)), "vars.at "},
		"a block's module child":                                       {applyUnitDestiny(config.Task{Name: "grp", Block: &config.BlockTask{Block: []config.Task{runTask("zone", perHost)}}}), "vars.at "},
		"a module task reading a per-host input directly":              {directly, "input.addr "},
		"a module task reading a vars.yml local over a per-host input": {viaLocal, "vars.target "},
	} {
		for pass, in := range map[string]RenderInput{
			"laid out":         {Hosts: unitHosts(), Destiny: tc.destiny},
			"routed to held a": heldA(RenderInput{Destiny: tc.destiny}),
		} {
			if _, _, err := unitRender(t, applier, 1, in); !refusedByName(err, tc.reference) {
				t.Errorf("%s, %s: err %v, want its when: refused by name over %s", label, pass, err, strings.TrimSpace(tc.reference))
			}
		}
	}

	for _, tc := range []struct {
		label, name string
		m           *config.ScenarioManifest
		step        int
		destiny     destinyByName
		places      int
		// refused is the reference a render routed to held a is refused by name over;
		// empty, it gives the layout's plan indices.
		refused string
	}{
		{"a scenario loop, its var reading soulprint.self", "zone", unitAlone(scenario, 1, ""), 1, nil, 3, ""},
		{"a scenario loop, its var reading a register", "probe zone", unitAlone(scenario, 2, ""), 1, nil, 3, ""},
		{"an apply unit, its var reading soulprint.self", "undo", unitAlone(scenario, 3, ""), 1, applyUnitDestiny(runTask("cfg")), 1, ""},
		{
			"an apply unit counted false, its destiny's when: over a per-host input", "undo",
			unitAlone(scenario, 3, "vars.at == '10.0.0.1'"), 1,
			applyUnitDestiny(runTask("cfg", func(t *config.Task) { t.When = "input.addr == 'x'" })), 1, "input.addr ",
		},
	} {
		tasks, plans, err := unitRender(t, tc.m, tc.step, RenderInput{Hosts: unitHosts(), Destiny: tc.destiny})
		if err != nil {
			t.Errorf("%s laid out: %v, want it laid out static-false", tc.label, err)
			continue
		}
		layout := indicesByName(tasks)
		if n := len(layout[tc.name]); n != tc.places {
			t.Errorf("%s laid out in %d places, want %d", tc.label, n, tc.places)
		}
		if _, ok := layout["cfg"]; ok {
			t.Errorf("%s: the destiny of a static-false applier was laid out", tc.label)
		}
		for _, rt := range tasks {
			if rt.Name != tc.name {
				continue
			}
			pl := planOf(plans, rt)
			if rt.When != tc.m.Tasks[1].When || rt.Params != nil {
				t.Errorf("%s[%d]: when %q, params %v - want a static-false placeholder", tc.label, rt.Index, rt.When, rt.Params)
			}
			skipped := []string{sidA, sidB}
			if tc.places == 1 {
				skipped = nil
			}
			if !reflect.DeepEqual(pl.TargetSIDs, skipped) {
				t.Errorf("%s[%d] targets %v, want %v", tc.label, rt.Index, pl.TargetSIDs, skipped)
			}
			for _, sid := range skipped {
				if rt.FlowBySID[sid] != unrenderedSkip {
					t.Errorf("%s[%d] on %s: flow %+v, want an unrendered skip", tc.label, rt.Index, sid, rt.FlowBySID[sid])
				}
			}
		}
		routed, _, err := unitRender(t, tc.m, tc.step, heldA(RenderInput{Destiny: tc.destiny}))
		if tc.refused != "" {
			if !refusedByName(err, tc.refused) {
				t.Errorf("%s routed to held a, its when: true there: err %v, want the destiny's when: refused by name over %s", tc.label, err, strings.TrimSpace(tc.refused))
			}
			continue
		}
		if err != nil {
			t.Errorf("%s routed to held a: %v", tc.label, err)
			continue
		}
		if got := indicesByName(routed); !reflect.DeepEqual(got, layout) {
			t.Errorf("%s: plan indices routed to held a = %v, laid out = %v", tc.label, got, layout)
		}
	}
}

const lateBoundWhenScenario = `
name: linear
vars:
  zone: "${ vars.at }"
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: zone
    module: core.exec.run
    onfail: [deploy]
    vars: { at: "${ soulprint.self.network.primary_ip }" }
    when: "vars.zone == 'eu'"
    loop:
      items: "${ ['p', 'q', 'r'] }"
    params: { cmd: "${ item }" }
  - name: flag
    module: core.exec.run
    onfail: [deploy]
    vars: { at: "${ soulprint.self.network.primary_ip }" }
    when: "incarnation.id == 'svc'"
    changed_when: "vars.zone == 'eu'"
    loop:
      items: "${ ['p', 'q'] }"
    params: { cmd: "${ item }" }
`

// ★ GUARD (ADR-0091 amendment NIM-993, rule 1; ADR-0082 amendment 2026-10-15, point
// 16). The entries a layout leaves out are the names of the task's whole view that read
// a per-host name. Under late binding a scenario `vars:` definition is one when it reads
// the task's override: `zone` reads the task's `at` over soulprint.self. A view
// evaluates a name when it is read, so a laid-out flow_context carries none of them.
// Laid out, a `when:` reading `zone` counts as false, and a `changed_when:` beside a
// `when:` that is true reads neither `zone` nor a destiny's `vars.yml` local over a
// per-host input. Routed to held a, the plan indices are the same.
//
// Mutations: close hostFreeWhen over the task's own `vars:` alone, or build a laid-out
// flow_context with the left-out names in it → zone and flag fail on vars.at, tune on
// input.addr.
func TestRender_ALaidOutWhenLeavesOutALateBoundVar(t *testing.T) {
	m := loadStagedManifest(t, lateBoundWhenScenario)
	applier := loadStagedManifest(t, applyUnitScenario)
	tune := destinyByName{"d": &ResolvedDestiny{
		Name:  "d",
		Input: config.InputSchemaMap{"addr": {Type: "string"}, "mode": {Type: "string"}},
		Vars:  map[string]any{"target": "${ input.addr }"},
		Tasks: []config.Task{runTask("tune", func(t *config.Task) {
			t.Loop = &config.LoopSpec{Items: "${ ['p', 'q'] }"}
			t.When, t.ChangedWhen = "input.mode == 'fast'", "vars.target == 'x'"
		})},
	}}
	for _, tc := range []struct {
		name    string
		m       *config.ScenarioManifest
		destiny destinyByName
		places  int
	}{
		{"zone", unitAlone(m, 1, ""), nil, 3},
		{"flag", unitAlone(m, 2, ""), nil, 2},
		{"tune", applier, tune, 2},
	} {
		tasks, _, err := unitRender(t, tc.m, 1, RenderInput{Hosts: unitHosts(), Destiny: tc.destiny})
		if err != nil {
			t.Errorf("%s laid out: %v, want it laid out", tc.name, err)
			continue
		}
		layout := indicesByName(tasks)
		if n := len(layout[tc.name]); n != tc.places {
			t.Errorf("%s laid out in %d places, want %d", tc.name, n, tc.places)
		}
		for _, rt := range tasks {
			if rt.Name == tc.name && (rt.Params != nil || rt.FlowBySID[sidA] != unrenderedSkip) {
				t.Errorf("%s[%d]: params %v, flow on a %+v - want an unrendered skip", tc.name, rt.Index, rt.Params, rt.FlowBySID[sidA])
			}
		}
		routed, _, err := unitRender(t, tc.m, 1, heldA(RenderInput{Destiny: tc.destiny}))
		if err != nil {
			t.Errorf("%s routed to held a: %v", tc.name, err)
			continue
		}
		if got := indicesByName(routed); !reflect.DeepEqual(got, layout) {
			t.Errorf("%s: plan indices routed to held a = %v, laid out = %v", tc.name, got, layout)
		}
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 1). Routed to a held host, a scenario
// unit's static `when:` reading a per-host `vars:` entry is decided on that host, as
// before: true there, the scenario `loop:` renders its two iterations for it. A
// destiny's is refused by name though it is true on that host, as in every destiny pass
// (ADR-0082 amendment 2026-10-15, point 7; user, 2026-10-27).
//
// Mutations: decide a routed unit's `when:` host-free too, in unitStaticWhen → probe
// zone takes three placeholders; drop the guard from staticWhenContext → zone renders
// its two iterations for a.
func TestRender_ARoutedUnitDecidesItsWhenOnTheHost(t *testing.T) {
	scenario := loadStagedManifest(t, perHostWhenScenario)
	applier := loadStagedManifest(t, applyUnitScenario)
	in := heldA(RenderInput{Destiny: applyUnitDestiny(runTask("zone", fanOut("vars.at == '10.0.0.1'")))})
	in.RegisterByHost[sidA]["deploy"] = map[string]any{"stdout": "eu"}

	tasks, plans := unitPass(t, unitAlone(scenario, 2, ""), 1, in)
	var ran int
	for _, rt := range tasks {
		if rt.Name != "probe zone" {
			continue
		}
		if paramsForHost(rt, sidA) == nil || !reflect.DeepEqual(planOf(plans, rt).TargetSIDs, []string{sidA, sidB}) {
			t.Errorf("probe zone[%d] for held a: params %v, targets %v - want it rendered for a, b settled", rt.Index, rt.Params, planOf(plans, rt).TargetSIDs)
		}
		ran++
	}
	if ran != 2 {
		t.Errorf("probe zone routed to held a takes %d places, want its two iterations", ran)
	}

	if _, _, err := unitRender(t, applier, 1, in); !refusedByName(err, "vars.at ") {
		t.Errorf("a destiny loop routed to held a, its when: true there: err %v, want it refused by name over vars.at", err)
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 1). Only a scenario unit's `when:` reading
// a per-host `vars:` entry counts as false laid out. One beside such an entry that
// reads a key no layer has, or a misspelt var, fails the render as it does routed, and
// so does one that does not compile, which reads every entry — at the top level of a
// scenario and in a destiny. The gap (Pipeline.layoutStaticWhen), at the scenario level
// alone: a misspelt var in a `when:` that also reads such an entry is not told from the
// entry left out, so it counts as false laid out and fails only routed, on a host where
// at is not 'eu'. In a destiny such a `when:` is refused by name in both passes, as one
// that does not compile and names a var is (ADR-0082 amendment 2026-10-15, point 7;
// user, 2026-10-27).
//
// Mutations: read every `when:` beside a per-host entry as undecidable → nope and att
// are accepted; read every static-when error laid out as false → nope and att are
// accepted; drop the compile check from an undecidable `when:` → the two
// "does not compile" rows are accepted; fail an undecidable `when:` on any eval error →
// the scenario misspelt-beside row fails laid out; let layoutGuard pass a task with no
// DSL refusal unguarded → the refused rows are accepted or fail on their eval error laid
// out.
func TestRender_ALaidOutWhenFailsOnAnythingElse(t *testing.T) {
	scenario := loadStagedManifest(t, perHostWhenScenario)
	applier := loadStagedManifest(t, applyUnitScenario)
	const misspeltBeside = "vars.at == 'eu' || vars.att == 'x'"
	const wholeVars = "a whole-namespace read of vars"
	for _, tc := range []struct {
		label      string
		m          *config.ScenarioManifest
		step       int
		destiny    destinyByName
		routedOnly bool
		// refused, when set, is the start of the Reference NIM-1014's refusal names, in
		// both passes.
		refused string
	}{
		{"a destiny loop reading a key no layer has", applier, 1, applyUnitDestiny(runTask("zone", fanOut("input.nope == 'eu'"))), false, ""},
		{"a destiny loop reading a misspelt var", applier, 1, applyUnitDestiny(runTask("zone", fanOut("vars.att == 'eu'"))), false, ""},
		{"a destiny loop that does not compile", applier, 1, applyUnitDestiny(runTask("zone", fanOut("incarnation.id == "))), false, ""},
		{"a scenario loop reading a misspelt var", unitAlone(scenario, 1, "vars.att == 'eu'"), 1, nil, false, ""},
		{"an apply unit reading a misspelt var", unitAlone(scenario, 3, "vars.att == 'eu'"), 1, applyUnitDestiny(runTask("cfg")), false, ""},
		{"a scenario loop that does not compile", unitAlone(scenario, 1, "vars.at == "), 1, nil, false, ""},
		{"a scenario loop, a misspelt var beside a per-host one", unitAlone(scenario, 1, misspeltBeside), 1, nil, true, ""},
		{"a destiny loop, a misspelt var beside a per-host one", applier, 1, applyUnitDestiny(runTask("zone", fanOut(misspeltBeside))), false, "vars.at "},
		{"a destiny loop naming a per-host var that does not compile", applier, 1, applyUnitDestiny(runTask("zone", fanOut("vars.at == "))), false, wholeVars},
	} {
		if tc.refused != "" {
			for pass, in := range map[string]RenderInput{
				"laid out":         {Hosts: unitHosts(), Destiny: tc.destiny},
				"routed to held a": heldA(RenderInput{Destiny: tc.destiny}),
			} {
				if _, _, err := unitRender(t, tc.m, tc.step, in); !refusedByName(err, tc.refused) {
					t.Errorf("%s %s: err %v, want it refused by name, %q", tc.label, pass, err, tc.refused)
				}
			}
			continue
		}
		in, pass := RenderInput{Hosts: unitHosts(), Destiny: tc.destiny}, "laid out"
		if tc.routedOnly {
			if _, _, err := unitRender(t, tc.m, tc.step, in); err != nil {
				t.Errorf("%s laid out: %v, want it counted false", tc.label, err)
			}
			in, pass = heldA(RenderInput{Destiny: tc.destiny}), "routed to held a"
		}
		var evalErr *staticWhenEvalError
		if _, _, err := unitRender(t, tc.m, tc.step, in); !errors.As(err, &evalErr) {
			t.Errorf("%s %s: err %v, want its static-when error", tc.label, pass, err)
		}
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 1; ADR-012(d)). A scenario unit laid out
// keeps its DSL guard's refusal as a laid-out destiny does: on a pass where nothing
// failed, a block's `loop:` unit child and a `loop:` on an `apply:` unit are refused
// under a static `when:` reading a per-host var, which routed may be true. Under one
// false host-free the block's child is laid out, an inactive branch.
//
// Mutations: pass unitStaticWhen no refusal from walkBlockChildren → the block row is
// accepted; pass it nil from Render (the guardPilotDSL argument) → only the applier row
// fails, with err nil where ErrUnsupportedDSL is wanted, and the block row stays refused
// by walkBlockChildren's own; keep every laid-out loop's refusal in layoutStaticWhen →
// the false-when row is refused.
func TestRender_ALaidOutScenarioUnitIsGuarded(t *testing.T) {
	scenario := loadStagedManifest(t, perHostWhenScenario)
	inBlock := func(when string) *config.ScenarioManifest {
		m := unitAlone(scenario, 1, when)
		m.Tasks[1] = config.Task{Name: "grp", Block: &config.BlockTask{Block: []config.Task{m.Tasks[1]}}}
		return m
	}
	looped := unitAlone(scenario, 3, "")
	looped.Tasks[1].Loop = &config.LoopSpec{Items: "${ ['p', 'q'] }"}
	for _, tc := range []struct {
		label   string
		m       *config.ScenarioManifest
		refused bool
	}{
		{"a block's loop unit child, per-host when", inBlock(""), true},
		{"a loop on an apply unit, per-host when", looped, true},
		{"a block's loop unit child, false when", inBlock("incarnation.id == 'other'"), false},
	} {
		_, _, err := unitRender(t, tc.m, 1, RenderInput{Hosts: unitHosts(), Destiny: applyUnitDestiny(runTask("cfg"))})
		if tc.refused && !errors.Is(err, ErrUnsupportedDSL) {
			t.Errorf("%s laid out: err %v, want %v", tc.label, err, ErrUnsupportedDSL)
		}
		if !tc.refused && err != nil {
			t.Errorf("%s laid out: %v, want it accepted", tc.label, err)
		}
	}
}

const staticFalseUnitScenario = `
name: linear
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: off
    module: core.exec.run
    register: off
    when: "incarnation.id == 'other'"
    params: { cmd: off }
  - name: off apply
    register: off_apply
    when: "incarnation.id == 'other'"
    apply:
      destiny: d
      input: { addr: "x", mode: fast }
  - name: page
    module: core.exec.run
    onfail: [deploy]
    register: page
    when: "incarnation.id == 'other'"
    params: { cmd: page }
  - name: roll back
    onfail: [deploy]
    when: "incarnation.id == 'other'"
    block:
      - name: restore
        module: core.exec.run
        params: { cmd: restore }
  - name: undo
    onfail: [deploy]
    register: undo
    when: "incarnation.id == 'other'"
    apply:
      destiny: d
      input: { addr: "x", mode: fast }
`

// staticFalsePasses are the passes a cross-step unit is rendered on: laid out, with
// nothing failed, and routed to host a, held with deploy failed there.
func staticFalsePasses() map[string]RenderInput {
	hosts := unitHosts()
	return map[string]RenderInput{
		"laid out": {Hosts: hosts},
		"routed":   {Hosts: hosts[1:], HeldHosts: hosts[:1], Rescue: &Rescue{Failed: map[string]map[string]int{sidA: {"deploy": 0}}}},
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 1). A task unit and a block unit whose
// static `when:` is false are routed before that `when:` is read, so each is still
// settled SKIPPED, unrendered, on the live hosts its `on:` chain selects — on a and b
// laid out, on b once a is held and routed to — and the routed host is sent nothing.
// Read before routing, as it was before NIM-993's rule-1 routing, the `when:` would
// leave every live host without them.
//
// Mutations: decide a unit's static `when:` before routing it, at the top level or in
// a block → page or restore targets nothing; leave a skipped unit without its skip
// targets in unitStaticWhen → the routed rows target nothing.
func TestRender_AStaticFalseUnitIsSettledOnItsLiveHosts(t *testing.T) {
	m := loadStagedManifest(t, staticFalseUnitScenario)
	want := map[string][]string{"laid out": {sidA, sidB}, "routed": {sidB}}
	for pass, in := range staticFalsePasses() {
		for name, step := range map[string]int{"page": 3, "restore": 4} {
			tasks, plans := unitPass(t, m, step, in)
			rt, pl := renderedByName(t, tasks, plans, name)
			if !reflect.DeepEqual(pl.TargetSIDs, want[pass]) || rt.Params != nil || rt.ParamsBySID != nil {
				t.Errorf("%s %s: targets %v, params %v - want %v, nothing rendered", name, pass, pl.TargetSIDs, rt.Params, want[pass])
			}
			for _, sid := range want[pass] {
				if rt.FlowBySID[sid] != unrenderedSkip {
					t.Errorf("%s %s on %s: flow %+v, want an unrendered skip", name, pass, sid, rt.FlowBySID[sid])
				}
			}
			if _, ok := rt.FlowBySID[sidA]; pass == "routed" && ok {
				t.Errorf("%s routed: the held a has flow %+v, want it sent nothing", name, rt.FlowBySID[sidA])
			}
		}
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 1). A static-false applier leaves no host
// anything — its one placeholder targets no host, so no record and no register is
// written — and a static-false applier unit leaves a live host just that, laid out or
// routed, not the SKIPPED record that would read `register.undo.skipped` true there. A
// static-false module task that is no unit targets no host either; a module unit is
// settled SKIPPED on its live hosts (TestRender_AStaticFalseUnitIsSettledOnItsLiveHosts).
//
// Mutation: give a skipped applier unit its skip targets in unitStaticWhen → undo
// targets the live hosts.
func TestRender_AStaticFalseApplierUnitLeavesNothingOnALiveHost(t *testing.T) {
	m := loadStagedManifest(t, staticFalseUnitScenario)
	for pass, in := range staticFalsePasses() {
		for name, step := range map[string]int{"off": 1, "off apply": 2, "undo": 5} {
			tasks, plans := unitPass(t, m, step, in)
			if n := len(indicesByName(tasks)[name]); n != 1 {
				t.Fatalf("%s %s: %d places, want one placeholder", name, pass, n)
			}
			rt, pl := renderedByName(t, tasks, plans, name)
			if len(pl.TargetSIDs) != 0 || len(rt.FlowBySID) != 0 {
				t.Errorf("%s %s: targets %v, flows %+v - want no host sent or settled anything", name, pass, pl.TargetSIDs, rt.FlowBySID)
			}
		}
	}
}

const pastEndUnitScenario = `
name: linear
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: stop
    module: core.exec.run
    register: stop
    params: { cmd: stop }
  - name: fan
    module: core.exec.run
    onfail: [deploy]
    when: "incarnation.id == 'other'"
    loop:
      items: "${ ['p', 'q', 'r'] }"
    params: { cmd: "${ item }" }
  - name: spread
    module: core.exec.run
    onfail: [deploy]
    when: "incarnation.id == 'other'"
    params: { cmd: spread }
  - name: wrap
    onfail: [deploy]
    block:
      - name: spin
        module: core.exec.run
        when: "incarnation.id == 'other'"
        params: { cmd: spin }
`

// ★ GUARD (ADR-0091 amendment NIM-993, rule 1; ADR-012(d)). Past the end of a run
// that stop ended, a unit of deploy, which failed nowhere, is routed to no host and is
// one placeholder at its step, before the DSL guard and with its `when:` not read: the
// `loop:` fan, three places when its static-false `when:` is read, takes one, and a
// block child with a `loop:` (spin) or an `apply:` with one (spread), DSL the guard
// refuses, is not refused, as a static-false `when:` keeps a task that is no unit from
// the guard. Each stands alone after deploy and stop.
//
// Mutations: let a unit routed to no host past the end fall through to the guard in
// unitStaticWhen → spin and spread are refused; decide its static `when:` host-free
// past the end, as for a unit the run goes on with → fan and spin take their loop's
// places (and spread, read as a loop of placeholders, dereferences the module it lacks).
func TestRender_APastEndUnitRoutedToNoHostIsOnePlaceholder(t *testing.T) {
	base := loadStagedManifest(t, pastEndUnitScenario)
	spin := base.Tasks[4]
	blk := *spin.Block
	blk.Block = slices.Clone(blk.Block)
	blk.Block[0].Loop = &config.LoopSpec{Items: "${ ['p', 'q'] }"}
	spin.Block = &blk
	spread := base.Tasks[3]
	spread.Module, spread.Apply, spread.Loop = nil, &config.ApplyTask{Destiny: "d"}, &config.LoopSpec{Items: "${ ['p', 'q'] }"}
	ended := RenderInput{Hosts: unitHosts(), Rescue: &Rescue{Ended: true, EndedAt: 1, Failed: map[string]map[string]int{sidA: {"stop": 1}}}}
	for _, tc := range []struct {
		name string
		task config.Task
	}{{"fan", base.Tasks[2]}, {"spin", spin}, {"spread", spread}} {
		m := *base
		m.Tasks = []config.Task{base.Tasks[0], base.Tasks[1], tc.task}
		tasks, plans, err := unitRender(t, &m, 2, ended)
		if err != nil {
			t.Errorf("%s past the end: %v, want a placeholder", tc.name, err)
			continue
		}
		if n := len(indicesByName(tasks)[tc.name]); n != 1 {
			t.Errorf("%s past the end takes %d places, want one", tc.name, n)
			continue
		}
		if _, pl := renderedByName(t, tasks, plans, tc.name); len(pl.TargetSIDs) != 0 {
			t.Errorf("%s past the end targets %v, want none", tc.name, pl.TargetSIDs)
		}
	}
}
