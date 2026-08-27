package config

import (
	"fmt"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// Within-block include (NIM-169): `include:` among a block's children is
// spliced in place, at any nesting depth, sharing the cycle/depth detection and
// the conditional-include cascade of the top-level expansion.

// taskNames lists tasks by their `name:` (a block task is reported as
// "<name>{...}" with its children inlined) — asserts splice ORDER, which module
// ids can't (test fixtures reuse core.cmd.shell).
func taskNames(tasks []Task) []string {
	out := make([]string, 0, len(tasks))
	for i := range tasks {
		t := &tasks[i]
		if t.Block != nil {
			out = append(out, fmt.Sprintf("%s{%s}", t.Name, strings.Join(taskNames(t.Block.Block), ",")))
			continue
		}
		out = append(out, t.Name)
	}
	return out
}

// blockChildren returns a block task's children, failing the test if the task
// is not a block (a passthrough regression would otherwise read as an empty diff).
func blockChildren(t *testing.T, task Task) []Task {
	t.Helper()
	if task.Block == nil {
		t.Fatalf("task %q is not a block task (expansion dropped the block?)", task.Name)
	}
	return task.Block.Block
}

// TestExpandIncludes_WithinBlock — the target case of NIM-169: an include child
// of a block is spliced flat among the block's siblings, in place, keeping order.
// The block node itself survives expansion (its when:/requisites are merged into
// every child at render, so the included tasks inherit them too).
func TestExpandIncludes_WithinBlock(t *testing.T) {
	root := []Task{
		{
			Name: "grp",
			When: "input.action == 'apply'",
			Block: &BlockTask{Block: []Task{
				{Include: &IncludeTask{Include: "acl.yml"}},
				{Name: "restart", Module: &ModuleTask{Module: "core.cmd.shell", Params: map[string]any{"cmd": "restart"}}},
			}},
		},
	}
	files := map[string]string{
		"acl.yml": `
- name: acl-a
  module: core.cmd.shell
  params: { cmd: "a" }
- name: acl-b
  module: core.cmd.shell
  params: { cmd: "b" }
`,
	}
	got, diags := ExpandIncludes(root, mapResolver(files))
	if diag.HasErrors(diags) {
		t.Fatalf("within-block include should expand cleanly, diagnostics: %v", diags)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1 (the block stays one top-level task)", len(got))
	}
	if got[0].When != "input.action == 'apply'" {
		t.Errorf("block.When = %q, want the source when preserved", got[0].When)
	}
	children := blockChildren(t, got[0])
	want := []string{"acl-a", "acl-b", "restart"}
	if names := taskNames(children); strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("block children = %v, want %v (include spliced in place)", names, want)
	}
	for i := range children {
		if children[i].Include != nil {
			t.Fatalf("child[%d] is still an include task — the expander skipped the block", i)
		}
	}
}

// TestExpandIncludes_WithinBlockNested — nesting does not terminate the
// recursion early: block → include → block → include → leaf. Both include levels
// expand, both block levels survive.
func TestExpandIncludes_WithinBlockNested(t *testing.T) {
	root := []Task{
		{Name: "outer", Block: &BlockTask{Block: []Task{
			{Include: &IncludeTask{Include: "mid.yml"}},
		}}},
	}
	files := map[string]string{
		"mid.yml": `
- name: inner
  block:
    - include: leaf.yml
`,
		"leaf.yml": "- name: leaf\n  module: core.cmd.shell\n  params: { cmd: 'true' }\n",
	}
	got, diags := ExpandIncludes(root, mapResolver(files))
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	want := []string{"outer{inner{leaf}}"}
	if names := taskNames(got); strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("tasks = %v, want %v (both nesting levels expanded)", names, want)
	}
}

// TestExpandIncludes_WithinBlockCycle — a cycle through a block is reported as
// include_cycle, not a stack overflow: the visited-stack is threaded into the
// block recursion unchanged, so self.yml re-entering itself via its own block is
// caught on the second visit.
func TestExpandIncludes_WithinBlockCycle(t *testing.T) {
	root := []Task{{Name: "grp", Block: &BlockTask{Block: []Task{
		{Include: &IncludeTask{Include: "self.yml"}},
	}}}}
	files := map[string]string{
		"self.yml": `
- name: grp-inner
  block:
    - include: self.yml
`,
	}
	_, diags := ExpandIncludes(root, mapResolver(files))
	if !hasCode(diags, "include_cycle") {
		t.Fatalf("expected include_cycle for a cycle through a block, diagnostics: %v", diags)
	}
}

