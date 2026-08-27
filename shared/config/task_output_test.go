package config

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// TestTaskOutput_RefusedOnEveryKind — ★ guard (NIM-334): task-level `output:` is
// refused fail-closed on EVERY task kind, in both entities that carry tasks.
//
// The key parsed into Task.Output and nothing ever resolved it: no consumer
// materialised a value and no name was checked against the destiny's top-level
// `output:` schema, while destiny/tasks.md §9 documented both halves as working.
// The whole output contract is one unbuilt slice, so the key is refused rather
// than accepted-and-dropped.
//
// The sub-tests are the discriminators plus the two placements a task-key check
// can be skipped in: a child of a `block:` (validateTaskNode recurses) and a
// destiny's own tasks/main.yml (a different loader). A refusal that covered only
// the scenario's flat list would leave two ways to write the key silently.
func TestTaskOutput_RefusedOnEveryKind(t *testing.T) {
	scenarios := map[string]struct{ src, wantPath, wantAbsent string }{
		"module": {`name: x
tasks:
  - module: core.exec.run
    params: { cmd: "true" }
    output: { dsn: "postgres://h/db" }
`, "$.tasks[0].output", ""},
		"apply": {`name: x
tasks:
  - apply:
      destiny: redis
      input: {}
    output: { dsn: "postgres://h/db" }
`, "$.tasks[0].output", ""},
		"block": {`name: x
tasks:
  - output: { dsn: "postgres://h/db" }
    block:
      - module: core.exec.run
        params: { cmd: "true" }
`, "$.tasks[0].output", "output_on_block_invalid"},
		"block child": {`name: x
tasks:
  - block:
      - module: core.exec.run
        params: { cmd: "true" }
        output: { dsn: "postgres://h/db" }
`, "$.tasks[0].block[0].output", ""},
		"include": {`name: x
tasks:
  - include: shared/probe.yml
    output: { dsn: "postgres://h/db" }
`, "$.tasks[0].output", ""},
		"keeper task": {`name: x
tasks:
  - module: core.vault.kv-read
    on: keeper
    params: { path: "svc/db" }
    output: { dsn: "postgres://h/db" }
`, "$.tasks[0].output", ""},
		"assert": {`name: x
tasks:
  - assert:
      that: ["1 == 1"]
      message: "nope"
    output: { dsn: "postgres://h/db" }
`, "$.tasks[0].output", ""},
	}
	for name, tc := range scenarios {
		t.Run(name, func(t *testing.T) {
			_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(tc.src), ValidateOptions{})
			assertOutputUnsupported(t, diags, tc.wantPath)
			// One key, one diagnostic: the universal refusal replaced the
			// per-discriminator one, it does not stack on top of it.
			if tc.wantAbsent != "" && hasCode(diags, tc.wantAbsent) {
				dump(t, diags)
				t.Fatalf("%s is still raised alongside output_unsupported — one key must yield one diagnostic", tc.wantAbsent)
			}
		})
	}

	// ★ No discriminator at all. Step 1c runs BEFORE the discriminator switch
	// (scenario_task.go), so the refusal does not depend on the task resolving to a
	// kind — a check placed after the switch would let a half-written task carry the
	// key past validation, and "refused on every kind" would silently exclude the
	// tasks that have no kind yet. Both diagnostics must be present: the key is
	// unbuilt independently of what the author eventually writes next to it.
	t.Run("no discriminator", func(t *testing.T) {
		const src = `name: x
tasks:
  - name: half-written
    output: { dsn: "postgres://h/db" }
`
		_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
		assertOutputUnsupported(t, diags, "$.tasks[0].output")
		if !hasCode(diags, "task_discriminator_missing") {
			dump(t, diags)
			t.Fatal("task_discriminator_missing is gone — output_unsupported must be raised ALONGSIDE the discriminator check, not instead of it")
		}
	})

	t.Run("destiny task", func(t *testing.T) {
		const src = `
- name: publish the dsn
  module: core.exec.run
  changed_when: false
  params: { cmd: "true" }
  output: { dsn: "postgres://h/db" }
`
		_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
		assertOutputUnsupported(t, diags, "$[0].output")
	})
}

// assertOutputUnsupported — the refusal fires EXACTLY ONCE and names the key: an
// author who only reads the first line of the diagnostic must be told which key
// to delete, so the code, the YAML path and the message text all have to carry
// `output`.
//
// Counting rather than stopping at the first match is the point. Two stages can
// reach the same key (schema validation and include expansion already did, and a
// second emission site is what the withdrawal of `output_on_block_invalid` and of
// the `output:` case in includeModifierReason was FOR), and a helper that returned
// on the first hit would let every test whose name claims "one key, one
// diagnostic" pass while the author got two.
func assertOutputUnsupported(t *testing.T, diags []diag.Diagnostic, wantPath string) {
	t.Helper()
	var found []diag.Diagnostic
	for _, d := range diags {
		if d.Code == "output_unsupported" {
			found = append(found, d)
		}
	}
	if len(found) != 1 {
		dump(t, diags)
		t.Fatalf("output_unsupported raised %d times at %s, want exactly 1", len(found), wantPath)
	}
	d := found[0]
	if d.Level != diag.LevelError {
		t.Fatalf("output_unsupported level = %v, want error (fail-closed)", d.Level)
	}
	if d.YAMLPath != wantPath {
		t.Fatalf("output_unsupported YAMLPath = %q, want %q", d.YAMLPath, wantPath)
	}
	if !strings.Contains(d.Message, "output:") {
		t.Fatalf("output_unsupported message does not name the key: %q", d.Message)
	}
}

