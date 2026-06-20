package render

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// loopTask строит module-задачу с loop: для render-тестов.
func loopTask(loop *config.LoopSpec, params map[string]any) config.Task {
	return config.Task{
		Name:   "loop task",
		Loop:   loop,
		Module: &config.ModuleTask{Module: "core.exec.run", Params: params},
	}
}

// cmdOf достаёт params.command из RenderedTask.
func cmdOf(t *testing.T, rt *RenderedTask) string {
	t.Helper()
	return rt.Params.GetFields()["cmd"].GetStringValue()
}

// TestRenderLoop_OverInputArray — loop по input-массиву → N задач с item,
// сквозные индексы.
func TestRenderLoop_OverInputArray(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "x",
		Tasks: []config.Task{loopTask(
			&config.LoopSpec{Items: "${ input.users }", As: "user"},
			map[string]any{"cmd": "useradd ${ user.name }"},
		)},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: manifest,
		Input: map[string]any{"users": []any{
			map[string]any{"name": "alice"},
			map[string]any{"name": "bob"},
		}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2", len(tasks))
	}
	if got := cmdOf(t, tasks[0]); got != "useradd alice" {
		t.Errorf("tasks[0].command = %q", got)
	}
	if got := cmdOf(t, tasks[1]); got != "useradd bob" {
		t.Errorf("tasks[1].command = %q", got)
	}
	if tasks[0].Index != 0 || tasks[1].Index != 1 {
		t.Errorf("indices = %d,%d, want 0,1", tasks[0].Index, tasks[1].Index)
	}
	if len(plans) != 2 || plans[0].TaskIndex != 0 || plans[1].TaskIndex != 1 {
		t.Errorf("plans indices wrong: %+v", plans)
	}
}

// TestRenderLoop_ContinuousIndex — задача до loop + loop + задача после: индексы
// сквозные.
func TestRenderLoop_ContinuousIndex(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "x",
		Tasks: []config.Task{
			{Name: "before", Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "pre"}}},
			loopTask(&config.LoopSpec{Items: "${ input.xs }", As: "x"}, map[string]any{"cmd": "do ${ x }"}),
			{Name: "after", Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "post"}}},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"xs": []any{"a", "b"}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("h", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 4 {
		t.Fatalf("len(tasks) = %d, want 4", len(tasks))
	}
	wantIdx := []int{0, 1, 2, 3}
	for i, rt := range tasks {
		if rt.Index != wantIdx[i] {
			t.Errorf("tasks[%d].Index = %d, want %d", i, rt.Index, wantIdx[i])
		}
	}
	if cmdOf(t, tasks[1]) != "do a" || cmdOf(t, tasks[2]) != "do b" {
		t.Errorf("loop commands wrong: %q %q", cmdOf(t, tasks[1]), cmdOf(t, tasks[2]))
	}
}

