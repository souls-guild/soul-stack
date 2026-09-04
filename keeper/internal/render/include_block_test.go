package render

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// Within-block include (NIM-169) end-to-end: the tests run the PRODUCTION
// expander (config.ExpandIncludes — the entry point of every loader: destiny
// artifact, scenario runner, trial) and then render, in BOTH layers — scenario
// (renderBlockTask) and destiny (renderDestinyBlock). ADR-009 gives scenario the
// full destiny DSL, so the two layers must agree.
//
// Covered per layer:
//
//	(a) an include child of a block splices into the block's children and
//	    inherits the block's when: like a hand-written sibling;
//	(b) a CONDITIONAL include inside a block composes with the block's when: on
//	    two independent axes — group-drop (physical removal, no index) is decided
//	    BEFORE static-when skip (placeholder with index).

// expandForRender runs ExpandIncludes over an in-memory file map and fails on
// any error diagnostic — these tests are about what reaches render, not about
// expansion diagnostics (those live in shared/config).
func expandForRender(t *testing.T, tasks []config.Task, files map[string]string) []config.Task {
	t.Helper()
	out, diags := config.ExpandIncludes(tasks, func(name string) ([]byte, string, error) {
		data, ok := files[name]
		if !ok {
			return nil, "", fmt.Errorf("file %q not found", name)
		}
		return []byte(data), name, nil
	})
	if diag.HasErrors(diags) {
		t.Fatalf("ExpandIncludes: %v", diags)
	}
	return out
}

// renderedShape describes the plan as "<name>" for a dispatched task and
// "<name>:skip" for a static-when placeholder — the exact distinction between
// group-drop (absent entirely) and block-when skip (present, not dispatched).
func renderedShape(tasks []*RenderedTask) []string {
	out := make([]string, 0, len(tasks))
	for _, rt := range tasks {
		if rt.Params == nil && rt.FlowContext != nil {
			out = append(out, rt.Name+":skip")
			continue
		}
		out = append(out, rt.Name)
	}
	return out
}

func assertShape(t *testing.T, tasks []*RenderedTask, want []string) {
	t.Helper()
	got := renderedShape(tasks)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("plan = %v, want %v", got, want)
	}
}

// aclFiles — one included file with two tasks, shared by the layer tests.
var aclFiles = map[string]string{
	"acl.yml": `
- name: acl-a
  module: core.cmd.shell
  params: { cmd: "a" }
- name: acl-b
  module: core.cmd.shell
  params: { cmd: "b" }
`,
	"cluster.yml": "- name: shard\n  module: core.cmd.shell\n  params: { cmd: 'shard' }\n",
}

// --- scenario layer ---

