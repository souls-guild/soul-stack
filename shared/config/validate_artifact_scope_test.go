package config

// Which ROOTS a `validate:` block may name is a property of the ARTIFACT, and this
// is the only place it can be caught offline (NIM-833).
//
// A scenario and a covenant fragment run on a request path that has an
// incarnation — how much of one is the path's business ([ValidateContext]), and
// the offline check cannot know it, so it compiles against the whole namespace.
// The destiny pass never has one: it is isolated by construction (ADR-009 V2) and
// receives run-level values only through `apply: input:`. Compiling a destiny's
// rules against the wider env would let `incarnation.x` lint clean and then fail
// at render for every caller — the schema validator exists to move exactly that
// earlier.
//
// The CREATE path is the third case, and it is not about roots but about FIELDS:
// a create scenario runs on a path where the incarnation does not exist yet, so a
// rule reading its state can never run. That one is decided by
// validateCreateScopeRules through the runtime stance itself, so the linter cannot
// come to flag what the keeper allows.
//
// HOW TO BREAK IT ON PURPOSE (each mutation was run):
//   - pass validateWithIncarnation at the destiny call site
//     (shared/config/destiny.go) — TestValidate_DestinyStaysInputOnly.
//   - drop the validateCreateScopeRules call from schemaValidateScenario —
//     TestValidate_CreateScenarioCannotReadWhatDoesNotExistYet.
//   - drop the resolveCovenantValidateScopeDiags call from ResolveScenarioCovenant
//     — TestValidate_InheritedRuleIsJudgedAgainstTheScenarioThatInheritsIt.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// TestValidate_ScenarioAndCovenantAcceptIncarnation — the root is in scope for
// both, and a rule naming it must not be reported as a broken predicate.
func TestValidate_ScenarioAndCovenantAcceptIncarnation(t *testing.T) {
	const rule = `incarnation.id.matches('^[a-z]')`

	src := "name: x\nvalidate:\n  - that: \"" + rule + "\"\n    message: \"m\"\ntasks: []\n"
	if _, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{}); hasCode(diags, "validate_rule_invalid") {
		dump(t, diags)
		t.Error("a scenario rule over incarnation.* was rejected at load")
	}

	frag := "validate:\n  - that: \"" + rule + "\"\n    message: \"m\"\n"
	if _, _, diags := LoadCovenantFragmentFromBytes("covenant.yml", []byte(frag), ValidateOptions{}); hasCode(diags, "validate_rule_invalid") {
		dump(t, diags)
		t.Error("a covenant rule over incarnation.* was rejected at load — a fragment merges into a scenario")
	}
}

// TestValidate_DestinyStaysInputOnly — the destiny pass has no incarnation, so the
// reference is an authoring error and must be reported as one. Left to the runtime
// it is a 5xx on every caller of that destiny, at render, with nothing static
// pointing at the line.
func TestValidate_DestinyStaysInputOnly(t *testing.T) {
	src := "input:\n  port: { type: integer }\n" +
		"validate:\n  - that: \"incarnation.id != ''\"\n    message: \"m\"\n" +
		"tasks: []\n"
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "validate_rule_invalid") {
		dump(t, diags)
		t.Fatal("a destiny validate rule reading incarnation.* was accepted at load — " +
			"the destiny pass is isolated and would refuse it at render, for every caller")
	}

	// The input-only rules the destiny half already had keep working.
	ok := "input:\n  port: { type: integer }\n" +
		"validate:\n  - that: \"int(input.port) > 0\"\n    message: \"m\"\n" +
		"tasks: []\n"
	if _, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(ok), ValidateOptions{}); hasCode(diags, "validate_rule_invalid") {
		dump(t, diags)
		t.Error("an input-only destiny rule was rejected")
	}
}

