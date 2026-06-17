package render

import (
	"context"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// TestRender_InterpolatesEssence — `${ essence.* }` в params рендерится из
// RenderInput.Essence (slice E2 passthrough). Доказывает, что essence доходит
// до per-host CEL-фазы через весь конвейер Render.
func TestRender_InterpolatesEssence(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "cfg",
		Tasks: []config.Task{
			{
				Name:   "write conn",
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "connect ${ essence.db.host }"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Essence:     map[string]any{"db": map[string]any{"host": "pg-primary"}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "connect pg-primary" {
		t.Errorf("command = %q, want %q", got, "connect pg-primary")
	}
}

// TestRender_EssenceInWhere — essence доступна в expression-key where:.
func TestRender_EssenceInWhere(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "gated",
		Tasks: []config.Task{
			{
				Name:   "feature gate",
				Where:  "essence.feature.enabled",
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "go"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	enabled := RenderInput{
		Scenario:    manifest,
		Essence:     map[string]any{"feature": map[string]any{"enabled": true}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	_, plans, err := p.Render(context.Background(), enabled)
	if err != nil {
		t.Fatalf("Render(enabled): %v", err)
	}
	if got := plans[0].TargetSIDs; len(got) != 1 {
		t.Errorf("enabled: TargetSIDs = %v, want 1 host", got)
	}

	disabled := enabled
	disabled.Essence = map[string]any{"feature": map[string]any{"enabled": false}}
	_, plans, err = p.Render(context.Background(), disabled)
	if err != nil {
		t.Fatalf("Render(disabled): %v", err)
	}
	if got := plans[0].TargetSIDs; len(got) != 0 {
		t.Errorf("disabled: TargetSIDs = %v, want 0 hosts (where отфильтровал)", got)
	}
}

// TestRender_LoopOverEssence — `items: ${ essence.users }` раскрывается по
// essence (host-инвариантный источник оси loop).
func TestRender_LoopOverEssence(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "x",
		Tasks: []config.Task{loopTask(
			&config.LoopSpec{Items: "${ essence.users }", As: "user"},
			map[string]any{"cmd": "useradd ${ user.name }"},
		)},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: manifest,
		Essence: map[string]any{"users": []any{
			map[string]any{"name": "alice"},
			map[string]any{"name": "bob"},
		}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2", len(tasks))
	}
	if cmdOf(t, tasks[0]) != "useradd alice" || cmdOf(t, tasks[1]) != "useradd bob" {
		t.Errorf("loop commands = %q, %q", cmdOf(t, tasks[0]), cmdOf(t, tasks[1]))
	}
}

// TestRender_EmptyEssenceNoLeak — отсутствие Essence в RenderInput не ломает
// прогон, который essence не трогает (нет паники, нет leak в env).
func TestRender_EmptyEssenceNoLeak(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "plain",
		Tasks: []config.Task{
			{Name: "noop", Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo hi"}}},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := cmdOf(t, tasks[0]); got != "echo hi" {
		t.Errorf("command = %q, want %q", got, "echo hi")
	}
}

// TestRender_ApplyDestiny_EssenceNotLeaked — изоляция destiny (slice A):
// destiny НЕ видит essence напрямую. Несмотря на непустой Essence в scenario-
// scope, ссылка `${ essence.* }` внутри destiny-задачи даёт eval-ошибку
// (no-such-key), т.к. renderApplyDestiny строит destiny-RenderInput с пустым
// Essence.
func TestRender_ApplyDestiny_EssenceNotLeaked(t *testing.T) {
	leaky := &ResolvedDestiny{
		Name: "leaky",
		Tasks: []config.Task{
			{
				Name:   "peek essence",
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo ${ essence.secret }"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("leaky", nil),
		Essence:     map[string]any{"secret": "topsecret"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
		Destiny:     &stubDestinyResolver{resolved: leaky},
	}
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("ожидали eval-ошибку: essence не должен быть виден в destiny-проходе")
	}
	if !strings.Contains(err.Error(), "essence") {
		t.Errorf("ошибка не про essence: %v", err)
	}
}

// TestRender_ApplyDestiny_EssenceViaInput — корректный путь essence в destiny:
// через apply: input:. essence резолвится в scenario-env (parent), значение
// проброшено в input destiny, который видит только результат.
func TestRender_ApplyDestiny_EssenceViaInput(t *testing.T) {
	dst := &ResolvedDestiny{
		Name:  "via-input",
		Input: config.InputSchemaMap{"db_host": {Type: "string", Required: true}},
		Tasks: []config.Task{
			{
				Name:   "use host",
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "connect ${ input.db_host }"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("via-input", map[string]any{"db_host": "${ essence.db.host }"}),
		Essence:     map[string]any{"db": map[string]any{"host": "pg-primary"}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
		Destiny:     &stubDestinyResolver{resolved: dst},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "connect pg-primary" {
		t.Errorf("command = %q, want %q (essence через apply:input)", got, "connect pg-primary")
	}
}
