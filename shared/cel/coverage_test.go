package cel

import (
	"testing"

	"github.com/google/cel-go/common/types/ref"
)

// recordingSink — тестовый CoverageSink: копит факты eval-а.
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

// TestCoverageSink_BothBranches — обе ветки (truthy/falsy) одного
// where:-выражения фиксируются sink-ом ([ADR-023]).
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
		t.Fatalf("ожидали 2 факта eval-а, получили %d: %+v", len(sink.records), sink.records)
	}

	var sawTrue, sawFalse bool
	for _, r := range sink.records {
		if r.expr != expr {
			t.Errorf("expr = %q, ожидали %q", r.expr, expr)
		}
		switch r.out {
		case true:
			sawTrue = true
		case false:
			sawFalse = true
		}
	}
	if !sawTrue || !sawFalse {
		t.Fatalf("ожидали обе ветки (truthy=%v, falsy=%v)", sawTrue, sawFalse)
	}
}

// TestCoverageSink_Interpolation — EvalInterpolation проходит через
// EvalExpression, поэтому `${ … }`-блоки тоже попадают в sink (хук не
// дублируется в интерполяции).
func TestCoverageSink_Interpolation(t *testing.T) {
	e := newEngine(t)
	sink := &recordingSink{}
	e.SetCoverageSink(sink)

	if _, err := e.EvalInterpolation("${ input.greeting }", Vars{Input: map[string]any{"greeting": "hi"}}); err != nil {
		t.Fatalf("EvalInterpolation: %v", err)
	}

	if len(sink.records) != 1 {
		t.Fatalf("ожидали 1 факт eval-а от интерполяции, получили %d", len(sink.records))
	}
	if sink.records[0].expr != "input.greeting" {
		t.Errorf("expr = %q, ожидали %q", sink.records[0].expr, "input.greeting")
	}
}

// TestCoverageSink_Nil — nil-sink (прод-режим) не падает и не учитывает.
func TestCoverageSink_Nil(t *testing.T) {
	e := newEngine(t)
	// sink не установлен — поведение по умолчанию.
	if _, err := e.EvalExpression("1 + 1 == 2", Vars{}); err != nil {
		t.Fatalf("EvalExpression с nil-sink: %v", err)
	}
}
