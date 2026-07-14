package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// schemaFromInput parses a scenario with the given input: block and returns its
// InputSchemaMap (with requiredKind set correctly via UnmarshalYAML).
func schemaFromInput(t *testing.T, inputYAML string) InputSchemaMap {
	t.Helper()
	body := "name: t\ndescription: d\nstate_changes: {}\ntasks: []\ninput:\n" + indentBlock(inputYAML, "  ")
	scn, _, diags, err := LoadScenarioManifestFromBytes("t.yml", []byte(body), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifestFromBytes: %v\n---\n%s", err, body)
	}
	for _, d := range diags {
		if d.Level == diag.LevelError {
			t.Fatalf("schema diagnostics: %s\n---\n%s", d.Message, body)
		}
	}
	return scn.Input
}

func indentBlock(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		if l == "" {
			continue
		}
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n") + "\n"
}

// TestResolveInputValues_MergesDefaults — unpassed params with a default are pulled
// in; passed ones are preserved.
func TestResolveInputValues_MergesDefaults(t *testing.T) {
	schema := schemaFromInput(t, `redis_version:
  type: string
  default: "7.2.4"
redis_socket:
  type: string
  default: /var/run/redis.sock
`)
	got, err := ResolveInputValues(schema, map[string]any{"redis_socket": "/custom.sock"})
	if err != nil {
		t.Fatalf("ResolveInputValues: %v", err)
	}
	want := map[string]any{
		"redis_version": "7.2.4",        // default merged
		"redis_socket":  "/custom.sock", // passed value preserved
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%#v want=%#v", got, want)
	}
}

// TestResolveInputValues_RequiredMissing — required without a default and without a
// value → error.
func TestResolveInputValues_RequiredMissing(t *testing.T) {
	schema := schemaFromInput(t, `redis_password:
  type: string
  required: true
`)
	_, err := ResolveInputValues(schema, map[string]any{})
	if err == nil {
		t.Fatal("ожидалась ошибка на отсутствующий required input")
	}
	if !strings.Contains(err.Error(), "redis_password") {
		t.Errorf("ошибка не называет параметр: %v", err)
	}
}

// TestResolveInputValues_RequiredPassed — required with a passed value → ok.
func TestResolveInputValues_RequiredPassed(t *testing.T) {
	schema := schemaFromInput(t, `redis_password:
  type: string
  required: true
`)
	got, err := ResolveInputValues(schema, map[string]any{"redis_password": "vault:secret/x#k"})
	if err != nil {
		t.Fatalf("ResolveInputValues: %v", err)
	}
	if got["redis_password"] != "vault:secret/x#k" {
		t.Errorf("got=%#v", got)
	}
}

// TestResolveInputValues_EmptyStringIsAbsent — an empty string for type=string without
// allow_empty is treated as absent → the default is substituted.
func TestResolveInputValues_EmptyStringIsAbsent(t *testing.T) {
	schema := schemaFromInput(t, `name:
  type: string
  default: fallback
`)
	got, err := ResolveInputValues(schema, map[string]any{"name": ""})
	if err != nil {
		t.Fatalf("ResolveInputValues: %v", err)
	}
	if got["name"] != "fallback" {
		t.Errorf(`name = %#v, want "fallback" (пустая строка = отсутствие)`, got["name"])
	}
}

// TestResolveInputValues_EmptyStringAllowed — allow_empty: true → an empty string
// passes as a value.
func TestResolveInputValues_EmptyStringAllowed(t *testing.T) {
	schema := schemaFromInput(t, `note:
  type: string
  allow_empty: true
  default: fallback
`)
	got, err := ResolveInputValues(schema, map[string]any{"note": ""})
	if err != nil {
		t.Fatalf("ResolveInputValues: %v", err)
	}
	if got["note"] != "" {
		t.Errorf(`note = %#v, want "" (allow_empty)`, got["note"])
	}
}

// TestResolveInputValues_PatternMismatch — the passed value doesn't match the pattern.
func TestResolveInputValues_PatternMismatch(t *testing.T) {
	schema := schemaFromInput(t, `redis_version:
  type: string
  pattern: "^[0-9]+\\.[0-9]+\\.[0-9]+$"
`)
	_, err := ResolveInputValues(schema, map[string]any{"redis_version": "not-a-version"})
	if err == nil {
		t.Fatal("ожидалась ошибка несоответствия pattern")
	}
}

