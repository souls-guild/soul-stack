package render

import (
	"context"
	"errors"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// TestResolveTaskVars_Empty proves an empty/nil task-vars leaves base untouched
// (the Vars field stays nil → normal no-such-key on vars.<key>).
func TestResolveTaskVars_Empty(t *testing.T) {
	e := newEngine(t)
	base := cel.Vars{Input: map[string]any{"x": "v"}}

	got, err := resolveTaskVars(e, nil, nil, base)
	if err != nil {
		t.Fatalf("resolveTaskVars(nil): %v", err)
	}
	if got.Vars != nil {
		t.Errorf("Vars = %v, want nil для пустых task-vars", got.Vars)
	}

	got, err = resolveTaskVars(e, nil, map[string]any{}, base)
	if err != nil {
		t.Fatalf("resolveTaskVars(empty): %v", err)
	}
	if got.Vars != nil {
		t.Errorf("Vars = %v, want nil для пустого map", got.Vars)
	}
}

// TestResolveTaskVars_FromInput proves a vars value referencing input is
// available as vars.<key>.
func TestResolveTaskVars_FromInput(t *testing.T) {
	e := newEngine(t)
	base := cel.Vars{Input: map[string]any{"host": "10.0.0.1"}}

	got, err := resolveTaskVars(e, nil, map[string]any{"addr": "${ input.host }"}, base)
	if err != nil {
		t.Fatalf("resolveTaskVars: %v", err)
	}
	if got.Vars["addr"] != "10.0.0.1" {
		t.Errorf("vars.addr = %v, want 10.0.0.1", got.Vars["addr"])
	}
}

// TestResolveTaskVars_NonStringPassthrough proves non-string vars values pass
// through as literals (CEL only touches strings, symmetric with params).
func TestResolveTaskVars_NonStringPassthrough(t *testing.T) {
	e := newEngine(t)
	got, err := resolveTaskVars(e, nil, map[string]any{
		"port":    int64(6379),
		"enabled": true,
	}, cel.Vars{})
	if err != nil {
		t.Fatalf("resolveTaskVars: %v", err)
	}
	if got.Vars["port"] != int64(6379) {
		t.Errorf("vars.port = %v, want 6379", got.Vars["port"])
	}
	if got.Vars["enabled"] != true {
		t.Errorf("vars.enabled = %v, want true", got.Vars["enabled"])
	}
}

// TestResolveTaskVars_NativeTypeSingleBlock proves a lone ${expr} yields a
// native type (number), not a string (templating.md §5(a)), same as in params.
func TestResolveTaskVars_NativeTypeSingleBlock(t *testing.T) {
	e := newEngine(t)
	base := cel.Vars{Input: map[string]any{"n": int64(5)}}

	got, err := resolveTaskVars(e, nil, map[string]any{"count": "${ input.n }"}, base)
	if err != nil {
		t.Fatalf("resolveTaskVars: %v", err)
	}
	if got.Vars["count"] != int64(5) {
		t.Errorf("vars.count = %v (%T), want native int64 5", got.Vars["count"], got.Vars["count"])
	}
}

// TestResolveTaskVars_VarToVar proves a task-var can reference ANOTHER
// task-var in the same layer (var→var within a layer is ALLOWED,
// eager-topological); declaration order doesn't matter (toposort). Guard test
// for the var→var invariant (case #1, task layer).
func TestResolveTaskVars_VarToVar(t *testing.T) {
	e := newEngine(t)
	base := cel.Vars{Input: map[string]any{"host": "h"}}

	got, err := resolveTaskVars(e, nil, map[string]any{
		"b": "${ vars.a }-x", // declared BEFORE a — order doesn't matter
		"a": "${ input.host }",
	}, base)
	if err != nil {
		t.Fatalf("resolveTaskVars: var→var внутри task-слоя должен резолвиться: %v", err)
	}
	if got.Vars["a"] != "h" {
		t.Errorf("vars.a = %v, want h", got.Vars["a"])
	}
	if got.Vars["b"] != "h-x" {
		t.Errorf("vars.b = %v, want h-x (b ссылается на a того же слоя)", got.Vars["b"])
	}
}

// TestResolveTaskVars_CannotSeeFileVar proves cross-layer isolation: a
// task-var cannot see a file-var (`${ vars.<file_var> }` → ErrVarUnknownRef,
// file-vars aren't in the task layer). Guard test for the isolation invariant
// (case #8, task→file).
func TestResolveTaskVars_CannotSeeFileVar(t *testing.T) {
	e := newEngine(t)
	base := cel.Vars{Input: map[string]any{"host": "h"}}

	_, err := resolveTaskVars(e,
		map[string]any{"fv": "FILE"},                   // file-vars (resolved)
		map[string]any{"tv": "${ vars.fv }-from-task"}, // task-var references a file-var
		base)
	if err == nil {
		t.Fatal("resolveTaskVars: task-var не должен видеть file-var (межслойная изоляция)")
	}
	if !errors.Is(err, ErrVarUnknownRef) {
		t.Errorf("err = %v, want ErrVarUnknownRef (var_unknown_ref)", err)
	}
}

// TestResolveTaskVars_Cycle proves a task-var→task-var cycle yields
// ErrVarCycle with a trace. Guard test (case #2/#4 on the task layer).
func TestResolveTaskVars_Cycle(t *testing.T) {
	e := newEngine(t)
	_, err := resolveTaskVars(e, nil, map[string]any{
		"a": "${ vars.b }",
		"b": "${ vars.a }",
	}, cel.Vars{})
	if err == nil || !errors.Is(err, ErrVarCycle) {
		t.Fatalf("resolveTaskVars: ожидался ErrVarCycle, получено: %v", err)
	}
}

// TestResolveTaskVars_FromSoulprintSelf proves vars can reference
// soulprint.self (destiny/tasks.md §9), resolved per-host.
func TestResolveTaskVars_FromSoulprintSelf(t *testing.T) {
	e := newEngine(t)
	base := cel.Vars{SoulprintSelf: map[string]any{"os": map[string]any{"family": "debian"}}}

	got, err := resolveTaskVars(e, nil, map[string]any{"fam": "${ soulprint.self.os.family }"}, base)
	if err != nil {
		t.Fatalf("resolveTaskVars: %v", err)
	}
	if got.Vars["fam"] != "debian" {
		t.Errorf("vars.fam = %v, want debian", got.Vars["fam"])
	}
}

// TestRender_VarsInParams is an end-to-end check: task-level vars: { addr:
// ${ input.host } } + params ${ vars.addr } → params resolve through vars.
func TestRender_VarsInParams(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "connect",
		Tasks: []config.Task{
			{
				Name: "ping addr",
				Vars: map[string]any{"addr": "${ input.host }"},
				Module: &config.ModuleTask{
					Module: "core.exec.run",
					Params: map[string]any{"cmd": "ping ${ vars.addr }"},
				},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"host": "10.0.0.1"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "ping 10.0.0.1" {
		t.Errorf("command = %q, want %q", got, "ping 10.0.0.1")
	}
}

// TestRender_VarsReusedAcrossParams proves a vars value simplifying a long
// expression can be reused by params multiple times.
func TestRender_VarsReusedAcrossParams(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "redis",
		Tasks: []config.Task{
			{
				Name: "render override",
				Vars: map[string]any{"unit": "${ input.svc }-staging"},
				Module: &config.ModuleTask{
					Module: "core.file.present",
					Params: map[string]any{
						"path":  "/etc/systemd/system/${ vars.unit }.service.d/override.conf",
						"label": "${ vars.unit }",
					},
				},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"svc": "redis-server"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	f := tasks[0].Params.GetFields()
	if got := f["path"].GetStringValue(); got != "/etc/systemd/system/redis-server-staging.service.d/override.conf" {
		t.Errorf("path = %q", got)
	}
	if got := f["label"].GetStringValue(); got != "redis-server-staging" {
		t.Errorf("label = %q, want redis-server-staging", got)
	}
}

// TestRender_VarsInWhere proves vars: is visible inside where: (bare
// vars.<key> in an expression-key), filtering hosts.
func TestRender_VarsInWhere(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "target",
		Tasks: []config.Task{
			{
				Name:  "only target host",
				Vars:  map[string]any{"target": "${ input.host }"},
				Where: "soulprint.self.sid == vars.target",
				Module: &config.ModuleTask{
					Module: "core.exec.run",
					Params: map[string]any{"cmd": "echo hit"},
				},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"host": "b.example.com"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts: []*topology.HostFacts{
			host("a.example.com", []string{"svc"}, nil),
			host("b.example.com", []string{"svc"}, nil),
		},
	}
	_, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := plans[0].TargetSIDs; len(got) != 1 || got[0] != "b.example.com" {
		t.Errorf("TargetSIDs = %v, want [b.example.com]", got)
	}
}

// TestRender_NoVars_NotBroken proves the absence of vars: doesn't break the
// render, while a vars.<key> reference in params without declared vars yields
// a no-such-key error (normal, like any unknown context).
func TestRender_NoVars_NotBroken(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "plain",
		Tasks: []config.Task{
			{
				Name:   "no vars",
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo ${ input.x }"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"x": "ok"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "echo ok" {
		t.Errorf("command = %q, want %q", got, "echo ok")
	}
}

// TestRender_VarsPerLoopIteration proves task-level vars: are recomputed on
// each loop iteration and can reference the loop variable <as>
// (destiny/tasks.md §12, open Q "composition with loop:" — settled as "yes,
// recomputed").
func TestRender_VarsPerLoopIteration(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "loop-vars",
		Tasks: []config.Task{
			{
				Name: "per item",
				Loop: &config.LoopSpec{Items: "${ input.names }", As: "item"},
				Vars: map[string]any{"greeting": "hi-${ item }"},
				Module: &config.ModuleTask{
					Module: "core.exec.run",
					Params: map[string]any{"cmd": "echo ${ vars.greeting }"},
				},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"names": []any{"a", "b"}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("h", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2 (loop по 2 элементам)", len(tasks))
	}
	want := []string{"echo hi-a", "echo hi-b"}
	for i, w := range want {
		if got := tasks[i].Params.GetFields()["cmd"].GetStringValue(); got != w {
			t.Errorf("tasks[%d].command = %q, want %q", i, got, w)
		}
	}
}
