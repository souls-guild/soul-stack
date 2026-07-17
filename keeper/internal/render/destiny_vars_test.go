package render

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/tmpl"
)

// destinyWithFileVars — a flat destiny with one module-task reading
// `${ vars.<x> }` (file-level destiny locals), plus the given file-vars (raw).
func destinyWithFileVars(fileVars map[string]any, taskParams map[string]any) *ResolvedDestiny {
	return &ResolvedDestiny{
		Name: "pilot-vars",
		Input: config.InputSchemaMap{
			"user": {Type: "string", Default: "alice"},
		},
		Vars: fileVars,
		Tasks: []config.Task{
			{
				Name:   "use file vars",
				Module: &config.ModuleTask{Module: "core.exec.run", Params: taskParams},
			},
		},
	}
}

// TestDestinyFileVars_InParams — file-level vars.yml resolves and is available as
// `${ vars.<x> }` in a destiny-task's params.
func TestDestinyFileVars_InParams(t *testing.T) {
	res := &stubDestinyResolver{resolved: destinyWithFileVars(
		map[string]any{"unit": "redis-server"},
		map[string]any{"cmd": "systemctl restart ${ vars.unit }"},
	)}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("pilot-vars", map[string]any{}),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "systemctl restart redis-server" {
		t.Errorf("cmd = %q, want systemctl restart redis-server", got)
	}
}

// TestDestinyFileVars_FromInputAndSelf — a vars.yml value resolves over input.*
// (destiny input) and soulprint.self.*; CEL access to them is allowed.
func TestDestinyFileVars_FromInputAndSelf(t *testing.T) {
	res := &stubDestinyResolver{resolved: destinyWithFileVars(
		map[string]any{
			"acl_path": "/etc/redis/users/${ input.user }.acl",
			"family":   "${ soulprint.self.os.family }",
		},
		map[string]any{"cmd": "echo ${ vars.acl_path } ${ vars.family }"},
	)}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("pilot-vars", map[string]any{"user": "bob"}),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts: []*topology.HostFacts{host("a.example.com", []string{"svc"}, map[string]any{
			"os": map[string]any{"family": "debian"},
		})},
		Destiny: res,
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "echo /etc/redis/users/bob.acl debian" {
		t.Errorf("cmd = %q, want echo /etc/redis/users/bob.acl debian", got)
	}
}

// TestDestinyFileVars_RegisterIsolation — a vars.yml value doesn't see register.*
// (at vars-resolve time no tasks have run yet; destiny-scope isolation) → error.
func TestDestinyFileVars_RegisterIsolation(t *testing.T) {
	res := &stubDestinyResolver{resolved: destinyWithFileVars(
		map[string]any{"x": "${ register.probe.stdout }"},
		map[string]any{"cmd": "echo ${ vars.x }"},
	)}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("pilot-vars", map[string]any{}),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("Render: expected an error - vars.yml must not see register.* (isolation)")
	}
}

// TestDestinyFileVars_HostsIsolation — var→var does NOT weaken isolation (case #6):
// a file-var referencing soulprint.hosts (a cross-host scenario-only accessor) is
// still rejected at compile (AllowHosts=false in the destiny pass).
func TestDestinyFileVars_HostsIsolation(t *testing.T) {
	res := &stubDestinyResolver{resolved: destinyWithFileVars(
		map[string]any{"x": "${ soulprint.hosts.size() }"},
		map[string]any{"cmd": "echo ${ vars.x }"},
	)}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("pilot-vars", map[string]any{}),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("Render: vars.yml must not see soulprint.hosts (destiny isolation, not weakened by var->var)")
	}
}

// TestDestinyFileVars_OverrideWithInLayerRef — case #9: same-named file/task vars
// (Variant-A override) plus an intra-task-layer reference stays intact. file-var
// `unit` is overridden by task-var `unit`, and task-var `svc` references task-var
// `unit` (same-layer var→var). params sees the task values.
func TestDestinyFileVars_OverrideWithInLayerRef(t *testing.T) {
	resolved := destinyWithFileVars(
		map[string]any{"unit": "redis-FILE"}, // file-level — will be overridden
		map[string]any{"cmd": "${ vars.svc }"},
	)
	resolved.Tasks[0].Vars = map[string]any{
		"unit": "redis-TASK",         // override file-var
		"svc":  "${ vars.unit }-svc", // reference to a task-var in the same layer
	}
	res := &stubDestinyResolver{resolved: resolved}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("pilot-vars", map[string]any{}),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "redis-TASK-svc" {
		t.Errorf("cmd = %q, want redis-TASK-svc (task-var svc sees task-var unit, override Variant A)", got)
	}
}