// TestResolveInputValues_PatternMatch — a valid value passes the pattern.
func TestResolveInputValues_PatternMatch(t *testing.T) {
	schema := schemaFromInput(t, `redis_version:
  type: string
  pattern: "^[0-9]+\\.[0-9]+\\.[0-9]+$"
`)
	got, err := ResolveInputValues(schema, map[string]any{"redis_version": "7.2.4"})
	if err != nil {
		t.Fatalf("ResolveInputValues: %v", err)
	}
	if got["redis_version"] != "7.2.4" {
		t.Errorf("got=%#v", got)
	}
}

// TestResolveInputValues_ExprValueSkipsPattern — an expression value (${...}) is not
// validated against the pattern: the final form appears after the render phase.
func TestResolveInputValues_ExprValueSkipsPattern(t *testing.T) {
	schema := schemaFromInput(t, `redis_version:
  type: string
  pattern: "^[0-9]+\\.[0-9]+\\.[0-9]+$"
`)
	got, err := ResolveInputValues(schema, map[string]any{"redis_version": "${ essence.version }"})
	if err != nil {
		t.Fatalf("выражение не должно валидироваться против pattern: %v", err)
	}
	if got["redis_version"] != "${ essence.version }" {
		t.Errorf("got=%#v", got)
	}
}

// TestResolveInputValues_TypeMismatch — the passed value doesn't match the type.
func TestResolveInputValues_TypeMismatch(t *testing.T) {
	schema := schemaFromInput(t, `replicas:
  type: integer
`)
	_, err := ResolveInputValues(schema, map[string]any{"replicas": "not-int"})
	if err == nil {
		t.Fatal("ожидалась ошибка несоответствия типа")
	}
}

// TestResolveInputValues_EnumValidated — a value outside the enum → error; inside → ok.
func TestResolveInputValues_EnumValidated(t *testing.T) {
	schema := schemaFromInput(t, `level:
  type: string
  enum: [debug, info, warn]
`)
	if _, err := ResolveInputValues(schema, map[string]any{"level": "trace"}); err == nil {
		t.Fatal("ожидалась ошибка значения вне enum")
	}
	if _, err := ResolveInputValues(schema, map[string]any{"level": "info"}); err != nil {
		t.Fatalf("значение из enum должно проходить: %v", err)
	}
}

// TestResolveInputValues_EnumIntegerBoundary — enum for type=integer (runtime).
// TestResolveInputValues_EnumValidated covers only a string-enum; an integer-enum goes
// through equalScalar with int↔float leniency (goccy types `42` as uint64). Boundary: a
// value from the set passes, a value outside fails, and an int↔float equivalent
// (42 ≡ 42.0) matches.
func TestResolveInputValues_EnumIntegerBoundary(t *testing.T) {
	schema := schemaFromInput(t, `replicas:
  type: integer
  enum: [1, 3, 5]
`)
	// A value from the set (as uint64 from goccy) — passes.
	if _, err := ResolveInputValues(schema, map[string]any{"replicas": uint64(3)}); err != nil {
		t.Errorf("3 входит в enum, но отвергнуто: %v", err)
	}
	// int↔float equivalent: 5.0 ≡ 5 (equalScalar leniency).
	if _, err := ResolveInputValues(schema, map[string]any{"replicas": float64(5)}); err != nil {
		t.Errorf("5.0 эквивалентно 5 в enum, но отвергнуто: %v", err)
	}
	// A value outside the set — fails.
	if _, err := ResolveInputValues(schema, map[string]any{"replicas": uint64(4)}); err == nil {
		t.Error("4 не входит в integer-enum, но ошибки нет")
	}
}

