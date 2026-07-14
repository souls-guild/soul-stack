package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

func TestLoadScenarioManifest_Golden(t *testing.T) {
	// Local patched copy of the golden (soul-lint/testdata/scenario-golden/): a
	// self-contained redis-create fixture, historically derived from the
	// redis-cluster create scenario. The derivation is intentional — the original
	// had a deviation from input.md (`type: object` without `properties:`); here
	// the form is fixed to the normative schema.
	path := filepath.FromSlash("../../soul-lint/testdata/scenario-golden/redis-create.yml")
	cfg, doc, diags, err := LoadScenarioManifest(path, ValidateOptions{})
	if err != nil {
		t.Fatalf("io error: %v", err)
	}
	if cfg == nil || doc == nil {
		t.Fatalf("cfg/doc must be non-nil")
	}
	if diag.HasErrors(diags) {
		for _, d := range diags {
			t.Logf("[%s] %s:%d:%d %s %s", d.Code, d.File, d.Line, d.Column, d.Message, d.YAMLPath)
		}
		t.Fatalf("expected 0 errors on golden scenario, got %d diagnostics", len(diags))
	}
	if cfg.Name != "create" {
		t.Errorf("name: got %q want create", cfg.Name)
	}
	if cfg.StateChanges == nil || len(cfg.StateChanges.Sets) == 0 {
		t.Errorf("state_changes.sets must be parsed")
	}
	if len(cfg.Tasks) == 0 {
		t.Errorf("tasks must be parsed")
	}
	// Discriminator round-trip smoke: the first task is module:.
	if cfg.Tasks[0].Module == nil {
		t.Errorf("tasks[0].Module must be set (provision uses core.cloud.created)")
	}
	if cfg.Tasks[0].Module != nil && cfg.Tasks[0].Module.Module != "core.cloud.created" {
		t.Errorf("tasks[0].Module.Module: got %q", cfg.Tasks[0].Module.Module)
	}
}

func TestLoadScenarioManifest_MissingName(t *testing.T) {
	src := `description: no name
tasks: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "missing_required_field") {
		dump(t, diags)
		t.Fatalf("expected missing_required_field for absent name")
	}
}

func TestLoadScenarioManifest_BadName(t *testing.T) {
	src := `name: Create
tasks: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "name_invalid_format") {
		dump(t, diags)
		t.Fatalf("expected name_invalid_format")
	}
}

func TestLoadScenarioManifest_MissingTasks(t *testing.T) {
	src := `name: noop
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	found := false
	for _, d := range diags {
		if d.Code == "missing_required_field" && d.YAMLPath == "$.tasks" {
			found = true
		}
	}
	if !found {
		dump(t, diags)
		t.Fatalf("expected missing_required_field on $.tasks")
	}
}

func TestLoadScenarioManifest_EmptyTasksOK(t *testing.T) {
	// Empty tasks: [] is valid (no-op scenario).
	src := `name: noop
tasks: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected no errors for empty tasks: []")
	}
}

func TestLoadScenarioManifest_DeprecatedKeys(t *testing.T) {
	for _, key := range []string{"wait", "filter", "version"} {
		key := key
		t.Run(key, func(t *testing.T) {
			src := "name: x\ntasks: []\n" + key + ": foo\n"
			_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
			found := false
			for _, d := range diags {
				if d.Code == "unknown_key" && d.YAMLPath == "$."+key && d.Hint != "" {
					found = true
				}
			}
			if !found {
				dump(t, diags)
				t.Fatalf("expected unknown_key with hint for deprecated %q", key)
			}
			// There must be no duplicate unknown_key on the same path.
			count := 0
			for _, d := range diags {
				if d.Code == "unknown_key" && d.YAMLPath == "$."+key {
					count++
				}
			}
			if count != 1 {
				dump(t, diags)
				t.Fatalf("expected exactly 1 unknown_key for $.%s, got %d", key, count)
			}
		})
	}
}

func TestLoadScenarioManifest_TaskNoDiscriminator(t *testing.T) {
	src := `name: x
tasks:
  - name: bare task
    when: "true"
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "task_discriminator_missing") {
		dump(t, diags)
		t.Fatalf("expected task_discriminator_missing")
	}
}

func TestLoadScenarioManifest_TaskMultiDiscriminator(t *testing.T) {
	src := `name: x
tasks:
  - name: both
    module: core.exec.run
    params: { cmd: "true" }
    apply:
      destiny: redis
      input: {}
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "task_discriminator_multiple") {
		dump(t, diags)
		t.Fatalf("expected task_discriminator_multiple")
	}
}

func TestLoadScenarioManifest_AllFourDiscriminators(t *testing.T) {
	// Edge case: one valid task of each kind.
	src := `name: x
tasks:
  - module: core.exec.run
    params: { cmd: "true" }
  - apply:
      destiny: redis
      input: { action: apply }
  - include: install.yml
  - block:
      - module: core.exec.run
        params: { cmd: "id" }
`
	cfg, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected no errors")
	}
	if cfg.Tasks[0].Module == nil || cfg.Tasks[1].Apply == nil ||
		cfg.Tasks[2].Include == nil || cfg.Tasks[3].Block == nil {
		t.Fatalf("discriminator fields not populated: %#v", cfg.Tasks)
	}
	if cfg.Tasks[2].Include.Include != "install.yml" {
		t.Errorf("include name not captured: %q", cfg.Tasks[2].Include.Include)
	}
	if len(cfg.Tasks[3].Block.Block) != 1 {
		t.Errorf("block content not captured: %#v", cfg.Tasks[3].Block.Block)
	}
}