// TestDestinyFileVars_EssenceIsolation — a vars.yml value doesn't see essence.*
// (essence is a service-level concept, absent entirely in destiny) → error.
func TestDestinyFileVars_EssenceIsolation(t *testing.T) {
	res := &stubDestinyResolver{resolved: destinyWithFileVars(
		map[string]any{"x": "${ essence.maxmemory }"},
		map[string]any{"cmd": "echo ${ vars.x }"},
	)}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("pilot-vars", map[string]any{}),
		Incarnation: IncarnationMeta{Name: "svc"},
		// scenario carries essence — destiny must NOT see it even in vars.yml.
		Essence: map[string]any{"maxmemory": "256mb"},
		Hosts:   []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny: res,
	}
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("Render: expected an error - vars.yml must not see essence.* (isolation)")
	}
}

// TestDestinyFileVars_VarToVar — a file-var references another file-var of the
// same layer (`${ vars.<other> }` is ALLOWED, eager-topological); mirrors
// TestResolveTaskVars_VarToVar (case #1, file layer). The ORIGINAL feature case:
// root_group: "${ vars.root_owner }".
func TestDestinyFileVars_VarToVar(t *testing.T) {
	res := &stubDestinyResolver{resolved: destinyWithFileVars(
		map[string]any{
			"base": "redis",
			"unit": "${ vars.base }-server",
		},
		map[string]any{"cmd": "echo ${ vars.unit }"},
	)}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("pilot-vars", map[string]any{}),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: var->var within the file layer must resolve: %v", err)
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "echo redis-server" {
		t.Errorf("cmd = %q, want echo redis-server (unit references base)", got)
	}
}

// TestDestinyFileVars_TransitiveChain — a transitive chain a→b→c of the same
// layer (case #2): c is computed first, b sees c, a sees b.
func TestDestinyFileVars_TransitiveChain(t *testing.T) {
	got, err := resolveVarLayer(newEngine(t), map[string]any{
		"a": "${ vars.b }/a",
		"b": "${ vars.c }/b",
		"c": "root",
	}, cel.Vars{})
	if err != nil {
		t.Fatalf("resolveVarLayer: %v", err)
	}
	if got["a"] != "root/b/a" {
		t.Errorf("vars.a = %v, want root/b/a (transitively a->b->c)", got["a"])
	}
}

// TestDestinyFileVars_Cycle — a cycle a→b→c→a (case #3) → ErrVarCycle with a trace.
func TestDestinyFileVars_Cycle(t *testing.T) {
	_, err := resolveVarLayer(newEngine(t), map[string]any{
		"a": "${ vars.b }",
		"b": "${ vars.c }",
		"c": "${ vars.a }",
	}, cel.Vars{})
	if err == nil || !errors.Is(err, ErrVarCycle) {
		t.Fatalf("resolveVarLayer: expected ErrVarCycle, got: %v", err)
	}
	// The trace is closed: it contains the starting node twice (a → … → a).
	if !strings.Contains(err.Error(), "a → b → c → a") {
		t.Errorf("err = %v, want trace 'a -> b -> c -> a'", err)
	}
}

// TestDestinyFileVars_SelfReference — a self-reference a→a (case #4) → ErrVarCycle
// (a special case of a cycle, trace 'a → a').
func TestDestinyFileVars_SelfReference(t *testing.T) {
	_, err := resolveVarLayer(newEngine(t), map[string]any{
		"a": "${ vars.a }-loop",
	}, cel.Vars{})
	if err == nil || !errors.Is(err, ErrVarCycle) {
		t.Fatalf("resolveVarLayer: self-reference must give ErrVarCycle, got: %v", err)
	}
	if !strings.Contains(err.Error(), "a → a") {
		t.Errorf("err = %v, want trace 'a -> a'", err)
	}
}

