package trial

import (
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/servicevars"
	"github.com/souls-guild/soul-stack/shared/cel"
)

// TestCoverageSink_ALazyVarsResultIsNotEvaluated — an expression whose result is
// the `vars` map itself (`${ vars }`) is recorded as a non-branch without its view
// being evaluated: the sink tells a bool by type, and a lazy map's Value() would
// evaluate every var again, a broken one included (ADR-0082 amendment 2026-10-15).
func TestCoverageSink_ALazyVarsResultIsNotEvaluated(t *testing.T) {
	view, err := servicevars.ViewOf(map[string]any{"broken": "${ vars.nope }"}, servicevars.IncarnationContext{})
	if err != nil {
		t.Fatalf("ViewOf: %v", err)
	}
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	out, err := engine.EvalExpression("vars", cel.Vars{View: view})
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}

	sink := newCoverageSink()
	sink.Record("vars", out)
	if n := view.Evaluations(); n != 0 {
		t.Fatalf("recording the result evaluated %d definitions, want 0", n)
	}
	if rep := sink.Report(); len(rep.Branches) != 0 || len(rep.NonBranch) != 1 {
		t.Fatalf("report = %+v, want one non-branch expression", rep)
	}
}