// TestLoadScenarioManifest_AssertTask — an assert task (ADR-009 amendment
// 2026-06-23): a valid form parses into AssertSpec (assert discriminator).
func TestLoadScenarioManifest_AssertTask(t *testing.T) {
	src := `name: x
tasks:
  - name: topology guard
    when: "input.redis_type == 'cluster'"
    assert:
      that:
        - "size(soulprint.hosts) == int(input.shards)"
      message: "topology mismatch"
`
	cfg, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected no errors on valid assert task")
	}
	if cfg.Tasks[0].Assert == nil {
		t.Fatalf("Assert field not populated: %#v", cfg.Tasks[0])
	}
	if len(cfg.Tasks[0].Assert.That) != 1 {
		t.Errorf("Assert.That = %#v, want 1 predicate", cfg.Tasks[0].Assert.That)
	}
	if cfg.Tasks[0].Assert.Message != "topology mismatch" {
		t.Errorf("Assert.Message = %q", cfg.Tasks[0].Assert.Message)
	}
}

// TestLoadScenarioManifest_AssertEmptyThat — empty that[] → error
// (assert requires at least one predicate).
func TestLoadScenarioManifest_AssertEmptyThat(t *testing.T) {
	src := `name: x
tasks:
  - assert:
      that: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "missing_required_field") {
		dump(t, diags)
		t.Fatalf("expected missing_required_field on empty assert.that")
	}
}

// TestLoadScenarioManifest_AssertWithModuleConflict — assert ⊕ module:
// (assert is a discriminator, mutually exclusive with module/apply/include/block).
func TestLoadScenarioManifest_AssertWithModuleConflict(t *testing.T) {
	src := `name: x
tasks:
  - module: core.exec.run
    params: { cmd: "true" }
    assert:
      that: [ "true" ]
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "task_discriminator_multiple") {
		dump(t, diags)
		t.Fatalf("expected task_discriminator_multiple for assert + module")
	}
}

func TestLoadScenarioManifest_BadModuleFormat(t *testing.T) {
	src := `name: x
tasks:
  - module: core.exec
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "module_format_invalid") {
		dump(t, diags)
		t.Fatalf("expected module_format_invalid for 2-level form")
	}
}

func TestLoadScenarioManifest_SerialRunOnceConflict(t *testing.T) {
	src := `name: x
tasks:
  - module: core.exec.run
    params: { cmd: "true" }
    serial: 1
    run_once: true
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "serial_run_once_conflict") {
		dump(t, diags)
		t.Fatalf("expected serial_run_once_conflict")
	}
}

func TestLoadScenarioManifest_RegisterOnBlock(t *testing.T) {
	src := `name: x
tasks:
  - register: r
    block:
      - module: core.exec.run
        params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "register_on_block_invalid") {
		dump(t, diags)
		t.Fatalf("expected register_on_block_invalid")
	}
}

// TestLoadScenarioManifest_BlockForbiddenKeys (guard #8) — module-specific keys
// on a block task are cut fail-closed with code <key>_on_block_invalid
// (destiny/tasks.md §6.5 does not mention them on block). parallel: is also rejected.
func TestLoadScenarioManifest_BlockForbiddenKeys(t *testing.T) {
	cases := map[string]string{
		"changed_when_on_block_invalid": "changed_when: \"true\"",
		"failed_when_on_block_invalid":  "failed_when: \"false\"",
		"retry_on_block_invalid":        "retry: { count: 3 }",
		"timeout_on_block_invalid":      "timeout: 30s",
		"output_on_block_invalid":       "output: { x: \"y\" }",
		"no_log_on_block_invalid":       "no_log: true",
		"params_on_block_invalid":       "params: { a: 1 }",
		"parallel_on_block_invalid":     "parallel: true",
	}
	for wantCode, line := range cases {
		t.Run(wantCode, func(t *testing.T) {
			src := "name: x\ntasks:\n  - " + line + "\n    block:\n      - module: core.exec.run\n        params: { cmd: \"true\" }\n"
			_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
			if !hasCode(diags, wantCode) {
				dump(t, diags)
				t.Fatalf("expected %s", wantCode)
			}
		})
	}
}

// TestLoadScenarioManifest_BlockInheritedKeysOK — inherited keys (when/
// where/vars/onchanges/onfail/serial/run_once/name) on a block task are VALID (§6.5
// explicitly allows them) — must not yield <key>_on_block_invalid.
func TestLoadScenarioManifest_BlockInheritedKeysOK(t *testing.T) {
	src := `name: x
tasks:
  - module: core.exec.run
    register: probe
    params: { cmd: "true" }
  - name: grp
    when: "input.go"
    where: "register.probe.changed"
    serial: 1
    vars: { v: "x" }
    onchanges: [probe]
    block:
      - module: core.service.restarted
        params: { name: redis }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	for _, d := range diags {
		if d.Level == diag.LevelError && strings.HasSuffix(d.Code, "_on_block_invalid") {
			dump(t, diags)
			t.Fatalf("inherited key wrongly rejected: %s", d.Code)
		}
	}
}