func renderScenarioTasks(t *testing.T, tasks []config.Task, input map[string]any) ([]*RenderedTask, []DispatchPlan, error) {
	t.Helper()
	p := NewPipeline(nil, newEngine(t), nil, nil)
	return p.Render(context.Background(), RenderInput{
		Scenario:    &config.ScenarioManifest{Name: "s", Tasks: tasks},
		Input:       input,
		Incarnation: IncarnationMeta{ID: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	})
}

// (a scenario) an include child of a block renders as an ordinary block child
// and inherits the block's when: — the included tasks are indistinguishable from
// hand-written siblings once expansion has run. when: here is register-dependent
// (not static), so it propagates to the child instead of being gated.
func TestRenderBlock_WithinBlockInclude(t *testing.T) {
	raw := []config.Task{{
		Name: "grp",
		When: "register.cfg.changed",
		Block: &config.BlockTask{Block: []config.Task{
			{Include: &config.IncludeTask{Include: "acl.yml"}},
			moduleTask("restart", "core.service.restarted"),
		}},
	}}
	tasks, _, err := renderScenarioTasks(t, expandForRender(t, raw, aclFiles), nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	assertShape(t, tasks, []string{"acl-a", "acl-b", "restart"})
	for i, rt := range tasks {
		if rt.When != "register.cfg.changed" {
			t.Errorf("tasks[%d] (%s).When = %q, want the block's when inherited", i, rt.Name, rt.When)
		}
		if rt.Index != i {
			t.Errorf("tasks[%d].Index = %d, want %d (contiguous indexes across the splice)", i, rt.Index, i)
		}
	}
}

// (b scenario) a conditional include INSIDE a block: group-drop and the block's
// static when: are independent axes and compose. group-drop wins the ordering —
// a dropped group leaves NO placeholder and reserves no index, while a
// block-when skip does.
func TestRenderBlock_WithinBlockConditionalInclude(t *testing.T) {
	raw := []config.Task{{
		Name: "grp",
		When: "input.action == 'apply'",
		Block: &config.BlockTask{Block: []config.Task{
			moduleTask("plain", "core.cmd.shell"),
			{Include: &config.IncludeTask{Include: "cluster.yml"}, When: "input.topology == 'cluster'"},
		}},
	}}
	expanded := expandForRender(t, raw, aclFiles)

	cases := []struct {
		name  string
		input map[string]any
		want  []string
	}{
		{"both-true", map[string]any{"action": "apply", "topology": "cluster"}, []string{"plain", "shard"}},
		{"include-false", map[string]any{"action": "apply", "topology": "standalone"}, []string{"plain"}},
		{"block-false", map[string]any{"action": "diagnose", "topology": "cluster"}, []string{"plain:skip", "shard:skip"}},
		{"both-false", map[string]any{"action": "diagnose", "topology": "standalone"}, []string{"plain:skip"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tasks, _, err := renderScenarioTasks(t, expanded, tc.input)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			assertShape(t, tasks, tc.want)
		})
	}
}

// --- destiny layer ---

// gatedDestiny — a destiny whose input carries the two switches the block tests
// need (the block's when: and the conditional include's when:).
func gatedDestiny(name string, tasks ...config.Task) *ResolvedDestiny {
	return &ResolvedDestiny{
		Name: name,
		Input: config.InputSchemaMap{
			"action":   {Type: "string", Default: "apply"},
			"topology": {Type: "string", Default: "standalone"},
		},
		Tasks: tasks,
	}
}

// (a destiny) mirror of the scenario case: an include child of a destiny block
// splices and inherits the block's when:.
func TestRenderDestinyBlock_WithinBlockInclude(t *testing.T) {
	raw := []config.Task{{
		Name: "grp",
		When: "register.cfg.changed",
		Block: &config.BlockTask{Block: []config.Task{
			{Include: &config.IncludeTask{Include: "acl.yml"}},
			moduleTask("restart", "core.service.restarted"),
		}},
	}}
	d := gatedDestiny("with-include", expandForRender(t, raw, aclFiles)...)

	tasks, _, err := renderBlockDestiny(t, d, map[string]any{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	assertShape(t, tasks, []string{"acl-a", "acl-b", "restart"})
	for i, rt := range tasks {
		if rt.When != "register.cfg.changed" {
			t.Errorf("tasks[%d] (%s).When = %q, want the block's when inherited", i, rt.Name, rt.When)
		}
	}
}

// (b destiny) group-drop of a conditional include inside a destiny block is
// evaluated in the ISOLATED destiny env (apply.input + schema defaults), and
// composes with the block's when: exactly as in scenario.
func TestRenderDestinyBlock_WithinBlockConditionalInclude(t *testing.T) {
	raw := []config.Task{{
		Name: "grp",
		When: "input.action == 'apply'",
		Block: &config.BlockTask{Block: []config.Task{
			moduleTask("plain", "core.cmd.shell"),
			{Include: &config.IncludeTask{Include: "cluster.yml"}, When: "input.topology == 'cluster'"},
		}},
	}}
	expanded := expandForRender(t, raw, aclFiles)

	cases := []struct {
		name  string
		input map[string]any
		want  []string
	}{
		{"both-true", map[string]any{"action": "apply", "topology": "cluster"}, []string{"plain", "shard"}},
		{"include-false", map[string]any{"action": "apply", "topology": "standalone"}, []string{"plain"}},
		{"block-false", map[string]any{"action": "diagnose", "topology": "cluster"}, []string{"plain:skip", "shard:skip"}},
		{"both-false", map[string]any{"action": "diagnose", "topology": "standalone"}, []string{"plain:skip"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := gatedDestiny("gated-include", expanded...)
			tasks, _, err := renderBlockDestiny(t, d, tc.input)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			assertShape(t, tasks, tc.want)
		})
	}
}

// TestWithinBlockInclude_NestedBlocks — block → include → block → include over
// the real expander and render: the whole chain flattens into contiguous
// dispatched tasks, with the outer block's when: cascading to the deepest leaf.
func TestWithinBlockInclude_NestedBlocks(t *testing.T) {
	raw := []config.Task{{
		Name: "outer",
		When: "register.cfg.changed",
		Block: &config.BlockTask{Block: []config.Task{
			{Include: &config.IncludeTask{Include: "mid.yml"}},
		}},
	}}
	files := map[string]string{
		// probe declares register `mid` in this file's own scope — a when: over a
		// register undeclared in the SAME file is rejected per-file
		// (unknown_register_reference), independently of this ticket.
		"mid.yml": `
- name: probe
  register: mid
  module: core.cmd.shell
  params: { cmd: "probe" }
- name: inner
  when: register.mid.changed
  block:
    - include: leaf.yml
`,
		"leaf.yml": "- name: leaf\n  module: core.cmd.shell\n  params: { cmd: 'true' }\n",
	}
	tasks, _, err := renderScenarioTasks(t, expandForRender(t, raw, files), nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	assertShape(t, tasks, []string{"probe", "leaf"})
	want := "(register.cfg.changed) && (register.mid.changed)"
	if tasks[1].When != want {
		t.Errorf("leaf.When = %q, want %q (cascading block inheritance through the include)", tasks[1].When, want)
	}
}
