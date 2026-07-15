package render

import (
	"context"
	"errors"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Guard tests for the block mechanism: INSIDE a destiny pass (ADR-009 amendment
// 2026-06-24, architect a9b54bbf). Synthetic inline destinies via
// stubDestinyResolver — NOT redis. Covers:
//
//	(a) block.when AND-merges with child.when → gated when on the child;
//	(b) a block child's register is visible to an onchanges task OUTSIDE the
//	    block (flat scope);
//	(c) block.when static-false → per-child skip placeholders, the child's
//	    register stays visible outside (flat register scope is invariant under
//	    static-skip); +c2 onchanges outside still resolves, +c3 a register typo
//	    still fails;
//	(d) where/serial/run_once/on/parallel/loop/include/apply on a destiny-block
//	    or its child → ErrUnsupportedDSL;
//	(e) a nested block expands as a cascade with sequential Index.

// blockDestiny — a destiny with one block task inside tasks[].
func blockDestiny(name string, tasks ...config.Task) *ResolvedDestiny {
	return &ResolvedDestiny{
		Name:  name,
		Input: config.InputSchemaMap{"enabled": {Type: "bool", Default: true}},
		Tasks: tasks,
	}
}

// renderBlockDestiny — a common Render run over a scenario with one
// apply:destiny, whose destiny carries the given tasks. Returns the flat plan.
func renderBlockDestiny(t *testing.T, d *ResolvedDestiny, applyInput map[string]any, hosts ...*topology.HostFacts) ([]*RenderedTask, []DispatchPlan, error) {
	t.Helper()
	if len(hosts) == 0 {
		hosts = []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)}
	}
	res := &stubDestinyResolver{resolved: d}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario(d.Name, applyInput),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       hosts,
		Destiny:     res,
	}
	return p.Render(context.Background(), in)
}

