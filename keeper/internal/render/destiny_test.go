package render

import (
	"context"
	"errors"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// stubDestinyResolver — in-memory резолвер для unit-тестов apply:destiny.
type stubDestinyResolver struct {
	resolved *ResolvedDestiny
	err      error
	gotName  string
}

func (s *stubDestinyResolver) Resolve(_ context.Context, name string) (*ResolvedDestiny, error) {
	s.gotName = name
	if s.err != nil {
		return nil, s.err
	}
	return s.resolved, nil
}

// flatDestiny — плоская тест-destiny из двух module-задач, читающих свой input.
func flatDestiny() *ResolvedDestiny {
	return &ResolvedDestiny{
		Name: "pilot-flat",
		Input: config.InputSchemaMap{
			"marker_file":    {Type: "string", Required: true},
			"marker_payload": {Type: "string", Required: true},
			"marker_mode":    {Type: "string", Default: "0644"},
		},
		Tasks: []config.Task{
			{
				Name: "Lay down the marker file",
				Module: &config.ModuleTask{
					Module: "core.file.present",
					Params: map[string]any{
						"path":    "${ input.marker_file }",
						"content": "${ input.marker_payload }",
						"mode":    "${ input.marker_mode }",
					},
				},
			},
			{
				Name:        "Record placement",
				ChangedWhen: "false",
				Module: &config.ModuleTask{
					Module: "core.exec.run",
					Params: map[string]any{"cmd": "echo ${ input.marker_file }"},
				},
			},
		},
	}
}

// applyScenario — сценарий с одной apply:destiny-задачей.
func applyScenario(destiny string, applyInput map[string]any) *config.ScenarioManifest {
	return &config.ScenarioManifest{
		Name: "create",
		Tasks: []config.Task{
			{
				Name:  "Apply destiny",
				Apply: &config.ApplyTask{Destiny: destiny, Input: applyInput},
			},
		},
	}
}

// TestRender_ApplyDestiny_Expands — apply:destiny раскрывается в задачи destiny
// со сквозными индексами; apply.input резолвит params; default добирается.
func TestRender_ApplyDestiny_Expands(t *testing.T) {
	res := &stubDestinyResolver{resolved: flatDestiny()}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("pilot-flat", map[string]any{"marker_file": "${ input.path }", "marker_payload": "${ input.content }"}),
		Input:       map[string]any{"path": "/etc/marker", "content": "ok"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}

	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if res.gotName != "pilot-flat" {
		t.Errorf("resolver got name %q, want pilot-flat", res.gotName)
	}
	if len(tasks) != 2 || len(plans) != 2 {
		t.Fatalf("len(tasks)=%d plans=%d, want 2/2", len(tasks), len(plans))
	}
	if tasks[0].Index != 0 || tasks[1].Index != 1 {
		t.Errorf("indices = %d,%d, want 0,1 (сквозные)", tasks[0].Index, tasks[1].Index)
	}
	if tasks[0].Module != "core.file.present" {
		t.Errorf("task0 module = %q", tasks[0].Module)
	}
	f0 := tasks[0].Params.GetFields()
	if got := f0["path"].GetStringValue(); got != "/etc/marker" {
		t.Errorf("path = %q, want /etc/marker (из apply.input ← scenario.input.path)", got)
	}
	if got := f0["content"].GetStringValue(); got != "ok" {
		t.Errorf("content = %q, want ok", got)
	}
	if got := f0["mode"].GetStringValue(); got != "0644" {
		t.Errorf("mode = %q, want 0644 (добран из default destiny)", got)
	}
	cmd := tasks[1].Params.GetFields()["cmd"].GetStringValue()
	if cmd != "echo /etc/marker" {
		t.Errorf("command = %q, want echo /etc/marker", cmd)
	}
}

// TestRender_ApplyDestiny_Isolation — destiny НЕ видит scenario-scope: ссылка на
// scenario-input, не проброшенный через apply.input, падает (no such key), а не
// тихо подхватывает значение родителя.
func TestRender_ApplyDestiny_Isolation(t *testing.T) {
	leaky := flatDestiny()
	// Задача destiny ссылается на input.secret_from_scenario, которого НЕТ в
	// apply.input — в изолированном env его быть не должно.
	leaky.Tasks[0].Module.Params["content"] = "${ input.secret_from_scenario }"
	res := &stubDestinyResolver{resolved: leaky}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: applyScenario("pilot-flat", map[string]any{"marker_file": "/m", "marker_payload": "p"}),
		// scenario-scope содержит secret_from_scenario — destiny НЕ должна его увидеть.
		Input:       map[string]any{"secret_from_scenario": "LEAK"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}

	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("Render: ожидалась ошибка — destiny не должна видеть scenario-input (изоляция ADR-009)")
	}
}