func TestLoadScenarioManifest_BadOn(t *testing.T) {
	src := `name: x
tasks:
  - on: 42
    module: core.exec.run
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "type_mismatch") {
		dump(t, diags)
		t.Fatalf("expected type_mismatch on on:")
	}
}

func TestLoadScenarioManifest_ChangedWhenBoolLiteral(t *testing.T) {
	// changed_when:/failed_when: accept a bool literal (force-shortcut) and a
	// CEL string; invalid types (number/list) → type_mismatch.
	ok := []string{"false", "true", `"register.self.exit_code != 0"`}
	for _, v := range ok {
		src := "name: x\ntasks:\n  - module: core.exec.run\n    params: { cmd: \"true\" }\n    changed_when: " + v + "\n    failed_when: " + v + "\n"
		_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
		for _, d := range diags {
			if d.Level == diag.LevelError {
				dump(t, diags)
				t.Fatalf("changed_when/failed_when: %s — неожиданная ошибка валидации", v)
			}
		}
	}

	bad := []string{"42", "[a, b]"}
	for _, v := range bad {
		src := "name: x\ntasks:\n  - module: core.exec.run\n    params: { cmd: \"true\" }\n    changed_when: " + v + "\n"
		_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
		if !hasCodeAt(diags, "type_mismatch", "$.tasks[0].changed_when") {
			dump(t, diags)
			t.Fatalf("changed_when: %s — ожидался type_mismatch", v)
		}
	}
}

func TestLoadScenarioManifest_BadStateChangesKey(t *testing.T) {
	src := `name: x
tasks: []
state_changes:
  unexpected: [foo]
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	found := false
	for _, d := range diags {
		if d.Code == "unknown_key" && d.YAMLPath == "$.state_changes.unexpected" {
			found = true
		}
	}
	if !found {
		dump(t, diags)
		t.Fatalf("expected unknown_key on $.state_changes.unexpected")
	}
}

func TestLoadScenarioManifest_StateChangesSetsScalar(t *testing.T) {
	// sets is now a mapping field→expression (orchestration.md §7.1); a scalar in
	// its place → type_mismatch.
	src := `name: x
tasks: []
state_changes:
  sets: "not-a-mapping"
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "type_mismatch") {
		dump(t, diags)
		t.Fatalf("expected type_mismatch on state_changes.sets")
	}
}

func TestLoadScenarioManifest_StateChangesSetsSeqRejected(t *testing.T) {
	// The old []string form (sets: [a, b]) is no longer valid: sets is a mapping.
	src := `name: x
tasks: []
state_changes:
  sets: [redis_version, redis_users]
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "type_mismatch") {
		dump(t, diags)
		t.Fatalf("expected type_mismatch on old seq-form sets")
	}
}

func TestLoadScenarioManifest_StateChangesSetsMap(t *testing.T) {
	src := `name: x
tasks: []
state_changes:
  sets:
    greeting_file: "/tmp/soul-stack-hello"
    redis_version: "${ input.version }"
`
	cfg, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected no errors for valid sets-map")
	}
	if cfg.StateChanges == nil || len(cfg.StateChanges.Sets) != 2 {
		t.Fatalf("sets parsed = %+v, want 2 entries", cfg.StateChanges)
	}
	if cfg.StateChanges.Sets["greeting_file"] != "/tmp/soul-stack-hello" {
		t.Errorf("sets.greeting_file = %q", cfg.StateChanges.Sets["greeting_file"])
	}
	if cfg.StateChanges.Sets["redis_version"] != "${ input.version }" {
		t.Errorf("sets.redis_version = %q", cfg.StateChanges.Sets["redis_version"])
	}
}

func TestLoadScenarioManifest_StateChangesSetsEmptyValue(t *testing.T) {
	src := `name: x
tasks: []
state_changes:
  sets:
    greeting_file: ""
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "empty_value") {
		dump(t, diags)
		t.Fatalf("expected empty_value on empty sets expression")
	}
}

func TestLoadScenarioManifest_EmptyStateChangesOK(t *testing.T) {
	// state_changes: {} — valid (restart-like, see examples).
	src := `name: noop
state_changes: {}
tasks: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected no errors for empty state_changes")
	}
}

// --- New list form of state_changes (pilot: set + add). ---

// TestLoadScenarioManifest_StateChangesTransitMapForm — ★ TRANSIT: the old
// map form `state_changes: { sets: {...} }` STILL parses (deprecated) — existing
// scenarios on it stay green. IsList=false, Sets populated.
func TestLoadScenarioManifest_StateChangesTransitMapForm(t *testing.T) {
	src := `name: x
tasks: []
state_changes:
  sets:
    redis_version: "${ input.version }"
`
	cfg, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("★ TRANSIT: старая map-форма должна парситься без ошибок")
	}
	if cfg.StateChanges == nil || cfg.StateChanges.IsList {
		t.Fatalf("★ map-форма: IsList должен быть false, got %+v", cfg.StateChanges)
	}
	if cfg.StateChanges.Sets["redis_version"] != "${ input.version }" {
		t.Errorf("map-форма sets не распарсен: %+v", cfg.StateChanges.Sets)
	}
}

