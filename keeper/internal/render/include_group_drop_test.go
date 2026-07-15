package render

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Conditional-include group-drop (ADR-009 amendment): tasks expanded from an include
// under a static `when:` carry through Task.IncludeWhen/IncludeGroupID
// (set by config.ExpandIncludes). Render evaluates include-when ONCE per
// group and on false drops ALL its tasks FOR REAL — no RenderedTask emitted and no
// idx++ (the index isn't reserved). This differs from static-when placeholder-skip
// (emitStaticWhenSkip emits a placeholder with idx++); the discriminator is IncludeGroupID.
//
// Tests build the carry-through fields directly (ExpandIncludes is a separate phase with
// its own tests — shared/config/include_expand_test.go), focusing on
// the render-side group-drop invariant.

// includeGroup marks tasks as members of one conditional include group (one
// include-when, one group-id) — what ExpandIncludes would set when
// expanding `- include: f.yml\n  when: <when>`.
func includeGroup(when string, groupID int, tasks ...config.Task) []config.Task {
	for i := range tasks {
		tasks[i].IncludeWhen = when
		tasks[i].IncludeGroupID = groupID
	}
	return tasks
}

func cmdTask(name, cmd string) config.Task {
	return config.Task{Name: name, Module: &config.ModuleTask{Module: "core.cmd.shell", Params: map[string]any{"cmd": cmd}}}
}

func singleHost() []*topology.HostFacts {
	return []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)}
}

// TestIncludeGroupDrop_WhenFalse_TasksAbsent — ★ include-when:false → the group's tasks
// are ABSENT from the plan (a real drop), and the tail's indices are contiguous with NO gaps: the task
// after the dropped group gets the idx the first dropped task would have occupied (the index
// isn't reserved).
func TestIncludeGroupDrop_WhenFalse_TasksAbsent(t *testing.T) {
	var all []config.Task
	all = append(all, cmdTask("head", "head"))
	all = append(all, includeGroup("input.topology == 'cluster'", 1,
		cmdTask("cluster-a", "ca"),
		cmdTask("cluster-b", "cb"),
	)...)
	all = append(all, cmdTask("tail", "tail"))

	manifest := &config.ScenarioManifest{Name: "cond-include", Tasks: all}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"topology": "standalone"}, // != cluster → group is dropped
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       singleHost(),
	}
	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2 (head + tail; группа дропнута БЕЗ placeholder)", len(tasks))
	}
	if len(plans) != 2 {
		t.Fatalf("len(plans) = %d, want 2 (дроп не резервирует план)", len(plans))
	}
	for _, rt := range tasks {
		if rt.Name == "cluster-a" || rt.Name == "cluster-b" {
			t.Fatalf("задача %q дропнутой группы присутствует в плане", rt.Name)
		}
	}
	// Indices are contiguous with no gaps: head=0, tail=1 (NOT tail=3 with gaps at 1,2).
	if tasks[0].Index != 0 || tasks[1].Index != 1 {
		t.Errorf("Index = %d,%d, want 0,1 (сквозные без дыр — дроп не резервирует idx)", tasks[0].Index, tasks[1].Index)
	}
	if tasks[0].Name != "head" || tasks[1].Name != "tail" {
		t.Errorf("план = [%q,%q], want [head,tail]", tasks[0].Name, tasks[1].Name)
	}
	if plans[0].TaskIndex != 0 || plans[1].TaskIndex != 1 {
		t.Errorf("plans TaskIndex = %d,%d, want 0,1", plans[0].TaskIndex, plans[1].TaskIndex)
	}
}

// TestIncludeGroupDrop_WhenTrue_TasksPresent — include-when:true → the group's tasks
// are present and render the normal way (the carry-through fields don't affect rendering).
func TestIncludeGroupDrop_WhenTrue_TasksPresent(t *testing.T) {
	var all []config.Task
	all = append(all, cmdTask("head", "head"))
	all = append(all, includeGroup("input.topology == 'cluster'", 1,
		cmdTask("cluster-a", "ca"),
		cmdTask("cluster-b", "cb"),
	)...)
	all = append(all, cmdTask("tail", "tail"))

	manifest := &config.ScenarioManifest{Name: "cond-include", Tasks: all}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"topology": "cluster"}, // == cluster → group stays
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       singleHost(),
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 4 {
		t.Fatalf("len(tasks) = %d, want 4 (head + 2 группы + tail)", len(tasks))
	}
	want := []string{"head", "cluster-a", "cluster-b", "tail"}
	for i, w := range want {
		if tasks[i].Name != w {
			t.Errorf("tasks[%d].Name = %q, want %q", i, tasks[i].Name, w)
		}
		if tasks[i].Index != i {
			t.Errorf("tasks[%d].Index = %d, want %d (сквозная нумерация)", i, tasks[i].Index, i)
		}
	}
	// The group renders fully (Params non-empty — normal path, not a placeholder).
	if tasks[1].Params == nil {
		t.Error("cluster-a.Params == nil — include-when:true должен рендерить группу обычным путём")
	}
	if got := tasks[1].Params.GetFields()["cmd"].GetStringValue(); got != "ca" {
		t.Errorf("cluster-a.cmd = %q, want ca", got)
	}
}

