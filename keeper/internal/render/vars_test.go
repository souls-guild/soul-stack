package render

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// The task view on one host of a scenario pass (varview.go, ADR-0082 amendment
// 2026-10-15, points 4 and 5): the service rung under the scenario `vars:` rung
// under the block + task rung, each definition evaluated when it is read and
// reading `vars.<other>` through the same view.

// viewOf is the view task reads on the first host of in (a synthetic one when in
// has none) — the view renderTaskIter builds for it.
func viewOf(t *testing.T, in RenderInput, task config.Task) *cel.VarView {
	t.Helper()
	if in.Scenario == nil {
		in.Scenario = &config.ScenarioManifest{Name: "s"}
	}
	h := &topology.HostFacts{SID: "a.example.com"}
	if len(in.Hosts) > 0 {
		h = in.Hosts[0]
	}
	view, err := NewPipeline(nil, newEngine(t), nil, nil).taskView(in, task, h, hostVars(in, h, 1))
	if err != nil {
		t.Fatalf("taskView: %v", err)
	}
	return view
}

func lookupOK(t *testing.T, view *cel.VarView, name string) any {
	t.Helper()
	val, found, err := view.Lookup(name)
	if err != nil || !found {
		t.Fatalf("vars.%s: found=%v err=%v", name, found, err)
	}
	return val
}

// TestTaskView_NoOwnRungIsTheServiceView — a task without `vars:` in a scenario
// without `vars:` reads the run's service view itself, so a service var it reads is
// evaluated once for the run, not once per task (point 8).
func TestTaskView_NoOwnRungIsTheServiceView(t *testing.T) {
	in := RenderInput{ServiceVars: serviceView(t, map[string]any{"conf_dir": "/etc/redis"})}
	if got := viewOf(t, in, config.Task{Name: "plain"}); got != in.ServiceVars {
		t.Fatalf("a task with no rung of its own must read the service view itself, got a new view")
	}
}

// TestTaskView_TaskVarReadsInput — a task var reads the reader's input.
func TestTaskView_TaskVarReadsInput(t *testing.T) {
	view := viewOf(t, RenderInput{Input: map[string]any{"host": "10.0.0.1"}},
		config.Task{Vars: map[string]any{"addr": "${ input.host }"}})
	if got := lookupOK(t, view, "addr"); got != "10.0.0.1" {
		t.Errorf("vars.addr = %v, want 10.0.0.1", got)
	}
}

// TestTaskView_NonStringLiteralsPassThrough — a non-string definition is a literal.
func TestTaskView_NonStringLiteralsPassThrough(t *testing.T) {
	view := viewOf(t, RenderInput{}, config.Task{Vars: map[string]any{"port": int64(6379), "enabled": true}})
	if got := lookupOK(t, view, "port"); got != int64(6379) {
		t.Errorf("vars.port = %v, want 6379", got)
	}
	if got := lookupOK(t, view, "enabled"); got != true {
		t.Errorf("vars.enabled = %v, want true", got)
	}
}

// TestTaskView_WholeExpressionKeepsItsNativeType — a lone ${expr} yields the
// native type of its result (templating.md §5(a)), as in params.
func TestTaskView_WholeExpressionKeepsItsNativeType(t *testing.T) {
	view := viewOf(t, RenderInput{Input: map[string]any{"n": int64(5)}},
		config.Task{Vars: map[string]any{"count": "${ input.n }"}})
	if got := lookupOK(t, view, "count"); got != int64(5) {
		t.Errorf("vars.count = %v (%T), want native int64 5", got, got)
	}
}

// TestTaskView_VarToVarInTheTaskRungIgnoresOrder — a task var reads another of the
// same rung whatever the order of declaration: nothing is sorted, a name is
// evaluated when it is read.
func TestTaskView_VarToVarInTheTaskRungIgnoresOrder(t *testing.T) {
	view := viewOf(t, RenderInput{Input: map[string]any{"host": "h"}}, config.Task{Vars: map[string]any{
		"b": "${ vars.a }-x",
		"a": "${ input.host }",
	}})
	if got := lookupOK(t, view, "b"); got != "h-x" {
		t.Errorf("vars.b = %v, want h-x", got)
	}
}

// TestTaskView_TaskVarReadsAServiceVar — a task var reaches down into the service
// rung (ADR-0082: one `vars.*` root), and the service var stays readable beside it.
func TestTaskView_TaskVarReadsAServiceVar(t *testing.T) {
	in := RenderInput{ServiceVars: serviceView(t, map[string]any{"conf_dir": "/etc/redis"})}
	view := viewOf(t, in, config.Task{Vars: map[string]any{"acl_path": "${ vars.conf_dir }/users.acl"}})
	if got := lookupOK(t, view, "acl_path"); got != "/etc/redis/users.acl" {
		t.Errorf("acl_path = %#v, want /etc/redis/users.acl", got)
	}
	if got := lookupOK(t, view, "conf_dir"); got != "/etc/redis" {
		t.Errorf("the service var must stay readable under the task rung: %#v", got)
	}
}

