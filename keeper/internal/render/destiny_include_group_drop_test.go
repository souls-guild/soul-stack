package render

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// Conditional-include group-drop ВНУТРИ apply:destiny (ADR-009 amendment, паритет
// со scenario-путём). renderApplyDestiny зеркалит scenario-цикл (pipeline.go): задачи,
// раскрытые из include под статическим `when:`, несут carry-through
// Task.IncludeWhen/IncludeGroupID. include-when вычисляется ОДИН раз на группу в
// ИЗОЛИРОВАННОМ destiny-env (input = резолвнутый apply.input + schema-defaults, НЕ
// scenario-scope) и при false дропает ВСЕ задачи группы РЕАЛЬНО — без эмита
// RenderedTask и без idx++. Кеш includeGroupKeep — per-проход, раздельный от scenario.
//
// Тесты строят carry-through-поля напрямую (через includeGroup из
// include_group_drop_test.go) — фокус на render-инварианте group-drop в destiny.

// applyDestinyScenario оборачивает destiny с conditional-include в scenario с одной
// apply:destiny-задачей, прокидывая applyInput в destiny-input.
func applyDestinyScenario(destiny string, applyInput map[string]any) *config.ScenarioManifest {
	return &config.ScenarioManifest{
		Name: "create",
		Tasks: []config.Task{
			{Name: "Apply destiny", Apply: &config.ApplyTask{Destiny: destiny, Input: applyInput}},
		},
	}
}

// TestDestinyIncludeGroupDrop_WhenFalse_TasksAbsent — ★ дроп условной include-группы
// внутри destiny при non-matching input: задачи группы физически ОТСУТСТВУЮТ в плане
// (реальный дроп), индексы хвоста сквозные без дыр. include-when ссылается на
// input.topology — резолвится против destiny-input (apply.input), НЕ против scenario-
// input, и НЕ падает no-such-key (input.topology передан в apply.input).
func TestDestinyIncludeGroupDrop_WhenFalse_TasksAbsent(t *testing.T) {
	var dtasks []config.Task
	dtasks = append(dtasks, cmdTask("head", "head"))
	dtasks = append(dtasks, includeGroup("input.topology == 'sentinel'", 1,
		cmdTask("sentinel-a", "sa"),
		cmdTask("sentinel-b", "sb"),
	)...)
	dtasks = append(dtasks, cmdTask("tail", "tail"))

	res := &stubDestinyResolver{resolved: &ResolvedDestiny{
		Name:  "cond-destiny",
		Input: config.InputSchemaMap{"topology": {Type: "string", Required: true}},
		Tasks: dtasks,
	}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyDestinyScenario("cond-destiny", map[string]any{"topology": "standalone"}),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       singleHost(),
		Destiny:     res,
	}

	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: дроп sentinel-группы при non-sentinel input НЕ должен падать (no-such-key недопустим — input.topology в apply.input): %v", err)
	}
	if len(tasks) != 2 || len(plans) != 2 {
		t.Fatalf("len(tasks)=%d plans=%d, want 2/2 (head + tail; группа дропнута БЕЗ placeholder)", len(tasks), len(plans))
	}
	for _, rt := range tasks {
		if rt.Name == "sentinel-a" || rt.Name == "sentinel-b" {
			t.Fatalf("задача %q дропнутой sentinel-группы присутствует в плане destiny", rt.Name)
		}
	}
	if tasks[0].Name != "head" || tasks[1].Name != "tail" {
		t.Errorf("план = [%q,%q], want [head,tail]", tasks[0].Name, tasks[1].Name)
	}
	if tasks[0].Index != 0 || tasks[1].Index != 1 {
		t.Errorf("Index = %d,%d, want 0,1 (сквозные без дыр — дроп не резервирует idx)", tasks[0].Index, tasks[1].Index)
	}
}

// TestDestinyIncludeGroupDrop_WhenTrue_TasksPresent — keep при matching input: задачи
// группы присутствуют и рендерятся обычным путём (carry-through-поля на рендер не
// влияют), индексы сквозные.
func TestDestinyIncludeGroupDrop_WhenTrue_TasksPresent(t *testing.T) {
	var dtasks []config.Task
	dtasks = append(dtasks, cmdTask("head", "head"))
	dtasks = append(dtasks, includeGroup("input.topology == 'sentinel'", 1,
		cmdTask("sentinel-a", "sa"),
		cmdTask("sentinel-b", "sb"),
	)...)
	dtasks = append(dtasks, cmdTask("tail", "tail"))

	res := &stubDestinyResolver{resolved: &ResolvedDestiny{
		Name:  "cond-destiny",
		Input: config.InputSchemaMap{"topology": {Type: "string", Required: true}},
		Tasks: dtasks,
	}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyDestinyScenario("cond-destiny", map[string]any{"topology": "sentinel"}),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       singleHost(),
		Destiny:     res,
	}

	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 4 {
		t.Fatalf("len(tasks)=%d, want 4 (head + 2 группы + tail)", len(tasks))
	}
	want := []string{"head", "sentinel-a", "sentinel-b", "tail"}
	for i, w := range want {
		if tasks[i].Name != w {
			t.Errorf("tasks[%d].Name = %q, want %q", i, tasks[i].Name, w)
		}
		if tasks[i].Index != i {
			t.Errorf("tasks[%d].Index = %d, want %d (сквозная нумерация)", i, tasks[i].Index, i)
		}
	}
	if tasks[1].Params == nil {
		t.Error("sentinel-a.Params == nil — include-when:true должен рендерить группу обычным путём")
	}
	if got := tasks[1].Params.GetFields()["cmd"].GetStringValue(); got != "sa" {
		t.Errorf("sentinel-a.cmd = %q, want sa", got)
	}
}

