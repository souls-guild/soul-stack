package render

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// stagedScenario — минимальная staged-фикстура (ADR-056, S3): passage 0 — probe
// роли (register: role), passage 1 — действие, таргетящееся `where: register.role
// == 'master'`. Воспроизводит каноническую идиому probe→where (orchestration.md
// §4-5 / ADR-008 волатильная роль).
const stagedScenario = `
name: staged
description: probe role then act on master
state_changes: {}
tasks:
  - name: probe role
    module: core.exec.run
    register: role
    changed_when: "false"
    params:
      cmd: detect-role
  - name: act on master only
    module: core.exec.run
    where: "register.role.stdout == 'master'"
    params:
      cmd: promote
`

func loadStagedManifest(t *testing.T, src string) *config.ScenarioManifest {
	t.Helper()
	m, _, diags, err := config.LoadScenarioManifestFromBytes("main.yml", []byte(src), config.ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifestFromBytes: %v", err)
	}
	for _, d := range diags {
		if d.Level == diag.LevelError {
			t.Fatalf("scenario diagnostic (%s): %s", d.Code, d.Message)
		}
	}
	return m
}

// TestRender_StagedPassageStamp — Render клеймит RenderedTask.Passage из
// RenderInput.TaskPassage (результат Stratify). probe → Passage 0, потребитель
// register.role → Passage 1 (staged-render, ADR-056 §б): probe и потребитель НЕ
// в одном Passage.
func TestRender_StagedPassageStamp(t *testing.T) {
	m := loadStagedManifest(t, stagedScenario)
	plan, err := Stratify(m.Tasks)
	if err != nil {
		t.Fatalf("Stratify: %v", err)
	}
	if plan.Count != 2 {
		t.Fatalf("Passage.Count = %d, want 2 (probe→consumer)", plan.Count)
	}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    m,
		Input:       map[string]any{},
		Incarnation: IncarnationMeta{Name: "redis-prod", Service: "redis"},
		Hosts: []*topology.HostFacts{
			host("a.example.com", []string{"redis-prod"}, nil),
			host("b.example.com", []string{"redis-prod"}, nil),
		},
		TaskPassage: plan.TaskPassage,
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("rendered tasks = %d, want 2", len(tasks))
	}
	if tasks[0].Passage != 0 {
		t.Errorf("probe Passage = %d, want 0", tasks[0].Passage)
	}
	if tasks[1].Passage != 1 {
		t.Errorf("consumer Passage = %d, want 1", tasks[1].Passage)
	}
}

// TestRender_StagedWhereResolvesPerHostRegister — СЕРДЦЕ staged-render на
// render-тире (ADR-056 §в.1): Passage 1 рендерится с per-host register Passage 0
// (in.RegisterByHost), и `where: register.role.stdout == 'master'` таргетит
// ТОЛЬКО master-хост. Доказывает, что drift закрыт на уровне резолва where:
// (раньше register был пуст → where отбирал 0 хостов).
func TestRender_StagedWhereResolvesPerHostRegister(t *testing.T) {
	m := loadStagedManifest(t, stagedScenario)
	plan, err := Stratify(m.Tasks)
	if err != nil {
		t.Fatalf("Stratify: %v", err)
	}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    m,
		Input:       map[string]any{},
		Incarnation: IncarnationMeta{Name: "redis-prod", Service: "redis"},
		Hosts: []*topology.HostFacts{
			host("a.example.com", []string{"redis-prod"}, nil),
			host("b.example.com", []string{"redis-prod"}, nil),
		},
		TaskPassage: plan.TaskPassage,
		// Stage-loop рендерит Passage 1 с ActivePassage=1: его where: резолвится.
		ActivePassage: 1,
		// Per-host register, накопленный барьером Passage 0: хост a — master,
		// хост b — slave (как probe вернул per-host).
		RegisterByHost: map[string]map[string]any{
			"a.example.com": {"role": map[string]any{"stdout": "master"}},
			"b.example.com": {"role": map[string]any{"stdout": "slave"}},
		},
	}
	_, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// Passage-1 задача (Index 1) обязана таргетить ТОЛЬКО master-хост.
	var consumer *DispatchPlan
	for i := range plans {
		if plans[i].TaskIndex == 1 {
			consumer = &plans[i]
		}
	}
	if consumer == nil {
		t.Fatalf("нет DispatchPlan для Passage-1 задачи (Index 1)")
	}
	if len(consumer.TargetSIDs) != 1 || consumer.TargetSIDs[0] != "a.example.com" {
		t.Fatalf("Passage-1 таргет = %v, want [a.example.com] (only master) — where не резолвнулся per-host register-ом", consumer.TargetSIDs)
	}
}

// TestRender_NoStratifyPlanIsPassage0 — backward-compat: без RenderInput.TaskPassage
// (nil) все RenderedTask несут Passage 0 (N=1 / не-staged caller: Trial / Acolyte
// RenderForHost / CheckDrift). Гарантирует, что не-staged caller получает прежнее
// поведение (БИТ-В-БИТ — стамп Passage не активируется).
func TestRender_NoStratifyPlanIsPassage0(t *testing.T) {
	// N=1-сценарий (без register-зависимостей): не-staged caller рендерит как до
	// staged-render. register-зависимый where тут отсутствует — иначе пустой
	// register у первого прохода был бы ошибкой (это и есть исходный drift,
	// закрываемый staged-loop-ом, а не render-фазой без register).
	const plain = `
name: plain
description: two independent tasks
state_changes: {}
tasks:
  - name: first
    module: core.exec.run
    params: { cmd: echo }
  - name: second
    module: core.exec.run
    params: { cmd: echo }
`
	m := loadStagedManifest(t, plain)

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    m,
		Input:       map[string]any{},
		Incarnation: IncarnationMeta{Name: "redis-prod", Service: "redis"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"redis-prod"}, nil)},
		// TaskPassage не задан (nil): не-staged caller.
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, tk := range tasks {
		if tk.Passage != 0 {
			t.Errorf("task %q Passage = %d, want 0 (nil TaskPassage → Passage 0)", tk.Name, tk.Passage)
		}
	}
}
