package render

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Render under linear dispatch (draft ADR NIM-907, §`passage`, §Rendering, fork
// 3): a default block's children are steps of their own, each gated, its targets
// held from the first step; a group is one step at one width.

const (
	sidA = "a.example.com"
	sidB = "b.example.com"
)

func linearHosts() []*topology.HostFacts {
	return []*topology.HostFacts{
		host(sidA, []string{"svc"}, nil),
		host(sidB, []string{"svc"}, nil),
	}
}

// linearPass renders one step of a linear run: TaskPassage and Steps from
// config.LinearPassage, ActivePassage = active, the given per-host register.
func linearPass(t *testing.T, m *config.ScenarioManifest, active int, reg map[string]map[string]any, held *HeldTargets) ([]*RenderedTask, []DispatchPlan, error) {
	t.Helper()
	plan, steps := config.LinearPassage(m.Tasks, m.Settings)
	in := RenderInput{
		Scenario:       m,
		Input:          map[string]any{},
		Incarnation:    IncarnationMeta{ID: "svc"},
		Hosts:          linearHosts(),
		TaskPassage:    plan.TaskPassage,
		ActivePassage:  active,
		Steps:          steps,
		HeldTargets:    held,
		RegisterByHost: reg,
	}
	return NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), in)
}

func renderedByName(t *testing.T, tasks []*RenderedTask, plans []DispatchPlan, name string) (*RenderedTask, DispatchPlan) {
	t.Helper()
	for _, rt := range tasks {
		if rt.Name != name {
			continue
		}
		for _, pl := range plans {
			if pl.TaskIndex == rt.Index {
				return rt, pl
			}
		}
		t.Fatalf("task %q has no plan", name)
	}
	t.Fatalf("task %q not rendered", name)
	return nil, DispatchPlan{}
}

const linearPair = `
name: linear
tasks:
  - name: probe
    module: core.exec.run
    register: probe
    params: { cmd: probe }
  - name: pair
    block:
      - name: a
        module: core.exec.run
        register: a
        params: { cmd: a }
      - name: b
        module: core.exec.run
        where: register.a.stdout == 'yes'
        params: { cmd: b }
`

// ★ GUARD. While a's step is active, b — whose where: reads a's register — is a
// placeholder carrying its own step, and the render does not fail on a register
// that does not exist yet.
//
// Mutations: drop the per-child gate in walkBlockChildren → b's where: is
// evaluated against an empty register and Render fails (no such key); stamp the
// block's fan-out with the block's passage again → b reads Passage 1, not 2.
func TestRender_LinearDefaultBlockChildIsAPlaceholderUntilItsStep(t *testing.T) {
	m := loadStagedManifest(t, linearPair)
	tasks, plans, err := linearPass(t, m, 1, nil, NewHeldTargets())
	if err != nil {
		t.Fatalf("Render at a's step: %v", err)
	}
	a, aPlan := renderedByName(t, tasks, plans, "a")
	if a.Passage != 1 || a.Params == nil || len(aPlan.TargetSIDs) != 2 {
		t.Errorf("a = {Passage:%d Params:%v Targets:%v}, want step 1, rendered, on both hosts", a.Passage, a.Params != nil, aPlan.TargetSIDs)
	}
	b, bPlan := renderedByName(t, tasks, plans, "b")
	if b.Passage != 2 {
		t.Errorf("b.Passage = %d, want 2 (its own step, not the block's)", b.Passage)
	}
	if b.Params != nil || len(bPlan.TargetSIDs) != 0 {
		t.Errorf("b rendered early: Params=%v Targets=%v, want a placeholder", b.Params != nil, bPlan.TargetSIDs)
	}
}

// At b's step its where: reads a's register per host, like any later step.
func TestRender_LinearDefaultBlockChildReadsEarlierSibling(t *testing.T) {
	m := loadStagedManifest(t, linearPair)
	reg := map[string]map[string]any{
		sidA: {"a": map[string]any{"stdout": "yes"}},
		sidB: {"a": map[string]any{"stdout": "no"}},
	}
	tasks, plans, err := linearPass(t, m, 2, reg, NewHeldTargets())
	if err != nil {
		t.Fatalf("Render at b's step: %v", err)
	}
	b, bPlan := renderedByName(t, tasks, plans, "b")
	if b.Passage != 2 || !reflect.DeepEqual(bPlan.TargetSIDs, []string{sidA}) {
		t.Errorf("b = {Passage:%d Targets:%v}, want step 2 on [%s]", b.Passage, bPlan.TargetSIDs, sidA)
	}
}

