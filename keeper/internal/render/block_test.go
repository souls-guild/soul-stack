package render

import (
	"context"
	"errors"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// moduleTask is a short helper: a module task with empty params.
func moduleTask(name, module string) config.Task {
	return config.Task{Name: name, Module: &config.ModuleTask{Module: module, Params: map[string]any{}}}
}

// renderBlock runs Render over a single block scenario on the given hosts.
func renderBlock(t *testing.T, task config.Task, hosts []*topology.HostFacts, input ...map[string]any) ([]*RenderedTask, []DispatchPlan) {
	t.Helper()
	var in0 map[string]any
	if len(input) > 0 {
		in0 = input[0]
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    &config.ScenarioManifest{Name: "s", Tasks: []config.Task{task}},
		Input:       in0,
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       hosts,
	}
	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return tasks, plans
}

// TestRenderBlock_WhenInheritance (guard #1) — block.when + child.when
// AND-merges into `(<block.when>) && (<child.when>)` in each child's
// RenderedTask.When. block.when is carried AS a CEL string (Soul evaluates
// it), so we check the predicate text, not the outcome.
func TestRenderBlock_WhenInheritance(t *testing.T) {
	task := config.Task{
		Name: "grp",
		When: "input.action == 'apply'",
		Block: &config.BlockTask{Block: []config.Task{
			func() config.Task {
				tk := moduleTask("inner", "core.exec.run")
				tk.When = "register.x.changed"
				return tk
			}(),
			moduleTask("inner2", "core.exec.run"),
		}},
	}
	tasks, _ := renderBlock(t, task, []*topology.HostFacts{host("a", []string{"svc"}, nil)}, map[string]any{"action": "apply"})
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2", len(tasks))
	}
	if got, want := tasks[0].When, "(input.action == 'apply') && (register.x.changed)"; got != want {
		t.Errorf("child[0].When = %q, want %q", got, want)
	}
	// A child without its own when inherits plain block.when.
	if got, want := tasks[1].When, "input.action == 'apply'"; got != want {
		t.Errorf("child[1].When = %q, want %q", got, want)
	}
}

// TestRenderBlock_WhereInheritance (guard #2) — block.where applies to all
// children: each gets the same TargetSIDs (where: resolved on the child with
// the inherited predicate). where: uses stable soulprint.self.sid (no
// register — no staged probe required).
func TestRenderBlock_WhereInheritance(t *testing.T) {
	hosts := []*topology.HostFacts{
		host("a.example.com", []string{"svc"}, map[string]any{"sid": "a.example.com"}),
		host("b.example.com", []string{"svc"}, map[string]any{"sid": "b.example.com"}),
	}
	task := config.Task{
		Name:  "grp",
		Where: "soulprint.self.sid == 'a.example.com'",
		Block: &config.BlockTask{Block: []config.Task{
			moduleTask("inner1", "core.exec.run"),
			moduleTask("inner2", "core.exec.run"),
		}},
	}
	_, plans := renderBlock(t, task, hosts)
	if len(plans) != 2 {
		t.Fatalf("len(plans) = %d, want 2", len(plans))
	}
	for i, pl := range plans {
		if len(pl.TargetSIDs) != 1 || pl.TargetSIDs[0] != "a.example.com" {
			t.Errorf("plans[%d].TargetSIDs = %v, want [a.example.com]", i, pl.TargetSIDs)
		}
	}
}

// TestRenderBlock_WhereAndMerge — block.where AND child.where: both narrow the
// target (intersection). Of two hosts, block.where selects both, child.where
// narrows to one.
func TestRenderBlock_WhereAndMerge(t *testing.T) {
	hosts := []*topology.HostFacts{
		host("a.example.com", []string{"svc"}, map[string]any{"sid": "a.example.com"}),
		host("b.example.com", []string{"svc"}, map[string]any{"sid": "b.example.com"}),
	}
	inner := moduleTask("inner", "core.exec.run")
	inner.Where = "soulprint.self.sid == 'a.example.com'"
	task := config.Task{
		Name:  "grp",
		Where: "soulprint.self.sid != 'c.example.com'", // both hosts pass
		Block: &config.BlockTask{Block: []config.Task{inner}},
	}
	_, plans := renderBlock(t, task, hosts)
	if len(plans) != 1 {
		t.Fatalf("len(plans) = %d, want 1", len(plans))
	}
	if len(plans[0].TargetSIDs) != 1 || plans[0].TargetSIDs[0] != "a.example.com" {
		t.Errorf("TargetSIDs = %v, want [a.example.com] (AND-merge block.where && child.where)", plans[0].TargetSIDs)
	}
}