// TestLoadScenarioManifest_StateChangesListForm — the new list form set+add
// parses: IsList=true, Ops in order, value object preserved, on_conflict/match.
func TestLoadScenarioManifest_StateChangesListForm(t *testing.T) {
	src := `name: add_replica
tasks: []
state_changes:
  - add: redis_hosts
    value:
      sid:  "${ vars.new_sid }"
      role: replica
    match: "elem.sid == value.sid"
    on_conflict: skip
  - set: redis_version
    value: "${ input.version }"
`
	cfg, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("list-форма set+add должна валидироваться без ошибок")
	}
	sc := cfg.StateChanges
	if sc == nil || !sc.IsList || len(sc.Ops) != 2 {
		t.Fatalf("ops = %+v, want IsList + 2 ops", sc)
	}
	if sc.Ops[0].Verb != VerbAdd || sc.Ops[0].Field != "redis_hosts" {
		t.Errorf("op[0] = %+v, want add redis_hosts", sc.Ops[0])
	}
	if sc.Ops[0].Match != "elem.sid == value.sid" || sc.Ops[0].OnConflict != OnConflictSkip {
		t.Errorf("op[0] match/on_conflict = %q/%q", sc.Ops[0].Match, sc.Ops[0].OnConflict)
	}
	valMap, ok := sc.Ops[0].Value.(map[string]any)
	if !ok || valMap["role"] != "replica" {
		t.Errorf("op[0].value = %+v, want map с role:replica", sc.Ops[0].Value)
	}
	if sc.Ops[1].Verb != VerbSet || sc.Ops[1].Field != "redis_version" || sc.Ops[1].Value != "${ input.version }" {
		t.Errorf("op[1] = %+v, want set redis_version", sc.Ops[1])
	}
}

// TestLoadScenarioManifest_StateChangesEmptyListOK — an empty `state_changes: []`
// is valid (state does not change), IsList=true.
func TestLoadScenarioManifest_StateChangesEmptyListOK(t *testing.T) {
	src := `name: noop
state_changes: []
tasks: []
`
	cfg, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("пустой list state_changes должен быть валиден")
	}
	if cfg.StateChanges == nil || !cfg.StateChanges.IsList || len(cfg.StateChanges.Ops) != 0 {
		t.Fatalf("state_changes = %+v, want пустой list", cfg.StateChanges)
	}
}

func TestLoadScenarioManifest_StateChangesSetMissingValue(t *testing.T) {
	src := `name: x
tasks: []
state_changes:
  - set: redis_version
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "missing_required_field") {
		dump(t, diags)
		t.Fatalf("expected missing_required_field (set без value)")
	}
}

func TestLoadScenarioManifest_StateChangesAddMissingValue(t *testing.T) {
	src := `name: x
tasks: []
state_changes:
  - add: redis_hosts
    match: "elem.sid == value.sid"
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "missing_required_field") {
		dump(t, diags)
		t.Fatalf("expected missing_required_field (add без value)")
	}
}

func TestLoadScenarioManifest_StateChangesBadOnConflict(t *testing.T) {
	src := `name: x
tasks: []
state_changes:
  - add: redis_hosts
    value: { sid: x }
    match: "elem.sid == value.sid"
    on_conflict: overwrite
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "invalid_value") {
		dump(t, diags)
		t.Fatalf("expected invalid_value (on_conflict: overwrite не из skip/replace/error)")
	}
}

func TestLoadScenarioManifest_StateChangesSetRejectsMatch(t *testing.T) {
	// match: does not apply to set → unknown_key.
	src := `name: x
tasks: []
state_changes:
  - set: redis_version
    value: "7.2"
    match: "true"
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "unknown_key") {
		dump(t, diags)
		t.Fatalf("expected unknown_key (match на set неприменим)")
	}
}

func TestLoadScenarioManifest_StateChangesAddRejectsPatch(t *testing.T) {
	src := `name: x
tasks: []
state_changes:
  - add: redis_hosts
    value: { sid: x }
    patch: { role: replica }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "unknown_key") {
		dump(t, diags)
		t.Fatalf("expected unknown_key (patch на add неприменим)")
	}
}

// TestLoadScenarioManifest_StateChangesModifyValid — modify with match+patch
// validates without errors (a narrow match → no wide_match warning).
func TestLoadScenarioManifest_StateChangesModifyValid(t *testing.T) {
	src := `name: update_acl
tasks: []
state_changes:
  - modify: redis_users
    match: "key == input.username"
    patch:
      acl:   "${ input.acl }"
      state: "${ input.state }"
`
	cfg, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("modify с match+patch должен валидироваться без ошибок")
	}
	op := cfg.StateChanges.Ops[0]
	if op.Verb != VerbModify || op.Field != "redis_users" || op.Match != "key == input.username" {
		t.Errorf("op = %+v, want modify redis_users", op)
	}
	patch, ok := op.Patch.(map[string]any)
	if !ok || patch["acl"] != "${ input.acl }" {
		t.Errorf("op.Patch = %+v, want map acl→CEL", op.Patch)
	}
}

// TestLoadScenarioManifest_StateChangesModifyMissingPatch — modify without patch →
// missing_required_field.
func TestLoadScenarioManifest_StateChangesModifyMissingPatch(t *testing.T) {
	src := `name: x
tasks: []
state_changes:
  - modify: redis_users
    match: "key == input.username"
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "missing_required_field") {
		dump(t, diags)
		t.Fatalf("expected missing_required_field (modify без patch)")
	}
}

// TestLoadScenarioManifest_StateChangesRemoveValid — remove with match (+expect) ok.
func TestLoadScenarioManifest_StateChangesRemoveValid(t *testing.T) {
	src := `name: remove_replica
tasks: []
state_changes:
  - remove: redis_hosts
    match: "elem.sid == input.sid"
    expect: one
`
	cfg, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("remove с match+expect должен валидироваться без ошибок")
	}
	op := cfg.StateChanges.Ops[0]
	if op.Verb != VerbRemove || op.Match != "elem.sid == input.sid" || op.Expect != ExpectOne {
		t.Errorf("op = %+v, want remove + expect one", op)
	}
}

