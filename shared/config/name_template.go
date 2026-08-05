package config

// The create-scenario key `name_template` (ADR-0079): the incarnation name is
// COMPOSED server-side from `input:` components instead of being free text typed
// by the operator (`{name}-{project}-{subproject}-redis-{service_type}`).
//
// The evaluator DELIBERATELY reuses the narrow `required_when` cel-go sandbox
// (input_required_when.go): every `${ … }` block is compiled against inputEnv with
// the single variable `input`. Composing a name is a pure function of the resolved
// input — a reference to vars/soulprint/register/vault/now is an
// undeclared-reference compile error, the same structural barrier as
// required_when/validate. No second CEL engine, and shared/config keeps its
// deliberate independence from shared/cel.
//
// Unlike general interpolation (ADR-010 §5(a)), a single `${ … }` block does NOT
// yield a native type here: a name is a string by definition, so every block is
// stringified and concatenated with the surrounding literals.

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/goccy/go-yaml/ast"
	"github.com/google/cel-go/common"
	celast "github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/parser"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// IncarnationNameMaxLen mirrors the length ceiling of the keeper-side
// `incarnation.NamePattern` (`^[a-z0-9][a-z0-9-]{0,62}$`) — shared/ cannot import
// keeper/internal, so the bound is restated here the same way the huma reply
// schemas restate the pattern. Used ONLY by the static lint ("the literal skeleton
// alone can never fit"); the authoritative check runs keeper-side on the composed
// name, where the full pattern lives.
const IncarnationNameMaxLen = 63

// ErrNameTemplateRender marks a runtime failure of [RenderNameTemplate]: a block
// did not compile/evaluate, or its result is a list/map that cannot be part of a
// name. Callers map it to 422 (the offending values came from operator input).
var ErrNameTemplateRender = errors.New("config: name_template render failed")

// ErrNameTemplateIndexForm — the template reaches input in index form
// (`input['k']` / `input[expr]`) instead of the canonical select form
// (`input.k`). Rejected deterministically: the static reference check
// ([NameTemplateInputRefs], soul-lint) needs the component name statically known
// from the AST, and a dynamic index does not guarantee that. Mirrors
// shared/cel.ErrVarIndexForm for `vars`.
var ErrNameTemplateIndexForm = errors.New("config: name_template must address components as ${input.<name>}, not input[...]")

// RenderNameTemplate composes an incarnation name from tmpl over the RESOLVED
// input (post default-merge, see [ResolveInputContract]). Literal text passes
// through; each `${ … }` block is evaluated input-only and stringified. `\${`
// escapes the marker (ADR-010 §9.1).
//
// The result is NOT validated against the incarnation name grammar here — that is
// the keeper's job (it owns the pattern and turns a violation into a 422 naming
// the offending components). This function only fails on things the template
// itself got wrong: a block that does not compile/evaluate, or a list/map result
// ([ErrNameTemplateRender]).
//
// input is nil-safe (an empty context; blocks referencing absent components then
// fail with a no-such-key eval error, which is the honest outcome).
func RenderNameTemplate(tmpl string, input map[string]any) (string, error) {
	segs, err := scanNameTemplate(tmpl)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNameTemplateRender, err)
	}
	if input == nil {
		input = map[string]any{}
	}

	var b strings.Builder
	for _, s := range segs {
		if !s.expr {
			b.WriteString(s.text)
			continue
		}
		prg, cerr := compileRequiredWhen(s.text)
		if cerr != nil {
			return "", fmt.Errorf("%w: block %q: %w", ErrNameTemplateRender, s.text, cerr)
		}
		out, _, eerr := prg.Eval(map[string]any{"input": input})
		if eerr != nil {
			return "", fmt.Errorf("%w: block %q: %w", ErrNameTemplateRender, s.text, eerr)
		}
		str, serr := nameBlockString(s.text, out)
		if serr != nil {
			return "", serr
		}
		b.WriteString(str)
	}
	return b.String(), nil
}

// nameBlockString coerces one evaluated block to its string form. Scalars
// (string/int/uint/double/bool) convert canonically through cel-go; a list/map has
// no meaningful place in a name → [ErrNameTemplateRender].
func nameBlockString(expr string, val ref.Val) (string, error) {
	if s, ok := val.Value().(string); ok {
		return s, nil
	}
	if s, ok := val.ConvertToType(types.StringType).Value().(string); ok {
		return s, nil
	}
	return "", fmt.Errorf("%w: block %q evaluated to %s, which cannot be part of a name (use a scalar component)",
		ErrNameTemplateRender, expr, val.Type().TypeName())
}

