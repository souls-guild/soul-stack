package render

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// loopDestiny — destiny с одной loop:-задачей по input.changes (зеркало
// service/redis-cluster scenario update_acl → destiny update_acls). Каждый item
// разворачивается в отдельную RenderedTask. input.changes передаётся через
// apply.input родителя.
func loopDestiny() *ResolvedDestiny {
	return &ResolvedDestiny{
		Name: "pilot-loop",
		Input: config.InputSchemaMap{
			"changes": {Type: "array", Required: true},
		},
		Tasks: []config.Task{
			{
				Name: "Apply ACL patch per user",
				Loop: &config.LoopSpec{
					Items:   "${ input.changes }",
					As:      "change",
					IndexAs: "username",
				},
				Module: &config.ModuleTask{
					Module: "core.cmd.shell",
					Params: map[string]any{
						"cmd": "redis-cli ACL SETUSER ${ username } ${ change.acl }",
					},
				},
			},
		},
	}
}

// TestRender_ApplyDestiny_Loop_Expands — loop ВНУТРИ destiny (слайс E снят):
// одна loop-задача разворачивается в N RenderedTask по элементам input.changes,
// биндинг item (<as>/<index_as>) корректен, индексы сквозные. Зеркало
// scenario add_acl_user, но loop живёт в destiny-задаче.
func TestRender_ApplyDestiny_Loop_Expands(t *testing.T) {
	res := &stubDestinyResolver{resolved: loopDestiny()}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: applyScenario("pilot-loop", map[string]any{"changes": "${ input.changes }"}),
		Input: map[string]any{"changes": map[string]any{
			"alice": map[string]any{"acl": "~* +@all"},
			"bob":   map[string]any{"acl": "~foo +get"},
		}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}

	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// object → 2 итерации, порядок алфавитный по ключам (alice, bob).
	if len(tasks) != 2 || len(plans) != 2 {
		t.Fatalf("len(tasks)=%d plans=%d, want 2/2 (loop в destiny развёрнут)", len(tasks), len(plans))
	}
	if tasks[0].Index != 0 || tasks[1].Index != 1 {
		t.Errorf("indices = %d,%d, want 0,1 (сквозные через loop в destiny)", tasks[0].Index, tasks[1].Index)
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "redis-cli ACL SETUSER alice ~* +@all" {
		t.Errorf("tasks[0].cmd = %q (биндинг username/change.acl первой итерации)", got)
	}
	if got := tasks[1].Params.GetFields()["cmd"].GetStringValue(); got != "redis-cli ACL SETUSER bob ~foo +get" {
		t.Errorf("tasks[1].cmd = %q (биндинг второй итерации)", got)
	}
	if plans[0].TaskIndex != 0 || plans[1].TaskIndex != 1 {
		t.Errorf("plans indices = %d,%d, want 0,1", plans[0].TaskIndex, plans[1].TaskIndex)
	}
}

// TestRender_ApplyDestiny_Loop_MixedPlan — module-задача ДО loop:destiny и ПОСЛЕ:
// сквозные индексы продолжаются через границу destiny-loop (как через apply:destiny).
func TestRender_ApplyDestiny_Loop_MixedPlan(t *testing.T) {
	res := &stubDestinyResolver{resolved: loopDestiny()}
	scn := &config.ScenarioManifest{
		Name: "create",
		Tasks: []config.Task{
			{Name: "pre", Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo pre"}}},
			{Name: "apply", Apply: &config.ApplyTask{Destiny: "pilot-loop", Input: map[string]any{"changes": "${ input.changes }"}}},
			{Name: "post", Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo post"}}},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: scn,
		Input: map[string]any{"changes": []any{
			map[string]any{"acl": "~a"},
			map[string]any{"acl": "~b"},
			map[string]any{"acl": "~c"},
		}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// pre(0) + loop×3 (1,2,3) + post(4) = 5 задач, сквозные индексы.
	if len(tasks) != 5 {
		t.Fatalf("len(tasks) = %d, want 5 (pre + loop×3 + post)", len(tasks))
	}
	for i, rt := range tasks {
		if rt.Index != i {
			t.Errorf("tasks[%d].Index = %d, want %d (сквозные через destiny-loop)", i, rt.Index, i)
		}
	}
	if got := tasks[4].Params.GetFields()["cmd"].GetStringValue(); got != "echo post" {
		t.Errorf("post после destiny-loop отрендерена неверно: %q", got)
	}
}

// TestRender_ApplyDestiny_Loop_Isolation — РЕВЕРС-GUARD изоляции: loop:-items в
// destiny ссылается на soulprint.hosts (scenario-only roster, недоступный
// изолированному destiny) → ошибка изоляции (AllowHosts=false при
// destinyIsolated). Ловит протечку изоляции, если loopInvariantVars перестанет
// уважать destinyIsolated.
func TestRender_ApplyDestiny_Loop_Isolation(t *testing.T) {
	leaky := loopDestiny()
	leaky.Tasks[0].Loop.Items = `${ soulprint.hosts.where("role == 'master'") }`
	res := &stubDestinyResolver{resolved: leaky}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: applyScenario("pilot-loop", map[string]any{"changes": "${ input.changes }"}),
		Input: map[string]any{"changes": []any{
			map[string]any{"acl": "~a"},
		}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("Render: ожидалась ошибка изоляции — destiny-loop не должен видеть soulprint.hosts (AllowHosts=false)")
	}
}

// TestRender_ApplyDestiny_Loop_RegisterIsolation — РЕВЕРС-GUARD: loop:-items в
// destiny ссылается на register (scenario-scope, пуст в изолированном destiny) →
// no-such-key. Register destiny пуст до barrier (изоляция ADR-009); cross-scope
// register в loop.items не протекает.
func TestRender_ApplyDestiny_Loop_RegisterIsolation(t *testing.T) {
	leaky := loopDestiny()
	leaky.Tasks[0].Loop.Items = "${ register.probe.stdout_lines }"
	res := &stubDestinyResolver{resolved: leaky}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: applyScenario("pilot-loop", map[string]any{"changes": "${ input.changes }"}),
		Input: map[string]any{"changes": []any{
			map[string]any{"acl": "~a"},
		}},
		Incarnation: IncarnationMeta{Name: "svc"},
		// scenario-scope register есть, но в destiny-env его быть не должно.
		Register: map[string]any{"probe": map[string]any{"stdout_lines": []any{"x"}}},
		Hosts:    []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:  res,
	}
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("Render: ожидалась ошибка — destiny-loop не должен видеть scenario register (изоляция)")
	}
}

// TestRender_ApplyDestiny_Loop_OnChanges — onchanges на destiny-loop-задачу:
// сквозные Index целы и register-имя резолвится в Index при наличии loop-fan-out
// внутри destiny (наследуется через resolveOnChanges финальным проходом).
//
// Семантика register+loop (общая со scenario, registerIndex.go): N итераций
// несут ОДИН register → карта register-имя→Index оставляет последнюю итерацию.
// onchanges на loop-register резолвится в Index последней итерации цикла. Это
// существующее поведение renderLoopTask (не специфично для destiny); тест
// закрепляет, что destiny-loop его НАСЛЕДУЕТ без сюрпризов, а Index целы.
func TestRender_ApplyDestiny_Loop_OnChanges(t *testing.T) {
	d := loopDestiny()
	d.Tasks[0].Register = "acl_patch"
	d.Tasks = append(d.Tasks, config.Task{
		Name:      "Notify after patch",
		OnChanges: []string{"acl_patch"},
		Module:    &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo reloaded"}},
	})
	res := &stubDestinyResolver{resolved: d}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: applyScenario("pilot-loop", map[string]any{"changes": "${ input.changes }"}),
		Input: map[string]any{"changes": []any{
			map[string]any{"acl": "~a"},
			map[string]any{"acl": "~b"},
		}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// loop×2 (Index 0,1) + consumer (Index 2).
	if len(tasks) != 3 {
		t.Fatalf("len(tasks) = %d, want 3 (loop×2 + consumer)", len(tasks))
	}
	for i, rt := range tasks {
		if rt.Index != i {
			t.Errorf("tasks[%d].Index = %d, want %d", i, rt.Index, i)
		}
	}
	// onchanges register-имя acl_patch резолвится в Index ПОСЛЕДНЕЙ loop-итерации
	// (registerIndex: один register → последний Index; общая со scenario семантика).
	consumer := tasks[2]
	if len(consumer.OnChangesIdx) != 1 {
		t.Fatalf("OnChangesIdx = %v, want 1 (register loop-задачи → Index последней итерации)", consumer.OnChangesIdx)
	}
	if consumer.OnChangesIdx[0] != 1 {
		t.Errorf("OnChangesIdx = %v, want [1] (remap register-имени в Index последней loop-итерации)", consumer.OnChangesIdx)
	}
}

// TestRender_ApplyDestiny_Loop_StaticWhenSkip — destiny-loop-задача с when:,
// статически вычисляемым в false (input.action != ...), даёт N skip-placeholder
// (паритет scenario static-when+loop): задачи в плане остаются (Params==nil), но
// params НЕ рендерятся → битый/недоступный ${...} в params НЕ падает. Зеркало
// scenario static-when placeholder-skip, но для loop в destiny.
func TestRender_ApplyDestiny_Loop_StaticWhenSkip(t *testing.T) {
	d := &ResolvedDestiny{
		Name: "pilot-loop-when",
		Input: config.InputSchemaMap{
			"action":  {Type: "string", Required: true},
			"changes": {Type: "array", Required: true},
		},
		Tasks: []config.Task{
			{
				Name: "Apply ACL patch (gated by action)",
				When: "input.action == 'update_acls'", // static-false при action=create
				Loop: &config.LoopSpec{Items: "${ input.changes }", As: "change", IndexAs: "username"},
				Module: &config.ModuleTask{
					Module: "core.cmd.shell",
					// missing_var заведомо отсутствует — рендер params упал бы, если бы НЕ skip.
					Params: map[string]any{"cmd": "redis-cli ACL SETUSER ${ username } ${ change.missing_var }"},
				},
			},
		},
	}
	res := &stubDestinyResolver{resolved: d}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: applyScenario("pilot-loop-when", map[string]any{
			"action":  "${ input.action }",
			"changes": "${ input.changes }",
		}),
		Input: map[string]any{"action": "create", "changes": []any{
			map[string]any{"acl": "~a"},
			map[string]any{"acl": "~b"},
		}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v (static-when:false должен скипнуть params, а не падать на missing_var)", err)
	}
	// 2 итерации → 2 skip-placeholder, Params==nil (не рендерились).
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2 (placeholder за каждую loop-итерацию)", len(tasks))
	}
	for i, rt := range tasks {
		if rt.Params != nil {
			t.Errorf("tasks[%d].Params != nil — static-when:false должен скипнуть рендер params", i)
		}
		if rt.Index != i {
			t.Errorf("tasks[%d].Index = %d, want %d (сквозные даже при skip)", i, rt.Index, i)
		}
	}
}