// TestRenderLoop_OverObject — object: as=значение, index_as=ключ, порядок
// алфавитный по ключам.
func TestRenderLoop_OverObject(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "x",
		Tasks: []config.Task{loopTask(
			&config.LoopSpec{Items: "${ input.acl }", As: "perm", IndexAs: "user"},
			map[string]any{"cmd": "set ${ user } ${ perm }"},
		)},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: manifest,
		// Намеренно не в алфавитном порядке: bob, alice.
		Input:       map[string]any{"acl": map[string]any{"bob": "ro", "alice": "rw"}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("h", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2", len(tasks))
	}
	// Алфавитный порядок ключей: alice, bob.
	if got := cmdOf(t, tasks[0]); got != "set alice rw" {
		t.Errorf("tasks[0] = %q, want 'set alice rw'", got)
	}
	if got := cmdOf(t, tasks[1]); got != "set bob ro" {
		t.Errorf("tasks[1] = %q, want 'set bob ro'", got)
	}
}

// TestRenderLoop_IndexAsArray — index_as для массива — 0-based индекс.
func TestRenderLoop_IndexAsArray(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "x",
		Tasks: []config.Task{loopTask(
			&config.LoopSpec{Items: "${ input.xs }", As: "x", IndexAs: "i"},
			map[string]any{"cmd": "echo ${ i }:${ x }"},
		)},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"xs": []any{"a", "b", "c"}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("h", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := []string{"echo 0:a", "echo 1:b", "echo 2:c"}
	if len(tasks) != len(want) {
		t.Fatalf("len(tasks) = %d, want %d", len(tasks), len(want))
	}
	for i, w := range want {
		if got := cmdOf(t, tasks[i]); got != w {
			t.Errorf("tasks[%d] = %q, want %q", i, got, w)
		}
	}
}

// TestRenderLoop_WhenFilters — when: фильтрует элементы.
func TestRenderLoop_WhenFilters(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "x",
		Tasks: []config.Task{loopTask(
			&config.LoopSpec{Items: "${ input.users }", As: "user", When: "user.active"},
			map[string]any{"cmd": "useradd ${ user.name }"},
		)},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: manifest,
		Input: map[string]any{"users": []any{
			map[string]any{"name": "alice", "active": true},
			map[string]any{"name": "bob", "active": false},
			map[string]any{"name": "carol", "active": true},
		}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("h", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2 (bob filtered out)", len(tasks))
	}
	if cmdOf(t, tasks[0]) != "useradd alice" || cmdOf(t, tasks[1]) != "useradd carol" {
		t.Errorf("filtered commands wrong: %q %q", cmdOf(t, tasks[0]), cmdOf(t, tasks[1]))
	}
}

// TestRenderLoop_WhenBySoulprintRejected — when: со ссылкой на soulprint →
// понятная ошибка. loop (items+when) host-инвариантен в пилоте; host-вариативный
// предикат по soulprint конкретного хоста не поддержан (per-host loop-фильтрация
// отложена), а НЕ молчаливо решается по первому хосту (баг 2).
func TestRenderLoop_WhenBySoulprintRejected(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "x",
		Tasks: []config.Task{loopTask(
			&config.LoopSpec{Items: "${ input.xs }", As: "x", When: "soulprint.self.os.family == 'debian'"},
			map[string]any{"cmd": "do ${ x }"},
		)},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"xs": []any{"a", "b"}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts: []*topology.HostFacts{
			host("deb", []string{"svc"}, map[string]any{"os": map[string]any{"family": "debian"}}),
			host("rh", []string{"svc"}, map[string]any{"os": map[string]any{"family": "rhel"}}),
		},
	}
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("ожидали ошибку: host-вариативный when по soulprint вне pilot-объёма")
	}
	if !strings.Contains(err.Error(), "soulprint") || !strings.Contains(err.Error(), "loop.when") {
		t.Fatalf("сообщение должно явно указывать на loop.when и soulprint, получили: %v", err)
	}
}

// TestRenderLoop_WhenFiltersAll — when: отсеивает ВСЕ элементы → 0 задач без
// паники (валидный no-op, как пустой items).
func TestRenderLoop_WhenFiltersAll(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "x",
		Tasks: []config.Task{loopTask(
			&config.LoopSpec{Items: "${ input.users }", As: "user", When: "user.active"},
			map[string]any{"cmd": "useradd ${ user.name }"},
		)},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: manifest,
		Input: map[string]any{"users": []any{
			map[string]any{"name": "alice", "active": false},
			map[string]any{"name": "bob", "active": false},
		}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("h", []string{"svc"}, nil)},
	}
	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 0 || len(plans) != 0 {
		t.Fatalf("len(tasks)=%d len(plans)=%d, want 0,0 (when отсеял всё)", len(tasks), len(plans))
	}
}

// TestRenderLoop_WhenNonBool — when: возвращает не-bool → понятная ошибка
// (предикат обязан возвращать булево).
func TestRenderLoop_WhenNonBool(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "x",
		Tasks: []config.Task{loopTask(
			&config.LoopSpec{Items: "${ input.users }", As: "user", When: "user.name"},
			map[string]any{"cmd": "useradd ${ user.name }"},
		)},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: manifest,
		Input: map[string]any{"users": []any{
			map[string]any{"name": "alice"},
		}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("h", []string{"svc"}, nil)},
	}
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("ожидали ошибку: when вернул не-bool")
	}
	if !strings.Contains(err.Error(), "loop.when") || !strings.Contains(err.Error(), "bool") {
		t.Fatalf("сообщение должно указывать loop.when и ожидаемый bool, получили: %v", err)
	}
}

