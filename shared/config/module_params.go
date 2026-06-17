package config

import (
	"fmt"
	"strings"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/coremanifest"
	"github.com/souls-guild/soul-stack/shared/diag"
	"github.com/souls-guild/soul-stack/shared/plugin"
)

// validateModuleParams — фаза статической проверки `params:` задачи против
// manifest-схемы модуля (docs/soul/modules.md → «Core-модули и manifest»).
//
// Сейчас покрыты только core-модули (namespace `core`): их manifest эмбедится в
// `shared/coremanifest`. Custom-модули (любой другой namespace) тут пропускаются
// — их manifest лежит на диске рядом с бинарём и валидируется отдельным путём
// (`validate-manifest` + резолв на полном чекауте сервиса, не пилот).
//
// Что ловит структурная проверка по plugin.InputParamDef:
//   - неизвестный param (`command` вместо `cmd` у core.exec) → unknown_param;
//   - отсутствие required-параметра (`cmd`/`path`) → missing_required_param;
//   - неверный тип литерала (string там, где ждали list) → param_type_mismatch;
//   - неизвестный state модуля (`core.exec.runn`) → module_state_unknown.
//
// Чего НЕ ловит (known-limitation, см. observations): enum, числовые границы,
// вложенные object/array-схемы — этого нет в plugin.InputParamDef DSL. Полная
// унификация config.InputSchema↔plugin.InputParamDef отложена.
//
// moduleKV/paramsKV — AST-узлы ключей `module:`/`params:` (для line/col и
// проверки значений). paramsKV может быть nil (валидатор `params:`-required уже
// поднял свою диагностику выше).
func validateModuleParams(moduleKV, paramsKV *ast.MappingValueNode, pathPrefix string) []diag.Diagnostic {
	sn, ok := moduleKV.Value.(*ast.StringNode)
	if !ok {
		return nil // формат module: уже отвалидирован validateModuleField.
	}
	ns, mod, state, ok := splitModuleAddress(sn.Value)
	if !ok || ns != "core" {
		// reModuleAddress-несоответствие уже даёт module_format_invalid; для
		// custom-namespace схемы тут нет.
		return nil
	}

	reg := coremanifest.Default()
	def, ok := reg.State("core."+mod, state)
	if !ok {
		// Либо модуль core.<mod> отсутствует в реестре, либо state неизвестен.
		// Если самого модуля нет (новый core, ещё не заведён manifest) — не
		// шумим (это не ошибка автора). Если модуль есть, а state нет — ошибка.
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

	// params: отсутствует — required-проверку всё равно надо отработать (нет
	// params значит ни один required не передан). Позицию берём от module:.
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

// checkUnknownAndType — для каждого присутствующего param-ключа: known? + тип.
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

// checkParamType — структурная проверка типа литерала против схемы. Значение,
// обёрнутое целиком в `${ … }` (CEL-выражение), пропускается: его рантайм-тип
// статически неизвестен (ADR-010, non-string CEL-результат).
func checkParamType(p plugin.InputParamDef, name string, value ast.Node, pathPrefix string) []diag.Diagnostic {
	if p.Type == "" {
		return nil
	}
	if sn, isStr := value.(*ast.StringNode); isStr && isCELWrapped(sn.Value) {
		return nil
	}
	if _, isNull := value.(*ast.NullNode); isNull {
		return nil // null = «не задано», эквивалент отсутствия ключа.
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

// checkRequired — каждый required-param из схемы должен присутствовать в params.
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
	// Детерминированный порядок диагностик: по имени параметра.
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
	// Малый размер (единицы) — простая вставочная сортировка без import sort.
	for i := 1; i < len(req); i++ {
		for j := i; j > 0 && req[j-1] > req[j]; j-- {
			req[j-1], req[j] = req[j], req[j-1]
		}
	}
	return req
}

// astMatchesType — соответствует ли AST-узел задекларированному типу. Числовые
// синонимы (int/integer/number) и list/array, map/object нормализуются.
func astMatchesType(declared string, value ast.Node) bool {
	switch canonicalType(declared) {
	case "string":
		// Block-scalar (folded `>` / literal `|`) парсится goccy как LiteralNode,
		// а не StringNode — но это та же строка. Без этой ветки многострочные
		// string-params (типичный core.cmd.shell с `cmd: >`) ложно реджектились
		// как param_type_mismatch.
		switch value.(type) {
		case *ast.StringNode, *ast.LiteralNode:
			return true
		}
		return false
	case "int":
		_, ok := value.(*ast.IntegerNode)
		return ok
	case "number":
		// number принимает и int, и float.
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
		// Неизвестный тип в схеме — не наша ответственность (manifest-валидатор
		// ловит input_type_unknown); type-check пропускаем.
		return true
	}
}

// canonicalType сводит синонимы docs/input.md к каноническим именам plugin-DSL.
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

// splitModuleAddress разбирает `<ns>.<module>.<state>` на части. Возвращает
// ok=false, если сегментов не ровно три.
func splitModuleAddress(addr string) (ns, mod, state string, ok bool) {
	parts := strings.Split(addr, ".")
	if len(parts) != 3 {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}
