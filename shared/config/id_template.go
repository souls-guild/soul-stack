package config

// The create-scenario block `id:` (ADR-0079, renamed by [ADR-0085], given a block by
// NIM-899): the incarnation id is COMPOSED server-side from `input:` components
// instead of being free text typed by the operator. The scalar spelling
// `id_template:` is folded into the same block before any rule here sees it
// (id_template_window.go).
//
// [ADR-0085]: ../../docs/adr/0085-entity-id-and-label.md
//
// The evaluator DELIBERATELY reuses the narrow `required_when` cel-go sandbox
// (input_required_when.go): every `${ … }` block is compiled against inputEnv with
// the single variable `input`. Composing an id is a pure function of the resolved
// input — a reference to vars/soulprint/register/vault/now is an
// undeclared-reference compile error, the same structural barrier as
// required_when/validate. No second CEL engine, and shared/config keeps its
// deliberate independence from shared/cel.
//
// Unlike general interpolation (ADR-010 §5(a)), a single `${ … }` block does NOT
// yield a native type here: an id is a string by definition, so every block is
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

// IncarnationIDMaxLen mirrors the ceiling of the keeper-side `incarnation.IDPattern`
// (`^[a-z0-9][a-z0-9-]{0,62}$`): shared/ cannot import keeper/internal, so the bound
// is restated here. A scenario may narrow it with `id.max_length` and never widen it.
const IncarnationIDMaxLen = 63

// IDSpec is the create-scenario `id:` block. The zero value means the scenario
// composes nothing and the operator names the incarnation.
type IDSpec struct {
	// Template is the `${ … }` template over input only.
	Template string `yaml:"template,omitempty"`

	// MaxLength bounds the COMPOSED id in characters. An ABSOLUTE number, not a
	// reserve off [IncarnationIDMaxLen]: the reason a service is bounded below the
	// platform lies outside it, so the number is the service's to state (ADR-0079
	// amendment (ii)). 0 = unset; a value outside 1..63 is a lint error and reads as
	// unset at runtime, because refusing every create over a typo in a bound is worse.
	MaxLength int `yaml:"max_length,omitempty"`
}

// Ceiling is the bound a composed id must satisfy, never above the platform's.
func (s IDSpec) Ceiling() int {
	if s.MaxLength >= 1 && s.MaxLength <= IncarnationIDMaxLen {
		return s.MaxLength
	}
	return IncarnationIDMaxLen
}

// CeilingPhrase names the bound and its SOURCE: which of the two it is decides
// whether the reader opens the scenario or nothing.
func (s IDSpec) CeilingPhrase() string {
	if n := s.Ceiling(); n != IncarnationIDMaxLen {
		return fmt.Sprintf("the %d-character ceiling id.max_length sets", n)
	}
	return fmt.Sprintf("the %d-character incarnation id ceiling", IncarnationIDMaxLen)
}

// ErrIDTemplateRender marks a runtime failure of [RenderIDTemplate]: a block
// did not compile/evaluate, or its result is a list/map that cannot be part of
// an id. Callers map it to 422 (the offending values came from operator input).
var ErrIDTemplateRender = errors.New("config: id.template render failed")

// ErrIDTemplateIndexForm — the template reaches input in index form
// (`input['k']` / `input[expr]`) instead of the canonical select form
// (`input.k`). Rejected deterministically: the static reference check
// ([IDTemplateInputRefs], soul-lint) needs the component name statically known
// from the AST, and a dynamic index does not guarantee that. Mirrors
// shared/cel.ErrVarIndexForm for `vars`.
var ErrIDTemplateIndexForm = errors.New("config: id.template must address components as ${input.<name>}, not input[...]")

// RenderIDTemplate composes an incarnation id from tmpl over the RESOLVED
// input (post default-merge, see [ResolveInputContract]). Literal text passes
// through; each `${ … }` block is evaluated input-only and stringified. `\${`
// escapes the marker (ADR-010 §9.1).
//
// The result is NOT validated against the incarnation id grammar here — that is
// the keeper's job (it owns the pattern and turns a violation into a 422 naming
// the offending components). This function only fails on things the template
// itself got wrong: a block that does not compile/evaluate, or a list/map result
// ([ErrIDTemplateRender]).
//
// input is nil-safe (an empty context; blocks referencing absent components then
// fail with a no-such-key eval error, which is the honest outcome).
func RenderIDTemplate(tmpl string, input map[string]any) (string, error) {
	segs, err := scanIDTemplate(tmpl)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrIDTemplateRender, err)
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
			return "", fmt.Errorf("%w: block %q: %w", ErrIDTemplateRender, s.text, cerr)
		}
		out, _, eerr := prg.Eval(map[string]any{"input": input})
		if eerr != nil {
			return "", fmt.Errorf("%w: block %q: %w", ErrIDTemplateRender, s.text, eerr)
		}
		str, serr := idBlockString(s.text, out)
		if serr != nil {
			return "", serr
		}
		b.WriteString(str)
	}
	return b.String(), nil
}