// TestRenderLoop_WithRunOnce — loop + run_once: вместе: run_once режет таргет до
// одного хоста (по SID), весь loop прокатывается на нём (итерации на единственном
// хосте). Разрешено: оси run_once (таргет) и loop (итерации) ортогональны.
func TestRenderLoop_WithRunOnce(t *testing.T) {
	task := loopTask(&config.LoopSpec{Items: "${ input.xs }", As: "x"}, map[string]any{"cmd": "do ${ x }"})
	task.RunOnce = true
	manifest := &config.ScenarioManifest{Name: "x", Tasks: []config.Task{task}}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"xs": []any{"a", "b", "c"}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts: []*topology.HostFacts{
			host("h2", []string{"svc"}, nil),
			host("h1", []string{"svc"}, nil),
		},
	}
	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// 3 итерации, каждая на единственном хосте (первый по SID — h1).
	if len(tasks) != 3 || len(plans) != 3 {
		t.Fatalf("len(tasks)=%d len(plans)=%d, want 3,3", len(tasks), len(plans))
	}
	for i, pl := range plans {
		if len(pl.TargetSIDs) != 1 || pl.TargetSIDs[0] != "h1" {
			t.Errorf("plans[%d].TargetSIDs = %v, want [h1] (run_once → первый по SID)", i, pl.TargetSIDs)
		}
	}
	if cmdOf(t, tasks[0]) != "do a" || cmdOf(t, tasks[2]) != "do c" {
		t.Errorf("loop commands wrong under run_once: %q %q", cmdOf(t, tasks[0]), cmdOf(t, tasks[2]))
	}
}