// TestDestinyIncludeGroupDrop_Nested — ★ паритет со scenario: nested conditional-include
// в destiny. Внешняя группа keep, внутренняя (другой group-id) drop — внешние задачи
// остаются, внутренние исчезают. include-when каждой группы вычисляется в изолированном
// destiny-env; кеши групп раздельны по IncludeGroupID.
func TestDestinyIncludeGroupDrop_Nested(t *testing.T) {
	var dtasks []config.Task
	dtasks = append(dtasks, cmdTask("head", "head"))
	// Внешняя группа (id=1) — keep при tls=='on'.
	dtasks = append(dtasks, includeGroup("input.tls == 'on'", 1,
		cmdTask("outer-a", "oa"),
	)...)
	// Вложенная группа (id=2) — drop при ha=='off' (другой group-id, своё условие).
	dtasks = append(dtasks, includeGroup("input.ha == 'on'", 2,
		cmdTask("inner-a", "ia"),
		cmdTask("inner-b", "ib"),
	)...)
	dtasks = append(dtasks, cmdTask("tail", "tail"))

	res := &stubDestinyResolver{resolved: &ResolvedDestiny{
		Name: "nested-destiny",
		Input: config.InputSchemaMap{
			"tls": {Type: "string", Required: true},
			"ha":  {Type: "string", Required: true},
		},
		Tasks: dtasks,
	}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyDestinyScenario("nested-destiny", map[string]any{"tls": "on", "ha": "off"}),
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       singleHost(),
		Destiny:     res,
	}

	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// head + outer-a(keep) + tail = 3; inner-группа (ha=off) дропнута.
	if len(tasks) != 3 {
		t.Fatalf("len(tasks)=%d, want 3 (head + outer-a + tail; inner-группа дропнута)", len(tasks))
	}
	names := map[string]*RenderedTask{}
	for _, rt := range tasks {
		names[rt.Name] = rt
	}
	if _, ok := names["inner-a"]; ok {
		t.Error("inner-a присутствует — вложенная include-группа (ha=off) должна быть дропнута")
	}
	if _, ok := names["inner-b"]; ok {
		t.Error("inner-b присутствует — вложенная include-группа (ha=off) должна быть дропнута")
	}
	if _, ok := names["outer-a"]; !ok {
		t.Error("outer-a отсутствует — внешняя include-группа (tls=on) должна остаться")
	}
	if tail := names["tail"]; tail == nil || tail.Index != 2 {
		t.Errorf("tail.Index = %v, want 2 (сквозной после head=0, outer-a=1; inner-drop не резервирует idx)", tail)
	}
}

// TestDestinyIncludeGroupDrop_IsolatedEnv — ★ include-when резолвится против
// ИЗОЛИРОВАННОГО destiny-input (apply.input + defaults), НЕ против scenario-scope.
// scenario-input несёт topology=='sentinel', но в apply.input передан standalone →
// группа ДОЛЖНА дропнуться (если бы include-when смотрел parentIn, она бы осталась).
func TestDestinyIncludeGroupDrop_IsolatedEnv(t *testing.T) {
	var dtasks []config.Task
	dtasks = append(dtasks, includeGroup("input.topology == 'sentinel'", 1,
		cmdTask("sentinel-only", "so"),
	)...)

	res := &stubDestinyResolver{resolved: &ResolvedDestiny{
		Name:  "iso-destiny",
		Input: config.InputSchemaMap{"topology": {Type: "string", Required: true}},
		Tasks: dtasks,
	}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		// apply.input.topology = literal standalone (НЕ из scenario-input).
		Scenario: applyDestinyScenario("iso-destiny", map[string]any{"topology": "standalone"}),
		// scenario-scope несёт topology=sentinel — destiny НЕ должна его увидеть.
		Input:       map[string]any{"topology": "sentinel"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       singleHost(),
		Destiny:     res,
	}

	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("len(tasks)=%d, want 0 — include-when должен читать destiny-input (standalone → дроп), НЕ scenario-input (sentinel)", len(tasks))
	}
}