// TestIncludeGroupDrop_StaticInputWhen — the typical case: `when: input.X == 'cluster'`
// on an include. The same plan under two different inputs gives keep/drop. Symmetric with
// when:false/when:true above, but the focus is that a static input.* predicate works.
func TestIncludeGroupDrop_StaticInputWhen(t *testing.T) {
	build := func() *config.ScenarioManifest {
		var all []config.Task
		all = append(all, includeGroup("input.role == 'cluster'", 7,
			cmdTask("only-cluster", "x"),
		)...)
		return &config.ScenarioManifest{Name: "typed-when", Tasks: all}
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)

	cases := []struct {
		role    string
		wantLen int
	}{
		{"cluster", 1},
		{"standalone", 0},
	}
	for _, tc := range cases {
		in := RenderInput{
			Scenario:    build(),
			Input:       map[string]any{"role": tc.role},
			Incarnation: IncarnationMeta{Name: "svc"},
			Hosts:       singleHost(),
		}
		tasks, _, err := p.Render(context.Background(), in)
		if err != nil {
			t.Fatalf("Render(role=%s): %v", tc.role, err)
		}
		if len(tasks) != tc.wantLen {
			t.Errorf("role=%s: len(tasks)=%d, want %d", tc.role, len(tasks), tc.wantLen)
		}
	}
}

// TestIncludeGroupDrop_OnChangesWithinGroupSafe — ★ NEGATIVE invariant: inside
// the dropped group there's an emitter (register:) + consumer (onchanges:). On drop BOTH
// disappear together → resolveOnChanges does NOT fail with ErrOnChangesUnknownRegister (no
// dangling reference). This is exactly the safety argument for group-drop: a dropped group's
// register isn't reachable from outside, because a cross-file register reference is already lint-forbidden
// offline (per-file validateTaskRefs — proven separately in the shared/config test
// TestConditionalInclude_CrossFileRegisterRejectedOffline).
func TestIncludeGroupDrop_OnChangesWithinGroupSafe(t *testing.T) {
	emitter := config.Task{
		Name:     "probe",
		Register: "probe_done",
		Module:   &config.ModuleTask{Module: "core.cmd.shell", Params: map[string]any{"cmd": "probe"}},
	}
	consumer := config.Task{
		Name:      "react",
		OnChanges: []string{"probe_done"}, // reference to a register of a group SIBLING
		Module:    &config.ModuleTask{Module: "core.cmd.shell", Params: map[string]any{"cmd": "react"}},
	}
	var all []config.Task
	all = append(all, cmdTask("head", "head"))
	all = append(all, includeGroup("input.topology == 'cluster'", 3, emitter, consumer)...)

	manifest := &config.ScenarioManifest{Name: "drop-onchanges", Tasks: all}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"topology": "standalone"}, // group drop
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       singleHost(),
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: дроп группы с внутренним onchanges НЕ должен падать ErrOnChangesUnknownRegister, got %v", err)
	}
	if len(tasks) != 1 || tasks[0].Name != "head" {
		t.Fatalf("план = %d задач, want [head] (вся группа — emitter+consumer — дропнута)", len(tasks))
	}
}

// TestIncludeGroupDrop_CoexistsWithBlockPlaceholderSkip — ★ coexistence of two
// mechanisms in ONE plan: a block with static-false when: (placeholder-skip, idx
// is reserved, children emit a skip placeholder) + a conditional include with when:false
// (group-drop, idx is NOT reserved). The IncludeGroupID discriminator separates them strictly:
// the block child stays a placeholder, the include group disappears.
func TestIncludeGroupDrop_CoexistsWithBlockPlaceholderSkip(t *testing.T) {
	blockTask := config.Task{
		Name: "gated-block",
		When: "input.action == 'apply'", // static-false when action=update → block-placeholder-skip
		Block: &config.BlockTask{Block: []config.Task{
			cmdTask("block-child", "bc"),
		}},
	}
	var all []config.Task
	all = append(all, cmdTask("head", "head"))
	all = append(all, blockTask)
	all = append(all, includeGroup("input.topology == 'cluster'", 5,
		cmdTask("inc-a", "ia"),
	)...)
	all = append(all, cmdTask("tail", "tail"))

	manifest := &config.ScenarioManifest{Name: "coexist", Tasks: all}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"action": "update", "topology": "standalone"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       singleHost(),
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// head + block-child(placeholder) + tail = 3; the include group is dropped (gone).
	if len(tasks) != 3 {
		t.Fatalf("len(tasks) = %d, want 3 (head + block-child-placeholder + tail; inc-группа дропнута)", len(tasks))
	}
	names := map[string]*RenderedTask{}
	for _, rt := range tasks {
		names[rt.Name] = rt
	}
	if _, ok := names["inc-a"]; ok {
		t.Error("inc-a присутствует — условный include должен быть РЕАЛЬНО дропнут (group-drop)")
	}
	bc, ok := names["block-child"]
	if !ok {
		t.Fatal("block-child отсутствует — block-static-skip должен оставить placeholder (НЕ дроп)")
	}
	if bc.Params != nil {
		t.Error("block-child.Params != nil — static-false block-потомок должен быть skip-placeholder")
	}
	if bc.FlowContext == nil {
		t.Error("block-child.FlowContext == nil — placeholder несёт flow_context для Soul-side evalWhen")
	}
	// tail after the group drop — index is contiguous (after head=0, block-child=1 → tail=2).
	tail := names["tail"]
	if tail == nil || tail.Index != 2 {
		t.Errorf("tail.Index = %v, want 2 (block-placeholder резервирует idx, include-drop — нет)", tail)
	}
}