// TestRenderLoop_InDestinyExpands — loop: на задаче внутри destiny РАЗВОРАЧИВАЕТСЯ
// (слайс E снят, guardDestinyTask больше не режет loop): одна loop-задача destiny
// даёт N RenderedTask со сквозными индексами, продолжающими предыдущие задачи
// destiny. items берётся из destiny-input (передан через apply.input). Расширенное
// покрытие destiny-loop — destiny_loop_test.go; здесь — минимальная регрессия
// рядом с loop-механикой.
func TestRenderLoop_InDestinyExpands(t *testing.T) {
	d := flatDestiny()
	// destiny-input получает xs (массив items) через apply.input; вторая задача
	// destiny размножается loop-ом по нему.
	d.Input["xs"] = &config.InputSchema{Type: "array", Required: true}
	d.Tasks[1].Loop = &config.LoopSpec{Items: "${ input.xs }", As: "x"}
	d.Tasks[1].Module.Params["cmd"] = "echo ${ x }"
	res := &stubDestinyResolver{resolved: d}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("pilot-flat", map[string]any{"marker_file": "/m", "marker_payload": "p", "xs": "${ input.xs }"}),
		Input:       map[string]any{"xs": []any{"a", "b", "c"}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// task0 (marker, Index 0) + loop×3 (Index 1,2,3) = 4 задачи, сквозные индексы.
	if len(tasks) != 4 {
		t.Fatalf("len(tasks) = %d, want 4 (marker + loop×3 в destiny)", len(tasks))
	}
	for i, rt := range tasks {
		if rt.Index != i {
			t.Errorf("tasks[%d].Index = %d, want %d (сквозные через destiny-loop)", i, rt.Index, i)
		}
	}
	if cmdOf(t, tasks[1]) != "echo a" || cmdOf(t, tasks[3]) != "echo c" {
		t.Errorf("destiny-loop commands wrong: %q ... %q", cmdOf(t, tasks[1]), cmdOf(t, tasks[3]))
	}
}

// TestRenderLoop_DefaultAs — as: опущен → переменная item.
func TestRenderLoop_DefaultAs(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "x",
		Tasks: []config.Task{loopTask(
			&config.LoopSpec{Items: "${ input.xs }"},
			map[string]any{"cmd": "echo ${ item }"},
		)},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"xs": []any{"a", "b"}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("h", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 2 || cmdOf(t, tasks[0]) != "echo a" || cmdOf(t, tasks[1]) != "echo b" {
		t.Fatalf("default-as wrong: %d tasks", len(tasks))
	}
}

// TestRenderLoop_WithWhere — loop катится на КАЖДОМ отфильтрованном where:-хосте;
// per-iteration host-инвариантность сохраняется (params одинаковы на хостах).
func TestRenderLoop_WithWhere(t *testing.T) {
	task := loopTask(&config.LoopSpec{Items: "${ input.xs }", As: "x"}, map[string]any{"cmd": "do ${ x }"})
	task.Where = "soulprint.self.os.family == 'debian'"
	manifest := &config.ScenarioManifest{Name: "x", Tasks: []config.Task{task}}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"xs": []any{"a", "b"}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts: []*topology.HostFacts{
			host("deb1", []string{"svc"}, map[string]any{"os": map[string]any{"family": "debian"}}),
			host("deb2", []string{"svc"}, map[string]any{"os": map[string]any{"family": "debian"}}),
			host("rh1", []string{"svc"}, map[string]any{"os": map[string]any{"family": "rhel"}}),
		},
	}
	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// 2 итерации (a, b); каждая таргетит только debian-хосты (deb1, deb2).
	if len(tasks) != 2 || len(plans) != 2 {
		t.Fatalf("len(tasks)=%d len(plans)=%d, want 2,2", len(tasks), len(plans))
	}
	for i, pl := range plans {
		if len(pl.TargetSIDs) != 2 || pl.TargetSIDs[0] != "deb1" || pl.TargetSIDs[1] != "deb2" {
			t.Errorf("plans[%d].TargetSIDs = %v, want [deb1 deb2]", i, pl.TargetSIDs)
		}
	}
}

// TestRenderLoop_WithSerial — loop под serial: прокатывается целиком на каждом
// хосте волны; SerialWidth наследуется всеми итерациями (оси ортогональны).
func TestRenderLoop_WithSerial(t *testing.T) {
	task := loopTask(&config.LoopSpec{Items: "${ input.xs }", As: "x"}, map[string]any{"cmd": "do ${ x }"})
	task.Serial = 1
	manifest := &config.ScenarioManifest{Name: "x", Tasks: []config.Task{task}}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"xs": []any{"a", "b", "c"}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts: []*topology.HostFacts{
			host("h1", []string{"svc"}, nil),
			host("h2", []string{"svc"}, nil),
		},
	}
	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 3 || len(plans) != 3 {
		t.Fatalf("len(tasks)=%d len(plans)=%d, want 3,3", len(tasks), len(plans))
	}
	for i, pl := range plans {
		if pl.SerialWidth != 1 {
			t.Errorf("plans[%d].SerialWidth = %d, want 1 (inherited by every iteration)", i, pl.SerialWidth)
		}
		if len(pl.TargetSIDs) != 2 {
			t.Errorf("plans[%d].TargetSIDs = %v, want 2 hosts", i, pl.TargetSIDs)
		}
	}
}

// TestRenderLoop_PerIterationHostInvariant — host-зависимые params ВНУТРИ
// итерации (разные хосты дают разный результат) → ошибка host-инвариантности,
// проверка применяется по-итерационно.
func TestRenderLoop_PerIterationHostInvariant(t *testing.T) {
	// params зависит и от loop-переменной, и от soulprint хоста: на разных
	// хостах одна итерация даёт разные params — нарушение host-инвариантности.
	task := loopTask(&config.LoopSpec{Items: "${ input.xs }", As: "x"},
		map[string]any{"cmd": "do ${ x } on ${ soulprint.self.os.family }"})
	manifest := &config.ScenarioManifest{Name: "x", Tasks: []config.Task{task}}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"xs": []any{"a"}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts: []*topology.HostFacts{
			host("deb", []string{"svc"}, map[string]any{"os": map[string]any{"family": "debian"}}),
			host("rh", []string{"svc"}, map[string]any{"os": map[string]any{"family": "rhel"}}),
		},
	}
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("ожидали ошибку host-инвариантности для host-зависимых params в итерации")
	}
}

