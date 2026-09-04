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
		t.Errorf("Vars = %v, want nil for empty task-vars", got.Vars)
	}

	got, err = resolveTaskVars(e, nil, map[string]any{}, base)
	if err != nil {
		t.Fatalf("resolveTaskVars(empty): %v", err)
	}
	if got.Vars != nil {
		t.Errorf("Vars = %v, want nil for an empty map", got.Vars)
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
		t.Fatalf("resolveTaskVars: var→var within the task layer must resolve: %v", err)
	}
	if got.Vars["a"] != "h" {
		t.Errorf("vars.a = %v, want h", got.Vars["a"])
	}
	if got.Vars["b"] != "h-x" {
		t.Errorf("vars.b = %v, want h-x (b refers to a of the same layer)", got.Vars["b"])
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
		t.Fatal("resolveTaskVars: task-var must not see file-var (cross-layer isolation)")
	}
	if !errors.Is(err, ErrVarUnknownRef) {
		t.Errorf("err = %v, want ErrVarUnknownRef (var_unknown_ref)", err)
	}
}

// TestResolveTaskVars_SeesServiceVarBelow — the merge of ADR-0082 in one test.
// A task var may reach DOWN into the service's own vars, because before the
// merge it reached them by spelling `${ essence.X }`, a different root that was
// always in scope. Refusing `${ vars.X }` now would turn a working scenario into
// var_unknown_ref for no reason an author could act on.
func TestResolveTaskVars_SeesServiceVarBelow(t *testing.T) {
	e := newEngine(t)
	// base.Vars is the service layer, exactly as hostVars seeds it.
	base := cel.Vars{Vars: map[string]any{"conf_dir": "/etc/redis"}}

	got, err := resolveTaskVars(e, nil, map[string]any{
		"acl_path": "${ vars.conf_dir }/users.acl",
	}, base)
	if err != nil {
		t.Fatalf("a task var must see the service layer below it: %v", err)
	}
	if got.Vars["acl_path"] != "/etc/redis/users.acl" {
		t.Errorf("acl_path = %#v, want /etc/redis/users.acl", got.Vars["acl_path"])
	}
	// The service var itself is still there — the task layer adds, it does not
	// replace the map.
	if got.Vars["conf_dir"] != "/etc/redis" {
		t.Errorf("the service layer must survive under the task layer: %#v", got.Vars)
	}
}

// TestResolveTaskVars_ShadowsAndDerivesFromTheSameName — the shape ADR-0082 §1
// sells as the profit of the merge, and the one a naive dependency graph turns
// into a cycle it cannot explain. A task var may redefine a service var IN TERMS
// OF the value it is shadowing; under two roots this was `${ essence.conf_dir }`
// and needed no rule.
func TestResolveTaskVars_ShadowsAndDerivesFromTheSameName(t *testing.T) {
	e := newEngine(t)
	base := cel.Vars{Vars: map[string]any{"conf_dir": "/etc/redis"}}

	got, err := resolveTaskVars(e, nil, map[string]any{
		"conf_dir": "${ vars.conf_dir }/conf.d",
	}, base)
	if err != nil {
		t.Fatalf("a task var must be able to derive from the same name below it: %v", err)
	}
	if got.Vars["conf_dir"] != "/etc/redis/conf.d" {
		t.Fatalf("conf_dir = %#v, want /etc/redis/conf.d", got.Vars["conf_dir"])
	}
}

// TestResolveTaskVars_SelfReferenceWithNothingBelowIsACycle — the other half.
// With no lower layer to read, a var referencing its own name IS a cycle, and
// must still be reported as one.
func TestResolveTaskVars_SelfReferenceWithNothingBelowIsACycle(t *testing.T) {
	e := newEngine(t)

	_, err := resolveTaskVars(e, nil, map[string]any{"x": "${ vars.x }-loop"}, cel.Vars{})
	if err == nil {
		t.Fatal("a self-reference with no layer below must be a cycle")
	}
	if !errors.Is(err, ErrVarCycle) {
		t.Fatalf("err = %v, want ErrVarCycle", err)
	}
}

