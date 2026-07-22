package config

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// TestStateRef_IncarnationStateOK — the canonical form `incarnation.state.<path>`
// (a read-only snapshot of incarnation.state in scenario render, ADR-009/010
// Variant A) is valid in a predicate and in apply.input. Must not yield
// state_naked_reference: `state` here is a field of incarnation (preceded by `.`),
// not a root identifier.
func TestStateRef_IncarnationStateOK(t *testing.T) {
	src := `name: ok
tasks:
  - name: t1
    where: "size(incarnation.state.redis_users) > 0"
    apply:
      destiny: redis
      input:
        current: "${ incarnation.state.redis_users }"
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "state_naked_reference") {
		dump(t, diags)
		t.Fatalf("false-positive state_naked_reference on canonical incarnation.state.*")
	}
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expect zero errors on incarnation.state.*")
	}
}

// TestStateRef_NakedInPredicateFlagged — a naked `state.<path>` in where: (state is
// not declared in scenario-CEL, migration-only) → state_naked_reference.
func TestStateRef_NakedInPredicateFlagged(t *testing.T) {
	src := `name: bad
tasks:
  - name: t1
    where: "size(state.redis_users) > 0"
    module: core.exec.run
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "state_naked_reference") {
		dump(t, diags)
		t.Fatalf("expect state_naked_reference on bare state.* in where:")
	}
}

// TestStateRef_NakedInApplyInputFlagged — the canonical update_acl case: a naked
// `${ state.redis_users }` in apply.input (the incarnation. prefix is forgotten) → error.
func TestStateRef_NakedInApplyInputFlagged(t *testing.T) {
	src := `name: bad
tasks:
  - name: t1
    apply:
      destiny: redis
      input:
        current: "${ state.redis_users }"
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "state_naked_reference", "$.tasks[0].apply.input") {
		dump(t, diags)
		t.Fatalf("expect state_naked_reference on bare state.* in apply.input")
	}
}

// TestStateRef_NakedInParamsFlagged — a naked state.* in params: (interpolation).
func TestStateRef_NakedInParamsFlagged(t *testing.T) {
	src := `name: bad
tasks:
  - name: t1
    module: core.exec.run
    params:
      cmd: "echo ${ state.redis_users }"
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "state_naked_reference") {
		dump(t, diags)
		t.Fatalf("expect state_naked_reference on bare state.* in params:")
	}
}

// TestStateRef_NestedCELLiteralIgnored — `state.x` inside a CEL string literal
// (data, not a reference) is not flagged: the literal is stripped before extraction.
func TestStateRef_NestedCELLiteralIgnored(t *testing.T) {
	src := `name: ok
tasks:
  - name: t1
    where: "incarnation.name == 'state.redis_users'"
    module: core.exec.run
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "state_naked_reference") {
		dump(t, diags)
		t.Fatalf("false-positive state_naked_reference on state.x inside a string literal")
	}
}

// TestStateRef_SubstringIdentNotFlagged — identifiers where `state` appears as a
// substring (`mystate.x`, `restate.y`) or as a field of another object
// (`foo.state.z`) are NOT the root `state` — not flagged.
func TestStateRef_SubstringIdentNotFlagged(t *testing.T) {
	src := `name: ok
tasks:
  - name: t1
    where: "incarnation.state_schema_version > 0"
    module: core.exec.run
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "state_naked_reference") {
		dump(t, diags)
		t.Fatalf("false-positive state_naked_reference on incarnation.state_schema_version (state is a substring, not the root)")
	}
}
