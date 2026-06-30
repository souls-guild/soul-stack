package config

import (
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

func TestLoadDestinyManifest_Golden(t *testing.T) {
	path := filepath.FromSlash("../../examples/destiny/redis/destiny.yml")
	cfg, doc, diags, err := LoadDestinyManifest(path, ValidateOptions{})
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
		t.Fatalf("expected 0 errors on golden destiny example, got %d diagnostics", len(diags))
	}
	if cfg.Name != "redis" {
		t.Errorf("name: got %q want redis", cfg.Name)
	}

	// Smoke-check глубокого рекурсивного декода: input.users —
	// type=object с additional_properties→object (typed-контракт нового
	// destiny/redis, без устаревшего action-DSL). Проверяем самый глубокий
	// enum: users → additional_properties → properties["state"].enum.
	users := cfg.Input["users"]
	if users == nil {
		t.Fatal("input.users missing")
	}
	if users.Type != "object" {
		t.Errorf("input.users.type: got %q want object", users.Type)
	}
	ap, ok := users.AdditionalProperties.(*InputSchema)
	if !ok {
		t.Fatalf("input.users.additional_properties must decode to *InputSchema (schema-form), got %T", users.AdditionalProperties)
	}
	if ap.Type != "object" {
		t.Errorf("input.users.additional_properties.type: got %q want object", ap.Type)
	}
	state := ap.Properties["state"]
	if state == nil {
		t.Fatal("input.users.additional_properties.properties.state missing")
	}
	if state.Type != "string" {
		t.Errorf("input.users.…state.type: got %q want string", state.Type)
	}
	if len(state.Enum) == 0 {
		t.Errorf("input.users.…state.enum must be non-empty")
	}

	// Golden стережёт ключевые ограничения typed-контракта.
	version := cfg.Input["version"]
	if version == nil || version.Pattern == "" {
		t.Errorf("input.version.pattern must be present")
	}

	// РЕДИЗАЙН default_admin (2026-06-30): top-level input.password УДАЛЁН
	// (requirepass убран из redis.conf, главного пароля больше нет). Стережём,
	// что он не вернулся: пароль теперь живёт per-user в map users (поле password
	// каждого ACL-юзера, secret:true), а не отдельным top-level secret-параметром.
	if cfg.Input["password"] != nil {
		t.Errorf("input.password must be absent after default_admin redesign (per-user password lives in input.users.*.password), got %#v", cfg.Input["password"])
	}
	// Носитель пароля — users → additional_properties → properties["password"]
	// (secret:true). Это новый контракт: default_admin/replica/monitoring приходят
	// в map users как все, каждый со своим зарезолвленным секретом.
	userPassword := ap.Properties["password"]
	if userPassword == nil {
		t.Fatal("input.users.additional_properties.properties.password missing")
	}
	if !userPassword.Secret {
		t.Errorf("input.users.…password.secret must be true (per-user secret replaces top-level password)")
	}
	conf := cfg.Input["config"]
	if conf == nil {
		t.Fatal("input.config missing")
	}
	if apBool, ok := conf.AdditionalProperties.(bool); !ok || !apBool {
		t.Errorf("input.config.additional_properties must decode to bool true, got %T %v", conf.AdditionalProperties, conf.AdditionalProperties)
	}
}

func TestLoadDestinyManifest_MissingName(t *testing.T) {
	src := `description: no name here
input:
  x: { type: string }
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "missing_required_field") {
		dump(t, diags)
		t.Fatalf("expected missing_required_field for absent name")
	}
}

func TestLoadDestinyManifest_BadName(t *testing.T) {
	src := `name: Redis-Master
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "name_invalid_format") {
		dump(t, diags)
		t.Fatalf("expected name_invalid_format")
	}
}

func TestLoadDestinyManifest_DeprecatedKeys(t *testing.T) {
	cases := []string{"tasks", "steps", "vars", "version", "templates", "tests"}
	for _, key := range cases {
		key := key
		t.Run(key, func(t *testing.T) {
			src := "name: redis\n" + key + ": foo\n"
			_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
			found := false
			for _, d := range diags {
				if d.Code == "unknown_key" && d.Hint != "" && d.YAMLPath == "$."+key {
					found = true
					break
				}
			}
			if !found {
				dump(t, diags)
				t.Fatalf("expected unknown_key with hint for deprecated key %q", key)
			}
		})
	}
}

