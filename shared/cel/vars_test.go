package cel

import "testing"

// TestEvalInterpolation_Vars — `${ vars.* }` резолвится из Vars.Vars (task-level
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

// TestEvalExpression_Vars — vars доступны в expression-key (where:/when:) голой
// формой vars.<key>.
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

// TestEvalExpression_VarsEmptyNoPanic — пустой Vars не даёт паники; обращение к
// vars.<key> — штатный no-such-key (ErrEval), не leak в env.
func TestEvalExpression_VarsEmptyNoPanic(t *testing.T) {
	e := newEngine(t)

	if _, err := e.EvalExpression("vars.absent", Vars{}); err == nil {
		t.Fatal("ожидали no-such-key для пустого Vars, получили nil")
	}
}