// TestResolveTaskVars_MutualCycleStillCaught — shadow-and-derive must not blunt
// real cycle detection: two task vars referencing each other are still a cycle
// even when both names also exist below.
func TestResolveTaskVars_MutualCycleStillCaught(t *testing.T) {
	e := newEngine(t)
	base := cel.Vars{Vars: map[string]any{"a": "A", "b": "B"}}

	_, err := resolveTaskVars(e, nil, map[string]any{
		"a": "${ vars.b }-1",
		"b": "${ vars.a }-2",
	}, base)
	if err == nil {
		t.Fatal("a mutual reference between two task vars is still a cycle")
	}
	if !errors.Is(err, ErrVarCycle) {
		t.Fatalf("err = %v, want ErrVarCycle", err)
	}
}

// TestResolveTaskVars_ShadowsServiceVar — the ladder is outermost-first, so a
// task var of the same name WINS. This is the cost of one flat namespace, and
// the reason soul-lint warns about it (`vars_shadows_service_var`, NIM-416).
func TestResolveTaskVars_ShadowsServiceVar(t *testing.T) {
	e := newEngine(t)
	base := cel.Vars{Vars: map[string]any{"conf_dir": "/etc/redis"}}

	got, err := resolveTaskVars(e, nil, map[string]any{"conf_dir": "/opt/redis"}, base)
	if err != nil {
		t.Fatalf("resolveTaskVars: %v", err)
	}
	if got.Vars["conf_dir"] != "/opt/redis" {
		t.Errorf("conf_dir = %#v, want the task value to shadow the service one", got.Vars["conf_dir"])
	}
}

// TestResolveTaskVars_ServiceVarUnderFileVarUnderTaskVar — the whole ladder in
// one assertion: service < file < task. Each layer supplies a key nobody else
// has, plus the shared name they all claim.
func TestResolveTaskVars_ServiceVarUnderFileVarUnderTaskVar(t *testing.T) {
	e := newEngine(t)
	base := cel.Vars{Vars: map[string]any{"who": "service", "only_service": 1}}

	got, err := resolveTaskVars(e,
		map[string]any{"who": "file", "only_file": 2},
		map[string]any{"who": "task", "only_task": 3},
		base)
	if err != nil {
		t.Fatalf("resolveTaskVars: %v", err)
	}
	if got.Vars["who"] != "task" {
		t.Errorf("who = %#v, want the innermost layer to win", got.Vars["who"])
	}
	for k, want := range map[string]any{"only_service": 1, "only_file": 2, "only_task": 3} {
		if got.Vars[k] != want {
			t.Errorf("%s = %#v, want %#v — every layer must contribute", k, got.Vars[k], want)
		}
	}
}

// TestResolveTaskVars_StillCannotSeeFileVarThroughService — the downward opening
// must not become a sideways one. The service layer is below BOTH, so its
// presence gives a task var no path to a file var.
func TestResolveTaskVars_StillCannotSeeFileVarThroughService(t *testing.T) {
	e := newEngine(t)
	base := cel.Vars{Vars: map[string]any{"svc": "SERVICE"}}

	_, err := resolveTaskVars(e,
		map[string]any{"fv": "FILE"},
		map[string]any{"tv": "${ vars.fv }-from-task"},
		base)
	if err == nil {
		t.Fatal("a task var must still not see a file var, service layer or not")
	}
	if !errors.Is(err, ErrVarUnknownRef) {
		t.Errorf("err = %v, want ErrVarUnknownRef", err)
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
		t.Fatalf("resolveTaskVars: expected ErrVarCycle, got: %v", err)
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
		Incarnation: IncarnationMeta{ID: "svc"},
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
		Incarnation: IncarnationMeta{ID: "svc"},
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
		Incarnation: IncarnationMeta{ID: "svc"},
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
		Incarnation: IncarnationMeta{ID: "svc"},
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
		Incarnation: IncarnationMeta{ID: "svc"},
		Hosts:       []*topology.HostFacts{host("h", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2 (loop over 2 items)", len(tasks))
	}
	want := []string{"echo hi-a", "echo hi-b"}
	for i, w := range want {
		if got := tasks[i].Params.GetFields()["cmd"].GetStringValue(); got != w {
			t.Errorf("tasks[%d].command = %q, want %q", i, got, w)
		}
	}
}
