package config

import (
	"errors"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// Covenant (extends:) — config-слой S1: типы ScenarioFragment, MergeCovenant
// (add-only merge), валидация формы фрагмента/extends. Резолв covenant.yml по
// ФС снапшота — keeper-side S2, здесь не тестируется.

// loadFragment — covenant.yml без ошибок (helper для merge-тестов).
func loadFragment(t *testing.T, src string) *ScenarioFragment {
	t.Helper()
	frag, _, diags := LoadCovenantFragmentFromBytes("covenant.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("fragment unexpectedly failed to load")
	}
	return frag
}

// loadScenario — main.yml без ошибок (helper для merge-тестов).
func loadScenario(t *testing.T, src string) *ScenarioManifest {
	t.Helper()
	m, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("scenario unexpectedly failed to load")
	}
	return m
}

// --- merge: add-only happy-path -------------------------------------------

func TestMergeCovenant_AddOnlyHappyPath(t *testing.T) {
	frag := loadFragment(t, `
input:
  password_ref:
    type: string
    secret: true
compute:
  base: "${ merge(essence.cfg, {}) }"
state_changes:
  - set: provisioned
    value: "${ true }"
validate:
  - that: "input.password_ref != ''"
    message: "password_ref required"
`)
	local := loadScenario(t, `
name: create
input:
  size:
    type: integer
compute:
  full: "${ merge(compute.base, { 'x': 'y' }) }"
state_changes:
  - set: size
    value: "${ input.size }"
validate:
  - that: "input.size > 0"
    message: "size positive"
tasks: []
`)

	if err := MergeCovenant(*frag, local); err != nil {
		t.Fatalf("happy-path merge must not error: %v", err)
	}

	// input: union обоих полей.
	if _, ok := local.Input["password_ref"]; !ok {
		t.Errorf("merged input missing covenant field password_ref")
	}
	if _, ok := local.Input["size"]; !ok {
		t.Errorf("merged input missing scenario field size")
	}
	if len(local.Input) != 2 {
		t.Errorf("merged input: want 2 fields, got %d", len(local.Input))
	}

	// compute: covenant-первым → [base, full].
	if len(local.Compute) != 2 || local.Compute[0].Name != "base" || local.Compute[1].Name != "full" {
		t.Errorf("merged compute order wrong: %+v", local.Compute)
	}

	// state_changes: covenant-первым → [set provisioned, set size].
	if local.StateChanges == nil || len(local.StateChanges.Ops) != 2 {
		t.Fatalf("merged state_changes: want 2 ops, got %+v", local.StateChanges)
	}
	if local.StateChanges.Ops[0].Field != "provisioned" || local.StateChanges.Ops[1].Field != "size" {
		t.Errorf("merged state_changes order wrong: %+v", local.StateChanges.Ops)
	}

	// validate: covenant-первым, оба правила накоплены.
	if len(local.Validate) != 2 || local.Validate[0].Message != "password_ref required" || local.Validate[1].Message != "size positive" {
		t.Errorf("merged validate order/content wrong: %+v", local.Validate)
	}
}

// covenant-only секции (сценарий не объявляет своих) приходят как есть.
func TestMergeCovenant_CovenantOnlySections(t *testing.T) {
	frag := loadFragment(t, `
input:
  a:
    type: string
compute:
  c: "${ 1 }"
state_changes:
  - set: s
    value: "${ 1 }"
validate:
  - that: "input.a != ''"
    message: "a"
`)
	local := loadScenario(t, "name: create\ntasks: []\n")

	if err := MergeCovenant(*frag, local); err != nil {
		t.Fatalf("merge must not error: %v", err)
	}
	if len(local.Input) != 1 || len(local.Compute) != 1 || len(local.Validate) != 1 {
		t.Errorf("covenant-only sections not adopted: input=%d compute=%d validate=%d",
			len(local.Input), len(local.Compute), len(local.Validate))
	}
	if local.StateChanges == nil || len(local.StateChanges.Ops) != 1 {
		t.Errorf("covenant-only state_changes not adopted: %+v", local.StateChanges)
	}
}

