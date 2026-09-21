package cel

import "testing"

// TestEvalInterpolation_ServiceVars — `${ vars.* }` resolves from Vars.ServiceVars.
func TestEvalInterpolation_ServiceVars(t *testing.T) {
	e := newEngine(t)
	vars := Vars{Vars: map[string]any{"db": map[string]any{"host": "pg-1"}}}

	out, err := e.EvalInterpolation("conn://${ vars.db.host }", vars)
	if err != nil {
		t.Fatalf("EvalInterpolation: %v", err)
	}
	if out != "conn://pg-1" {
		t.Fatalf("expected %q, got %q", "conn://pg-1", out)
	}
}

// TestEvalExpression_ServiceVars — a service var is available in expression keys (where:/when:).
func TestEvalExpression_ServiceVars(t *testing.T) {
	e := newEngine(t)
	vars := Vars{Vars: map[string]any{"feature": map[string]any{"enabled": true}}}

	val, err := e.EvalExpression("vars.feature.enabled", vars)
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if got := val.Value(); got != true {
		t.Fatalf("expected true, got %v", got)
	}
}

// TestEvalExpression_ServiceVarsEmptyNoPanic — empty ServiceVars does not panic; a field
// access is a normal no-such-key (ErrEval), not an env leak.
func TestEvalExpression_ServiceVarsEmptyNoPanic(t *testing.T) {
	e := newEngine(t)

	if _, err := e.EvalExpression("vars.absent", Vars{}); err == nil {
		t.Fatal("expected no-such-key for an empty ServiceVars, got nil")
	}
}

// TestEvalExpression_ServiceVarsCoexists — a service var coexists with input/loop in a single
// expression (host-invariant layer alongside the rest of the context).
func TestEvalExpression_ServiceVarsCoexists(t *testing.T) {
	e := newEngine(t)
	vars := Vars{
		Input: map[string]any{"env": "prod"},
		Vars:  map[string]any{"prefix": "svc-"},
		Loop:  map[string]any{"user": map[string]any{"name": "alice"}},
	}
	out, err := e.EvalInterpolation("${ vars.prefix }${ user.name }@${ input.env }", vars)
	if err != nil {
		t.Fatalf("EvalInterpolation: %v", err)
	}
	if out != "svc-alice@prod" {
		t.Fatalf("expected %q, got %q", "svc-alice@prod", out)
	}
}