func TestLoadDestinyManifest_RequiredModuleFormat(t *testing.T) {
	src := `name: redis
required_modules: [wb.haproxy, wb.myapp, "bad-no-dot", "ns.UPPER"]
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	count := 0
	for _, d := range diags {
		if d.Code == "required_module_invalid_format" {
			count++
		}
	}
	if count != 2 {
		dump(t, diags)
		t.Fatalf("expected 2 required_module_invalid_format diagnostics, got %d", count)
	}
}

func TestLoadDestinyManifest_InputRequiredAmbiguity(t *testing.T) {
	// Параметр верхнего уровня — required: true (bool).
	src1 := `name: x
input:
  foo:
    type: string
    required: true
`
	cfg, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src1), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected no errors for `required: true` on string param")
	}
	if cfg.Input["foo"].Required != true {
		t.Fatalf("required bool not decoded")
	}
	if len(cfg.Input["foo"].RequiredProps) != 0 {
		t.Fatalf("RequiredProps must be empty for bool form")
	}

	// type=object — required: [name1, name2] (list).
	src2 := `name: x
input:
  o:
    type: object
    properties:
      a: { type: string }
      b: { type: string }
    required: [a, b]
`
	cfg2, _, diags2, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src2), ValidateOptions{})
	if diag.HasErrors(diags2) {
		dump(t, diags2)
		t.Fatalf("expected no errors for `required: [a, b]` on object param")
	}
	rp := cfg2.Input["o"].RequiredProps
	if len(rp) != 2 || rp[0] != "a" || rp[1] != "b" {
		t.Fatalf("RequiredProps not decoded: %#v", rp)
	}
}

func TestLoadDestinyManifest_InputRequiredListOnNonObject(t *testing.T) {
	src := `name: x
input:
  s:
    type: string
    required: [a, b]
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "input_key_invalid_for_type") {
		dump(t, diags)
		t.Fatalf("expected input_key_invalid_for_type when `required` is list on type=string")
	}
}

func TestLoadDestinyManifest_InputDeepNesting(t *testing.T) {
	// Рекурсия в items → object → properties → array → items.
	src := `name: x
input:
  outer:
    type: array
    items:
      type: object
      properties:
        nested:
          type: array
          items:
            type: string
            format: bogus
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "input_format_invalid") {
		dump(t, diags)
		t.Fatalf("expected input_format_invalid bubbled up from deep nesting")
	}
}

func TestLoadDestinyManifest_DefaultTypeMismatch(t *testing.T) {
	src := `name: x
input:
  age:
    type: integer
    default: "thirty"
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "input_default_type_mismatch") {
		dump(t, diags)
		t.Fatalf("expected input_default_type_mismatch")
	}
}

func TestLoadDestinyManifest_MinExclusiveMinConflict(t *testing.T) {
	src := `name: x
input:
  n:
    type: integer
    min: 1
    exclusive_min: 0
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "input_min_conflict") {
		dump(t, diags)
		t.Fatalf("expected input_min_conflict")
	}
}

func TestLoadDestinyManifest_PatternFormatWarn(t *testing.T) {
	src := `name: x
input:
  host:
    type: string
    pattern: "^[a-z]+$"
    format: hostname
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "input_pattern_format_conflict") {
		dump(t, diags)
		t.Fatalf("expected input_pattern_format_conflict warning")
	}
}

func TestLoadDestinyManifest_AllowEmptyWithMinLength(t *testing.T) {
	src := `name: x
input:
  s:
    type: string
    allow_empty: true
    min_length: 1
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "input_allow_empty_min_length_conflict") {
		dump(t, diags)
		t.Fatalf("expected input_allow_empty_min_length_conflict warning")
	}
}

func TestLoadDestinyManifest_OutputSymmetric(t *testing.T) {
	// `output:` использует тот же стандарт input.md.
	src := `name: x
output:
  result:
    type: string
    enum: [ok, fail]
`
	cfg, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected no errors on valid output:")
	}
	if cfg.Output["result"].Type != "string" {
		t.Fatalf("output decoded wrong: %#v", cfg.Output["result"])
	}
}

func TestLoadDestinyManifest_UnknownTopKey(t *testing.T) {
	src := `name: x
mystery: 42
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "unknown_key") {
		dump(t, diags)
		t.Fatalf("expected unknown_key for non-deprecated unknown top-level field")
	}
}

func TestLoadDestinyManifest_EmptyName(t *testing.T) {
	src := `name: ""
description: empty string is invalid format, not missing key
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "name_invalid_format") {
		dump(t, diags)
		t.Fatalf("expected name_invalid_format for empty name")
	}
	if hasCode(diags, "missing_required_field") {
		dump(t, diags)
		t.Fatalf("must not emit missing_required_field when key present with empty string")
	}
}

func TestLoadDestinyManifest_NullName(t *testing.T) {
	src := "name:\n"
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "name_invalid_format") {
		dump(t, diags)
		t.Fatalf("expected name_invalid_format for null name")
	}
}

func TestLoadDestinyManifest_DeprecatedKeyNoDuplicate(t *testing.T) {
	// Bug 3: deprecated top-level ключ должен дать ровно одну диагностику
	// (с hint от schemaValidateDestiny), а не дубль из reflect-walker-а.
	src := `name: x
tasks: foo
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	count := 0
	for _, d := range diags {
		if d.Code == "unknown_key" && d.YAMLPath == "$.tasks" {
			count++
		}
	}
	if count != 1 {
		dump(t, diags)
		t.Fatalf("expected exactly 1 unknown_key diag for deprecated `tasks`, got %d", count)
	}
}

