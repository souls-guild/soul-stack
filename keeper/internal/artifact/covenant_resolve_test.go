package artifact

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// writeCovenant puts the `<name>.yml` covenant fragment into the test
// serviceRoot root (sibling of types.yml/scenario/), as
// config.ResolveScenarioCovenant searches for it.
func writeCovenant(t *testing.T, root, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name+".yml"), []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile covenant %s: %v", name, err)
	}
}

// loadResolved is a wrapper: reads scenario/<name>/main.yml from snapshot root
// through the same path as runtime callers (ReadFile ->
// LoadScenarioManifestResolved).
func loadResolved(t *testing.T, root, scenario string) (*config.ScenarioManifest, []diag.Diagnostic) {
	t.Helper()
	rel := filepath.ToSlash(filepath.Join(scenarioDir, scenario, scenarioMainFile))
	art := &ServiceArtifact{LocalDir: root}
	data, err := readSnapshotFile(root, rel)
	if err != nil {
		t.Fatalf("readSnapshotFile %s: %v", rel, err)
	}
	scn, _, diags, err := LoadScenarioManifestResolved(art, rel, data)
	if err != nil {
		t.Fatalf("LoadScenarioManifestResolved %s: %v", scenario, err)
	}
	return scn, diags
}

// diagCodes collects error-level diagnostic codes for assertions.
func diagCodes(ds []diag.Diagnostic) []string {
	var out []string
	for _, d := range ds {
		if d.Level == diag.LevelError {
			out = append(out, d.Code)
		}
	}
	return out
}

func hasCode(ds []diag.Diagnostic, code string) bool {
	for _, c := range diagCodes(ds) {
		if c == code {
			return true
		}
	}
	return false
}

// extends -> covenant.yml is resolved and merged: the merged manifest carries
// BOTH covenant fields (input/compute) and the scenario's own fields.
func TestResolveCovenant_MergesInputAndCompute(t *testing.T) {
	root := t.TempDir()
	writeCovenant(t, root, "base", `input:
  cluster_name:
    type: string
compute:
  shared_prefix: "${ input.cluster_name }"
`)
	writeScenario(t, root, "create", `name: create
extends: base
tasks: []
input:
  replicas:
    type: integer
compute:
  local_label: "local"
`)

	scn, diags := loadResolved(t, root, "create")
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected resolve errors: %v", diagCodes(diags))
	}
	if _, ok := scn.Input["cluster_name"]; !ok {
		t.Errorf("covenant input cluster_name was not merged: %#v", scn.Input)
	}
	if _, ok := scn.Input["replicas"]; !ok {
		t.Errorf("local input replicas lost: %#v", scn.Input)
	}
	if len(scn.Compute) != 2 {
		t.Fatalf("want 2 compute entries (covenant+local), got %d: %#v", len(scn.Compute), scn.Compute)
	}
	// covenant compute goes FIRST (shared contract precedes the delta).
	if scn.Compute[0].Name != "shared_prefix" {
		t.Errorf("covenant compute should be first, got %q", scn.Compute[0].Name)
	}
	if scn.Compute[1].Name != "local_label" {
		t.Errorf("local compute should be second, got %q", scn.Compute[1].Name)
	}
}

// covenant input carrying a $type reference is resolved after merge (covenant
// before $type resolution): the merged field gets the type body + x-type and
// does not remain raw.
func TestResolveCovenant_CovenantInputTypeRefResolved(t *testing.T) {
	root := t.TempDir()
	writeTypesCatalog(t, root, `types:
  Endpoint:
    type: object
    properties:
      host:
        type: string
`)
	writeCovenant(t, root, "base", `input:
  target:
    $type: Endpoint
`)
	writeScenario(t, root, "create", `name: create
extends: base
tasks: []
`)

	scn, diags := loadResolved(t, root, "create")
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diagCodes(diags))
	}
	target, ok := scn.Input["target"]
	if !ok {
		t.Fatalf("covenant input target was not merged: %#v", scn.Input)
	}
	// covenant input passed $type resolution AFTER merge: the node is typed
	// (Type=object + Properties), not left as an untyped reference ($type -> empty
	// Type, submitted-input shape validation would silently pass).
	if target.Type != "object" {
		t.Errorf("covenant field target.Type = %q, want object ($type resolution after merge did not work)", target.Type)
	}
	if _, ok := target.Properties["host"]; !ok {
		t.Errorf("covenant field target.Properties.host missing after resolve: %#v", target.Properties)
	}
}

// section_key_conflict: one input field name appears in both covenant and
// scenario -> load diagnostic with code section_key_conflict and clear text
// (covenant name + key).
func TestResolveCovenant_SectionKeyConflict(t *testing.T) {
	root := t.TempDir()
	writeCovenant(t, root, "base", `input:
  replicas:
    type: integer
`)
	writeScenario(t, root, "create", `name: create
extends: base
tasks: []
input:
  replicas:
    type: string
`)

	_, diags := loadResolved(t, root, "create")
	if !hasCode(diags, "section_key_conflict") {
		t.Fatalf("want section_key_conflict, codes: %v", diagCodes(diags))
	}
	var msg string
	for _, d := range diags {
		if d.Code == "section_key_conflict" {
			msg = d.Message
		}
	}
	if !strings.Contains(msg, "base") || !strings.Contains(msg, "replicas") {
		t.Errorf("conflict text should carry covenant name and key, got %q", msg)
	}
}

