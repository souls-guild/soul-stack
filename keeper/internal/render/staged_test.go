package render

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// stagedScenario is a minimal staged fixture (ADR-056, S3): passage 0 probes
// the role (register: role), passage 1 acts, targeting `where:
// register.role == 'master'`. Reproduces the canonical probe→where idiom
// (orchestration.md §4-5 / ADR-008 volatile role).
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

// TestRender_StagedPassageStamp — Render stamps RenderedTask.Passage from
// RenderInput.TaskPassage (Stratify's result). probe → Passage 0, the
// register.role consumer → Passage 1 (staged render, ADR-056 §b): probe and
// consumer are NOT in the same Passage.
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

// TestRender_StagedWhereResolvesPerHostRegister is the HEART of staged
// render on the render tier (ADR-056 §c.1): Passage 1 renders with Passage
// 0's per-host register (in.RegisterByHost), and `where:
// register.role.stdout == 'master'` targets ONLY the master host. Proves the
// drift is closed at the where: resolve level (previously register was empty
// → where selected 0 hosts).
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
		// The stage loop renders Passage 1 with ActivePassage=1: its where:
		// gets resolved.
		ActivePassage: 1,
		// Per-host register accumulated by Passage 0's barrier: host a is
		// master, host b is slave (as probe returned per-host).
		RegisterByHost: map[string]map[string]any{
			"a.example.com": {"role": map[string]any{"stdout": "master"}},
			"b.example.com": {"role": map[string]any{"stdout": "slave"}},
		},
	}
	_, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// The Passage-1 task (Index 1) must target ONLY the master host.
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

// TestRender_NoStratifyPlanIsPassage0 — backward compat: without
// RenderInput.TaskPassage (nil) every RenderedTask carries Passage 0 (N=1 /
// non-staged caller: Trial / Acolyte RenderForHost / CheckDrift). Guarantees a
// non-staged caller gets the previous behavior (BIT-FOR-BIT — Passage
// stamping isn't activated).
func TestRender_NoStratifyPlanIsPassage0(t *testing.T) {
	// N=1 scenario (no register dependencies): a non-staged caller renders as
	// before staged render. There's no register-dependent where here —
	// otherwise an empty register on the first pass would be an error (that's
	// exactly the original drift closed by the staged loop, not by the
	// render phase without register).
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
		// TaskPassage isn't set (nil): non-staged caller.
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