// TestResolveInputValues_ArrayItemsEnum — an enum on array items is validated
// per-element (validateArrayItems recursively calls validateValueAt for each element).
// There are tests for a nested object with enum/pattern, but not for array items with a
// constraint. Boundary: all elements in the set → OK; one outside → fails with path [i].
func TestResolveInputValues_ArrayItemsEnum(t *testing.T) {
	schema := schemaFromInput(t, `levels:
  type: array
  items:
    type: string
    enum: [debug, info, warn]
`)
	if _, err := ResolveInputValues(schema, map[string]any{"levels": []any{"info", "warn"}}); err != nil {
		t.Errorf("все элементы в enum, но отвергнуто: %v", err)
	}
	_, err := ResolveInputValues(schema, map[string]any{"levels": []any{"info", "trace"}})
	if err == nil {
		t.Fatal("элемент 'trace' вне items.enum, но ошибки нет")
	}
	if !strings.Contains(err.Error(), "[1]") {
		t.Errorf("ошибка должна указывать индекс невалидного элемента [1]: %v", err)
	}
}

// TestResolveInputValues_ArrayItemsPattern — a pattern on array items is validated
// per-element. Symmetric with the enum case: the pattern applies to each element
// string; an invalid element → an error with an index.
func TestResolveInputValues_ArrayItemsPattern(t *testing.T) {
	schema := schemaFromInput(t, `tags:
  type: array
  items:
    type: string
    pattern: "^[a-z]+$"
`)
	if _, err := ResolveInputValues(schema, map[string]any{"tags": []any{"alpha", "beta"}}); err != nil {
		t.Errorf("все элементы матчат pattern, но отвергнуто: %v", err)
	}
	_, err := ResolveInputValues(schema, map[string]any{"tags": []any{"alpha", "B3ta"}})
	if err == nil {
		t.Fatal("элемент 'B3ta' не матчит items.pattern, но ошибки нет")
	}
	if !strings.Contains(err.Error(), "[1]") {
		t.Errorf("ошибка должна указывать индекс [1]: %v", err)
	}
}

// TestResolveInputValues_DoesNotMutateProvided — provided is not mutated.
func TestResolveInputValues_DoesNotMutateProvided(t *testing.T) {
	schema := schemaFromInput(t, `suffix:
  type: string
  default: "!"
`)
	provided := map[string]any{"other": "x"}
	if _, err := ResolveInputValues(schema, provided); err != nil {
		t.Fatalf("ResolveInputValues: %v", err)
	}
	if _, leaked := provided["suffix"]; leaked {
		t.Fatalf("provided мутирован: %#v", provided)
	}
}

// TestResolveInputValues_UnknownKeyPassthrough — a key without a schema is passed
// through (MVP doesn't forbid an unknown input key).
func TestResolveInputValues_UnknownKeyPassthrough(t *testing.T) {
	schema := schemaFromInput(t, `known:
  type: string
  default: d
`)
	got, err := ResolveInputValues(schema, map[string]any{"extra": "passed"})
	if err != nil {
		t.Fatalf("ResolveInputValues: %v", err)
	}
	if got["extra"] != "passed" || got["known"] != "d" {
		t.Errorf("got=%#v", got)
	}
}

// TestResolveInputValues_NilSchema — nil schema: provided is passed through as-is.
func TestResolveInputValues_NilSchema(t *testing.T) {
	got, err := ResolveInputValues(nil, map[string]any{"x": "y"})
	if err != nil {
		t.Fatalf("ResolveInputValues: %v", err)
	}
	if got["x"] != "y" {
		t.Errorf("got=%#v", got)
	}
}

// TestResolveInputValues_EnumExprValueSkipped — an expression value (${...}) in an
// enum param is not rejected (symmetric with pattern: the final form appears after the
// render phase, the operator's responsibility). Before the fix enum rejected expr.
func TestResolveInputValues_EnumExprValueSkipped(t *testing.T) {
	schema := schemaFromInput(t, `level:
  type: string
  enum: [debug, info, warn]
`)
	got, err := ResolveInputValues(schema, map[string]any{"level": "${ essence.log_level }"})
	if err != nil {
		t.Fatalf("выражение не должно валидироваться против enum: %v", err)
	}
	if got["level"] != "${ essence.log_level }" {
		t.Errorf("got=%#v", got)
	}
}

