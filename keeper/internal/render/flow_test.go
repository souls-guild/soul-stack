package render

import (
	"context"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Flow control settled on the Keeper under linear dispatch (ADR-0091,
// decision 4): a step's `when:` decided per host, its post-module predicates
// rendered per host, and the result carried to each host on its own.

const flowProbeThenCheck = `
name: linear
tasks:
  - name: probe
    module: core.exec.run
    register: probe
    params: { cmd: probe }
  - name: check
    module: core.exec.run
    failed_when: register.self.stdout != register.probe.stdout
    params: { cmd: check }
`

func probeRegisters(a, b map[string]any) map[string]map[string]any {
	return map[string]map[string]any{sidA: {"probe": a}, sidB: {"probe": b}}
}

// ★ GUARD (class NIM-908, for predicates). Two hosts whose probe answered
// differently each receive their own literal in failed_when:, with no flow_context
// beside it.
//
// Mutation: send every host the first host's predicate (flowForHost answering
// from one host's entry) → host B checks against "alice".
func TestFlow_PostModulePredicateIsRenderedPerHost(t *testing.T) {
	m := loadStagedManifest(t, flowProbeThenCheck)
	reg := probeRegisters(map[string]any{"stdout": "alice"}, map[string]any{"stdout": "bob"})
	tasks, plans, err := linearPass(t, m, 1, reg, NewHeldTargets())
	if err != nil {
		t.Fatalf("Render at check's step: %v", err)
	}
	check, _ := renderedByName(t, tasks, plans, "check")
	want := map[string]string{
		sidA: `register.self.stdout != dyn("alice")`,
		sidB: `register.self.stdout != dyn("bob")`,
	}
	for sid, text := range want {
		wire := ToProtoTasksForHost([]*RenderedTask{check}, sid)[0]
		if wire.GetFailedWhen() != text {
			t.Errorf("%s failed_when = %q, want %q", sid, wire.GetFailedWhen(), text)
		}
		if wire.GetFlowContext() != nil {
			t.Errorf("%s carries flow_context %v, want none: every reference is substituted", sid, wire.GetFlowContext())
		}
		if !check.FlowBySID[sid].Rendered {
			t.Errorf("%s: Rendered = false for a substituted predicate", sid)
		}
	}
	if !PlanIsPerHost([]*RenderedTask{check}) {
		t.Errorf("PlanIsPerHost = false: a converter without a SID would flatten the predicates onto one host")
	}
}

// A destiny's tasks travel whole and the Soul keeps evaluating their predicates
// (decision 6): an applier step settles nothing.
//
// Mutation: settle flow control in the destiny pass too → its task's changed_when:
// travels without the flow_context the Soul evaluates it against.
func TestFlow_DestinyTasksAreNotSettled(t *testing.T) {
	scn := applyScenario("pilot-flat", map[string]any{"marker_file": "/m", "marker_payload": "p"})
	tasks, plans, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), RenderInput{
		Scenario: scn, Input: map[string]any{}, Incarnation: IncarnationMeta{ID: "svc"}, Hosts: linearHosts(),
		Steps: mustSteps(scn), Destiny: &stubDestinyResolver{resolved: flatDestiny()},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	rec, _ := renderedByName(t, tasks, plans, "Record placement")
	if rec.FlowBySID != nil {
		t.Errorf("a destiny task was settled: %+v", rec.FlowBySID)
	}
	if wire := ToProtoTasksForHost([]*RenderedTask{rec}, sidA)[0]; wire.GetChangedWhen() != "false" || wire.GetFlowContext() == nil {
		t.Errorf("destiny task wire = {changed_when:%q flow_context:%v}, want it as written with its flow_context", wire.GetChangedWhen(), wire.GetFlowContext() != nil)
	}
}

// ★ GUARD. A step's `when:` is decided on the Keeper per host: where it holds it is
// blanked from the wire, where it does not the host skips.
//
// Mutations: leave the decided when: on the wire → host A is sent a predicate
// reading a register it does not have; decide once for every host → B runs.
func TestFlow_WhenIsDecidedPerHostAndBlanked(t *testing.T) {
	m := loadStagedManifest(t, `
name: linear
tasks:
  - name: probe
    module: core.exec.run
    register: probe
    params: { cmd: probe }
  - name: act
    module: core.exec.run
    when: register.probe.stdout == 'yes'
    params: { cmd: act }
`)
	reg := probeRegisters(map[string]any{"stdout": "yes"}, map[string]any{"stdout": "no"})
	tasks, plans, err := linearPass(t, m, 1, reg, NewHeldTargets())
	if err != nil {
		t.Fatalf("Render at act's step: %v", err)
	}
	act, _ := renderedByName(t, tasks, plans, "act")
	if hf := act.FlowBySID[sidA]; hf.Skip || hf.Failure != nil {
		t.Errorf("A = %+v, want it runs", hf)
	}
	if w := ToProtoTasksForHost([]*RenderedTask{act}, sidA)[0].GetWhen(); w != "" {
		t.Errorf("A is sent when: %q, want the decided when: blanked", w)
	}
	if hf := act.FlowBySID[sidB]; !hf.Skip {
		t.Errorf("B = %+v, want skipped", hf)
	}
}

// ★ GUARD (predicate-rendering.md §5, the two guards lifted). A `when:` over
// soulprint.self, or over a task var derived from it, on a multi-host target is
// refused under staged dispatch, where one flow_context served every host; under
// linear dispatch it is decided per host.
//
// Mutations: keep either guard in a linear pass → that case fails the render.
func TestFlow_HostVariantWhenIsSettledOnlyUnderLinearDispatch(t *testing.T) {
	for name, tc := range map[string]struct{ task, refusal string }{
		"soulprint": {
			task: `
    when: soulprint.self.sid == '` + sidA + `'`,
			refusal: "host-variant flow-control predicate",
		},
		"vars from soulprint": {
			task: `
    vars: { me: "${ soulprint.self.sid }" }
    when: vars.me == '` + sidA + `'`,
			refusal: "host-variant flow_context",
		},
	} {
		m := loadStagedManifest(t, `
name: linear
tasks:
  - name: only-a
    module: core.exec.run`+tc.task+`
    params: { cmd: x }
`)
		tasks, plans, err := linearPass(t, m, 0, nil, NewHeldTargets())
		if err != nil {
			t.Fatalf("%s: linear Render: %v", name, err)
		}
		rt, _ := renderedByName(t, tasks, plans, "only-a")
		if rt.FlowBySID[sidA].Skip || !rt.FlowBySID[sidB].Skip {
			t.Errorf("%s: FlowBySID = %+v, want A runs and B skips", name, rt.FlowBySID)
		}

		_, _, err = NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), RenderInput{
			Scenario: m, Input: map[string]any{}, Incarnation: IncarnationMeta{ID: "svc"}, Hosts: linearHosts(),
		})
		if err == nil || !strings.Contains(err.Error(), tc.refusal) {
			t.Errorf("%s: staged Render error = %v, want %q", name, err, tc.refusal)
		}
	}
}