// extends to a missing covenant file -> covenant_extends_target_not_found.
func TestResolveCovenant_TargetNotFound(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "create", `name: create
extends: nope
tasks: []
`)

	_, diags := loadResolved(t, root, "create")
	if !hasCode(diags, "covenant_extends_target_not_found") {
		t.Fatalf("want covenant_extends_target_not_found, codes: %v", diagCodes(diags))
	}
}

// extends with an invalid name (path traversal/separator) ->
// covenant_extends_invalid; file resolution is not attempted (name fails
// grammar).
func TestResolveCovenant_InvalidExtendsName(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "create", `name: create
extends: "../escape"
tasks: []
`)

	_, diags := loadResolved(t, root, "create")
	// config validator itself raises diag for invalid extends in schema phase;
	// resolver additionally fail-closes with covenant_extends_invalid. It is
	// enough that there is an error and NO "not found" attempt (name is rejected
	// before file access).
	if !diag.HasErrors(diags) {
		t.Fatalf("invalid extends should return an error, codes: %v", diagCodes(diags))
	}
	if hasCode(diags, "covenant_extends_target_not_found") {
		t.Errorf("invalid name should not reach file resolution: %v", diagCodes(diags))
	}
}

// Cross-form state_changes: covenant in list form, scenario in map form (or
// vice versa) -> state_changes_form_mismatch (S1 merge does not detect across
// forms).
func TestResolveCovenant_StateChangesFormMismatch(t *testing.T) {
	root := t.TempDir()
	// covenant: list-form state_changes.
	writeCovenant(t, root, "base", `state_changes:
  - set: shared_field
    value: "${ input.x }"
`)
	// scenario: map form (deprecated Sets).
	writeScenario(t, root, "create", `name: create
extends: base
tasks: []
input:
  x:
    type: string
state_changes:
  sets:
    local_field: "${ input.x }"
`)

	_, diags := loadResolved(t, root, "create")
	if !hasCode(diags, "state_changes_form_mismatch") {
		t.Fatalf("want state_changes_form_mismatch, codes: %v", diagCodes(diags))
	}
}

// Scenario WITHOUT extends -> forward-compat: covenant resolution is no-op,
// manifest is bit-for-bit as without the feature (no covenant diagnostics,
// input/compute remain local).
func TestResolveCovenant_NoExtendsForwardCompat(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "create", `name: create
tasks: []
input:
  replicas:
    type: integer
compute:
  local_label: "local"
`)

	scn, diags := loadResolved(t, root, "create")
	if diag.HasErrors(diags) {
		t.Fatalf("without extends there should be no errors, codes: %v", diagCodes(diags))
	}
	for _, c := range diagCodes(diags) {
		if strings.HasPrefix(c, "covenant") || c == "section_key_conflict" || c == "state_changes_form_mismatch" {
			t.Errorf("covenant diagnostic without extends is not allowed: %s", c)
		}
	}
	if len(scn.Input) != 1 {
		t.Errorf("input without extends should carry only its own field, got %#v", scn.Input)
	}
	if len(scn.Compute) != 1 {
		t.Errorf("compute without extends should carry only its own field, got %#v", scn.Compute)
	}
}

// Fresh decode: repeated LoadScenarioManifestResolved for the same scenario
// does NOT accumulate covenant sections. Mutating the merged manifest does not
// stick to cache because manifest is born from bytes on each call (cache holds
// snapshot files).
func TestResolveCovenant_RepeatedLoadNoAccumulation(t *testing.T) {
	root := t.TempDir()
	writeCovenant(t, root, "base", `input:
  cluster_name:
    type: string
compute:
  shared_prefix: "p"
validate:
  - that: "true"
    message: "shared invariant"
`)
	writeScenario(t, root, "create", `name: create
extends: base
tasks: []
input:
  replicas:
    type: integer
compute:
  local_label: "local"
`)

	first, d1 := loadResolved(t, root, "create")
	if diag.HasErrors(d1) {
		t.Fatalf("first Load: %v", diagCodes(d1))
	}
	second, d2 := loadResolved(t, root, "create")
	if diag.HasErrors(d2) {
		t.Fatalf("second Load: %v", diagCodes(d2))
	}

	if len(first.Input) != len(second.Input) {
		t.Errorf("input accumulated between loads: 1st=%d 2nd=%d", len(first.Input), len(second.Input))
	}
	if len(first.Compute) != len(second.Compute) {
		t.Errorf("compute accumulated: 1st=%d 2nd=%d", len(first.Compute), len(second.Compute))
	}
	if len(first.Validate) != len(second.Validate) {
		t.Errorf("validate accumulated: 1st=%d 2nd=%d", len(first.Validate), len(second.Validate))
	}
	// Absolute expectations (catches even synchronous doubling in BOTH).
	if len(second.Compute) != 2 {
		t.Errorf("compute should be covenant(1)+local(1)=2, got %d", len(second.Compute))
	}
	if len(second.Validate) != 1 {
		t.Errorf("validate should be exactly covenant(1), got %d", len(second.Validate))
	}
}

