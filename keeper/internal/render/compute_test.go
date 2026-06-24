package render

import (
	"context"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// compute: — scenario-level вычисляемые vars (ADR-009 amendment 2026-06-23):
// резолв ОДИН раз в рун-уровневом контексте (без soulprint), результат
// `compute.<name>` виден в apply.input и в state_changes БИТ-В-БИТ (drift снят).

// resolveCompute: цепочка (compute ссылается на ранний compute) + рун-уровневый
// контекст input/essence. Порядок объявления значим.
func TestResolveCompute_ChainAndContext(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "create",
		Compute: config.ComputeBlock{
			{Name: "base", Value: "${ merge(essence.defaults, default(input.over, {})) }"},
			{Name: "full", Value: "${ merge(compute.base, { 'extra': 'yes' }) }"},
			{Name: "n", Value: int64(7)},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: manifest,
		Essence:  map[string]any{"defaults": map[string]any{"a": "1", "b": "2"}},
		Input:    map[string]any{"over": map[string]any{"b": "9"}},
		Hosts:    []*topology.HostFacts{host("h1", []string{"create"}, nil)},
		Ctx:      context.Background(),
	}
	got, err := p.resolveCompute(in)
	if err != nil {
		t.Fatalf("resolveCompute: %v", err)
	}
	base, _ := got["base"].(map[string]any)
	if base["a"] != "1" || base["b"] != "9" {
		t.Fatalf("compute.base = %#v, want {a:1, b:9} (input.over.b бьёт essence)", got["base"])
	}
	full, _ := got["full"].(map[string]any)
	if full["a"] != "1" || full["b"] != "9" || full["extra"] != "yes" {
		t.Fatalf("compute.full = %#v, want base + extra:yes (ссылка на ранний compute)", got["full"])
	}
	if got["n"] != int64(7) {
		t.Fatalf("compute.n = %#v, want литерал 7", got["n"])
	}
}

// ★ Барьер изоляции #2: compute-выражение, ссылающееся на soulprint.self,
// падает с no-such-key — резолв-контекст compute рун-уровневый (без soulprint),
// поэтому compute host-инвариантна и безопасна в state_changes.
func TestResolveCompute_BarrierSoulprint(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "create",
		Compute: config.ComputeBlock{
			{Name: "ip", Value: "${ soulprint.self.network.primary_ip }"},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: manifest,
		Hosts: []*topology.HostFacts{host("h1", []string{"create"},
			map[string]any{"network": map[string]any{"primary_ip": "10.0.0.1"}})},
		Ctx: context.Background(),
	}
	_, err := p.resolveCompute(in)
	if err == nil {
		t.Fatalf("★ ожидалась ошибка: compute не должен видеть soulprint (барьер host-инвариантности)")
	}
	if !strings.Contains(err.Error(), "compute.ip") {
		t.Fatalf("ошибка должна указывать на compute.ip, получено: %v", err)
	}
}

// compute доступен в apply.input (через params/where рендер) И в state_changes,
// одно и то же значение (drift-guard снят самим compute). Прогон через Render +
// RenderStateOps на одном RenderInput.
func TestCompute_SameValueInTasksAndStateChanges(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "create",
		Compute: config.ComputeBlock{
			{Name: "cfg", Value: "${ merge(essence.base, { 'maxmemory': string(int(input.mb)) + 'mb' }) }"},
		},
		StateChanges: &config.StateChanges{
			IsList: true,
			Ops: []config.StateChange{
				{Verb: config.VerbSet, Field: "redis_config", Value: "${ compute.cfg }"},
			},
		},
		Tasks: []config.Task{
			{
				Name: "render",
				Module: &config.ModuleTask{
					Module: "core.noop.run",
					Params: map[string]any{"config": "${ compute.cfg }"},
				},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Essence:     map[string]any{"base": map[string]any{"appendonly": "yes"}},
		Input:       map[string]any{"mb": int64(512)},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("h1", []string{"create"}, nil)},
		Ctx:         context.Background(),
	}

	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(tasks))
	}
	taskCfg := tasks[0].Params.GetFields()["config"].GetStructValue().GetFields()
	if taskCfg["appendonly"].GetStringValue() != "yes" || taskCfg["maxmemory"].GetStringValue() != "512mb" {
		t.Fatalf("apply.input.config = %v, want {appendonly:yes, maxmemory:512mb}", taskCfg)
	}

	ops, err := p.RenderStateOps(in)
	if err != nil {
		t.Fatalf("RenderStateOps: %v", err)
	}
	if len(ops) != 1 || ops[0].Field != "redis_config" {
		t.Fatalf("ops = %+v, want one set redis_config", ops)
	}
	stateCfg, _ := ops[0].Value.(map[string]any)
	if stateCfg["appendonly"] != "yes" || stateCfg["maxmemory"] != "512mb" {
		t.Fatalf("state_changes.redis_config = %#v, want {appendonly:yes, maxmemory:512mb} (== apply.input.config)", ops[0].Value)
	}
}

