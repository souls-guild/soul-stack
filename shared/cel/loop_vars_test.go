package cel

import "testing"

// TestEvalExpression_LoopVarBare — a bare loop variable resolves in an
// expression-key (when: form, destiny/tasks.md §7).
func TestEvalExpression_LoopVarBare(t *testing.T) {
	e := newEngine(t)
	vars := Vars{Loop: map[string]any{"user": map[string]any{"active": true}}}

	val, err := e.EvalExpression("user.active", vars)
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if got := val.Value(); got != true {
		t.Fatalf("expected true, got %v", got)
	}
}

// TestEvalInterpolation_LoopVar — `${ <as>.* }` is interpolated from loop-vars.
func TestEvalInterpolation_LoopVar(t *testing.T) {
	e := newEngine(t)
	vars := Vars{Loop: map[string]any{"user": map[string]any{"name": "alice"}}}

	out, err := e.EvalInterpolation("hello ${ user.name }", vars)
	if err != nil {
		t.Fatalf("EvalInterpolation: %v", err)
	}
	if out != "hello alice" {
		t.Fatalf("expected %q, got %q", "hello alice", out)
	}
}

// TestEvalExpression_LoopVarIndex — index_as is available on par with as.
func TestEvalExpression_LoopVarIndex(t *testing.T) {
	e := newEngine(t)
	vars := Vars{Loop: map[string]any{"item": "x", "i": 2}}

	val, err := e.EvalExpression("i == 2", vars)
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if got := val.Value(); got != true {
		t.Fatalf("expected true, got %v", got)
	}
}

// TestEvalExpression_LoopVarIsolated — a loop variable is visible only when Loop
// is present in Vars; without it the env doesn't know it (compile error), and the
// base env cache is not polluted by the child. Guarantees env isolation by the
// set of loop names.
func TestEvalExpression_LoopVarIsolated(t *testing.T) {
	e := newEngine(t)

	// With a loop variable — ok.
	if _, err := e.EvalExpression("user.name == 'x'", Vars{Loop: map[string]any{"user": map[string]any{"name": "x"}}}); err != nil {
		t.Fatalf("loop eval: %v", err)
	}
	// Without it — undeclared (the base env doesn't know it).
	if _, err := e.EvalExpression("user.name == 'x'", Vars{}); err == nil {
		t.Fatalf("expected a compile error for an undeclared user without Loop")
	}
}

// TestEvalExpression_CacheNoCrossContextCollision — the same expression
// (`input.x`) in a loop context and in the base context does not collide in the
// compile cache: the cache key is discriminated by the set of loop names
// (loopKey). We check both orders (loop-then-base and base-then-loop) —
// compilation against the child env must not "leak" into the base and vice versa.
func TestEvalExpression_CacheNoCrossContextCollision(t *testing.T) {
	const expr = "input.x"

	check := func(t *testing.T, e *Engine) {
		loopVars := Vars{Input: map[string]any{"x": "L"}, Loop: map[string]any{"item": "i"}}
		baseVars := Vars{Input: map[string]any{"x": "B"}}

		loopVal, err := e.EvalExpression(expr, loopVars)
		if err != nil {
			t.Fatalf("loop ctx: %v", err)
		}
		baseVal, err := e.EvalExpression(expr, baseVars)
		if err != nil {
			t.Fatalf("base ctx: %v", err)
		}
		if loopVal.Value() != "L" {
			t.Errorf("loop ctx: got %v, want L", loopVal.Value())
		}
		if baseVal.Value() != "B" {
			t.Errorf("base ctx: got %v, want B", baseVal.Value())
		}
	}

	t.Run("loop-then-base", func(t *testing.T) {
		check(t, newEngine(t))
	})
	t.Run("base-then-loop", func(t *testing.T) {
		e := newEngine(t)
		// Warm the cache with the base context first.
		if _, err := e.EvalExpression(expr, Vars{Input: map[string]any{"x": "B"}}); err != nil {
			t.Fatalf("warm base: %v", err)
		}
		check(t, e)
	})
}

// TestEvalExpression_LoopVarCoexists — a loop variable coexists with the base
// context (input) in one expression.
func TestEvalExpression_LoopVarCoexists(t *testing.T) {
	e := newEngine(t)
	vars := Vars{
		Input: map[string]any{"prefix": "u-"},
		Loop:  map[string]any{"user": map[string]any{"name": "alice"}},
	}
	out, err := e.EvalInterpolation("${ input.prefix }${ user.name }", vars)
	if err != nil {
		t.Fatalf("EvalInterpolation: %v", err)
	}
	if out != "u-alice" {
		t.Fatalf("expected %q, got %q", "u-alice", out)
	}
}
