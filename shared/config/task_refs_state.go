package config

import (
	"regexp"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// Static check of `state.<...>` references inside scenario CEL. In the
// scenario-render context incarnation.state is available as
// `incarnation.state.<path>` (a read-only snapshot of stateBefore, ADR-009/010
// Variant A), while the bare root `state` is NOT declared (only migration-CEL
// declares it — [migrationVars], mutable). So `state.<path>` in a scenario is
// almost always a forgotten `incarnation.` prefix: the runtime would give the
// cryptic compile error "undeclared reference to 'state'". The linter catches
// this statically and suggests the canonical form — symmetric to how
// checkSoulprintRefs rejects a bare `soulprint.<path>` without `.self`.
//
// Scope is scenario mode (this file is called only from collectRefs over
// scenario/destiny tasks). Migration-CEL (migrations/<N>_to_<M>) is validated by
// a separate path where a bare mutable `state.*` is legitimate (ADR-019) — this
// check does NOT reach there.
//
// Token extraction is textual (the contents of CEL string literals are stripped,
// as in the register/soulprint checks): a full CEL-AST parse is unnecessary, the
// form is fixed by the context grammar (`state.<path>`).

// reStateCELRef extracts the root identifier `state` followed by `.` (dotted
// access to a state field). The left boundary is start-of-string OR a character
// that is not part of an identifier and not `.`: `incarnation.state.x` does NOT
// match (a `.` precedes `state` — it is a field of incarnation, the legitimate
// form), and `mystate.x` and `foo.state.y` do not match either (`state` must be
// the root). So it fires exactly on a bare `state.<path>`, which is undeclared
// in scenario CEL.
var reStateCELRef = regexp.MustCompile(`(^|[^A-Za-z0-9_.])state\.[a-z]`)

// hasNakedStateRef reports whether a CEL string contains a bare `state.<path>`
// reference (after stripping string literals — `state.x` inside a data literal
// does not count). celStringLiteral is reused from task_refs.go.
func hasNakedStateRef(expr string) bool {
	stripped := celStringLiteral.ReplaceAllString(expr, `""`)
	return reStateCELRef.MatchString(stripped)
}

// checkStateRefs catches a bare `state.<path>` in one scenario CEL predicate
// (where/when/changed_when/failed_when/retry.until/loop.when). A bool literal /
// null (force-shortcut) is not a CEL string and is skipped. The diagnostic is at
// the value node's position (the exact offset within the string is not
// extracted, as in checkPredicateRefs/checkSoulprintRefs).
func checkStateRefs(kind string, value ast.Node, taskPath string) []diag.Diagnostic {
	sn, ok := value.(*ast.StringNode)
	if !ok || !hasNakedStateRef(sn.Value) {
		return nil
	}
	return []diag.Diagnostic{stateNakedDiag(kind, sn, taskPath)}
}

// checkInterpStateRefs recursively walks an interpolation source field
// (vars/output/params/apply.input/loop.items) and catches a bare `state.<path>`
// in `${ … }` strings. Symmetric to checkInterpRefs (register), but for the
// state root. The interpolation zone matters: the canonical case is
// `current: ${ state.redis_users }` in apply.input (update_acl), where the
// forgotten prefix is a string value, not a predicate.
func checkInterpStateRefs(node ast.Node, taskPath, kind string) []diag.Diagnostic {
	switch n := node.(type) {
	case *ast.StringNode:
		if !hasNakedStateRef(n.Value) {
			return nil
		}
		return []diag.Diagnostic{stateNakedDiag(kind, n, taskPath)}
	case *ast.MappingNode:
		var out []diag.Diagnostic
		for _, kv := range n.Values {
			out = append(out, checkInterpStateRefs(kv.Value, taskPath, kind)...)
		}
		return out
	case *ast.MappingValueNode:
		return checkInterpStateRefs(n.Value, taskPath, kind)
	case *ast.SequenceNode:
		var out []diag.Diagnostic
		for _, v := range n.Values {
			out = append(out, checkInterpStateRefs(v, taskPath, kind)...)
		}
		return out
	default:
		return nil
	}
}

// stateNakedDiag builds a single "bare state.* in scenario" diagnostic with a
// hint of the canonical form. kind is the field name (for message and
// yaml_path); the position comes from the string node's token sn.
func stateNakedDiag(kind string, sn *ast.StringNode, taskPath string) diag.Diagnostic {
	line, col := 0, 0
	if tok := sn.GetToken(); tok != nil {
		line, col = tok.Position.Line, tok.Position.Column
	}
	return diagAt(line, col, diag.Diagnostic{
		Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
		Code:     "state_naked_reference",
		Message:  kind + " references bare state.<path>, which is undeclared in scenario CEL (state is migration-only)",
		Hint:     "incarnation.state.<path> is the read-only snapshot of incarnation.state in scenario render (ADR-009/010); bare state.* is reserved for migration DSL",
		YAMLPath: taskPath + "." + kind,
	})
}