// TestLoadScenarioManifest_StateChangesForeachValid — foreach with as+do parses:
// In carries the collection CEL expression, Do — a nested add.
func TestLoadScenarioManifest_StateChangesForeachValid(t *testing.T) {
	src := `name: add_replicas
tasks: []
state_changes:
  - foreach: "${ input.replicas }"
    as: sid
    do:
      - add: redis_hosts
        value: "${ sid }"
        match: "elem == sid"
        on_conflict: skip
`
	cfg, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("foreach с as+do должен валидироваться без ошибок")
	}
	op := cfg.StateChanges.Ops[0]
	if op.Verb != VerbForeach || op.In != "${ input.replicas }" || op.As != "sid" {
		t.Errorf("op = %+v, want foreach in/as", op)
	}
	if len(op.Do) != 1 || op.Do[0].Verb != VerbAdd || op.Do[0].Field != "redis_hosts" {
		t.Errorf("op.Do = %+v, want [add redis_hosts]", op.Do)
	}
}

// TestLoadScenarioManifest_StateChangesForeachMissingAsDo — foreach without as/do.
func TestLoadScenarioManifest_StateChangesForeachMissingAsDo(t *testing.T) {
	src := `name: x
tasks: []
state_changes:
  - foreach: "${ input.replicas }"
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "missing_required_field") {
		dump(t, diags)
		t.Fatalf("expected missing_required_field (foreach без as/do)")
	}
}

// TestLoadScenarioManifest_StateChangesBadExpect — expect outside {one,at_most_one,
// any} → invalid_value.
func TestLoadScenarioManifest_StateChangesBadExpect(t *testing.T) {
	src := `name: x
tasks: []
state_changes:
  - remove: redis_hosts
    match: "elem.sid == input.sid"
    expect: exactly_two
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "invalid_value") {
		dump(t, diags)
		t.Fatalf("expected invalid_value (expect: exactly_two)")
	}
}

// TestLoadScenarioManifest_StateChangesWideMatchWarn — ★ safeguard (a):
// modify/remove without match: OR with a constant-true match → wide_match WARN
// (not an error, exit-code 0).
func TestLoadScenarioManifest_StateChangesWideMatchWarn(t *testing.T) {
	cases := map[string]string{
		"remove-no-match": `name: x
tasks: []
state_changes:
  - remove: redis_hosts
`,
		"modify-const-true": `name: x
tasks: []
state_changes:
  - modify: redis_hosts
    match: "true"
    patch: { role: "${ 'replica' }" }
`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
			if diag.HasErrors(diags) {
				dump(t, diags)
				t.Fatalf("wide match — WARN, не ошибка (exit-code 0)")
			}
			if !hasCode(diags, "wide_match") {
				dump(t, diags)
				t.Fatalf("expected wide_match warning")
			}
		})
	}
}

// TestLoadScenarioManifest_StateChangesDeprecatedMapWarn — ★ safeguard (b):
// a valid old map form gives a deprecated_form WARN; appends/modifies also give a
// noop_placeholder WARN. Not an error (dual-parse transit).
func TestLoadScenarioManifest_StateChangesDeprecatedMapWarn(t *testing.T) {
	src := `name: x
tasks: []
state_changes:
  sets:
    redis_version: "${ input.version }"
  appends: [redis_hosts]
  modifies: [redis_users.acl]
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("map-форма deprecated — WARN, не ошибка")
	}
	if !hasCode(diags, "deprecated_form") {
		dump(t, diags)
		t.Fatalf("expected deprecated_form warning on map-form")
	}
	if !hasCode(diags, "noop_placeholder") {
		dump(t, diags)
		t.Fatalf("expected noop_placeholder warning on appends/modifies")
	}
}

func TestLoadScenarioManifest_StateChangesNoVerb(t *testing.T) {
	src := `name: x
tasks: []
state_changes:
  - value: { sid: x }
    match: "true"
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "missing_required_field") {
		dump(t, diags)
		t.Fatalf("expected missing_required_field (операция без глагола)")
	}
}

func TestLoadScenarioManifest_OnAsKeeperOK(t *testing.T) {
	src := `name: x
tasks:
  - on: keeper
    module: core.cloud.created
    params: { provider: aws }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected no errors for on: keeper")
	}
}

func TestLoadScenarioManifest_OnAsBadScalar(t *testing.T) {
	// on: arbitrary-scalar (not keeper) — must be enum_invalid.
	src := `name: x
tasks:
  - on: random
    module: core.exec.run
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "enum_invalid") {
		dump(t, diags)
		t.Fatalf("expected enum_invalid on on: random (only 'keeper' as scalar)")
	}
}

func TestLoadScenarioManifest_OnCovenList(t *testing.T) {
	src := `name: x
tasks:
  - on: ["${ incarnation.name }", baremetal]
    module: core.exec.run
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected no errors for on: [CEL, coven]")
	}
}

func TestLoadScenarioManifest_BadCovenInOnList(t *testing.T) {
	src := `name: x
tasks:
  - on: [BAD_NAME]
    module: core.exec.run
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "name_invalid_format") {
		dump(t, diags)
		t.Fatalf("expected name_invalid_format for non-kebab coven in on[]")
	}
}

