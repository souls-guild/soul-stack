package config

// The top-level scenario section `validate:` (ADR-009 amendment 2026-06-23, DSL
// wave 2): declarative input validation as a list of rules `[{that, message}]`.
// See the doc-comment on [ValidateRule] (scenario.go) for its purpose and the
// boundary with assert/required_when.
//
// The implementation DELIBERATELY reuses the narrow `required_when` cel-go
// sandbox (input_required_when.go): the `that` predicate is compiled by the same
// [compileRequiredWhen] against inputEnv with the single variable `input`. No
// second CEL engine — one source of input-only eval for both required_when and
// validate. A reference to any name outside `input`
// (essence/soulprint/register/vault/now) → an undeclared-reference compile
// error: the structural input-only barrier comes from the env being undeclared,
// not from a text guard (symmetric to required_when and migration-CEL ADR-019).

import (
	"fmt"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// ValidateRuleFailure is a single `validate:` rule failing at runtime eval: the
// rule's index in the list + its message (for 422 validation_failed) + the
// predicate itself (for diagnostics/logs). Returned by [EvalValidateRules] on
// the first `that == false`.
type ValidateRuleFailure struct {
	Index   int
	Message string
	That    string
}

// Error is the human-readable failure form: the rule's message + the predicate
// index/text. Symmetric to the render.ErrAssertFailed format.
func (f ValidateRuleFailure) Error() string {
	return fmt.Sprintf("%s (validate[%d] %q вычислился в false)", f.Message, f.Index, f.That)
}

// EvalValidateRules evaluates the `validate:` rules over the merged input (after
// mergeInputDefaults) in an input-only CEL context. Returns:
//   - (nil, nil) — all rules passed (or the list is empty);
//   - (*ValidateRuleFailure, nil) — the first `that == false`: the offending rule;
//   - (nil, err) — an internal eval failure (predicate not bool / CEL runtime
//     error / a compile failure — impossible after schema validation, but not
//     swallowed).
//
// merged is nil-safe (empty context). The first false wins (short-circuit in
// declaration order — like required_when over fields and assert over that[]).
func EvalValidateRules(rules []ValidateRule, merged map[string]any) (*ValidateRuleFailure, error) {
	if merged == nil {
		merged = map[string]any{}
	}
	for i, rule := range rules {
		prg, err := compileRequiredWhen(rule.That)
		if err != nil {
			return nil, fmt.Errorf("validate[%d] %q: %w", i, rule.That, err)
		}
		out, _, err := prg.Eval(map[string]any{"input": merged})
		if err != nil {
			return nil, fmt.Errorf("validate[%d] %q: %w", i, rule.That, err)
		}
		b, ok := out.Value().(bool)
		if !ok {
			return nil, fmt.Errorf("validate[%d] %q вернул %s, ожидался bool", i, rule.That, out.Type().TypeName())
		}
		if !b {
			return &ValidateRuleFailure{Index: i, Message: rule.Message, That: rule.That}, nil
		}
	}
	return nil, nil
}

// validateValidateBlock — schema-time check of the top-level `validate:` block:
// a sequence of rules, each a mapping `{ that: <CEL-bool>, message: <str> }`.
//
//   - the block must be a non-empty sequence (an empty `validate: []` is
//     meaningless — rejected as empty_value: a rule-set with no rules misleads
//     the author);
//   - `that` — required, a non-empty string parsable/compilable against inputEnv
//     (input-only). A parse error OR a reference to a name outside `input` →
//     validate_rule_invalid;
//   - `message` — required, a non-empty string (without message a rule failure
//     is anonymous — the operator cannot tell the cause of the 422; the
//     asymmetry with the optional assert.message is justified: assert carries
//     the task name as a fallback, a validate rule has no name);
//   - any other key inside a rule — unknown_key (fail-closed).
func validateValidateBlock(root *ast.MappingNode, pathPrefix string) []diag.Diagnostic {
	node := findValueNode(root, "validate")
	seq, ok := node.(*ast.SequenceNode)
	if !ok {
		line, col := 0, 0
		if vt := node.GetToken(); vt != nil {
			line, col = vt.Position.Line, vt.Position.Column
		}
		return []diag.Diagnostic{diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "validate must be a list of rules { that: <CEL-bool>, message: <str> }",
			Hint:     `validate: [ { that: "input.port > 0", message: "port must be positive" } ]`,
			YAMLPath: pathPrefix,
		})}
	}
	if len(seq.Values) == 0 {
		line, col := 0, 0
		if vt := node.GetToken(); vt != nil {
			line, col = vt.Position.Line, vt.Position.Column
		}
		return []diag.Diagnostic{diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "empty_value",
			Message:  "validate must contain at least one rule (drop the key for no validation)",
			YAMLPath: pathPrefix,
		})}
	}

	var out []diag.Diagnostic
	for i, item := range seq.Values {
		out = append(out, validateValidateRule(item, fmt.Sprintf("%s[%d]", pathPrefix, i))...)
	}
	return out
}

