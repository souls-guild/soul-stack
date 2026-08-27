package trial

// Drift-guard for the L0 claim of NIM-668: "the three topologies of
// example-cloud-bootstrap render from vars/ with no row in either registry".
//
// Why a guard is needed at all. L0 has no service tree: the harness builds
// ServiceVars from `fixtures.vars` and from nothing else (harness.go,
// RenderInput.ServiceVars). So the three cases carry a COPY of the service's
// vars/00-base.yaml, and by themselves they only prove that the scenario
// renders from a map somebody typed into the case. Edit the real vars/ file
// afterwards — rename `fqdn_suffix`, drop a topology, change an instance_type —
// and the cases stay green while the shipped service no longer renders. The
// acceptance sentence would then be false and nothing would say so.
//
// The invariant: for every case of scenario/create-inline,
//
//	case.fixtures.vars.cloud == vars/00-base.yaml -> cloud
//
// by deep equality, not by "contains" — a case must not quietly carry a key the
// service does not ship, which is the direction that makes a green L0 lie.
// Equality is what turns "renders from a fixture" into "renders from vars/".
//
// What this does NOT pin: that the render consumes the block (the three cases do
// that, via assert.task_present on driver/region/fqdn_suffix/profile), and that
// the registry path still works (scenario/create, untouched by NIM-668).

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	yaml "github.com/goccy/go-yaml"
)

const (
	inlineCloudServiceDir = "../../../examples/service/example-cloud-bootstrap"
	inlineCloudVarsKey    = "cloud"
)

// TestInlineCloudCasesMirrorServiceVars pins every create-inline case to the
// service vars file it claims to render from.
func TestInlineCloudCasesMirrorServiceVars(t *testing.T) {
	varsFile := filepath.Join(inlineCloudServiceDir, "vars", "00-base.yaml")
	serviceVars := decodeYAMLMap(t, varsFile)

	want, ok := serviceVars[inlineCloudVarsKey]
	if !ok {
		t.Fatalf("%s has no %q key — the inline scenario has nothing to render from", varsFile, inlineCloudVarsKey)
	}

	caseFiles, err := filepath.Glob(filepath.Join(inlineCloudServiceDir, "scenario", "create-inline", "tests", "*", "case.yml"))
	if err != nil {
		t.Fatalf("glob create-inline cases: %v", err)
	}
	// A floor, not a count: this test is about every case mirroring the service
	// vars, and it would pass vacuously on zero. Three is what the acceptance
	// named, so fewer means a topology was deleted — but a FOURTH is someone
	// covering more, and must not redden a package that has no opinion about how
	// many topologies there are.
	if len(caseFiles) < 3 {
		t.Fatalf("found %d create-inline cases (%v), want at least the 3 topologies of the acceptance", len(caseFiles), caseFiles)
	}

	for _, path := range caseFiles {
		t.Run(filepath.Base(filepath.Dir(path)), func(t *testing.T) {
			c, _, err := LoadCase(path)
			if err != nil {
				t.Fatalf("load case: %v", err)
			}
			got, ok := c.Fixtures.Vars[inlineCloudVarsKey]
			if !ok {
				t.Fatalf("fixtures.vars has no %q — this case renders from something other than the service vars", inlineCloudVarsKey)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("fixtures.vars.%s drifted from %s\n got: %#v\nwant: %#v", inlineCloudVarsKey, varsFile, got, want)
			}
		})
	}
}

// decodeYAMLMap reads a YAML mapping into map[string]any with the SAME decoder
// LoadCase uses (goccy/go-yaml — it scalarises integers as uint64, gopkg.in
// as int), so DeepEqual compares values and not decoder dialects.
func decodeYAMLMap(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return doc
}
