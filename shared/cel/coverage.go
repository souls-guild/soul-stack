package cel

import "github.com/google/cel-go/common/types/ref"

// CoverageSink is the receiver of CEL-expression eval facts for DSL coverage
// ("trial coverage", [ADR-023]). Every successful eval of a top-level
// expression-key or `${ … }` block is reported to the sink together with the
// result — enough to record the truthy/falsy branch of a predicate
// (`where:`/`when:`/…) in coverage.
//
// The sink implementation lives in the Trial runner (`soul-trial`), not here:
// shared/cel stays free of test infrastructure. In production the sink is not set
// (nil → no-op, zero overhead).
//
// [ADR-023]: docs/adr/0023-trial-test-runner.md
type CoverageSink interface {
	// Record captures one successful eval. expr is the normalized expression
	// text (without the `${ }` wrapper); out is the CEL result. Called only
	// after a successful prg.Eval (eval errors don't reach the sink).
	Record(expr string, out ref.Val)
}

// SetCoverageSink sets the sink for DSL-coverage accounting. nil disables it (the
// default). Intended for test mode (`soul-trial`); not called in production.
//
// Not thread-safe with respect to concurrent EvalExpression: the sink is set once
// when the Engine is built in the runner, before the run starts.
func (e *Engine) SetCoverageSink(sink CoverageSink) {
	e.sink = sink
}