// TestTaskView_TaskVarDerivesFromTheNameItShadows — a self-reference reads the
// definition below its own rung (point 9): `conf_dir: ${ vars.conf_dir }/conf.d`.
func TestTaskView_TaskVarDerivesFromTheNameItShadows(t *testing.T) {
	in := RenderInput{ServiceVars: serviceView(t, map[string]any{"conf_dir": "/etc/redis"})}
	view := viewOf(t, in, config.Task{Vars: map[string]any{"conf_dir": "${ vars.conf_dir }/conf.d"}})
	if got := lookupOK(t, view, "conf_dir"); got != "/etc/redis/conf.d" {
		t.Fatalf("conf_dir = %#v, want /etc/redis/conf.d", got)
	}
}

// TestTaskView_SelfReferenceWithNothingBelowIsACycle — with no definition below to
// read, a self-reference is `var_cycle x → x` (point 9).
func TestTaskView_SelfReferenceWithNothingBelowIsACycle(t *testing.T) {
	view := viewOf(t, RenderInput{}, config.Task{Vars: map[string]any{"x": "${ vars.x }-loop"}})
	_, _, err := view.Lookup("x")
	if !errors.Is(err, ErrVarCycle) || !strings.Contains(err.Error(), "var_cycle: x → x") {
		t.Fatalf("err = %v, want var_cycle: x → x", err)
	}
}

// TestTaskView_MutualCycleIsACycleOverServiceNamesToo — two task vars reading each
// other are a cycle although the service rung defines both names below: neither is
// a self-reference.
func TestTaskView_MutualCycleIsACycleOverServiceNamesToo(t *testing.T) {
	in := RenderInput{ServiceVars: serviceView(t, map[string]any{"a": "A", "b": "B"})}
	view := viewOf(t, in, config.Task{Vars: map[string]any{"a": "${ vars.b }-1", "b": "${ vars.a }-2"}})
	for _, name := range []string{"a", "b"} {
		_, _, err := view.Lookup(name)
		if !errors.Is(err, ErrVarCycle) || !strings.Contains(err.Error(), "var_cycle: a → b → a") {
			t.Fatalf("vars.%s: err = %v, want var_cycle: a → b → a whichever is read first", name, err)
		}
	}
}

// TestTaskView_TaskVarShadowsServiceVar — the highest rung wins a name. The cost of
// one flat namespace, and why soul-lint warns about it (vars_shadows_service_var).
func TestTaskView_TaskVarShadowsServiceVar(t *testing.T) {
	in := RenderInput{ServiceVars: serviceView(t, map[string]any{"conf_dir": "/etc/redis"})}
	view := viewOf(t, in, config.Task{Vars: map[string]any{"conf_dir": "/opt/redis"}})
	if got := lookupOK(t, view, "conf_dir"); got != "/opt/redis" {
		t.Errorf("conf_dir = %#v, want the task value to shadow the service one", got)
	}
}

// TestTaskView_ServiceUnderScenarioUnderTask — the scenario pass's whole ladder:
// each rung supplies a name nobody else has, plus the one they all claim.
func TestTaskView_ServiceUnderScenarioUnderTask(t *testing.T) {
	in := RenderInput{
		ServiceVars: serviceView(t, map[string]any{"who": "service", "only_service": 1}),
		Scenario:    &config.ScenarioManifest{Name: "s", Vars: map[string]any{"who": "scenario", "only_scenario": 2}},
	}
	view := viewOf(t, in, config.Task{Vars: map[string]any{"who": "task", "only_task": 3}})
	if got := lookupOK(t, view, "who"); got != "task" {
		t.Errorf("who = %#v, want the highest rung to win", got)
	}
	for k, want := range map[string]any{"only_service": 1, "only_scenario": 2, "only_task": 3} {
		if got := lookupOK(t, view, k); got != want {
			t.Errorf("%s = %#v, want %#v — every rung must contribute", k, got, want)
		}
	}
}

// TestTaskView_TaskVarReadsSoulprintSelf — a task var reads the host it is
// evaluated for (destiny/tasks.md §9).
func TestTaskView_TaskVarReadsSoulprintSelf(t *testing.T) {
	in := RenderInput{Hosts: []*topology.HostFacts{host("a", nil, map[string]any{"os": map[string]any{"family": "debian"}})}}
	view := viewOf(t, in, config.Task{Vars: map[string]any{"fam": "${ soulprint.self.os.family }"}})
	if got := lookupOK(t, view, "fam"); got != "debian" {
		t.Errorf("vars.fam = %v, want debian", got)
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
