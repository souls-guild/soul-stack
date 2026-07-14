package config

import (
	"fmt"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// mapResolver is an IncludeResolver over an in-memory name→content map. The
// display path = file name (enough for tests: different names = different sources).
func mapResolver(files map[string]string) IncludeResolver {
	return func(name string) ([]byte, string, error) {
		data, ok := files[name]
		if !ok {
			return nil, "", fmt.Errorf("файл %q не найден", name)
		}
		return []byte(data), name, nil
	}
}

func moduleNames(tasks []Task) []string {
	out := make([]string, 0, len(tasks))
	for _, t := range tasks {
		if t.Module != nil {
			out = append(out, t.Module.Module)
		} else {
			out = append(out, "<non-module>")
		}
	}
	return out
}

func TestExpandIncludes_FlatSplice(t *testing.T) {
	root := []Task{
		{Module: &ModuleTask{Module: "core.pkg.installed", Params: map[string]any{"name": "x"}}},
		{Include: &IncludeTask{Include: "sub.yml"}},
		{Module: &ModuleTask{Module: "core.service.running", Params: map[string]any{"name": "x"}}},
	}
	files := map[string]string{
		// Leaf modules carry valid required params: after the H2 rollout all core
		// modules are in the coremanifest registry and ExpandIncludes runs param
		// validation. This test checks the include splice; params are minimal but valid.
		"sub.yml": `
- name: a
  module: core.git.cloned
  params: { repo: "https://x", path: "/y" }
- name: b
  module: core.cmd.shell
  params: { cmd: "true" }
`,
	}
	got, diags := ExpandIncludes(root, mapResolver(files))
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	want := []string{"core.pkg.installed", "core.git.cloned", "core.cmd.shell", "core.service.running"}
	if names := moduleNames(got); strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("modules = %v, want %v", names, want)
	}
}

func TestExpandIncludes_Nested(t *testing.T) {
	root := []Task{{Include: &IncludeTask{Include: "a.yml"}}}
	files := map[string]string{
		"a.yml": "- include: b.yml\n",
		"b.yml": "- name: leaf\n  module: core.cmd.shell\n  params: { cmd: 'true' }\n",
	}
	got, diags := ExpandIncludes(root, mapResolver(files))
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if names := moduleNames(got); len(names) != 1 || names[0] != "core.cmd.shell" {
		t.Fatalf("modules = %v, want [core.cmd.shell]", names)
	}
}

func TestExpandIncludes_DirectCycle(t *testing.T) {
	root := []Task{{Include: &IncludeTask{Include: "self.yml"}}}
	files := map[string]string{
		"self.yml": "- include: self.yml\n",
	}
	_, diags := ExpandIncludes(root, mapResolver(files))
	if !hasCode(diags, "include_cycle") {
		t.Fatalf("ожидался include_cycle, diagnostics: %v", diags)
	}
}

func TestExpandIncludes_MutualCycle(t *testing.T) {
	root := []Task{{Include: &IncludeTask{Include: "a.yml"}}}
	files := map[string]string{
		"a.yml": "- include: b.yml\n",
		"b.yml": "- include: a.yml\n",
	}
	_, diags := ExpandIncludes(root, mapResolver(files))
	if !hasCode(diags, "include_cycle") {
		t.Fatalf("ожидался include_cycle для a→b→a, diagnostics: %v", diags)
	}
}

func TestExpandIncludes_ResolveError(t *testing.T) {
	root := []Task{{Include: &IncludeTask{Include: "missing.yml"}}}
	_, diags := ExpandIncludes(root, mapResolver(map[string]string{}))
	if !hasCode(diags, "include_resolve_failed") {
		t.Fatalf("ожидался include_resolve_failed, diagnostics: %v", diags)
	}
}

func TestExpandIncludes_ParseErrorInSub(t *testing.T) {
	root := []Task{{Include: &IncludeTask{Include: "bad.yml"}}}
	files := map[string]string{
		// A task without a discriminator — a schema error from the task parser.
		"bad.yml": "- name: orphan\n  params: {}\n",
	}
	_, diags := ExpandIncludes(root, mapResolver(files))
	if !diag.HasErrors(diags) {
		t.Fatalf("ожидалась диагностика парсинга подключённого файла, diagnostics: %v", diags)
	}
}

