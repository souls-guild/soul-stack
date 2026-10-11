package scenario

// ExecutionModel is how a run orders its tasks (ADR-0091,
// docs/adr/0091-scenario-linear-barrier.md, item 15). It is decided once, when the
// run starts, and the whole run executes in it — never re-read from whichever
// instance serves a later step, so Keepers of different versions during a rollout
// cannot split one run between two orders. It lives in the run goroutine. Only a
// linear run's step row carries a step in its recipe (applyrun.RecipeStep), so an
// instance claiming a row knows the run's model from the row, never from its own
// configuration. The run records it in its engine provenance (engine_compat) and
// in its `incarnation.run_completed` event.
type ExecutionModel string

const (
	// ExecutionStaged is ADR-056: Passages derived from register references, a
	// block one Passage unit. Deprecated: kept for one release as the rollback
	// behind keeper.yml::scenario.execution_model (NIM-952).
	ExecutionStaged ExecutionModel = "staged"
	// ExecutionLinear is the ADR-0091: a step per top-level task, a block
	// rolled as one group counting as one, and per child of a default block, in plan
	// order ([config.LinearPassage]). The Passage loop in run.go iterates those
	// steps, each after the barrier of the one before it (NIM-944).
	ExecutionLinear ExecutionModel = "linear"
)

// executionModel is the model a run started now executes in: linear unless the
// Runner is configured staged by name, so a zero or unknown value never brings the
// rollback model back.
func (r *Runner) executionModel() ExecutionModel {
	m := r.deps.ExecutionModel
	if r.deps.ExecutionModelFn != nil {
		m = r.deps.ExecutionModelFn()
	}
	if m == ExecutionStaged {
		return ExecutionStaged
	}
	return ExecutionLinear
}