// ★ GUARD (fork 3). A default block's where: and run_once: are resolved at its
// first child and held: the second child goes to the hosts the first went to,
// although the register the block's where: reads has changed in between.
//
// Mutations: resolve the block's targets on every pass (no holdTargets) → b loses
// a host; AND the block's where: into each child again (linearOverlayChild keeps
// the merged where) → the same.
func TestRender_LinearDefaultBlockHoldsItsTargets(t *testing.T) {
	cases := []struct {
		name      string
		src       string
		later     map[string]map[string]any
		wantHosts []string
	}{
		{
			// a's register does not exist at the first step, so the block goes to
			// both hosts; at b's step it does, and says no for B.
			name: "where",
			src: `
name: linear
tasks:
  - name: pair
    where: "!has(register.a) || register.a.stdout == 'yes'"
    block:
      - name: a
        module: core.exec.run
        register: a
        params: { cmd: a }
      - name: b
        module: core.exec.run
        params: { cmd: b }
`,
			later: map[string]map[string]any{
				sidA: {"a": map[string]any{"stdout": "yes"}},
				sidB: {"a": map[string]any{"stdout": "no"}},
			},
			wantHosts: []string{sidA, sidB},
		},
		{
			// run_once picks A at the first step; re-resolved at b's step it would
			// pick B, the only host the where: still selects.
			name: "run_once",
			src: `
name: linear
tasks:
  - name: pair
    where: "!has(register.a) || register.a.stdout == 'yes'"
    run_once: true
    block:
      - name: a
        module: core.exec.run
        register: a
        params: { cmd: a }
      - name: b
        module: core.exec.run
        params: { cmd: b }
`,
			later: map[string]map[string]any{
				sidA: {"a": map[string]any{"stdout": "no"}},
				sidB: {"a": map[string]any{"stdout": "yes"}},
			},
			wantHosts: []string{sidA},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := loadStagedManifest(t, tc.src)
			held := NewHeldTargets()
			tasks, plans, err := linearPass(t, m, 0, nil, held)
			if err != nil {
				t.Fatalf("Render at a's step: %v", err)
			}
			if _, aPlan := renderedByName(t, tasks, plans, "a"); !reflect.DeepEqual(aPlan.TargetSIDs, tc.wantHosts) {
				t.Fatalf("a targets = %v, want %v", aPlan.TargetSIDs, tc.wantHosts)
			}
			tasks, plans, err = linearPass(t, m, 1, tc.later, held)
			if err != nil {
				t.Fatalf("Render at b's step: %v", err)
			}
			if _, bPlan := renderedByName(t, tasks, plans, "b"); !reflect.DeepEqual(bPlan.TargetSIDs, tc.wantHosts) {
				t.Errorf("b targets = %v, want %v (the hosts a went to)", bPlan.TargetSIDs, tc.wantHosts)
			}
		})
	}
}

// ★ GUARD. Steps without TaskPassage gates nothing: a caller that renders the
// whole plan in one pass — Trial, the synthetic `core.ssh.apply` render — gets
// every child rendered, as before.
//
// Mutation: drop `in.TaskPassage != nil` from the per-child gate → b becomes a
// placeholder.
func TestRender_LinearStepsWithoutTaskPassageRendersEveryChild(t *testing.T) {
	m := loadStagedManifest(t, linearPair)
	_, steps := config.LinearPassage(m.Tasks, m.Settings)
	in := RenderInput{
		Scenario:    m,
		Input:       map[string]any{},
		Incarnation: IncarnationMeta{ID: "svc"},
		Hosts:       linearHosts(),
		Steps:       steps,
		RegisterByHost: map[string]map[string]any{
			sidA: {"a": map[string]any{"stdout": "yes"}},
			sidB: {"a": map[string]any{"stdout": "yes"}},
		},
	}
	tasks, plans, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	b, bPlan := renderedByName(t, tasks, plans, "b")
	if b.Params == nil || len(bPlan.TargetSIDs) != 2 {
		t.Errorf("b = {Params:%v Targets:%v}, want rendered on both hosts", b.Params != nil, bPlan.TargetSIDs)
	}
}

const linearSerial = `
name: linear
tasks:
  - name: rolled
    serial: 1
    settings:
      serial_scope: task
    block:
      - name: a
        module: core.exec.run
        params: { cmd: a }
      - name: b
        module: core.exec.run
        serial: 2
        params: { cmd: b }
  - name: canary
    serial: 1
    settings:
      serial_scope: block
    block:
      - name: c
        module: core.exec.run
        serial: 2
        params: { cmd: c }
      - name: nested
        serial: 3
        block:
          - name: d
            module: core.exec.run
            params: { cmd: d }
`

func serialWidths(t *testing.T, tasks []*RenderedTask, plans []DispatchPlan) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, name := range []string{"a", "b", "c", "d"} {
		_, pl := renderedByName(t, tasks, plans, name)
		out[name] = pl.SerialWidth
	}
	return out
}