// countCode returns the number of error diagnostics with the given code (for
// exact "one" assertions).
func countCode(ds []diag.Diagnostic, code string) int {
	n := 0
	for _, d := range ds {
		if d.Level == diag.LevelError && d.Code == code {
			n++
		}
	}
	return n
}

// Post-merge form SEES covenant input (guard for the central new code
// resolveCovenantFormDiags): covenant carries an input field, scenario declares
// a form field for it -> zero form ERRORS. Before moving to post-merge stage,
// form was gated in semantic phase BEFORE merge; covenant field was absent
// there, and this same form would have produced a false form_field_unknown. This
// test proves validation runs on MERGED input.
func TestResolveCovenant_FormSeesCovenantInput(t *testing.T) {
	root := t.TempDir()
	writeCovenant(t, root, "base", `input:
  cluster_name:
    type: string
`)
	// form section references covenant field cluster_name; scenario has no local
	// input, so the only form field is covered exactly by the covenant field.
	writeScenario(t, root, "create", `name: create
extends: base
tasks: []
form:
  sections:
    - key: main
      title: Main
      fields:
        - name: cluster_name
`)

	_, diags := loadResolved(t, root, "create")
	if hasCode(diags, "form_field_unknown") {
		t.Errorf("form_field_unknown on covenant field - post-merge form did not see merged input: %v", diagCodes(diags))
	}
	if diag.HasErrors(diags) {
		t.Errorf("want zero form errors, codes: %v", diagCodes(diags))
	}
}

// Reverse guard: the validator did NOT get weaker after moving to post-merge
// stage. A form field for a name absent from both covenant and local input
// yields EXACTLY ONE post-merge form_field_unknown. The second form field
// references covenant input (cluster_name) and must pass, proving unknown is
// emitted for the actually absent field, not the covenant field (the validator
// both sees covenant input and keeps catching garbage).
func TestResolveCovenant_FormFieldUnknownPostMerge(t *testing.T) {
	root := t.TempDir()
	writeCovenant(t, root, "base", `input:
  cluster_name:
    type: string
`)
	writeScenario(t, root, "create", `name: create
extends: base
tasks: []
input:
  replicas:
    type: integer
form:
  sections:
    - key: main
      title: Main
      fields:
        - name: cluster_name
        - name: replicas
        - name: ghost_field
`)

	_, diags := loadResolved(t, root, "create")
	if n := countCode(diags, "form_field_unknown"); n != 1 {
		t.Fatalf("want exactly one form_field_unknown (on ghost_field), got %d; codes: %v", n, diagCodes(diags))
	}
	// Prove addressability: covenant field did NOT produce a false unknown
	// (otherwise there would be >1, but check explicitly: text carries
	// ghost_field, not cluster_name/replicas).
	for _, d := range diags {
		if d.Code == "form_field_unknown" && !strings.Contains(d.Message, "ghost_field") {
			t.Errorf("form_field_unknown pointed somewhere other than ghost_field: %q", d.Message)
		}
	}
}

// Cross-scenario independence (S1-review finding): two DIFFERENT scenarios with
// one covenant.yml resolve independently; resolving scenario A does not affect
// scenario B (fresh fragment per scenario, read-only fragment contract).
func TestResolveCovenant_TwoScenariosOneCovenantIndependent(t *testing.T) {
	root := t.TempDir()
	writeCovenant(t, root, "base", `input:
  cluster_name:
    type: string
`)
	writeScenario(t, root, "create", `name: create
extends: base
tasks: []
input:
  a_only:
    type: integer
`)
	writeScenario(t, root, "migrate_cluster", `name: migrate_cluster
extends: base
tasks: []
input:
  b_only:
    type: integer
`)

	a, da := loadResolved(t, root, "create")
	b, db := loadResolved(t, root, "migrate_cluster")
	if diag.HasErrors(da) || diag.HasErrors(db) {
		t.Fatalf("resolve errors: A=%v B=%v", diagCodes(da), diagCodes(db))
	}
	// Each sees covenant + only its own field; the other one did NOT leak.
	if _, ok := a.Input["a_only"]; !ok {
		t.Errorf("scenario A lost its own field: %#v", a.Input)
	}
	if _, leaked := a.Input["b_only"]; leaked {
		t.Errorf("scenario B field leaked into scenario A (cross-scenario aliasing): %#v", a.Input)
	}
	if _, ok := b.Input["b_only"]; !ok {
		t.Errorf("scenario B lost its own field: %#v", b.Input)
	}
	if _, leaked := b.Input["a_only"]; leaked {
		t.Errorf("scenario A field leaked into scenario B (cross-scenario aliasing): %#v", b.Input)
	}
}