func TestExpandIncludes_ModifierUnsupported(t *testing.T) {
	cases := map[string]Task{
		"vars":     {Include: &IncludeTask{Include: "x.yml"}, Vars: map[string]any{"a": 1}},
		"loop":     {Include: &IncludeTask{Include: "x.yml"}, Loop: &LoopSpec{Items: "${ input.users }"}},
		"on":       {Include: &IncludeTask{Include: "x.yml"}, On: []any{"coven"}},
		"serial":   {Include: &IncludeTask{Include: "x.yml"}, Serial: 2},
		"run_once": {Include: &IncludeTask{Include: "x.yml"}, RunOnce: true},
		"register": {Include: &IncludeTask{Include: "x.yml"}, Register: "r"},
	}
	files := map[string]string{"x.yml": "- name: a\n  module: core.cmd.shell\n  params: { cmd: 'true' }\n"}
	for name, task := range cases {
		t.Run(name, func(t *testing.T) {
			_, diags := ExpandIncludes([]Task{task}, mapResolver(files))
			if !hasCode(diags, "include_modifier_unsupported") {
				t.Fatalf("ожидался include_modifier_unsupported, diagnostics: %v", diags)
			}
		})
	}
}

func TestExpandIncludes_DepthExceeded(t *testing.T) {
	// A linear NON-cyclic include chain deeper than the ceiling: each file
	// `level-<n>.yml` includes `level-<n+1>.yml`, the last a leaf module.
	// The resolver generates content on the fly (no 33 fixtures); all display
	// paths differ, so cycle detection won't fire — it hits the depth limit.
	depth := maxIncludeDepth + 1
	resolve := func(name string) ([]byte, string, error) {
		var n int
		if _, err := fmt.Sscanf(name, "level-%d.yml", &n); err != nil {
			return nil, "", fmt.Errorf("неожиданное имя %q", name)
		}
		if n >= depth {
			return []byte("- name: leaf\n  module: core.cmd.shell\n  params: { cmd: 'true' }\n"), name, nil
		}
		body := fmt.Sprintf("- include: level-%d.yml\n", n+1)
		return []byte(body), name, nil
	}
	root := []Task{{Include: &IncludeTask{Include: "level-0.yml"}}}
	_, diags := ExpandIncludes(root, resolve)
	if !hasCode(diags, "include_depth_exceeded") {
		t.Fatalf("ожидался include_depth_exceeded для цепочки глубиной %d, diagnostics: %v", depth, diags)
	}
}

// levelChainResolver generates a linear NON-cyclic include chain
// `level-<n>.yml` → `level-<n+1>.yml` on the fly (no N fixtures). File
// `level-<leafLevel>.yml` is the leaf module (end of the chain); nothing deeper
// is generated. All display paths differ — cycle detection won't fire, only the
// depth check hits the ceiling.
func levelChainResolver(leafLevel int) IncludeResolver {
	return func(name string) ([]byte, string, error) {
		var n int
		if _, err := fmt.Sscanf(name, "level-%d.yml", &n); err != nil {
			return nil, "", fmt.Errorf("неожиданное имя %q", name)
		}
		if n >= leafLevel {
			return []byte("- name: leaf\n  module: core.cmd.shell\n  params: { cmd: 'true' }\n"), name, nil
		}
		return []byte(fmt.Sprintf("- include: level-%d.yml\n", n+1)), name, nil
	}
}

// TestExpandIncludes_DepthBoundary guards the off-by-one in `len(stack) >=
// maxIncludeDepth`. The deepest allowed chain must pass; one deeper must hit
// include_depth_exceeded.
//
// expandOne for `level-<n>.yml` is called with a stack of length exactly n (the
// root include runs on a nil stack: level-0 at len 0, level-1 at len 1, …). The
// depth check `len(stack) >= maxIncludeDepth` fires on the first include whose
// stack has grown to maxIncludeDepth, i.e. `level-<maxIncludeDepth>.yml`. So the
// deepest leaf still resolved is `level-<maxIncludeDepth-1>.yml` (expanded at
// len(stack) == maxIncludeDepth-1 < maxIncludeDepth), and `level-<maxIncludeDepth>.yml`
// is the first rejected. Shifting the check to `>` (off-by-one) would let
// maxIncludeDepth through — this test catches that.
func TestExpandIncludes_DepthBoundary(t *testing.T) {
	cases := []struct {
		name      string
		leafLevel int
		wantExc   bool
	}{
		{"deepest-allowed", maxIncludeDepth - 1, false},
		{"one-over", maxIncludeDepth, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := []Task{{Include: &IncludeTask{Include: "level-0.yml"}}}
			got, diags := ExpandIncludes(root, levelChainResolver(tc.leafLevel))
			gotExc := hasCode(diags, "include_depth_exceeded")
			if gotExc != tc.wantExc {
				t.Fatalf("include_depth_exceeded = %v, want %v (leafLevel=%d), diagnostics: %v",
					gotExc, tc.wantExc, tc.leafLevel, diags)
			}
			if !tc.wantExc {
				// At the boundary the chain expands to a single leaf.
				if diag.HasErrors(diags) {
					t.Fatalf("на границе depth=%d unexpected diagnostics: %v", tc.leafLevel, diags)
				}
				if names := moduleNames(got); len(names) != 1 || names[0] != "core.cmd.shell" {
					t.Fatalf("modules = %v, want [core.cmd.shell]", names)
				}
			}
		})
	}
}