// TestRenderLoop_PerIterationDifferentParamsOK — разные params ПО ОСИ ИТЕРАЦИЙ
// (но host-инвариантные внутри каждой) — норма, не ошибка.
func TestRenderLoop_PerIterationDifferentParamsOK(t *testing.T) {
	task := loopTask(&config.LoopSpec{Items: "${ input.xs }", As: "x"},
		map[string]any{"cmd": "do ${ x }"})
	manifest := &config.ScenarioManifest{Name: "x", Tasks: []config.Task{task}}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"xs": []any{"a", "b"}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts: []*topology.HostFacts{
			host("h1", []string{"svc"}, nil),
			host("h2", []string{"svc"}, nil),
		},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v (разные params по оси итераций должны быть ок)", err)
	}
	if cmdOf(t, tasks[0]) != "do a" || cmdOf(t, tasks[1]) != "do b" {
		t.Errorf("commands wrong: %q %q", cmdOf(t, tasks[0]), cmdOf(t, tasks[1]))
	}
}

// TestRenderLoop_EmptyItems — пустой items → 0 задач (валидный no-op).
func TestRenderLoop_EmptyItems(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "x",
		Tasks: []config.Task{loopTask(
			&config.LoopSpec{Items: "${ input.xs }", As: "x"},
			map[string]any{"cmd": "do ${ x }"},
		)},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"xs": []any{}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("h", []string{"svc"}, nil)},
	}
	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 0 || len(plans) != 0 {
		t.Fatalf("len(tasks)=%d len(plans)=%d, want 0,0", len(tasks), len(plans))
	}
}

// TestRenderLoop_NonCollectionItems — items, не разрешившийся в array/object →
// ошибка.
func TestRenderLoop_NonCollectionItems(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "x",
		Tasks: []config.Task{loopTask(
			&config.LoopSpec{Items: "${ input.scalar }", As: "x"},
			map[string]any{"cmd": "do ${ x }"},
		)},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"scalar": "not-a-list"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("h", []string{"svc"}, nil)},
	}
	if _, _, err := p.Render(context.Background(), in); err == nil {
		t.Fatal("ожидали ошибку: items не array/object")
	}
}

// whenLoopTask строит module-задачу с loop: + статическим when: для
// static-when-skip-тестов.
func whenLoopTask(when string, loop *config.LoopSpec, params map[string]any) config.Task {
	t := loopTask(loop, params)
	t.When = when
	return t
}

// TestRenderLoop_StaticWhenSkip_UnresolvableItems — ★ баговый кейс (ordering
// static-when ↔ loop.items): loop-задача со статически-false when: и items на
// ОТСУТСТВУЮЩИЙ input-ключ. static-when предшествует loop-fan-out (инвариант
// architect): задача скипается ЦЕЛИКОМ ДО resolveLoopItems, поэтому absent-ключ
// в items НЕ должен ронять Render. Резолв items здесь падает (нет input.users) →
// 1 skip-placeholder (Params==nil, When протянут, FlowContext≠nil, Index сквозной).
func TestRenderLoop_StaticWhenSkip_UnresolvableItems(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "x",
		Tasks: []config.Task{whenLoopTask(
			"input.action == 'apply'", // static-false при action=update_acls
			&config.LoopSpec{Items: "${ input.users }", As: "user"},
			map[string]any{"cmd": "useradd ${ user.name }"},
		)},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"action": "update_acls"}, // users НЕ передан
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("h", []string{"svc"}, nil)},
	}
	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v (static-when:false должен скипнуть задачу ДО resolveLoopItems, а не падать на no-such-key input.users)", err)
	}
	if len(tasks) != 1 || len(plans) != 1 {
		t.Fatalf("len(tasks)=%d len(plans)=%d, want 1,1 (нерезолвимый items → один skip-placeholder)", len(tasks), len(plans))
	}
	rt := tasks[0]
	if rt.Params != nil {
		t.Errorf("Params != nil — static-when:false должен скипнуть рендер")
	}
	if rt.When != "input.action == 'apply'" {
		t.Errorf("When = %q, не протянут", rt.When)
	}
	if rt.FlowContext == nil {
		t.Errorf("FlowContext == nil — нужен для Soul-side evalWhen → SKIPPED")
	}
	if rt.Index != 0 {
		t.Errorf("Index = %d, want 0 (сквозной)", rt.Index)
	}
	if plans[0].TaskIndex != 0 {
		t.Errorf("plans[0].TaskIndex = %d, want 0", plans[0].TaskIndex)
	}
}