// ★ Барьер изоляции #1: compute НЕ протекает в изолированный destiny-проход.
// Структурный: destinyIn (destiny.go:99-107) НЕ несёт поле Compute, поэтому
// resolveCompute для destiny-входа даёт nil — `compute.<name>` внутри destiny =
// штатный no-such-key.
func TestCompute_NotLeakingIntoDestiny(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	destinyIn := RenderInput{
		Scenario:        &config.ScenarioManifest{Name: "redis"},
		destinyIsolated: true,
		Ctx:             context.Background(),
	}
	got, err := p.resolveCompute(destinyIn)
	if err != nil {
		t.Fatalf("resolveCompute(destiny): %v", err)
	}
	if got != nil {
		t.Fatalf("★ destiny compute = %#v, want nil (изоляция: compute не протекает в destiny)", got)
	}
}

// ★ Барьер изоляции #1 (ПОЗИТИВНЫЙ, end-to-end): сценарий с НЕпустым compute: +
// apply:destiny, чей destiny-шаг ссылается на ${ compute.x } — полный Render
// падает no-such-key (destinyIn.Compute не пробрасывается, destiny.go:99-107).
// Доказывает структурную изоляцию НА РЕАЛЬНОМ проходе (не только resolveCompute
// в вакууме): родитель compute резолвит (apply.input компонует значение), а
// внутри destiny `compute.*` недоступен — значение НЕ доезжает.
func TestCompute_NotLeakingIntoDestiny_RenderThrough(t *testing.T) {
	// destiny ссылается на compute.cfg, которого в изолированном env нет.
	leaky := flatDestiny()
	leaky.Tasks[0].Module.Params["content"] = "${ compute.cfg }"
	res := &stubDestinyResolver{resolved: leaky}

	scenario := applyScenario("pilot-flat", map[string]any{"marker_file": "/m", "marker_payload": "p"})
	// НЕпустой compute: на родителе — он резолвится в scenario-scope, но в destiny
	// НЕ пробрасывается.
	scenario.Compute = config.ComputeBlock{
		{Name: "cfg", Value: "${ merge(essence.base, {}) }"},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    scenario,
		Essence:     map[string]any{"base": map[string]any{"appendonly": "yes"}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}

	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("★ Render: ожидалась ошибка — destiny не должна видеть compute (изоляция, compute.* недоступен в destiny-проходе)")
	}
}

// ★ Forward-reference запрещён: compute[i] ссылается на compute[j], объявленный
// ПОЗЖЕ → no-such-key (резолв строго в порядке объявления, acc копит только уже
// вычисленное). Доказывает, что forward-ref не «подхватывает» позднее значение
// тихо, а честно падает.
func TestResolveCompute_ForwardReferenceIsNoSuchKey(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "create",
		Compute: config.ComputeBlock{
			// early ссылается на late, объявленный ниже — на момент резолва early
			// в acc его ещё нет.
			{Name: "early", Value: "${ compute.late }"},
			{Name: "late", Value: "ready"},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: manifest,
		Hosts:    []*topology.HostFacts{host("h1", []string{"create"}, nil)},
		Ctx:      context.Background(),
	}
	_, err := p.resolveCompute(in)
	if err == nil {
		t.Fatal("★ ожидалась ошибка: forward-reference compute.late из compute.early (объявлен позже)")
	}
	if !strings.Contains(err.Error(), "compute.early") {
		t.Fatalf("ошибка должна указывать на compute.early, получено: %v", err)
	}
}

// ★ Битый CEL внутри compute-выражения → ошибка обёрнута как render: compute.<name>:
// (а не паника/тихий пропуск). Доказывает, что синтаксическая ошибка выражения
// атрибутируется конкретной compute-записи.
func TestResolveCompute_BrokenCELWrapped(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "create",
		Compute: config.ComputeBlock{
			{Name: "bad", Value: "${ 1 + + }"}, // синтаксически невалидное CEL
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: manifest,
		Hosts:    []*topology.HostFacts{host("h1", []string{"create"}, nil)},
		Ctx:      context.Background(),
	}
	_, err := p.resolveCompute(in)
	if err == nil {
		t.Fatal("★ ожидалась ошибка на битом CEL внутри compute.bad")
	}
	if !strings.Contains(err.Error(), "render: compute.bad") {
		t.Fatalf("ошибка должна быть обёрнута как 'render: compute.bad: ...', получено: %v", err)
	}
}
