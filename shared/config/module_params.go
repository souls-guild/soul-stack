package config

import (
	"fmt"
	"strings"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/coremanifest"
	"github.com/souls-guild/soul-stack/shared/diag"
	"github.com/souls-guild/soul-stack/shared/plugin"
)

// validateModuleParams — the static-check phase of a task's `params:` against the
// module's manifest schema (docs/soul/modules.md → "Core modules and manifest").
//
// Currently only core modules (namespace `core`) are covered: their manifest is
// embedded in `shared/coremanifest`. Custom modules (any other namespace) are skipped
// here — their manifest lives on disk next to the binary and is validated by a
// separate path (`validate-manifest` + resolve on a full service checkout, not pilot).
//
// What the structural check over plugin.InputParamDef catches:
//   - unknown param (`command` instead of `cmd` for core.exec) → unknown_param;
//   - missing required param (`cmd`/`path`) → missing_required_param;
//   - wrong literal type (string where a list was expected) → param_type_mismatch;
//   - unknown module state (`core.exec.runn`) → module_state_unknown.
//
// What it does NOT catch (known limitation, see observations): enum, numeric bounds,
// nested object/array schemas — absent from the plugin.InputParamDef DSL. Full
// unification of config.InputSchema↔plugin.InputParamDef is deferred.
//
// moduleKV/paramsKV — AST nodes of the `module:`/`params:` keys (for line/col and
// value checks). paramsKV may be nil (the `params:`-required validator already raised
// its diagnostic above).
func validateModuleParams(moduleKV, paramsKV *ast.MappingValueNode, pathPrefix string) []diag.Diagnostic {
	sn, ok := moduleKV.Value.(*ast.StringNode)
	if !ok {
		return nil // module: format already validated by validateModuleField.
	}
	ns, mod, state, ok := splitModuleAddress(sn.Value)
	if !ok || ns != "core" {
		// A reModuleAddress mismatch already yields module_format_invalid; there is no
		// schema here for a custom namespace.
		return nil
	}

	reg := coremanifest.Default()
	def, ok := reg.State("core."+mod, state)
	if !ok {
		// Either module core.<mod> is absent from the registry, or the state is
		// unknown. If the module itself is missing (new core, no manifest yet) — stay
		// quiet (not an author error). If the module exists but the state doesn't — error.
		if _, hasMod := reg.Lookup("core." + mod); !hasMod {
			return nil
		}
		tok := sn.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:     "module_state_unknown",
			Message:  fmt.Sprintf("core.%s has no state %q", mod, state),
			Hint:     "see the module's manifest spec.states for valid states",
			YAMLPath: pathPrefix + ".module",
		})}
	}

	// params: absent — the required check must still run (no params means no required
	// was passed). Position is taken from module:.
	var paramsNode *ast.MappingNode
	if paramsKV != nil {
		if mm, isMap := paramsKV.Value.(*ast.MappingNode); isMap {
			paramsNode = mm
		}
	}

	var out []diag.Diagnostic
	out = append(out, checkUnknownAndType(def, paramsNode, pathPrefix)...)
	out = append(out, checkRequired(def, paramsNode, moduleKV, pathPrefix)...)
	return out
}

// checkUnknownAndType — for each present param key: known? + type.
func checkUnknownAndType(def plugin.StateDef, paramsNode *ast.MappingNode, pathPrefix string) []diag.Diagnostic {
	if paramsNode == nil {
		return nil
	}
	var out []diag.Diagnostic
	for _, kv := range paramsNode.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		name := tok.Value
		p, known := def.Input[name]
		if !known {
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				Code:     "unknown_param",
				Message:  fmt.Sprintf("unknown param %q for this module state", name),
				Hint:     "see the module's manifest spec.states.<state>.input for accepted params",
				YAMLPath: pathPrefix + ".params." + name,
			}))
			continue
		}
		out = append(out, checkParamType(p, name, kv.Value, pathPrefix)...)
	}
	return out
}