// TestResolveInputValues_FalsyNotAbsent — false/0/[]/empty object as passed values are
// NOT replaced by the default (isAbsentValue counts only an empty string without
// allow_empty as absent). Locks the behavior against regression (qa coverage).
func TestResolveInputValues_FalsyNotAbsent(t *testing.T) {
	schema := schemaFromInput(t, `flag:
  type: boolean
  default: true
count:
  type: integer
  default: 5
tags:
  type: array
  default: [d]
  items:
    type: string
opts:
  type: object
  default: { k: d }
  properties:
    k:
      type: string
`)
	provided := map[string]any{
		"flag":  false,
		"count": 0,
		"tags":  []any{},
		"opts":  map[string]any{},
	}
	got, err := ResolveInputValues(schema, provided)
	if err != nil {
		t.Fatalf("ResolveInputValues: %v", err)
	}
	if got["flag"] != false {
		t.Errorf("flag = %#v, want false (не подмена дефолтом)", got["flag"])
	}
	if got["count"] != 0 {
		t.Errorf("count = %#v, want 0 (не подмена дефолтом)", got["count"])
	}
	if arr, ok := got["tags"].([]any); !ok || len(arr) != 0 {
		t.Errorf("tags = %#v, want [] (не подмена дефолтом)", got["tags"])
	}
	if obj, ok := got["opts"].(map[string]any); !ok || len(obj) != 0 {
		t.Errorf("opts = %#v, want {} (не подмена дефолтом)", got["opts"])
	}
}

// aclUsersSchema — an input schema equivalent to scenario/add_acl_user (array of object
// with required fields and a pattern on a nested field). The operator→render trust
// boundary: before the fix all nested validation was skipped up to CEL/shell.
func aclUsersSchema(t *testing.T) InputSchemaMap {
	t.Helper()
	return schemaFromInput(t, `users:
  type: array
  required: true
  items:
    type: object
    required: [name, acl]
    properties:
      name:
        type: string
        pattern: "^[A-Za-z0-9._-]+$"
      acl:
        type: string
`)
}

// TestResolveInputValues_NestedValid — a valid list of nested objects passes through.
func TestResolveInputValues_NestedValid(t *testing.T) {
	schema := aclUsersSchema(t)
	_, err := ResolveInputValues(schema, map[string]any{
		"users": []any{
			map[string]any{"name": "app", "acl": "on >secret ~* +@read"},
			map[string]any{"name": "ops_01", "acl": "on >x +@all"},
		},
	})
	if err != nil {
		t.Fatalf("валидный вложенный список должен проходить: %v", err)
	}
}

// TestResolveInputValues_NestedElementTypeMismatch — an array element of the wrong type
// (string instead of object) → an error with the element path.
func TestResolveInputValues_NestedElementTypeMismatch(t *testing.T) {
	schema := aclUsersSchema(t)
	_, err := ResolveInputValues(schema, map[string]any{
		"users": []any{"i-am-a-string"},
	})
	if err == nil {
		t.Fatal("ожидалась ошибка: элемент array не соответствует items.type")
	}
	if !strings.Contains(err.Error(), "users[0]") {
		t.Errorf("ошибка не содержит путь элемента: %v", err)
	}
}

// TestResolveInputValues_NestedFieldTypeMismatch — an object field of the wrong type
// (name: 123 instead of string) → an error with the field path.
func TestResolveInputValues_NestedFieldTypeMismatch(t *testing.T) {
	schema := aclUsersSchema(t)
	_, err := ResolveInputValues(schema, map[string]any{
		"users": []any{map[string]any{"name": 123, "acl": "on"}},
	})
	if err == nil {
		t.Fatal("ожидалась ошибка: поле object не соответствует properties.type")
	}
	if !strings.Contains(err.Error(), "users[0].name") {
		t.Errorf("ошибка не содержит путь поля: %v", err)
	}
}

