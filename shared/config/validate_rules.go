package config

// The top-level scenario section `validate:` (ADR-009 amendment 2026-06-23, DSL
// wave 2): declarative validation of what arrived on the request, as a list of
// rules `[{that, message}]`. See the doc-comment on [ValidateRule] (scenario.go)
// for its purpose and the boundary with assert/required_when.
//
// The context is `input` + `incarnation` and nothing else (NIM-833): `vars` are
// the service's parameters rather than the request, and `compute` resolves inside
// the run, later than this point by construction. Everything outside those two
// (vars/soulprint/register/vault/now) is an undeclared-reference compile error —
// the structural barrier comes from the env, not from a text guard (symmetric to
// required_when and migration-CEL ADR-019).
//
// The env is validateEnv here rather than the narrower `required_when` sandbox it
// used to share (input_required_when.go): required_when is a pure function of
// input by design and does NOT gain the incarnation. Same cel-go, one more
// declared variable — not a second evaluator.
//
// WHAT the `incarnation` namespace holds is the caller's stance, not this file's:
// see validate_scope.go, which is where "the create path has no incarnation yet"
// is made structural instead of being papered over with an empty map.

import (
	"fmt"
	"sync"

	"github.com/goccy/go-yaml/ast"
	"github.com/google/cel-go/cel"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// validateEnv — the `validate:` CEL environment: `input` and `incarnation`, both
// DynType (like the rest of Soul Stack context). Immutable after first build,
// built lazily; the program cache is validateProgs, keyed by predicate text.
var (
	validateEnvOnce sync.Once
	validateEnv     *cel.Env
	validateEnvErr  error

	validateProgMu sync.RWMutex
	validateProgs  = map[string]cel.Program{}
)

func validateRuleEnv() (*cel.Env, error) {
	validateEnvOnce.Do(func() {
		validateEnv, validateEnvErr = cel.NewEnv(
			cel.StdLib(),
			cel.Variable("input", cel.DynType),
			cel.Variable(incarnationRoot, cel.DynType),
		)
		if validateEnvErr != nil {
			validateEnvErr = fmt.Errorf("building CEL environment for validate: %w", validateEnvErr)
		}
	})
	return validateEnv, validateEnvErr
}

// compileValidateRule compiles (and caches) a `validate:` predicate against
// [validateRuleEnv]. A compile error (syntax / a name outside input+incarnation /
// incompatible types) is returned to the caller for classification: schema
// validator → validate_rule_invalid, runtime → [ErrValidateRuleEval].
func compileValidateRule(expr string) (cel.Program, error) {
	validateProgMu.RLock()
	prg, ok := validateProgs[expr]
	validateProgMu.RUnlock()
	if ok {
		return prg, nil
	}

	env, err := validateRuleEnv()
	if err != nil {
		return nil, err
	}
	ast, issues := env.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return nil, issues.Err()
	}
	prg, err = env.Program(ast)
	if err != nil {
		return nil, err
	}

	validateProgMu.Lock()
	validateProgs[expr] = prg
	validateProgMu.Unlock()
	return prg, nil
}

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
	return fmt.Sprintf("%s (validate[%d] %q evaluated to false)", f.Message, f.Index, f.That)
}

// EvalValidateRules evaluates the `validate:` rules over the merged input (after
// mergeInputDefaults) and the incarnation facts inc says this path knows. Returns:
//   - (nil, nil) — all rules passed (or the list is empty);
//   - (*ValidateRuleFailure, nil) — the first `that == false`: the offending rule;
//   - (nil, err) — an internal failure (a rule reading an incarnation fact this
//     path does not have → [ErrIncarnationNotInScope]; predicate not bool / CEL
//     runtime error / a compile failure — impossible after schema validation, but
//     not swallowed).
//
// The scope check is a PRE-PASS over every rule, before any of them is evaluated,
// and that ordering is load-bearing: inside the eval loop it would sit behind the
// first-false short-circuit, so a scenario whose second rule reads a fact this path
// lacks would look healthy on every request that trips its first rule and 5xx on
// the ones that do not. Whether a scenario is broken must not depend on what the
// operator typed.
//
// A scope refusal is not a [ValidateRuleFailure]: the operator's input is not what
// is wrong, the rule is, and reporting it as a failed invariant would tell the
// operator to fix a value that is fine.
//
// merged is nil-safe (empty context); the zero inc is the input-only context this
// section had before NIM-833. The first false wins (short-circuit in declaration
// order — like required_when over fields and assert over that[]).
func EvalValidateRules(rules []ValidateRule, merged map[string]any, inc ValidateContext) (*ValidateRuleFailure, error) {
	if merged == nil {
		merged = map[string]any{}
	}
	for i, rule := range rules {
		if err := inc.guard(rule.That); err != nil {
			return nil, fmt.Errorf("validate[%d]: %w", i, err)
		}
	}

	act := inc.activation(merged)
	for i, rule := range rules {
		prg, err := compileValidateRule(rule.That)
		if err != nil {
			return nil, fmt.Errorf("validate[%d] %q: %w", i, rule.That, err)
		}
		out, _, err := prg.Eval(act)
		if err != nil {
			return nil, fmt.Errorf("validate[%d] %q: %w", i, rule.That, err)
		}
		b, ok := out.Value().(bool)
		if !ok {
			return nil, fmt.Errorf("validate[%d] %q returned %s, expected bool", i, rule.That, out.Type().TypeName())
		}
		if !b {
			return &ValidateRuleFailure{Index: i, Message: rule.Message, That: rule.That}, nil
		}
	}
	return nil, nil
}