// TestRenderLoop_StaticWhenSkip_UnresolvableItems_ContinuousIndex — нерезолвимый
// static-skip loop в середине плана: index сквозной (1 placeholder, не N).
func TestRenderLoop_StaticWhenSkip_UnresolvableItems_ContinuousIndex(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "x",
		Tasks: []config.Task{
			{Name: "before", Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "pre"}}},
			whenLoopTask("input.action == 'apply'",
				&config.LoopSpec{Items: "${ input.users }", As: "user"},
				map[string]any{"cmd": "useradd ${ user.name }"}),
			{Name: "after", Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "post"}}},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"action": "update_acls"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("h", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// before(0) + skip-placeholder(1) + after(2) = 3 задачи.
	if len(tasks) != 3 {
		t.Fatalf("len(tasks) = %d, want 3 (before + 1 placeholder + after)", len(tasks))
	}
	for i, rt := range tasks {
		if rt.Index != i {
			t.Errorf("tasks[%d].Index = %d, want %d", i, rt.Index, i)
		}
	}
	if tasks[1].Params != nil {
		t.Errorf("placeholder Params != nil")
	}
	if cmdOf(t, tasks[2]) != "post" {
		t.Errorf("after-задача рендерится после placeholder: %q", cmdOf(t, tasks[2]))
	}
}

// TestRenderLoop_StaticTrueWhen_FansOut — реверс на классификацию: static-TRUE
// when: + loop → обычный fan-out N реальных задач (активную ветку не скипаем).
func TestRenderLoop_StaticTrueWhen_FansOut(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "x",
		Tasks: []config.Task{whenLoopTask(
			"input.action == 'apply'", // static-TRUE при action=apply
			&config.LoopSpec{Items: "${ input.users }", As: "user"},
			map[string]any{"cmd": "useradd ${ user.name }"},
		)},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: manifest,
		Input: map[string]any{"action": "apply", "users": []any{
			map[string]any{"name": "alice"},
			map[string]any{"name": "bob"},
		}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("h", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2 (static-true → обычный fan-out)", len(tasks))
	}
	if cmdOf(t, tasks[0]) != "useradd alice" || cmdOf(t, tasks[1]) != "useradd bob" {
		t.Errorf("static-true fan-out commands wrong: %q %q", cmdOf(t, tasks[0]), cmdOf(t, tasks[1]))
	}
	for i, rt := range tasks {
		if rt.Params == nil {
			t.Errorf("tasks[%d].Params == nil — static-true задача должна рендериться", i)
		}
	}
}

// TestRenderLoop_MixedWhen_NotStaticSkipped — реверс на классификацию: mixed-when
// (input + register) НЕ статический → items резолвится, fan-out обычный (register-
// зависимый when протягивается строкой, вычисляется Soul-side). При резолвимом
// items static-skip-ветка НЕ должна срабатывать.
func TestRenderLoop_MixedWhen_NotStaticSkipped(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "x",
		Tasks: []config.Task{whenLoopTask(
			"input.action == 'apply' && register.probe.changed",
			&config.LoopSpec{Items: "${ input.users }", As: "user"},
			map[string]any{"cmd": "useradd ${ user.name }"},
		)},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: manifest,
		Input: map[string]any{"action": "create", "users": []any{
			map[string]any{"name": "alice"},
		}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("h", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// register-зависимый when → НЕ static-skip: items резолвится, fan-out по нему.
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1 (mixed-when не статический, fan-out по items)", len(tasks))
	}
	if tasks[0].Params == nil {
		t.Errorf("Params == nil — mixed-when не статический, params должны рендериться")
	}
	if cmdOf(t, tasks[0]) != "useradd alice" {
		t.Errorf("command = %q, want 'useradd alice'", cmdOf(t, tasks[0]))
	}
}

// TestRenderLoop_StaticWhenSkip_ConsistentAcrossPassages — static-false loop
// (нерезолвимый items) даёт одинаковый результат при повторном рендере (passage
// активируется заново). Один input-снимок → один и тот же 1-placeholder skip.
func TestRenderLoop_StaticWhenSkip_ConsistentAcrossPassages(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "x",
		Tasks: []config.Task{whenLoopTask(
			"input.action == 'apply'",
			&config.LoopSpec{Items: "${ input.users }", As: "user"},
			map[string]any{"cmd": "useradd ${ user.name }"},
		)},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"action": "update_acls"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("h", []string{"svc"}, nil)},
	}
	first, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render (passage 0): %v", err)
	}
	second, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render (повтор): %v", err)
	}
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("len first=%d second=%d, want 1,1 (консистентность по проходам)", len(first), len(second))
	}
	if first[0].When != second[0].When || first[0].Index != second[0].Index {
		t.Errorf("placeholder разошёлся между проходами: %+v vs %+v", first[0], second[0])
	}
	if first[0].Params != nil || second[0].Params != nil {
		t.Errorf("Params должны быть nil на обоих проходах")
	}
}

