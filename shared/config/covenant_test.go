package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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
  base: "${ merge(vars.cfg, {}) }"
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
	// NIM-694: one subdirectory level and `_` are inside the grammar — a service
	// with several scenarios of one family keeps their common contract in
	// `shared/scenario_create.yml` instead of the service root.
	for _, name := range []string{"base", "redis-common", "a", "x1-2-3", "scenario_create", "shared/scenario_create", "_shared/contract"} {
		m := loadScenario(t, "name: create\nextends: "+name+"\ntasks: []\n")
		if m.Extends != name {
			t.Errorf("Extends decode: want %q, got %q", name, m.Extends)
		}
	}
}

func TestScenario_ExtendsTraversalRejected(t *testing.T) {
	// wantReason pins the rule that must do the rejecting. Without it the two
	// depth-cap rows below are indistinguishable from the alphabet rows: removing
	// the `?` cap from reCovenantName would leave them green, because `a/b/c`
	// also fails nothing else only by accident.
	bad := []struct{ name, wantReason string }{
		{"../escape", "must not contain a `.` or `..` path segment"},
		{"sub/../escape", "must not contain a `.` or `..` path segment"},
		{"shared/../../escape", "must not contain a `.` or `..` path segment"},
		// The subdirectory is capped at ONE level (NIM-694): `shared/x` resolves,
		// `shared/deep/x` does not — the depth is part of the grammar, so a
		// service cannot grow an arbitrary tree the resolvers would have to walk.
		{"a/b/c", "at most one subdirectory level"},
		{"shared/nested/contract", "at most one subdirectory level"},
		{".hidden", "each path segment must match"},
		{"shared/.hidden", "each path segment must match"},
		{"/abs", "must not be an absolute path"},
		{"UPPER", "each path segment must match"},
		{"covenant.yml", "each path segment must match"},
		{"a..b", "each path segment must match"},
		{"-leading", "each path segment must match"},
		{"shared/", "each path segment must match"},
		{"/", "must not be an absolute path"},
	}
	for _, tc := range bad {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			src := "name: create\nextends: \"" + tc.name + "\"\ntasks: []\n"
			_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
			d := diagWithCode(diags, "covenant_extends_invalid")
			if d == nil {
				dump(t, diags)
				t.Fatalf("expected covenant_extends_invalid for extends %q", tc.name)
			}
			if !strings.Contains(d.Message, tc.wantReason) {
				t.Fatalf("extends %q rejected for the wrong reason:\n got: %s\nwant it to name: %s",
					tc.name, d.Message, tc.wantReason)
			}
		})
	}
}

// ValidExtendsName — single source of truth about the form (resolver S2 relies on it).
func TestValidExtendsName(t *testing.T) {
	ok := []string{"base", "redis-common", "a", "x1", "x_y", "a/b", "shared/scenario_create", "_shared/x"}
	for _, n := range ok {
		if !ValidExtendsName(n) {
			t.Errorf("ValidExtendsName(%q) = false, want true", n)
		}
	}
	// `a/b/c` — one subdirectory level is the cap (NIM-694). The rest cannot be
	// expressed at all: `.` is outside the segment alphabet, so `..`, hidden names
	// and an extension are rejected by the grammar rather than by a later path
	// check — the traversal clamp IS the name form.
	notOk := []string{"", "../x", "a/b/c", "a/../b", "A", ".x", "-x", "x.y", "/x", "x/", "a//b"}
	for _, n := range notOk {
		if ValidExtendsName(n) {
			t.Errorf("ValidExtendsName(%q) = true, want false", n)
		}
	}
}

// TestResolveScenarioCovenant_Subdirectory is the NIM-694 guard for the resolve
// side of the widened `extends:`: `extends: shared/contract` must read
// `<serviceRoot>/shared/contract.yml` and merge it exactly as a root-level
// fragment. Without the widening the name never reaches the FS (S1 rejects it);
// with a wrong path join it reads nothing.
func TestResolveScenarioCovenant_Subdirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	fragment := "input:\n  cluster_name:\n    type: string\n    required: true\n"
	if err := os.WriteFile(filepath.Join(root, "shared", "contract.yml"), []byte(fragment), 0o644); err != nil {
		t.Fatal(err)
	}

	src := "name: create\nextends: shared/contract\ntasks: []\n"
	m, doc, diags, err := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if err != nil || diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("scenario with extends: shared/contract must parse, err=%v", err)
	}
	rdiags := ResolveScenarioCovenant(m, doc, root)
	if diag.HasErrors(rdiags) {
		dump(t, rdiags)
		t.Fatalf("extends: shared/contract must resolve under the service root")
	}
	if _, ok := m.Input["cluster_name"]; !ok {
		t.Fatalf("covenant input was not merged: %v", m.Input)
	}
}

// TestReadCovenantFile_ClampedToServiceRoot proves the second line of defence
// directly (NIM-694): even handed a name the grammar can never produce, the
// reader stays inside serviceRoot — securejoin resolves `..` against the root
// instead of climbing out of it. The witness is a file that EXISTS one level
// above the root and must NOT be readable.
func TestReadCovenantFile_ClampedToServiceRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "service")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "outside.yml"), []byte("input: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Same basename INSIDE the root would make the negative meaningless (a read
	// could succeed for the honest reason), so the root deliberately stays empty.
	for _, name := range []string{"../outside.yml", "../../outside.yml", "shared/../../outside.yml", "/outside.yml"} {
		if data, err := readCovenantFile(root, name); err == nil {
			t.Fatalf("readCovenantFile(%q) escaped the service root and read %d bytes", name, len(data))
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
