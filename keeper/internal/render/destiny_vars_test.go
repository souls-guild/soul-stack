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

// destinyWithFileVars — плоская destiny из одной module-задачи, читающей
// `${ vars.<x> }` (file-level destiny-локалы), плюс заданные file-vars (raw).
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

// TestDestinyFileVars_InParams — file-level vars.yml резолвится и доступен как
// `${ vars.<x> }` в params destiny-задачи.
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

// TestDestinyFileVars_FromInputAndSelf — vars.yml-значение резолвится над
// input.* (destiny-input) и soulprint.self.*; CEL-доступ к ним разрешён.
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

// TestDestinyFileVars_RegisterIsolation — vars.yml-значение не видит register.*
// (на момент резолва vars задач ещё не было; изоляция destiny-scope) → ошибка.
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
		t.Fatal("Render: ожидалась ошибка — vars.yml не должен видеть register.* (изоляция)")
	}
}

// TestDestinyFileVars_HostsIsolation — var→var НЕ ослабляет изоляцию (кейс #6):
// file-var, ссылающийся на soulprint.hosts (cross-host scenario-only аксессор),
// по-прежнему отвергается на compile (AllowHosts=false в destiny-проходе).
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
		t.Fatal("Render: vars.yml не должен видеть soulprint.hosts (изоляция destiny, не ослаблена var→var)")
	}
}

// TestDestinyFileVars_OverrideWithInLayerRef — кейс #9: одноимённые file/task vars
// (Вариант-A override) + ссылка ВНУТРИ task-слоя цела. file-var `unit` перетёрт
// task-var `unit`, а task-var `svc` ссылается на task-var `unit` (внутрислоевой
// var→var). params видит task-значения.
func TestDestinyFileVars_OverrideWithInLayerRef(t *testing.T) {
	resolved := destinyWithFileVars(
		map[string]any{"unit": "redis-FILE"}, // file-level — будет перетёрт
		map[string]any{"cmd": "${ vars.svc }"},
	)
	resolved.Tasks[0].Vars = map[string]any{
		"unit": "redis-TASK",         // override file-var
		"svc":  "${ vars.unit }-svc", // ссылка на task-var того же слоя
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
		t.Errorf("cmd = %q, want redis-TASK-svc (task-var svc видит task-var unit, override Вариант A)", got)
	}
}

// TestDestinyFileVars_EssenceIsolation — vars.yml-значение не видит essence.*
// (essence — концепция уровня service, в destiny её нет вовсе) → ошибка.
func TestDestinyFileVars_EssenceIsolation(t *testing.T) {
	res := &stubDestinyResolver{resolved: destinyWithFileVars(
		map[string]any{"x": "${ essence.maxmemory }"},
		map[string]any{"cmd": "echo ${ vars.x }"},
	)}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("pilot-vars", map[string]any{}),
		Incarnation: IncarnationMeta{Name: "svc"},
		// scenario несёт essence — destiny НЕ должна его увидеть даже в vars.yml.
		Essence: map[string]any{"maxmemory": "256mb"},
		Hosts:   []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny: res,
	}
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("Render: ожидалась ошибка — vars.yml не должен видеть essence.* (изоляция)")
	}
}

// TestDestinyFileVars_VarToVar — file-var ссылается на другой file-var того же
// слоя (`${ vars.<other> }` РАЗРЕШЕНО, eager-topological); зеркало
// TestResolveTaskVars_VarToVar (кейс #1, file-слой). ИСХОДНЫЙ кейс фичи:
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
		t.Fatalf("Render: var→var внутри file-слоя должен резолвиться: %v", err)
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "echo redis-server" {
		t.Errorf("cmd = %q, want echo redis-server (unit ссылается на base)", got)
	}
}

// TestDestinyFileVars_TransitiveChain — транзитивная цепочка a→b→c того же слоя
// (кейс #2): c вычисляется первым, b видит c, a видит b.
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
		t.Errorf("vars.a = %v, want root/b/a (транзитивно a→b→c)", got["a"])
	}
}

