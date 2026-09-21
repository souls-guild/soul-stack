package config

// The offline half of the `validate:` incarnation scope (NIM-833): a CREATE
// scenario whose rule reads a fact the create path cannot answer for.
//
// Without this, such a scenario passes `soul-lint validate-service` and then 5xx's
// on EVERY `POST /v1/incarnations` — a rule that can never run, reported once per
// request instead of once at authoring time, with nothing static pointing at the
// line. The manifest already carries the deciding facts (`create:` and
// `id_template:`), which is what makes the check possible here at all; this is the
// same move NIM-272 made for a roster-reading `assert:`.
//
// It reports through the RUNTIME stance ([ValidateContext.guard]) rather than a
// second rule of its own, so the linter cannot come to flag what the keeper allows
// or stay quiet on what it refuses.
//
// FAIL-CLOSED, and the trade-off is worth stating: a `create: true` scenario may
// also be run day-2, where the row exists and a `incarnation.state` rule would work.
// It is still refused, because the create path is a declared path of that scenario
// and the rule hard-fails there. Splitting a day-2-only rule into a day-2-only
// scenario is the way out, and the message says so.

import (
	"fmt"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// createScopePlaceholderID stands in for the id an operator would send. Its VALUE
// never matters — the guard compares field NAMES — but it has to be non-empty, or
// [RequestedIncarnation] degrades to the zero value and the check would refuse
// `incarnation.id` in a scenario where the create path really does carry it.
const createScopePlaceholderID = "id"

// validateCreateScopeRules checks a create scenario's `validate:` rules against the
// stance a create request will actually arrive with:
//
//   - `id_template:` set — the id is composed from `input:` AFTER the gate, so NO
//     incarnation fact is knowable and any `incarnation.*` reference is refused;
//   - otherwise — the operator supplies the id, so `incarnation.id` / `.name` are
//     readable and nothing else is.
//
// A scenario that is not a create starter is not checked here: its rules run on the
// day-2 path, which answers for the full set.
//
// rules is passed separately from m because the post-merge caller
// (resolveCovenantValidateScopeDiags) checks the EFFECTIVE list — covenant rules
// included — while m.Validate at semantic-validation time holds only the local
// delta.
func validateCreateScopeRules(root *ast.MappingNode, m *ScenarioManifest, rules []ValidateRule) []diag.Diagnostic {
	if m == nil || m.Create == nil || !*m.Create || len(rules) == 0 {
		return nil
	}

	inc := RequestedIncarnation(createScopePlaceholderID)
	if m.IDTemplate != "" {
		inc = inc.WithComposedID()
	}

	// Positions come from the scenario's own `validate:` node, and only line up when
	// rules IS that node — i.e. no covenant merge happened. After a merge the list
	// is covenant-first, so index i names a different rule than seq.Values[i], and
	// pointing at that line would be a confident lie about which rule is wrong.
	// Then the diagnostic carries the file and no line, and the message's index
	// names the position in the merged list the keeper evaluates.
	seq, _ := findValueNode(root, "validate").(*ast.SequenceNode)
	positioned := seq != nil && len(seq.Values) == len(rules)

	var out []diag.Diagnostic
	for i, rule := range rules {
		err := inc.guard(rule.That)
		if err == nil {
			continue
		}
		line, col := 0, 0
		if positioned {
			line, col = lineOf(seq.Values[i]), colOf(seq.Values[i])
		}
		out = append(out, diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:     "validate_rule_out_of_scope",
			Message:  fmt.Sprintf("validate[%d] cannot run on the create path: %v", i, err),
			Hint:     "on create the incarnation does not exist yet; move a rule that needs its state or history to a day-2 scenario, and express a constraint on the identifier over incarnation.id (or, when id_template composes it, over the input.* components it composes from)",
			YAMLPath: fmt.Sprintf("$.validate[%d].that", i),
		}))
	}
	return out
}
