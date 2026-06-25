package config

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// hasWarn — есть ли WARNING-диагностика с данным кодом (hasCode не различает level).
func hasWarn(ds []diag.Diagnostic, code string) bool {
	for _, d := range ds {
		if d.Code == code && d.Level == diag.LevelWarning {
			return true
		}
	}
	return false
}

// TestForm_Valid — валидный `form:` с двумя секциями, все поля ∈ input, все
// покрыты, key уникальны → нет ни одной диагностики (errors+warnings).
func TestForm_Valid(t *testing.T) {
	src := `name: x
input:
  redis_password: { type: string, secret: true }
  tls_enabled: { type: boolean, default: false }
  tls_port: { type: integer, default: 6379 }
form:
  sections:
    - key: connection
      title: "Подключение"
      description: "Сетевые параметры"
      collapsed: false
      fields:
        - { name: tls_enabled, label: "TLS" }
        - { name: tls_port }
    - key: secrets
      title: "Секреты"
      collapsed: true
      fields:
        - { name: redis_password, label: "Пароль Redis" }
tasks: []
`
	cfg, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if len(diags) != 0 {
		dump(t, diags)
		t.Fatalf("expected zero diagnostics on valid form, got %d", len(diags))
	}
	if cfg.Form == nil || len(cfg.Form.Sections) != 2 {
		t.Fatalf("Form = %#v, want 2 sections", cfg.Form)
	}
	if cfg.Form.Sections[1].Key != "secrets" || !cfg.Form.Sections[1].Collapsed {
		t.Errorf("section[1] = %#v, want key=secrets collapsed=true", cfg.Form.Sections[1])
	}
	if cfg.Form.Sections[0].Fields[0].Label != "TLS" {
		t.Errorf("field label = %q, want TLS", cfg.Form.Sections[0].Fields[0].Label)
	}
}

// TestForm_FieldUnknown — field.name, которого нет в input: → form_field_unknown ERROR.
func TestForm_FieldUnknown(t *testing.T) {
	src := `name: x
input:
  tls_enabled: { type: boolean }
form:
  sections:
    - key: s1
      fields:
        - { name: tls_enabled }
        - { name: nonexistent }
tasks: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "form_field_unknown", "$.form.sections[0].fields[1].name") {
		dump(t, diags)
		t.Fatalf("expected form_field_unknown on nonexistent field")
	}
}

// TestForm_FieldDuplicate — одно имя поля в >1 секции → form_field_duplicate ERROR.
func TestForm_FieldDuplicate(t *testing.T) {
	src := `name: x
input:
  a: { type: string }
  b: { type: string }
form:
  sections:
    - key: s1
      fields:
        - { name: a }
    - key: s2
      fields:
        - { name: b }
        - { name: a }
tasks: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "form_field_duplicate") {
		dump(t, diags)
		t.Fatalf("expected form_field_duplicate when field a appears twice")
	}
}

// TestForm_DuplicateSectionKey — повтор section.key → duplicate_key ERROR.
func TestForm_DuplicateSectionKey(t *testing.T) {
	src := `name: x
input:
  a: { type: string }
  b: { type: string }
form:
  sections:
    - key: dup
      fields:
        - { name: a }
    - key: dup
      fields:
        - { name: b }
tasks: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "duplicate_key") {
		dump(t, diags)
		t.Fatalf("expected duplicate_key on repeated section.key")
	}
}

// TestForm_Uncovered — поле input без секции → form_field_uncovered WARNING (НЕ error):
// форма валидна, прогон не падает; новое input-поле не ломает form.
func TestForm_Uncovered(t *testing.T) {
	src := `name: x
input:
  covered: { type: string }
  orphan: { type: string }
form:
  sections:
    - key: s1
      fields:
        - { name: covered }
tasks: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("uncovered field must NOT be an error (forward-compat)")
	}
	if !hasWarn(diags, "form_field_uncovered") {
		dump(t, diags)
		t.Fatalf("expected form_field_uncovered WARNING on orphan field")
	}
}

// TestForm_EmptyLabel — label: "" → form_field_empty_label WARNING (НЕ error):
// fallback на description/имя.
func TestForm_EmptyLabel(t *testing.T) {
	src := `name: x
input:
  a: { type: string }
form:
  sections:
    - key: s1
      fields:
        - { name: a, label: "" }
tasks: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("empty label must NOT be an error")
	}
	if !hasWarn(diags, "form_field_empty_label") {
		dump(t, diags)
		t.Fatalf("expected form_field_empty_label WARNING")
	}
}

// TestForm_MissingSectionKey — секция без key → missing_required_field ERROR.
func TestForm_MissingSectionKey(t *testing.T) {
	src := `name: x
input:
  a: { type: string }
form:
  sections:
    - title: "no key"
      fields:
        - { name: a }
tasks: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "missing_required_field", "$.form.sections[0].key") {
		dump(t, diags)
		t.Fatalf("expected missing_required_field on section without key")
	}
}

// TestForm_BadSectionKeyFormat — невалидный формат key → name_invalid_format ERROR.
func TestForm_BadSectionKeyFormat(t *testing.T) {
	src := `name: x
input:
  a: { type: string }
form:
  sections:
    - key: "Has Spaces"
      fields:
        - { name: a }
tasks: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "name_invalid_format") {
		dump(t, diags)
		t.Fatalf("expected name_invalid_format on key with spaces")
	}
}

// TestForm_UnknownKeyInSection — лишний ключ в секции → unknown_key ERROR, ровно один
// (reflect-walker подавлен formLayoutType, без дубля).
func TestForm_UnknownKeyInSection(t *testing.T) {
	src := `name: x
input:
  a: { type: string }
form:
  sections:
    - key: s1
      bogus: true
      fields:
        - { name: a }
tasks: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if n := countCode(diags, "unknown_key"); n != 1 {
		dump(t, diags)
		t.Fatalf("expected exactly 1 unknown_key (walker suppressed), got %d", n)
	}
}