// TestResolveInputValues_NestedMissingRequired — an element without the required field
// acl → an error with a clear path.
func TestResolveInputValues_NestedMissingRequired(t *testing.T) {
	schema := aclUsersSchema(t)
	_, err := ResolveInputValues(schema, map[string]any{
		"users": []any{
			map[string]any{"name": "app", "acl": "on"},
			map[string]any{"name": "app2"}, // no acl
		},
	})
	if err == nil {
		t.Fatal("ожидалась ошибка: отсутствует обязательное поле acl")
	}
	if !strings.Contains(err.Error(), "users[1].acl") {
		t.Errorf("ошибка не содержит путь $.users[1].acl: %v", err)
	}
}

// TestResolveInputValues_NestedPatternMismatch — a violation of
// items.properties.name.pattern → an error with a path.
func TestResolveInputValues_NestedPatternMismatch(t *testing.T) {
	schema := aclUsersSchema(t)
	_, err := ResolveInputValues(schema, map[string]any{
		"users": []any{map[string]any{"name": "bad name!", "acl": "on"}},
	})
	if err == nil {
		t.Fatal("ожидалась ошибка: name нарушает pattern")
	}
	if !strings.Contains(err.Error(), "users[0].name") {
		t.Errorf("ошибка не содержит путь поля: %v", err)
	}
}

// TestResolveInputValues_NestedExprFieldSkipped — an expression string in a nested
// pattern field is exempt from the pattern at its level.
func TestResolveInputValues_NestedExprFieldSkipped(t *testing.T) {
	schema := aclUsersSchema(t)
	_, err := ResolveInputValues(schema, map[string]any{
		"users": []any{map[string]any{"name": "${ user.name }", "acl": "on"}},
	})
	if err != nil {
		t.Fatalf("выражение во вложенном поле не должно валидироваться против pattern: %v", err)
	}
}

// TestResolveInputValues_NestedObjectMissingRequiredEmptyString — an empty string in a
// required string field without allow_empty is treated as absent.
func TestResolveInputValues_NestedObjectMissingRequiredEmptyString(t *testing.T) {
	schema := aclUsersSchema(t)
	_, err := ResolveInputValues(schema, map[string]any{
		"users": []any{map[string]any{"name": "app", "acl": ""}},
	})
	if err == nil {
		t.Fatal("ожидалась ошибка: пустая строка в required acl = отсутствие")
	}
	if !strings.Contains(err.Error(), "users[0].acl") {
		t.Errorf("ошибка не содержит путь поля: %v", err)
	}
}

// secretFieldRaw — a deliberately "secret" raw value whose presence in the error text
// means a leak.
const secretFieldRaw = "s3cr3t-leak-canary"

// TestResolveInputValues_SecretValueMaskedOnType — a secret field with an invalid type:
// the error contains <masked>, NOT the raw value.
func TestResolveInputValues_SecretValueMaskedOnType(t *testing.T) {
	schema := schemaFromInput(t, `redis_password:
  type: string
  secret: true
`)
	// integer instead of string — a type-check failure on a secret field.
	_, err := ResolveInputValues(schema, map[string]any{"redis_password": 12345})
	if err == nil {
		t.Fatal("ожидалась ошибка: integer не соответствует type string")
	}
	if strings.Contains(err.Error(), "12345") {
		t.Errorf("сырое значение secret-поля утекло в ошибку: %v", err)
	}
	if !strings.Contains(err.Error(), maskedSecretLiteral) {
		t.Errorf("ошибка не содержит маску %q: %v", maskedSecretLiteral, err)
	}
}

// TestResolveInputValues_SecretValueMaskedOnPattern — a secret field violating the
// pattern: the raw value is masked, the pattern stays in the error (it's the schema, not
// the secret).
func TestResolveInputValues_SecretValueMaskedOnPattern(t *testing.T) {
	schema := schemaFromInput(t, `redis_password:
  type: string
  secret: true
  pattern: "^[a-z]+$"
`)
	_, err := ResolveInputValues(schema, map[string]any{"redis_password": secretFieldRaw})
	if err == nil {
		t.Fatal("ожидалась ошибка: значение нарушает pattern")
	}
	if strings.Contains(err.Error(), secretFieldRaw) {
		t.Errorf("сырое значение secret-поля утекло в ошибку: %v", err)
	}
	if !strings.Contains(err.Error(), maskedSecretLiteral) {
		t.Errorf("ошибка не содержит маску %q: %v", maskedSecretLiteral, err)
	}
}

