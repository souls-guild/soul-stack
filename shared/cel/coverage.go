package cel

import "github.com/google/cel-go/common/types/ref"

// CoverageSink — приёмник фактов eval-а CEL-выражений для DSL-coverage
// («trial coverage», [ADR-023]). Каждый успешный eval top-level
// expression-ключа или `${ … }`-блока сообщается sink-у вместе с
// результатом — этого достаточно, чтобы учесть truthy/falsy-ветку
// предиката (`where:`/`when:`/…) в покрытии.
//
// Реализация sink-а живёт в раннере Trial (`soul-trial`), не здесь:
// shared/cel остаётся свободен от тест-инфраструктуры. В проде sink не
// устанавливается (nil → no-op, нулевой оверхед).
//
// [ADR-023]: docs/adr/0023-trial-test-runner.md#adr-023-тест-раннер-trial-soul-trial-и-dsl-coverage
type CoverageSink interface {
	// Record фиксирует один успешный eval. expr — нормализованный текст
	// выражения (без обёртки `${ }`); out — результат CEL. Вызывается
	// только после успешного prg.Eval (ошибки eval до sink-а не доходят).
	Record(expr string, out ref.Val)
}

// SetCoverageSink устанавливает sink для учёта DSL-coverage. nil
// отключает учёт (поведение по умолчанию). Предназначен для тест-режима
// (`soul-trial`); в проде не вызывается.
//
// Не потокобезопасен относительно конкурентных EvalExpression: sink
// ставится один раз при сборке Engine в раннере, до старта прогона.
func (e *Engine) SetCoverageSink(sink CoverageSink) {
	e.sink = sink
}
