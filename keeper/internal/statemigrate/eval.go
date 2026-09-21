package statemigrate

import (
	"fmt"

	"github.com/souls-guild/soul-stack/shared/cel"
)

// Evaluator resolves CEL expressions in the migration context ([ADR-019]). A narrow
// port: the statemigrate core depends only on it, not on a specific engine.
// scope — the current state (mutable) plus foreach variables (`as:`); how they
// are threaded through is an implementation detail.
type Evaluator interface {
	// Eval evaluates an expression WITHOUT the `${ }` wrapper (the whole string = CEL).
	// Used for foreach.in and for `${ … }` segments of a path address.
	// Returns a native Go value (map[string]any/[]any/scalar).
	Eval(expr string, scope Scope) (any, error)

	// Interpolate resolves a set.value string with embedded `${ … }` blocks
	// (literal + blocks, [docs/migrations.md] set). Exactly one block with no
	// surrounding text → native type; otherwise concatenation via stringification.
	Interpolate(raw string, scope Scope) (any, error)
}

// Scope — variables for a single CEL evaluation in a migration: the root State and a set
// of active foreach variables (`as:` name → current element/value).
// Loop is a flat map; nested foreach blocks add their names on top.
type Scope struct {
	State map[string]any
	Loop  map[string]any
}

// celEvaluator — an Evaluator implementation on top of cel.Engine in migration mode
// ([cel.NewMigration]). A single Engine is reused by all operations in the chain
// (compile-cache hot path).
type celEvaluator struct {
	engine *cel.Engine
}

// NewEvaluator builds an Evaluator on the shared/cel migration engine: only
// the `state` variable is declared, other context is unavailable (sandbox by
// undeclaration), vault()/now() are blocked by guards ([cel.NewMigration]).
func NewEvaluator() (Evaluator, error) {
	engine, err := cel.NewMigration()
	if err != nil {
		return nil, fmt.Errorf("statemigrate: building migration-CEL: %w", err)
	}
	return &celEvaluator{engine: engine}, nil
}

// Eval compiles and evaluates the bare expr against the migration-env. State →
// Vars.State; foreach variables → Vars.Loop (the same Extend mechanism used by
// loop:). The result is normalized to plain Go data (we wrap the expression in
// the marker to reuse shared/cel's toNative/cel-container unwrapping).
func (e *celEvaluator) Eval(expr string, scope Scope) (any, error) {
	return e.engine.EvalInterpolation("${ "+expr+" }", e.vars(scope))
}

// Interpolate resolves a set.value string with arbitrary `${ … }` blocks via
// shared/cel's standard EvalInterpolation (literal+blocks, native type for a single
// block with no surrounding text).
func (e *celEvaluator) Interpolate(raw string, scope Scope) (any, error) {
	return e.engine.EvalInterpolation(raw, e.vars(scope))
}

func (e *celEvaluator) vars(scope Scope) cel.Vars {
	return cel.Vars{State: scope.State, Loop: scope.Loop}
}