// TestRenderLoop_StaticWhenSkip_PreservesOnChanges — guard: static-false loop-
// задача с onchanges: → skip-placeholder сохраняет requisite-имена (loopSkipPlaceholder
// протягивает onChangesNames симметрично staticSkipPlaceholder) → финальный
// resolveOnChanges мапит их в OnChangesIdx. Без протяжки имена терялись бы на
// placeholder и OnChangesIdx остался бы nil — латентная потеря requisites.
func TestRenderLoop_StaticWhenSkip_PreservesOnChanges(t *testing.T) {
	loopT := whenLoopTask(
		"input.action == 'apply'", // static-false при action=update_acls
		&config.LoopSpec{Items: "${ input.users }", As: "user"},
		map[string]any{"cmd": "useradd ${ user.name }"},
	)
	loopT.OnChanges = []string{"probe"}

	manifest := &config.ScenarioManifest{
		Name: "x",
		Tasks: []config.Task{
			{
				Name:     "probe",
				Register: "probe",
				Module:   &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "id"}},
			},
			loopT,
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"action": "update_acls"}, // users НЕ передан → static-skip ДО resolveLoopItems
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("h", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// probe(0) + 1 skip-placeholder(1) = 2 задачи.
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2 (probe + 1 placeholder)", len(tasks))
	}
	ph := tasks[1]
	if ph.Params != nil {
		t.Errorf("placeholder Params != nil — static-when:false должен скипнуть рендер")
	}
	if len(ph.OnChangesIdx) != 1 || ph.OnChangesIdx[0] != 0 {
		t.Fatalf("OnChangesIdx = %v, want [0] (onchanges: [probe] → Index probe-задачи) — requisite-имена потерялись на skip-placeholder", ph.OnChangesIdx)
	}
}

// TestRenderLoop_OnApplyRejected — loop на apply-задаче по-прежнему вне pilot
// (guardPilotDSL отвергает с ErrUnsupportedDSL).
func TestRenderLoop_OnApplyRejected(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "x",
		Tasks: []config.Task{{
			Name:  "apply loop",
			Loop:  &config.LoopSpec{Items: "${ input.xs }", As: "x"},
			Apply: &config.ApplyTask{Destiny: "sub"},
		}},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"xs": []any{"a"}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("h", []string{"svc"}, nil)},
	}
	_, _, err := p.Render(context.Background(), in)
	if !errors.Is(err, ErrUnsupportedDSL) {
		t.Fatalf("err = %v, want ErrUnsupportedDSL", err)
	}
}
