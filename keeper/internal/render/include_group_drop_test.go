package render

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Conditional-include group-drop (ADR-009 amendment): задачи, раскрытые из include
// под статическим `when:`, несут carry-through Task.IncludeWhen/IncludeGroupID
// (проставляет config.ExpandIncludes). Render вычисляет include-when ОДИН раз на
// группу и при false дропает ВСЕ её задачи РЕАЛЬНО — без эмита RenderedTask и без
// idx++ (индекс не резервируется). Это отличается от static-when placeholder-skip
// (emitStaticWhenSkip эмитит placeholder с idx++); дискриминатор — IncludeGroupID.
//
// Тесты строят carry-through-поля напрямую (ExpandIncludes — отдельная фаза, её
// собственные тесты — shared/config/include_expand_test.go), фокусируясь на
// render-инварианте group-drop.

// includeGroup помечает задачи как члены одной условной include-группы (один
// include-when, один group-id) — то, что ExpandIncludes проставил бы при
// раскрытии `- include: f.yml\n  when: <when>`.
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

// TestIncludeGroupDrop_WhenFalse_TasksAbsent — ★ include-when:false → задачи группы
// ОТСУТСТВУЮТ в плане (реальный дроп), а индексы хвоста сквозные БЕЗ дыр: задача
// после дропнутой группы получает idx, который занимала бы первая дропнутая (индекс
// не зарезервирован).
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
		Input:       map[string]any{"topology": "standalone"}, // != cluster → группа дропается
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
	// Индексы сквозные без дыр: head=0, tail=1 (НЕ tail=3 с дырами 1,2).
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

// TestIncludeGroupDrop_WhenTrue_TasksPresent — include-when:true → задачи группы
// присутствуют и рендерятся обычным путём (carry-through-поля на рендер не влияют).
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
		Input:       map[string]any{"topology": "cluster"}, // == cluster → группа остаётся
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
	// Группа рендерится полноценно (Params непусты — обычный путь, не placeholder).
	if tasks[1].Params == nil {
		t.Error("cluster-a.Params == nil — include-when:true должен рендерить группу обычным путём")
	}
	if got := tasks[1].Params.GetFields()["cmd"].GetStringValue(); got != "ca" {
		t.Errorf("cluster-a.cmd = %q, want ca", got)
	}
}

// TestIncludeGroupDrop_StaticInputWhen — типовой кейс: `when: input.X == 'cluster'`
// на include. Один и тот же план под двумя input даёт keep/drop. Симметрия с
// when:false/when:true выше, но фокус — что статический input.*-предикат работает.
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

// TestIncludeGroupDrop_OnChangesWithinGroupSafe — ★ НЕГАТИВ-инвариант: внутри
// дропнутой группы есть emitter (register:) + consumer (onchanges:). При дропе ОБА
// исчезают вместе → resolveOnChanges НЕ падает ErrOnChangesUnknownRegister (нет
// dangling-ссылки). Это и есть аргумент безопасности group-drop: register дропнутой
// группы недоступен снаружи, потому что cross-file register-ссылка уже lint-запрещена
// офлайн (per-file validateTaskRefs — доказано отдельно в shared/config-тесте
// TestConditionalInclude_CrossFileRegisterRejectedOffline).
func TestIncludeGroupDrop_OnChangesWithinGroupSafe(t *testing.T) {
	emitter := config.Task{
		Name:     "probe",
		Register: "probe_done",
		Module:   &config.ModuleTask{Module: "core.cmd.shell", Params: map[string]any{"cmd": "probe"}},
	}
	consumer := config.Task{
		Name:      "react",
		OnChanges: []string{"probe_done"}, // ссылка на register СОСЕДА по группе
		Module:    &config.ModuleTask{Module: "core.cmd.shell", Params: map[string]any{"cmd": "react"}},
	}
	var all []config.Task
	all = append(all, cmdTask("head", "head"))
	all = append(all, includeGroup("input.topology == 'cluster'", 3, emitter, consumer)...)

	manifest := &config.ScenarioManifest{Name: "drop-onchanges", Tasks: all}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"topology": "standalone"}, // дроп группы
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

// TestIncludeGroupDrop_CoexistsWithBlockPlaceholderSkip — ★ сосуществование двух
// механизмов в ОДНОМ плане: block со static-false when: (placeholder-skip, idx
// резервируется, потомки эмитят skip-placeholder) + условный include с when:false
// (group-drop, idx НЕ резервируется). Дискриминатор IncludeGroupID разводит их строго:
// block-потомок остаётся placeholder-ом, include-группа исчезает.
func TestIncludeGroupDrop_CoexistsWithBlockPlaceholderSkip(t *testing.T) {
	blockTask := config.Task{
		Name: "gated-block",
		When: "input.action == 'apply'", // static-false при action=update → block-placeholder-skip
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
	// head + block-child(placeholder) + tail = 3; include-группа дропнута (исчезла).
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
	// tail после дропа группы — индекс сквозной (после head=0, block-child=1 → tail=2).
	tail := names["tail"]
	if tail == nil || tail.Index != 2 {
		t.Errorf("tail.Index = %v, want 2 (block-placeholder резервирует idx, include-drop — нет)", tail)
	}
}