func TestLoadScenarioManifest_SerialPercent(t *testing.T) {
	src := `name: x
tasks:
  - serial: "25%"
    module: core.exec.run
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected no errors for serial: \"25%%\"")
	}
}

func TestLoadScenarioManifest_SerialBadPercent(t *testing.T) {
	src := `name: x
tasks:
  - serial: "0%"
    module: core.exec.run
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "value_out_of_range") {
		dump(t, diags)
		t.Fatalf("expected value_out_of_range for serial: \"0%%\"")
	}
}

func TestLoadScenarioManifest_SerialZero(t *testing.T) {
	src := `name: x
tasks:
  - serial: 0
    module: core.exec.run
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "value_out_of_range") {
		dump(t, diags)
		t.Fatalf("expected value_out_of_range for serial: 0")
	}
}

func TestLoadScenarioManifest_RegisterIdentifierInvalid(t *testing.T) {
	src := `name: x
tasks:
  - register: BAD-ID
    module: core.exec.run
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "register_identifier_invalid") {
		dump(t, diags)
		t.Fatalf("expected register_identifier_invalid")
	}
}

func TestLoadScenarioManifest_IDValidOnModule(t *testing.T) {
	// id: on a module task without register — valid.
	src := `name: x
tasks:
  - id: redis_config
    module: core.file.present
    params: { path: /etc/redis/redis.conf, content: "..." }
`
	cfg, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected no errors for valid id on module task")
	}
	if cfg.Tasks[0].ID != "redis_config" {
		t.Errorf("id not captured: %q", cfg.Tasks[0].ID)
	}
}

func TestLoadScenarioManifest_IDInvalidFormat(t *testing.T) {
	// Invalid id format: kebab / CamelCase / digit-first / empty string.
	// `redis-config` is the explicit "dash in the middle = invalid" case (id is
	// snake_case, not kebab), separate from BAD-ID where invalidity is also from caps.
	for _, v := range []string{"BAD-ID", "redis-config", "RedisConfig", "9config", `""`} {
		v := v
		t.Run(v, func(t *testing.T) {
			src := "name: x\ntasks:\n  - id: " + v + "\n    module: core.exec.run\n    params: { cmd: \"true\" }\n"
			_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
			if !hasCode(diags, "id_identifier_invalid") {
				dump(t, diags)
				t.Fatalf("expected id_identifier_invalid for id: %s", v)
			}
		})
	}
}

func TestLoadScenarioManifest_IDWithRegisterConflict(t *testing.T) {
	// Guard invariant: id together with register is always an error (a task with
	// register already has an address; id is redundant and ambiguous).
	src := `name: x
tasks:
  - id: redis_config
    register: redis_config
    module: core.file.present
    params: { path: /etc/redis/redis.conf, content: "..." }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "id_register_conflict") {
		dump(t, diags)
		t.Fatalf("expected id_register_conflict when both id and register set")
	}
}

func TestLoadScenarioManifest_IDOnBlockRejected(t *testing.T) {
	// pilot: id on a block task is not supported.
	src := `name: x
tasks:
  - id: grp
    block:
      - module: core.exec.run
        params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "id_unsupported_target") {
		dump(t, diags)
		t.Fatalf("expected id_unsupported_target for id on block task")
	}
}

func TestLoadScenarioManifest_IDOnIncludeRejected(t *testing.T) {
	// pilot: id on an include task is not supported.
	src := `name: x
tasks:
  - id: inc
    include: install.yml
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "id_unsupported_target") {
		dump(t, diags)
		t.Fatalf("expected id_unsupported_target for id on include task")
	}
}

func TestLoadScenarioManifest_NoIDOK(t *testing.T) {
	// Regression: a task without id (like everything existing) is valid — id is optional.
	src := `name: x
tasks:
  - module: core.exec.run
    register: probe
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected no errors for task without id")
	}
}

func TestLoadScenarioManifest_RetryCountMissing(t *testing.T) {
	src := `name: x
tasks:
  - module: core.exec.run
    params: { cmd: "true" }
    retry:
      delay: 5s
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "missing_required_field") {
		dump(t, diags)
		t.Fatalf("expected missing_required_field for retry.count")
	}
}

func TestLoadScenarioManifest_RetryCountZero(t *testing.T) {
	src := `name: x
tasks:
  - module: core.exec.run
    params: { cmd: "true" }
    retry:
      count: 0
      delay: 5s
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "value_out_of_range") {
		dump(t, diags)
		t.Fatalf("expected value_out_of_range for retry.count: 0")
	}
}

func TestLoadScenarioManifest_RetryBadDelay(t *testing.T) {
	src := `name: x
tasks:
  - module: core.exec.run
    params: { cmd: "true" }
    retry:
      count: 3
      delay: "not-a-duration"
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "duration_invalid") {
		dump(t, diags)
		t.Fatalf("expected duration_invalid for retry.delay")
	}
}

func TestLoadScenarioManifest_TimeoutBad(t *testing.T) {
	src := `name: x
tasks:
  - module: core.exec.run
    params: { cmd: "true" }
    timeout: "forever"
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "duration_invalid") {
		dump(t, diags)
		t.Fatalf("expected duration_invalid for timeout")
	}
}

