package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// schemaFromInput парсит scenario с заданным input:-блоком и возвращает его
// InputSchemaMap (с корректно проставленным requiredKind через UnmarshalYAML).
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

// TestResolveInputValues_MergesDefaults — непереданные параметры с default
// подтягиваются; переданные сохраняются.
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
		"redis_version": "7.2.4",        // default смёржен
		"redis_socket":  "/custom.sock", // переданное сохранено
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%#v want=%#v", got, want)
	}
}

// TestResolveInputValues_RequiredMissing — required без default и без значения →
// ошибка.
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

// TestResolveInputValues_RequiredPassed — required с переданным значением → ок.
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

// TestResolveInputValues_EmptyStringIsAbsent — пустая строка для type=string без
// allow_empty трактуется как отсутствие → default подставляется.
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

// TestResolveInputValues_EmptyStringAllowed — allow_empty: true → пустая строка
// проходит как значение.
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

// TestResolveInputValues_PatternMismatch — переданное значение не матчит pattern.
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

// TestResolveInputValues_PatternMatch — валидное значение проходит pattern.
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

// TestResolveInputValues_ExprValueSkipsPattern — значение-выражение (${...}) не
// валидируется против pattern: финальная форма появится после render-фазы.
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

// TestResolveInputValues_TypeMismatch — переданное значение не соответствует type.
func TestResolveInputValues_TypeMismatch(t *testing.T) {
	schema := schemaFromInput(t, `replicas:
  type: integer
`)
	_, err := ResolveInputValues(schema, map[string]any{"replicas": "not-int"})
	if err == nil {
		t.Fatal("ожидалась ошибка несоответствия типа")
	}
}

// TestResolveInputValues_EnumValidated — значение вне enum → ошибка; внутри → ок.
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

// TestResolveInputValues_EnumIntegerBoundary — enum для type=integer (runtime).
// Существующий TestResolveInputValues_EnumValidated покрывает только string-enum;
// integer-enum идёт через equalScalar с int↔float-послаблением (goccy типизирует
// `42` как uint64). Граница: значение из набора проходит, значение вне — падает,
// а int↔float-эквивалент (42 ≡ 42.0) совпадает.
func TestResolveInputValues_EnumIntegerBoundary(t *testing.T) {
	schema := schemaFromInput(t, `replicas:
  type: integer
  enum: [1, 3, 5]
`)
	// Значение из набора (как uint64 от goccy) — проходит.
	if _, err := ResolveInputValues(schema, map[string]any{"replicas": uint64(3)}); err != nil {
		t.Errorf("3 входит в enum, но отвергнуто: %v", err)
	}
	// int↔float-эквивалент: 5.0 ≡ 5 (equalScalar-послабление).
	if _, err := ResolveInputValues(schema, map[string]any{"replicas": float64(5)}); err != nil {
		t.Errorf("5.0 эквивалентно 5 в enum, но отвергнуто: %v", err)
	}
	// Значение вне набора — падает.
	if _, err := ResolveInputValues(schema, map[string]any{"replicas": uint64(4)}); err == nil {
		t.Error("4 не входит в integer-enum, но ошибки нет")
	}
}

// TestResolveInputValues_ArrayItemsEnum — enum на items массива валидируется
// per-element (validateArrayItems рекурсивно вызывает validateValueAt для каждого
// элемента). Тесты nested object с enum/pattern есть, array items с ограничением —
// нет. Граница: все элементы в наборе → OK; один вне → падает с путём [i].
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

// TestResolveInputValues_ArrayItemsPattern — pattern на items массива
// валидируется per-element. Симметрично enum-кейсу: pattern применяется к каждой
// строке элемента; невалидный элемент → ошибка с индексом.
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

// TestResolveInputValues_DoesNotMutateProvided — provided не мутируется.
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

// TestResolveInputValues_UnknownKeyPassthrough — ключ без схемы пробрасывается
// (MVP не запрещает unknown input key).
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