// validateValidateRule — validation of one `validate[i]` rule (see
// validateValidateBlock). that/message are required and non-empty; that is
// compiled input-only; unknown keys are rejected.
func validateValidateRule(node ast.Node, path string) []diag.Diagnostic {
	mm, ok := node.(*ast.MappingNode)
	if !ok {
		line, col := 0, 0
		if vt := node.GetToken(); vt != nil {
			line, col = vt.Position.Line, vt.Position.Column
		}
		return []diag.Diagnostic{diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "validate rule must be a mapping { that: <CEL-bool>, message: <str> }",
			YAMLPath: path,
		})}
	}

	var out []diag.Diagnostic
	var hasThat, hasMessage bool
	for _, kv := range mm.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		switch tok.Value {
		case "that":
			hasThat = true
			out = append(out, validateRuleThat(kv, path)...)
		case "message":
			hasMessage = true
			out = append(out, validateRuleMessage(kv, path)...)
		default:
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "unknown_key",
				Message:  `unknown field "` + tok.Value + `" in validate rule`,
				Hint:     "validate rule accepts only that: + message:",
				YAMLPath: path + "." + tok.Value,
			}))
		}
	}

	if !hasThat {
		out = append(out, diagAt(lineOf(mm), colOf(mm), diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "validate rule requires that: <CEL-bool predicate over input.*>",
			YAMLPath: path + ".that",
		}))
	}
	if !hasMessage {
		out = append(out, diagAt(lineOf(mm), colOf(mm), diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "validate rule requires message: <reason shown to operator on failure>",
			YAMLPath: path + ".message",
		}))
	}
	return out
}

// validateRuleThat — `that` is a non-empty string, parsable/compilable input-only.
func validateRuleThat(kv *ast.MappingValueNode, path string) []diag.Diagnostic {
	sn, isStr := kv.Value.(*ast.StringNode)
	if !isStr {
		return []diag.Diagnostic{diagAtKV(kv, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "validate.that must be a CEL-bool predicate string",
			YAMLPath: path + ".that",
		})}
	}
	if sn.Value == "" {
		return []diag.Diagnostic{diagAtKV(kv, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "empty_value",
			Message:  "validate.that must be a non-empty CEL-bool predicate over input.*",
			Hint:     `e.g. that: "input.tls || input.port > 0"`,
			YAMLPath: path + ".that",
		})}
	}
	if _, err := compileRequiredWhen(sn.Value); err != nil {
		return []diag.Diagnostic{diagAtKV(kv, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "validate_rule_invalid",
			Message:  fmt.Sprintf("validate.that does not compile as CEL over input.*: %v", err),
			Hint:     "predicate may reference only input.* (no essence/soulprint/register/vault/now)",
			YAMLPath: path + ".that",
		})}
	}
	return nil
}

// validateRuleMessage — `message` is a non-empty string.
func validateRuleMessage(kv *ast.MappingValueNode, path string) []diag.Diagnostic {
	sn, isStr := kv.Value.(*ast.StringNode)
	if !isStr {
		return []diag.Diagnostic{diagAtKV(kv, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "validate.message must be a string",
			YAMLPath: path + ".message",
		})}
	}
	if sn.Value == "" {
		return []diag.Diagnostic{diagAtKV(kv, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "empty_value",
			Message:  "validate.message must be a non-empty string (reason shown to operator)",
			YAMLPath: path + ".message",
		})}
	}
	return nil
}
