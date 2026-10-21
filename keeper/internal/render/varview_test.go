package render

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/servicevars"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// The render-level guards of ADR-0082 amendment 2026-10-15: what each context
// reads of `vars.*` and when it is evaluated (varview.go).

// serviceView is a service view over defs as a run builds one: a single
// service-rung layer, evaluated against an incarnation with no fields.
func serviceView(t testing.TB, defs map[string]any) *cel.VarView {
	t.Helper()
	view, err := servicevars.ViewOf(defs, servicevars.IncarnationContext{})
	if err != nil {
		t.Fatalf("servicevars.ViewOf: %v", err)
	}
	return view
}

// confLayout is point 5's example: acl_file is derived from conf_dir.
func confLayout() map[string]any {
	return map[string]any{
		"conf_dir": "/etc/redis",
		"acl_file": "${ vars.conf_dir }/users.acl",
	}
}

func twoHosts() []*topology.HostFacts {
	return []*topology.HostFacts{
		host("a.example.com", []string{"svc"}, nil),
		host("b.example.com", []string{"svc"}, nil),
	}
}

func execTask(name string, vars map[string]any, cmd string) config.Task {
	return config.Task{Name: name, Vars: vars, Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": cmd}}}
}

func renderScenario(t *testing.T, in RenderInput) ([]*RenderedTask, error) {
	t.Helper()
	if in.Incarnation.ID == "" {
		in.Incarnation = IncarnationMeta{ID: "inc-1", Service: "svc"}
	}
	if in.Hosts == nil {
		in.Hosts = twoHosts()
	}
	tasks, _, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), in)
	return tasks, err
}

func cmdsOf(t *testing.T, tasks []*RenderedTask) []string {
	t.Helper()
	out := make([]string, len(tasks))
	for i, rt := range tasks {
		out[i] = cmdOf(t, rt)
	}
	return out
}