// TestResolveInputValues_NilSchema — nil-схема: provided пробрасывается как есть.
func TestResolveInputValues_NilSchema(t *testing.T) {
	got, err := ResolveInputValues(nil, map[string]any{"x": "y"})
	if err != nil {
		t.Fatalf("ResolveInputValues: %v", err)
	}
	if got["x"] != "y" {
		t.Errorf("got=%#v", got)
	}
}

// TestResolveInputValues_EnumExprValueSkipped — значение-выражение (${...}) в
// enum-параметре не отвергается (симметрия с pattern: финальная форма появится
// после render-фазы, ответственность оператора). До фикса enum отвергал expr.
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

// TestResolveInputValues_FalsyNotAbsent — false/0/[]/пустой object как
// переданные значения НЕ подменяются дефолтом (isAbsentValue считает
// отсутствием только пустую строку без allow_empty). Закрепляет поведение
// против регрессии (qa coverage).
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

// aclUsersSchema — input-схема, эквивалентная scenario/add_acl_user (array of
// object с required-полями и pattern на вложенном поле). Граница доверия
// оператор→рендер: до фикса вся вложенная валидация пропускалась до CEL/shell.
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

// TestResolveInputValues_NestedValid — корректный список вложенных объектов
// проходит насквозь.
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

// TestResolveInputValues_NestedElementTypeMismatch — элемент array неверного
// типа (строка вместо object) → ошибка с путём элемента.
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

// TestResolveInputValues_NestedFieldTypeMismatch — поле object неверного типа
// (name: 123 вместо string) → ошибка с путём поля.
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

// TestResolveInputValues_NestedMissingRequired — элемент без обязательного
// поля acl → ошибка с понятным путём.
func TestResolveInputValues_NestedMissingRequired(t *testing.T) {
	schema := aclUsersSchema(t)
	_, err := ResolveInputValues(schema, map[string]any{
		"users": []any{
			map[string]any{"name": "app", "acl": "on"},
			map[string]any{"name": "app2"}, // нет acl
		},
	})
	if err == nil {
		t.Fatal("ожидалась ошибка: отсутствует обязательное поле acl")
	}
	if !strings.Contains(err.Error(), "users[1].acl") {
		t.Errorf("ошибка не содержит путь $.users[1].acl: %v", err)
	}
}

// TestResolveInputValues_NestedPatternMismatch — нарушение
// items.properties.name.pattern → ошибка с путём.
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

// TestResolveInputValues_NestedExprFieldSkipped — строка-выражение во вложенном
// pattern-поле освобождается от pattern на своём уровне.
func TestResolveInputValues_NestedExprFieldSkipped(t *testing.T) {
	schema := aclUsersSchema(t)
	_, err := ResolveInputValues(schema, map[string]any{
		"users": []any{map[string]any{"name": "${ user.name }", "acl": "on"}},
	})
	if err != nil {
		t.Fatalf("выражение во вложенном поле не должно валидироваться против pattern: %v", err)
	}
}

// TestResolveInputValues_NestedObjectMissingRequiredEmptyString — пустая строка
// в обязательном строковом поле без allow_empty трактуется как отсутствие.
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

// secretFieldRaw — заведомо «секретное» сырое значение, наличие которого в
// тексте ошибки означает утечку.
const secretFieldRaw = "s3cr3t-leak-canary"