// ★ GUARD. A `when:` that fails to evaluate on one host fails that host's task and
// leaves the other host's alone; the render itself does not fail.
//
// Mutation: return the evaluation error from Render → the whole run aborts
// render_failed.
func TestFlow_WhenErrorFailsOnlyThatHost(t *testing.T) {
	m := loadStagedManifest(t, `
name: linear
tasks:
  - name: probe
    module: core.exec.run
    register: probe
    params: { cmd: probe }
  - name: act
    module: core.exec.run
    when: register.probe.rc > 0
    params: { cmd: act }
`)
	reg := probeRegisters(map[string]any{"rc": 1}, map[string]any{"stdout": "no rc here"})
	tasks, plans, err := linearPass(t, m, 1, reg, NewHeldTargets())
	if err != nil {
		t.Fatalf("Render failed as a whole: %v", err)
	}
	act, _ := renderedByName(t, tasks, plans, "act")
	if hf := act.FlowBySID[sidA]; hf.Skip || hf.Failure != nil {
		t.Errorf("A = %+v, want it runs", hf)
	}
	f := act.FlowBySID[sidB].Failure
	if f == nil || f.Code != "flowcontrol.when_error" {
		t.Fatalf("B failure = %+v, want flowcontrol.when_error", f)
	}
}