// TestLoadScenarioManifest_DurationDaysSuffix — destiny duration fields accept the
// `<N>d` suffix per the `duration` convention (config.ParseDuration), unified with
// keeper.yml validation. Previously bare time.ParseDuration rejected `30d`.
func TestLoadScenarioManifest_DurationDaysSuffix(t *testing.T) {
	src := `name: x
tasks:
  - module: core.exec.run
    params: { cmd: "true" }
    timeout: "30d"
    retry:
      count: 3
      delay: "1d"
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "duration_invalid") {
		dump(t, diags)
		t.Fatalf("did not expect duration_invalid for <N>d-suffixed durations")
	}
}

// TestLoadScenarioManifest_DurationGoSyntaxStillValid — backward-compat: forms
// accepted by time.ParseDuration (`30s`/`5m`) remain valid.
func TestLoadScenarioManifest_DurationGoSyntaxStillValid(t *testing.T) {
	src := `name: x
tasks:
  - module: core.exec.run
    params: { cmd: "true" }
    timeout: "5m"
    retry:
      count: 3
      delay: "30s"
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "duration_invalid") {
		dump(t, diags)
		t.Fatalf("did not expect duration_invalid for Go-syntax durations")
	}
}

func TestLoadScenarioManifest_ApplyMissingDestiny(t *testing.T) {
	src := `name: x
tasks:
  - apply:
      input: {}
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	found := false
	for _, d := range diags {
		if d.Code == "missing_required_field" && d.YAMLPath == "$.tasks[0].apply.destiny" {
			found = true
		}
	}
	if !found {
		dump(t, diags)
		t.Fatalf("expected missing_required_field on apply.destiny")
	}
}

func TestLoadScenarioManifest_ApplyUnknownKey(t *testing.T) {
	src := `name: x
tasks:
  - apply:
      destiny: redis
      input: {}
      mystery: 42
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	found := false
	for _, d := range diags {
		if d.Code == "unknown_key" && d.YAMLPath == "$.tasks[0].apply.mystery" {
			found = true
		}
	}
	if !found {
		dump(t, diags)
		t.Fatalf("expected unknown_key on apply.mystery")
	}
}

func TestLoadScenarioManifest_IncludeBadName(t *testing.T) {
	src := `name: x
tasks:
  - include: ../escape.yml
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "name_invalid_format") {
		dump(t, diags)
		t.Fatalf("expected name_invalid_format for include with ../")
	}
}

func TestLoadScenarioManifest_BlockRecursive(t *testing.T) {
	// Recursive block: block inside block — an error in the nested task must bubble up.
	src := `name: x
tasks:
  - block:
      - block:
          - name: deep
            when: "true"
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "task_discriminator_missing") {
		dump(t, diags)
		t.Fatalf("expected task_discriminator_missing bubbled up from nested block")
	}
}

func TestLoadScenarioManifest_TaskWaitDeprecated(t *testing.T) {
	src := `name: x
tasks:
  - module: core.exec.run
    params: { cmd: "true" }
    wait: { condition: "true", timeout: 30s }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	found := false
	for _, d := range diags {
		if d.Code == "unknown_key" && d.YAMLPath == "$.tasks[0].wait" && d.Hint != "" {
			found = true
		}
	}
	if !found {
		dump(t, diags)
		t.Fatalf("expected task-level unknown_key for deprecated wait:")
	}
}

// --- BUG-1: expect does not apply to set/add (the cardinality assert is ONLY for
// modify/remove, ADR-057 §c). It was accepted silently → ignored at runtime
// (the operator expected a duplicate safeguard on add, but there is none). ---

func TestLoadScenarioManifest_StateChangesSetRejectsExpect(t *testing.T) {
	src := `name: x
tasks: []
state_changes:
  - set: redis_version
    value: "7.2"
    expect: one
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "unknown_key", "$.state_changes[0].expect") {
		dump(t, diags)
		t.Fatalf("expected unknown_key on $.state_changes[0].expect (expect неприменим к set)")
	}
}

func TestLoadScenarioManifest_StateChangesAddRejectsExpect(t *testing.T) {
	src := `name: x
tasks: []
state_changes:
  - add: redis_hosts
    value: { sid: x }
    expect: one
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "unknown_key", "$.state_changes[0].expect") {
		dump(t, diags)
		t.Fatalf("expected unknown_key on $.state_changes[0].expect (expect неприменим к add — дедуп делает on_conflict)")
	}
}

// --- BUG-2: a nested foreach in do: is outside the ADR-057 grammar (do carries
// CRUD verbs, not another loop). It must be caught at validation (lint-error),
// NOT at runtime (where it would fall into state_changes_apply_failed →
// error_locked AFTER apply on hosts). ---

func TestLoadScenarioManifest_StateChangesNestedForeachRejected(t *testing.T) {
	src := `name: x
tasks: []
state_changes:
  - foreach: "${ input.outer }"
    as: o
    do:
      - foreach: "${ o.inner }"
        as: i
        do:
          - add: redis_hosts
            value: "${ i }"
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "nested_foreach_unsupported") {
		dump(t, diags)
		t.Fatalf("★ expected nested_foreach_unsupported (lint-error, НЕ рантайм) on do-level foreach")
	}
}

// --- foreach.as reserved-binding collision: as: must not shadow a CEL-context
// name (input/register/...) or a local element binding (elem/key/value) →
// reserved_binding_name. ---

func TestLoadScenarioManifest_StateChangesForeachReservedAs(t *testing.T) {
	for _, name := range []string{"input", "register", "vars", "essence", "incarnation", "soulprint", "elem", "key", "value"} {
		src := `name: x
tasks: []
state_changes:
  - foreach: "${ input.replicas }"
    as: ` + name + `
    do:
      - add: redis_hosts
        value: "${ ` + name + ` }"
`
		_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
		if !hasCode(diags, "reserved_binding_name") {
			dump(t, diags)
			t.Fatalf("★ expected reserved_binding_name for foreach.as: %s", name)
		}
	}
}