// TestExpandIncludes_Diamond pins that the same file included in TWO independent
// branches (a→b→leaf and a→c→leaf) is NOT a cycle: cycle detection looks at the
// active chain (visited stack), not a global seen-set. leaf expands TWICE. A
// regression guard against refactoring cycle detection into a global set that
// would silently collapse the diamond.
func TestExpandIncludes_Diamond(t *testing.T) {
	root := []Task{{Include: &IncludeTask{Include: "a.yml"}}}
	files := map[string]string{
		"a.yml":    "- include: b.yml\n- include: c.yml\n",
		"b.yml":    "- include: leaf.yml\n",
		"c.yml":    "- include: leaf.yml\n",
		"leaf.yml": "- name: leaf\n  module: core.cmd.shell\n  params: { cmd: 'true' }\n",
	}
	got, diags := ExpandIncludes(root, mapResolver(files))
	if diag.HasErrors(diags) {
		t.Fatalf("diamond не должен давать ошибок (это не цикл), diagnostics: %v", diags)
	}
	names := moduleNames(got)
	count := 0
	for _, n := range names {
		if n == "core.cmd.shell" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("leaf-задача встречается %d раз(а), want 2 (diamond раскрывается дважды); modules = %v", count, names)
	}
}

// TestExpandIncludes_NameAllowed — positive whitelist: an include task with only
// `name:` filled (and nothing else) must NOT yield include_modifier_unsupported
// and must expand normally. Guards the "accidentally added Name to the forbidden
// fields" regression (Name is deliberately outside includeModifierReason: an
// include node's name is a legitimate label, not scope/control).
func TestExpandIncludes_NameAllowed(t *testing.T) {
	root := []Task{{Name: "подключаю install", Include: &IncludeTask{Include: "x.yml"}}}
	files := map[string]string{"x.yml": "- name: a\n  module: core.cmd.shell\n  params: { cmd: 'true' }\n"}
	got, diags := ExpandIncludes(root, mapResolver(files))
	if hasCode(diags, "include_modifier_unsupported") {
		t.Fatalf("name: на include-задаче не должен отвергаться, diagnostics: %v", diags)
	}
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if names := moduleNames(got); len(names) != 1 || names[0] != "core.cmd.shell" {
		t.Fatalf("modules = %v, want [core.cmd.shell]", names)
	}
}

// --- T2: uniqueness of the register ∪ id address space across include ---

// TestExpandIncludes_DuplicateAddressAcrossInclude — the main file's register and
// the included file's register/id collide: per-file validateTaskRefs does NOT see
// this duplicate (different scopes); only post-flatten validateFlatTaskAddresses catches it.
func TestExpandIncludes_DuplicateAddressAcrossInclude(t *testing.T) {
	cases := map[string]struct {
		root string // address on the root task (register/id)
		sub  string // address on the included file's task
	}{
		"register-vs-register": {
			root: "register: shared",
			sub:  "register: shared",
		},
		"register-vs-id": {
			root: "register: shared",
			sub:  "id: shared",
		},
		"id-vs-id": {
			root: "id: shared",
			sub:  "id: shared",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rootSrc := fmt.Sprintf(`
- name: root task
  module: core.file.present
  %s
  params: { path: /etc/x, content: a }
- include: sub.yml
`, tc.root)
			subSrc := fmt.Sprintf(`
- name: sub task
  module: core.sysctl.present
  %s
  params: { name: vm.swappiness, value: "10" }
`, tc.sub)
			rootTasks, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(rootSrc), ValidateOptions{})
			if diag.HasErrors(diags) {
				t.Fatalf("root-файл не должен иметь per-file ошибок (дубль cross-file): %v", diags)
			}
			files := map[string]string{"sub.yml": subSrc}
			_, idiags := ExpandIncludes(rootTasks, mapResolver(files))
			if !hasCode(idiags, "duplicate_task_address") {
				t.Fatalf("ожидался duplicate_task_address через include, diagnostics: %v", idiags)
			}
		})
	}
}