// validateRuleScope says whether THIS artifact's `validate:` rules may reference
// the incarnation root at all. It is a property of the artifact, not of the
// request path: a scenario and a covenant fragment run on a path that has an
// incarnation (to some degree — [ValidateContext] decides how much), while the
// destiny pass is isolated by construction (ADR-009 V2) and never will.
//
// The distinction has to be drawn HERE because it is the only place it can be
// caught offline. Compiling a destiny's rules against the wider env would let
// `incarnation.x` lint clean and then fail at render for every caller — the class
// of defect the schema validator exists to move earlier.
type validateRuleScope uint8

const (
	// validateWithIncarnation — scenario and covenant: `input` + `incarnation`.
	validateWithIncarnation validateRuleScope = iota
	// validateInputOnly — the destiny manifest, whose pass has no incarnation.
	validateInputOnly
)

// validateValidateBlock — schema-time check of the top-level `validate:` block:
// a sequence of rules, each a mapping `{ that: <CEL-bool>, message: <str> }`.
//
//   - the block must be a non-empty sequence (an empty `validate: []` is
//     meaningless — rejected as empty_value: a rule-set with no rules misleads
//     the author);
//   - `that` — required, a non-empty string parsable/compilable against the env
//     scope names. A parse error OR a reference to a name outside it →
//     validate_rule_invalid. WHICH incarnation FIELDS a rule may read is a
//     property of the path it runs on rather than of the file, so where the root
//     is in scope the offline check compiles against the whole namespace and the
//     per-path narrowing is [ValidateContext.guard]'s (validate_scope.go);
//   - `message` — required, a non-empty string (without message a rule failure
//     is anonymous — the operator cannot tell the cause of the 422; the
//     asymmetry with the optional assert.message is justified: assert carries
//     the task name as a fallback, a validate rule has no name);
//   - any other key inside a rule — unknown_key (fail-closed).
func validateValidateBlock(root *ast.MappingNode, pathPrefix string, scope validateRuleScope) []diag.Diagnostic {
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
		out = append(out, validateValidateRule(item, fmt.Sprintf("%s[%d]", pathPrefix, i), scope)...)
	}
	return out
}

// validateValidateRule — validation of one `validate[i]` rule (see
// validateValidateBlock). that/message are required and non-empty; that is
// compiled against the scope's roots; unknown keys are rejected.
func validateValidateRule(node ast.Node, path string, scope validateRuleScope) []diag.Diagnostic {
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
			out = append(out, validateRuleThat(kv, path, scope)...)
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

// validateRuleThat — `that` is a non-empty string, parsable/compilable against
// the roots the artifact's scope declares.
func validateRuleThat(kv *ast.MappingValueNode, path string, scope validateRuleScope) []diag.Diagnostic {
	roots := "input.* / incarnation.*"
	compile := compileValidateRule
	forbidden := "no vars/soulprint/register/compute/vault/now"
	if scope == validateInputOnly {
		roots = "input.*"
		compile = compileRequiredWhen
		forbidden = "the destiny pass is isolated (ADR-009 V2), so incarnation.* is not available there either — a run-level value reaches a destiny only through `apply: input:`"
	}

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
			Message:  "validate.that must be a non-empty CEL-bool predicate over " + roots,
			Hint:     `e.g. that: "input.tls || input.port > 0"`,
			YAMLPath: path + ".that",
		})}
	}
	if _, err := compile(sn.Value); err != nil {
		return []diag.Diagnostic{diagAtKV(kv, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "validate_rule_invalid",
			Message:  fmt.Sprintf("validate.that does not compile as CEL over %s: %v", roots, err),
			Hint:     "predicate may reference only " + roots + " (" + forbidden + ")",
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