// TestDestinyFileVars_UnusedBrokenRef — an UNUSED var references nonexistent
// vars.z (case #5): EAGER marker — layer resolution fails with ErrVarUnknownRef,
// even if the referencing var is never read by params.
func TestDestinyFileVars_UnusedBrokenRef(t *testing.T) {
	res := &stubDestinyResolver{resolved: destinyWithFileVars(
		map[string]any{
			"used":   "redis-server",
			"broken": "${ vars.z }", // z doesn't exist; broken is never read
		},
		map[string]any{"cmd": "echo ${ vars.used }"}, // params only reads used
	)}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("pilot-vars", map[string]any{}),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("Render: a broken unused var must fail render EAGERLY (var_unknown_ref)")
	}
	if !errors.Is(err, ErrVarUnknownRef) {
		t.Errorf("err = %v, want ErrVarUnknownRef", err)
	}
}

// TestDestinyFileVars_OrderIndependent — key order in the file layer doesn't
// matter (case #7): two raw variants with different declaration order give an
// equal result.
func TestDestinyFileVars_OrderIndependent(t *testing.T) {
	e := newEngine(t)
	v1, err := resolveVarLayer(e, map[string]any{
		"a": "root",
		"b": "${ vars.a }-b",
		"c": "${ vars.b }-c",
	}, cel.Vars{})
	if err != nil {
		t.Fatalf("resolveVarLayer v1: %v", err)
	}
	v2, err := resolveVarLayer(e, map[string]any{
		"c": "${ vars.b }-c",
		"a": "root",
		"b": "${ vars.a }-b",
	}, cel.Vars{})
	if err != nil {
		t.Fatalf("resolveVarLayer v2: %v", err)
	}
	for _, k := range []string{"a", "b", "c"} {
		if v1[k] != v2[k] {
			t.Errorf("vars.%s diverges with different order: v1=%v v2=%v", k, v1[k], v2[k])
		}
	}
	if v1["c"] != "root-b-c" {
		t.Errorf("vars.c = %v, want root-b-c", v1["c"])
	}
}

// TestDestinyFileVars_TaskOverridesFile — Variant A: task-level vars: overrides a
// same-named file-level var of the same destiny (deterministic outcome — task
// wins).
func TestDestinyFileVars_TaskOverridesFile(t *testing.T) {
	resolved := destinyWithFileVars(
		map[string]any{"unit": "redis-server"}, // file-level
		map[string]any{"cmd": "echo ${ vars.unit }"},
	)
	// task-level vars: on the same task with the same name — must win.
	resolved.Tasks[0].Vars = map[string]any{"unit": "redis-staging"}
	res := &stubDestinyResolver{resolved: resolved}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("pilot-vars", map[string]any{}),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "echo redis-staging" {
		t.Errorf("cmd = %q, want echo redis-staging (task-level vars over file-level, Variant A)", got)
	}
}

// TestDestinyFileVars_TaskAndFileCoexist — non-matching names coexist: both the
// file-var and the task-var are visible in one task's params.
func TestDestinyFileVars_TaskAndFileCoexist(t *testing.T) {
	resolved := destinyWithFileVars(
		map[string]any{"unit": "redis-server"},
		map[string]any{"cmd": "${ vars.unit } ${ vars.extra }"},
	)
	resolved.Tasks[0].Vars = map[string]any{"extra": "flag"}
	res := &stubDestinyResolver{resolved: resolved}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("pilot-vars", map[string]any{}),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "redis-server flag" {
		t.Errorf("cmd = %q, want 'redis-server flag' (file-var + task-var coexist)", got)
	}
}

// TestDestinyFileVars_ScenarioVarsDoNotLeak — destiny does NOT see scenario-level
// `vars:` (only through apply: input:). scenario task-vars `leak` on the
// apply-task → no-such-key in the destiny pass; only what's forwarded through
// apply.input gets through.
func TestDestinyFileVars_ScenarioVarsDoNotLeak(t *testing.T) {
	res := &stubDestinyResolver{resolved: destinyWithFileVars(
		nil, // no file-vars — checking that scenario-vars don't substitute for them
		map[string]any{"cmd": "echo ${ vars.leak }"},
	)}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	// applyScenario with scenario task-vars `leak` on the apply-task itself.
	scn := &config.ScenarioManifest{
		Name: "create",
		Tasks: []config.Task{
			{
				Name:  "Apply destiny",
				Vars:  map[string]any{"leak": "SCENARIO_LEAK"},
				Apply: &config.ApplyTask{Destiny: "pilot-vars", Input: map[string]any{}},
			},
		},
	}
	in := RenderInput{
		Scenario:    scn,
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("Render: expected an error - scenario vars must not flow into destiny (only via apply.input)")
	}
}