// --- merge: conflicts (fail-closed, no override) ---------------------------

func TestMergeCovenant_InputFieldConflict(t *testing.T) {
	frag := loadFragment(t, "input:\n  shared:\n    type: string\n")
	local := loadScenario(t, "name: create\ninput:\n  shared:\n    type: integer\ntasks: []\n")

	err := MergeCovenant(*frag, local)
	assertSectionConflict(t, err, "input", "shared")
}

func TestMergeCovenant_ComputeNameConflict(t *testing.T) {
	frag := loadFragment(t, "compute:\n  dup: \"${ 1 }\"\n")
	local := loadScenario(t, "name: create\ncompute:\n  dup: \"${ 2 }\"\ntasks: []\n")

	err := MergeCovenant(*frag, local)
	assertSectionConflict(t, err, "compute", "dup")
}

func TestMergeCovenant_StateSetFieldConflict(t *testing.T) {
	frag := loadFragment(t, "state_changes:\n  - set: field\n    value: \"${ 1 }\"\n")
	local := loadScenario(t, "name: create\nstate_changes:\n  - set: field\n    value: \"${ 2 }\"\ntasks: []\n")

	err := MergeCovenant(*frag, local)
	assertSectionConflict(t, err, "state_changes", "set field")
}

// Не-set глаголы на одном поле НЕ конфликтуют (несколько add/modify легитимны).
func TestMergeCovenant_NonSetSameFieldNoConflict(t *testing.T) {
	frag := loadFragment(t, "state_changes:\n  - add: users\n    value: \"${ 'a' }\"\n")
	local := loadScenario(t, "name: create\nstate_changes:\n  - add: users\n    value: \"${ 'b' }\"\ntasks: []\n")

	if err := MergeCovenant(*frag, local); err != nil {
		t.Fatalf("two add ops on same field must not conflict: %v", err)
	}
	if len(local.StateChanges.Ops) != 2 {
		t.Errorf("want 2 add ops, got %d", len(local.StateChanges.Ops))
	}
}

// --- forward-compat: сценарий без extends ----------------------------------

// Сценарий без extends парсится валидно, Extends пуст, MergeCovenant не
// вызывается резолвером (S2 пропускает при пустом extends). Здесь проверяем,
// что Extends == "" и что пустой fragment-merge — no-op (на случай вызова с
// нулевым фрагментом).
func TestScenario_NoExtends_ForwardCompat(t *testing.T) {
	m := loadScenario(t, "name: create\ninput:\n  x:\n    type: string\ntasks: []\n")
	if m.Extends != "" {
		t.Fatalf("expected empty Extends, got %q", m.Extends)
	}

	// Пустой фрагмент (нулевой ScenarioFragment) merge-ится как no-op.
	before := len(m.Input)
	if err := MergeCovenant(ScenarioFragment{}, m); err != nil {
		t.Fatalf("empty-fragment merge must be no-op, got %v", err)
	}
	if len(m.Input) != before {
		t.Errorf("empty-fragment merge mutated input: %d -> %d", before, len(m.Input))
	}
}

// --- extends form: valid / traversal-reject --------------------------------

func TestScenario_ExtendsValidName(t *testing.T) {
	for _, name := range []string{"base", "redis-common", "a", "x1-2-3"} {
		m := loadScenario(t, "name: create\nextends: "+name+"\ntasks: []\n")
		if m.Extends != name {
			t.Errorf("Extends decode: want %q, got %q", name, m.Extends)
		}
	}
}

func TestScenario_ExtendsTraversalRejected(t *testing.T) {
	bad := []string{
		"../escape",
		"sub/covenant",
		".hidden",
		"/abs",
		"UPPER",
		"covenant.yml",
		"a..b",
		"-leading",
	}
	for _, name := range bad {
		name := name
		t.Run(name, func(t *testing.T) {
			src := "name: create\nextends: \"" + name + "\"\ntasks: []\n"
			_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
			if !hasCode(diags, "covenant_extends_invalid") {
				dump(t, diags)
				t.Fatalf("expected covenant_extends_invalid for extends %q", name)
			}
		})
	}
}

