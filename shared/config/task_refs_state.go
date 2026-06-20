package config

import (
	"regexp"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// Статпроверка ссылок `state.<...>` внутри scenario-CEL. В scenario-render-
// контексте incarnation.state доступен как `incarnation.state.<path>` (read-only
// снимок stateBefore, ADR-009/010 Вариант A), а голый корень `state` НЕ объявлен
// (его объявляет только migration-CEL — [migrationVars], мутируемый). Поэтому
// `state.<path>` в scenario — почти всегда забытый префикс `incarnation.`:
// рантайм отдал бы cryptic compile-ошибку «undeclared reference to 'state'».
// Линтер ловит это статически и подсказывает каноническую форму — симметрично
// тому, как checkSoulprintRefs режет голую `soulprint.<path>` без `.self`.
//
// Зона — scenario-режим (этот файл вызывается только из collectRefs над
// scenario/destiny-задачами). Migration-CEL (migrations/<N>_to_<M>) валидируется
// отдельным путём, где bare `state.*` mutable легитимен (ADR-019) — туда эта
// проверка НЕ дотягивается.
//
// Извлечение токенов текстовое (содержимое строковых литералов CEL вырезается,
// как в register/soulprint-проверках): полный CEL-AST-парс не нужен, форма
// фиксирована грамматикой контекста (`state.<path>`).

// reStateCELRef извлекает корневой идентификатор `state`, за которым идёт `.`
// (точечный доступ к полю state). Граница слева — начало строки ИЛИ символ, не
// входящий в идентификатор и не `.`: `incarnation.state.x` НЕ матчит (перед
// `state` стоит `.` — это поле incarnation, легитимная форма), `mystate.x` и
// `foo.state.y` тоже не матчат (`state` обязан быть корневым). Так срабатывает
// ровно на голом `state.<path>`, который в scenario-CEL не объявлен.
var reStateCELRef = regexp.MustCompile(`(^|[^A-Za-z0-9_.])state\.[a-z]`)

// hasNakedStateRef сообщает, есть ли в CEL-строке голая ссылка `state.<path>`
// (после вырезания строковых литералов — `state.x` внутри литерала-данных не
// считается). celStringLiteral переиспользуется из task_refs.go.
func hasNakedStateRef(expr string) bool {
	stripped := celStringLiteral.ReplaceAllString(expr, `""`)
	return reStateCELRef.MatchString(stripped)
}

// checkStateRefs ловит голую `state.<path>` в одном scenario-CEL-предикате
// (where/when/changed_when/failed_when/retry.until/loop.when). bool-литерал/null
// (force-shortcut) — не CEL-строка, пропускается. Диагностика — на позиции
// value-ноды (точное смещение внутри строки не извлекается, как в
// checkPredicateRefs/checkSoulprintRefs).
func checkStateRefs(kind string, value ast.Node, taskPath string) []diag.Diagnostic {
	sn, ok := value.(*ast.StringNode)
	if !ok || !hasNakedStateRef(sn.Value) {
		return nil
	}
	return []diag.Diagnostic{stateNakedDiag(kind, sn, taskPath)}
}

// checkInterpStateRefs рекурсивно обходит интерполяционное source-поле
// (vars/output/params/apply.input/loop.items) и ловит голую `state.<path>` в
// `${ … }`-строках. Симметрично checkInterpRefs (register), но для state-корня.
// Зона интерполяции критична: канонический кейс — `current: ${ state.redis_users }`
// в apply.input (update_acl), где забытый префикс не предикат, а строковое
// значение.
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

// stateNakedDiag собирает единую диагностику «голый state.* в scenario» с
// подсказкой канонической формы. kind — имя поля (для message и yaml_path);
// позиция берётся из токена строкового узла sn.
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