// TestDestinyFileVars_Cycle — цикл a→b→c→a (кейс #3) → ErrVarCycle с трассой.
func TestDestinyFileVars_Cycle(t *testing.T) {
	_, err := resolveVarLayer(newEngine(t), map[string]any{
		"a": "${ vars.b }",
		"b": "${ vars.c }",
		"c": "${ vars.a }",
	}, cel.Vars{})
	if err == nil || !errors.Is(err, ErrVarCycle) {
		t.Fatalf("resolveVarLayer: ожидался ErrVarCycle, получено: %v", err)
	}
	// Трасса замкнута: содержит стартовый узел дважды (a → … → a).
	if !strings.Contains(err.Error(), "a → b → c → a") {
		t.Errorf("err = %v, want трассу 'a → b → c → a'", err)
	}
}

// TestDestinyFileVars_SelfReference — самоссылка a→a (кейс #4) → ErrVarCycle
// (частный случай цикла, трасса 'a → a').
func TestDestinyFileVars_SelfReference(t *testing.T) {
	_, err := resolveVarLayer(newEngine(t), map[string]any{
		"a": "${ vars.a }-loop",
	}, cel.Vars{})
	if err == nil || !errors.Is(err, ErrVarCycle) {
		t.Fatalf("resolveVarLayer: самоссылка должна давать ErrVarCycle, получено: %v", err)
	}
	if !strings.Contains(err.Error(), "a → a") {
		t.Errorf("err = %v, want трассу 'a → a'", err)
	}
}

// TestDestinyFileVars_UnusedBrokenRef — НЕИСПОЛЬЗУЕМЫЙ var ссылается на
// несуществующий vars.z (кейс #5): EAGER-маркер — резолв слоя падает
// ErrVarUnknownRef, даже если ссылающийся var нигде не читается params-ом.
func TestDestinyFileVars_UnusedBrokenRef(t *testing.T) {
	res := &stubDestinyResolver{resolved: destinyWithFileVars(
		map[string]any{
			"used":   "redis-server",
			"broken": "${ vars.z }", // z не существует; broken никто не читает
		},
		map[string]any{"cmd": "echo ${ vars.used }"}, // params читает только used
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
		t.Fatal("Render: битый неиспользуемый var должен ронять рендер EAGER (var_unknown_ref)")
	}
	if !errors.Is(err, ErrVarUnknownRef) {
		t.Errorf("err = %v, want ErrVarUnknownRef", err)
	}
}

// TestDestinyFileVars_OrderIndependent — порядок ключей в file-слое безразличен
// (кейс #7): два варианта raw с разным порядком объявления дают равный результат.
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
			t.Errorf("vars.%s расходится при разном порядке: v1=%v v2=%v", k, v1[k], v2[k])
		}
	}
	if v1["c"] != "root-b-c" {
		t.Errorf("vars.c = %v, want root-b-c", v1["c"])
	}
}

// TestDestinyFileVars_TaskOverridesFile — Вариант A: task-level vars:
// переопределяет одноимённый file-level var того же destiny (детерминированный
// исход — побеждает task).
func TestDestinyFileVars_TaskOverridesFile(t *testing.T) {
	resolved := destinyWithFileVars(
		map[string]any{"unit": "redis-server"}, // file-level
		map[string]any{"cmd": "echo ${ vars.unit }"},
	)
	// task-level vars: на той же задаче с тем же именем — должен победить.
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
		t.Errorf("cmd = %q, want echo redis-staging (task-level vars поверх file-level, Вариант A)", got)
	}
}

// TestDestinyFileVars_TaskAndFileCoexist — несовпадающие имена сосуществуют:
// file-var и task-var обоих видны в params одной задачи.
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
		t.Errorf("cmd = %q, want 'redis-server flag' (file-var + task-var сосуществуют)", got)
	}
}

// TestDestinyFileVars_ScenarioVarsDoNotLeak — destiny НЕ видит scenario-level
// `vars:` (только через apply: input:). scenario task-vars `leak` на apply-задаче
// в destiny-проходе → no-such-key; долетает только то, что проброшено в apply.input.
func TestDestinyFileVars_ScenarioVarsDoNotLeak(t *testing.T) {
	res := &stubDestinyResolver{resolved: destinyWithFileVars(
		nil, // file-vars нет — проверяем, что scenario-vars не подменяют их
		map[string]any{"cmd": "echo ${ vars.leak }"},
	)}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	// applyScenario с scenario task-vars `leak` на самой apply-задаче.
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
		t.Fatal("Render: ожидалась ошибка — scenario vars не должны течь в destiny (только через apply.input)")
	}
}