// ★ GUARD. A `when:` reading a secret that fails to evaluate does not carry CEL's
// error into the failure, which reaches error_summary and the audit log: CEL quotes
// a missing map key by its value.
//
// Mutation: keep the error text for every predicate → the secret is in the message.
func TestFlow_WhenErrorOverASecretWithholdsTheDetail(t *testing.T) {
	const secret = "s3cr3t-value"
	for name, when := range map[string]string{
		"static":           `{'known': 1}[input.token] == 1`,
		"reads a register": `register.probe.known[input.token] == 1`,
	} {
		for _, secretInput := range []bool{true, false} {
			m := loadStagedManifest(t, `
name: linear
tasks:
  - name: probe
    module: core.exec.run
    register: probe
    params: { cmd: probe }
  - name: act
    module: core.exec.run
    when: "`+when+`"
    params: { cmd: act }
`)
			m.Input = config.InputSchemaMap{"token": {Type: "string", Secret: secretInput}}
			plan, steps := config.LinearPassage(m.Tasks, m.Settings)
			reg := probeRegisters(map[string]any{"known": map[string]any{}}, map[string]any{"known": map[string]any{}})
			tasks, plans, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), RenderInput{
				Scenario: m, Input: map[string]any{"token": secret}, Incarnation: IncarnationMeta{ID: "svc"},
				Hosts: linearHosts(), Steps: steps, TaskPassage: plan.TaskPassage, ActivePassage: 1, RegisterByHost: reg,
			})
			if err != nil {
				t.Fatalf("%s, secret=%v: Render: %v", name, secretInput, err)
			}
			act, _ := renderedByName(t, tasks, plans, "act")
			f := act.FlowBySID[sidA].Failure
			if f == nil || f.Code != "flowcontrol.when_error" {
				t.Fatalf("%s, secret=%v: failure = %+v, want flowcontrol.when_error", name, secretInput, f)
			}
			if got := strings.Contains(f.Message, secret); got == secretInput {
				t.Errorf("%s, secret=%v: the message quotes the value = %v: %s", name, secretInput, got, f.Message)
			}
		}
	}
}

// ★ GUARD (decision 4: "as a failing static when: does"). A static `when:` that fails
// to evaluate fails the task on every host it targets, without rendering its params,
// instead of failing the render; staged dispatch still fails the render.
//
// Mutation: return the static evaluation error from emitStaticWhenSkip under linear
// dispatch too → Render fails.
func TestFlow_StaticWhenErrorFailsTheTaskPerHost(t *testing.T) {
	m := loadStagedManifest(t, `
name: linear
tasks:
  - name: act
    module: core.exec.run
    when: input.action == 'go'
    params: { cmd: "${ input.missing_too }" }
`)
	tasks, plans, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), RenderInput{
		Scenario: m, Input: map[string]any{}, Incarnation: IncarnationMeta{ID: "svc"},
		Hosts: linearHosts(), Steps: mustSteps(m),
	})
	if err != nil {
		t.Fatalf("linear Render: %v", err)
	}
	act, plan := renderedByName(t, tasks, plans, "act")
	if len(plan.TargetSIDs) != 2 || act.Params != nil {
		t.Errorf("act = {Targets:%v Params:%v}, want both hosts and no params", plan.TargetSIDs, act.Params != nil)
	}
	for _, sid := range []string{sidA, sidB} {
		if f := act.FlowBySID[sid].Failure; f == nil || f.Code != "flowcontrol.when_error" {
			t.Errorf("%s failure = %+v, want flowcontrol.when_error", sid, f)
		}
	}

	_, _, err = NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), RenderInput{
		Scenario: m, Input: map[string]any{}, Incarnation: IncarnationMeta{ID: "svc"}, Hosts: linearHosts(),
	})
	if err == nil || !strings.Contains(err.Error(), "static-when") {
		t.Errorf("staged Render error = %v, want the static-when failure", err)
	}
}

func mustSteps(m *config.ScenarioManifest) []config.StepNode {
	_, steps := config.LinearPassage(m.Tasks, m.Settings)
	return steps
}