// TestExpandIncludes_WithinBlockDepthExceeded — the depth ceiling applies inside
// a block too: an acyclic chain where every level wraps its include in a block
// still hits include_depth_exceeded (the stack grows through block recursion).
func TestExpandIncludes_WithinBlockDepthExceeded(t *testing.T) {
	leafLevel := maxIncludeDepth + 1
	resolve := func(name string) ([]byte, string, error) {
		var n int
		if _, err := fmt.Sscanf(name, "level-%d.yml", &n); err != nil {
			return nil, "", fmt.Errorf("unexpected name %q", name)
		}
		if n >= leafLevel {
			return []byte("- name: leaf\n  module: core.cmd.shell\n  params: { cmd: 'true' }\n"), name, nil
		}
		body := fmt.Sprintf("- name: grp-%d\n  block:\n    - include: level-%d.yml\n", n, n+1)
		return []byte(body), name, nil
	}
	root := []Task{{Include: &IncludeTask{Include: "level-0.yml"}}}
	_, diags := ExpandIncludes(root, resolve)
	if !hasCode(diags, "include_depth_exceeded") {
		t.Fatalf("expected include_depth_exceeded for a block-wrapped chain of depth %d, diagnostics: %v", leafLevel, diags)
	}
}

// TestExpandIncludes_WithinBlockConditional — a conditional include INSIDE a
// block stamps its group onto the spliced children only; the block's own
// hand-written children stay unconditional. The two axes (include group-drop and
// block when: inheritance) are independent, so a child carries both.
func TestExpandIncludes_WithinBlockConditional(t *testing.T) {
	root := []Task{
		{
			Name: "grp",
			When: "input.action == 'apply'",
			Block: &BlockTask{Block: []Task{
				{Name: "plain", Module: &ModuleTask{Module: "core.cmd.shell", Params: map[string]any{"cmd": "plain"}}},
				{Include: &IncludeTask{Include: "cluster.yml"}, When: "input.topology == 'cluster'"},
			}},
		},
	}
	files := map[string]string{
		"cluster.yml": "- name: shard\n  module: core.cmd.shell\n  params: { cmd: 'shard' }\n",
	}
	got, diags := ExpandIncludes(root, mapResolver(files))
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	children := blockChildren(t, got[0])
	if len(children) != 2 {
		t.Fatalf("block children = %v, want 2 (plain + spliced shard)", taskNames(children))
	}
	if children[0].IncludeGroupID != 0 || children[0].IncludeWhen != "" {
		t.Errorf("hand-written child: IncludeGroupID=%d IncludeWhen=%q, want 0/\"\" (outside the include group)",
			children[0].IncludeGroupID, children[0].IncludeWhen)
	}
	if children[1].IncludeGroupID == 0 {
		t.Errorf("spliced child: IncludeGroupID = 0, want != 0 (conditional include inside a block)")
	}
	if children[1].IncludeWhen != "input.topology == 'cluster'" {
		t.Errorf("spliced child: IncludeWhen = %q, want the include-when threaded through", children[1].IncludeWhen)
	}
	// The block's own when: is NOT folded into the include-when — the axes stay
	// separate (render ANDs block.when into every child; group-drop is evaluated
	// on its own).
	if strings.Contains(children[1].IncludeWhen, "input.action") {
		t.Errorf("spliced child: IncludeWhen = %q — block.when leaked into the include group axis", children[1].IncludeWhen)
	}
}

// TestExpandIncludes_WithinBlockConditionalCascade — a conditional include whose
// file contains a block with ANOTHER conditional include: the inner group's
// effective include-when is the ancestor conjunction, so dropping the outer group
// cascades through the block boundary.
func TestExpandIncludes_WithinBlockConditionalCascade(t *testing.T) {
	root := []Task{{Include: &IncludeTask{Include: "outer.yml"}, When: "input.x"}}
	files := map[string]string{
		"outer.yml": `
- name: grp
  block:
    - include: inner.yml
      when: input.y
`,
		"inner.yml": "- name: inner\n  module: core.cmd.shell\n  params: { cmd: 'inner' }\n",
	}
	got, diags := ExpandIncludes(root, mapResolver(files))
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if got[0].IncludeWhen != "input.x" {
		t.Errorf("block node: IncludeWhen = %q, want input.x (outer group)", got[0].IncludeWhen)
	}
	children := blockChildren(t, got[0])
	if len(children) != 1 {
		t.Fatalf("block children = %v, want 1", taskNames(children))
	}
	if children[0].IncludeWhen != "(input.x) && (input.y)" {
		t.Errorf("inner child: IncludeWhen = %q, want \"(input.x) && (input.y)\" (cascading drop through the block)", children[0].IncludeWhen)
	}
	if children[0].IncludeGroupID == got[0].IncludeGroupID {
		t.Errorf("inner child shares group-id %d with the block node — the nested drop would follow the outer when", children[0].IncludeGroupID)
	}
}