// TestTaskOutput_DeclaredNameIsRefusedToo — ★ guard (NIM-334): the refusal does
// not depend on the name. docs/destiny/output.md promised that a name declared in
// the destiny's top-level `output:` would be published and an undeclared one
// would be a validation error; neither half was built, so BOTH names are refused
// with the same code. A gate that only rejected undeclared names would re-state
// the missing contract check and keep the silence for declared ones.
func TestTaskOutput_DeclaredNameIsRefusedToo(t *testing.T) {
	const manifest = `
name: db-bootstrap
output:
  dsn: { type: string }
`
	m, _, diags, err := LoadDestinyManifestFromBytes("destiny.yml", []byte(manifest), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadDestinyManifestFromBytes: %v", err)
	}
	for _, d := range diags {
		if d.Level == diag.LevelError {
			dump(t, diags)
			t.Fatalf("top-level output: is a schema declaration and stays valid; got %s", d.Code)
		}
	}
	if _, ok := m.Output["dsn"]; !ok {
		t.Fatalf("top-level output: did not decode: %#v", m.Output)
	}

	const tasks = `
- name: publish the declared field
  module: core.exec.run
  changed_when: false
  params: { cmd: "true" }
  output: { dsn: "postgres://h/db" }
`
	_, tdiags, err := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(tasks), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadDestinyTasksFromBytes: %v", err)
	}
	assertOutputUnsupported(t, tdiags, "$[0].output")
}

// TestTaskOutput_AbsentKeyIsSilent — the negative half: the check keys off the
// PRESENCE of `output:` alone, so every task that does not write it stays clean.
// Without this a refusal that fired unconditionally would still pass the tests
// above.
func TestTaskOutput_AbsentKeyIsSilent(t *testing.T) {
	const src = `name: x
tasks:
  - module: core.exec.run
    register: probe
    params: { cmd: "true" }
  - block:
      - module: core.exec.run
        params: { cmd: "true" }
  - apply:
      destiny: redis
      input: { seed: "${ register.probe.stdout }" }
    register: rolled
`
	_, _, diags, err := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifestFromBytes: %v", err)
	}
	if hasCode(diags, "output_unsupported") {
		dump(t, diags)
		t.Fatal("output_unsupported raised on tasks that carry no output:")
	}
	// ...and green for the RIGHT reason: a fixture that stopped parsing would
	// raise no codes at all and satisfy the check above without proving anything.
	for _, d := range diags {
		if d.Level == diag.LevelError {
			dump(t, diags)
			t.Fatalf("the fixture must be otherwise valid, else the absence proves nothing; got %s", d.Code)
		}
	}
}

// TestTaskOutput_IncludeYieldsOneDiagnostic — ★ guard (NIM-334): an include task
// carrying `output:` produces the refusal ONCE, across both stages an author's
// tooling runs (validation, then include expansion).
//
// The two stages used to overlap: expansion rejected `output:` as an include
// modifier and told the author to "move the modifier onto a module task of the
// included file". Once a module task refuses the key too, that advice is false —
// and a second error restating a withdrawn instruction is worse than none. The
// surviving diagnostic must be the true one.
func TestTaskOutput_IncludeYieldsOneDiagnostic(t *testing.T) {
	const src = `name: x
tasks:
  - include: shared.yml
    output: { dsn: "postgres://h/db" }
`
	m, _, diags, err := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifestFromBytes: %v", err)
	}
	assertOutputUnsupported(t, diags, "$.tasks[0].output")

	files := map[string]string{"shared.yml": "- name: a\n  module: core.cmd.shell\n  params: { cmd: 'true' }\n"}
	_, ediags := ExpandIncludes(m.Tasks, mapResolver(files))
	if hasCode(ediags, "include_modifier_unsupported") {
		dump(t, ediags)
		t.Fatal("expansion still rejects output: as an include modifier — the author gets two errors for one key, and the second one's hint points at a task kind that now refuses it as well")
	}
}

// TestTaskOutput_RefusedInsideAnIncludedFile — ★ guard (NIM-334): the eighth
// placement. A task that carries `output:` inside an INCLUDED file is refused
// too: ExpandIncludes parses each included body through
// LoadDestinyTasksFromBytes (include_expand.go:328), which runs the same
// validateTaskNode. Without this, "refused everywhere" would hold for every file
// an author writes directly and silently fail for the ones they splice in — the
// exact shape of the hole this ticket closes.
//
// The root list is built in Go, so this exercises the EXPANSION stage alone, not
// the production load→validate→expand order (that order is what
// TestTaskOutput_IncludeYieldsOneDiagnostic covers). Deliberate: the root task
// must carry no `output:` of its own, so the included body is the only thing that
// can produce the diagnostic.
//
// ★ The file is asserted, not just the path. `LoadDestinyTasksFromBytes` stamps
// `$[i]` on every element whatever file it parsed, so `$[0].output` alone is the
// SAME address a destiny's own tasks/main.yml produces — the assertion would hold
// even if the refusal came from somewhere else entirely. Diagnostic.File
// (destiny_tasks.go:94-98, the resolver's display name) is the only field that
// says which file the author has to open.
func TestTaskOutput_RefusedInsideAnIncludedFile(t *testing.T) {
	root := []Task{{Include: &IncludeTask{Include: "shared.yml"}}}
	files := map[string]string{
		"shared.yml": `- name: inner
  module: core.cmd.shell
  changed_when: false
  params: { cmd: 'true' }
  output: { dsn: "postgres://h/db" }
`,
	}
	_, diags := ExpandIncludes(root, mapResolver(files))
	assertOutputUnsupported(t, diags, "$[0].output")
	for _, d := range diags {
		if d.Code == "output_unsupported" && d.File != "shared.yml" {
			dump(t, diags)
			t.Fatalf("output_unsupported File = %q, want the included file %q — the diagnostic must send the author to the file that carries the key", d.File, "shared.yml")
		}
	}
}