// TestRenderBlock_SerialInheritance (guard #3, render part) — block.serial: 1
// on 3 hosts → each child carries SerialWidth=1 in DispatchPlan. The
// integration part (splitWaves+groupByHost: a host's ApplyRequest carries ALL
// block children, 3 waves of 1) is checked in the scenario package
// (TestDispatch_BlockSerialWave).
func TestRenderBlock_SerialInheritance(t *testing.T) {
	hosts := []*topology.HostFacts{
		host("a.example.com", []string{"svc"}, nil),
		host("b.example.com", []string{"svc"}, nil),
		host("c.example.com", []string{"svc"}, nil),
	}
	task := config.Task{
		Name:   "grp",
		Serial: 1,
		Block: &config.BlockTask{Block: []config.Task{
			moduleTask("inner1", "core.exec.run"),
			moduleTask("inner2", "core.exec.run"),
		}},
	}
	_, plans := renderBlock(t, task, hosts)
	if len(plans) != 2 {
		t.Fatalf("len(plans) = %d, want 2", len(plans))
	}
	for i, pl := range plans {
		if pl.SerialWidth != 1 {
			t.Errorf("plans[%d].SerialWidth = %d, want 1", i, pl.SerialWidth)
		}
		if len(pl.TargetSIDs) != 3 {
			t.Errorf("plans[%d].TargetSIDs = %v, want all 3 hosts", i, pl.TargetSIDs)
		}
	}
}

