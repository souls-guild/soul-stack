package render

import (
	"context"
	"errors"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// onChangesScenario — a two-task scenario: a source with register: redis_conf
// and a consumer with onchanges: [redis_conf].
func onChangesScenario(onchanges []string) *config.ScenarioManifest {
	return &config.ScenarioManifest{
		Name: "redis",
		Tasks: []config.Task{
			{
				Name:     "redis_conf",
				Register: "redis_conf",
				Module:   &config.ModuleTask{Module: "core.file.present", Params: map[string]any{"path": "/etc/redis.conf"}},
			},
			{
				Name:      "restart",
				OnChanges: onchanges,
				Module:    &config.ModuleTask{Module: "core.service.restarted", Params: map[string]any{"name": "redis"}},
			},
		},
	}
}

func onChangesRenderInput(m *config.ScenarioManifest) RenderInput {
	return RenderInput{
		Scenario:    m,
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{{SID: "a", Coven: []string{"svc"}}},
	}
}

// TestRender_OnChanges_ResolvesNameToIndex — config.Task.OnChanges (register
// name) resolves into render.RenderedTask.OnChangesIdx (source task index,
// Variant A).
func TestRender_OnChanges_ResolvesNameToIndex(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	tasks, _, err := p.Render(context.Background(), onChangesRenderInput(onChangesScenario([]string{"redis_conf"})))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2", len(tasks))
	}
	if tasks[0].OnChangesIdx != nil {
		t.Errorf("источник: OnChangesIdx = %v, want nil (безусловный запуск)", tasks[0].OnChangesIdx)
	}
	got := tasks[1].OnChangesIdx
	if len(got) != 1 || got[0] != 0 {
		t.Errorf("потребитель: OnChangesIdx = %v, want [0] (индекс задачи redis_conf)", got)
	}
}

// TestRender_OnChanges_Empty — without onchanges: OnChangesIdx is nil for both
// tasks (unconditional run, no-gating behavior).
func TestRender_OnChanges_Empty(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	tasks, _, err := p.Render(context.Background(), onChangesRenderInput(onChangesScenario(nil)))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for i, rt := range tasks {
		if rt.OnChangesIdx != nil {
			t.Errorf("tasks[%d].OnChangesIdx = %v, want nil", i, rt.OnChangesIdx)
		}
	}
}

// TestRender_OnChanges_UnknownRegister — onchanges references a nonexistent
// register → render error (strict variant, catches a register-id typo).
func TestRender_OnChanges_UnknownRegister(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	_, _, err := p.Render(context.Background(), onChangesRenderInput(onChangesScenario([]string{"typo_conf"})))
	if err == nil {
		t.Fatal("Render: ожидалась ошибка на несуществующий onchanges register, got nil")
	}
	if !errors.Is(err, ErrOnChangesUnknownRegister) {
		t.Errorf("err = %v, want ErrOnChangesUnknownRegister", err)
	}
}

// TestRender_OnChanges_MultiSource — multiple register names resolve into
// multiple indices (any-semantics gating happens on Soul).
func TestRender_OnChanges_MultiSource(t *testing.T) {
	m := &config.ScenarioManifest{
		Name: "multi",
		Tasks: []config.Task{
			{Name: "a", Register: "a", Module: &config.ModuleTask{Module: "core.file.present", Params: map[string]any{}}},
			{Name: "b", Register: "b", Module: &config.ModuleTask{Module: "core.file.present", Params: map[string]any{}}},
			{Name: "restart", OnChanges: []string{"a", "b"}, Module: &config.ModuleTask{Module: "core.service.restarted", Params: map[string]any{}}},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	tasks, _, err := p.Render(context.Background(), onChangesRenderInput(m))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := tasks[2].OnChangesIdx
	if len(got) != 2 || got[0] != 0 || got[1] != 1 {
		t.Errorf("OnChangesIdx = %v, want [0 1]", got)
	}
}