// TestResolveInputValues_SecretValueMaskedOnEnum — a secret field outside the enum:
// neither the value itself nor the list of allowed values (also secret) leaks.
func TestResolveInputValues_SecretValueMaskedOnEnum(t *testing.T) {
	schema := schemaFromInput(t, `tier_key:
  type: string
  secret: true
  enum: ["enum-secret-a", "enum-secret-b"]
`)
	_, err := ResolveInputValues(schema, map[string]any{"tier_key": secretFieldRaw})
	if err == nil {
		t.Fatal("ожидалась ошибка: значение вне enum")
	}
	if strings.Contains(err.Error(), secretFieldRaw) {
		t.Errorf("сырое значение secret-поля утекло в ошибку: %v", err)
	}
	if strings.Contains(err.Error(), "enum-secret-a") {
		t.Errorf("enum-литералы secret-поля утекли в ошибку: %v", err)
	}
	if !strings.Contains(err.Error(), maskedSecretLiteral) {
		t.Errorf("ошибка не содержит маску %q: %v", maskedSecretLiteral, err)
	}
}

// TestResolveInputValues_NonSecretValueShown — a non-secret field: the raw value stays
// in the error (diagnostics are needed).
func TestResolveInputValues_NonSecretValueShown(t *testing.T) {
	schema := schemaFromInput(t, `region:
  type: string
  pattern: "^[a-z]+$"
`)
	_, err := ResolveInputValues(schema, map[string]any{"region": "BadRegion1"})
	if err == nil {
		t.Fatal("ожидалась ошибка: значение нарушает pattern")
	}
	if !strings.Contains(err.Error(), "BadRegion1") {
		t.Errorf("значение не-secret поля должно быть в ошибке для диагностики: %v", err)
	}
	if strings.Contains(err.Error(), maskedSecretLiteral) {
		t.Errorf("не-secret поле не должно маскироваться: %v", err)
	}
}

// TestResolveInputValues_NestedSecretFieldMasked — a nested secret field
// (users[].secret) with a broken value is masked; a non-secret neighbor field is not.
func TestResolveInputValues_NestedSecretFieldMasked(t *testing.T) {
	schema := schemaFromInput(t, `users:
  type: array
  items:
    type: object
    properties:
      name:
        type: string
      token:
        type: string
        secret: true
        pattern: "^[a-z]+$"
`)
	_, err := ResolveInputValues(schema, map[string]any{
		"users": []any{map[string]any{"name": "app", "token": secretFieldRaw}},
	})
	if err == nil {
		t.Fatal("ожидалась ошибка: вложенное token нарушает pattern")
	}
	if strings.Contains(err.Error(), secretFieldRaw) {
		t.Errorf("сырое значение вложенного secret-поля утекло в ошибку: %v", err)
	}
	if !strings.Contains(err.Error(), maskedSecretLiteral) {
		t.Errorf("ошибка не содержит маску %q: %v", maskedSecretLiteral, err)
	}
	if !strings.Contains(err.Error(), "users[0].token") {
		t.Errorf("ошибка не содержит путь вложенного поля: %v", err)
	}
}

// TestResolveInputValues_MinLengthTooShort — a string shorter than min_length → error.
func TestResolveInputValues_MinLengthTooShort(t *testing.T) {
	schema := schemaFromInput(t, `secret_key:
  type: string
  min_length: 16
`)
	_, err := ResolveInputValues(schema, map[string]any{"secret_key": "short"})
	if err == nil {
		t.Fatal("ожидалась ошибка: значение короче min_length")
	}
	if !strings.Contains(err.Error(), "min_length") {
		t.Errorf("ошибка не упоминает min_length: %v", err)
	}
}

// TestResolveInputValues_MaxLengthTooLong — a string longer than max_length → error.
func TestResolveInputValues_MaxLengthTooLong(t *testing.T) {
	schema := schemaFromInput(t, `code:
  type: string
  max_length: 4
`)
	_, err := ResolveInputValues(schema, map[string]any{"code": "abcdef"})
	if err == nil {
		t.Fatal("ожидалась ошибка: значение длиннее max_length")
	}
	if !strings.Contains(err.Error(), "max_length") {
		t.Errorf("ошибка не упоминает max_length: %v", err)
	}
}

