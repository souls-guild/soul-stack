package render

import (
	"context"
	"fmt"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// Cascading drop of a nested conditional include (ADR-009 amendment, fix for
// "nested conditional-include doesn't cascade the drop"). Verified on the PROD
// path config.ExpandIncludes → Render: the effective include-when of a nested
// group is the conjunction of ancestors `(outer) && (inner)`, so a false outer
// suppresses the nested group regardless of inner. Tests run a 2×2 matrix
// (outer×inner) and assert plan LENGTH: dropped tasks are physically absent
// (not a placeholder), which the Trial task_absent matcher can't distinguish
// by length alone.

// mapIncludeResolver — a config.IncludeResolver over an in-memory name→YAML map
// (display path = file name). Mirrors a helper from the shared/config tests but
// lives here: the render test genuinely runs the prod ExpandIncludes phase with
// its own resolver.
func mapIncludeResolver(files map[string]string) config.IncludeResolver {
	return func(name string) ([]byte, string, error) {
		data, ok := files[name]
		if !ok {
			return nil, "", fmt.Errorf("файл %q не найден", name)
		}
		return []byte(data), name, nil
	}
}

// nestedIncludeFixture — a main with a conditional outer-include, and inside
// outer a conditional inner-include with an inner-task. Mirrors the structure
// from the bug report.
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

// expandNestedFixture parses the main tasks with the same task parser as the
// scenario loader, runs config.ExpandIncludes (the prod phase), and returns a
// flat list ready for Render.
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

// TestNestedConditionalInclude_DropMatrix — ★ a 2×2 nested-include matrix on
// the prod ExpandIncludes→Render path. Key bug case: outer='no', inner='yes' →
// inner-task MUST be absent (cascading drop), even though its own inner-when
// is true. Asserts plan LENGTH and the set of names (physical absence, not a
// placeholder).
func TestNestedConditionalInclude_DropMatrix(t *testing.T) {
	expanded := expandNestedFixture(t)

	cases := []struct {
		outer, inner string
		wantNames    []string // exactly these tasks in the plan (order), len = drop matrix
	}{
		// outer=no → empty regardless of inner (cascade).
		{"no", "no", nil},
		{"no", "yes", nil},
		// outer=yes,inner=no → only outer-direct.
		{"yes", "no", []string{"outer-direct"}},
		// outer=yes,inner=yes → both.
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
			// ★ len assertion: dropped tasks are physically absent (not a placeholder).
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
			// Hard guarantee the dropped inner-task is absent in the negative cases.
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