// ★ GUARD (fork 3, settings.md §1). In a default block the block's serial: is
// each child's default and a child's own overrides it; in a group the group's
// width is every descendant's, a nested block's serial: included. Staged
// dispatch keeps today's widths.
//
// Mutations: give a default block's children the block's width → b rolls at 1;
// let a block inside a group take its own width → d rolls at 3.
func TestRender_LinearSerialByBlockMode(t *testing.T) {
	m := loadStagedManifest(t, linearSerial)
	hosts := []*topology.HostFacts{
		host("h1.example.com", []string{"svc"}, nil),
		host("h2.example.com", []string{"svc"}, nil),
		host("h3.example.com", []string{"svc"}, nil),
		host("h4.example.com", []string{"svc"}, nil),
	}
	render := func(steps []config.StepNode) map[string]int {
		in := RenderInput{Scenario: m, Input: map[string]any{}, Incarnation: IncarnationMeta{ID: "svc"}, Hosts: hosts, Steps: steps}
		tasks, plans, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), in)
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		return serialWidths(t, tasks, plans)
	}

	_, steps := config.LinearPassage(m.Tasks, m.Settings)
	if got, want := render(steps), map[string]int{"a": 1, "b": 2, "c": 1, "d": 1}; !reflect.DeepEqual(got, want) {
		t.Errorf("linear widths = %v, want %v", got, want)
	}
	if got, want := render(nil), map[string]int{"a": 1, "b": 1, "c": 1, "d": 3}; !reflect.DeepEqual(got, want) {
		t.Errorf("staged widths = %v, want %v (unchanged)", got, want)
	}
}

// ★ GUARD. An applier inside a default block is one step: its destiny — a block
// of its own included — renders whole at that step and every destiny task carries
// the applier's step (decision 6: a destiny is unchanged).
//
// Mutation: drop the stamp after the apply branch in walkBlockChildren → the
// destiny tasks read Passage 0 and would be dispatched with the first step.
func TestRender_LinearApplierChildIsOneStep(t *testing.T) {
	res := &ResolvedDestiny{
		Name: "d",
		Tasks: []config.Task{{
			Name: "inside",
			Block: &config.BlockTask{Block: []config.Task{
				moduleTask("d1", "core.exec.run"),
				moduleTask("d2", "core.exec.run"),
			}},
		}},
	}
	m := &config.ScenarioManifest{Name: "s", Tasks: []config.Task{{
		Name: "pair",
		Block: &config.BlockTask{Block: []config.Task{
			moduleTask("first", "core.exec.run"),
			{Name: "apply-step", Apply: &config.ApplyTask{Destiny: "d"}},
		}},
	}}}
	plan, steps := config.LinearPassage(m.Tasks, m.Settings)
	in := RenderInput{
		Scenario:      m,
		Incarnation:   IncarnationMeta{ID: "svc"},
		Hosts:         linearHosts(),
		Destiny:       staticResolver{res},
		TaskPassage:   plan.TaskPassage,
		ActivePassage: 1,
		Steps:         steps,
		HeldTargets:   NewHeldTargets(),
	}
	tasks, plans, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, name := range []string{"d1", "d2"} {
		rt, pl := renderedByName(t, tasks, plans, name)
		if rt.Passage != 1 || rt.Params == nil || len(pl.TargetSIDs) != 2 {
			t.Errorf("%s = {Passage:%d Params:%v Targets:%v}, want step 1, rendered, on both hosts", name, rt.Passage, rt.Params != nil, pl.TargetSIDs)
		}
	}
	if first, _ := renderedByName(t, tasks, plans, "first"); first.Passage != 0 {
		t.Errorf("first.Passage = %d, want 0", first.Passage)
	}
}

// ★ GUARD (fork 3, "at any depth"). A nested default block holds its hosts too:
// its where: reads its own first child's register, which exists only from that
// child's step on, and its second child still goes where the first went.
//
// Mutation: hold only the top-level block (no holdTargets for a nested one) → z
// loses host B.
func TestRender_LinearNestedDefaultBlockHoldsItsTargets(t *testing.T) {
	m := loadStagedManifest(t, `
name: linear
tasks:
  - name: outer
    block:
      - name: x
        module: core.exec.run
        params: { cmd: x }
      - name: inner
        where: "!has(register.y) || register.y.stdout == 'yes'"
        block:
          - name: y
            module: core.exec.run
            register: y
            params: { cmd: y }
          - name: z
            module: core.exec.run
            params: { cmd: z }
`)
	held := NewHeldTargets()
	for active := 0; active <= 1; active++ {
		if _, _, err := linearPass(t, m, active, nil, held); err != nil {
			t.Fatalf("Render at step %d: %v", active, err)
		}
	}
	reg := map[string]map[string]any{
		sidA: {"y": map[string]any{"stdout": "yes"}},
		sidB: {"y": map[string]any{"stdout": "no"}},
	}
	tasks, plans, err := linearPass(t, m, 2, reg, held)
	if err != nil {
		t.Fatalf("Render at z's step: %v", err)
	}
	if z, zPlan := renderedByName(t, tasks, plans, "z"); z.Passage != 2 || !reflect.DeepEqual(zPlan.TargetSIDs, []string{sidA, sidB}) {
		t.Errorf("z = {Passage:%d Targets:%v}, want step 2 on both hosts (what y went to)", z.Passage, zPlan.TargetSIDs)
	}
}

