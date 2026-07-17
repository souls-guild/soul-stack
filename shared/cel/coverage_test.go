package cel

import (
	"testing"

	"github.com/google/cel-go/common/types/ref"
)

// recordingSink is a test CoverageSink that accumulates eval facts.
type recordingSink struct {
	records []record
}

type record struct {
	expr string
	out  any
}

func (s *recordingSink) Record(expr string, out ref.Val) {
	s.records = append(s.records, record{expr: expr, out: out.Value()})
}

// TestCoverageSink_BothBranches — both branches (truthy/falsy) of one
// where: expression are recorded by the sink ([ADR-023]).
func TestCoverageSink_BothBranches(t *testing.T) {
	e := newEngine(t)
	sink := &recordingSink{}
	e.SetCoverageSink(sink)

	const expr = "input.tier == 'web'"

	if _, err := e.EvalExpression(expr, Vars{Input: map[string]any{"tier": "web"}}); err != nil {
		t.Fatalf("EvalExpression (truthy): %v", err)
	}
	if _, err := e.EvalExpression(expr, Vars{Input: map[string]any{"tier": "db"}}); err != nil {
		t.Fatalf("EvalExpression (falsy): %v", err)
	}

	if len(sink.records) != 2 {
		t.Fatalf("expected 2 eval facts, got %d: %+v", len(sink.records), sink.records)
	}

	var sawTrue, sawFalse bool
	for _, r := range sink.records {
		if r.expr != expr {
			t.Errorf("expr = %q, expected %q", r.expr, expr)
		}
		switch r.out {
		case true:
			sawTrue = true
		case false:
			sawFalse = true
		}
	}
	if !sawTrue || !sawFalse {
		t.Fatalf("expected both branches (truthy=%v, falsy=%v)", sawTrue, sawFalse)
	}
}

// TestCoverageSink_Interpolation — EvalInterpolation goes through
// EvalExpression, so `${ … }` blocks also reach the sink (the hook is not
// duplicated in interpolation).
func TestCoverageSink_Interpolation(t *testing.T) {
	e := newEngine(t)
	sink := &recordingSink{}
	e.SetCoverageSink(sink)

	if _, err := e.EvalInterpolation("${ input.greeting }", Vars{Input: map[string]any{"greeting": "hi"}}); err != nil {
		t.Fatalf("EvalInterpolation: %v", err)
	}

	if len(sink.records) != 1 {
		t.Fatalf("expected 1 eval fact from interpolation, got %d", len(sink.records))
	}
	if sink.records[0].expr != "input.greeting" {
		t.Errorf("expr = %q, expected %q", sink.records[0].expr, "input.greeting")
	}
}

// TestCoverageSink_Nil — a nil sink (prod mode) does not panic and records nothing.
func TestCoverageSink_Nil(t *testing.T) {
	e := newEngine(t)
	// sink not set — default behavior.
	if _, err := e.EvalExpression("1 + 1 == 2", Vars{}); err != nil {
		t.Fatalf("EvalExpression with a nil sink: %v", err)
	}
}