// TestLoadScenarioManifest_StateChangesForeachNonReservedAsOK — an ordinary as-name
// does not trigger reserved_binding_name (guard against over-rejection).
func TestLoadScenarioManifest_StateChangesForeachNonReservedAsOK(t *testing.T) {
	src := `name: add_replicas
tasks: []
state_changes:
  - foreach: "${ input.replicas }"
    as: sid
    do:
      - add: redis_hosts
        value: "${ sid }"
        match: "elem == sid"
        on_conflict: skip
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "reserved_binding_name") {
		dump(t, diags)
		t.Fatalf("as: sid не должен триггерить reserved_binding_name")
	}
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("корректный foreach с as: sid не должен давать ошибок")
	}
}

// --- create: top-level flag of a starter scenario (multiple-create mechanism) ---

// TestLoadScenarioManifest_CreateFlagTrue — `create: true` parses into *bool
// (the scenario is declared as a starter/bootstrap-capable one; reading the flag
// is a layer above, keeper resolution of the create-set by artifact.Scenario.Create).
func TestLoadScenarioManifest_CreateFlagTrue(t *testing.T) {
	src := `name: create_cluster
create: true
tasks: []
`
	cfg, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("create: true должен быть валидным top-level ключом")
	}
	if cfg.Create == nil || *cfg.Create != true {
		t.Fatalf("cfg.Create = %v, want *true", cfg.Create)
	}
}

// TestLoadScenarioManifest_CreateFlagFalse — `create: false` is distinguishable from
// "unset" (an explicit opt-out from the create-set).
func TestLoadScenarioManifest_CreateFlagFalse(t *testing.T) {
	src := `name: add_user
create: false
tasks: []
`
	cfg, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("create: false должен быть валидным")
	}
	if cfg.Create == nil || *cfg.Create != false {
		t.Fatalf("cfg.Create = %v, want *false", cfg.Create)
	}
}

// TestLoadScenarioManifest_CreateFlagAbsent — a missing key: Create==nil
// (back-compat: an ordinary operational scenario does not silently become a create-starter).
func TestLoadScenarioManifest_CreateFlagAbsent(t *testing.T) {
	src := `name: restart
tasks: []
`
	cfg, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("отсутствие create: — валидно")
	}
	if cfg.Create != nil {
		t.Fatalf("cfg.Create = %v, want nil", cfg.Create)
	}
}

// TestLoadScenarioManifest_CreateFlagBadType — a non-bool `create:` value → type_mismatch
// (the key is known to a struct field, the decode phase catches the type mismatch).
func TestLoadScenarioManifest_CreateFlagBadType(t *testing.T) {
	src := `name: x
create: "yes"
tasks: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "type_mismatch") {
		dump(t, diags)
		t.Fatalf("create: \"yes\" должен дать type_mismatch (ожидается boolean)")
	}
	// `create:` must NOT be caught as unknown_key (it is known to a struct field).
	if hasCodeAt(diags, "unknown_key", "$.create") {
		dump(t, diags)
		t.Fatalf("create: не должен быть unknown_key (известное поле)")
	}
}

// --- from: list of source versions of an upgrade scenario (ADR-0068) ---

// TestLoadScenarioManifest_FromVersions — the top-level `from:` (a self-describing
// list of an upgrade scenario's source versions) parses with the strict walker
// WITHOUT unknown_key and fills ScenarioManifest.FromVersions (ADR-0068 §3). Without
// the FromVersions field the walker would reject `from` as unknown_key — that is the
// red without the fix.
func TestLoadScenarioManifest_FromVersions(t *testing.T) {
	src := `name: v2
from: ["v1.0.0", "v1.2.0"]
tasks: []
`
	cfg, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("top-level from: должен быть валидным ключом (ADR-0068)")
	}
	if hasCodeAt(diags, "unknown_key", "$.from") {
		dump(t, diags)
		t.Fatalf("from: не должен быть unknown_key (известное поле)")
	}
	want := []string{"v1.0.0", "v1.2.0"}
	if len(cfg.FromVersions) != len(want) {
		t.Fatalf("cfg.FromVersions = %v, want %v", cfg.FromVersions, want)
	}
	for i, v := range want {
		if cfg.FromVersions[i] != v {
			t.Fatalf("cfg.FromVersions[%d] = %q, want %q", i, cfg.FromVersions[i], v)
		}
	}
}

// TestLoadScenarioManifest_FromVersionsAbsent — a missing key: FromVersions==nil
// (an ordinary scenario without an upgrade role does not silently become one).
func TestLoadScenarioManifest_FromVersionsAbsent(t *testing.T) {
	src := `name: restart
tasks: []
`
	cfg, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("отсутствие from: — валидно")
	}
	if cfg.FromVersions != nil {
		t.Fatalf("cfg.FromVersions = %v, want nil", cfg.FromVersions)
	}
}

// TestLoadScenarioManifest_FromVersionsBadType — a scalar `from:` value instead of
// a list → type_mismatch (the key is known to a struct field, the decode phase catches the type).
func TestLoadScenarioManifest_FromVersionsBadType(t *testing.T) {
	src := `name: x
from: "v1.0.0"
tasks: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "type_mismatch") {
		dump(t, diags)
		t.Fatalf(`from: "v1.0.0" (скаляр вместо списка) должен дать type_mismatch`)
	}
	if hasCodeAt(diags, "unknown_key", "$.from") {
		dump(t, diags)
		t.Fatalf("from: не должен быть unknown_key (известное поле)")
	}
}
