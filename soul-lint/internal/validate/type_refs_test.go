package validate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// Offline validation of $type references against the service's type
// catalog. Fixture — a mini-service on disk: <root>/types.yml +
// <root>/scenario/<name>/main.yml. Lint main.yml, verify that
// unknown/cycle/duplicate from the catalog and the resolve are caught.

// writeMiniService lays out a mini-service: types.yml at the root +
// scenario/<name>/main.yml. typesYAML=="" → types.yml isn't created (no
// catalog). Returns the path to main.yml.
func writeMiniService(t *testing.T, typesYAML, scenarioName, mainYAML string) string {
	t.Helper()
	root := t.TempDir()
	if typesYAML != "" {
		if err := os.WriteFile(filepath.Join(root, "types.yml"), []byte(typesYAML), 0o600); err != nil {
			t.Fatalf("write types.yml: %v", err)
		}
	}
	scnDir := filepath.Join(root, "scenario", scenarioName)
	if err := os.MkdirAll(scnDir, 0o755); err != nil {
		t.Fatalf("mkdir scenario: %v", err)
	}
	mainPath := filepath.Join(scnDir, "main.yml")
	if err := os.WriteFile(mainPath, []byte(mainYAML), 0o600); err != nil {
		t.Fatalf("write main.yml: %v", err)
	}
	return mainPath
}

// runJSON runs scenario lint in JSON mode and returns the diagnostics.
func runJSON(t *testing.T, mainPath string) []diag.Diagnostic {
	t.Helper()
	var out, errOut bytes.Buffer
	Run(Options{Path: mainPath, Kind: KindScenario, JSON: true}, &out, &errOut)
	var diags []diag.Diagnostic
	dec := json.NewDecoder(bytes.NewReader(out.Bytes()))
	for dec.More() {
		var d diag.Diagnostic
		if err := dec.Decode(&d); err != nil {
			t.Fatalf("decode diag: %v\nstdout: %s\nstderr: %s", err, out.String(), errOut.String())
		}
		diags = append(diags, d)
	}
	return diags
}

func hasCode(diags []diag.Diagnostic, code string) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}

// A valid reference to an existing type — no type errors.
func TestTypeRefs_ValidRef_OK(t *testing.T) {
	types := `types:
  Endpoint:
    type: object
    properties:
      host:
        type: string
`
	main := `name: deploy
input:
  target:
    $type: Endpoint
tasks: []
`
	p := writeMiniService(t, types, "deploy", main)
	diags := runJSON(t, p)
	for _, c := range []string{"input_type_unknown", "input_type_cycle", "input_type_duplicate", "input_type_ref_conflict"} {
		if hasCode(diags, c) {
			t.Fatalf("a valid reference should not produce %s: %+v", c, diags)
		}
	}
}

// A reference to a missing type → input_type_unknown.
func TestTypeRefs_Unknown(t *testing.T) {
	types := `types:
  Endpoint:
    type: object
    properties:
      host:
        type: string
`
	main := `name: deploy
input:
  target:
    $type: Ghost
tasks: []
`
	p := writeMiniService(t, types, "deploy", main)
	diags := runJSON(t, p)
	if !hasCode(diags, "input_type_unknown") {
		t.Fatalf("reference to a missing type -> input_type_unknown: %+v", diags)
	}
}

// A cycle between types in the catalog → input_type_cycle (NOT a linter
// hang).
func TestTypeRefs_CatalogCycle(t *testing.T) {
	types := `types:
  A:
    type: object
    properties:
      b:
        $type: B
  B:
    type: object
    properties:
      a:
        $type: A
`
	main := `name: deploy
input:
  target:
    $type: A
tasks: []
`
	p := writeMiniService(t, types, "deploy", main)
	diags := runJSON(t, p)
	if !hasCode(diags, "input_type_cycle") {
		t.Fatalf("cycle between types -> input_type_cycle: %+v", diags)
	}
}

// A duplicate type name in the catalog → input_type_duplicate.
func TestTypeRefs_Duplicate(t *testing.T) {
	types := `types:
  Dup:
    type: string
  Dup:
    type: integer
`
	main := `name: deploy
input:
  target:
    $type: Dup
tasks: []
`
	p := writeMiniService(t, types, "deploy", main)
	diags := runJSON(t, p)
	if !hasCode(diags, "input_type_duplicate") {
		t.Fatalf("duplicate type -> input_type_duplicate: %+v", diags)
	}
}

// $type + an inline type on a node → input_type_ref_conflict (caught by
// scenario parse, no catalog needed).
func TestTypeRefs_Conflict(t *testing.T) {
	main := `name: deploy
input:
  target:
    $type: Endpoint
    type: object
tasks: []
`
	p := writeMiniService(t, "", "deploy", main)
	diags := runJSON(t, p)
	if !hasCode(diags, "input_type_ref_conflict") {
		t.Fatalf("$type + type: → input_type_ref_conflict: %+v", diags)
	}
}

// Back-compat: a scenario without $type doesn't require types.yml and
// doesn't break.
func TestTypeRefs_NoRef_BackCompat(t *testing.T) {
	main := `name: deploy
input:
  port:
    type: integer
tasks: []
`
	p := writeMiniService(t, "", "deploy", main)
	diags := runJSON(t, p)
	for _, c := range []string{"input_type_unknown", "input_type_cycle", "io_error"} {
		if hasCode(diags, c) {
			t.Fatalf("a scenario without $type should not produce %s: %+v", c, diags)
		}
	}
}