// TestRender_ApplyDestiny_MissingRequired — обязательный input destiny не передан
// через apply.input и без default → ошибка контракта.
func TestRender_ApplyDestiny_MissingRequired(t *testing.T) {
	res := &stubDestinyResolver{resolved: flatDestiny()}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		// marker_payload (required, без default) не передан.
		Scenario:    applyScenario("pilot-flat", map[string]any{"marker_file": "/m"}),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("Render: ожидалась ошибка на отсутствующий обязательный input destiny")
	}
}

// TestRender_ApplyDestiny_NilResolver — apply:destiny без резолвера → ErrUnsupportedDSL.
func TestRender_ApplyDestiny_NilResolver(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("pilot-flat", nil),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		// Destiny: nil
	}
	_, _, err := p.Render(context.Background(), in)
	if !errors.Is(err, ErrUnsupportedDSL) {
		t.Fatalf("err = %v, want ErrUnsupportedDSL", err)
	}
}

// TestRender_ApplyDestiny_UnexpandedInclude — within-destiny include
// раскрывается до render (в загрузчике/fixture-резолвере). Если резолвер отдал
// ResolvedDestiny с нераскрытым include — render ловит его как ErrUnexpandedInclude
// (баг раскрытия, не «вне pilot»).
func TestRender_ApplyDestiny_UnexpandedInclude(t *testing.T) {
	nested := flatDestiny()
	nested.Tasks = append(nested.Tasks, config.Task{
		Name:    "nested include",
		Include: &config.IncludeTask{Include: "more.yml"},
	})
	res := &stubDestinyResolver{resolved: nested}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("pilot-flat", map[string]any{"marker_file": "/m", "marker_payload": "p"}),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	_, _, err := p.Render(context.Background(), in)
	if !errors.Is(err, ErrUnexpandedInclude) {
		t.Fatalf("err = %v, want ErrUnexpandedInclude (нераскрытый include в destiny)", err)
	}
}

// TestRender_ApplyDestiny_RejectsSerial — задача внутри destiny не может нести
// scenario-only оркестрационный ключ serial: (guardDestinyTask, destiny.go).
// serial: scenario-уровня наследуется destiny через параметр renderApplyDestiny,
// не через поле destiny-задачи → собственный serial: на destiny-задаче →
// ErrUnsupportedDSL.
func TestRender_ApplyDestiny_RejectsSerial(t *testing.T) {
	d := flatDestiny()
	d.Tasks[0].Serial = 1
	res := &stubDestinyResolver{resolved: d}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("pilot-flat", map[string]any{"marker_file": "/m", "marker_payload": "p"}),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	_, _, err := p.Render(context.Background(), in)
	if !errors.Is(err, ErrUnsupportedDSL) {
		t.Fatalf("err = %v, want ErrUnsupportedDSL (serial: на destiny-задаче)", err)
	}
}

// TestRender_ApplyDestiny_RejectsRunOnce — симметрично serial: задача внутри
// destiny не может нести run_once: (guardDestinyTask, destiny.go) → ErrUnsupportedDSL.
func TestRender_ApplyDestiny_RejectsRunOnce(t *testing.T) {
	d := flatDestiny()
	d.Tasks[1].RunOnce = true
	res := &stubDestinyResolver{resolved: d}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("pilot-flat", map[string]any{"marker_file": "/m", "marker_payload": "p"}),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	_, _, err := p.Render(context.Background(), in)
	if !errors.Is(err, ErrUnsupportedDSL) {
		t.Fatalf("err = %v, want ErrUnsupportedDSL (run_once: на destiny-задаче)", err)
	}
}

