package config

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// compute: — scenario-level computed vars (ADR-009 amendment 2026-06-23).
// Tests structural validation (validateComputeBlock) + decoding that preserves
// declaration order (ComputeBlock.UnmarshalYAML).

func TestLoadScenarioManifest_ComputeOK(t *testing.T) {
	src := `name: create
compute:
  base: "${ merge(vars.redis_config, default(input.redis_settings, {})) }"
  full: "${ merge(compute.base, { 'cluster-enabled': 'yes' }) }"
  count: 3
tasks: []
`
	cfg, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected no errors for valid compute block")
	}
	if len(cfg.Compute) != 3 {
		t.Fatalf("expected 3 compute vars, got %d", len(cfg.Compute))
	}
	// Declaration order preserved (compute.full references compute.base — base
	// must come first).
	if cfg.Compute[0].Name != "base" || cfg.Compute[1].Name != "full" || cfg.Compute[2].Name != "count" {
		t.Fatalf("compute declaration order not preserved: %+v", cfg.Compute)
	}
	// A literal (number) passes through as non-string.
	if cfg.Compute[2].Value != uint64(3) {
		t.Fatalf("compute.count literal: want uint64(3), got %#v", cfg.Compute[2].Value)
	}
}

func TestLoadScenarioManifest_ComputeReservedName(t *testing.T) {
	for _, name := range []string{"input", "soulprint", "vars", "compute", "incarnation", "register"} {
		name := name
		t.Run(name, func(t *testing.T) {
			src := "name: x\ncompute:\n  " + name + ": \"${ 1 }\"\ntasks: []\n"
			_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
			if !hasCode(diags, "reserved_binding_name") {
				dump(t, diags)
				t.Fatalf("expected reserved_binding_name for compute.%s", name)
			}
		})
	}
}

func TestLoadScenarioManifest_ComputeBadName(t *testing.T) {
	src := `name: x
compute:
  redis-config: "${ 1 }"
tasks: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "name_invalid_format") {
		dump(t, diags)
		t.Fatalf("expected name_invalid_format for dashed compute name")
	}
}

func TestLoadScenarioManifest_ComputeEmptyValue(t *testing.T) {
	src := `name: x
compute:
  cfg: ""
tasks: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "empty_value") {
		dump(t, diags)
		t.Fatalf("expected empty_value for empty compute expression")
	}
}

func TestLoadScenarioManifest_ComputeNotMapping(t *testing.T) {
	src := `name: x
compute:
  - cfg
tasks: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "type_mismatch") {
		dump(t, diags)
		t.Fatalf("expected type_mismatch for non-mapping compute block")
	}
}

// --- NIM-619: `compute` as a binding name. A loop binding that shadows the
// namespace is rejected at parse time, so it never reaches a run: the loop axis
// itself has no `compute` (ComputeOutOfScopeLoopAxis), but the binding travels into
// the task body, which does — and there `compute.<name>` would silently stop meaning
// the computed var and start meaning a field of the element being iterated. ---

func TestLoadScenarioManifest_LoopComputeIsReserved(t *testing.T) {
	cases := map[string]string{
		"as":       "as: compute",
		"index_as": "as: u\n      index_as: compute",
	}
	for key, spec := range cases {
		key, spec := key, spec
		t.Run(key, func(t *testing.T) {
			src := `name: x
tasks:
  - module: core.exec.run
    loop:
      items: "${ input.users }"
      ` + spec + `
    params: { cmd: "true" }
`
			_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
			if !hasCode(diags, "loop_var_reserved") {
				dump(t, diags)
				t.Fatalf("expected loop_var_reserved for loop.%s: compute", key)
			}
		})
	}
}

// TestReservedNameHintsListCompute — the diagnostic tells the author which names are
// taken, and that list is hand-written next to each rule. A name added to the map
// without the hint leaves the author reading a list that does not contain the name
// they were just rejected for.
func TestReservedNameHintsListCompute(t *testing.T) {
	cases := []struct {
		what string
		src  string
		code string
	}{
		{
			what: "loop.as",
			code: "loop_var_reserved",
			src: `name: x
tasks:
  - module: core.exec.run
    loop: { items: "${ input.users }", as: compute }
    params: { cmd: "true" }
`,
		},
		{
			what: "compute name",
			code: "reserved_binding_name",
			src:  "name: x\ncompute:\n  compute: \"${ 1 }\"\ntasks: []\n",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.what, func(t *testing.T) {
			_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(tc.src), ValidateOptions{})
			var hint string
			for _, d := range diags {
				if d.Code == tc.code {
					hint = d.Hint
					break
				}
			}
			if hint == "" {
				dump(t, diags)
				t.Fatalf("no %s diagnostic with a hint", tc.code)
			}
			if !strings.Contains(hint, "compute") {
				t.Fatalf("hint does not list the name it just rejected: %q", hint)
			}
		})
	}
}
