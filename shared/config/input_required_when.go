package config

// Узкий CEL-эвалуатор для input-схемного ключа `required_when` (docs/input.md →
// «Условная обявательность»). Намеренно НЕ зависит от shared/cel: тот несёт
// полный Engine (vault()/glob()/merge()/coverage/soulprint.hosts) и весь
// контекст scenario/destiny. required_when — input-валидация ДО render-фазы,
// чистая функция от input.*; ей нужен минимальный env с ЕДИНСТВЕННОЙ переменной
// `input`. Обращение к любому другому имени (essence/soulprint/register/…) →
// compile-ошибка undeclared reference — sandbox обеспечивается необъявленностью,
// симметрично migration-CEL ([ADR-019]). cel-go уже в модуле shared (тянет
// shared/cel) — package-level зависимость config→cel-go не добавляет нового
// module-dep и не создаёт цикла (cel-go — внешняя библиотека).

import (
	"fmt"
	"sync"

	"github.com/goccy/go-yaml/ast"
	"github.com/google/cel-go/cel"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// inputEnv — CEL-окружение required_when: единственная объявленная переменная
// `input` (DynType, как остальной контекст Soul Stack — типизация по схемам
// backlog). Иммутабелен после первой сборки; собирается лениво (env-сборка не
// бесплатна, а большинство схем required_when не несут). Кеш программ —
// inputPrograms (по нормализованному тексту выражения).
var (
	inputEnvOnce sync.Once
	inputEnv     *cel.Env
	inputEnvErr  error

	inputProgMu sync.RWMutex
	inputProgs  = map[string]cel.Program{}
)

func requiredWhenEnv() (*cel.Env, error) {
	inputEnvOnce.Do(func() {
		inputEnv, inputEnvErr = cel.NewEnv(
			cel.StdLib(),
			cel.Variable("input", cel.DynType),
		)
		if inputEnvErr != nil {
			inputEnvErr = fmt.Errorf("сборка CEL-окружения required_when: %w", inputEnvErr)
		}
	})
	return inputEnv, inputEnvErr
}

// compileRequiredWhen компилирует (и кеширует) предикат required_when против
// inputEnv. Ошибка компиляции (синтаксис / неизвестный идентификатор вне `input`
// / несовместимые типы) возвращается caller-у для классификации:
// schema-валидатор → input_required_when_invalid, runtime → input-ошибка.
func compileRequiredWhen(expr string) (cel.Program, error) {
	inputProgMu.RLock()
	prg, ok := inputProgs[expr]
	inputProgMu.RUnlock()
	if ok {
		return prg, nil
	}

	env, err := requiredWhenEnv()
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

	inputProgMu.Lock()
	inputProgs[expr] = prg
	inputProgMu.Unlock()
	return prg, nil
}

// evalRequiredWhen вычисляет предикат required_when над смерженным input и
// приводит результат к bool. Используется requireInputValues на input-стадии
// (ПОСЛЕ mergeInputDefaults). merged nil-безопасен (пустой контекст). Не-bool
// результат предиката — ошибка (required_when обязан возвращать булево).
func evalRequiredWhen(expr string, merged map[string]any) (bool, error) {
	prg, err := compileRequiredWhen(expr)
	if err != nil {
		return false, fmt.Errorf("required_when %q: %w", expr, err)
	}
	if merged == nil {
		merged = map[string]any{}
	}
	out, _, err := prg.Eval(map[string]any{"input": merged})
	if err != nil {
		return false, fmt.Errorf("required_when %q: %w", expr, err)
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("required_when %q вернул %s, ожидался bool", expr, out.Type().TypeName())
	}
	return b, nil
}

// validateRequiredWhen — schema-time статическая проверка: required_when —
// непустая, парсимая/компилируемая против inputEnv CEL-строка. Применим к любому
// type (условие — над прочим input, не над самим полем). Парс-ошибка или
// обращение к имени вне `input` → input_required_when_invalid. kv — `required_when`
// MappingValueNode (для line/col диагностики).
func validateRequiredWhen(s *InputSchema, kv *ast.MappingValueNode, path string) []diag.Diagnostic {
	if s == nil {
		return nil
	}
	if s.RequiredWhen == "" {
		// Пустая строка — бессмысленный предикат (нет условия). Отвергаем явно,
		// иначе тихо никогда-не-обязателен (footgun автора схемы).
		return []diag.Diagnostic{diagAtKV(kv, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:     "input_required_when_invalid",
			Message:  "required_when must be a non-empty CEL predicate over input.*",
			Hint:     `e.g. required_when: "input.redis_type == 'cluster'"`,
			YAMLPath: path + ".required_when",
		})}
	}
	if _, err := compileRequiredWhen(s.RequiredWhen); err != nil {
		return []diag.Diagnostic{diagAtKV(kv, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:     "input_required_when_invalid",
			Message:  fmt.Sprintf("required_when does not compile as CEL over input.*: %v", err),
			Hint:     "predicate may reference only input.* (no essence/soulprint/register/vault/now)",
			YAMLPath: path + ".required_when",
		})}
	}
	return nil
}