// TestRenderBlock_RequisitesInheritance (guard #4) — block.onchanges carries
// through to each child; resolveOnChanges resolves the register name to the
// source's Index.
func TestRenderBlock_RequisitesInheritance(t *testing.T) {
	probe := moduleTask("probe", "core.exec.run")
	probe.Register = "cfg"
	grp := config.Task{
		Name:      "grp",
		OnChanges: []string{"cfg"},
		Block: &config.BlockTask{Block: []config.Task{
			moduleTask("inner1", "core.service.restarted"),
			moduleTask("inner2", "core.service.restarted"),
		}},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    &config.ScenarioManifest{Name: "s", Tasks: []config.Task{probe, grp}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("len(tasks) = %d, want 3 (probe + 2 block children)", len(tasks))
	}
	for _, ti := range []int{1, 2} {
		if len(tasks[ti].OnChangesIdx) != 1 || tasks[ti].OnChangesIdx[0] != 0 {
			t.Errorf("tasks[%d].OnChangesIdx = %v, want [0] (resolved register cfg → index 0)", ti, tasks[ti].OnChangesIdx)
		}
	}
}

// TestRenderBlock_RequisitesUnion — block.onchanges + child.onchanges union in
// the child (union of names → union of indices after resolve).
func TestRenderBlock_RequisitesUnion(t *testing.T) {
	probeA := moduleTask("probeA", "core.exec.run")
	probeA.Register = "a_reg"
	probeB := moduleTask("probeB", "core.exec.run")
	probeB.Register = "b_reg"
	inner := moduleTask("inner", "core.service.restarted")
	inner.OnChanges = []string{"b_reg"}
	grp := config.Task{
		Name:      "grp",
		OnChanges: []string{"a_reg"},
		Block:     &config.BlockTask{Block: []config.Task{inner}},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    &config.ScenarioManifest{Name: "s", Tasks: []config.Task{probeA, probeB, grp}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// inner is index 2; sources a_reg=0, b_reg=1 → OnChangesIdx contains both.
	got := tasks[2].OnChangesIdx
	if len(got) != 2 {
		t.Fatalf("inner.OnChangesIdx = %v, want union of 2 (block a_reg + child b_reg)", got)
	}
	seen := map[int]bool{got[0]: true, got[1]: true}
	if !seen[0] || !seen[1] {
		t.Errorf("inner.OnChangesIdx = %v, want {0,1}", got)
	}
}

// TestRenderBlock_NestedRecursion (guard #5) — block-in-block expands with a
// threaded Index, inheritance cascades (outer when + inner block-when + leaf
// when → triple AND).
func TestRenderBlock_NestedRecursion(t *testing.T) {
	leaf := moduleTask("leaf", "core.exec.run")
	leaf.When = "input.c"
	task := config.Task{
		Name: "outer",
		When: "input.a",
		Block: &config.BlockTask{Block: []config.Task{
			moduleTask("sibling", "core.exec.run"),
			{
				Name:  "inner",
				When:  "input.b",
				Block: &config.BlockTask{Block: []config.Task{leaf}},
			},
		}},
	}
	tasks, _ := renderBlock(t, task, []*topology.HostFacts{host("a", []string{"svc"}, nil)},
		map[string]any{"a": true, "b": true, "c": true})
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2 (sibling + leaf)", len(tasks))
	}
	if tasks[0].Index != 0 || tasks[1].Index != 1 {
		t.Errorf("indices = %d,%d, want 0,1 (сквозные)", tasks[0].Index, tasks[1].Index)
	}
	// sibling: outer.when only.
	if got, want := tasks[0].When, "input.a"; got != want {
		t.Errorf("sibling.When = %q, want %q", got, want)
	}
	// leaf: outer.when && inner.when && leaf.when (cascade; the nested merge
	// wraps the already-merged outer predicate in its own parens).
	if got, want := tasks[1].When, "((input.a) && (input.b)) && (input.c)"; got != want {
		t.Errorf("leaf.When = %q, want %q (каскад outer&&inner&&leaf)", got, want)
	}
}

// TestRenderBlock_StaticSkipPerChild (guard #6) — a block-level static
// when:false expands into per-child skip placeholders (NOT one for the whole
// block): block.when is ANDed into each child, each child's static-when
// becomes false, and each child emits its OWN placeholder. Index of
// subsequent tasks doesn't shift (placeholder count == child count).
func TestRenderBlock_StaticSkipPerChild(t *testing.T) {
	grp := config.Task{
		Name: "grp",
		When: "input.action == 'apply'", // static, false for any other action
		Block: &config.BlockTask{Block: []config.Task{
			moduleTask("inner1", "core.exec.run"),
			moduleTask("inner2", "core.exec.run"),
			moduleTask("inner3", "core.exec.run"),
		}},
	}
	after := moduleTask("after", "core.exec.run")
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    &config.ScenarioManifest{Name: "s", Tasks: []config.Task{grp, after}},
		Input:       map[string]any{"action": "diagnose"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// 3 per-child placeholders + 1 after task = 4.
	if len(tasks) != 4 {
		t.Fatalf("len(tasks) = %d, want 4 (3 per-child placeholder + after)", len(tasks))
	}
	for i := 0; i < 3; i++ {
		if tasks[i].Params != nil {
			t.Errorf("placeholder[%d] Params = %v, want nil (skip)", i, tasks[i].Params)
		}
		// Inherited block.when is ANDed into each child (AND with empty
		// child.when → plain block.when).
		if got, want := tasks[i].When, "input.action == 'apply'"; got != want {
			t.Errorf("placeholder[%d].When = %q, want %q (унаследованный block.when)", i, got, want)
		}
	}
	// after didn't shift: Index 3.
	if tasks[3].Index != 3 || tasks[3].Module != "core.exec.run" {
		t.Errorf("after = {Index:%d Module:%q}, want {3 core.exec.run}", tasks[3].Index, tasks[3].Module)
	}
	for i := range tasks {
		if tasks[i].Index != i {
			t.Errorf("tasks[%d].Index = %d, want %d (сквозной без дыр)", i, tasks[i].Index, i)
		}
	}
	if len(plans) != 4 {
		t.Errorf("len(plans) = %d, want 4", len(plans))
	}
}

// TestRenderBlock_StaticSkipPreservesRegister (guard #6b, case #10) — a
// static-false scenario block with a register-carrying child: the child's
// register is visible FROM OUTSIDE via the skip placeholder, an outside
// onchanges task resolves it in OnChangesIdx, render does NOT fail with
// ErrOnChangesUnknownRegister. This is the latent defect being fixed.
func TestRenderBlock_StaticSkipPreservesRegister(t *testing.T) {
	probe := moduleTask("probe", "core.exec.run")
	probe.Register = "cfg_changed"
	grp := config.Task{
		Name:  "grp",
		When:  "input.action == 'apply'", // static, false for diagnose
		Block: &config.BlockTask{Block: []config.Task{probe}},
	}
	restart := moduleTask("restart", "core.service.restarted")
	restart.OnChanges = []string{"cfg_changed"} // reference to a static-false block child's register
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    &config.ScenarioManifest{Name: "s", Tasks: []config.Task{grp, restart}},
		Input:       map[string]any{"action": "diagnose"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render НЕ должен падать на register потомка static-false block: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2 (skip-placeholder потомка + restart)", len(tasks))
	}
	if tasks[0].Register != "cfg_changed" {
		t.Errorf("tasks[0].Register = %q, want cfg_changed (register потомка виден на skip-placeholder)", tasks[0].Register)
	}
	if tasks[0].Params != nil {
		t.Errorf("tasks[0].Params = %v, want nil (static-skip placeholder)", tasks[0].Params)
	}
	if len(tasks[1].OnChangesIdx) != 1 || tasks[1].OnChangesIdx[0] != 0 {
		t.Errorf("restart.OnChangesIdx = %v, want [0] — register потомка static-false block резолвится снаружи", tasks[1].OnChangesIdx)
	}
}

// TestRenderBlock_StaticSkipNested (guard #6c) — nested static-false block: the
// AND-merge cascade suppresses each leaf child via its own placeholder, the
// leaf's register is visible from outside. Checks the fix holds at any nesting
// depth.
func TestRenderBlock_StaticSkipNested(t *testing.T) {
	leaf := moduleTask("leaf", "core.exec.run")
	leaf.Register = "leaf_reg"
	inner := config.Task{
		Name:  "inner",
		Block: &config.BlockTask{Block: []config.Task{leaf}},
	}
	grp := config.Task{
		Name:  "grp",
		When:  "input.action == 'apply'", // static, false for diagnose
		Block: &config.BlockTask{Block: []config.Task{inner}},
	}
	restart := moduleTask("restart", "core.service.restarted")
	restart.OnChanges = []string{"leaf_reg"}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    &config.ScenarioManifest{Name: "s", Tasks: []config.Task{grp, restart}},
		Input:       map[string]any{"action": "diagnose"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render НЕ должен падать на register листа вложенного static-false block: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2 (skip-placeholder листа + restart)", len(tasks))
	}
	if tasks[0].Register != "leaf_reg" {
		t.Errorf("tasks[0].Register = %q, want leaf_reg (register листа виден через каскад)", tasks[0].Register)
	}
	// AND cascade: outer block.when is merged into inner, then into leaf — wrapped in parens.
	if got, want := tasks[0].When, "input.action == 'apply'"; got != want {
		t.Errorf("leaf placeholder.When = %q, want %q (каскад AND с пустыми child.when → чистый block.when)", got, want)
	}
	if len(tasks[1].OnChangesIdx) != 1 || tasks[1].OnChangesIdx[0] != 0 {
		t.Errorf("restart.OnChangesIdx = %v, want [0] — register листа вложенного static-false block резолвится снаружи", tasks[1].OnChangesIdx)
	}
}

// TestRenderBlock_StaticFalseBlockDynamicChildOperand (flag #10-review) — a
// block with static-false when: + a child with its OWN DYNAMIC operand
// (when: register.cfg.changed). The AND-merge gives `(input.action ==
// 'apply') && (register.cfg.changed)` — the register reference makes the
// string NOT static (isStaticWhen → false via ExtractRegisterRefs), so the
// child is NOT suppressed by static-skip and IS RENDERED (Params != nil): a
// dynamic operand routes processing back into render. Locks in the invariant
// "a child's dynamic operand beats the parent's static-false": the child's
// register is preserved and visible from outside (onchanges resolves it),
// params are rendered, and the AND predicate is passed as-is to Soul (Soul
// evaluates the whole AND, including the false block.when — the child won't
// actually run, but Soul decides, not a render-time skip). Contrast with
// TestRenderBlock_StaticSkipPreservesRegister (there child.when is empty →
// AND = plain static-false → skip placeholder).
func TestRenderBlock_StaticFalseBlockDynamicChildOperand(t *testing.T) {
	child := moduleTask("reload", "core.service.restarted")
	child.Register = "reloaded"
	child.When = "register.cfg.changed" // child's own dynamic operand
	child.Module.Params = map[string]any{"name": "${ input.unit }"}
	grp := config.Task{
		Name:  "grp",
		When:  "input.action == 'apply'", // static-false for diagnose
		Block: &config.BlockTask{Block: []config.Task{child}},
	}
	consumer := moduleTask("consumer", "core.exec.run")
	consumer.OnChanges = []string{"reloaded"} // reference to the child's register from outside
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    &config.ScenarioManifest{Name: "s", Tasks: []config.Task{grp, consumer}},
		Input:       map[string]any{"action": "diagnose", "unit": "redis-server"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: динамический операнд потомка static-false block НЕ должен ронять рендер: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2 (отрендеренный потомок + consumer)", len(tasks))
	}
	// ★ Key invariant: the child IS RENDERED (not a placeholder) because the AND-merge isn't static.
	if tasks[0].Params == nil {
		t.Fatal("tasks[0].Params == nil — динамический операнд должен переводить обработку в РЕНДЕР, а не static-skip")
	}
	if got := tasks[0].Params.GetFields()["name"].GetStringValue(); got != "redis-server" {
		t.Errorf("tasks[0].name = %q, want redis-server (params отрендерены обычным путём)", got)
	}
	// The AND predicate passes through as-is — Soul evaluates the whole AND (incl. the false block.when).
	if got, want := tasks[0].When, "(input.action == 'apply') && (register.cfg.changed)"; got != want {
		t.Errorf("tasks[0].When = %q, want %q (AND-merge протянут as-is на Soul)", got, want)
	}
	// The child's register is preserved and visible from outside: consumer resolves it in OnChangesIdx.
	if tasks[0].Register != "reloaded" {
		t.Errorf("tasks[0].Register = %q, want reloaded (register не теряется при рендере)", tasks[0].Register)
	}
	if len(tasks[1].OnChangesIdx) != 1 || tasks[1].OnChangesIdx[0] != 0 {
		t.Errorf("consumer.OnChangesIdx = %v, want [0] — register отрендеренного потомка виден снаружи", tasks[1].OnChangesIdx)
	}
}

// TestRenderBlock_IndexIntegrity (guard #7) — block fan-out + a subsequent
// task give a threaded, monotonic Index with no gaps.
func TestRenderBlock_IndexIntegrity(t *testing.T) {
	before := moduleTask("before", "core.exec.run")
	grp := config.Task{
		Name: "grp",
		Block: &config.BlockTask{Block: []config.Task{
			moduleTask("inner1", "core.exec.run"),
			moduleTask("inner2", "core.exec.run"),
		}},
	}
	after := moduleTask("after", "core.exec.run")
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    &config.ScenarioManifest{Name: "s", Tasks: []config.Task{before, grp, after}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 4 {
		t.Fatalf("len(tasks) = %d, want 4 (before + 2 block + after)", len(tasks))
	}
	for i := range tasks {
		if tasks[i].Index != i {
			t.Errorf("tasks[%d].Index = %d, want %d (сквозной без дыр)", i, tasks[i].Index, i)
		}
		if plans[i].TaskIndex != i {
			t.Errorf("plans[%d].TaskIndex = %d, want %d", i, plans[i].TaskIndex, i)
		}
	}
}

// TestRenderBlock_VarsInheritance — block.vars is the base, child.vars layers
// on top; the child sees merged vars (via params interpolation).
func TestRenderBlock_VarsInheritance(t *testing.T) {
	inner := config.Task{
		Name: "inner",
		Vars: map[string]any{"b": "child-b"},
		Module: &config.ModuleTask{
			Module: "core.exec.run",
			Params: map[string]any{"command": "${ vars.a } ${ vars.b }"},
		},
	}
	task := config.Task{
		Name:  "grp",
		Vars:  map[string]any{"a": "block-a", "b": "block-b"},
		Block: &config.BlockTask{Block: []config.Task{inner}},
	}
	tasks, _ := renderBlock(t, task, []*topology.HostFacts{host("a", []string{"svc"}, nil)})
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1", len(tasks))
	}
	got := tasks[0].Params.GetFields()["command"].GetStringValue()
	if got != "block-a child-b" {
		t.Errorf("command = %q, want %q (block.vars база, child.vars поверх)", got, "block-a child-b")
	}
}

// TestRenderBlock_ApplyChild — an apply child in a block expands via
// renderApplyDestiny and inherits width. Uses a fixture destiny resolver.
func TestRenderBlock_ApplyChild(t *testing.T) {
	res := &ResolvedDestiny{
		Name:  "d",
		Tasks: []config.Task{moduleTask("d-step", "core.exec.run")},
	}
	task := config.Task{
		Name:   "grp",
		Serial: 1,
		Block: &config.BlockTask{Block: []config.Task{
			{Name: "apply-step", Apply: &config.ApplyTask{Destiny: "d"}},
		}},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    &config.ScenarioManifest{Name: "s", Tasks: []config.Task{task}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts: []*topology.HostFacts{
			host("a.example.com", []string{"svc"}, nil),
			host("b.example.com", []string{"svc"}, nil),
		},
		Destiny: staticResolver{res},
	}
	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1 (one destiny task)", len(tasks))
	}
	if tasks[0].Module != "core.exec.run" {
		t.Errorf("module = %q, want core.exec.run", tasks[0].Module)
	}
	if plans[0].SerialWidth != 1 {
		t.Errorf("SerialWidth = %d, want 1 (унаследован block.serial)", plans[0].SerialWidth)
	}
}

// TestRenderBlock_LoopChildRejected — a loop child in a block is rejected (out of pilot scope).
func TestRenderBlock_LoopChildRejected(t *testing.T) {
	inner := moduleTask("inner", "core.exec.run")
	inner.Loop = &config.LoopSpec{Items: "${ input.xs }"}
	task := config.Task{
		Name:  "grp",
		Block: &config.BlockTask{Block: []config.Task{inner}},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    &config.ScenarioManifest{Name: "s", Tasks: []config.Task{task}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	_, _, err := p.Render(context.Background(), in)
	if !errors.Is(err, ErrUnsupportedDSL) {
		t.Fatalf("err = %v, want ErrUnsupportedDSL (loop в block вне pilot)", err)
	}
}

// TestRenderBlock_IncludeChildRejected (QA gap #10a) — an include child in a
// block is rejected before render (must be expanded by ExpandIncludes
// earlier). The grammar allows include inside a block body, but pilot C1
// doesn't support within-block include (docs/destiny/tasks.md §6.5) —
// guardPilotBlockChild raises ErrUnexpandedInclude.
func TestRenderBlock_IncludeChildRejected(t *testing.T) {
	inner := config.Task{Name: "inc", Include: &config.IncludeTask{Include: "sub.yml"}}
	task := config.Task{
		Name:  "grp",
		Block: &config.BlockTask{Block: []config.Task{inner}},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    &config.ScenarioManifest{Name: "s", Tasks: []config.Task{task}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	_, _, err := p.Render(context.Background(), in)
	if !errors.Is(err, ErrUnexpandedInclude) {
		t.Fatalf("err = %v, want ErrUnexpandedInclude (include-потомок block вне pilot)", err)
	}
}

// TestRenderBlock_ParallelChildRejected (QA gap #10b) — a parallel child in a
// block is rejected (parallel as a whole is deferred post-pilot,
// docs/destiny/tasks.md §6.5).
func TestRenderBlock_ParallelChildRejected(t *testing.T) {
	inner := moduleTask("inner", "core.exec.run")
	inner.Parallel = true
	task := config.Task{
		Name:  "grp",
		Block: &config.BlockTask{Block: []config.Task{inner}},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    &config.ScenarioManifest{Name: "s", Tasks: []config.Task{task}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	_, _, err := p.Render(context.Background(), in)
	if !errors.Is(err, ErrUnsupportedDSL) {
		t.Fatalf("err = %v, want ErrUnsupportedDSL (parallel-потомок block вне pilot)", err)
	}
}

// TestRenderBlock_RunOnceOnBlock (QA gap #7) — ★ run_once: true ON a block task
// narrows the target to ONE host (first by SID) BEFORE fan-out → ALL block
// children run on the SAME host. Critical for a destructive block (failover):
// "exactly one host gets the whole block", not each child resolving run_once
// independently.
func TestRenderBlock_RunOnceOnBlock(t *testing.T) {
	hosts := []*topology.HostFacts{
		host("c.example.com", []string{"svc"}, nil),
		host("a.example.com", []string{"svc"}, nil),
		host("b.example.com", []string{"svc"}, nil),
	}
	task := config.Task{
		Name:    "failover-group",
		RunOnce: true,
		Block: &config.BlockTask{Block: []config.Task{
			moduleTask("step1", "core.exec.run"),
			moduleTask("step2", "core.service.restarted"),
			moduleTask("step3", "core.exec.run"),
		}},
	}
	tasks, plans := renderBlock(t, task, hosts)
	if len(tasks) != 3 {
		t.Fatalf("len(tasks) = %d, want 3 (3 block children)", len(tasks))
	}
	for i, pl := range plans {
		if len(pl.TargetSIDs) != 1 {
			t.Fatalf("plans[%d].TargetSIDs = %v, want ровно 1 хост (run_once на block)", i, pl.TargetSIDs)
		}
		if pl.TargetSIDs[0] != "a.example.com" {
			t.Errorf("plans[%d].TargetSIDs = %v, want [a.example.com] (первый по SID, ОДИН на весь блок)", i, pl.TargetSIDs)
		}
	}
}

// TestRenderBlock_EmptyBlock (QA gap #8) — an empty block (block: []) → 0
// RenderedTask, render succeeds (intentional: an empty group is a no-op, not
// a failure). Pinned as a contract so a regression doesn't turn this into a
// panic (zero fan-out) or a spurious error.
func TestRenderBlock_EmptyBlock(t *testing.T) {
	task := config.Task{
		Name:  "empty-grp",
		Block: &config.BlockTask{Block: []config.Task{}},
	}
	after := moduleTask("after", "core.exec.run")
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    &config.ScenarioManifest{Name: "s", Tasks: []config.Task{task, after}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render пустого block: %v (ожидался no-op, не ошибка)", err)
	}
	// 0 tasks for the empty block + 1 after = 1; after's Index didn't shift.
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1 (пустой block даёт 0 задач + after)", len(tasks))
	}
	if tasks[0].Index != 0 || tasks[0].Name != "after" {
		t.Errorf("tasks[0] = {Index:%d Name:%q}, want {0 after} (пустой block не сдвигает Index)", tasks[0].Index, tasks[0].Name)
	}
	if len(plans) != 1 {
		t.Errorf("len(plans) = %d, want 1", len(plans))
	}
}

// staticResolver is a fixture DestinyResolver for the block-apply test.
type staticResolver struct{ res *ResolvedDestiny }

func (s staticResolver) Resolve(_ context.Context, name string) (*ResolvedDestiny, error) {
	if name != s.res.Name {
		return nil, errors.New("unknown destiny")
	}
	return s.res, nil
}
