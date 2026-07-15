package render

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// loopTask builds a module task with loop: for render tests.
func loopTask(loop *config.LoopSpec, params map[string]any) config.Task {
	return config.Task{
		Name:   "loop task",
		Loop:   loop,
		Module: &config.ModuleTask{Module: "core.exec.run", Params: params},
	}
}

// cmdOf extracts params.command from a RenderedTask.
func cmdOf(t *testing.T, rt *RenderedTask) string {
	t.Helper()
	return rt.Params.GetFields()["cmd"].GetStringValue()
}

// TestRenderLoop_OverInputArray — loop over an input array → N tasks with item,
// continuous indices.
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

// TestRenderLoop_ContinuousIndex — task before loop + loop + task after:
// indices stay continuous.
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

// TestRenderLoop_OverObject — object: as=value, index_as=key, iteration order
// is alphabetical by key.
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
		// Intentionally not alphabetical: bob, alice.
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
	// Alphabetical key order: alice, bob.
	if got := cmdOf(t, tasks[0]); got != "set alice rw" {
		t.Errorf("tasks[0] = %q, want 'set alice rw'", got)
	}
	if got := cmdOf(t, tasks[1]); got != "set bob ro" {
		t.Errorf("tasks[1] = %q, want 'set bob ro'", got)
	}
}

// TestRenderLoop_IndexAsArray — index_as for an array is a 0-based index.
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

// TestRenderLoop_WhenFilters — when: filters elements.
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

// TestRenderLoop_WhenBySoulprintRejected — when: referencing soulprint → a
// clear error. loop (items+when) is host-invariant in the pilot; a
// host-varying predicate on a specific host's soulprint isn't supported
// (per-host loop filtering is deferred), and must NOT be silently resolved
// against the first host (bug 2).
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

// TestRenderLoop_WhenFiltersAll — when: filters out ALL elements → 0 tasks, no
// panic (a valid no-op, same as empty items).
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

// TestRenderLoop_WhenNonBool — when: returns non-bool → a clear error (the
// predicate must return a boolean).
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

// TestRenderLoop_WithRunOnce — loop + run_once together: run_once trims the
// target to a single host (by SID), the whole loop runs on it (iterations on
// that one host). Allowed: the run_once (target) and loop (iteration) axes are
// orthogonal.
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
	// 3 iterations, each on the single host (first by SID — h1).
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

// TestRenderLoop_InDestinyExpands — loop: a task inside a destiny EXPANDS
// (slice E lifted, guardDestinyTask no longer cuts loop): one destiny loop task
// yields N RenderedTask with continuous indices carrying on from earlier
// destiny tasks. items comes from destiny-input (passed via apply.input).
// Broader destiny-loop coverage lives in destiny_loop_test.go; this is a
// minimal regression alongside the loop mechanics.
func TestRenderLoop_InDestinyExpands(t *testing.T) {
	d := flatDestiny()
	// destiny-input receives xs (an item array) via apply.input; the destiny's
	// second task fans out via loop over it.
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
	// task0 (marker, Index 0) + loop×3 (Index 1,2,3) = 4 tasks, continuous indices.
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

// TestRenderLoop_DefaultAs — as: omitted → variable defaults to item.
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

// TestRenderLoop_WithWhere — loop runs on EVERY host passing where:; per-
// iteration host invariance still holds (params match across hosts).
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
	// 2 iterations (a, b); each targets only the debian hosts (deb1, deb2).
	if len(tasks) != 2 || len(plans) != 2 {
		t.Fatalf("len(tasks)=%d len(plans)=%d, want 2,2", len(tasks), len(plans))
	}
	for i, pl := range plans {
		if len(pl.TargetSIDs) != 2 || pl.TargetSIDs[0] != "deb1" || pl.TargetSIDs[1] != "deb2" {
			t.Errorf("plans[%d].TargetSIDs = %v, want [deb1 deb2]", i, pl.TargetSIDs)
		}
	}
}

// TestRenderLoop_WithSerial — loop under serial: runs in full on each host of
// a wave; SerialWidth is inherited by every iteration (orthogonal axes).
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