// TestExpandIncludes_UniqueAddressAcrossInclude — different addresses in the main
// and included files expand without a duplicate.
func TestExpandIncludes_UniqueAddressAcrossInclude(t *testing.T) {
	rootSrc := `
- name: root task
  module: core.file.present
  register: root_conf
  params: { path: /etc/x, content: a }
- include: sub.yml
`
	files := map[string]string{
		"sub.yml": `
- name: sub task
  module: core.sysctl.present
  id: sub_tuned
  params: { name: vm.swappiness, value: "10" }
`,
	}
	rootTasks, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(rootSrc), ValidateOptions{})
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected per-file diagnostics: %v", diags)
	}
	_, idiags := ExpandIncludes(rootTasks, mapResolver(files))
	if diag.HasErrors(idiags) {
		t.Fatalf("уникальные адреса через include не должны давать ошибок: %v", idiags)
	}
}

// --- conditional-include: carry-through include-when + group-id ---

// TestExpandIncludes_ConditionalWhenCarriesThrough — a static `when:` on an include
// expands without error and threads the include-when + one group-id into EVERY
// spliced task (Task.IncludeWhen/IncludeGroupID). This is the raw material for
// keeper-side group-drop (render drops the whole group in one include-when evaluation).
func TestExpandIncludes_ConditionalWhenCarriesThrough(t *testing.T) {
	root := []Task{
		{Module: &ModuleTask{Module: "core.cmd.shell", Params: map[string]any{"cmd": "true"}}},
		{Include: &IncludeTask{Include: "cluster.yml"}, When: "input.topology == 'cluster'"},
	}
	files := map[string]string{
		"cluster.yml": `
- name: a
  module: core.cmd.shell
  params: { cmd: "a" }
- name: b
  module: core.cmd.shell
  params: { cmd: "b" }
`,
	}
	got, diags := ExpandIncludes(root, mapResolver(files))
	if diag.HasErrors(diags) {
		t.Fatalf("статический when на include не должен давать ошибок: %v", diags)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (head + 2 вклеенных)", len(got))
	}
	// Head — outside the conditional include.
	if got[0].IncludeGroupID != 0 || got[0].IncludeWhen != "" {
		t.Errorf("head: IncludeGroupID=%d IncludeWhen=%q, want 0/\"\" (безусловная задача)", got[0].IncludeGroupID, got[0].IncludeWhen)
	}
	// Both spliced tasks carry the same group-id and include-when.
	for _, gi := range []int{1, 2} {
		if got[gi].IncludeGroupID == 0 {
			t.Errorf("got[%d].IncludeGroupID = 0, want != 0 (условный include)", gi)
		}
		if got[gi].IncludeWhen != "input.topology == 'cluster'" {
			t.Errorf("got[%d].IncludeWhen = %q, want протянутый include-when", gi, got[gi].IncludeWhen)
		}
	}
	if got[1].IncludeGroupID != got[2].IncludeGroupID {
		t.Errorf("задачи одной include-группы получили РАЗНЫЕ group-id: %d vs %d", got[1].IncludeGroupID, got[2].IncludeGroupID)
	}
}

