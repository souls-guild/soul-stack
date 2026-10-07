package config

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// NIM-941: `async:` and `require:` are refused wherever a scenario holds a task —
// the main file at any block depth and an included body — and left alone in a
// destiny, where ADR-0075 still gives them something to act on.

func TestDestinyOnlyKeys_ScenarioRefusesEveryPlacement(t *testing.T) {
	src := `name: x
tasks:
  - name: probe
    module: core.exec.run
    register: probe
    async: true
    params: { cmd: "true" }
  - name: fan out
    module: core.exec.run
    loop: { items: "${ ['a'] }" }
    async: true
    params: { cmd: "true" }
  - name: wait on the probe
    module: core.exec.run
    require: [probe]
    params: { cmd: "true" }
  - name: roll the destiny
    apply: { destiny: redis, input: {} }
    require: all
  - name: record the owner
    module: core.state.set
    require: [probe]
    params: { field: owner, value: alice }
  - block:
      - block:
          - module: core.exec.run
            async: true
            require: [probe]
            params: { cmd: "true" }
    require: [probe]
  - include: more.yml
    require: [probe]
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	want := []struct{ code, path string }{
		{CodeAsyncOutsideDestiny, "$.tasks[0].async"},
		{CodeAsyncOutsideDestiny, "$.tasks[1].async"},
		{CodeRequireOutsideDestiny, "$.tasks[2].require"},
		{CodeRequireOutsideDestiny, "$.tasks[3].require"},
		{CodeRequireOutsideDestiny, "$.tasks[4].require"},
		{CodeRequireOutsideDestiny, "$.tasks[5].require"},
		{CodeAsyncOutsideDestiny, "$.tasks[5].block[0].block[0].async"},
		{CodeRequireOutsideDestiny, "$.tasks[5].block[0].block[0].require"},
		{CodeRequireOutsideDestiny, "$.tasks[6].require"},
	}
	for _, w := range want {
		if !hasCodeAt(diags, w.code, w.path) {
			dump(t, diags)
			t.Errorf("expected %s at %s", w.code, w.path)
		}
	}
	if got := countCodes(diags, CodeAsyncOutsideDestiny, CodeRequireOutsideDestiny); got != len(want) {
		dump(t, diags)
		t.Errorf("%d refusals, want %d — one per key", got, len(want))
	}
}

// One key, one diagnostic: where a construct already refuses `async:` in a
// destiny and a scenario alike, its code is the one that stays.
func TestDestinyOnlyKeys_AsyncKeepsTheMoreSpecificCode(t *testing.T) {
	for name, tc := range map[string]struct{ task, code string }{
		"applier":     {"  - apply: { destiny: redis, input: {} }\n    async: true\n", "async_on_apply_invalid"},
		"block":       {"  - block:\n      - module: core.exec.run\n        params: { cmd: \"true\" }\n    async: true\n", "async_on_block_invalid"},
		"keeper-side": {"  - module: core.state.set\n    async: true\n    params: { field: owner, value: alice }\n", "async_on_keeper_invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte("name: x\ntasks:\n"+tc.task), ValidateOptions{})
			if !hasCodeAt(diags, tc.code, "$.tasks[0].async") {
				dump(t, diags)
				t.Fatalf("expected %s", tc.code)
			}
			if hasCode(diags, CodeAsyncOutsideDestiny) {
				dump(t, diags)
				t.Fatalf("%s doubled by %s", tc.code, CodeAsyncOutsideDestiny)
			}
		})
	}
}

// A scenario's included body is loaded by the same function as a destiny's task
// file; only the missing DestinyTasks flag says which one it is.
func TestDestinyOnlyKeys_ScenarioIncludedBodyRefuses(t *testing.T) {
	m, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte("name: x\ntasks:\n  - include: more.yml\n"), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("the including file is clean")
	}
	body := `- name: probe
  module: core.exec.run
  register: probe
  async: true
  params: { cmd: "true" }
- block:
    - module: core.exec.run
      require: [probe]
      params: { cmd: "true" }
`
	_, idiags := ExpandIncludes(m.Tasks, mapResolver(map[string]string{"more.yml": body}))
	for _, w := range []struct{ code, path string }{
		{CodeAsyncOutsideDestiny, "$[0].async"},
		{CodeRequireOutsideDestiny, "$[1].block[0].require"},
	} {
		if !hasCodeAt(idiags, w.code, w.path) {
			dump(t, idiags)
			t.Errorf("expected %s at %s of the included body", w.code, w.path)
		}
	}
}

// The other half: a destiny keeps both keys, in its task file, inside a block and
// one include down.
func TestDestinyOnlyKeys_DestinyKeepsBothKeys(t *testing.T) {
	main := `- name: probe
  module: core.exec.run
  register: probe
  async: true
  params: { cmd: "true" }
- name: wait on the probe
  module: core.exec.run
  require: [probe]
  params: { cmd: "true" }
- block:
    - module: core.exec.run
      require: all
      params: { cmd: "true" }
- include: more.yml
`
	body := `- name: warm
  module: core.exec.run
  register: warm
  async: true
  params: { cmd: "true" }
- name: verify
  module: core.exec.run
  require: [probe, warm]
  params: { cmd: "true" }
`
	tasks, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(main), ValidateOptions{DestinyTasks: true})
	_, idiags := ExpandIncludesInDestiny(tasks, mapResolver(map[string]string{"more.yml": body}))
	for _, d := range append(diags, idiags...) {
		if d.Level == diag.LevelError {
			dump(t, append(diags, idiags...))
			t.Fatalf("a destiny's async:/require: refused: %s", d.Code)
		}
	}
}

// A scenario include carrying either key draws only the validation refusal: the
// expansion's include_modifier_unsupported would send the author to move the key
// onto a task of the body, where it is refused too. In a destiny that expansion
// code stays the key's only refusal.
func TestDestinyOnlyKeys_IncludeModifierOnlyInADestiny(t *testing.T) {
	body := map[string]string{"more.yml": "- module: core.exec.run\n  params: { cmd: \"true\" }\n"}
	for _, key := range []string{"require: [probe]", "async: true"} {
		t.Run(key, func(t *testing.T) {
			list := "- module: core.exec.run\n  register: probe\n  params: { cmd: \"true\" }\n- include: more.yml\n  " + key + "\n"

			m, _, _, _ := LoadScenarioManifestFromBytes("main.yml", []byte("name: x\ntasks:\n"+indent(list)), ValidateOptions{})
			if _, idiags := ExpandIncludes(m.Tasks, mapResolver(body)); hasCode(idiags, "include_modifier_unsupported") {
				dump(t, idiags)
				t.Errorf("a scenario include doubled its validation refusal at expansion")
			}

			tasks, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(list), ValidateOptions{DestinyTasks: true})
			if diag.HasErrors(diags) {
				dump(t, diags)
				t.Fatalf("the destiny file itself is clean")
			}
			if _, idiags := ExpandIncludesInDestiny(tasks, mapResolver(body)); !hasCode(idiags, "include_modifier_unsupported") {
				dump(t, idiags)
				t.Errorf("a destiny include carrying %s was not refused", key)
			}
		})
	}
}

// A scenario's `require:` draws one refusal, not a second one about its names: a
// hint to fix a typo or move a source would lead back to require_outside_destiny.
// A destiny still judges the names (task_require_order_test.go, task_refs_test.go).
func TestDestinyOnlyKeys_ScenarioRequireNamesAreNotJudged(t *testing.T) {
	list := `- module: core.exec.run
  require: [later]
  params: { cmd: "true" }
- module: core.exec.run
  require: [typo]
  params: { cmd: "true" }
- module: core.exec.run
  register: later
  params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte("name: x\ntasks:\n"+indent(list)), ValidateOptions{})
	m, _, _, _ := LoadScenarioManifestFromBytes("main.yml", []byte("name: x\ntasks:\n  - include: more.yml\n"), ValidateOptions{})
	_, idiags := ExpandIncludes(m.Tasks, mapResolver(map[string]string{"more.yml": list}))
	for name, ds := range map[string][]diag.Diagnostic{"main file": diags, "included body": idiags} {
		if hasCode(ds, "require_forward_reference") || hasCode(ds, "unknown_register_reference") {
			dump(t, ds)
			t.Errorf("%s: a refused require: list had its names judged too", name)
		}
		if countCodes(ds, CodeRequireOutsideDestiny) != 2 {
			dump(t, ds)
			t.Errorf("%s: want one %s per require:", name, CodeRequireOutsideDestiny)
		}
	}
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(strings.TrimSuffix(s, "\n"), "\n", "\n  ") + "\n"
}

func countCodes(diags []diag.Diagnostic, codes ...string) int {
	n := 0
	for _, d := range diags {
		for _, c := range codes {
			if d.Code == c {
				n++
			}
		}
	}
	return n
}
