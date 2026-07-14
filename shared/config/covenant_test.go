package config

import (
	"errors"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// Covenant (extends:) — config-layer S1: the ScenarioFragment, MergeCovenant types
// (add-only merge), fragment/extends form validation. Resolving covenant.yml over the
// snapshot FS is keeper-side S2, not tested here.

// loadFragment — covenant.yml without errors (helper for merge tests).
func loadFragment(t *testing.T, src string) *ScenarioFragment {
	t.Helper()
	frag, _, diags := LoadCovenantFragmentFromBytes("covenant.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("fragment unexpectedly failed to load")
	}
	return frag
}

// loadScenario — main.yml without errors (helper for merge tests).
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

	// input: union of both fields.
	if _, ok := local.Input["password_ref"]; !ok {
		t.Errorf("merged input missing covenant field password_ref")
	}
	if _, ok := local.Input["size"]; !ok {
		t.Errorf("merged input missing scenario field size")
	}
	if len(local.Input) != 2 {
		t.Errorf("merged input: want 2 fields, got %d", len(local.Input))
	}

	// compute: covenant first → [base, full].
	if len(local.Compute) != 2 || local.Compute[0].Name != "base" || local.Compute[1].Name != "full" {
		t.Errorf("merged compute order wrong: %+v", local.Compute)
	}

	// state_changes: covenant first → [set provisioned, set size].
	if local.StateChanges == nil || len(local.StateChanges.Ops) != 2 {
		t.Fatalf("merged state_changes: want 2 ops, got %+v", local.StateChanges)
	}
	if local.StateChanges.Ops[0].Field != "provisioned" || local.StateChanges.Ops[1].Field != "size" {
		t.Errorf("merged state_changes order wrong: %+v", local.StateChanges.Ops)
	}

	// validate: covenant first, both rules accumulated.
	if len(local.Validate) != 2 || local.Validate[0].Message != "password_ref required" || local.Validate[1].Message != "size positive" {
		t.Errorf("merged validate order/content wrong: %+v", local.Validate)
	}
}

// covenant-only sections (the scenario declares none of its own) come through as-is.
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

// Non-set verbs on the same field do NOT conflict (multiple add/modify are legitimate).
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

// --- forward-compat: scenario without extends ----------------------------

// A scenario without extends parses valid, Extends is empty, and the resolver does not
// call MergeCovenant (S2 skips it on empty extends). Here we check that Extends == ""
// and that an empty fragment-merge is a no-op (in case it is called with a zero fragment).
func TestScenario_NoExtends_ForwardCompat(t *testing.T) {
	m := loadScenario(t, "name: create\ninput:\n  x:\n    type: string\ntasks: []\n")
	if m.Extends != "" {
		t.Fatalf("expected empty Extends, got %q", m.Extends)
	}

	// An empty fragment (zero ScenarioFragment) merges as a no-op.
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

// ValidExtendsName — single source of truth about the form (resolver S2 relies on it).
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
			// Exactly one covenant_unexpected_key per key — generic unknown_key suppressed.
			if n := countCode(diags, "unknown_key"); n != 0 {
				t.Errorf("generic unknown_key not suppressed for covenant key %q (got %d)", key, n)
			}
		})
	}
}

// covenant with only the 4 sections — no errors.
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

// Structural validation of a covenant section works (same DSL as the scenario): an
// invalid compute name inside a covenant is caught.
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

// unexpectedKeyValue gives a type-safe value for each "foreign" covenant key so the
// YAML parses (covenant_unexpected_key is raised on the form, not on the value type).
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