func TestLoadDestinyManifest_AdditionalPropertiesNestedSchema(t *testing.T) {
	// Bug 2: `additional_properties: <schema>` должен рекурсивно валидироваться.
	// pattern на integer внутри AP-схемы — ошибка input_key_invalid_for_type.
	src := `name: x
input:
  m:
    type: object
    properties: {}
    additional_properties:
      type: integer
      pattern: "^x$"
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "input_key_invalid_for_type") {
		dump(t, diags)
		t.Fatalf("expected input_key_invalid_for_type inside additional_properties schema")
	}
}

func TestLoadDestinyManifest_RequiredBadValue(t *testing.T) {
	// Bug 4: `required:` с не-bool / не-sequence значением должно поднять
	// input_required_value_invalid.
	src := `name: x
input:
  s:
    type: string
    required: "blabla"
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "input_required_value_invalid") {
		dump(t, diags)
		t.Fatalf("expected input_required_value_invalid for `required: \"blabla\"`")
	}
}

func TestLoadDestinyManifest_DefaultNotInEnum(t *testing.T) {
	src := `name: x
input:
  mode:
    type: string
    enum: [a, b]
    default: c
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "input_default_not_in_enum") {
		dump(t, diags)
		t.Fatalf("expected input_default_not_in_enum")
	}
}

func TestLoadDestinyManifest_DefaultArrayElementMismatch(t *testing.T) {
	src := `name: x
input:
  ports:
    type: array
    items:
      type: integer
    default: [1, 2, "x"]
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "input_default_type_mismatch") {
		dump(t, diags)
		t.Fatalf("expected input_default_type_mismatch for array element mismatching items.type")
	}
}

func TestLoadDestinyManifest_DefaultObjectFieldMismatch(t *testing.T) {
	src := `name: x
input:
  cfg:
    type: object
    properties:
      port:
        type: integer
    default:
      port: "not-int"
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "input_default_type_mismatch") {
		dump(t, diags)
		t.Fatalf("expected input_default_type_mismatch for object field mismatching properties.<name>.type")
	}
}

func TestLoadDestinyManifest_EnumUnsupportedForArray(t *testing.T) {
	// Bug 1 (qa.2 BLOCKER): enum для type=array раньше паниковал в equalScalar
	// при сравнении []any через `==`. Должен теперь выдавать
	// input_enum_unsupported_for_type, без panic.
	src := `name: ok
input:
  pairs:
    type: array
    items: { type: integer }
    enum:
      - [1, 2]
      - [3, 4]
    default: [5, 6]
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "input_enum_unsupported_for_type") {
		dump(t, diags)
		t.Fatalf("expected input_enum_unsupported_for_type for enum on type=array")
	}
	// Strictly: no panic — это уже гарантируется тем, что мы досюда добрались.
	// Параллельно убедимся, что input_default_not_in_enum НЕ дублируется
	// (поскольку enum-чек пропускается для composite).
	if hasCode(diags, "input_default_not_in_enum") {
		dump(t, diags)
		t.Fatalf("input_default_not_in_enum must not fire when enum is unsupported for composite type")
	}
}

func TestLoadDestinyManifest_EnumUnsupportedForObject(t *testing.T) {
	// Симметричная проверка для type=object.
	src := `name: ok
input:
  cfg:
    type: object
    properties:
      a: { type: integer }
    enum:
      - { a: 1 }
      - { a: 2 }
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "input_enum_unsupported_for_type") {
		dump(t, diags)
		t.Fatalf("expected input_enum_unsupported_for_type for enum on type=object")
	}
}

func TestLoadDestinyManifest_DefaultDeepNestedMismatch(t *testing.T) {
	// Bug 2 (qa.2 minor): default content recursion должен ловить mismatching
	// leaf-значения на 3+ уровнях вложенности (array[object[array[integer]]]).
	src := `name: ok
input:
  matrix:
    type: array
    items:
      type: object
      properties:
        rows:
          type: array
          items: { type: integer }
    default:
      - rows: [1, 2, "BROKEN"]
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "input_default_type_mismatch") {
		dump(t, diags)
		t.Fatalf("expected input_default_type_mismatch at leaf depth=3")
	}
	// YAML-path должен указывать на конкретный leaf.
	want := "$.input.matrix.default[0].rows[2]"
	found := false
	for _, d := range diags {
		if d.Code == "input_default_type_mismatch" && d.YAMLPath == want {
			found = true
			break
		}
	}
	if !found {
		dump(t, diags)
		t.Fatalf("expected YAMLPath %q on deep-nested default mismatch", want)
	}
}

func TestLoadDestinyManifest_ParamNameInvalid(t *testing.T) {
	src := `name: x
input:
  with.dot:
    type: string
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "input_param_name_invalid") {
		dump(t, diags)
		t.Fatalf("expected input_param_name_invalid for `with.dot`")
	}
}
