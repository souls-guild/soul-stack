package config

import (
	"path/filepath"
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