// TestResolveInputValues_LengthInRange — a value within [min_length, max_length] passes.
func TestResolveInputValues_LengthInRange(t *testing.T) {
	schema := schemaFromInput(t, `name:
  type: string
  min_length: 3
  max_length: 10
`)
	if _, err := ResolveInputValues(schema, map[string]any{"name": "alice"}); err != nil {
		t.Fatalf("значение в диапазоне отвергнуто: %v", err)
	}
	// Boundary values — length exactly at min and at max — are valid.
	if _, err := ResolveInputValues(schema, map[string]any{"name": "abc"}); err != nil {
		t.Fatalf("значение длиной = min_length отвергнуто: %v", err)
	}
	if _, err := ResolveInputValues(schema, map[string]any{"name": "0123456789"}); err != nil {
		t.Fatalf("значение длиной = max_length отвергнуто: %v", err)
	}
}

// TestResolveInputValues_LengthInRunes — length is counted in Unicode code points, not
// bytes: 4 Cyrillic characters (8 UTF-8 bytes) fit within max_length 4.
func TestResolveInputValues_LengthInRunes(t *testing.T) {
	schema := schemaFromInput(t, `word:
  type: string
  min_length: 4
  max_length: 4
`)
	if _, err := ResolveInputValues(schema, map[string]any{"word": "тест"}); err != nil {
		t.Fatalf("4-символьное слово (8 байт) должно пройти max_length 4: %v", err)
	}
}

// TestResolveInputValues_LengthExprSkipped — an expression value is exempt from the
// length check: the final length appears only after the render phase.
func TestResolveInputValues_LengthExprSkipped(t *testing.T) {
	schema := schemaFromInput(t, `token:
  type: string
  min_length: 16
`)
	if _, err := ResolveInputValues(schema, map[string]any{"token": "${ input.x }"}); err != nil {
		t.Fatalf("выражение не должно валидироваться против min_length: %v", err)
	}
}

// TestResolveInputValues_MinLengthSecretMasked — a secret field violating min_length:
// the raw value is masked (trust boundary + ADR-010).
func TestResolveInputValues_MinLengthSecretMasked(t *testing.T) {
	schema := schemaFromInput(t, `pass:
  type: string
  secret: true
  min_length: 16
`)
	_, err := ResolveInputValues(schema, map[string]any{"pass": secretFieldRaw[:5]})
	if err == nil {
		t.Fatal("ожидалась ошибка: secret короче min_length")
	}
	if strings.Contains(err.Error(), secretFieldRaw[:5]) {
		t.Errorf("сырое значение secret-поля утекло в ошибку: %v", err)
	}
	if !strings.Contains(err.Error(), maskedSecretLiteral) {
		t.Errorf("ошибка не содержит маску %q: %v", maskedSecretLiteral, err)
	}
}

// requiredWhenSchema — a shared fixture for required_when tests: a mode + a shards
// field, required only when redis_type == 'cluster' (redis-consolidation use case).
func requiredWhenSchema(t *testing.T) InputSchemaMap {
	t.Helper()
	return schemaFromInput(t, `redis_type:
  type: string
  enum: [standalone, cluster]
  default: standalone
shards:
  type: integer
  min: 1
  required_when: "input.redis_type == 'cluster'"
`)
}

// TestResolveInputValues_RequiredWhenTruePredicateMissing — the predicate is true
// (redis_type=cluster) and the field is absent → a (conditional) required error.
func TestResolveInputValues_RequiredWhenTruePredicateMissing(t *testing.T) {
	schema := requiredWhenSchema(t)
	_, err := ResolveInputValues(schema, map[string]any{"redis_type": "cluster"})
	if err == nil {
		t.Fatal("ожидалась ошибка: shards обязателен при redis_type=cluster")
	}
	if !strings.Contains(err.Error(), "shards") {
		t.Errorf("ошибка не называет параметр: %v", err)
	}
	// A recognizable required-error form — downstream detection (checkdrift) catches
	// both unconditional and conditional required with a single substring match.
	if !strings.Contains(err.Error(), "обязателен, но не передан и не имеет default") {
		t.Errorf("ошибка не несёт узнаваемую required-форму: %v", err)
	}
}