// checkParamType — structural check of a literal's type against the schema. A value
// wrapped entirely in `${ … }` (a CEL expression) is skipped: its runtime type is
// statically unknown (ADR-010, non-string CEL result).
func checkParamType(p plugin.InputParamDef, name string, value ast.Node, pathPrefix string) []diag.Diagnostic {
	if p.Type == "" {
		return nil
	}
	if sn, isStr := value.(*ast.StringNode); isStr && isCELWrapped(sn.Value) {
		return nil
	}
	if _, isNull := value.(*ast.NullNode); isNull {
		return nil // null = "not set", equivalent to a missing key.
	}
	if astMatchesType(p.Type, value) {
		return nil
	}
	tok := value.GetToken()
	line, col := 0, 0
	if tok != nil {
		line, col = tok.Position.Line, tok.Position.Column
	}
	return []diag.Diagnostic{diagAt(line, col, diag.Diagnostic{
		Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
		Code:     "param_type_mismatch",
		Message:  fmt.Sprintf("param %q must be %s", name, canonicalType(p.Type)),
		YAMLPath: pathPrefix + ".params." + name,
	})}
}

// checkRequired — every required param from the schema must be present in params.
func checkRequired(def plugin.StateDef, paramsNode *ast.MappingNode, moduleKV *ast.MappingValueNode, pathPrefix string) []diag.Diagnostic {
	present := map[string]bool{}
	if paramsNode != nil {
		for _, kv := range paramsNode.Values {
			if tok := kv.Key.GetToken(); tok != nil {
				present[tok.Value] = true
			}
		}
	}
	var out []diag.Diagnostic
	// Deterministic diagnostic order: by param name.
	for _, name := range sortedRequired(def) {
		if present[name] {
			continue
		}
		tok := moduleKV.Key.GetToken()
		line, col := 0, 0
		if tok != nil {
			line, col = tok.Position.Line, tok.Position.Column
		}
		out = append(out, diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:     "missing_required_param",
			Message:  fmt.Sprintf("required param %q is missing", name),
			YAMLPath: pathPrefix + ".params." + name,
		}))
	}
	return out
}

func sortedRequired(def plugin.StateDef) []string {
	var req []string
	for name, p := range def.Input {
		if p.Required {
			req = append(req, name)
		}
	}
	// Small size (a few) — simple insertion sort, no import sort.
	for i := 1; i < len(req); i++ {
		for j := i; j > 0 && req[j-1] > req[j]; j-- {
			req[j-1], req[j] = req[j], req[j-1]
		}
	}
	return req
}

// astMatchesType — whether the AST node matches the declared type. Numeric synonyms
// (int/integer/number) and list/array, map/object are normalized.
func astMatchesType(declared string, value ast.Node) bool {
	switch canonicalType(declared) {
	case "string":
		// A block scalar (folded `>` / literal `|`) is parsed by goccy as a
		// LiteralNode, not a StringNode — but it's the same string. Without this
		// branch, multi-line string params (typical core.cmd.shell with `cmd: >`)
		// were falsely rejected as param_type_mismatch.
		switch value.(type) {
		case *ast.StringNode, *ast.LiteralNode:
			return true
		}
		return false
	case "int":
		_, ok := value.(*ast.IntegerNode)
		return ok
	case "number":
		// number accepts both int and float.
		if _, ok := value.(*ast.IntegerNode); ok {
			return true
		}
		_, ok := value.(*ast.FloatNode)
		return ok
	case "bool":
		_, ok := value.(*ast.BoolNode)
		return ok
	case "list":
		_, ok := value.(*ast.SequenceNode)
		return ok
	case "map":
		_, ok := value.(*ast.MappingNode)
		return ok
	default:
		// Unknown type in the schema is not our concern (the manifest validator
		// catches input_type_unknown); skip the type check.
		return true
	}
}

// canonicalType maps docs/input.md synonyms to the canonical plugin-DSL names.
func canonicalType(t string) string {
	switch t {
	case "integer":
		return "int"
	case "boolean":
		return "bool"
	case "array":
		return "list"
	case "object":
		return "map"
	default:
		return t
	}
}

// splitModuleAddress splits `<ns>.<module>.<state>` into parts. Returns ok=false if
// there are not exactly three segments.
func splitModuleAddress(addr string) (ns, mod, state string, ok bool) {
	parts := strings.Split(addr, ".")
	if len(parts) != 3 {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}