// ★ GUARD. Under linear dispatch too, a static `when:` that fails to evaluate fails
// the render for an applier and a keeper task — nothing past the skip reads their
// `when:`, so settled per host they would run — and for a loop, whose `loop.when` can
// leave no iteration to carry the failure.
//
// Mutations: drop the applier, keeper or loop exclusion from settlesStaticWhenError →
// that case renders without an error.
func TestFlow_StaticWhenErrorStillFailsWhereNothingCarriesIt(t *testing.T) {
	const when = "input.action == 'go'"
	cases := map[string]config.Task{
		"applier": {Name: "place", When: when, Apply: &config.ApplyTask{
			Destiny: "pilot-flat", Input: map[string]any{"marker_file": "/m", "marker_payload": "p"},
		}},
		"keeper task": {Name: "record", When: when, Module: &config.ModuleTask{
			Module: "core.state.set", Params: map[string]any{"field": "mode", "value": "x"},
		}},
		"loop": {Name: "each", When: when, Loop: &config.LoopSpec{Items: []any{1, 2}, When: "false"}, Module: &config.ModuleTask{
			Module: "core.exec.run", Params: map[string]any{"cmd": "x"},
		}},
	}
	for name, task := range cases {
		scn := &config.ScenarioManifest{Name: "linear", Tasks: []config.Task{task}}
		_, _, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), RenderInput{
			Scenario: scn, Input: map[string]any{}, Incarnation: IncarnationMeta{ID: "svc"}, Hosts: linearHosts(),
			Steps: mustSteps(scn), Destiny: &stubDestinyResolver{resolved: flatDestiny()},
		})
		if err == nil || !strings.Contains(err.Error(), "static-when") {
			t.Errorf("%s: Render error = %v, want the static-when failure", name, err)
		}
	}
}

// A post-module predicate the Keeper cannot carry fails that host's task with the
// rewrite's code, and the failure quotes the author's predicate, not the value.
func TestFlow_UnrenderablePredicateFailsOnlyThatHost(t *testing.T) {
	m := loadStagedManifest(t, flowProbeThenCheck)
	huge := strings.Repeat("s", 100_001)
	reg := probeRegisters(map[string]any{"stdout": "alice"}, map[string]any{"stdout": huge})
	tasks, plans, err := linearPass(t, m, 1, reg, NewHeldTargets())
	if err != nil {
		t.Fatalf("Render failed as a whole: %v", err)
	}
	check, _ := renderedByName(t, tasks, plans, "check")
	if f := check.FlowBySID[sidA].Failure; f != nil {
		t.Errorf("A failed: %+v", f)
	}
	f := check.FlowBySID[sidB].Failure
	if f == nil || f.Code != cel.CodePredicateNotRenderable {
		t.Fatalf("B failure = %+v, want %s", f, cel.CodePredicateNotRenderable)
	}
	if strings.Contains(f.Message, "sss") {
		t.Errorf("the failure quotes the substituted value")
	}
}

// ★ GUARD (decision 4, "for the active task only"). A task of an earlier step is
// re-rendered at every later one and never settled again: its when: would now fail
// against a register written since, and it has already run.
//
// Mutation: settle every task the pass renders → probe2's when: fails at act's step.
func TestFlow_OnlyTheStepBeingDispatchedIsSettled(t *testing.T) {
	m := loadStagedManifest(t, `
name: linear
tasks:
  - name: probe
    module: core.exec.run
    register: probe
    params: { cmd: probe }
  - name: probe2
    module: core.exec.run
    register: late
    when: register.probe.rc == 0
    params: { cmd: probe2 }
  - name: act
    module: core.exec.run
    params: { cmd: act }
`)
	reg := map[string]map[string]any{
		sidA: {"probe": map[string]any{"stdout": "rc vanished"}, "late": map[string]any{"changed": false}},
		sidB: {"probe": map[string]any{"stdout": "rc vanished"}, "late": map[string]any{"changed": false}},
	}
	tasks, plans, err := linearPass(t, m, 2, reg, NewHeldTargets())
	if err != nil {
		t.Fatalf("Render at act's step: %v", err)
	}
	probe2, _ := renderedByName(t, tasks, plans, "probe2")
	if probe2.FlowBySID != nil {
		t.Errorf("an earlier step was settled again: %+v", probe2.FlowBySID)
	}
	if act, _ := renderedByName(t, tasks, plans, "act"); len(act.FlowBySID) != 2 {
		t.Errorf("act FlowBySID = %+v, want both hosts settled", act.FlowBySID)
	}
}

const flowGroup = `
name: linear
tasks:
  - name: probe
    module: core.exec.run
    register: probe
    params: { cmd: probe }
  - name: canary
    serial: 1
    settings: { serial_scope: block }
    block:
      - name: a
        module: core.exec.run
        register: a
        params: { cmd: a }
      - name: b
        module: core.exec.run
        when: register.a.changed
        params: { cmd: b }
      - name: c
        module: core.exec.run
        when: register.probe.stdout == 'yes'
        failed_when: register.self.rc != register.a.rc || register.self.stdout != register.probe.stdout
        params: { cmd: c }
      - name: inner
        block:
          - name: d
            module: core.exec.run
            register: d
            params: { cmd: d }
      - name: e
        module: core.exec.run
        failed_when: register.self.rc != register.d.rc
        params: { cmd: e }
`

