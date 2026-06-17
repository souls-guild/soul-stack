package cel

import "testing"

// TestEvalExpression_LoopVarBare — голая loop-переменная резолвится в
// expression-key (when:-форма, destiny/tasks.md §7).
func TestEvalExpression_LoopVarBare(t *testing.T) {
	e := newEngine(t)
	vars := Vars{Loop: map[string]any{"user": map[string]any{"active": true}}}

	val, err := e.EvalExpression("user.active", vars)
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if got := val.Value(); got != true {
		t.Fatalf("ожидали true, получили %v", got)
	}
}

// TestEvalInterpolation_LoopVar — `${ <as>.* }` интерполируется из loop-vars.
func TestEvalInterpolation_LoopVar(t *testing.T) {
	e := newEngine(t)
	vars := Vars{Loop: map[string]any{"user": map[string]any{"name": "alice"}}}

	out, err := e.EvalInterpolation("hello ${ user.name }", vars)
	if err != nil {
		t.Fatalf("EvalInterpolation: %v", err)
	}
	if out != "hello alice" {
		t.Fatalf("ожидали %q, получили %q", "hello alice", out)
	}
}

// TestEvalExpression_LoopVarIndex — index_as доступен наравне с as.
func TestEvalExpression_LoopVarIndex(t *testing.T) {
	e := newEngine(t)
	vars := Vars{Loop: map[string]any{"item": "x", "i": 2}}

	val, err := e.EvalExpression("i == 2", vars)
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if got := val.Value(); got != true {
		t.Fatalf("ожидали true, получили %v", got)
	}
}

// TestEvalExpression_LoopVarIsolated — loop-переменная видна только при наличии
// Loop в Vars; без него env её не знает (compile-ошибка), кеш базового env
// не загрязнён дочерним. Гарантирует изоляцию env по набору loop-имён.
func TestEvalExpression_LoopVarIsolated(t *testing.T) {
	e := newEngine(t)

	// С loop-переменной — ок.
	if _, err := e.EvalExpression("user.name == 'x'", Vars{Loop: map[string]any{"user": map[string]any{"name": "x"}}}); err != nil {
		t.Fatalf("loop eval: %v", err)
	}
	// Без неё — undeclared (базовый env её не знает).
	if _, err := e.EvalExpression("user.name == 'x'", Vars{}); err == nil {
		t.Fatalf("ожидали ошибку компиляции для необъявленной user без Loop")
	}
}

// TestEvalExpression_CacheNoCrossContextCollision — одно и то же выражение
// (`input.x`) в loop-контексте и в базовом не коллизит в compile-cache: ключ
// кеша дискриминирован по набору loop-имён (loopKey). Проверяем оба порядка
// (loop-then-base и base-then-loop) — компиляция против дочернего env не
// должна «протечь» в базовый и наоборот.
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
		// Прогреваем кеш сначала базовым контекстом.
		if _, err := e.EvalExpression(expr, Vars{Input: map[string]any{"x": "B"}}); err != nil {
			t.Fatalf("warm base: %v", err)
		}
		check(t, e)
	})
}

// TestEvalExpression_LoopVarCoexists — loop-переменная сосуществует с базовым
// контекстом (input) в одном выражении.
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
		t.Fatalf("ожидали %q, получили %q", "u-alice", out)
	}
}
