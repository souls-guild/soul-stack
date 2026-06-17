package config

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// TestLoadScenarioManifest_LoopOK — корректный loop: на module-задаче проходит
// валидацию и декодится в LoopSpec.
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

// TestLoadScenarioManifest_LoopMissingItems — items обязателен.
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

// TestLoadScenarioManifest_LoopBadAs — невалидный идентификатор as:.
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

// TestLoadScenarioManifest_LoopReservedAs — as: не должен затирать контекст.
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

// TestLoadScenarioManifest_LoopAsEqualsIndexAs — as: и index_as: с одинаковым
// именем отвергаются: в render-контексте индекс молча затёр бы элемент.
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

// TestLoadScenarioManifest_LoopUnknownKey — неизвестный ключ внутри loop:.
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

// TestLoadScenarioManifest_LoopOnInclude — loop на include-задаче отвергается
// (slice E1: только module).
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

// TestLoadScenarioManifest_LoopOnApply — loop на apply-задаче отвергается.
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
