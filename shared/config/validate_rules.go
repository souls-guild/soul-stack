package config

// Top-level scenario-секция `validate:` (ADR-009 amendment 2026-06-23, DSL wave 2):
// декларативная input-валидация списком правил `[{that, message}]`. См. doc-comment
// на [ValidateRule] (scenario.go) о назначении и границе с assert/required_when.
//
// Реализация СОЗНАТЕЛЬНО переиспользует узкий cel-go-sandbox `required_when`
// (input_required_when.go): `that`-предикат компилируется тем же [compileRequiredWhen]
// против inputEnv с единственной переменной `input`. Никакого второго CEL-движка —
// один источник input-only-eval на required_when и validate. Ссылка на любое имя
// вне `input` (essence/soulprint/register/vault/now) → compile-ошибка undeclared
// reference: структурный input-only-барьер обеспечивается необъявленностью env,
// не текстовым guard-ом (симметрично required_when и migration-CEL ADR-019).

import (
	"fmt"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// ValidateRuleFailure — провал одного `validate:`-правила на runtime-eval: индекс
// правила в списке + его message (для 422 validation_failed) + сам предикат (для
// диагностики/логов). Возвращается [EvalValidateRules] на первом `that == false`.
type ValidateRuleFailure struct {
	Index   int
	Message string
	That    string
}

// Error — человекочитаемая форма провала: message правила + индекс/текст предиката.
// Симметрично формату render.ErrAssertFailed («message (предикат that[i] ... )»).
func (f ValidateRuleFailure) Error() string {
	return fmt.Sprintf("%s (validate[%d] %q вычислился в false)", f.Message, f.Index, f.That)
}

// EvalValidateRules вычисляет правила `validate:` над смерженным input (после
// mergeInputDefaults) в input-only CEL-контексте. Возвращает:
//   - (nil, nil) — все правила прошли (или список пуст);
//   - (*ValidateRuleFailure, nil) — первое `that == false`: правило-нарушитель;
//   - (nil, err) — внутренний сбой eval (предикат не bool / CEL runtime-error /
//     сбой компиляции — невозможен после schema-валидации, но не глотаем).
//
// merged nil-безопасен (пустой контекст). Первый false выигрывает (короткое
// замыкание по порядку объявления — как required_when по полям и assert по that[]).
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

// validateValidateBlock — schema-time проверка top-level `validate:`-блока:
// sequence правил, каждое — mapping `{ that: <CEL-bool>, message: <str> }`.
//
//   - блок обязан быть непустым sequence (пустой `validate: []` бессмыслен —
//     отвергаем как empty_value: правило-без-правил вводит автора в заблуждение);
//   - `that` — обязателен, непустая строка, парсимая/компилируемая против inputEnv
//     (input-only). Парс-ошибка ИЛИ ссылка на имя вне `input` →
//     validate_rule_invalid;
//   - `message` — обязателен, непустая строка (без message провал правила
//     анонимен — оператор не поймёт причину 422; ассиметрия с assert.message,
//     который опционален, оправдана: assert несёт имя задачи как fallback,
//     у validate-правила имени нет);
//   - прочие ключи внутри правила — unknown_key (fail-closed).
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

// validateValidateRule — валидация одного правила `validate[i]` (см.
// validateValidateBlock). that/message обязательны и непусты; that компилируется
// input-only; неизвестные ключи отбраковываются.
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

// validateRuleThat — `that` непустая строка, парсимая/компилируемая input-only.
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

// validateRuleMessage — `message` непустая строка.
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