// idBlockString coerces one evaluated block to its string form. Scalars
// (string/int/uint/double/bool) convert canonically through cel-go; a list/map has
// no meaningful place in an id → [ErrIDTemplateRender].
func idBlockString(expr string, val ref.Val) (string, error) {
	if s, ok := val.Value().(string); ok {
		return s, nil
	}
	if s, ok := val.ConvertToType(types.StringType).Value().(string); ok {
		return s, nil
	}
	return "", fmt.Errorf("%w: block %q evaluated to %s, which cannot be part of an id (use a scalar component)",
		ErrIDTemplateRender, expr, val.Type().TypeName())
}

// IDTemplateInputRefs extracts the `input.<name>` components tmpl references.
// AST-based, not textual: `input.x` in literal text OUTSIDE `${ … }` is not a
// reference, and `input.x` inside a CEL string literal is a constant, not a
// select. Names come back deduplicated, in order of first appearance.
//
// Index form → [ErrIDTemplateIndexForm]; a template with no blocks → an empty
// slice.
func IDTemplateInputRefs(tmpl string) ([]string, error) {
	segs, err := scanIDTemplate(tmpl)
	if err != nil {
		return nil, err
	}
	p, err := noMacroParserInstance()
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
			// Unparseable blocks never get here (scanIDTemplate gates the block
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
					visitErr = fmt.Errorf("%w (block %q)", ErrIDTemplateIndexForm, s.text)
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

// idSegment — a piece of a template: literal text (expr=false) or a CEL
// expression stripped of its `${ }` wrapper (expr=true).
type idSegment struct {
	text string
	expr bool
}

// scanIDTemplate splits raw into literal segments and `${ … }` blocks. The
// closing `}` is found by parsing (like shared/cel.scanInterpolation), not by
// counting braces: the substring after `${` grows to the first `}` at which the
// content parses as valid CEL. `\${` escapes the marker.
func scanIDTemplate(raw string) ([]idSegment, error) {
	var segs []idSegment
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
				segs = append(segs, idSegment{text: lit.String()})
				lit.Reset()
			}
			inner, next, err := parseIDBlock(raw, i+2)
			if err != nil {
				return nil, err
			}
			segs = append(segs, idSegment{text: inner, expr: true})
			i = next
			continue
		}
		lit.WriteByte(raw[i])
		i++
	}
	if lit.Len() > 0 {
		segs = append(segs, idSegment{text: lit.String()})
	}
	return segs, nil
}

// parseIDBlock finds the end of the `${ … }` block opened at start (the first
// byte after `${`), returning the trimmed expression and the index past `}`.
func parseIDBlock(raw string, start int) (string, int, error) {
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
	noMacroParserOnce sync.Once
	noMacroParser     *parser.Parser
	noMacroParserErr  error
)

// noMacroParserInstance returns the package's shared macro-free parser, used for
// reference extraction here and in validate_scope.go: with macros disabled,
// `has()`/`.filter()` stay plain calls instead of expanding into comprehensions
// that hide the `input.<name>` / `incarnation.<field>` selects.
func noMacroParserInstance() (*parser.Parser, error) {
	noMacroParserOnce.Do(func() {
		noMacroParser, noMacroParserErr = parser.NewParser()
		if noMacroParserErr != nil {
			noMacroParserErr = fmt.Errorf("building the macro-free CEL parser: %w", noMacroParserErr)
		}
	})
	return noMacroParser, noMacroParserErr
}

// validateIDTemplate is the schema-time check of the `id:` block against the
// scenario's own `input:` — the non-extends path (see the covenant gate in
// [schemaValidateScenario]). A thin wrapper over
// [validateIDTemplateAgainstInputKeys].
func validateIDTemplate(root *ast.MappingNode, m *ScenarioManifest, key string) []diag.Diagnostic {
	inputKeys := make(map[string]bool, len(m.Input))
	for name := range m.Input {
		inputKeys[name] = true
	}
	return validateIDTemplateAgainstInputKeys(root, m.ID, m.Create, inputKeys, key)
}