// TestRenderLoop_PerIterationHostInvariant — host-dependent params WITHIN an
// iteration (different hosts yield different results) → host-invariance
// error; the check applies per iteration.
func TestRenderLoop_PerIterationHostInvariant(t *testing.T) {
	// params depends on both the loop variable and the host's soulprint: on
	// different hosts, one iteration yields different params — a host-invariance
	// violation.
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

// TestRenderLoop_PerIterationDifferentParamsOK — different params ACROSS
// iterations (but host-invariant within each) is normal, not an error.
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

// TestRenderLoop_EmptyItems — empty items → 0 tasks (a valid no-op).
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

// TestRenderLoop_NonCollectionItems — items that doesn't resolve to an
// array/object → error.
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

// whenLoopTask builds a module task with loop: + a static when: for the
// static-when-skip tests.
func whenLoopTask(when string, loop *config.LoopSpec, params map[string]any) config.Task {
	t := loopTask(loop, params)
	t.When = when
	return t
}

// TestRenderLoop_StaticWhenSkip_UnresolvableItems — ★ bug case (ordering of
// static-when vs loop.items): a loop task with a statically-false when: and
// items pointing at a MISSING input key. static-when precedes loop fan-out
// (architect invariant): the task is skipped ENTIRELY BEFORE resolveLoopItems,
// so an absent key in items must NOT crash Render. Resolving items here would
// fail (no input.users) → 1 skip placeholder (Params==nil, When carried
// through, FlowContext≠nil, Index continuous).
func TestRenderLoop_StaticWhenSkip_UnresolvableItems(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "x",
		Tasks: []config.Task{whenLoopTask(
			"input.action == 'apply'", // static-false when action=update_acls
			&config.LoopSpec{Items: "${ input.users }", As: "user"},
			map[string]any{"cmd": "useradd ${ user.name }"},
		)},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"action": "update_acls"}, // users not passed
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

// TestRenderLoop_StaticWhenSkip_UnresolvableItems_ContinuousIndex — an
// unresolvable static-skip loop mid-plan: index stays continuous (1
// placeholder, not N).
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
	// before(0) + skip-placeholder(1) + after(2) = 3 tasks.
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

// TestRenderLoop_StaticTrueWhen_FansOut — classification reverse case:
// static-TRUE when: + loop → normal fan-out into N real tasks (an active
// branch is never skipped).
func TestRenderLoop_StaticTrueWhen_FansOut(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "x",
		Tasks: []config.Task{whenLoopTask(
			"input.action == 'apply'", // static-TRUE when action=apply
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

// TestRenderLoop_MixedWhen_NotStaticSkipped — classification reverse case:
// mixed-when (input + register) is NOT static → items resolves, fan-out is
// normal (register-dependent when is carried as a string, evaluated
// Soul-side). With resolvable items, the static-skip branch must NOT trigger.
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
	// register-dependent when → NOT static-skip: items resolves, fan-out over it.
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

// TestRenderLoop_StaticWhenSkip_ConsistentAcrossPassages — a static-false loop
// (unresolvable items) gives the same result on re-render (passage activates
// again). One input snapshot → the same 1-placeholder skip every time.
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

// TestRenderLoop_StaticWhenSkip_PreservesOnChanges — guard: a static-false loop
// task with onchanges: → the skip placeholder preserves the requisite names
// (loopSkipPlaceholder carries onChangesNames symmetrically to
// staticSkipPlaceholder) → the final resolveOnChanges maps them into
// OnChangesIdx. Without carrying them, names would be lost at the placeholder
// and OnChangesIdx would stay nil — a latent loss of requisites.
func TestRenderLoop_StaticWhenSkip_PreservesOnChanges(t *testing.T) {
	loopT := whenLoopTask(
		"input.action == 'apply'", // static-false when action=update_acls
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
		Input:       map[string]any{"action": "update_acls"}, // users NOT passed → static-skip BEFORE resolveLoopItems
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("h", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// probe(0) + 1 skip-placeholder(1) = 2 tasks.
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

// TestRenderLoop_OnApplyRejected — loop on an apply task is still out of pilot
// scope (guardPilotDSL rejects with ErrUnsupportedDSL).
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