// TestLateBinding_ServiceVarFollowsTheTaskOverride — point 5: a service var derived
// from another reads it through the READING task's view, so a task overriding
// conf_dir gets its own acl_file, and a task without the override still gets the
// service's — before and after the overriding one, whatever the service view's
// memo holds by then.
func TestLateBinding_ServiceVarFollowsTheTaskOverride(t *testing.T) {
	tasks, err := renderScenario(t, RenderInput{
		Scenario: &config.ScenarioManifest{Name: "acl", Tasks: []config.Task{
			execTask("before", nil, "touch ${ vars.acl_file }"),
			execTask("override", map[string]any{"conf_dir": "/opt/redis"}, "touch ${ vars.acl_file }"),
			execTask("after", nil, "touch ${ vars.acl_file }"),
		}},
		ServiceVars: serviceView(t, confLayout()),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := []string{"touch /etc/redis/users.acl", "touch /opt/redis/users.acl", "touch /etc/redis/users.acl"}
	if got := cmdsOf(t, tasks); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("cmds = %q, want %q", got, want)
	}
}

// TestScenarioVars_ApplyToEveryTaskAndYieldToATask — point 2: the scenario's
// top-level `vars:` is a rung of every task view, evaluated over the reader's run
// data, and a task's own `vars:` overrides it — also for a scenario var derived
// from the name the task overrides.
func TestScenarioVars_ApplyToEveryTaskAndYieldToATask(t *testing.T) {
	tasks, err := renderScenario(t, RenderInput{
		Scenario: &config.ScenarioManifest{
			Name: "s",
			Vars: map[string]any{
				"region": "${ input.region }",
				"label":  "${ vars.region }-${ incarnation.id }",
			},
			Tasks: []config.Task{
				execTask("reads", nil, "echo ${ vars.label }"),
				execTask("overrides", map[string]any{"region": "us"}, "echo ${ vars.label } ${ vars.region }"),
			},
		},
		Input: map[string]any{"region": "eu"},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := []string{"echo eu-inc-1", "echo us-inc-1 us"}
	if got := cmdsOf(t, tasks); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("cmds = %q, want %q", got, want)
	}
}

// TestScenarioVars_RefuseHostAndRegisterRoots — point 6: a scenario `vars:`
// definition sees run data only, so a host root or a register is refused when it is
// compiled, as an undeclared reference.
func TestScenarioVars_RefuseHostAndRegisterRoots(t *testing.T) {
	for def, root := range map[string]string{
		"${ soulprint.self.sid }":    "soulprint",
		"${ register.probe.stdout }": "register",
	} {
		t.Run(root, func(t *testing.T) {
			_, err := renderScenario(t, RenderInput{
				Scenario: &config.ScenarioManifest{
					Name:  "s",
					Vars:  map[string]any{"x": def},
					Tasks: []config.Task{execTask("reads", nil, "echo ${ vars.x }")},
				},
				Register: map[string]any{"probe": map[string]any{"stdout": "out"}},
			})
			if err == nil || !strings.Contains(err.Error(), "undeclared reference to '"+root+"'") {
				t.Fatalf("err = %v, want an undeclared reference to %q", err, root)
			}
		})
	}
}

// TestDestinyVars_FileAndTaskRungsSeeEachOther — point 5 removes the file↔task
// isolation in both directions: a destiny task var reads a `vars.yml` name, and a
// `vars.yml` definition reads the task's override.
func TestDestinyVars_FileAndTaskRungsSeeEachOther(t *testing.T) {
	resolved := destinyWithFileVars(
		map[string]any{
			"base":     "redis",
			"conf_dir": "/etc/x",
			"acl":      "${ vars.conf_dir }/acl",
		},
		map[string]any{"cmd": "echo ${ vars.acl } ${ vars.unit }"},
	)
	resolved.Tasks[0].Vars = map[string]any{
		"conf_dir": "/opt/x",
		"unit":     "${ vars.base }-svc",
	}
	tasks, err := renderScenario(t, RenderInput{
		Scenario: applyScenario("pilot-vars", map[string]any{}),
		Destiny:  &stubDestinyResolver{resolved: resolved},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := cmdOf(t, tasks[0]); got != "echo /opt/x/acl redis-svc" {
		t.Fatalf("cmd = %q, want %q", got, "echo /opt/x/acl redis-svc")
	}
}

// TestLoopAxis_DoesNotSeeTheTaskRung — point 5: `loop.items:` reads the service and
// scenario rungs only. A task-rung definition is evaluated per iteration, so the set
// being iterated cannot depend on it; the task's params still read the task's own.
func TestLoopAxis_DoesNotSeeTheTaskRung(t *testing.T) {
	task := execTask("fan", map[string]any{"users": []any{"task-a", "task-b"}}, "echo ${ u } ${ size(vars.users) }")
	task.Loop = &config.LoopSpec{Items: "${ vars.users }", As: "u"}
	tasks, err := renderScenario(t, RenderInput{
		Scenario:    &config.ScenarioManifest{Name: "s", Tasks: []config.Task{task}},
		ServiceVars: serviceView(t, map[string]any{"users": []any{"svc-a"}}),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := cmdsOf(t, tasks); len(got) != 1 || got[0] != "echo svc-a 2" {
		t.Fatalf("cmds = %q, want one iteration over the service's users, params reading the task's: [echo svc-a 2]", got)
	}
}

// TestLoopAxis_RefusesAScenarioVarReadingCompute — point 6: a scenario definition is
// evaluated with its READER's compute scope, so one reading `compute.*` is refused on
// the loop axis (NIM-619) and read in a task's params.
func TestLoopAxis_RefusesAScenarioVarReadingCompute(t *testing.T) {
	scenario := func(task config.Task) *config.ScenarioManifest {
		return &config.ScenarioManifest{
			Name:    "s",
			Compute: config.ComputeBlock{{Name: "names", Value: "${ ['a', 'b'] }"}},
			Vars:    map[string]any{"names": "${ compute.names }"},
			Tasks:   []config.Task{task},
		}
	}

	loop := execTask("fan", nil, "echo ${ n }")
	loop.Loop = &config.LoopSpec{Items: "${ vars.names }", As: "n"}
	_, err := renderScenario(t, RenderInput{Scenario: scenario(loop)})
	var outOfScope *cel.ErrOutOfScope
	if !errors.As(err, &outOfScope) {
		t.Fatalf("loop.items over a scenario var reading compute: err = %v, want *cel.ErrOutOfScope", err)
	}

	tasks, err := renderScenario(t, RenderInput{Scenario: scenario(execTask("reads", nil, "echo ${ vars.names[1] }"))})
	if err != nil {
		t.Fatalf("a task's params reading the same scenario var: %v", err)
	}
	if got := cmdOf(t, tasks[0]); got != "echo b" {
		t.Fatalf("cmd = %q, want echo b", got)
	}
}

// TestAssert_WhenGateReadsTheTasksOwnVars — point 7, a breaking change: an
// `assert:` task's own `when:` gate reads the task's view, so its `vars:` override
// the service's — at render and in the pre-flight's EvalAsserts alike. The assert
// would fail if it ran.
func TestAssert_WhenGateReadsTheTasksOwnVars(t *testing.T) {
	in := RenderInput{
		Scenario: &config.ScenarioManifest{Name: "s", Tasks: []config.Task{{
			Name:   "cluster only",
			Vars:   map[string]any{"cluster": false},
			When:   "vars.cluster",
			Assert: &config.AssertSpec{That: []string{"false"}, Message: "the cluster assert ran"},
		}}},
		ServiceVars: serviceView(t, map[string]any{"cluster": true}),
		Incarnation: IncarnationMeta{ID: "inc-1", Service: "svc"},
		Hosts:       twoHosts(),
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	if _, _, err := p.Render(context.Background(), in); err != nil {
		t.Fatalf("Render: the gate must read the task's cluster=false and skip the assert: %v", err)
	}
	if err := p.EvalAsserts(context.Background(), in); err != nil {
		t.Fatalf("EvalAsserts: the gate must read the task's cluster=false and skip the assert: %v", err)
	}
}

// TestRenderContext_VarsEqualParamsUnderATaskOverride — point 17: render_context's
// values come from the task's view, so `.vars.acl_file` is what `${ vars.acl_file }`
// is in the same task's params, the task's override of conf_dir included.
func TestRenderContext_VarsEqualParamsUnderATaskOverride(t *testing.T) {
	const tmplPath = "templates/acl.tmpl"
	tasks, err := renderScenario(t, RenderInput{
		Scenario: &config.ScenarioManifest{Name: "s", Tasks: []config.Task{{
			Name: "write acl",
			Vars: map[string]any{"conf_dir": "/opt/redis"},
			Module: &config.ModuleTask{Module: moduleFileRendered, Params: map[string]any{
				"path":     "${ vars.acl_file }",
				"template": tmplPath,
			}},
		}}},
		ServiceVars: serviceView(t, confLayout()),
		Templates:   fakeReader{files: map[string][]byte{tmplPath: []byte("{{ .vars.acl_file }} in {{ .vars.conf_dir }}\n")}},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	fields := tasks[0].Params.GetFields()
	path := fields["path"].GetStringValue()
	rc := fields[paramRenderContext].GetStructValue().AsMap()["vars"].(map[string]any)
	if path != "/opt/redis/users.acl" || rc["acl_file"] != path || rc["conf_dir"] != "/opt/redis" {
		t.Fatalf("params.path = %q, render_context.vars = %#v; want both acl_file = /opt/redis/users.acl and conf_dir = /opt/redis", path, rc)
	}
}

// TestRenderContext_WholeReadWidensServiceAndScenarioNotVarsYml — point 17: a whole
// read of `.vars` carries every name of the service and scenario rungs, never a
// task-only name, and in a destiny pass never a `vars.yml` name the template does
// not spell.
func TestRenderContext_WholeReadWidensServiceAndScenarioNotVarsYml(t *testing.T) {
	const tmplPath = "templates/all.tmpl"
	const wholeAndNamed = "{{ range $k, $v := .vars }}{{ $k }} {{ end }}{{ .vars.bin }}\n"
	reader := fakeReader{files: map[string][]byte{tmplPath: []byte(wholeAndNamed)}}
	rendered := config.Task{
		Name: "render",
		Vars: map[string]any{"task_only": "t"},
		Module: &config.ModuleTask{Module: moduleFileRendered, Params: map[string]any{
			"path": "/etc/x.conf", "template": tmplPath,
		}},
	}
	rcVars := func(t *testing.T, rt *RenderedTask) map[string]any {
		t.Helper()
		return rt.Params.GetFields()[paramRenderContext].GetStructValue().AsMap()["vars"].(map[string]any)
	}

	t.Run("scenario pass", func(t *testing.T) {
		tasks, err := renderScenario(t, RenderInput{
			Scenario:    &config.ScenarioManifest{Name: "s", Vars: map[string]any{"region": "eu"}, Tasks: []config.Task{rendered}},
			ServiceVars: serviceView(t, map[string]any{"bin": "/usr/bin/x", "port": 6379}),
			Templates:   reader,
		})
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		got := rcVars(t, tasks[0])
		if len(got) != 3 || got["bin"] != "/usr/bin/x" || got["region"] != "eu" || got["port"] == nil {
			t.Fatalf("render_context.vars = %#v, want bin, port and region and no task-only name", got)
		}
	})

	t.Run("destiny pass", func(t *testing.T) {
		resolved := &ResolvedDestiny{
			Name:      "d",
			Vars:      map[string]any{"bin": "/usr/bin/x", "internal": "plumbing"},
			Templates: reader,
			Tasks:     []config.Task{rendered},
		}
		tasks, err := renderScenario(t, RenderInput{
			Scenario: applyScenario("d", map[string]any{}),
			Destiny:  &stubDestinyResolver{resolved: resolved},
		})
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		if got := rcVars(t, tasks[0]); len(got) != 1 || got["bin"] != "/usr/bin/x" {
			t.Fatalf("render_context.vars = %#v, want only the vars.yml name the template spells: bin", got)
		}
	})
}

// TestNotRequested_BrokenVarsNobodyReadsDoNotFailTheRender — point 4: a name nobody
// reads is never evaluated, a broken one included, in every rung.
func TestNotRequested_BrokenVarsNobodyReadsDoNotFailTheRender(t *testing.T) {
	const broken = "${ vars.nope }"
	cases := map[string]RenderInput{
		"service rung": {
			Scenario:    &config.ScenarioManifest{Name: "s", Tasks: []config.Task{execTask("t", nil, "echo ${ vars.used }")}},
			ServiceVars: serviceView(t, map[string]any{"used": "ok", "broken": broken}),
		},
		"scenario rung": {
			Scenario: &config.ScenarioManifest{Name: "s", Vars: map[string]any{"used": "ok", "broken": broken},
				Tasks: []config.Task{execTask("t", nil, "echo ${ vars.used }")}},
		},
		"task rung": {
			Scenario: &config.ScenarioManifest{Name: "s", Tasks: []config.Task{
				execTask("t", map[string]any{"used": "ok", "broken": broken}, "echo ${ vars.used }"),
			}},
		},
		"vars.yml": {
			Scenario: applyScenario("pilot-vars", map[string]any{}),
			Destiny: &stubDestinyResolver{resolved: destinyWithFileVars(
				map[string]any{"used": "ok", "broken": broken},
				map[string]any{"cmd": "echo ${ vars.used }"},
			)},
		},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			tasks, err := renderScenario(t, in)
			if err != nil {
				t.Fatalf("Render: a broken var nothing reads must not fail the render: %v", err)
			}
			if got := cmdOf(t, tasks[0]); got != "echo ok" {
				t.Fatalf("cmd = %q, want echo ok", got)
			}
		})
	}
}

// TestVarCycle_ReportsTheNormalizedPath — points 10 and 11: a cycle is var_cycle
// with the path rotated to its smallest name, within a rung and across rungs, so
// the text does not depend on which var a cell asked for.
func TestVarCycle_ReportsTheNormalizedPath(t *testing.T) {
	for name, tc := range map[string]struct {
		service map[string]any
		task    map[string]any
		read    string
		want    string
	}{
		"within the task rung": {
			task: map[string]any{"b": "${ vars.c }", "a": "${ vars.b }", "c": "${ vars.a }"},
			read: "b", want: "var_cycle: a → b → c → a",
		},
		"across the service and task rungs": {
			service: map[string]any{"a": "${ vars.b }"},
			task:    map[string]any{"b": "${ vars.a }"},
			read:    "b", want: "var_cycle: a → b → a",
		},
	} {
		t.Run(name, func(t *testing.T) {
			in := RenderInput{Scenario: &config.ScenarioManifest{Name: "s", Tasks: []config.Task{
				execTask("t", tc.task, "echo ${ vars."+tc.read+" }"),
			}}}
			if tc.service != nil {
				in.ServiceVars = serviceView(t, tc.service)
			}
			_, err := renderScenario(t, in)
			if !errors.Is(err, ErrVarCycle) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestFlowContext_VarsCarriesOnlyTheNamesItsPredicatesRead — point 17:
// flow_context.vars is a request against the task's view: exactly the names the
// task's predicates read, with late-bound values, and no evaluation of the rest — a
// broken var the predicates do not read included.
func TestFlowContext_VarsCarriesOnlyTheNamesItsPredicatesRead(t *testing.T) {
	task := execTask("touch", map[string]any{"conf_dir": "/opt/redis"}, "true")
	task.ChangedWhen = "vars.acl_file == '/opt/redis/users.acl'"
	defs := confLayout()
	defs["broken"] = "${ vars.nope }"
	tasks, err := renderScenario(t, RenderInput{
		Scenario:    &config.ScenarioManifest{Name: "s", Tasks: []config.Task{task}},
		ServiceVars: serviceView(t, defs),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := tasks[0].FlowContext.AsMap()["vars"].(map[string]any)
	if len(got) != 1 || got["acl_file"] != "/opt/redis/users.acl" {
		t.Fatalf("flow_context.vars = %#v, want exactly {acl_file: /opt/redis/users.acl}", got)
	}
}

// TestEvalBoolExpr_ALazyVarsResultIsNamedByItsType — a predicate that returns the
// `vars` map itself is refused by naming its type; nothing is evaluated to say so,
// so a broken var cannot turn the message into its own error.
func TestEvalBoolExpr_ALazyVarsResultIsNamedByItsType(t *testing.T) {
	view := serviceView(t, map[string]any{"broken": "${ vars.nope }"})
	_, err := evalBoolExpr(newEngine(t), "where", "vars", cel.Vars{View: view})
	if err == nil || !strings.Contains(err.Error(), "returned map, expected bool") {
		t.Fatalf("err = %v, want it to name the map type", err)
	}
	if n := view.Evaluations(); n != 0 {
		t.Fatalf("naming the result's type evaluated %d definitions, want 0", n)
	}
}

// TestMemo_AnUntouchedServiceVarIsEvaluatedOncePerRender — point 8: a service var no
// task override touches is computed once per service view, however many tasks × hosts
// read it. Each task here has a view of its own (an unrelated task var), and every one
// reuses the value the first stored in the run's service view.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: build a task view as a stack of
// its own rather than on the service view — `cel.NewVarView(nil,
// append(in.ServiceVars.Layers(), layers...)...)` in taskView. The service view then
// evaluates nothing and each task × host evaluates port again.
func TestMemo_AnUntouchedServiceVarIsEvaluatedOncePerRender(t *testing.T) {
	svc := serviceView(t, map[string]any{"port": "${ 6000 + 379 }"})
	var tasks []config.Task
	for _, name := range []string{"a", "b", "c"} {
		tasks = append(tasks, execTask(name, map[string]any{"own_" + name: name}, "echo ${ vars.port }"))
	}
	rendered, err := renderScenario(t, RenderInput{
		Scenario:    &config.ScenarioManifest{Name: "s", Tasks: tasks},
		ServiceVars: svc,
		Hosts:       twoHosts(),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := cmdsOf(t, rendered); strings.Join(got, "|") != "echo 6379|echo 6379|echo 6379" {
		t.Fatalf("cmds = %q", got)
	}
	if n := svc.Evaluations(); n != 1 {
		t.Fatalf("the service view evaluated port %d times for 3 tasks × 2 hosts, want 1", n)
	}
}

// cyclicGroup is shared/cel's case of that name: n definitions on one ring of reads
// three wide, v_i reading v_{i+1}, v_{i+2} and v_{i+3} modulo n.
func cyclicGroup(n int) map[string]any {
	defs := make(map[string]any, n)
	for i := range n {
		defs[fmt.Sprintf("v%02d", i)] = fmt.Sprintf("${ string(vars.v%02d) + string(vars.v%02d) + string(vars.v%02d) }", (i+1)%n, (i+2)%n, (i+3)%n)
	}
	return defs
}

// failingLattice is shared/cel's case of that name: n acyclic definitions, v_i reading
// v_{i+1} and v_{i+2}, over a last one that divides by zero.
func failingLattice(n int) map[string]any {
	defs := make(map[string]any, n)
	for i := range n - 1 {
		defs[fmt.Sprintf("v%02d", i)] = fmt.Sprintf("${ vars.v%02d + vars.v%02d }", i+1, min(i+2, n-1))
	}
	defs[fmt.Sprintf("v%02d", n-1)] = "${ 1 / 0 }"
	return defs
}

// TestFailingVars_ARenderReturnsTheFailurePromptly — point 8, at the Keeper render: a
// task whose params read `vars.v00` fails with the error v00 always reported, over a
// service rung where every path to the failure would evaluate it again. A read
// evaluates a failed definition once, so the views the render builds evaluate at most n
// definitions per host read; before that, one read of a 20-var cyclic group took 33 s.
// The counter is watched while the render runs, so a render that re-evaluates fails on
// it; the 1 s deadline is only a backstop.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code, either of:
//   - drop the failure-memo lookup, `if f, ok := read.failed[key]; ok { return nil, false, f }`,
//     from cel.VarView.resolve;
//   - drop every kept failure whenever an evaluation returns: `clear(r.failed)` and
//     `return 0` at the top of cel's varRead.settle.
//
// Both cases go red on the counter.
func TestFailingVars_ARenderReturnsTheFailurePromptly(t *testing.T) {
	ring := make([]string, 0, 25)
	for i := range 24 {
		ring = append(ring, fmt.Sprintf("v%02d", i))
	}
	hosts := twoHosts()
	for name, tc := range map[string]struct {
		defs map[string]any
		want string
	}{
		"a 24-var cyclic group":    {cyclicGroup(24), "var_cycle: " + strings.Join(append(ring, "v00"), " → ")},
		"a 30-var failing lattice": {failingLattice(30), `vars.v29 (service vars): CEL eval "1 / 0": division by zero`},
	} {
		t.Run(name, func(t *testing.T) {
			svc := serviceView(t, tc.defs)
			p := NewPipeline(nil, newEngine(t), nil, nil)
			var mu sync.Mutex
			views := []*cel.VarView{svc}
			p.onView = func(v *cel.VarView) {
				mu.Lock()
				defer mu.Unlock()
				views = append(views, v)
			}
			evaluations := func() int64 {
				mu.Lock()
				defer mu.Unlock()
				var n int64
				for _, v := range views {
					n += v.Evaluations()
				}
				return n
			}
			in := RenderInput{
				Scenario:    &config.ScenarioManifest{Name: "s", Tasks: []config.Task{execTask("t", map[string]any{"own": "x"}, "echo ${ vars.v00 }")}},
				ServiceVars: svc,
				Hosts:       hosts,
				Incarnation: IncarnationMeta{ID: "inc-1", Service: "svc"},
			}
			done := make(chan error, 1)
			start := time.Now()
			go func() {
				_, _, err := p.Render(context.Background(), in)
				done <- err
			}()
			limit := int64(len(tc.defs) * len(hosts))
			backstop := time.After(time.Second)
			tick := time.NewTicker(time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case err := <-done:
					n := evaluations()
					t.Logf("returned in %v after %d evaluations", time.Since(start), n)
					if err == nil || !strings.Contains(err.Error(), tc.want) {
						t.Fatalf("err = %v, want it to carry %q", err, tc.want)
					}
					if n < int64(len(tc.defs)) || n > limit {
						t.Fatalf("%d evaluations, want between %d, one read, and %d, one per host", n, len(tc.defs), limit)
					}
					return
				case <-tick.C:
					if n := evaluations(); n > limit {
						t.Fatalf("%d evaluations after %v and the render still running, want at most %d: %d definitions per host read", n, time.Since(start), limit, len(tc.defs))
					}
				case <-backstop:
					t.Fatalf("still rendering after 1s, %d evaluations", evaluations())
				}
			}
		})
	}
}

// TestFailingVars_AnAbsorbedCycleIsNotKeptPastItsPath — point 8, at the Keeper render:
// shared/cel's cases A and B as a service rung. In each, b fails on a cycle through a
// that a absorbs, and b read again outside a reads a's value: A renders both values, B
// fails on its missing c instead, with and without strict mode.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in cel's varRead.settle,
// `if ok {` → `if false {`. A goes red on the task reading both names and, under
// strict mode, on the one reading neither; B on the error text.
func TestFailingVars_AnAbsorbedCycleIsNotKeptPastItsPath(t *testing.T) {
	const noSuchC = `vars.b (service vars): CEL eval "vars.a ? vars.c : 0": no such key: c`
	a := map[string]any{"a": "${ vars['b' + ''] || true }", "b": "${ vars.a }"}
	b := map[string]any{"a": "${ vars.b == 1 || true }", "b": "${ vars.a ? vars.c : 0 }"}
	const both = "echo ${ string(vars.a) + string(vars.b) }"
	for _, tc := range []struct {
		name   string
		defs   map[string]any
		cmd    string
		strict bool
		want   string // each host's command, or the end of the error
	}{
		{"A, reading neither", a, "echo ok", false, "echo ok"},
		{"A, reading neither, strict", a, "echo ok", true, "echo ok"},
		{"A, reading both", a, both, false, "echo truetrue"},
		{"A, reading both, strict", a, both, true, "echo truetrue"},
		{"B, reading neither", b, "echo ok", false, "echo ok"},
		{"B, reading neither, strict", b, "echo ok", true, noSuchC},
		{"B, reading both", b, both, false, noSuchC},
		{"B, reading both, strict", b, both, true, noSuchC},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tasks, err := renderScenario(t, RenderInput{
				Scenario:    &config.ScenarioManifest{Name: "s", Tasks: []config.Task{execTask("t", nil, tc.cmd)}},
				ServiceVars: serviceView(t, tc.defs),
				StrictVars:  tc.strict,
			})
			if tc.want == noSuchC {
				if err == nil || !strings.HasSuffix(err.Error(), noSuchC) {
					t.Fatalf("err = %v, want it to end in %q", err, noSuchC)
				}
				return
			}
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			for _, cmd := range cmdsOf(t, tasks) {
				if cmd != tc.want {
					t.Fatalf("cmd = %q, want %q", cmd, tc.want)
				}
			}
		})
	}
}

// TestHostVariantVarNames_ACheckCostsWhatTheDefinitionReads — point 16: viewClosure
// checks every definition on every pass while its closed set grows, so one check must
// cost what the definition reads, never what is closed. readsHostRoots looks each
// `vars.<x>` a definition reads up in the closed set: a definition reading one name is
// checked 5000 times against 200 000 closed names well inside a second, and a
// 2000-definition chain over a `soulprint.self` read closes whole. Walking the closed
// set instead made the closure cubic on a chain: 500 definitions took 0.2 s, 1000
// 1.4 s, 2000 11.4 s. The chain itself is not timed: under -race it takes 0.7 s.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in readsHostRoots, replace
// `if readsAny(reads[varsRootName], variantVars) { return true }` with the walk over the
// closed set, `vr := reads[varsRootName]; if len(variantVars) > 0 && vr.Whole { return true };
// for name := range variantVars { if vr.Fields[name] { return true } }`. The checks go
// red on the deadline.
func TestHostVariantVarNames_ACheckCostsWhatTheDefinitionReads(t *testing.T) {
	closed := make(map[string]bool, 200_000)
	for i := range 200_000 {
		closed[fmt.Sprintf("c%06d", i)] = true
	}
	reads := map[string]cel.RootReads{varsRootName: {Fields: map[string]bool{"other": true}}}
	start := time.Now()
	for i := range 5000 {
		if readsHostRoots(reads, closed, nil, nil) {
			t.Fatal("a read of a name outside the closed set classified as per-host")
		}
		if i%100 == 0 && time.Since(start) > time.Second {
			t.Fatalf("%d checks in %v, want 5000 inside 1s", i, time.Since(start))
		}
	}
	t.Logf("5000 checks against %d closed names in %v", len(closed), time.Since(start))

	const n = 2000
	chain := make(map[string]any, n+1)
	for i := range n - 1 {
		chain[fmt.Sprintf("v%04d", i)] = fmt.Sprintf("${ vars.v%04d }", i+1)
	}
	chain[fmt.Sprintf("v%04d", n-1)] = "${ soulprint.self.network.primary_ip }"
	chain["timeout"] = "30s"
	start = time.Now()
	got := hostVariantVarNames(newEngine(t), []cel.VarLayer{{Rung: cel.RungTask, Defs: chain}}, nil, nil)
	t.Logf("a %d-definition chain closed in %v", n, time.Since(start))
	if len(got) != n || got["timeout"] {
		t.Fatalf("closed %d names, timeout among them: %v; want the chain's %d", len(got), got["timeout"], n)
	}
}

// TestScenarioRung_IsEvaluatedWithEachReadersRunLevelValues — points 5 and 6: the
// scenario rung is a rung of each task view, never of a shared parent, because its
// definitions read the READER's run-level values: a host task's host_count is its
// targeted hosts, a keeper task's is 0. In one render, in either order.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: evaluate the scenario rung with
// one run-level reader for every task — `Base: scenarioRungBase(hostVars(in,
// &topology.HostFacts{}, len(in.Hosts)))` in scenarioRung, the values a shared parent
// would carry. The keeper task then reads 2.
func TestScenarioRung_IsEvaluatedWithEachReadersRunLevelValues(t *testing.T) {
	hostTask := execTask("on hosts", nil, "echo ${ vars.n }")
	keeperTask := config.Task{Name: "on keeper", On: config.KeeperTarget, Module: &config.ModuleTask{
		Module: "core.state.present", Params: map[string]any{"path": "n", "value": "${ vars.n }"},
	}}
	for name, order := range map[string][]config.Task{
		"host task first":   {hostTask, keeperTask},
		"keeper task first": {keeperTask, hostTask},
	} {
		t.Run(name, func(t *testing.T) {
			rendered, err := renderScenario(t, RenderInput{
				Scenario: &config.ScenarioManifest{Name: "s", Vars: map[string]any{"n": "${ incarnation.host_count }"}, Tasks: order},
				Hosts:    twoHosts(),
			})
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			for _, rt := range rendered {
				switch rt.Name {
				case hostTask.Name:
					if got := cmdOf(t, rt); got != "echo 2" {
						t.Errorf("host task cmd = %q, want echo 2", got)
					}
				case keeperTask.Name:
					if got := rt.Params.GetFields()["value"].GetNumberValue(); got != 0 {
						t.Errorf("keeper task value = %v, want 0", got)
					}
				}
			}
		})
	}
}

// TestIncludeWhen_ReadsTheScenarioRungNotTheTasksOwnVars — point 5: an include
// `when:` decides whether its group exists, so it reads the service and scenario rungs
// and never a task's own `vars:`, even one of the same name.
func TestIncludeWhen_ReadsTheScenarioRungNotTheTasksOwnVars(t *testing.T) {
	task := execTask("included", map[string]any{"mode": "off"}, "echo ${ vars.mode }")
	task.IncludeGroupID, task.IncludeWhen = 1, "vars.mode == 'on'"
	rendered, err := renderScenario(t, RenderInput{
		Scenario: &config.ScenarioManifest{Name: "s", Vars: map[string]any{"mode": "on"}, Tasks: []config.Task{task}},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := cmdsOf(t, rendered); len(got) != 1 || got[0] != "echo off" {
		t.Fatalf("cmds = %q, want the group kept by the scenario's mode and the task's own in its params", got)
	}
}

// TestIncludeWhen_InADestinyReadsVarsYml — point 7: in a destiny pass an include
// `when:` reads the first host's `vars.yml` view.
func TestIncludeWhen_InADestinyReadsVarsYml(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("enabled %v", enabled), func(t *testing.T) {
			resolved := destinyWithFileVars(
				map[string]any{"enabled": enabled, "unit": "redis"},
				map[string]any{"cmd": "systemctl restart ${ vars.unit }"},
			)
			resolved.Tasks[0].IncludeGroupID, resolved.Tasks[0].IncludeWhen = 1, "vars.enabled"
			rendered, err := renderScenario(t, RenderInput{
				Scenario: applyScenario("pilot-vars", map[string]any{}),
				Destiny:  &stubDestinyResolver{resolved: resolved},
			})
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if kept := len(rendered) == 1; kept != enabled {
				t.Fatalf("rendered %d tasks, want the group kept = %v", len(rendered), enabled)
			}
		})
	}
}

// TestTemplatePath_ReadsTheServiceAndScenarioRungsNotTheTasks — point 5: the
// `template:` path of `core.file.rendered` chooses the one file Keeper reads, before
// any host and before the task's own `vars:`, so it reads the service and scenario
// rungs only — a task override of the names it reads does not reach it.
func TestTemplatePath_ReadsTheServiceAndScenarioRungsNotTheTasks(t *testing.T) {
	rendered, err := renderScenario(t, RenderInput{
		Scenario: &config.ScenarioManifest{
			Name: "s",
			Vars: map[string]any{"tmpl": "${ vars.dir }/scenario.tmpl"},
			Tasks: []config.Task{{
				Name: "render",
				Vars: map[string]any{"dir": "task", "tmpl": "task/task.tmpl"},
				Module: &config.ModuleTask{Module: moduleFileRendered, Params: map[string]any{
					"path": "/etc/x.conf", "template": "${ vars.tmpl }",
				}},
			}},
		},
		ServiceVars: serviceView(t, map[string]any{"dir": "templates"}),
		Templates: fakeReader{files: map[string][]byte{
			"templates/scenario.tmpl": []byte("from the scenario rung\n"),
			"task/task.tmpl":          []byte("from the task rung\n"),
		}},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := rendered[0].RawTemplate; got != "from the scenario rung\n" {
		t.Fatalf("template = %q, want the file the service and scenario rungs name", got)
	}
}

// TestTemplatePath_AnEmptyFileIsNotReadAgainFromTheTaskView — the path chosen on the
// run-level view is the one file delivered, whatever it holds: an EMPTY file is
// content like any other, and the task's own `vars:` resolve the same cell to another
// file that must not be read in its place.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in renderTaskIter, before
// injectTemplateContent, re-read params.template through in.Templates when
// tf.content is "". It goes red.
func TestTemplatePath_AnEmptyFileIsNotReadAgainFromTheTaskView(t *testing.T) {
	rendered, err := renderScenario(t, RenderInput{
		Scenario: &config.ScenarioManifest{
			Name: "s",
			Vars: map[string]any{"tmpl": "a.tmpl"},
			Tasks: []config.Task{{
				Name: "render",
				Vars: map[string]any{"tmpl": "b.tmpl"},
				Module: &config.ModuleTask{Module: moduleFileRendered, Params: map[string]any{
					"path": "/etc/x.conf", "template": "${ vars.tmpl }",
				}},
			}},
		},
		Templates: fakeReader{files: map[string][]byte{
			"a.tmpl": {},
			"b.tmpl": []byte("from the task rung\n"),
		}},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	fields := rendered[0].Params.GetFields()
	content, ok := fields[paramTemplateContent]
	if !ok || content.GetStringValue() != "" || rendered[0].RawTemplate != "" {
		t.Fatalf("template_content = %v (present %v), raw %q; want a.tmpl's empty content", content, ok, rendered[0].RawTemplate)
	}
	if _, left := fields[paramTemplate]; left {
		t.Errorf("params still carry the template path: %v", fields[paramTemplate])
	}
}

// TestStaticWhen_AKeeperTaskIsDecidedInTheKeeperContext — point 7: a keeper task's
// static `when:` is read where its params are, in keeperVars — no host bound,
// `host_count` 0, the keeper's register — not on the roster's first host. A task var
// reading a host root is refused there by name, as it is in the task's params.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: drop the keeper branch of
// staticWhenSkips, so the reader is hostLoopVars on the first host again. The first
// two cases skip the task, and the third decides it on a.example.com.
func TestStaticWhen_AKeeperTaskIsDecidedInTheKeeperContext(t *testing.T) {
	keeperTask := func(when string, vars map[string]any) config.Task {
		return config.Task{Name: "on keeper", On: config.KeeperTarget, When: when, Vars: vars, Module: &config.ModuleTask{
			Module: "core.state.present", Params: map[string]any{"path": "x", "value": "v"},
		}}
	}
	for name, tc := range map[string]struct {
		task    config.Task
		scnVars map[string]any
	}{
		"incarnation.host_count":           {task: keeperTask("incarnation.host_count == 0", nil)},
		"a scenario var over host_count":   {task: keeperTask("vars.n == 0", nil), scnVars: map[string]any{"n": "${ incarnation.host_count }"}},
		"a task var over a host's own sid": {task: keeperTask("vars.sid == ''", map[string]any{"sid": "${ soulprint.self.sid }"})},
	} {
		t.Run(name, func(t *testing.T) {
			rendered, err := renderScenario(t, RenderInput{
				Scenario: &config.ScenarioManifest{Name: "s", Vars: tc.scnVars, Tasks: []config.Task{tc.task}},
				Hosts:    twoHosts(),
			})
			if tc.task.Vars != nil {
				var hs *cel.ErrHostOutOfScope
				if !errors.As(err, &hs) {
					t.Fatalf("err = %v, want the host root refused by name in the keeper context", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if len(rendered) != 1 || rendered[0].Params == nil {
				t.Fatalf("rendered %+v, want the keeper task active: its when: reads host_count 0", rendered)
			}
		})
	}
}

// TestDestinyBase_AnUnknownHostGetsNoView — a destiny pass builds one `vars.yml` view
// per targeted host plus the synthetic empty host's. A host the pass did not target is
// not given another host's view: that would be the NIM-908 substitution, answering for
// one host with another's values.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: fall back to the synthetic
// view for any SID in destinyBase. The unknown host then gets it.
func TestDestinyBase_AnUnknownHostGetsNoView(t *testing.T) {
	targeted := twoHosts()
	destinyIn := RenderInput{Incarnation: IncarnationMeta{ID: "inc-1"}, Hosts: targeted, destinyIsolated: true}
	destinyIn.destinyVars = NewPipeline(nil, newEngine(t), nil, nil).destinyBaseViews(destinyIn, map[string]any{"sid": "${ soulprint.self.sid }"}, targeted)

	for _, h := range targeted {
		got, found, err := destinyIn.destinyBase(h).Lookup("sid")
		if err != nil || !found || got != h.SID {
			t.Fatalf("%s: sid = %#v, %v, %v; want its own view", h.SID, got, found, err)
		}
	}
	if destinyIn.destinyBase(nil) != destinyIn.destinyVars[""] || destinyIn.destinyBase(&topology.HostFacts{}) != destinyIn.destinyVars[""] {
		t.Fatal("no host must get the synthetic empty host's view")
	}
	if view := destinyIn.destinyBase(host("c.example.com", nil, nil)); view != nil {
		t.Fatalf("an untargeted host got a view: %v", view.Names())
	}
}

// renderCountingEvaluations renders in and returns how many definitions the views of
// `vars.*` the render built evaluated, its service view included.
func renderCountingEvaluations(t *testing.T, in RenderInput) ([]*RenderedTask, int64, error) {
	t.Helper()
	if in.Incarnation.ID == "" {
		in.Incarnation = IncarnationMeta{ID: "inc-1", Service: "svc"}
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	var mu sync.Mutex
	var views []*cel.VarView
	p.onView = func(v *cel.VarView) {
		mu.Lock()
		defer mu.Unlock()
		views = append(views, v)
	}
	tasks, _, err := p.Render(context.Background(), in)
	var total int64
	if in.ServiceVars != nil {
		total = in.ServiceVars.Evaluations()
	}
	for _, v := range views {
		total += v.Evaluations()
	}
	return tasks, total, err
}

// numberedDefs is n definitions prefix00…, each a CEL expression of its own.
func numberedDefs(prefix string, n int) map[string]any {
	out := make(map[string]any, n)
	for i := range n {
		name := fmt.Sprintf("%s%02d", prefix, i)
		out[name] = "${ '" + name + "' + '-v' }"
	}
	return out
}

// TestStrictVars_DoesNotMaterializeAStaticallySkippedTask — point 12b: strict mode
// materializes the view of every task the render renders, and a task a static `when:`
// skips is not one — it may read a name only another mode of the scenario defines.
// Each task carries a broken var nothing reads: skipped, it renders under strict mode;
// active, strict mode fails it and names the var.
func TestStrictVars_DoesNotMaterializeAStaticallySkippedTask(t *testing.T) {
	broken := map[string]any{"broken": "${ vars.nope }"}
	scenario := func(task config.Task) RenderInput {
		return RenderInput{
			Scenario:   &config.ScenarioManifest{Name: "s", Tasks: []config.Task{task}},
			Input:      map[string]any{"mode": "a"},
			StrictVars: true,
		}
	}
	const active, inactive = "input.mode == 'a'", "input.mode == 'b'"
	for name, tc := range map[string]struct {
		in               func(when string) RenderInput
		active, inactive string
	}{
		"a host task": {in: func(when string) RenderInput {
			task := execTask("t", broken, "echo ok")
			task.When = when
			return scenario(task)
		}, active: active, inactive: inactive},
		"a keeper task": {in: func(when string) RenderInput {
			return scenario(config.Task{Name: "bind", Vars: broken, When: when, Module: &config.ModuleTask{
				Module: "core.soul.registered", Params: map[string]any{"sid": "a.example.com", "coven": []any{"x"}, "mode": "append"},
			}})
		}, active: active, inactive: inactive},
		"an assert task": {in: func(when string) RenderInput {
			return scenario(config.Task{Name: "guard", Vars: broken, When: when, Assert: &config.AssertSpec{That: []string{"true"}}})
		}, active: active, inactive: inactive},
		"a destiny task": {in: func(when string) RenderInput {
			resolved := destinyWithFileVars(nil, map[string]any{"cmd": "echo ok"})
			resolved.Tasks[0].Vars, resolved.Tasks[0].When = broken, when
			return RenderInput{
				Scenario:   applyScenario("pilot-vars", map[string]any{}),
				Destiny:    &stubDestinyResolver{resolved: resolved},
				StrictVars: true,
			}
		}, active: "input.user == 'alice'", inactive: "input.user == 'bob'"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := renderScenario(t, tc.in(tc.inactive)); err != nil {
				t.Fatalf("skipped by its static when: strict mode must leave its view alone: %v", err)
			}
			if _, err := renderScenario(t, tc.in(tc.active)); err == nil || !strings.Contains(err.Error(), "vars.broken") {
				t.Fatalf("active: err = %v, want strict mode to fail it naming vars.broken", err)
			}
		})
	}
}

// TestStrictVars_LeavesTheViewsItDoesNotClaimAlone — point 12b names what strict mode
// does NOT materialize, and each case holds a definition that fails only in that view,
// so the case renders in production and must render under strict mode too:
//
//   - a task whose where: targets no host renders its params on a stand-in host, which
//     is no host: its `soulprint.self` has no facts, and a var over them that the params
//     do not read is no error;
//   - so does an applier whose where: targets no host, for its `apply: input:`, and an
//     `assert:` over an empty roster, for its predicates;
//   - the loop axis has no `compute`, which a scenario var reads for the params;
//   - an include `when:` that drops its group leaves a scenario var over an input only
//     the other mode passes;
//   - a template path is chosen in the keeper context, where host_count is 0.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code, one per case:
//   - in renderTaskIter, drop `&& len(targeted) > 0` from the strict-mode condition;
//   - in resolveApplyInput, drop `&& len(targeted) > 0` from the strict-mode condition;
//   - in evalAssertTask, drop `&& len(in.Hosts) > 0` from the strict-mode condition;
//   - in renderLoopTask, after loopAxisView: `if in.StrictVars { if err := materialize(axis); err != nil { return nil, nil, err } }`;
//   - in evalIncludeWhen, before buildFlowContext: `if in.StrictVars { if err := materialize(vars.View); err != nil { return false, err } }`;
//   - in resolveTemplateUsesInput, after `kv.View = view`: `if in.StrictVars { if err := materialize(view); err != nil { return templateFile{}, err } }`.
//
// Each turns its case red.
func TestStrictVars_LeavesTheViewsItDoesNotClaimAlone(t *testing.T) {
	withFacts := []*topology.HostFacts{
		host("a.example.com", []string{"svc"}, map[string]any{"network": map[string]any{"primary_ip": "10.0.0.1"}}),
		host("b.example.com", []string{"svc"}, map[string]any{"network": map[string]any{"primary_ip": "10.0.0.2"}}),
	}
	for name, tc := range map[string]struct {
		in    RenderInput
		tasks int
	}{
		"a task whose where: targets no host": {in: func() RenderInput {
			task := execTask("t", map[string]any{"ip": "${ soulprint.self.network.primary_ip }"}, "echo ok")
			task.Where = "false"
			return RenderInput{Scenario: &config.ScenarioManifest{Name: "s", Tasks: []config.Task{task}}, Hosts: withFacts}
		}(), tasks: 1},
		"an applier whose where: targets no host": {in: func() RenderInput {
			scenario := applyScenario("pilot-vars", map[string]any{})
			scenario.Tasks[0].Where = "false"
			scenario.Tasks[0].Vars = map[string]any{"ip": "${ soulprint.self.network.primary_ip }"}
			return RenderInput{
				Scenario: scenario,
				Destiny:  &stubDestinyResolver{resolved: destinyWithFileVars(nil, map[string]any{"cmd": "echo ok"})},
				Hosts:    withFacts,
			}
		}(), tasks: 1},
		"an assert: over an empty roster": {in: RenderInput{
			Scenario: &config.ScenarioManifest{Name: "s", Tasks: []config.Task{{
				Name:   "guard",
				Vars:   map[string]any{"ip": "${ soulprint.self.network.primary_ip }"},
				Assert: &config.AssertSpec{That: []string{"true"}},
			}}},
			Hosts: []*topology.HostFacts{},
		}, tasks: 0},
		"the loop axis": {in: func() RenderInput {
			task := execTask("fan", nil, "echo ${ u } ${ vars.s }")
			task.Loop = &config.LoopSpec{Items: []any{"a", "b"}, As: "u"}
			return RenderInput{Scenario: &config.ScenarioManifest{
				Name:    "s",
				Compute: config.ComputeBlock{{Name: "x", Value: "${ 'v' }"}},
				Vars:    map[string]any{"s": "${ compute.x }"},
				Tasks:   []config.Task{task},
			}}
		}(), tasks: 2},
		"an include when: that drops its group": {in: func() RenderInput {
			task := execTask("included", nil, "echo ${ vars.conf }")
			task.IncludeGroupID, task.IncludeWhen = 1, "input.mode == 'full'"
			return RenderInput{
				Scenario: &config.ScenarioManifest{Name: "s", Vars: map[string]any{"conf": "${ input.conf }"}, Tasks: []config.Task{task}},
				Input:    map[string]any{"mode": "lite"},
			}
		}(), tasks: 0},
		"a template path": {in: RenderInput{
			Scenario: &config.ScenarioManifest{
				Name: "s",
				Vars: map[string]any{"tmpl": "a.tmpl", "share": "${ 100 / incarnation.host_count }"},
				Tasks: []config.Task{{
					Name: "render",
					Module: &config.ModuleTask{Module: moduleFileRendered, Params: map[string]any{
						"path": "/etc/x.conf", "template": "${ vars.tmpl }", "vars": map[string]any{"share": "${ vars.share }"},
					}},
				}},
			},
			Templates: fakeReader{files: map[string][]byte{"a.tmpl": []byte("share {{ .vars.share }}\n")}},
		}, tasks: 1},
	} {
		t.Run(name, func(t *testing.T) {
			tc.in.StrictVars = true
			rendered, err := renderScenario(t, tc.in)
			if err != nil {
				t.Fatalf("strict mode must leave this view alone: %v", err)
			}
			if len(rendered) != tc.tasks {
				t.Fatalf("rendered %d tasks, want %d", len(rendered), tc.tasks)
			}
		})
	}
}

// TestNotRequested_ATaskViewEvaluatesOnlyWhatItsCellsRead — point 4, with the counter
// and without strict mode: a task defining 50 vars whose params read one evaluates that
// one and the one it reads, on each host; a destiny `vars.yml` of 50 names likewise.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: materialize each task view as it
// is built — `_, _ = view.Materialize()` in withTaskView. Each host then evaluates 50.
func TestNotRequested_ATaskViewEvaluatesOnlyWhatItsCellsRead(t *testing.T) {
	chained := func(prefix string) map[string]any {
		defs := numberedDefs(prefix, 50)
		defs[prefix+"00"] = "${ vars." + prefix + "01 }-x"
		return defs
	}
	hosts := costHosts(3)
	for name, tc := range map[string]struct {
		in   RenderInput
		want string
	}{
		"the task rung": {
			in:   RenderInput{Scenario: &config.ScenarioManifest{Name: "s", Tasks: []config.Task{execTask("t", chained("t"), "echo ${ vars.t00 }")}}},
			want: "echo t01-v-x",
		},
		"vars.yml": {
			in: RenderInput{
				Scenario: applyScenario("pilot-vars", map[string]any{}),
				Destiny:  &stubDestinyResolver{resolved: destinyWithFileVars(chained("f"), map[string]any{"cmd": "echo ${ vars.f00 }"})},
			},
			want: "echo f01-v-x",
		},
	} {
		t.Run(name, func(t *testing.T) {
			tc.in.Hosts = hosts
			tasks, evaluations, err := renderCountingEvaluations(t, tc.in)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if got := cmdOf(t, tasks[0]); got != tc.want {
				t.Fatalf("cmd = %q, want %q", got, tc.want)
			}
			if want := int64(2 * len(hosts)); evaluations != want {
				t.Fatalf("evaluated %d definitions on %d hosts, want %d: the one the params read and the one it reads, per host", evaluations, len(hosts), want)
			}
		})
	}
}

// taskViewCostScenario is one destiny task whose view stacks 50 task vars on a
// `vars.yml` of 50, its params reading two of each.
func taskViewCostScenario() (*config.ScenarioManifest, *stubDestinyResolver) {
	resolved := destinyWithFileVars(numberedDefs("f", 50), map[string]any{"cmd": "echo ${ vars.t00 } ${ vars.t01 } ${ vars.f00 } ${ vars.f01 }"})
	resolved.Tasks[0].Vars = numberedDefs("t", 50)
	return applyScenario("pilot-vars", map[string]any{}), &stubDestinyResolver{resolved: resolved}
}

// TestTaskViewCost_AHostEvaluatesWhatTheParamsRead — the evaluation count behind
// [BenchmarkTaskView_RenderByRoster], so the saving it measures is checked by the build:
// of the 100 definitions the task's view stacks, each host evaluates the four its
// params read, at every roster size the benchmark runs.
func TestTaskViewCost_AHostEvaluatesWhatTheParamsRead(t *testing.T) {
	for _, n := range []int{1, 9, 100} {
		manifest, res := taskViewCostScenario()
		tasks, evaluations, err := renderCountingEvaluations(t, perHostRenderInput(manifest, res, costHosts(n)))
		if err != nil {
			t.Fatalf("hosts=%d: Render: %v", n, err)
		}
		if got := cmdOf(t, tasks[0]); got != "echo t00-v t01-v f00-v f01-v" {
			t.Fatalf("hosts=%d: cmd = %q", n, got)
		}
		if want := int64(4 * n); evaluations != want {
			t.Fatalf("hosts=%d: evaluated %d definitions, want %d — four per host", n, evaluations, want)
		}
	}
}

// BenchmarkTaskView_RenderByRoster measures what evaluating `vars.*` on request saves
// (ADR-0082 amendment 2026-10-15, point 13), on the model of
// [BenchmarkApplyInput_ResolveByRoster]: a Render of [taskViewCostScenario], one task
// whose view stacks 50 task vars on a `vars.yml` of 50 and whose params read two of
// each, with the roster as its axis. "strict" is the same Render materializing every
// view whole — what an evaluator that computed every definition would pay:
//
//	go test ./internal/render/ -run '^$' -bench BenchmarkTaskView_RenderByRoster -benchmem -count 5
//
// Measured 2026-10-20 on this tree (AMD Ryzen 9 7900), median of five runs:
//
//	hosts  on-request                strict
//	1        4.5 ms,  46.3k allocs     6.0 ms,  62.4k allocs
//	9        5.8 ms,  57.7k allocs    19.6 ms, 200.8k allocs
//	100     23.1 ms, 267.7k allocs   182.9 ms,  1.86M allocs
//
// i.e. an extra host costs ~0.19 ms with the four definitions it reads evaluated, and
// ~1.8 ms with all hundred.
//
// Not an assertion: that a host evaluates four definitions and not a hundred is
// TestTaskViewCost_AHostEvaluatesWhatTheParamsRead, and a timing threshold in the gate
// is a flake.
func BenchmarkTaskView_RenderByRoster(b *testing.B) {
	for _, n := range []int{1, 9, 100} {
		for _, strict := range []bool{false, true} {
			mode := "on-request"
			if strict {
				mode = "strict"
			}
			b.Run(fmt.Sprintf("hosts=%d/%s", n, mode), func(b *testing.B) {
				manifest, res := taskViewCostScenario()
				e, err := cel.New()
				if err != nil {
					b.Fatal(err)
				}
				p := NewPipeline(nil, e, nil, nil)
				in := perHostRenderInput(manifest, res, costHosts(n))
				in.StrictVars = strict
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, _, err := p.Render(context.Background(), in); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// TestFlowContext_TheReportedVarIsTheFirstInOrder — point 10: two broken vars a task's
// predicates read through flow_context, and the one reported is the first in sorted
// order on every render, whichever the predicate names first.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in flowVarsSection, hand
// lookupVars `slices.Collect(maps.Keys(r.Fields))`, unsorted. It goes red.
func TestFlowContext_TheReportedVarIsTheFirstInOrder(t *testing.T) {
	task := execTask("t", map[string]any{"a_bad": "${ input.nope_a }", "b_bad": "${ input.nope_b }"}, "true")
	task.ChangedWhen = "vars.b_bad == 1 || vars.a_bad == 1"
	for i := range 50 {
		_, err := renderScenario(t, RenderInput{Scenario: &config.ScenarioManifest{Name: "s", Tasks: []config.Task{task}}})
		if err == nil || !strings.Contains(err.Error(), "vars.a_bad") || !strings.Contains(err.Error(), "nope_a") {
			t.Fatalf("render %d: err = %v, want vars.a_bad, the first in order, reported", i, err)
		}
	}
}

// TestStrictVars_AKeeperTaskChecksTheScenarioRungInItsOwnContext — point 12b: a var must
// evaluate in every view it is visible in, and a keeper task's view holds the scenario
// rung evaluated with keeperVars, where host_count is 0. A share over host_count renders
// leniently — the host task reads it over two hosts, the keeper task not at all — and
// strict mode fails it in the keeper task's view, naming the var. Guarding the division
// renders under both.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: drop the strict-mode
// materialize from renderKeeperTask, so strict mode passes keeper tasks over. It goes
// red.
func TestStrictVars_AKeeperTaskChecksTheScenarioRungInItsOwnContext(t *testing.T) {
	scenario := func(share string, strict bool) RenderInput {
		return RenderInput{
			Scenario: &config.ScenarioManifest{
				Name: "s",
				Vars: map[string]any{"share": share},
				Tasks: []config.Task{
					execTask("on hosts", nil, "echo ${ vars.share }"),
					{Name: "bind", Module: &config.ModuleTask{Module: "core.soul.registered", Params: map[string]any{
						"sid": "a.example.com", "coven": []any{"x"}, "mode": "append",
					}}},
				},
			},
			StrictVars: strict,
		}
	}
	const unguarded = "${ 100 / incarnation.host_count }"
	const guarded = "${ incarnation.host_count > 0 ? 100 / incarnation.host_count : 0 }"

	rendered, err := renderScenario(t, scenario(unguarded, false))
	if err != nil {
		t.Fatalf("lenient: %v", err)
	}
	if len(rendered) != 2 || cmdOf(t, rendered[0]) != "echo 50" {
		t.Fatalf("lenient: rendered %d tasks, the host task's cmd %q; want 2 and echo 50", len(rendered), cmdOf(t, rendered[0]))
	}
	if _, err := renderScenario(t, scenario(unguarded, true)); err == nil ||
		!strings.Contains(err.Error(), `keeper task "bind"`) || !strings.Contains(err.Error(), "vars.share") {
		t.Fatalf("strict: err = %v, want the keeper task's view to fail naming vars.share", err)
	}
	if _, err := renderScenario(t, scenario(guarded, true)); err != nil {
		t.Fatalf("strict, guarded: %v", err)
	}
}