// TestExpandIncludes_ConditionalGroupsDistinct — two conditional includes yield
// DIFFERENT group-ids (dropping each is an independent evaluation of its include-when).
func TestExpandIncludes_ConditionalGroupsDistinct(t *testing.T) {
	root := []Task{
		{Include: &IncludeTask{Include: "a.yml"}, When: "input.x"},
		{Include: &IncludeTask{Include: "b.yml"}, When: "input.y"},
	}
	files := map[string]string{
		"a.yml": "- name: a\n  module: core.cmd.shell\n  params: { cmd: 'a' }\n",
		"b.yml": "- name: b\n  module: core.cmd.shell\n  params: { cmd: 'b' }\n",
	}
	got, diags := ExpandIncludes(root, mapResolver(files))
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].IncludeGroupID == got[1].IncludeGroupID {
		t.Errorf("два разных условных include получили ОДИН group-id %d — дроп смешался бы", got[0].IncludeGroupID)
	}
	if got[0].IncludeWhen != "input.x" || got[1].IncludeWhen != "input.y" {
		t.Errorf("include-when группы спутан: %q / %q", got[0].IncludeWhen, got[1].IncludeWhen)
	}
}

// TestExpandIncludes_NestedConditionalConjoinsWhen — ★ nested conditional include:
// an outer `when: input.x` includes a file that itself has another `when: input.y`.
// The inner group gets its OWN group-id, and its effective include-when is the
// CONJUNCTION of ancestors `(input.x) && (input.y)`, so dropping the outer group
// (input.x:false) cascades to the nested one even if its own input.y is true. This
// fixes the "nested conditional-include does not cascade the drop" bug.
func TestExpandIncludes_NestedConditionalConjoinsWhen(t *testing.T) {
	root := []Task{{Include: &IncludeTask{Include: "outer.yml"}, When: "input.x"}}
	files := map[string]string{
		"outer.yml": `
- name: outer-direct
  module: core.cmd.shell
  params: { cmd: "outer" }
- include: inner.yml
  when: input.y
`,
		"inner.yml": "- name: inner\n  module: core.cmd.shell\n  params: { cmd: 'inner' }\n",
	}
	got, diags := ExpandIncludes(root, mapResolver(files))
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (outer-direct + inner)", len(got))
	}
	// outer-direct → outer group (when input.x — no conjunction, top level).
	if got[0].IncludeWhen != "input.x" {
		t.Errorf("outer-direct.IncludeWhen = %q, want input.x", got[0].IncludeWhen)
	}
	// inner → inner group: effective when = conjunction of ancestor and its own.
	if got[1].IncludeWhen != "(input.x) && (input.y)" {
		t.Errorf("inner.IncludeWhen = %q, want \"(input.x) && (input.y)\" (конъюнкция предка для каскадного дропа)", got[1].IncludeWhen)
	}
	if got[0].IncludeGroupID == got[1].IncludeGroupID {
		t.Errorf("внешняя и вложенная группы получили один group-id %d — дроп вложенной зависел бы от внешнего when", got[0].IncludeGroupID)
	}
}

// TestExpandIncludes_NestedConditionalThroughUnconditional — ancestor-when threads
// THROUGH an unconditional intermediate include: outer(when:input.x) → mid.yml
// (no when) → inner(when:input.y). The nested conditional group's effective
// include-when is the conjunction `(input.x) && (input.y)` (the unconditional mid
// starts no group of its own, but the accumulated ancestor is kept). mid's own
// tasks (if any) belong to the outer group input.x.
func TestExpandIncludes_NestedConditionalThroughUnconditional(t *testing.T) {
	root := []Task{{Include: &IncludeTask{Include: "outer.yml"}, When: "input.x"}}
	files := map[string]string{
		"outer.yml": "- include: mid.yml\n",
		"mid.yml": `
- name: mid-direct
  module: core.cmd.shell
  params: { cmd: "mid" }
- include: inner.yml
  when: input.y
`,
		"inner.yml": "- name: inner\n  module: core.cmd.shell\n  params: { cmd: 'inner' }\n",
	}
	got, diags := ExpandIncludes(root, mapResolver(files))
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (mid-direct + inner)", len(got))
	}
	// mid-direct → outer conditional group (input.x): the unconditional mid does
	// not reset the accumulated ancestor, and the outer stamp brands its tasks with its when.
	if got[0].IncludeWhen != "input.x" {
		t.Errorf("mid-direct.IncludeWhen = %q, want input.x (ancestor сквозь безусловный include)", got[0].IncludeWhen)
	}
	// inner → conjunction of ancestor (input.x) and its own (input.y).
	if got[1].IncludeWhen != "(input.x) && (input.y)" {
		t.Errorf("inner.IncludeWhen = %q, want \"(input.x) && (input.y)\"", got[1].IncludeWhen)
	}
}

