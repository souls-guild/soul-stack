package config

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// compute: — scenario-level computed vars (ADR-009 amendment 2026-06-23).
// Tests structural validation (validateComputeBlock) + decoding that preserves
// declaration order (ComputeBlock.UnmarshalYAML).

func TestLoadScenarioManifest_ComputeOK(t *testing.T) {
	src := `name: create
compute:
  base: "${ merge(essence.redis_config, default(input.redis_settings, {})) }"
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
	for _, name := range []string{"input", "essence", "soulprint", "vars", "compute", "incarnation", "register"} {
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