// NameTemplateInputRefs extracts the `input.<name>` components tmpl references.
// AST-based, not textual: `input.x` in literal text OUTSIDE `${ … }` is not a
// reference, and `input.x` inside a CEL string literal is a constant, not a
// select. Names come back deduplicated, in order of first appearance.
//
// Index form → [ErrNameTemplateIndexForm]; a template with no blocks → an empty
// slice.
func NameTemplateInputRefs(tmpl string) ([]string, error) {
	segs, err := scanNameTemplate(tmpl)
	if err != nil {
		return nil, err
	}
	p, err := nameTemplateParserInstance()
	if err != nil {
		return nil, err
	}

	var refs []string
	seen := map[string]bool{}
	for _, s := range segs {
		if !s.expr {
			continue
		}
		parsed, iss := p.Parse(common.NewTextSource(s.text))
		if iss != nil && len(iss.GetErrors()) > 0 {
			// Unparseable blocks never get here (scanNameTemplate gates the block
			// boundary via env.Parse); defensive, mirrors shared/cel.VarRefs.
			continue
		}
		var visitErr error
		celast.PostOrderVisit(parsed.Expr(), celast.NewExprVisitor(func(n celast.Expr) {
			if visitErr != nil {
				return
			}
			switch n.Kind() {
			case celast.SelectKind:
				sel := n.AsSelect()
				if sel.IsTestOnly() {
					return
				}
				op := sel.Operand()
				if op.Kind() != celast.IdentKind || op.AsIdent() != "input" {
					return
				}
				if !seen[sel.FieldName()] {
					seen[sel.FieldName()] = true
					refs = append(refs, sel.FieldName())
				}
			case celast.CallKind:
				if isInputIndex(n) {
					visitErr = fmt.Errorf("%w (block %q)", ErrNameTemplateIndexForm, s.text)
				}
			}
		}))
		if visitErr != nil {
			return nil, visitErr
		}
	}
	return refs, nil
}

// isInputIndex reports whether n is `input[<expr>]` (the CEL index operator over
// the `input` identifier).
func isInputIndex(n celast.Expr) bool {
	c := n.AsCall()
	if c.IsMemberFunction() || c.FunctionName() != "_[_]" {
		return false
	}
	args := c.Args()
	if len(args) == 0 {
		return false
	}
	return args[0].Kind() == celast.IdentKind && args[0].AsIdent() == "input"
}

// nameSegment — a piece of a template: literal text (expr=false) or a CEL
// expression stripped of its `${ }` wrapper (expr=true).
type nameSegment struct {
	text string
	expr bool
}

// scanNameTemplate splits raw into literal segments and `${ … }` blocks. The
// closing `}` is found by parsing (like shared/cel.scanInterpolation), not by
// counting braces: the substring after `${` grows to the first `}` at which the
// content parses as valid CEL. `\${` escapes the marker.
func scanNameTemplate(raw string) ([]nameSegment, error) {
	var segs []nameSegment
	var lit strings.Builder
	i := 0
	for i < len(raw) {
		if raw[i] == '\\' && i+2 < len(raw) && raw[i+1] == '$' && raw[i+2] == '{' {
			lit.WriteString("${")
			i += 3
			continue
		}
		if raw[i] == '$' && i+1 < len(raw) && raw[i+1] == '{' {
			if lit.Len() > 0 {
				segs = append(segs, nameSegment{text: lit.String()})
				lit.Reset()
			}
			inner, next, err := parseNameBlock(raw, i+2)
			if err != nil {
				return nil, err
			}
			segs = append(segs, nameSegment{text: inner, expr: true})
			i = next
			continue
		}
		lit.WriteByte(raw[i])
		i++
	}
	if lit.Len() > 0 {
		segs = append(segs, nameSegment{text: lit.String()})
	}
	return segs, nil
}

// parseNameBlock finds the end of the `${ … }` block opened at start (the first
// byte after `${`), returning the trimmed expression and the index past `}`.
func parseNameBlock(raw string, start int) (string, int, error) {
	env, err := requiredWhenEnv()
	if err != nil {
		return "", 0, err
	}
	for end := start; end < len(raw); end++ {
		if raw[end] != '}' {
			continue
		}
		inner := strings.TrimSpace(raw[start:end])
		if _, iss := env.Parse(inner); iss == nil || iss.Err() == nil {
			return inner, end + 1, nil
		}
	}
	return "", 0, fmt.Errorf("${ without a closing } or an invalid expression: %q", raw[start:])
}

var (
	nameTemplateParserOnce sync.Once
	nameTemplateParser     *parser.Parser
	nameTemplateParserErr  error
)

// nameTemplateParserInstance returns the shared macro-free parser used for
// reference extraction: with macros disabled, `has()`/`.filter()` stay plain calls
// instead of expanding into comprehensions that hide the `input.<name>` selects.
func nameTemplateParserInstance() (*parser.Parser, error) {
	nameTemplateParserOnce.Do(func() {
		nameTemplateParser, nameTemplateParserErr = parser.NewParser()
		if nameTemplateParserErr != nil {
			nameTemplateParserErr = fmt.Errorf("building CEL parser for name_template: %w", nameTemplateParserErr)
		}
	})
	return nameTemplateParser, nameTemplateParserErr
}