// ★ GUARD. A group nested in a default block is one step: every task in it carries
// that step and rolls at the group's width — its own serial: or, as here, the one
// it inherits from the default block above it.
//
// Mutations: leave the nested group's tasks unstamped → they read Passage 0 and
// step 1 dispatches nothing; roll the nested group at width 0 → the canary goes to
// every host at once.
func TestRender_LinearNestedGroupIsOneStepAtItsWidth(t *testing.T) {
	m := loadStagedManifest(t, `
name: linear
tasks:
  - name: outer
    serial: 1
    settings:
      serial_scope: task
    block:
      - name: x
        module: core.exec.run
        params: { cmd: x }
      - name: canary
        settings:
          serial_scope: block
        block:
          - name: g1
            module: core.exec.run
            serial: 2
            params: { cmd: g1 }
          - name: g2
            module: core.exec.run
            params: { cmd: g2 }
`)
	tasks, plans, err := linearPass(t, m, 1, nil, NewHeldTargets())
	if err != nil {
		t.Fatalf("Render at the group's step: %v", err)
	}
	for _, name := range []string{"g1", "g2"} {
		rt, pl := renderedByName(t, tasks, plans, name)
		if rt.Passage != 1 || pl.SerialWidth != 1 || len(pl.TargetSIDs) != 2 {
			t.Errorf("%s = {Passage:%d Width:%d Targets:%v}, want step 1 at the group's width 1 on both hosts", name, rt.Passage, pl.SerialWidth, pl.TargetSIDs)
		}
	}
}

// ★ GUARD. A child a static when: switches off keeps its own step: its skip
// placeholder is emitted while an earlier step is active — it reads no register —
// and must be stamped with its step, not the block's.
//
// Mutation: drop the stamp after the static skip → the placeholder reads Passage 0
// and is persisted and dispatched with the first step.
func TestRender_LinearStaticSkipChildKeepsItsStep(t *testing.T) {
	m := loadStagedManifest(t, `
name: linear
tasks:
  - name: probe
    module: core.exec.run
    params: { cmd: probe }
  - name: pair
    block:
      - name: a
        module: core.exec.run
        params: { cmd: a }
      - name: off
        module: core.exec.run
        when: "false"
        params: { cmd: off }
`)
	for active := 0; active <= 2; active++ {
		tasks, _, err := linearPass(t, m, active, nil, NewHeldTargets())
		if err != nil {
			t.Fatalf("Render at step %d: %v", active, err)
		}
		for _, rt := range tasks {
			if rt.Name == "off" && rt.Passage != 2 {
				t.Errorf("at step %d: off.Passage = %d, want 2", active, rt.Passage)
			}
		}
	}
}

// A step shape built from another task list is refused, not indexed: a block's
// node list shorter than its children would panic, and a top-level one would
// silently render the extra tasks staged.
//
// Mutation: drop the length checks → no error.
func TestRender_LinearStepShapeMustMatchThePlan(t *testing.T) {
	m := loadStagedManifest(t, linearPair)
	_, steps := config.LinearPassage(m.Tasks, m.Settings)
	cases := map[string][]config.StepNode{
		"top level short": steps[:1],
		"block short":     {steps[0], {Step: steps[1].Step, Children: steps[1].Children[:1]}},
		"block childless": {steps[0], {Step: steps[1].Step}},
	}
	// a's register is there, so nothing but the shape can fail the render.
	reg := map[string]map[string]any{
		sidA: {"a": map[string]any{"stdout": "yes"}},
		sidB: {"a": map[string]any{"stdout": "yes"}},
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			in := RenderInput{Scenario: m, Input: map[string]any{}, Incarnation: IncarnationMeta{ID: "svc"}, Hosts: linearHosts(), Steps: bad, RegisterByHost: reg}
			_, _, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), in)
			if err == nil || !strings.Contains(err.Error(), "step shape") {
				t.Errorf("Render error = %v, want the step shape refused", err)
			}
		})
	}
	in := RenderInput{Scenario: m, Input: map[string]any{}, Incarnation: IncarnationMeta{ID: "svc"}, Hosts: linearHosts(), Steps: steps, RegisterByHost: reg}
	if _, _, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), in); err != nil {
		t.Fatalf("Render with the matching shape: %v", err)
	}
}
