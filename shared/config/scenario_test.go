package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

func TestLoadScenarioManifest_Golden(t *testing.T) {
	// Локальная patched-копия (soul-lint/testdata/scenario-golden/) — оригинал
	// `examples/service/redis-cluster/scenario/create/main.yml` имеет
	// deviation от input.md (`type: object` без `properties:`); правка
	// examples — out of scope M1.2.c (delegation §«Что НЕ делаешь»).
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
	// Discriminator round-trip smoke: первая задача — module:.
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
	// Пустой tasks: [] валиден (no-op scenario).
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
			// Не должно быть дубля unknown_key на тот же путь.
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
	// Edge-case: одна валидная задача каждого вида.
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

// TestLoadScenarioManifest_BlockForbiddenKeys (guard #8) — module-специфичные
// ключи на block-задаче режутся fail-closed кодом <key>_on_block_invalid
// (destiny/tasks.md §6.5 их на block не упоминает). parallel: тоже отвергается.
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

// TestLoadScenarioManifest_BlockInheritedKeysOK — унаследованные ключи (when/
// where/vars/onchanges/onfail/serial/run_once/name) на block-задаче ВАЛИДНЫ (§6.5
// явно их допускает) — не должны давать <key>_on_block_invalid.
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
	// changed_when:/failed_when: принимают bool-литерал (force-shortcut) и
	// CEL-строку; невалидные типы (число/список) → type_mismatch.
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
	// sets — теперь mapping поле→выражение (orchestration.md §7.1); скаляр на
	// его месте → type_mismatch.
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
	// Старая []string-форма (sets: [a, b]) больше не валидна: sets — mapping.
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
	// state_changes: {} — валидно (restart-like, см. examples).
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

// --- Новая list-форма state_changes (пилот: set + add). ---

// TestLoadScenarioManifest_StateChangesTransitMapForm — ★ TRANSIT: старая
// map-форма `state_changes: { sets: {...} }` ВСЁ ЕЩЁ парсится (deprecated) —
// существующие сценарии на ней остаются зелёными. IsList=false, Sets заполнен.
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

// TestLoadScenarioManifest_StateChangesListForm — новая list-форма set+add
// парсится: IsList=true, Ops по порядку, value-объект сохранён, on_conflict/match.
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

// TestLoadScenarioManifest_StateChangesEmptyListOK — пустой `state_changes: []`
// валиден (state не меняется), IsList=true.
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
	// match: неприменим к set → unknown_key.
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

// TestLoadScenarioManifest_StateChangesModifyValid — modify с match+patch
// валидируется без ошибок (узкий match → без wide_match-warn).
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

// TestLoadScenarioManifest_StateChangesModifyMissingPatch — modify без patch →
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

// TestLoadScenarioManifest_StateChangesRemoveValid — remove с match (+expect) ок.
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

// TestLoadScenarioManifest_StateChangesForeachValid — foreach с as+do парсится:
// In несёт CEL-выражение коллекции, Do — вложенный add.
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

// TestLoadScenarioManifest_StateChangesForeachMissingAsDo — foreach без as/do.
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

// TestLoadScenarioManifest_StateChangesBadExpect — expect вне {one,at_most_one,
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

// TestLoadScenarioManifest_StateChangesWideMatchWarn — ★ предохранитель (a):
// modify/remove без match: ИЛИ с константно-истинным match → wide_match WARN
// (не ошибка, exit-code 0).
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

// TestLoadScenarioManifest_StateChangesDeprecatedMapWarn — ★ предохранитель (b):
// валидная старая map-форма даёт deprecated_form WARN; appends/modifies — ещё и
// noop_placeholder WARN. Не ошибка (dual-parse транзит).
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
	// on: arbitrary-scalar (не keeper) — должно быть enum_invalid.
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
	// id: на module-задаче без register — валидно.
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
	// Невалидный формат id: kebab / CamelCase / цифра-первой / пустая строка.
	// `redis-config` — явный кейс «дефис в середине = invalid» (id — snake_case,
	// не kebab), отдельно от BAD-ID, где невалидность ещё и из-за заглавных.
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
	// Guard-инвариант: id вместе с register — всегда ошибка (у задачи с register
	// адрес уже есть, id избыточен и двусмысленен).
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
	// pilot: id на block-задаче не поддерживается.
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
	// pilot: id на include-задаче не поддерживается.
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
	// Регресс: задача без id (как и всё существующее) валидна — id опционален.
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

// TestLoadScenarioManifest_DurationDaysSuffix — destiny duration-поля принимают
// суффикс `<N>d` per convention `duration` (config.ParseDuration), единой с
// keeper.yml-валидацией. Раньше голый time.ParseDuration отвергал `30d`.
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

// TestLoadScenarioManifest_DurationGoSyntaxStillValid — backward-compat: формы,
// принимаемые time.ParseDuration (`30s`/`5m`), остаются валидными.
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
	// Recursive block: block внутри block — ошибка во вложенной задаче должна
	// всплывать.
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

// --- BUG-1: expect неприменим к set/add (ассерт кратности ТОЛЬКО для
// modify/remove, ADR-057 §c). Принимался молча → игнорировался в рантайме
// (оператор ждал страховку от дубля на add, её там нет). ---

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

// --- BUG-2: вложенный foreach в do: вне грамматики ADR-057 (do несёт CRUD-
// глаголы, не повторный цикл). Должен ловиться на этапе валидации (lint-error),
// НЕ в рантайме (где упал бы в state_changes_apply_failed → error_locked ПОСЛЕ
// apply на хостах). ---

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

// --- foreach.as reserved-binding collision: as: не должен затенять имя
// CEL-контекста (input/register/...) или локальный биндинг элемента
// (elem/key/value) → reserved_binding_name. ---

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

// TestLoadScenarioManifest_StateChangesForeachNonReservedAsOK — обычное as-имя не
// триггерит reserved_binding_name (guard против over-rejection).
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