// validateNameTemplate is the schema-time check of `name_template:` against the
// scenario's own `input:` — the non-extends path (see the covenant gate in
// [schemaValidateScenario]). A thin wrapper over
// [validateNameTemplateAgainstInputKeys].
func validateNameTemplate(root *ast.MappingNode, m *ScenarioManifest, pathPrefix string) []diag.Diagnostic {
	inputKeys := make(map[string]bool, len(m.Input))
	for name := range m.Input {
		inputKeys[name] = true
	}
	return validateNameTemplateAgainstInputKeys(root, m.NameTemplate, m.Create, inputKeys, pathPrefix)
}

// validateNameTemplateAgainstInputKeys is the CORE `name_template` check with a
// PARAMETERIZED source of input names (the scenario's own `input:` before a
// covenant merge, or the MERGED effective input after one — mirrors the `form:`
// split):
//
//   - empty template → empty_value (drop the key for a free-text name);
//   - a block that does not compile input-only, or index form → ERROR
//     name_template_invalid;
//   - `${input.X}` with X not declared in input → ERROR name_template_input_unknown
//     (a create would fail at runtime for every operator — the exact class the
//     linter exists to catch);
//   - the literal skeleton alone exceeding the 63-char name ceiling → ERROR
//     name_template_too_long (no input can rescue it);
//   - no `${ … }` block at all → WARNING name_template_constant (every incarnation
//     of the service would collide on the same name);
//   - the scenario is not a create starter → WARNING name_template_ignored (the key
//     is only read on the create path).
func validateNameTemplateAgainstInputKeys(root *ast.MappingNode, tmpl string, create *bool, inputKeys map[string]bool, pathPrefix string) []diag.Diagnostic {
	if tmpl == "" {
		return []diag.Diagnostic{atPath(root, pathPrefix, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "empty_value",
			Message: "name_template must be a non-empty template over input.* (drop the key to keep a free-text name)",
			Hint:    `e.g. name_template: "${input.name}-${input.project}-redis-${input.service_type}"`,
		})}
	}

	var out []diag.Diagnostic
	if create == nil || !*create {
		out = append(out, atPath(root, pathPrefix, diag.Diagnostic{
			Level: diag.LevelWarning, Phase: diag.PhaseSchemaValidate,
			Code:    "name_template_ignored",
			Message: "name_template is read only on the create path; this scenario is not a create starter",
			Hint:    "add create: true, or move name_template to the create scenario",
		}))
	}

	segs, err := scanNameTemplate(tmpl)
	if err != nil {
		return append(out, atPath(root, pathPrefix, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "name_template_invalid",
			Message: fmt.Sprintf("name_template is not a valid ${ … } template: %v", err),
			Hint:    "components are addressed as ${input.<name>}; escape a literal marker as \\${",
		}))
	}

	literal := 0
	blocks := 0
	for _, s := range segs {
		if !s.expr {
			literal += len(s.text)
			continue
		}
		blocks++
		if _, cerr := compileRequiredWhen(s.text); cerr != nil {
			out = append(out, atPath(root, pathPrefix, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "name_template_invalid",
				Message: fmt.Sprintf("name_template block %q does not compile as CEL over input.*: %v", s.text, cerr),
				Hint:    "a name is composed from input only (no vars/soulprint/register/vault/now)",
			}))
		}
	}

	if blocks == 0 {
		out = append(out, atPath(root, pathPrefix, diag.Diagnostic{
			Level: diag.LevelWarning, Phase: diag.PhaseSchemaValidate,
			Code:    "name_template_constant",
			Message: "name_template contains no ${ … } block — every incarnation of this service would be composed to the same name",
			Hint:    "reference at least one component, e.g. ${input.name}",
		}))
	}
	if literal > IncarnationNameMaxLen {
		out = append(out, atPath(root, pathPrefix, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code: "name_template_too_long",
			Message: fmt.Sprintf("literal part of name_template is %d characters, over the %d-character incarnation name ceiling — no input can make it fit",
				literal, IncarnationNameMaxLen),
			Hint: "shorten the fixed text between the components",
		}))
	}

	refs, rerr := NameTemplateInputRefs(tmpl)
	if rerr != nil {
		return append(out, atPath(root, pathPrefix, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "name_template_invalid",
			Message: rerr.Error(),
			Hint:    "address a component as ${input.<name>}",
		}))
	}
	for _, ref := range refs {
		if inputKeys[ref] {
			continue
		}
		out = append(out, atPath(root, pathPrefix, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "name_template_input_unknown",
			Message: fmt.Sprintf("name_template references input.%s, which is not declared in input:", ref),
			Hint:    "declare the component in input:, or fix the reference",
		}))
	}
	return out
}