// TestDestinyFileVars_OnlyViaApplyInput — единственный легальный мост из scenario
// в destiny — apply: input:. scenario.input пробрасывается в destiny-input, а
// vars.yml destiny резолвится над этим input.
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
				// мост: scenario.input.who → destiny input.user.
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

// TestDestinyFileVars_InRenderedTemplateContext — file-level vars.yml доступен
// шаблону core.file.rendered как `.vars.<file_var>` НАПРЯМУЮ, без passthrough
// через params.vars задачи (симметрия Варианта A: render_context.vars = file-vars
// база + task-level params.vars override). Это ровно node-exporter-кейс: шаблон
// читает `.vars.bin_path`, где bin_path — file-var (vars.yml), а в params.vars
// задачи ЕГО НЕТ.
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
						// НЕТ params.vars: file-var bin_path должен дойти НАПРЯМУЮ.
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
		t.Fatalf("render_context.vars.bin_path = %#v, want /usr/local/bin/node_exporter (file-var НАПРЯМУЮ в .vars)", vars["bin_path"])
	}

	// Исполняем тем же движком, что Soul, render_context КОРНЕМ.
	engine, err := tmpl.New()
	if err != nil {
		t.Fatalf("tmpl.New: %v", err)
	}
	out, err := engine.Render(tasks[0].Params.GetFields()[paramTemplateContent].GetStringValue(), rc)
	if err != nil {
		t.Fatalf("soul-render упал (.vars.bin_path недоступен?): %v", err)
	}
	if !strings.Contains(out, "ExecStart=/usr/local/bin/node_exporter") {
		t.Errorf(".vars.bin_path (file-var) не подставлен:\n%s", out)
	}
}

// TestDestinyFileVars_TaskVarsOverrideFileInRenderContext — в render_context.vars
// действует та же Вариант-A семантика, что в CEL-фазе: одноимённый task-level
// params.vars перетирает file-var. Гарантирует, что слияние file→task под `.vars`
// детерминировано (побеждает task), а не теряет один из слоёв.
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
						// task-var bin_path перетирает file-var; extra остаётся file-var.
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
		t.Errorf("render_context.vars.bin_path = %#v, want /from/task (task-var override file-var, Вариант A)", vars["bin_path"])
	}
	if vars["extra"] != "/file-only" {
		t.Errorf("render_context.vars.extra = %#v, want /file-only (file-var без одноимённого task-var)", vars["extra"])
	}
}

// TestDestinyFileVars_PerHost — vars.yml ссылается на soulprint.self → резолвится
// per-host; разные хосты дают разные значения (DestinyVarsResolved по SID).
func TestDestinyFileVars_PerHost(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)

	// Прямой вызов renderApplyDestiny через Render: destiny-задача host-инвариантна
	// в пилоте, поэтому params с per-host soulprint.self упали бы на сверке. Чтобы
	// проверить именно per-host резолв vars, держим params host-инвариантными, а
	// per-host проверяем через resolveDestinyVars напрямую.
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
		t.Errorf("host b vars.family = %v, want rhel (per-host резолв)", got["b"]["family"])
	}
}

// TestDestinyFileVars_StagedInvariant — file-vars инвариантны по Passage:
// резолвятся над input+self+incarnation БЕЗ register, поэтому ActivePassage на них
// не влияет. Сверяем резолв при ActivePassage 0 и 1 — идентичен (ADR-056: вход
// destiny-прохода инвариантен на passages, file-vars тем более).
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
			t.Errorf("vars.%s расходится P0=%v P1=%v — file-vars обязаны быть инвариантны по Passage", key, p0["a"][key], p1["a"][key])
		}
	}
	if p0["a"]["acl"] != "/acl/dave" || p0["a"]["family"] != "debian" {
		t.Errorf("file-vars резолв = %v, want acl=/acl/dave family=debian", p0["a"])
	}
}
