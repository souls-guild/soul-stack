package render

import (
	"context"
	"errors"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// moduleTask — короткий хелпер: module-задача с пустыми params.
func moduleTask(name, module string) config.Task {
	return config.Task{Name: name, Module: &config.ModuleTask{Module: module, Params: map[string]any{}}}
}

// renderBlock — общий прогон Render над одним block-сценарием на заданных хостах.
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

// TestRenderBlock_WhenInheritance (guard #1) — block.when + child.when даёт
// AND-merge `(<block.when>) && (<child.when>)` в RenderedTask.When каждого
// потомка. Block.when протягивается КАК CEL-строка (Soul вычисляет), поэтому
// проверяем текст предиката, а не исход.
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
	// Потомок без своего when: наследует чистый block.when.
	if got, want := tasks[1].When, "input.action == 'apply'"; got != want {
		t.Errorf("child[1].When = %q, want %q", got, want)
	}
}

// TestRenderBlock_WhereInheritance (guard #2) — block.where применяется ко всем
// потомкам: каждый получает одинаковый TargetSIDs (резолв where: на потомке с
// унаследованным предикатом). where: по стабильному soulprint.self.sid (без
// register — не требует staged probe).
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

// TestRenderBlock_WhereAndMerge — block.where AND child.where: оба сужают таргет
// (пересечение). На двух хостах block.where отбирает оба, child.where сужает до
// одного.
func TestRenderBlock_WhereAndMerge(t *testing.T) {
	hosts := []*topology.HostFacts{
		host("a.example.com", []string{"svc"}, map[string]any{"sid": "a.example.com"}),
		host("b.example.com", []string{"svc"}, map[string]any{"sid": "b.example.com"}),
	}
	inner := moduleTask("inner", "core.exec.run")
	inner.Where = "soulprint.self.sid == 'a.example.com'"
	task := config.Task{
		Name:  "grp",
		Where: "soulprint.self.sid != 'c.example.com'", // оба хоста проходят
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

// TestRenderBlock_SerialInheritance (guard #3, render-часть) — block.serial: 1 на
// 3 хостах → каждый потомок несёт SerialWidth=1 в DispatchPlan. Integration-часть
// (splitWaves+groupByHost: ApplyRequest хоста несёт ВСЕ потомки блока, 3 волны по
// 1) проверяется в scenario-пакете (TestDispatch_BlockSerialWave).
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

// TestRenderBlock_RequisitesInheritance (guard #4) — block.onchanges: протягивается
// на каждый потомок; resolveOnChanges резолвит register-имя в Index источника.
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

// TestRenderBlock_RequisitesUnion — block.onchanges + child.onchanges объединяются
// в потомке (union имён → union индексов после резолва).
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
	// inner — индекс 2; источники a_reg=0, b_reg=1 → OnChangesIdx содержит оба.
	got := tasks[2].OnChangesIdx
	if len(got) != 2 {
		t.Fatalf("inner.OnChangesIdx = %v, want union of 2 (block a_reg + child b_reg)", got)
	}
	seen := map[int]bool{got[0]: true, got[1]: true}
	if !seen[0] || !seen[1] {
		t.Errorf("inner.OnChangesIdx = %v, want {0,1}", got)
	}
}

// TestRenderBlock_NestedRecursion (guard #5) — block-в-block разворачивается,
// Index сквозной, наследование каскадом (внешний when + внутренний block-when +
// лист when → тройной AND).
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
	// leaf: outer.when && inner.when && leaf.when (каскад; вложенный merge
	// оборачивает уже-merged внешний предикат в свои скобки).
	if got, want := tasks[1].When, "((input.a) && (input.b)) && (input.c)"; got != want {
		t.Errorf("leaf.When = %q, want %q (каскад outer&&inner&&leaf)", got, want)
	}
}

// TestRenderBlock_StaticSkipSingle (guard #6) — block-level статический when:false
// → РОВНО 1 placeholder за весь блок (не N), Index последующих задач не съезжает.
func TestRenderBlock_StaticSkipSingle(t *testing.T) {
	grp := config.Task{
		Name: "grp",
		When: "input.action == 'apply'", // статический, false при другом action
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
	// 1 placeholder за весь блок + 1 задача after = 2.
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2 (1 block-placeholder + after)", len(tasks))
	}
	if tasks[0].Params != nil {
		t.Errorf("block placeholder Params = %v, want nil (skip)", tasks[0].Params)
	}
	if tasks[0].Index != 0 {
		t.Errorf("placeholder.Index = %d, want 0", tasks[0].Index)
	}
	// after не съехал: Index 1.
	if tasks[1].Index != 1 || tasks[1].Module != "core.exec.run" {
		t.Errorf("after = {Index:%d Module:%q}, want {1 core.exec.run}", tasks[1].Index, tasks[1].Module)
	}
	if len(plans) != 2 {
		t.Errorf("len(plans) = %d, want 2", len(plans))
	}
}

// TestRenderBlock_IndexIntegrity (guard #7) — fan-out блока + последующая задача
// дают сквозные монотонные Index без дыр.
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

// TestRenderBlock_VarsInheritance — block.vars база, child.vars поверх; потомок
// видит merged vars (через params-интерполяцию).
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

// TestRenderBlock_ApplyChild — apply-потомок в block разворачивается через
// renderApplyDestiny, наследует width. fixture-резолвер destiny.
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

// TestRenderBlock_LoopChildRejected — loop-потомок в block отвергается (вне pilot).
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

// TestRenderBlock_IncludeChildRejected (QA-пробел #10a) — include-потомок block
// отвергается до render (must быть раскрыт ExpandIncludes раньше). Грамматика
// допускает include в block-теле, но pilot C1 within-block include не поддержан
// (docs/destiny/tasks.md §6.5) — guardPilotBlockChild валит ErrUnexpandedInclude.
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

// TestRenderBlock_ParallelChildRejected (QA-пробел #10b) — parallel-потомок block
// отвергается (parallel целиком отложен пост-pilot, docs/destiny/tasks.md §6.5).
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

// TestRenderBlock_RunOnceOnBlock (QA-пробел #7) — ★ run_once: true НА block-задаче
// режет таргет до ОДНОГО хоста (первого по SID) ДО fan-out → ВСЕ потомки блока едут
// на ОДНОМ И ТОМ ЖЕ хосте. Критично для разрушительного блока (failover): «ровно
// один хост получает весь блок», а не каждый потомок резолвит run_once независимо.
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

// TestRenderBlock_EmptyBlock (QA-пробел #8) — пустой block (block: []) → 0
// RenderedTask, render проходит без ошибки (намеренное поведение: пустая группа —
// no-op, не падение). Фиксируем как контракт, чтобы регресс не превратил это в panic
// (нулевой fan-out) или ложную ошибку.
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
	// 0 задач за пустой блок + 1 after = 1; after не съехал по Index.
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

// staticResolver — фикстурный DestinyResolver для block-apply теста.
type staticResolver struct{ res *ResolvedDestiny }

func (s staticResolver) Resolve(_ context.Context, name string) (*ResolvedDestiny, error) {
	if name != s.res.Name {
		return nil, errors.New("unknown destiny")
	}
	return s.res, nil
}