// TestDestinyFileVars_OnlyViaApplyInput — the only legal bridge from scenario into
// destiny is apply: input:. scenario.input is forwarded into destiny-input, and
// the destiny's vars.yml resolves over that input.
func TestDestinyFileVars_OnlyViaApplyInput(t *testing.T) {
	res := &stubDestinyResolver{resolved: destinyWithFileVars(
		map[string]any{"acl": "/acl/${ input.user }"},
		map[string]any{"cmd": "echo ${ vars.acl }"},
	)}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	scn := &config.ScenarioManifest{
		Name: "create",
		Tasks: []config.Task{
			{
				Name: "Apply destiny",
				// bridge: scenario.input.who → destiny input.user.
				Apply: &config.ApplyTask{Destiny: "pilot-vars", Input: map[string]any{"user": "${ input.who }"}},
			},
		},
	}
	in := RenderInput{
		Scenario:    scn,
		Input:       map[string]any{"who": "carol"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "echo /acl/carol" {
		t.Errorf("cmd = %q, want echo /acl/carol (scenario.input → apply.input → destiny vars.yml)", got)
	}
}

// TestDestinyFileVars_InRenderedTemplateContext — file-level vars.yml is available
// to the core.file.rendered template as `.vars.<file_var>` DIRECTLY, without a
// passthrough via the task's params.vars (symmetric with Variant A:
// render_context.vars = file-vars base + task-level params.vars override). This is
// exactly the node-exporter case: the template reads `.vars.bin_path`, where
// bin_path is a file-var (vars.yml), and the task's params.vars does NOT have it.
func TestDestinyFileVars_InRenderedTemplateContext(t *testing.T) {
	const tmplPath = "templates/unit.tmpl"
	const tmplBody = "ExecStart={{ .vars.bin_path }}\n"

	resolved := &ResolvedDestiny{
		Name:  "pilot-rendered",
		Input: config.InputSchemaMap{"bin_dir": {Type: "string", Default: "/usr/local/bin"}},
		Vars: map[string]any{
			"bin_path": "${ input.bin_dir + '/node_exporter' }",
		},
		Templates: fakeReader{files: map[string][]byte{tmplPath: []byte(tmplBody)}},
		Tasks: []config.Task{
			{
				Name: "render unit",
				Module: &config.ModuleTask{
					Module: moduleFileRendered,
					Params: map[string]any{
						"path":     "/etc/systemd/system/node_exporter.service",
						"template": tmplPath,
						// NO params.vars: file-var bin_path must arrive DIRECTLY.
					},
				},
			},
		},
	}
	res := &stubDestinyResolver{resolved: resolved}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("pilot-rendered", map[string]any{}),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	rc := tasks[0].Params.GetFields()[paramRenderContext].GetStructValue().AsMap()
	vars, _ := rc["vars"].(map[string]any)
	if vars["bin_path"] != "/usr/local/bin/node_exporter" {
		t.Fatalf("render_context.vars.bin_path = %#v, want /usr/local/bin/node_exporter (file-var DIRECTLY in .vars)", vars["bin_path"])
	}

	// Execute with the same engine as Soul, render_context as the ROOT.
	engine, err := tmpl.New()
	if err != nil {
		t.Fatalf("tmpl.New: %v", err)
	}
	out, err := engine.Render(tasks[0].Params.GetFields()[paramTemplateContent].GetStringValue(), rc)
	if err != nil {
		t.Fatalf("soul-render failed (.vars.bin_path unavailable?): %v", err)
	}
	if !strings.Contains(out, "ExecStart=/usr/local/bin/node_exporter") {
		t.Errorf(".vars.bin_path (file-var) not substituted:\n%s", out)
	}
}

// TestDestinyFileVars_TaskVarsOverrideFileInRenderContext — render_context.vars
// follows the same Variant-A semantics as the CEL phase: a same-named task-level
// params.vars overrides the file-var. Guarantees that the file→task merge under
// `.vars` is deterministic (task wins), not dropping either layer.
func TestDestinyFileVars_TaskVarsOverrideFileInRenderContext(t *testing.T) {
	const tmplPath = "templates/unit.tmpl"
	const tmplBody = "bin {{ .vars.bin_path }}\nextra {{ .vars.extra }}\n"

	resolved := &ResolvedDestiny{
		Name: "pilot-rendered-override",
		Vars: map[string]any{
			"bin_path": "/from/file",
			"extra":    "/file-only",
		},
		Templates: fakeReader{files: map[string][]byte{tmplPath: []byte(tmplBody)}},
		Tasks: []config.Task{
			{
				Name: "render unit",
				Module: &config.ModuleTask{
					Module: moduleFileRendered,
					Params: map[string]any{
						"path":     "/etc/x.service",
						"template": tmplPath,
						// task-var bin_path overrides the file-var; extra stays a file-var.
						"vars": map[string]any{"bin_path": "/from/task"},
					},
				},
			},
		},
	}
	res := &stubDestinyResolver{resolved: resolved}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("pilot-rendered-override", map[string]any{}),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	vars, _ := tasks[0].Params.GetFields()[paramRenderContext].GetStructValue().AsMap()["vars"].(map[string]any)
	if vars["bin_path"] != "/from/task" {
		t.Errorf("render_context.vars.bin_path = %#v, want /from/task (task-var override file-var, Variant A)", vars["bin_path"])
	}
	if vars["extra"] != "/file-only" {
		t.Errorf("render_context.vars.extra = %#v, want /file-only (file-var without a matching task-var)", vars["extra"])
	}
}

// TestDestinyFileVars_PerHost — vars.yml references soulprint.self → resolves
// per-host; different hosts give different values (DestinyVarsResolved by SID).
func TestDestinyFileVars_PerHost(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)

	// A direct call to renderApplyDestiny via Render: a destiny-task is
	// host-invariant in the pilot, so params with per-host soulprint.self would fail
	// verification. To check per-host vars resolution specifically, we keep params
	// host-invariant and check per-host via resolveDestinyVars directly.
	destinyIn := RenderInput{
		Scenario:        &config.ScenarioManifest{Name: "pilot-vars"},
		Input:           map[string]any{},
		Incarnation:     IncarnationMeta{Name: "svc"},
		destinyIsolated: true,
	}
	hosts := []*topology.HostFacts{
		host("a", []string{"svc"}, map[string]any{"os": map[string]any{"family": "debian"}}),
		host("b", []string{"svc"}, map[string]any{"os": map[string]any{"family": "rhel"}}),
	}
	raw := map[string]any{"family": "${ soulprint.self.os.family }"}
	got, err := p.resolveDestinyVars(destinyIn, raw, hosts)
	if err != nil {
		t.Fatalf("resolveDestinyVars: %v", err)
	}
	if got["a"]["family"] != "debian" {
		t.Errorf("host a vars.family = %v, want debian", got["a"]["family"])
	}
	if got["b"]["family"] != "rhel" {
		t.Errorf("host b vars.family = %v, want rhel (per-host resolve)", got["b"]["family"])
	}
}

// TestDestinyFileVars_StagedInvariant — file-vars are invariant across Passages:
// they resolve over input+self+incarnation WITHOUT register, so ActivePassage
// doesn't affect them. We compare resolution at ActivePassage 0 and 1 — identical
// (ADR-056: the destiny pass's input is invariant across passages, file-vars even
// more so).
func TestDestinyFileVars_StagedInvariant(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	hosts := []*topology.HostFacts{host("a", []string{"svc"}, map[string]any{
		"os": map[string]any{"family": "debian"},
	})}
	raw := map[string]any{
		"acl":    "/acl/${ input.user }",
		"family": "${ soulprint.self.os.family }",
	}
	base := func(passage int) RenderInput {
		return RenderInput{
			Scenario:        &config.ScenarioManifest{Name: "pilot-vars"},
			Input:           map[string]any{"user": "dave"},
			Incarnation:     IncarnationMeta{Name: "svc"},
			ActivePassage:   passage,
			destinyIsolated: true,
		}
	}
	p0, err := p.resolveDestinyVars(base(0), raw, hosts)
	if err != nil {
		t.Fatalf("resolveDestinyVars P0: %v", err)
	}
	p1, err := p.resolveDestinyVars(base(1), raw, hosts)
	if err != nil {
		t.Fatalf("resolveDestinyVars P1: %v", err)
	}
	for _, key := range []string{"acl", "family"} {
		if p0["a"][key] != p1["a"][key] {
			t.Errorf("vars.%s diverges P0=%v P1=%v - file-vars must be invariant across Passage", key, p0["a"][key], p1["a"][key])
		}
	}
	if p0["a"]["acl"] != "/acl/dave" || p0["a"]["family"] != "debian" {
		t.Errorf("file-vars resolve = %v, want acl=/acl/dave family=debian", p0["a"])
	}
}