// TestValidate_CreateScenarioCannotReadWhatDoesNotExistYet — a CREATE scenario's
// rule is judged against the stance a create request will arrive with. Left to the
// runtime it is a 5xx on every POST /v1/incarnations, once per request, with
// nothing static naming the line.
//
// FAIL-CLOSED and deliberately so: a `create: true` scenario may also be run day-2,
// where the rule would work. It is refused anyway, because create is a declared path
// of that scenario and the rule hard-fails there.
func TestValidate_CreateScenarioCannotReadWhatDoesNotExistYet(t *testing.T) {
	rule := func(create bool, extra, that string) []byte {
		head := "name: x\n"
		if create {
			head += "create: true\n"
		}
		return []byte(head + extra +
			"input:\n  port: { type: integer, default: 1 }\n" +
			"validate:\n  - that: \"" + that + "\"\n    message: \"m\"\ntasks: []\n")
	}

	// The incarnation does not exist yet: its state is unreadable on create.
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", rule(true, "", `incarnation.state.tier == 'gold'`), ValidateOptions{})
	d := diagWithCode(diags, "validate_rule_out_of_scope")
	if d == nil {
		dump(t, diags)
		t.Fatal("a create scenario reading incarnation.state was accepted — every POST /v1/incarnations would 500")
	}
	if d.YAMLPath != "$.validate[0].that" {
		t.Errorf("diagnostic must address the offending rule, got %q", d.YAMLPath)
	}
	if d.Line == 0 {
		t.Error("a rule written in THIS file must carry its line")
	}

	// The identifier IS readable on create — that is the whole point of the ticket.
	if _, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", rule(true, "", `incarnation.id.matches('^[a-z]')`), ValidateOptions{}); hasCode(diags, "validate_rule_out_of_scope") {
		dump(t, diags)
		t.Error("a create scenario was refused a rule over the id it receives in the request")
	}

	// Unless the scenario composes it, in which case nothing is readable yet.
	if _, _, diags, _ := LoadScenarioManifestFromBytes("main.yml",
		rule(true, "id_template: \"${input.port}-x\"\n", `incarnation.id.matches('^[a-z]')`), ValidateOptions{}); !hasCode(diags, "validate_rule_out_of_scope") {
		dump(t, diags)
		t.Error("a composing create scenario was allowed to read the id it has not composed yet")
	}

	// A day-2 scenario is untouched: its path answers for the full set.
	if _, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", rule(false, "", `incarnation.state.tier == 'gold'`), ValidateOptions{}); hasCode(diags, "validate_rule_out_of_scope") {
		dump(t, diags)
		t.Error("a day-2 scenario was judged against the create path")
	}
}

// TestValidate_InheritedRuleIsJudgedAgainstTheScenarioThatInheritsIt — a covenant
// fragment cannot be checked on its own: the same rule is correct for every day-2
// scenario that extends it and fatal for every create scenario that does. Only the
// merged manifest says which this one is, so the check runs post-merge, like the
// `form:` and `id_template:` ones beside it.
func TestValidate_InheritedRuleIsJudgedAgainstTheScenarioThatInheritsIt(t *testing.T) {
	root := t.TempDir()
	fragment := "input:\n  port: { type: integer, default: 1 }\n" +
		"validate:\n  - that: \"incarnation.state.tier == 'gold'\"\n    message: \"m\"\n"
	if err := os.WriteFile(filepath.Join(root, "covenant.yml"), []byte(fragment), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		src     string
		refused bool
	}{
		{"create", "name: x\ncreate: true\nextends: covenant\ntasks: []\n", true},
		{"day-2", "name: x\nextends: covenant\ntasks: []\n", false},
	} {
		m, doc, diags, err := LoadScenarioManifestFromBytes("main.yml", []byte(tc.src), ValidateOptions{})
		if err != nil || diag.HasErrors(diags) {
			dump(t, diags)
			t.Fatalf("%s: scenario must parse, err=%v", tc.name, err)
		}
		rdiags := ResolveScenarioCovenant(m, doc, root)
		if got := hasCode(rdiags, "validate_rule_out_of_scope"); got != tc.refused {
			dump(t, rdiags)
			t.Errorf("%s: refused = %v, want %v", tc.name, got, tc.refused)
		}
		if !tc.refused {
			continue
		}
		// The rule is not in this file, so the diagnostic must not point at a line
		// in it. The index it names is the position in the MERGED list, which is
		// the order the keeper evaluates.
		d := diagWithCode(rdiags, "validate_rule_out_of_scope")
		if d.Line != 0 {
			t.Errorf("%s: an inherited rule was given line %d in a file that does not contain it", tc.name, d.Line)
		}
		if !strings.Contains(d.Message, "validate[0]") {
			t.Errorf("%s: message must name the position in the merged list: %s", tc.name, d.Message)
		}
	}
}