// ★ GUARD (decision 4, predicate-rendering.md §4). Inside a group the group's own
// registers stay open: a child's `when:` reading a sibling's register travels as
// written for the Soul to decide, a `when:` reading only earlier steps is decided
// here and a false one travels as `when: false`, and a post-module predicate keeps
// the sibling's register while the earlier step's value is substituted.
//
// Mutations: settle a grouped child's open when: on the Keeper → b fails on both
// hosts (register.a does not exist yet); leave a group's skipped child out of the
// message (no Grouped) → the wire carries no when: false; drop the group's
// registers from the open set → c's failed_when: fails to render.
func TestFlow_GroupKeepsItsOwnRegistersOpen(t *testing.T) {
	m := loadStagedManifest(t, flowGroup)
	reg := probeRegisters(map[string]any{"stdout": "yes"}, map[string]any{"stdout": "no"})
	tasks, plans, err := linearPass(t, m, 1, reg, NewHeldTargets())
	if err != nil {
		t.Fatalf("Render at the group's step: %v", err)
	}
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		if rt, _ := renderedByName(t, tasks, plans, name); !rt.Grouped {
			t.Errorf("%s is not marked grouped", name)
		}
	}
	// A nested block's register is the group's too.
	if e, _ := renderedByName(t, tasks, plans, "e"); e.FlowBySID[sidA].FailedWhen != "register.self.rc != register.d.rc" {
		t.Errorf("e failed_when on A = %+v, want register.d left open", e.FlowBySID[sidA])
	}

	b, _ := renderedByName(t, tasks, plans, "b")
	for _, sid := range []string{sidA, sidB} {
		hf := b.FlowBySID[sid]
		if hf.Failure != nil || hf.Skip || hf.When != "register.a.changed" || hf.Rendered {
			t.Errorf("b on %s = %+v, want the open when: as written", sid, hf)
		}
	}

	c, _ := renderedByName(t, tasks, plans, "c")
	a := c.FlowBySID[sidA]
	if a.Failure != nil || a.Skip || a.When != "" {
		t.Fatalf("c on A = %+v, want it runs with when: decided", a)
	}
	if want := `register.self.rc != register.a.rc || register.self.stdout != dyn("yes")`; a.FailedWhen != want {
		t.Errorf("c failed_when on A = %q, want %q", a.FailedWhen, want)
	}
	if !c.FlowBySID[sidB].Skip {
		t.Errorf("c on B = %+v, want skipped", c.FlowBySID[sidB])
	}
	wireB := ToProtoTasksForHost([]*RenderedTask{c}, sidB)[0]
	if wireB.GetWhen() != "false" || wireB.GetFailedWhen() != "" {
		t.Errorf("c sent to B: when=%q failed_when=%q, want when: false and nothing to evaluate", wireB.GetWhen(), wireB.GetFailedWhen())
	}
}

// Staged dispatch is untouched: nothing is settled, and the predicates travel as
// written with their flow_context.
func TestFlow_StagedDispatchSettlesNothing(t *testing.T) {
	m := loadStagedManifest(t, flowProbeThenCheck)
	plan, perr := config.Stratify(m.Tasks)
	if perr != nil {
		t.Fatalf("Stratify: %v", perr)
	}
	tasks, plans, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), RenderInput{
		Scenario: m, Input: map[string]any{}, Incarnation: IncarnationMeta{ID: "svc"}, Hosts: linearHosts(),
		TaskPassage: plan.TaskPassage,
	})
	if err != nil {
		t.Fatalf("staged Render: %v", err)
	}
	check, _ := renderedByName(t, tasks, plans, "check")
	if check.FlowBySID != nil {
		t.Errorf("staged FlowBySID = %+v, want nil", check.FlowBySID)
	}
	wire := ToProtoTasksForHost([]*RenderedTask{check}, sidB)[0]
	if wire.GetFailedWhen() != check.FailedWhen || wire.GetFlowContext() == nil {
		t.Errorf("staged wire = {failed_when:%q flow_context:%v}, want the author's predicate with its flow_context", wire.GetFailedWhen(), wire.GetFlowContext() != nil)
	}
}