// TestExpandIncludes_ThreeLevelConjunction — three levels of conditional include:
// L1(input.a) → L2(input.b) → L3(input.c). Each level's effective include-when is
// the growing conjunction of ancestors, so a false value at ANY ancestor drops all descendants.
func TestExpandIncludes_ThreeLevelConjunction(t *testing.T) {
	root := []Task{{Include: &IncludeTask{Include: "l1.yml"}, When: "input.a"}}
	files := map[string]string{
		"l1.yml": "- name: t1\n  module: core.cmd.shell\n  params: { cmd: '1' }\n- include: l2.yml\n  when: input.b\n",
		"l2.yml": "- name: t2\n  module: core.cmd.shell\n  params: { cmd: '2' }\n- include: l3.yml\n  when: input.c\n",
		"l3.yml": "- name: t3\n  module: core.cmd.shell\n  params: { cmd: '3' }\n",
	}
	got, diags := ExpandIncludes(root, mapResolver(files))
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (t1,t2,t3)", len(got))
	}
	want := map[string]string{
		"t1": "input.a",
		"t2": "(input.a) && (input.b)",
		"t3": "((input.a) && (input.b)) && (input.c)",
	}
	byName := map[string]Task{}
	for _, t := range got {
		byName[t.Name] = t
	}
	for name, w := range want {
		if byName[name].IncludeWhen != w {
			t.Errorf("%s.IncludeWhen = %q, want %q", name, byName[name].IncludeWhen, w)
		}
	}
	// Three DIFFERENT group-ids (dropping each level is its own evaluation of its conjunction).
	ids := map[int]bool{byName["t1"].IncludeGroupID: true, byName["t2"].IncludeGroupID: true, byName["t3"].IncludeGroupID: true}
	if len(ids) != 3 {
		t.Errorf("ожидались 3 различных group-id, got %v", ids)
	}
}

// TestExpandIncludes_DynamicWhenRejected — a dynamic when on an include
// (register./soulprint.) → include_when_dynamic_unsupported. The include expands
// BEFORE Stratify, so register/soulprint are unavailable.
func TestExpandIncludes_DynamicWhenRejected(t *testing.T) {
	cases := map[string]string{
		"register":  "register.probe.changed",
		"soulprint": "soulprint.self.os.family == 'debian'",
		"mixed":     "input.x && register.probe.ok",
	}
	files := map[string]string{"x.yml": "- name: a\n  module: core.cmd.shell\n  params: { cmd: 'true' }\n"}
	for name, when := range cases {
		t.Run(name, func(t *testing.T) {
			root := []Task{{Include: &IncludeTask{Include: "x.yml"}, When: when}}
			_, diags := ExpandIncludes(root, mapResolver(files))
			if !hasCode(diags, "include_when_dynamic_unsupported") {
				t.Fatalf("ожидался include_when_dynamic_unsupported для when %q, diagnostics: %v", when, diags)
			}
		})
	}
}

// TestConditionalInclude_CrossFileRegisterRejectedOffline — the safety foundation of
// group-drop: an external `onchanges:` on a register emitted in an included file is
// already rejected OFFLINE by per-file validateTaskRefs (unknown_register_reference)
// when loading the consuming file — its register scope does not see another file.
// So such a scenario never reaches the render phase, and dropping a conditional
// include cannot leave a dangling external onchanges → ErrOnChangesUnknownRegister
// never arises on the keeper.
func TestConditionalInclude_CrossFileRegisterRejectedOffline(t *testing.T) {
	// Main file: onchanges on a register declared ONLY in the included file.
	mainSrc := `
- name: external consumer
  module: core.cmd.shell
  onchanges: [probe_done]
  params: { cmd: "react" }
- include: cluster.yml
  when: input.topology == 'cluster'
`
	_, diags, _ := LoadDestinyTasksFromBytes("scenario/create/main.yml", []byte(mainSrc), ValidateOptions{})
	if !hasCode(diags, "unknown_register_reference") {
		t.Fatalf("cross-file onchanges на register included-файла обязан отвергаться офлайн (unknown_register_reference), diagnostics: %v", diags)
	}
}

func TestExpandIncludes_NoInclude(t *testing.T) {
	root := []Task{{Module: &ModuleTask{Module: "core.cmd.shell", Params: map[string]any{"cmd": "true"}}}}
	got, diags := ExpandIncludes(root, mapResolver(map[string]string{}))
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (passthrough без include)", len(got))
	}
}