// ValidExtendsName — единый источник правды о форме (резолвер S2 опирается на него).
func TestValidExtendsName(t *testing.T) {
	ok := []string{"base", "redis-common", "a", "x1"}
	for _, n := range ok {
		if !ValidExtendsName(n) {
			t.Errorf("ValidExtendsName(%q) = false, want true", n)
		}
	}
	notOk := []string{"", "../x", "a/b", "A", ".x", "-x", "x.y", "x_y"}
	for _, n := range notOk {
		if ValidExtendsName(n) {
			t.Errorf("ValidExtendsName(%q) = true, want false", n)
		}
	}
}

// --- covenant fragment form: unexpected scenario keys ----------------------

func TestLoadCovenant_UnexpectedKey(t *testing.T) {
	for _, key := range []string{"name", "tasks", "create", "form", "extends", "vars", "description"} {
		key := key
		t.Run(key, func(t *testing.T) {
			src := "input:\n  a:\n    type: string\n" + key + ": " + unexpectedKeyValue(key) + "\n"
			_, _, diags := LoadCovenantFragmentFromBytes("covenant.yml", []byte(src), ValidateOptions{})
			if !hasCode(diags, "covenant_unexpected_key") {
				dump(t, diags)
				t.Fatalf("expected covenant_unexpected_key for covenant field %q", key)
			}
			// Ровно один covenant_unexpected_key на ключ — generic unknown_key подавлен.
			if n := countCode(diags, "unknown_key"); n != 0 {
				t.Errorf("generic unknown_key not suppressed for covenant key %q (got %d)", key, n)
			}
		})
	}
}

// covenant с только 4 секциями — без ошибок.
func TestLoadCovenant_OnlySectionsOK(t *testing.T) {
	src := `
input:
  a:
    type: string
compute:
  c: "${ 1 }"
state_changes:
  - set: s
    value: "${ 1 }"
validate:
  - that: "input.a != ''"
    message: "a"
`
	frag, _, diags := LoadCovenantFragmentFromBytes("covenant.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("4-section covenant must be valid")
	}
	if len(frag.Input) != 1 || len(frag.Compute) != 1 || len(frag.Validate) != 1 || frag.StateChanges == nil {
		t.Errorf("fragment sections not decoded: %+v", frag)
	}
}

// Структурная валидация секции covenant работает (тот же DSL, что у сценария):
// невалидное имя compute внутри covenant ловится.
func TestLoadCovenant_SectionStructureValidated(t *testing.T) {
	src := "compute:\n  bad-name: \"${ 1 }\"\n"
	_, _, diags := LoadCovenantFragmentFromBytes("covenant.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "name_invalid_format") {
		dump(t, diags)
		t.Fatalf("expected covenant compute section to be structurally validated")
	}
}

// --- helpers ---------------------------------------------------------------

func assertSectionConflict(t *testing.T, err error, section, key string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected section_key_conflict for %s.%s, got nil", section, key)
	}
	var conflict *SectionKeyConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("expected *SectionKeyConflict, got %T: %v", err, err)
	}
	if conflict.Code() != "section_key_conflict" {
		t.Errorf("conflict code: want section_key_conflict, got %q", conflict.Code())
	}
	if conflict.Section != section || conflict.Key != key {
		t.Errorf("conflict: want %s.%s, got %s.%s", section, key, conflict.Section, conflict.Key)
	}
}

// unexpectedKeyValue даёт типобезопасное значение для каждого «чужого» covenant
// ключа, чтобы YAML распарсился (covenant_unexpected_key поднимается на форме, а
// не на типе значения).
func unexpectedKeyValue(key string) string {
	switch key {
	case "tasks":
		return "[]"
	case "create":
		return "true"
	case "form":
		return "{}"
	default:
		return "x"
	}
}
