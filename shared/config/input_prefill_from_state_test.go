package config

import (
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// TestResolveInputValues_PrefillFromStateNotLeaked — GUARD (инвариант a):
// `prefill_from_state` НЕ участвует в резолве эффективного input. Поле с этим
// ключом БЕЗ default и БЕЗ переданного значения отсутствует в эффективном input
// (prefill — операционный UI-hint, НЕ create-дефолт; incarnation.state не должен
// протечь в input-резолв). Ловит регресс «prefill_from_state незаметно стал
// дефолтом».
func TestResolveInputValues_PrefillFromStateNotLeaked(t *testing.T) {
	schema := schemaFromInput(t, `redis_version:
  type: string
  prefill_from_state: state.redis_version
`)
	// Ничего не передано: prefill_from_state не подставляет значение (в отличие
	// от default), и required не поднимается (ключ не делает поле обязательным).
	got, err := ResolveInputValues(schema, map[string]any{})
	if err != nil {
		t.Fatalf("ResolveInputValues: %v", err)
	}
	if _, present := got["redis_version"]; present {
		t.Fatalf("prefill_from_state протёк в эффективный input: redis_version=%#v (ожидалось отсутствие)", got["redis_version"])
	}
	if len(got) != 0 {
		t.Fatalf("эффективный input не пуст: %#v", got)
	}
}

// TestResolveInputValues_PrefillFromStateCoexistsWithDefault — `default` и
// `prefill_from_state` сосуществуют: default остаётся create-дефолтом (в merge),
// prefill_from_state в резолве не участвует (виден только form-prefill-эндпоинту).
func TestResolveInputValues_PrefillFromStateCoexistsWithDefault(t *testing.T) {
	schema := schemaFromInput(t, `tls_enabled:
  type: boolean
  default: false
  prefill_from_state: state.tls_enabled
`)
	got, err := ResolveInputValues(schema, map[string]any{})
	if err != nil {
		t.Fatalf("ResolveInputValues: %v", err)
	}
	want := map[string]any{"tls_enabled": false} // default смёржен; prefill не влияет
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%#v want=%#v", got, want)
	}
}

// TestValidatePrefillFromState_Accepts — валидные пути `state.<path>` проходят
// schema-валидацию без диагностик. Применим к любому type.
func TestValidatePrefillFromState_Accepts(t *testing.T) {
	cases := []string{
		`f:
  type: string
  prefill_from_state: state.redis_version
`,
		`f:
  type: object
  properties:
    x: { type: string }
  prefill_from_state: state.config.nested_field
`,
		`f:
  type: array
  items: { type: string }
  prefill_from_state: state.redis_users
`,
	}
	for _, c := range cases {
		diags := diagsForInput(t, c)
		for _, d := range diags {
			if d.Code == "input_prefill_from_state_invalid" {
				t.Fatalf("валидный prefill_from_state отвергнут: %s\n---\n%s", d.Message, c)
			}
		}
	}
}

// TestValidatePrefillFromState_Rejects — невалидные формы пути поднимают
// input_prefill_from_state_invalid (нет корня state / пустой / битый сегмент).
func TestValidatePrefillFromState_Rejects(t *testing.T) {
	cases := []string{
		`f:
  type: string
  prefill_from_state: redis_version
`, // нет корня state.
		`f:
  type: string
  prefill_from_state: state
`, // только корень, нет сегмента
		`f:
  type: string
  prefill_from_state: state.
`, // пустой сегмент
		`f:
  type: string
  prefill_from_state: incarnation.state.redis_version
`, // чужой корень (не state)
		`f:
  type: string
  prefill_from_state: state.Bad-Segment
`, // не snake_case сегмент
	}
	for _, c := range cases {
		if !hasDiagCode(diagsForInput(t, c), "input_prefill_from_state_invalid") {
			t.Fatalf("невалидный prefill_from_state НЕ отвергнут:\n---\n%s", c)
		}
	}
}

// TestPrefillFromState_KnownKey — `prefill_from_state` не поднимает unknown_key
// (зарегистрирован в inputSchemaKnownKeys).
func TestPrefillFromState_KnownKey(t *testing.T) {
	c := `f:
  type: string
  prefill_from_state: state.x
`
	for _, d := range diagsForInput(t, c) {
		if d.Code == "unknown_key" {
			t.Fatalf("prefill_from_state поднял unknown_key: %s", d.Message)
		}
	}
}

// diagsForInput парсит scenario с input-блоком и возвращает ВСЕ диагностики
// (включая ошибки — тест сам решает, что ожидать). Отличие от schemaFromInput:
// тот падает на любой error-диагностике.
func diagsForInput(t *testing.T, inputYAML string) []diag.Diagnostic {
	t.Helper()
	body := "name: t\ndescription: d\nstate_changes: {}\ntasks: []\ninput:\n" + indentBlock(inputYAML, "  ")
	_, _, diags, err := LoadScenarioManifestFromBytes("t.yml", []byte(body), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifestFromBytes: %v\n---\n%s", err, body)
	}
	return diags
}

func hasDiagCode(diags []diag.Diagnostic, code string) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}
