package cel

import "testing"

// TestEvalInterpolation_Vars — `${ vars.* }` resolves from Vars.Vars (task-level
// vars:, destiny/tasks.md §9).
func TestEvalInterpolation_Vars(t *testing.T) {
	e := newEngine(t)
	vars := Vars{Vars: map[string]any{"addr": "10.0.0.1"}}

	out, err := e.EvalInterpolation("ping ${ vars.addr }", vars)
	if err != nil {
		t.Fatalf("EvalInterpolation: %v", err)
	}
	if out != "ping 10.0.0.1" {
		t.Fatalf("ожидали %q, получили %q", "ping 10.0.0.1", out)
	}
}

// TestEvalExpression_Vars — vars are available in expression keys (where:/when:) in
// the bare form vars.<key>.
func TestEvalExpression_Vars(t *testing.T) {
	e := newEngine(t)
	vars := Vars{Vars: map[string]any{"target": "b.example.com"}}

	val, err := e.EvalExpression("vars.target == 'b.example.com'", vars)
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if got := val.Value(); got != true {
		t.Fatalf("ожидали true, получили %v", got)
	}
}

// TestEvalExpression_VarsEmptyNoPanic — empty Vars does not panic; a vars.<key>
// access is a normal no-such-key (ErrEval), not an env leak.
func TestEvalExpression_VarsEmptyNoPanic(t *testing.T) {
	e := newEngine(t)

	if _, err := e.EvalExpression("vars.absent", Vars{}); err == nil {
		t.Fatal("ожидали no-such-key для пустого Vars, получили nil")
	}
}