// TestExpandIncludes_WithinBlockDuplicateAddress — register/id uniqueness spans
// the block boundary: a register brought in by a within-block include that
// collides with a top-level one is caught (collectFlatAddresses is already
// recursive; this pins that the newly-expanded children reach it).
func TestExpandIncludes_WithinBlockDuplicateAddress(t *testing.T) {
	root := []Task{
		{Name: "top", Register: "probe", Module: &ModuleTask{Module: "core.cmd.shell", Params: map[string]any{"cmd": "top"}}},
		{Name: "grp", Block: &BlockTask{Block: []Task{
			{Include: &IncludeTask{Include: "dup.yml"}},
		}}},
	}
	files := map[string]string{
		"dup.yml": "- name: dup\n  register: probe\n  module: core.cmd.shell\n  params: { cmd: 'dup' }\n",
	}
	_, diags := ExpandIncludes(root, mapResolver(files))
	if !hasCode(diags, "duplicate_task_address") {
		t.Fatalf("expected duplicate_task_address for a register colliding across the block boundary, diagnostics: %v", diags)
	}
}

// TestExpandIncludes_WithinBlockKeepsSourceIntact — expansion is not allowed to
// mutate the caller's manifest: the source block still carries its include child
// after ExpandIncludes returns. The loader keeps the parsed manifest around
// (Trial re-renders it per case), so an in-place splice would leak across runs.
func TestExpandIncludes_WithinBlockKeepsSourceIntact(t *testing.T) {
	inner := []Task{{Include: &IncludeTask{Include: "leaf.yml"}}}
	root := []Task{{Name: "grp", Block: &BlockTask{Block: inner}}}
	files := map[string]string{
		"leaf.yml": "- name: leaf\n  module: core.cmd.shell\n  params: { cmd: 'true' }\n",
	}
	if _, diags := ExpandIncludes(root, mapResolver(files)); diag.HasErrors(diags) {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if len(root[0].Block.Block) != 1 || root[0].Block.Block[0].Include == nil {
		t.Fatalf("source block was mutated by expansion: %v", taskNames(root))
	}
}

// TestExpandIncludes_BlockOnKeeperInsideAnIncludedFile — `block:` + `on: keeper`
// (NIM-652) is refused in an INCLUDED file too, and reported at that file's own
// coordinates.
//
// This is the layer guard, not a second copy of
// TestLoadScenarioManifest_BlockOnKeeper. The combination is refused three
// times: soul-lint offline, at config parse, and fail-closed at render
// (ErrUnsupportedDSL, keeper/internal/render). Only the first two are offline,
// and both come from validateTaskNode — which the bare-sequence loader runs over
// an included body (LoadDestinyTasksFromBytes). Move the check up into a
// per-task rule over a manifest's own `tasks:` and it goes silent here, because
// at that level `tasks:` holds the `- include:` node and not the tasks it
// splices in: the rule would then cover the shape the fixtures are written in
// and miss the shape a service is written in (a thin main.yml over a body of
// included files). The panic it stands in front of is reachable from either
// shape.
func TestExpandIncludes_BlockOnKeeperInsideAnIncludedFile(t *testing.T) {
	cases := []struct {
		name     string
		yamlPath string
		body     string
	}{
		{
			name:     "on the block",
			yamlPath: "$[0].block",
			body: `
- name: record the topology
  on: keeper
  block:
    - name: probe the node
      module: core.exec.run
      params: { cmd: "true" }
`,
		},
		{
			name:     "on a block child",
			yamlPath: "$[0].block[0].on",
			body: `
- name: record the topology
  block:
    - name: the field
      on: keeper
      module: core.state.set
      params: { field: mode, value: sentinel }
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := []Task{{Include: &IncludeTask{Include: "capture.yml"}}}
			_, diags := ExpandIncludes(root, mapResolver(map[string]string{"capture.yml": tc.body}))
			if !hasCode(diags, "block_on_keeper_invalid") {
				dump(t, diags)
				t.Fatalf("expected block_on_keeper_invalid for a keeper task in a block arriving via include:")
			}
			for _, d := range diags {
				if d.Code != "block_on_keeper_invalid" {
					continue
				}
				if d.File != "capture.yml" {
					t.Errorf("File = %q, want the included file the author has to edit", d.File)
				}
				if d.Line == 0 {
					t.Errorf("Line = 0, want the offending line inside the included file")
				}
				// Both levels raise the same code; the path is what separates
				// them, and it must be rebased onto the included file's own
				// bare-sequence root, not onto the parent's `$.tasks[...]`.
				if d.YAMLPath != tc.yamlPath {
					t.Errorf("YAMLPath = %q, want %q", d.YAMLPath, tc.yamlPath)
				}
			}
		})
	}
}
