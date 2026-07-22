// ADR-045 S7-amend — consistency of the `items` declaration under `type: map`
// (the value type, map[string]<items>) between the built-in core manifest and
// what the `params:` validator actually accepts.
//
// Covers two cases, symmetric to the list field:
//   - map + items.type=string — a flat string map (env/headers/vars): items is
//     set, the value type is scalar → the UI renders a KEY→VALUE editor;
//   - map without items — an arbitrary structure (cloud profile) → the UI renders JSON.
//
// The file physically lives in shared/coremanifest/ (external test package), does
// not touch production code — the same motivation as manifest_validator_consistency_test.go.
package coremanifest_test

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/coremanifest"
	"github.com/souls-guild/soul-stack/shared/diag"
	"github.com/souls-guild/soul-stack/shared/plugin"
)

// mapFieldAddr is the address of one map param of a core module for checking items.
type mapFieldAddr struct {
	module, state, param string
}

// TestS7Amend_MapValueItemsDeclared — flat string maps carry items.type=string
// (KEY→VALUE editor in the UI), and an arbitrary cloud-profile has NO items (JSON
// editor). The source of truth is the built-in coremanifest.
func TestS7Amend_MapValueItemsDeclared(t *testing.T) {
	reg := coremanifest.Default()

	lookup := func(f mapFieldAddr) (plugin.InputParamDef, bool) {
		def, ok := reg.State(f.module, f.state)
		if !ok {
			return plugin.InputParamDef{}, false
		}
		p, ok := def.Input[f.param]
		return p, ok
	}

	withStringItems := []mapFieldAddr{
		{"core.cmd", "shell", "env"},
		{"core.exec", "run", "env"},
		{"core.file", "rendered", "vars"},
		{"core.http", "probe", "headers"},
		{"core.url", "fetched", "headers"},
	}
	for _, f := range withStringItems {
		p, ok := lookup(f)
		if !ok {
			t.Errorf("%s.%s.%s: param not found in coremanifest", f.module, f.state, f.param)
			continue
		}
		if p.Type != "map" {
			t.Errorf("%s.%s.%s: expected type=map, got %q", f.module, f.state, f.param, p.Type)
		}
		if p.Items == nil {
			t.Errorf("%s.%s.%s: expected items (map value type), got nil", f.module, f.state, f.param)
			continue
		}
		if p.Items.Type != "string" {
			t.Errorf("%s.%s.%s: expected items.type=string, got %q", f.module, f.state, f.param, p.Items.Type)
		}
	}

	// cloud profile — deliberately without items (arbitrary structure → JSON in the UI).
	profile, ok := lookup(mapFieldAddr{"core.cloud", "created", "profile"})
	if !ok {
		t.Fatal("core.cloud.created.profile not found in coremanifest")
	}
	if profile.Type != "map" {
		t.Errorf("core.cloud.created.profile: expected type=map, got %q", profile.Type)
	}
	if profile.Items != nil {
		t.Errorf("core.cloud.created.profile: expected NO items (freeform structure), got %+v", profile.Items)
	}
}

// TestS7Amend_MapWithItemsValidatorAccepts — a declared map+items does not raise
// items errors in the manifest validator, and a task with a map param passes the
// config validator (the public path, as in TestP5_*).
func TestS7Amend_MapWithItemsValidatorAccepts(t *testing.T) {
	const withItems = `kind: soul_module
protocol_version: 1
namespace: core
name: probe
spec:
  states:
    s:
      input:
        env: { type: map, items: { type: string } }
`
	if _, diags := plugin.LoadFromBytes("manifest.yaml", []byte(withItems)); hasItemsErr(diags) {
		t.Errorf("map+items.type=string produced an items error: %v", diags)
	}

	const noItems = `kind: soul_module
protocol_version: 1
namespace: core
name: probe
spec:
  states:
    s:
      input:
        profile: { type: map }
`
	if _, diags := plugin.LoadFromBytes("manifest.yaml", []byte(noItems)); hasItemsErr(diags) {
		t.Errorf("map without items produced an items error: %v", diags)
	}

	src := "- name: probe\n  module: core.cmd.shell\n  params:\n    cmd: \"echo hi\"\n    env: { FOO: bar }\n"
	if _, diags, _ := config.LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), config.ValidateOptions{}); hasMapValueErr(diags) {
		t.Errorf("task with a map param env produced an items/value error: %v", diags)
	}
}

func hasItemsErr(diags []diag.Diagnostic) bool {
	for _, d := range diags {
		if strings.HasPrefix(d.Code, "input_items_") {
			return true
		}
	}
	return false
}

func hasMapValueErr(diags []diag.Diagnostic) bool {
	for _, d := range diags {
		switch d.Code {
		case "param_type_mismatch", "input_items_invalid_for_type":
			return true
		}
	}
	return false
}
