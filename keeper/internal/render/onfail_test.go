package render

import (
	"context"
	"errors"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// onFailScenario — scenario из двух задач: источник с register: migration и
// rescue-обработчик с onfail: [migration].
func onFailScenario(onfail []string) *config.ScenarioManifest {
	return &config.ScenarioManifest{
		Name: "redis",
		Tasks: []config.Task{
			{
				Name:     "migration",
				Register: "migration",
				Module:   &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "redis-migrate up"}},
			},
			{
				Name:   "rollback",
				OnFail: onfail,
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "redis-migrate down"}},
			},
		},
	}
}

// TestRender_OnFail_ResolvesNameToIndex — config.Task.OnFail (register-имя)
// резолвится в render.RenderedTask.OnFailIdx (task-индекс источника, Variant A).
func TestRender_OnFail_ResolvesNameToIndex(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	tasks, _, err := p.Render(context.Background(), onChangesRenderInput(onFailScenario([]string{"migration"})))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2", len(tasks))
	}
	if tasks[0].OnFailIdx != nil {
		t.Errorf("источник: OnFailIdx = %v, want nil (не-onfail-задача)", tasks[0].OnFailIdx)
	}
	got := tasks[1].OnFailIdx
	if len(got) != 1 || got[0] != 0 {
		t.Errorf("rescue: OnFailIdx = %v, want [0] (индекс задачи migration)", got)
	}
}

// TestRender_OnFail_Empty — без onfail: OnFailIdx nil у обеих задач
// (не-onfail-задачи, gating не применяется).
func TestRender_OnFail_Empty(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	tasks, _, err := p.Render(context.Background(), onChangesRenderInput(onFailScenario(nil)))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for i, rt := range tasks {
		if rt.OnFailIdx != nil {
			t.Errorf("tasks[%d].OnFailIdx = %v, want nil", i, rt.OnFailIdx)
		}
	}
}

// TestRender_OnFail_UnknownRegister — onfail ссылается на несуществующий register
// → ошибка рендера (строгий вариант, ловит опечатку register-id; зеркало onchanges).
func TestRender_OnFail_UnknownRegister(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	_, _, err := p.Render(context.Background(), onChangesRenderInput(onFailScenario([]string{"typo_migration"})))
	if err == nil {
		t.Fatal("Render: ожидалась ошибка на несуществующий onfail register, got nil")
	}
	if !errors.Is(err, ErrOnFailUnknownRegister) {
		t.Errorf("err = %v, want ErrOnFailUnknownRegister", err)
	}
}

// TestRender_OnFail_MultiSource — несколько register-имён резолвятся в несколько
// индексов (any-семантика gating-а — на Soul-е).
func TestRender_OnFail_MultiSource(t *testing.T) {
	m := &config.ScenarioManifest{
		Name: "multi",
		Tasks: []config.Task{
			{Name: "a", Register: "a", Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{}}},
			{Name: "b", Register: "b", Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{}}},
			{Name: "rescue", OnFail: []string{"a", "b"}, Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{}}},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	tasks, _, err := p.Render(context.Background(), onChangesRenderInput(m))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := tasks[2].OnFailIdx
	if len(got) != 2 || got[0] != 0 || got[1] != 1 {
		t.Errorf("OnFailIdx = %v, want [0 1]", got)
	}
}

// TestRender_OnFailAndOnChanges_Independent — задача с onchanges И onfail на разные
// источники: оба резолва независимы (require:[migration] + onfail:[migration] —
// типовая связка, destiny/tasks.md §8).
func TestRender_OnFailAndOnChanges_Independent(t *testing.T) {
	m := &config.ScenarioManifest{
		Name: "mix",
		Tasks: []config.Task{
			{Name: "conf", Register: "conf", Module: &config.ModuleTask{Module: "core.file.present", Params: map[string]any{}}},
			{Name: "migration", Register: "migration", Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{}}},
			{
				Name:      "react",
				OnChanges: []string{"conf"},
				OnFail:    []string{"migration"},
				Module:    &config.ModuleTask{Module: "core.service.restarted", Params: map[string]any{}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	tasks, _, err := p.Render(context.Background(), onChangesRenderInput(m))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if oc := tasks[2].OnChangesIdx; len(oc) != 1 || oc[0] != 0 {
		t.Errorf("OnChangesIdx = %v, want [0] (conf)", oc)
	}
	if of := tasks[2].OnFailIdx; len(of) != 1 || of[0] != 1 {
		t.Errorf("OnFailIdx = %v, want [1] (migration)", of)
	}
}
