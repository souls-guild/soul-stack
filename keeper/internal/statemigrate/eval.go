package statemigrate

import (
	"fmt"

	"github.com/souls-guild/soul-stack/shared/cel"
)

// Evaluator резолвит CEL-выражения migration-контекста ([ADR-019]). Узкий
// порт: ядро statemigrate зависит только от него, не от конкретного движка.
// scope — текущий state (мутируемый) плюс foreach-переменные (`as:`); как они
// прокидываются — деталь реализации.
type Evaluator interface {
	// Eval вычисляет выражение БЕЗ обёртки `${ }` (вся строка = CEL).
	// Используется для foreach.in и для `${ … }`-сегментов адреса path.
	// Возвращает нативное Go-значение (map[string]any/[]any/скаляр).
	Eval(expr string, scope Scope) (any, error)

	// Interpolate резолвит строку set.value со встроенными `${ … }`-блоками
	// (литерал + блоки, [docs/migrations.md] set). Ровно один блок без
	// окружающего текста → нативный тип; иначе склейка через стрингификацию.
	Interpolate(raw string, scope Scope) (any, error)
}

// Scope — переменные одной CEL-оценки в миграции: корневой State и набор
// активных foreach-переменных (имя `as:` → текущий элемент/значение).
// Loop — плоская карта; вложенные foreach добавляют свои имена поверх.
type Scope struct {
	State map[string]any
	Loop  map[string]any
}

// celEvaluator — реализация Evaluator поверх cel.Engine в migration-режиме
// ([cel.NewMigration]). Один Engine переиспользуется всеми операциями цепочки
// (compile-cache горячий путь).
type celEvaluator struct {
	engine *cel.Engine
}

// NewEvaluator собирает Evaluator на migration-движке shared/cel: объявлена
// только переменная `state`, прочий контекст недоступен (sandbox by
// undeclaration), vault()/now() отсекаются guard-ами ([cel.NewMigration]).
func NewEvaluator() (Evaluator, error) {
	engine, err := cel.NewMigration()
	if err != nil {
		return nil, fmt.Errorf("statemigrate: сборка migration-CEL: %w", err)
	}
	return &celEvaluator{engine: engine}, nil
}

// Eval компилирует и вычисляет голое expr против migration-env. State →
// Vars.State; foreach-переменные → Vars.Loop (тот же механизм Extend, что у
// loop:). Результат нормализуется в чистые Go-данные (оборачиваем выражение в
// маркер, чтобы переиспользовать toNative/развёртку cel-контейнеров shared/cel).
func (e *celEvaluator) Eval(expr string, scope Scope) (any, error) {
	return e.engine.EvalInterpolation("${ "+expr+" }", e.vars(scope))
}

// Interpolate резолвит строку set.value с произвольными `${ … }`-блоками через
// штатный EvalInterpolation shared/cel (литерал+блоки, нативный тип при одном
// блоке без текста).
func (e *celEvaluator) Interpolate(raw string, scope Scope) (any, error) {
	return e.engine.EvalInterpolation(raw, e.vars(scope))
}

func (e *celEvaluator) vars(scope Scope) cel.Vars {
	return cel.Vars{State: scope.State, Loop: scope.Loop}
}