// validateIDTemplateAgainstInputKeys is the CORE `id:` check. inputKeys is
// PARAMETERIZED — the scenario's own `input:` before a covenant merge, the MERGED one
// after (mirrors the `form:` split).
//
// Rules: empty_value, id_template_invalid, id_template_input_unknown,
// id_template_too_long (against the EFFECTIVE ceiling, so a declared `max_length`
// sharpens it), id_max_length_over_ceiling, id_max_length_invalid, and the warnings
// id_template_constant / id_template_ignored.
//
// key is the spelling the FILE wrote, so a scenario on the scalar is never told to fix
// a key it does not have. The codes do not move with the spelling.
func validateIDTemplateAgainstInputKeys(root *ast.MappingNode, spec IDSpec, create *bool, inputKeys map[string]bool, key string) []diag.Diagnostic {
	pathPrefix := "$." + key
	tmpl := spec.Template
	if tmpl == "" {
		return []diag.Diagnostic{atIDPath(root, pathPrefix, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "empty_value",
			Message: key + " must be a non-empty template over input.* (drop the id: block to keep a free-text id)",
			Hint:    idTemplateExample(key),
		})}
	}

	var out []diag.Diagnostic
	if create == nil || !*create {
		out = append(out, atPath(root, pathPrefix, diag.Diagnostic{
			Level: diag.LevelWarning, Phase: diag.PhaseSchemaValidate,
			Code:    "id_template_ignored",
			Message: key + " is read only on the create path; this scenario is not a create starter",
			Hint:    "add create: true, or move " + key + " to the create scenario",
		}))
	}

	segs, err := scanIDTemplate(tmpl)
	if err != nil {
		return append(out, atPath(root, pathPrefix, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "id_template_invalid",
			Message: fmt.Sprintf("%s is not a valid ${ … } template: %v", key, err),
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
				Code:    "id_template_invalid",
				Message: fmt.Sprintf("%s block %q does not compile as CEL over input.*: %v", key, s.text, cerr),
				Hint:    "an id is composed from input only (no vars/soulprint/register/vault/now)",
			}))
		}
	}

	if blocks == 0 {
		out = append(out, atPath(root, pathPrefix, diag.Diagnostic{
			Level: diag.LevelWarning, Phase: diag.PhaseSchemaValidate,
			Code:    "id_template_constant",
			Message: key + " contains no ${ … } block — every incarnation of this service would be composed to the same id",
			Hint:    "reference at least one component, e.g. ${input.name}",
		}))
	}
	out = append(out, validateIDMaxLength(root, spec)...)
	if literal > spec.Ceiling() {
		out = append(out, atPath(root, pathPrefix, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code: "id_template_too_long",
			Message: fmt.Sprintf("literal part of %s is %d characters, over %s — no input can make it fit",
				key, literal, spec.CeilingPhrase()),
			Hint: "shorten the fixed text between the components",
		}))
	}

	refs, rerr := IDTemplateInputRefs(tmpl)
	if rerr != nil {
		return append(out, atPath(root, pathPrefix, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "id_template_invalid",
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
			Code:    "id_template_input_unknown",
			Message: fmt.Sprintf("%s references input.%s, which is not declared in input:", key, ref),
			Hint:    "declare the component in input:, or fix the reference",
		}))
	}
	return out
}

// idTemplateExample renders the hint as pastable YAML. The written spelling decides
// the SHAPE: under the block the template is a nested key, so `id.template: "…"` would
// name a key that does not exist.
func idTemplateExample(key string) string {
	const tmpl = `"${input.name}-${input.project}-redis-${input.service_type}"`
	if key == idBlockTemplateKey {
		return "e.g. " + idBlockKey + ":\n  template: " + tmpl
	}
	return "e.g. " + key + ": " + tmpl
}

// idMaxLengthPath — only the `id:` block can carry a ceiling, so the path is fixed.
const idMaxLengthPath = "$." + idBlockKey + ".max_length"

// validateIDMaxLength checks a WRITTEN `id.max_length`. Presence comes from the AST,
// not the decoded number: a decoded 0 cannot be told from an absent key, and 0 is the
// value worth reporting — the runtime reads it as "no ceiling", the author meant the
// opposite.
func validateIDMaxLength(root *ast.MappingNode, spec IDSpec) []diag.Diagnostic {
	if _, _, written := lookupPath(root, idMaxLengthPath); !written {
		return nil
	}
	switch {
	case spec.MaxLength < 1:
		return []diag.Diagnostic{atPath(root, idMaxLengthPath, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code: "id_max_length_invalid",
			Message: fmt.Sprintf("id.max_length is %d, which bounds no id — a ceiling must be at least 1",
				spec.MaxLength),
			Hint: fmt.Sprintf("set the number this service's id must fit, or drop the key to take the %d-character platform ceiling",
				IncarnationIDMaxLen),
		})}
	case spec.MaxLength > IncarnationIDMaxLen:
		return []diag.Diagnostic{atPath(root, idMaxLengthPath, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code: "id_max_length_over_ceiling",
			Message: fmt.Sprintf("id.max_length is %d, over the %d-character incarnation id ceiling — a bound above the grammar's own narrows nothing while reading as though it did",
				spec.MaxLength, IncarnationIDMaxLen),
			Hint: fmt.Sprintf("lower it to %d or below, or drop the key to take the platform ceiling",
				IncarnationIDMaxLen),
		})}
	}
	return nil
}

// atIDPath is [atPath] with a fallback to the `id:` key when the file never wrote the
// leaf — an author cannot open a line that is not there.
func atIDPath(root *ast.MappingNode, path string, d diag.Diagnostic) diag.Diagnostic {
	out := atPath(root, path, d)
	if out.Line != 0 {
		return out
	}
	if line, col, ok := lookupPath(root, "$."+idBlockKey); ok {
		out.Line, out.Column = line, col
	}
	return out
}