// TestRender_ApplyDestiny_ResolverError — ошибка резолвера пробрасывается.
func TestRender_ApplyDestiny_ResolverError(t *testing.T) {
	res := &stubDestinyResolver{err: errors.New("not found in registry")}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("ghost", nil),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("Render: ожидалась проброшенная ошибка резолвера")
	}
}

// TestRender_ApplyDestiny_MixedPlan — scenario с module-задачей ДО apply:destiny:
// сквозные индексы продолжаются через границу apply.
func TestRender_ApplyDestiny_MixedPlan(t *testing.T) {
	res := &stubDestinyResolver{resolved: flatDestiny()}
	scn := &config.ScenarioManifest{
		Name: "create",
		Tasks: []config.Task{
			{Name: "pre", Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo pre"}}},
			{Name: "apply", Apply: &config.ApplyTask{Destiny: "pilot-flat", Input: map[string]any{"marker_file": "/m", "marker_payload": "p"}}},
			{Name: "post", Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo post"}}},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    scn,
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// pre(0) + destiny(1,2) + post(3) = 4 задачи со сквозными индексами.
	if len(tasks) != 4 {
		t.Fatalf("len(tasks) = %d, want 4", len(tasks))
	}
	wantIdx := []int{0, 1, 2, 3}
	for i, rt := range tasks {
		if rt.Index != wantIdx[i] {
			t.Errorf("tasks[%d].Index = %d, want %d", i, rt.Index, wantIdx[i])
		}
	}
	if tasks[3].Module != "core.exec.run" || tasks[3].Params.GetFields()["cmd"].GetStringValue() != "echo post" {
		t.Errorf("post-задача после destiny отрендерена неверно: %+v", tasks[3])
	}
}

// TestRender_ApplyDestiny_RejectsNestedApply — вложенный apply: внутри destiny
// (apply:destiny → задача destiny сама несёт apply:) → ErrUnsupportedDSL
// (guardDestinyTask, case task.Apply != nil). apply:destiny — одноуровневая
// раскладка (V2, ADR-009); рекурсивная вложенность apply вне пилот-объёма.
// Существующие destiny-guard-тесты покрывают serial/run_once/loop/include внутри
// destiny, но НЕ вложенный apply — единственную ветку guardDestinyTask без теста.
func TestRender_ApplyDestiny_RejectsNestedApply(t *testing.T) {
	d := flatDestiny()
	d.Tasks = append(d.Tasks, config.Task{
		Name:  "nested apply",
		Apply: &config.ApplyTask{Destiny: "another", Input: map[string]any{}},
	})
	res := &stubDestinyResolver{resolved: d}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("pilot-flat", map[string]any{"marker_file": "/m", "marker_payload": "p"}),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	_, _, err := p.Render(context.Background(), in)
	if !errors.Is(err, ErrUnsupportedDSL) {
		t.Fatalf("err = %v, want ErrUnsupportedDSL (вложенный apply: в destiny)", err)
	}
}

// TestRender_ApplyDestiny_RejectsNestedBlock — block: внутри destiny →
// ErrUnsupportedDSL (guardDestinyTask, case task.Block != nil). Дополняет тираж
// guard-веток: block — единственная оставшаяся непокрытая ветка после
// apply/include/loop/serial/run_once.
func TestRender_ApplyDestiny_RejectsNestedBlock(t *testing.T) {
	d := flatDestiny()
	d.Tasks = append(d.Tasks, config.Task{
		Name:  "nested block",
		Block: &config.BlockTask{Block: []config.Task{}},
	})
	res := &stubDestinyResolver{resolved: d}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("pilot-flat", map[string]any{"marker_file": "/m", "marker_payload": "p"}),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	_, _, err := p.Render(context.Background(), in)
	if !errors.Is(err, ErrUnsupportedDSL) {
		t.Fatalf("err = %v, want ErrUnsupportedDSL (block: в destiny)", err)
	}
}