// TestForm_Absent — нет ключа form: → Form==nil, ноль form-диагностик (forward-compat).
func TestForm_Absent(t *testing.T) {
	src := `name: x
input:
  a: { type: string }
tasks: []
`
	cfg, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("absent form must not produce errors")
	}
	if cfg.Form != nil {
		t.Fatalf("Form = %#v, want nil when form: absent", cfg.Form)
	}
	if hasCode(diags, "form_field_uncovered") {
		t.Fatalf("uncovered must not fire without a form: block")
	}
}

// TestForm_ShowWhen_Valid — show_when над input.* на секции И поле → нет
// диагностик; строки попадают в распарсенную форму как есть (eval — client-side).
func TestForm_ShowWhen_Valid(t *testing.T) {
	src := `name: x
input:
  tls_enabled: { type: boolean, default: false }
  tls_port: { type: integer, default: 6379 }
form:
  sections:
    - key: tls
      title: "TLS"
      show_when: "input.tls_enabled"
      fields:
        - { name: tls_enabled, label: "Включить TLS" }
        - { name: tls_port, show_when: "input.tls_enabled" }
tasks: []
`
	cfg, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if len(diags) != 0 {
		dump(t, diags)
		t.Fatalf("expected zero diagnostics on valid show_when, got %d", len(diags))
	}
	if cfg.Form == nil || cfg.Form.Sections[0].ShowWhen != "input.tls_enabled" {
		t.Fatalf("section show_when not parsed: %#v", cfg.Form)
	}
	if cfg.Form.Sections[0].Fields[1].ShowWhen != "input.tls_enabled" {
		t.Errorf("field show_when not parsed: %#v", cfg.Form.Sections[0].Fields[1])
	}
}

// TestForm_ShowWhen_EssenceRef — show_when ссылается на essence.* (вне input-only
// sandbox) → form_show_when_invalid ERROR (undeclared-reference compile-ошибка).
func TestForm_ShowWhen_FieldEssenceRef(t *testing.T) {
	src := `name: x
input:
  a: { type: string }
form:
  sections:
    - key: s1
      fields:
        - { name: a, show_when: "essence.tls.enabled" }
tasks: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "form_show_when_invalid", "$.form.sections[0].fields[0].show_when") {
		dump(t, diags)
		t.Fatalf("expected form_show_when_invalid on essence-ref field show_when")
	}
}

// TestForm_ShowWhen_SectionEssenceRef — то же на уровне секции.
func TestForm_ShowWhen_SectionEssenceRef(t *testing.T) {
	src := `name: x
input:
  a: { type: string }
form:
  sections:
    - key: s1
      show_when: "soulprint.self.os.family == 'debian'"
      fields:
        - { name: a }
tasks: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "form_show_when_invalid", "$.form.sections[0].show_when") {
		dump(t, diags)
		t.Fatalf("expected form_show_when_invalid on soulprint-ref section show_when")
	}
}

// TestForm_ShowWhen_Empty — show_when: "" → form_show_when_invalid ERROR
// (бессмысленный «никогда не видимо»; симметрия с пустым required_when).
func TestForm_ShowWhen_Empty(t *testing.T) {
	src := `name: x
input:
  a: { type: string }
form:
  sections:
    - key: s1
      fields:
        - { name: a, show_when: "" }
tasks: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "form_show_when_invalid") {
		dump(t, diags)
		t.Fatalf("expected form_show_when_invalid on empty show_when")
	}
}

// TestForm_PlaceholderHint_Valid — placeholder/hint парсятся, непустые → нет
// диагностик; omitempty-семантика: отсутствие ключей не эмитит ничего.
func TestForm_PlaceholderHint_Valid(t *testing.T) {
	src := `name: x
input:
  port: { type: integer }
  host: { type: string }
form:
  sections:
    - key: s1
      fields:
        - { name: port, placeholder: "6379", hint: "TCP-порт Redis" }
        - { name: host }
tasks: []
`
	cfg, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if len(diags) != 0 {
		dump(t, diags)
		t.Fatalf("expected zero diagnostics, got %d", len(diags))
	}
	f0 := cfg.Form.Sections[0].Fields[0]
	if f0.Placeholder != "6379" || f0.Hint != "TCP-порт Redis" {
		t.Errorf("placeholder/hint not parsed: %#v", f0)
	}
	f1 := cfg.Form.Sections[0].Fields[1]
	if f1.Placeholder != "" || f1.Hint != "" {
		t.Errorf("absent placeholder/hint must be empty: %#v", f1)
	}
}

// TestForm_PlaceholderHint_Empty — placeholder: "" / hint: "" → form_field_empty_label
// WARNING (не error), как у пустого label.
func TestForm_PlaceholderHint_Empty(t *testing.T) {
	src := `name: x
input:
  a: { type: string }
form:
  sections:
    - key: s1
      fields:
        - { name: a, placeholder: "", hint: "" }
tasks: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("empty placeholder/hint must NOT be an error")
	}
	if !hasWarn(diags, "form_field_empty_label") {
		dump(t, diags)
		t.Fatalf("expected form_field_empty_label WARNING on empty placeholder/hint")
	}
}

// TestForm_NotMapping — form: <scalar> → type_mismatch ERROR.
func TestForm_NotMapping(t *testing.T) {
	src := `name: x
form: "oops"
tasks: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "type_mismatch", "$.form") {
		dump(t, diags)
		t.Fatalf("expected type_mismatch on scalar form")
	}
}