// TestResolveInputValues_RequiredWhenFalsePredicateMissing — the predicate is false
// (redis_type=standalone) and the field is absent → OK (conditional requiredness
// doesn't trigger).
func TestResolveInputValues_RequiredWhenFalsePredicateMissing(t *testing.T) {
	schema := requiredWhenSchema(t)
	got, err := ResolveInputValues(schema, map[string]any{"redis_type": "standalone"})
	if err != nil {
		t.Fatalf("standalone без shards должен пройти: %v", err)
	}
	if _, present := got["shards"]; present {
		t.Errorf("shards не передан — не должен появиться в эффективном input: %#v", got)
	}
}

// TestResolveInputValues_RequiredWhenTruePredicatePassed — the predicate is true and
// the field is passed → OK.
func TestResolveInputValues_RequiredWhenTruePredicatePassed(t *testing.T) {
	schema := requiredWhenSchema(t)
	got, err := ResolveInputValues(schema, map[string]any{"redis_type": "cluster", "shards": 3})
	if err != nil {
		t.Fatalf("cluster с shards должен пройти: %v", err)
	}
	if got["shards"] != uint64(3) && got["shards"] != 3 {
		t.Errorf("shards = %#v, want 3", got["shards"])
	}
}

// TestResolveInputValues_RequiredWhenDefaultMaterialized — the predicate reads a field
// with a default materialized by the merge phase (predicate eval AFTER
// mergeInputDefaults). redis_type not passed → default standalone → predicate false →
// shards optional.
func TestResolveInputValues_RequiredWhenDefaultMaterialized(t *testing.T) {
	schema := requiredWhenSchema(t)
	got, err := ResolveInputValues(schema, map[string]any{})
	if err != nil {
		t.Fatalf("дефолтный redis_type=standalone → shards опционален: %v", err)
	}
	if got["redis_type"] != "standalone" {
		t.Errorf("default redis_type не смёржен: %#v", got)
	}
}

// TestRequiredWhen_InvalidCELRejectedAtSchema — an unparseable predicate or one
// referencing a name outside input is rejected by schema validation
// (input_required_when_invalid), not at runtime.
func TestRequiredWhen_InvalidCELRejectedAtSchema(t *testing.T) {
	cases := []struct{ name, expr string }{
		{"syntax", "input.x =="},
		{"undeclared_essence", "essence.mode == 'x'"},
		{"undeclared_soulprint", "soulprint.self.os.family == 'debian'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := "name: t\ndescription: d\nstate_changes: {}\ntasks: []\ninput:\n" +
				indentBlock("f:\n  type: string\n  required_when: \""+tc.expr+"\"\n", "  ")
			_, _, diags, err := LoadScenarioManifestFromBytes("t.yml", []byte(body), ValidateOptions{})
			if err != nil {
				t.Fatalf("LoadScenarioManifestFromBytes: %v", err)
			}
			var found bool
			for _, d := range diags {
				if d.Code == "input_required_when_invalid" {
					found = true
				}
			}
			if !found {
				t.Errorf("ожидался диагностический код input_required_when_invalid для %q; diags=%v", tc.expr, diags)
			}
		})
	}
}

// TestRequiredWhen_EmptyStringRejectedAtSchema — an empty required_when is rejected (a
// meaningless predicate → the "never required" footgun).
func TestRequiredWhen_EmptyStringRejectedAtSchema(t *testing.T) {
	body := "name: t\ndescription: d\nstate_changes: {}\ntasks: []\ninput:\n" +
		indentBlock("f:\n  type: string\n  required_when: \"\"\n", "  ")
	_, _, diags, err := LoadScenarioManifestFromBytes("t.yml", []byte(body), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifestFromBytes: %v", err)
	}
	var found bool
	for _, d := range diags {
		if d.Code == "input_required_when_invalid" {
			found = true
		}
	}
	if !found {
		t.Errorf("ожидался input_required_when_invalid на пустой required_when; diags=%v", diags)
	}
}
