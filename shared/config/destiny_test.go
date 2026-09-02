package config

import (
	"path/filepath"
	"strings"
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

	// Smoke-check of deep recursive decode: input.users is type=object with
	// additional_properties→object (typed contract of the new destiny/redis, no legacy
	// action-DSL). Check the deepest enum: users → additional_properties →
	// properties["state"].enum.
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

	// Golden guards the key constraints of the typed contract.
	version := cfg.Input["version"]
	if version == nil || version.Pattern == "" {
		t.Errorf("input.version.pattern must be present")
	}

	// default_admin redesign (2026-06-30): top-level input.password REMOVED
	// (requirepass dropped from redis.conf, no more master password). Guard that it did
	// not come back: the password now lives per-user in the users map (each ACL user's
	// password field, secret:true), not as a separate top-level secret param.
	if cfg.Input["password"] != nil {
		t.Errorf("input.password must be absent after default_admin redesign (per-user password lives in input.users.*.password), got %#v", cfg.Input["password"])
	}
	// The password carrier is users → additional_properties → properties["password"]
	// (secret:true). New contract: default_admin/replica/monitoring arrive in the users
	// map like everyone else, each with its own resolved secret.
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
required_modules: [acme.haproxy, acme.myapp, "bad-no-dot", "ns.UPPER"]
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

// `required` is a bool, on the field it belongs to, at every level ([ADR-0086] §2).
// The list form — one statement about several properties, written beside them rather
// than on them — is what let one record be described two ways at once.
func TestLoadDestinyManifest_InputRequiredIsABool(t *testing.T) {
	// Top-level param.
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

	// Inside an object: the same spelling, on each property.
	src2 := `name: x
input:
  o:
    type: object
    properties:
      a: { type: string, required: true }
      b: { type: string, required: true }
`
	cfg2, _, diags2, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src2), ValidateOptions{})
	if diag.HasErrors(diags2) {
		dump(t, diags2)
		t.Fatalf("expected no errors for per-property `required: true`")
	}
	for _, name := range []string{"a", "b"} {
		if !cfg2.Input["o"].Properties[name].Required {
			t.Errorf("property %q did not decode its own required flag", name)
		}
	}
}

// The list form is refused wherever it is written, and the refusal says where the
// requiredness goes instead. Left merely undecoded it would be silent: a schema whose
// `required: [a, b]` is ignored declares nothing required and loads clean.
func TestLoadDestinyManifest_InputRequiredListIsRefused(t *testing.T) {
	for name, src := range map[string]string{
		"on a scalar": `name: x
input:
  s:
    type: string
    required: [a, b]
`,
		"on an object": `name: x
input:
  o:
    type: object
    required: [a, b]
    properties:
      a: { type: string }
      b: { type: string }
`,
	} {
		t.Run(name, func(t *testing.T) {
			_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
			if !hasCode(diags, RequiredListRemovedCode) {
				dump(t, diags)
				t.Fatalf("expected input_required_value_invalid for the list form")
			}
			for _, d := range diags {
				if d.Code == "input_required_value_invalid" && !strings.Contains(d.Hint, "required: true") {
					t.Errorf("the refusal does not say what to write instead: %q", d.Hint)
				}
			}
		})
	}
}

func TestLoadDestinyManifest_InputDeepNesting(t *testing.T) {
	// Recursion into items → object → properties → array → items.
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
	// `output:` uses the same input.md standard.
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
	// Bug 3: a deprecated top-level key must yield exactly one diagnostic
	// (with a hint from schemaValidateDestiny), not a duplicate from the reflect-walker.
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
	// Bug 2: `additional_properties: <schema>` must be validated recursively.
	// pattern on an integer inside the AP schema is an input_key_invalid_for_type error.
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
	// A scalar that is neither a bool nor the former list is a plain mistake, and keeps
	// the generic code — the list gets its own ([RequiredListRemovedCode]) because it
	// names a form that used to be correct.
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
	// Bug 1 (qa.2 BLOCKER): enum for type=array used to panic in equalScalar when
	// comparing []any via `==`. It must now emit input_enum_unsupported_for_type,
	// without a panic.
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
	// Strictly: no panic — already guaranteed by the fact we got this far.
	// Also confirm input_default_not_in_enum is NOT duplicated (the enum check is
	// skipped for composite types).
	if hasCode(diags, "input_default_not_in_enum") {
		dump(t, diags)
		t.Fatalf("input_default_not_in_enum must not fire when enum is unsupported for composite type")
	}
}

func TestLoadDestinyManifest_EnumUnsupportedForObject(t *testing.T) {
	// Symmetric check for type=object.
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
	// Bug 2 (qa.2 minor): default content recursion must catch mismatching leaf values
	// at 3+ nesting levels (array[object[array[integer]]]).
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
	// The YAML path must point at the specific leaf.
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

// TestLoadDestinyManifest_ValidateBlock — `validate:` is a first-class top-level
// key of destiny.yml (NIM-167, ADR-009 amendment 2026-07-26), decoded into
// DestinyManifest.Validate and checked by the SAME validator as scenario/covenant.
func TestLoadDestinyManifest_ValidateBlock(t *testing.T) {
	src := `name: redis
input:
  redis_type: { type: string, enum: [standalone, cluster] }
  cluster_nodes: { type: array, items: { type: string } }
validate:
  - that: "input.redis_type != 'cluster' || size(input.cluster_nodes) >= 3"
    message: "cluster requires at least 3 nodes"
`
	cfg, _, diags, err := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if err != nil {
		t.Fatalf("io error: %v", err)
	}
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("valid destiny validate: block must not raise diagnostics")
	}
	if len(cfg.Validate) != 1 {
		t.Fatalf("len(Validate) = %d, want 1", len(cfg.Validate))
	}
	if cfg.Validate[0].Message != "cluster requires at least 3 nodes" {
		t.Errorf("Validate[0].Message = %q", cfg.Validate[0].Message)
	}
}

// TestLoadDestinyManifest_ValidateBlockRejectsBadRule — the destiny block is held
// to the scenario grammar: `that` compiles input-only, `message` is required.
func TestLoadDestinyManifest_ValidateBlockRejectsBadRule(t *testing.T) {
	cases := []struct {
		name string
		src  string
		code string
	}{
		{
			name: "that references scenario scope",
			src: `name: redis
validate:
  - that: "register.probe.changed"
    message: "leaks scenario scope"
`,
			code: "validate_rule_invalid",
		},
		{
			name: "message missing",
			src: `name: redis
validate:
  - that: "input.port > 0"
`,
			code: "missing_required_field",
		},
		{
			name: "unknown key inside a rule",
			src: `name: redis
validate:
  - that: "input.port > 0"
    message: "positive"
    severity: warn
`,
			code: "unknown_key",
		},
		{
			name: "empty list",
			src: `name: redis
validate: []
`,
			code: "empty_value",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(tc.src), ValidateOptions{})
			if !hasCode(diags, tc.code) {
				dump(t, diags)
				t.Fatalf("expected %s", tc.code)
			}
		})
	}
}
