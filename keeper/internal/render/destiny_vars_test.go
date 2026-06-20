package render

import (
	"context"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
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

// TestDestinyFileVars_NoSelfReference — vars.yml-значение не видит другие
// file-vars (`vars.<other>` → no-such-key); зеркало TestResolveTaskVars_NoSelfReference.
func TestDestinyFileVars_NoSelfReference(t *testing.T) {
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
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("Render: ожидалась ошибка — vars.yml не должен ссылаться на vars.<other> (нет перекрёстных ссылок)")
	}
	if !strings.Contains(err.Error(), "vars.unit") {
		t.Errorf("err = %v, want упоминание vars.unit", err)
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
