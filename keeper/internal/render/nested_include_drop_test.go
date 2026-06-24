package render

import (
	"context"
	"fmt"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// Каскадный дроп вложенного условного include (ADR-009 amendment, фикс «вложенный
// conditional-include не каскадит дроп»). Проверяется на ПРОД-пути
// config.ExpandIncludes → Render: эффективный include-when вложенной группы =
// конъюнкция предков `(outer) && (inner)`, поэтому ложный outer гасит вложенную
// группу при ЛЮБОМ inner. Тесты гоняют 2×2-матрицу (outer×inner) и ассертят
// ДЛИНУ плана: дропнутые задачи физически отсутствуют (не placeholder), что
// Trial-матчер task_absent на длине не различает.

// mapIncludeResolver — config.IncludeResolver поверх in-memory map имя→YAML
// (display-путь = имя файла). Зеркалит helper из shared/config-тестов, но живёт
// здесь: render-тест честно прогоняет прод-фазу ExpandIncludes своим резолвером.
func mapIncludeResolver(files map[string]string) config.IncludeResolver {
	return func(name string) ([]byte, string, error) {
		data, ok := files[name]
		if !ok {
			return nil, "", fmt.Errorf("файл %q не найден", name)
		}
		return []byte(data), name, nil
	}
}

// nestedIncludeFixture — main с условным outer-include, внутри outer — условный
// inner-include с inner-task. Это структура из репорта бага.
func nestedIncludeFixture() (rootSrc string, files map[string]string) {
	rootSrc = `
- include: outer.yml
  when: input.outer == 'yes'
`
	files = map[string]string{
		"outer.yml": `
- name: outer-direct
  module: core.cmd.shell
  params: { cmd: "outer-direct" }
- include: inner.yml
  when: input.inner == 'yes'
`,
		"inner.yml": "- name: inner-task\n  module: core.cmd.shell\n  params: { cmd: 'inner' }\n",
	}
	return rootSrc, files
}

// expandNestedFixture парсит main-задачи тем же task-парсером, что scenario-loader,
// прогоняет config.ExpandIncludes (прод-фаза) и возвращает плоский список —
// готовый для Render.
func expandNestedFixture(t *testing.T) []config.Task {
	t.Helper()
	rootSrc, files := nestedIncludeFixture()
	rootTasks, diags, _ := config.LoadDestinyTasksFromBytes("scenario/create/main.yml", []byte(rootSrc), config.ValidateOptions{})
	if diag.HasErrors(diags) {
		t.Fatalf("парс main: %v", diags)
	}
	expanded, idiags := config.ExpandIncludes(rootTasks, mapIncludeResolver(files))
	if diag.HasErrors(idiags) {
		t.Fatalf("ExpandIncludes: %v", idiags)
	}
	return expanded
}

// TestNestedConditionalInclude_DropMatrix — ★ матрица вложенного include 2×2 на
// прод-пути ExpandIncludes→Render. Ключевой кейс бага: outer='no', inner='yes' →
// inner-task ОБЯЗАН отсутствовать (каскадный дроп), хотя его собственный inner-when
// истинен. Ассерт — на ДЛИНЕ плана и составе имён (физическое отсутствие, не
// placeholder).
func TestNestedConditionalInclude_DropMatrix(t *testing.T) {
	expanded := expandNestedFixture(t)

	cases := []struct {
		outer, inner string
		wantNames    []string // ровно эти задачи в плане (порядок), len = матрица дропа
	}{
		// outer=no → пусто при ЛЮБОМ inner (каскад).
		{"no", "no", nil},
		{"no", "yes", nil},
		// outer=yes,inner=no → только outer-direct.
		{"yes", "no", []string{"outer-direct"}},
		// outer=yes,inner=yes → обе.
		{"yes", "yes", []string{"outer-direct", "inner-task"}},
	}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	for _, tc := range cases {
		t.Run(fmt.Sprintf("outer=%s,inner=%s", tc.outer, tc.inner), func(t *testing.T) {
			in := RenderInput{
				Scenario:    &config.ScenarioManifest{Name: "nested-cond", Tasks: expanded},
				Input:       map[string]any{"outer": tc.outer, "inner": tc.inner},
				Incarnation: IncarnationMeta{Name: "svc"},
				Hosts:       singleHost(),
			}
			tasks, plans, err := p.Render(context.Background(), in)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			// ★ len-ассерт: дропнутые задачи физически отсутствуют (не placeholder).
			if len(tasks) != len(tc.wantNames) {
				gotNames := make([]string, len(tasks))
				for i, rt := range tasks {
					gotNames[i] = rt.Name
				}
				t.Fatalf("len(tasks) = %d, want %d; got %v, want %v",
					len(tasks), len(tc.wantNames), gotNames, tc.wantNames)
			}
			if len(plans) != len(tc.wantNames) {
				t.Fatalf("len(plans) = %d, want %d (дроп не резервирует план)", len(plans), len(tc.wantNames))
			}
			for i, w := range tc.wantNames {
				if tasks[i].Name != w {
					t.Errorf("tasks[%d].Name = %q, want %q", i, tasks[i].Name, w)
				}
				if tasks[i].Index != i {
					t.Errorf("tasks[%d].Index = %d, want %d (сквозная нумерация без дыр)", i, tasks[i].Index, i)
				}
			}
			// Жёсткая гарантия отсутствия дропнутой inner-task в негативных кейсах.
			for _, rt := range tasks {
				if rt.Name == "inner-task" && tc.inner == "no" {
					t.Errorf("inner-task присутствует при inner=no — должна быть дропнута")
				}
				if rt.Name == "inner-task" && tc.outer == "no" {
					t.Errorf("inner-task присутствует при outer=no — каскадный дроп не сработал (исходный баг)")
				}
			}
		})
	}
}
