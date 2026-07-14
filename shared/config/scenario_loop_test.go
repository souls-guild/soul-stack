package config

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// TestLoadScenarioManifest_LoopOK — a valid loop: on a module task it passes
// validation and decodes into LoopSpec.
func TestLoadScenarioManifest_LoopOK(t *testing.T) {
	src := `name: x
tasks:
  - module: core.exec.run
    loop:
      items: "${ input.users }"
      as: user
      index_as: i
      when: "user.active"
    params: { cmd: "echo ${ user.name }" }
`
	cfg, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("unexpected error diagnostics")
	}
	if len(cfg.Tasks) != 1 || cfg.Tasks[0].Loop == nil {
		t.Fatalf("loop not decoded into LoopSpec")
	}
	lp := cfg.Tasks[0].Loop
	if lp.As != "user" || lp.IndexAs != "i" || lp.When != "user.active" {
		t.Errorf("loop fields = %+v", lp)
	}
}

// TestLoadScenarioManifest_LoopMissingItems — items is required.
func TestLoadScenarioManifest_LoopMissingItems(t *testing.T) {
	src := `name: x
tasks:
  - module: core.exec.run
    loop:
      as: user
    params: { cmd: "echo ${ user }" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "missing_required_field") {
		dump(t, diags)
		t.Fatalf("expected missing_required_field for loop.items")
	}
}

// TestLoadScenarioManifest_LoopBadAs — invalid as: identifier.
func TestLoadScenarioManifest_LoopBadAs(t *testing.T) {
	src := `name: x
tasks:
  - module: core.exec.run
    loop:
      items: "${ input.users }"
      as: "Bad-Name"
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "loop_var_invalid") {
		dump(t, diags)
		t.Fatalf("expected loop_var_invalid")
	}
}

// TestLoadScenarioManifest_LoopReservedAs — as: must not shadow the context.
func TestLoadScenarioManifest_LoopReservedAs(t *testing.T) {
	src := `name: x
tasks:
  - module: core.exec.run
    loop:
      items: "${ input.users }"
      as: input
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "loop_var_reserved") {
		dump(t, diags)
		t.Fatalf("expected loop_var_reserved")
	}
}

// TestLoadScenarioManifest_LoopAsEqualsIndexAs — as: and index_as: with the same
// name are rejected: in the render context the index would silently shadow the element.
func TestLoadScenarioManifest_LoopAsEqualsIndexAs(t *testing.T) {
	src := `name: x
tasks:
  - module: core.exec.run
    loop:
      items: "${ input.users }"
      as: u
      index_as: u
    params: { cmd: "useradd ${ u }" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "loop_var_conflict", "$.tasks[0].loop.index_as") {
		dump(t, diags)
		t.Fatalf("expected loop_var_conflict for as == index_as")
	}
}

// TestLoadScenarioManifest_LoopUnknownKey — unknown key inside loop:.
func TestLoadScenarioManifest_LoopUnknownKey(t *testing.T) {
	src := `name: x
tasks:
  - module: core.exec.run
    loop:
      items: "${ input.users }"
      parallel: true
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "unknown_key", "$.tasks[0].loop.parallel") {
		dump(t, diags)
		t.Fatalf("expected unknown_key for loop.parallel")
	}
}

// TestLoadScenarioManifest_LoopOnInclude — loop on an include task is rejected
// (slice E1: module only).
func TestLoadScenarioManifest_LoopOnInclude(t *testing.T) {
	src := `name: x
tasks:
  - include: sub.yml
    loop:
      items: "${ input.users }"
      as: user
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "loop_unsupported_target") {
		dump(t, diags)
		t.Fatalf("expected loop_unsupported_target on include")
	}
}

// TestLoadScenarioManifest_LoopOnApply — loop on an apply task is rejected.
func TestLoadScenarioManifest_LoopOnApply(t *testing.T) {
	src := `name: x
tasks:
  - apply: { destiny: sub, input: {} }
    loop:
      items: "${ input.users }"
      as: user
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "loop_unsupported_target") {
		dump(t, diags)
		t.Fatalf("expected loop_unsupported_target on apply")
	}
}
