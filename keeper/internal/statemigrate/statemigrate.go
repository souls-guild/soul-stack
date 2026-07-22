// Package statemigrate — a pure core of the state_schema migration executor
// ([ADR-019], normative spec [docs/migrations.md]). Applies a chain of
// migrations to incarnation.state as a pure function state_v<N> → state_v<M>:
// NO Postgres, NO host-side effects, NO host context (a migration is a
// keeper-side operation on a single state object).
//
// transform grammar: rename / set / delete / move(=rename) / foreach
// ([dsl.go]). CEL expressions in set.value / foreach.in / ${ … } path segments
// are resolved via the migration-CEL engine ([eval.go], shared/cel.NewMigration):
// only the `state` variable is declared, other context is unavailable (sandbox by
// undeclaration), vault()/now() are blocked by guards.
//
// The transactional wrapper (PG SELECT FOR UPDATE → state_history snapshot per-step
// → COMMIT) and the L1 trial runner are NOT here (separate tasks on top of this core).
package statemigrate

import (
	"context"
	"fmt"
)

// Result — the outcome of applying a migration chain. FinalState — the state after
// the last step (a new map, the caller's input is not mutated). Steps —
// per-step before/after snapshots (for writing to state_history by the transactional
// layer on top of the core).
type Result struct {
	FinalState map[string]any
	Steps      []StepSnapshot
}

// StepSnapshot — a snapshot of a single chain step: versions and state before/after. StateBefore
// and StateAfter are independent deep-copies (the caller is free to serialize them into
// state_history without risking shared references).
type StepSnapshot struct {
	FromVersion int
	ToVersion   int
	StateBefore map[string]any
	StateAfter  map[string]any
}

// Apply runs the chain of migrations over state, returning the final
// state and per-step snapshots. A pure function: the input state is NOT mutated
// (deep-copy on entry), Postgres is not touched.
//
// The chain is validated for version continuity: the ToVersion of step i must equal
// the FromVersion of step i+1 (a gap → EvalError ClassChainVersion). An empty chain →
// FinalState = deep-copy of the input, Steps is empty.
//
// ev — an Evaluator over migration-CEL ([NewEvaluator]); reused by all
// steps (compile-cache). An error in any step aborts the chain and is returned
// as-is (the transactional layer performs the ROLLBACK / status: migration_failed).
func Apply(ctx context.Context, state map[string]any, chain Chain, ev Evaluator) (Result, error) {
	_ = ctx // reserved: the core is synchronous; ctx is threaded through for symmetry with the PG layer

	cur := deepCopyMap(state)
	steps := make([]StepSnapshot, 0, len(chain))

	for i, m := range chain {
		if i > 0 && chain[i-1].ToVersion != m.FromVersion {
			return Result{}, &EvalError{
				Class: ClassChainVersion,
				Msg:   fmt.Sprintf("chain break: step %d->%d follows %d->%d", m.FromVersion, m.ToVersion, chain[i-1].FromVersion, chain[i-1].ToVersion),
			}
		}

		before := deepCopyMap(cur)
		if err := applyOps(m.Transform, cur, ev, nil); err != nil {
			return Result{}, fmt.Errorf("migration %d->%d: %w", m.FromVersion, m.ToVersion, err)
		}
		steps = append(steps, StepSnapshot{
			FromVersion: m.FromVersion,
			ToVersion:   m.ToVersion,
			StateBefore: before,
			StateAfter:  deepCopyMap(cur),
		})
	}

	return Result{FinalState: cur, Steps: steps}, nil
}
