package cel

import (
	"slices"
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

// TestVarView_CheckRecordsNoCoverageAndFillsNoMemo — [VarView.Check] only observes:
// it evaluates each definition once, records none of them in the sink, keeps none in
// the memo and counts none in Evaluations. A read after it evaluates and records as if
// the check had not run, and a value already memoized is reused by the check.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: store a checked value with
// v.remember in resolve — the read after the check is a memo hit and records nothing.
func TestVarView_CheckRecordsNoCoverageAndFillsNoMemo(t *testing.T) {
	e := newEngine(t)
	sink := &recordingSink{}
	e.SetCoverageSink(sink)
	v := NewVarView(nil, taskLayer(e, map[string]any{
		"a":    "${ vars.b && true }",
		"b":    "${ 'c' == 'c' }",
		"read": "${ 1 == 1 }",
	}))
	var evaluated []string
	v.onEvaluate = func(name string, _ int) { evaluated = append(evaluated, name) }

	if got := mustLookup(t, v, "read"); got != true {
		t.Fatalf("read = %#v, want true", got)
	}
	sink.records, evaluated = nil, nil
	if err := v.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(sink.records) != 0 {
		t.Fatalf("Check recorded %+v, want nothing", sink.records)
	}
	if !slices.Equal(evaluated, []string{"a", "b"}) {
		t.Fatalf("Check evaluated %v, want [a b]: each once, the memoized read not again", evaluated)
	}
	if n := v.Evaluations(); n != 1 {
		t.Fatalf("Evaluations() = %d after the check, want the one read's 1", n)
	}

	if got := mustLookup(t, v, "b"); got != true {
		t.Fatalf("b = %#v, want true", got)
	}
	if len(sink.records) != 1 || sink.records[0].expr != "'c' == 'c'" {
		t.Fatalf("the read after the check recorded %+v, want b's definition", sink.records)
	}
}
