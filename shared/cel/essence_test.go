package cel

import "testing"

// TestEvalInterpolation_Essence — `${ essence.* }` resolves from Vars.Essence.
func TestEvalInterpolation_Essence(t *testing.T) {
	e := newEngine(t)
	vars := Vars{Essence: map[string]any{"db": map[string]any{"host": "pg-1"}}}

	out, err := e.EvalInterpolation("conn://${ essence.db.host }", vars)
	if err != nil {
		t.Fatalf("EvalInterpolation: %v", err)
	}
	if out != "conn://pg-1" {
		t.Fatalf("ожидали %q, получили %q", "conn://pg-1", out)
	}
}

// TestEvalExpression_Essence — essence is available in expression keys (where:/when:).
func TestEvalExpression_Essence(t *testing.T) {
	e := newEngine(t)
	vars := Vars{Essence: map[string]any{"feature": map[string]any{"enabled": true}}}

	val, err := e.EvalExpression("essence.feature.enabled", vars)
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if got := val.Value(); got != true {
		t.Fatalf("ожидали true, получили %v", got)
	}
}

// TestEvalExpression_EssenceEmptyNoPanic — empty Essence does not panic; a field
// access is a normal no-such-key (ErrEval), not an env leak.
func TestEvalExpression_EssenceEmptyNoPanic(t *testing.T) {
	e := newEngine(t)

	if _, err := e.EvalExpression("essence.absent", Vars{}); err == nil {
		t.Fatal("ожидали no-such-key для пустого Essence, получили nil")
	}
}

// TestEvalExpression_EssenceCoexists — essence coexists with input/loop in a single
// expression (host-invariant layer alongside the rest of the context).
func TestEvalExpression_EssenceCoexists(t *testing.T) {
	e := newEngine(t)
	vars := Vars{
		Input:   map[string]any{"env": "prod"},
		Essence: map[string]any{"prefix": "svc-"},
		Loop:    map[string]any{"user": map[string]any{"name": "alice"}},
	}
	out, err := e.EvalInterpolation("${ essence.prefix }${ user.name }@${ input.env }", vars)
	if err != nil {
		t.Fatalf("EvalInterpolation: %v", err)
	}
	if out != "svc-alice@prod" {
		t.Fatalf("ожидали %q, получили %q", "svc-alice@prod", out)
	}
}