// (a) block.when AND-merges with child.when: the child carries
// `(<block>) && (<child>)` as a CEL When string (Keeper doesn't evaluate
// register-dependent when, ADR-012(d)). when here is register-dependent
// (register.probe.*) — NOT static, so it genuinely propagates to the child
// instead of being gated by a placeholder.
func TestRenderDestinyBlock_WhenAndMerge(t *testing.T) {
	inner := moduleTask("restart", "core.service.restarted")
	inner.When = "register.probe.changed"
	block := config.Task{
		Name:  "grp",
		When:  "register.cfg.changed",
		Block: &config.BlockTask{Block: []config.Task{inner}},
	}
	d := blockDestiny("with-block", block)

	tasks, _, err := renderBlockDestiny(t, d, map[string]any{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1 (один block-потомок)", len(tasks))
	}
	want := "(register.cfg.changed) && (register.probe.changed)"
	if tasks[0].When != want {
		t.Errorf("tasks[0].When = %q, want %q (AND-merge block.when ∧ child.when)", tasks[0].When, want)
	}
	if tasks[0].Module != "core.service.restarted" {
		t.Errorf("tasks[0].Module = %q, want core.service.restarted", tasks[0].Module)
	}
}

// (b) a block child's register is visible to an onchanges task OUTSIDE the
// block (case #10). Flat register scope: a task inside the block registers a
// value, a restart task AFTER the block (outside it) refers to that register via
// onchanges → resolveOnChanges maps the name to the source's Index inside the
// block.
func TestRenderDestinyBlock_FlatRegisterScope(t *testing.T) {
	probe := moduleTask("probe", "core.exec.run")
	probe.Register = "tls_changed"
	block := config.Task{
		Name:  "tls-grp",
		Block: &config.BlockTask{Block: []config.Task{probe}},
	}
	restart := moduleTask("restart", "core.service.restarted")
	restart.OnChanges = []string{"tls_changed"} // reference to a register INSIDE the block.
	d := blockDestiny("tls-flat", block, restart)

	tasks, _, err := renderBlockDestiny(t, d, map[string]any{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2 (block-потомок probe + restart снаружи)", len(tasks))
	}
	if tasks[0].Register != "tls_changed" {
		t.Errorf("tasks[0].Register = %q, want tls_changed (потомок block виден в плоском плане)", tasks[0].Register)
	}
	if len(tasks[1].OnChangesIdx) != 1 || tasks[1].OnChangesIdx[0] != 0 {
		t.Errorf("restart.OnChangesIdx = %v, want [0] — register потомка block виден onchanges СНАРУЖИ block", tasks[1].OnChangesIdx)
	}
}

// (c) block.when static-false → per-child skip placeholders (NOT one for the
// whole block): block.when merges into each child via AND, each one's
// static-when becomes false, and each child emits ITS OWN placeholder with its
// own register. when is static here (input.enabled — no register/soulprint), so
// at enabled=false each child is gated by its own placeholder. The child's
// register stays VISIBLE outside (flat scope is invariant under static-skip —
// this was a fixed block-static-skip defect).
func TestRenderDestinyBlock_StaticWhenFalse_PerChild(t *testing.T) {
	probe := moduleTask("inner", "core.service.restarted")
	probe.Register = "inner_reg"
	block := config.Task{
		Name:  "grp",
		When:  "input.enabled",
		Block: &config.BlockTask{Block: []config.Task{probe}},
	}
	d := blockDestiny("gated", block)

	tasks, plans, err := renderBlockDestiny(t, d, map[string]any{"enabled": false})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 1 || len(plans) != 1 {
		t.Fatalf("len(tasks)=%d plans=%d, want 1/1 (per-потомок placeholder)", len(tasks), len(plans))
	}
	if tasks[0].FlowContext == nil {
		t.Errorf("placeholder должен нести FlowContext (static-skip)")
	}
	if tasks[0].Register != "inner_reg" {
		t.Errorf("tasks[0].Register = %q, want inner_reg — register потомка виден на skip-placeholder", tasks[0].Register)
	}
	if tasks[0].Params != nil {
		t.Errorf("tasks[0].Params = %v, want nil (static-skip placeholder)", tasks[0].Params)
	}
	if len(plans[0].TargetSIDs) != 0 {
		t.Errorf("placeholder.TargetSIDs = %v, want пусто (не диспатчится)", plans[0].TargetSIDs)
	}
}

// (c2) static-false destiny-block + onchanges OUTSIDE on the child's register
// (case #10, mirrors scenario): render does NOT fail with
// ErrOnChangesUnknownRegister, the child's register resolves into OnChangesIdx
// on the task after the block. This is the latent block-static-skip defect that
// was fixed in the destiny layer.
func TestRenderDestinyBlock_StaticWhenFalse_PreservesRegister(t *testing.T) {
	probe := moduleTask("probe", "core.exec.run")
	probe.Register = "tls_changed"
	block := config.Task{
		Name:  "tls-grp",
		When:  "input.enabled",
		Block: &config.BlockTask{Block: []config.Task{probe}},
	}
	restart := moduleTask("restart", "core.service.restarted")
	restart.OnChanges = []string{"tls_changed"}
	d := blockDestiny("tls-gated", block, restart)

	tasks, _, err := renderBlockDestiny(t, d, map[string]any{"enabled": false})
	if err != nil {
		t.Fatalf("Render НЕ должен падать на register потомка static-false destiny-block: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2 (skip-placeholder потомка + restart)", len(tasks))
	}
	if tasks[0].Register != "tls_changed" {
		t.Errorf("tasks[0].Register = %q, want tls_changed (register потомка виден через skip-placeholder)", tasks[0].Register)
	}
	if len(tasks[1].OnChangesIdx) != 1 || tasks[1].OnChangesIdx[0] != 0 {
		t.Errorf("restart.OnChangesIdx = %v, want [0] — register потомка static-false destiny-block резолвится снаружи", tasks[1].OnChangesIdx)
	}
}

// (c3, negative) onchanges on a KNOWN-nonexistent register (a typo) STILL fails
// with ErrOnChangesUnknownRegister — the block-static-skip fix did NOT weaken
// validation. Guarantees that expanding a static-false block into per-child
// placeholders doesn't mask register-id typos.
func TestRenderDestinyBlock_StaticWhenFalse_UnknownRegisterStillFails(t *testing.T) {
	probe := moduleTask("probe", "core.exec.run")
	probe.Register = "tls_changed"
	block := config.Task{
		Name:  "tls-grp",
		When:  "input.enabled",
		Block: &config.BlockTask{Block: []config.Task{probe}},
	}
	restart := moduleTask("restart", "core.service.restarted")
	restart.OnChanges = []string{"typo_changed"} // typo: no such register exists
	d := blockDestiny("tls-typo", block, restart)

	_, _, err := renderBlockDestiny(t, d, map[string]any{"enabled": false})
	if err == nil {
		t.Fatal("Render: ожидалась ошибка на несуществующий onchanges register, got nil")
	}
	if !errors.Is(err, ErrOnChangesUnknownRegister) {
		t.Errorf("err = %v, want ErrOnChangesUnknownRegister (валидация не ослаблена static-skip)", err)
	}
}

// (d) scenario orchestration on a destiny-block OR its child → ErrUnsupportedDSL.
// A render-layer key boundary (the config layer is shared and these keys are
// valid there).
func TestRenderDestinyBlock_RejectsScenarioKeys(t *testing.T) {
	leaf := func() config.Task { return moduleTask("leaf", "core.exec.run") }

	cases := []struct {
		name  string
		block config.Task
	}{
		{"where_on_block", config.Task{Name: "g", Where: "soulprint.self.os.family=='debian'", Block: &config.BlockTask{Block: []config.Task{leaf()}}}},
		{"serial_on_block", config.Task{Name: "g", Serial: 2, Block: &config.BlockTask{Block: []config.Task{leaf()}}}},
		{"run_once_on_block", config.Task{Name: "g", RunOnce: true, Block: &config.BlockTask{Block: []config.Task{leaf()}}}},
		{"on_on_block", config.Task{Name: "g", On: []string{"svc"}, Block: &config.BlockTask{Block: []config.Task{leaf()}}}},
		{"parallel_on_block", config.Task{Name: "g", Parallel: true, Block: &config.BlockTask{Block: []config.Task{leaf()}}}},
		{"loop_on_block", config.Task{Name: "g", Loop: &config.LoopSpec{Items: "${ [1,2] }"}, Block: &config.BlockTask{Block: []config.Task{leaf()}}}},
		{"where_on_child", config.Task{Name: "g", Block: &config.BlockTask{Block: []config.Task{withWhere(leaf(), "soulprint.self.os.family=='debian'")}}}},
		{"serial_on_child", config.Task{Name: "g", Block: &config.BlockTask{Block: []config.Task{withSerial(leaf(), 2)}}}},
		{"run_once_on_child", config.Task{Name: "g", Block: &config.BlockTask{Block: []config.Task{withRunOnce(leaf())}}}},
		{"on_on_child", config.Task{Name: "g", Block: &config.BlockTask{Block: []config.Task{withOn(leaf(), []string{"svc"})}}}},
		{"parallel_on_child", config.Task{Name: "g", Block: &config.BlockTask{Block: []config.Task{withParallel(leaf())}}}},
		{"loop_on_child", config.Task{Name: "g", Block: &config.BlockTask{Block: []config.Task{withLoop(leaf())}}}},
		{"apply_on_child", config.Task{Name: "g", Block: &config.BlockTask{Block: []config.Task{{Name: "ap", Apply: &config.ApplyTask{Destiny: "other"}}}}}},
		{"include_on_child", config.Task{Name: "g", Block: &config.BlockTask{Block: []config.Task{{Name: "inc", Include: &config.IncludeTask{Include: "x.yml"}}}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := blockDestiny("rej", tc.block)
			_, _, err := renderBlockDestiny(t, d, map[string]any{})
			// include gives ErrUnexpandedInclude (an expansion bug), everything
			// else gives ErrUnsupportedDSL. Both are rejections; we check the
			// specific family.
			wantInclude := tc.name == "include_on_child"
			switch {
			case wantInclude && !errors.Is(err, ErrUnexpandedInclude):
				t.Fatalf("err = %v, want ErrUnexpandedInclude", err)
			case !wantInclude && !errors.Is(err, ErrUnsupportedDSL):
				t.Fatalf("err = %v, want ErrUnsupportedDSL (scenario-ключ %s)", err, tc.name)
			}
		})
	}
}

// (e) a nested block expands as a cascade: outer when ∧ inner block-when ∧
// leaf-when → a triple AND on the leaf; Index is sequential.
func TestRenderDestinyBlock_Nested(t *testing.T) {
	leaf := moduleTask("leaf", "core.exec.run")
	leaf.When = "register.c.changed"
	innerBlock := config.Task{
		Name:  "inner",
		When:  "register.b.changed",
		Block: &config.BlockTask{Block: []config.Task{leaf}},
	}
	sibling := moduleTask("sib", "core.exec.run")
	outerBlock := config.Task{
		Name:  "outer",
		When:  "register.a.changed",
		Block: &config.BlockTask{Block: []config.Task{sibling, innerBlock}},
	}
	d := blockDestiny("nested", outerBlock)

	tasks, _, err := renderBlockDestiny(t, d, map[string]any{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2 (sibling + leaf вложенного block)", len(tasks))
	}
	if tasks[0].Index != 0 || tasks[1].Index != 1 {
		t.Errorf("indices = %d,%d, want 0,1 (сквозные)", tasks[0].Index, tasks[1].Index)
	}
	// sibling: only the outer when.
	if tasks[0].When != "register.a.changed" {
		t.Errorf("sibling.When = %q, want register.a.changed", tasks[0].When)
	}
	// leaf: outer ∧ inner ∧ leaf. Cascade: outer merges into the inner block
	// (→ (a)&&(b)), then the result merges into the leaf (→ ((a)&&(b))&&(c)).
	want := "((register.a.changed) && (register.b.changed)) && (register.c.changed)"
	if tasks[1].When != want {
		t.Errorf("leaf.When = %q, want %q (тройной AND каскадом)", tasks[1].When, want)
	}
}

// Mutator helpers for matrix (d): return a task copy with one scenario key set.
func withWhere(t config.Task, w string) config.Task { t.Where = w; return t }
func withSerial(t config.Task, s int) config.Task   { t.Serial = s; return t }
func withRunOnce(t config.Task) config.Task         { t.RunOnce = true; return t }
func withOn(t config.Task, on []string) config.Task { t.On = on; return t }
func withParallel(t config.Task) config.Task        { t.Parallel = true; return t }
func withLoop(t config.Task) config.Task {
	t.Loop = &config.LoopSpec{Items: "${ [1,2] }"}
	return t
}