// TestResolveInputValues_SecretValueMaskedOnType — secret-поле с невалидным
// типом: ошибка содержит <masked>, НЕ сырое значение.
func TestResolveInputValues_SecretValueMaskedOnType(t *testing.T) {
	schema := schemaFromInput(t, `redis_password:
  type: string
  secret: true
`)
	// integer вместо string — провал type-проверки на secret-поле.
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

// TestResolveInputValues_SecretValueMaskedOnPattern — secret-поле, нарушающее
// pattern: сырое значение замаскировано, pattern в ошибке остаётся (это схема,
// не секрет).
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

// TestResolveInputValues_SecretValueMaskedOnEnum — secret-поле вне enum: ни
// само значение, ни список допустимых (он тоже секрет) не утекают.
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

// TestResolveInputValues_NonSecretValueShown — не-secret поле: сырое значение
// остаётся в ошибке (диагностика нужна).
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

// TestResolveInputValues_NestedSecretFieldMasked — вложенное secret-поле
// (users[].secret) с битым значением маскируется; не-secret соседнее поле — нет.
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

// TestResolveInputValues_MinLengthTooShort — строка короче min_length → ошибка.
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

// TestResolveInputValues_MaxLengthTooLong — строка длиннее max_length → ошибка.
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

// TestResolveInputValues_LengthInRange — значение внутри [min_length, max_length]
// проходит.
func TestResolveInputValues_LengthInRange(t *testing.T) {
	schema := schemaFromInput(t, `name:
  type: string
  min_length: 3
  max_length: 10
`)
	if _, err := ResolveInputValues(schema, map[string]any{"name": "alice"}); err != nil {
		t.Fatalf("значение в диапазоне отвергнуто: %v", err)
	}
	// Граничные значения — длина ровно на min и на max — валидны.
	if _, err := ResolveInputValues(schema, map[string]any{"name": "abc"}); err != nil {
		t.Fatalf("значение длиной = min_length отвергнуто: %v", err)
	}
	if _, err := ResolveInputValues(schema, map[string]any{"name": "0123456789"}); err != nil {
		t.Fatalf("значение длиной = max_length отвергнуто: %v", err)
	}
}

// TestResolveInputValues_LengthInRunes — длина считается в Unicode-кодпоинтах,
// не байтах: 4 кириллических символа (8 UTF-8 байт) укладываются в max_length 4.
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

// TestResolveInputValues_LengthExprSkipped — значение-выражение освобождено от
// length-проверки: финальная длина появится только после render-фазы.
func TestResolveInputValues_LengthExprSkipped(t *testing.T) {
	schema := schemaFromInput(t, `token:
  type: string
  min_length: 16
`)
	if _, err := ResolveInputValues(schema, map[string]any{"token": "${ input.x }"}); err != nil {
		t.Fatalf("выражение не должно валидироваться против min_length: %v", err)
	}
}

// TestResolveInputValues_MinLengthSecretMasked — secret-поле, нарушающее
// min_length: сырое значение замаскировано (граница доверия + ADR-010).
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

// requiredWhenSchema — общая фикстура required_when-тестов: режим + поле shards,
// обязательное только при redis_type == 'cluster' (use-case redis-консолидации).
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

// TestResolveInputValues_RequiredWhenTruePredicateMissing — предикат истинен
// (redis_type=cluster) и поле отсутствует → required-ошибка (условная).
func TestResolveInputValues_RequiredWhenTruePredicateMissing(t *testing.T) {
	schema := requiredWhenSchema(t)
	_, err := ResolveInputValues(schema, map[string]any{"redis_type": "cluster"})
	if err == nil {
		t.Fatal("ожидалась ошибка: shards обязателен при redis_type=cluster")
	}
	if !strings.Contains(err.Error(), "shards") {
		t.Errorf("ошибка не называет параметр: %v", err)
	}
	// Узнаваемая форма required-ошибки — downstream-детект (checkdrift) ловит
	// безусловный и условный required единым матчингом подстроки.
	if !strings.Contains(err.Error(), "обязателен, но не передан и не имеет default") {
		t.Errorf("ошибка не несёт узнаваемую required-форму: %v", err)
	}
}

// TestResolveInputValues_RequiredWhenFalsePredicateMissing — предикат ложен
// (redis_type=standalone) и поле отсутствует → OK (условная обязательность не
// срабатывает).
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

// TestResolveInputValues_RequiredWhenTruePredicatePassed — предикат истинен и
// поле передано → OK.
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

// TestResolveInputValues_RequiredWhenDefaultMaterialized — предикат читает поле с
// default, материализованным merge-фазой (predicate eval ПОСЛЕ mergeInputDefaults).
// redis_type не передан → default standalone → предикат ложен → shards опционален.
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

// TestRequiredWhen_InvalidCELRejectedAtSchema — непарсимый/ссылающийся на имя вне
// input предикат отвергается schema-валидацией (input_required_when_invalid), а
// не на runtime.
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

// TestRequiredWhen_EmptyStringRejectedAtSchema — пустой required_when отвергается
// (бессмысленный предикат → footgun «никогда не обязателен»).
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
