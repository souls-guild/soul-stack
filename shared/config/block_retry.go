package config

import (
	"fmt"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// RetryOverApply refuses a block's `retry:` that reaches an applier beneath it
// (NIM-910) with the code an applier's own `retry:` gets, `retry_on_apply_invalid`:
// the key would land on the applier, which invokes no module, and passing it on to
// the destiny's tasks would retry steps their author never marked retryable. Each
// applier is reported against the nearest enclosing block that writes `retry:` —
// the one whose value would reach it.
//
// It runs on the plan after include expansion, called by soul-lint and by the
// Keeper's run path alike: an applier spliced into a block by a within-block
// `include:` is invisible to the per-file validator.
func RetryOverApply(tasks []Task) []diag.Diagnostic {
	var out []diag.Diagnostic
	for i := range tasks {
		walkRetryOverApply(&tasks[i], nil, &out)
	}
	return out
}

func walkRetryOverApply(t *Task, from *Task, out *[]diag.Diagnostic) {
	if t.Block == nil {
		if t.Apply != nil && from != nil {
			*out = append(*out, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				Code: "retry_on_apply_invalid",
				Message: fmt.Sprintf("retry: on block %q reaches the applier %q beneath it -- an applier invokes no module, "+
					"and its destiny's tasks are not retried on the scenario's behalf (see docs/scenario/orchestration.md §2.1.2)",
					taskLabel(from), taskLabel(t)),
				Hint: "move the applier out of the block, or write retry: on the block's other children instead of on the block",
			})
		}
		return
	}
	if t.Retry != nil {
		from = t
	}
	for i := range t.Block.Block {
		walkRetryOverApply(&t.Block.Block[i], from, out)
	}
}
