package cel

import (
	"path/filepath"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

// CEL-функция glob() ([templating.md «Custom CEL-функции»], [ADR-040]).
// Shell-glob matching строки против pattern-а — Salt-parity matcher `-G`:
//
//	'sid.glob("prod-*")'                          → bool
//	'soulprint.self.os.family.glob("debian*")'   → bool
//
// Pure от двух строк (без I/O, сети, eval-time состояния), симметрично
// stdlib-функциям size()/contains(). Реализация — [filepath.Match]
// (`*`/`?`/`[abc]`/`[a-z]`, escape через `\\`). Битый pattern → false без
// ошибки: target.where в Tide/ErrandRun не должен валиться на per-host
// предикате — невычислимый pattern на хосте трактуем как «не подходит»,
// валидацию синтаксиса делает [soul-lint] на компиляции scenario.
//
// Member-overload `s.glob(p)` (string receiver + string arg) симметрично
// stdlib-`s.contains(p)`/`s.matches(re)`. Глобальная форма `glob(s, p)` не
// регистрируется — единый способ записи в Destiny/scenario.
//
// Регистрируется только в основном scenario/destiny-режиме (см. [buildEngine]):
// migration-CEL ([ADR-019]) — hermetic-песочница (только `state` + чистые
// арифметические/stdlib-операции), расширение custom-функциями требует
// отдельного ADR. Flow-control-режим ([NewFlowControl], [ADR-012(d)]) glob()
// получает: предикаты when:/changed_when:/failed_when: на Soul-е симметричны
// scenario-предикатам, glob() не тянет внешний контекст.

// globFuncName — имя функции в CEL-env. Пользователь пишет `s.glob(p)`.
const globFuncName = "glob"

// globEnvOptions возвращает EnvOption-ы регистрации glob(): один
// member-overload `string.glob(string) bool`. Вызывается из [buildEngine]
// для всех режимов, КРОМЕ migration (migration-CEL остаётся hermetic).
func globEnvOptions() []cel.EnvOption {
	return []cel.EnvOption{
		cel.Function(globFuncName,
			cel.MemberOverload("string_glob_string",
				[]*cel.Type{cel.StringType, cel.StringType},
				cel.BoolType,
				cel.BinaryBinding(callGlob),
			),
		),
	}
}

// callGlob — binding функции `<string>.glob(<pattern>)`. Оба аргумента
// CEL гарантировал к string на этапе type-check (overload закреплён к
// StringType). Битый pattern ([filepath.ErrBadPattern]) → false: per-host
// предикат target.where не должен валиться на отдельном хосте — синтаксис
// pattern-а валидируется soul-lint-ом до прогона.
func callGlob(strVal, patternVal ref.Val) ref.Val {
	s, ok := strVal.Value().(string)
	if !ok {
		return types.Bool(false)
	}
	pattern, ok := patternVal.Value().(string)
	if !ok {
		return types.Bool(false)
	}
	matched, err := filepath.Match(pattern, s)
	if err != nil {
		return types.Bool(false)
	}
	return types.Bool(matched)
}
