package cel

import (
	"errors"
	"sync"
	"testing"
)

func newEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func TestEvalExpression_BoolKey(t *testing.T) {
	e := newEngine(t)
	vars := Vars{Input: map[string]any{"foo": "bar"}}

	val, err := e.EvalExpression("input.foo == 'bar'", vars)
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if got := val.Value(); got != true {
		t.Fatalf("ожидали true, получили %v", got)
	}
}

func TestEvalExpression_IntArithmetic(t *testing.T) {
	e := newEngine(t)
	vars := Vars{Input: map[string]any{"replicas": 2}}

	val, err := e.EvalExpression("input.replicas * 2", vars)
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if got := val.Value(); got != int64(4) {
		t.Fatalf("ожидали 4, получили %v (%T)", got, got)
	}
}

func TestEvalExpression_SoulprintSelf(t *testing.T) {
	e := newEngine(t)
	vars := Vars{SoulprintSelf: map[string]any{
		"os": map[string]any{"family": "debian"},
	}}

	val, err := e.EvalExpression("soulprint.self.os.family == 'debian'", vars)
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if val.Value() != true {
		t.Fatalf("ожидали true, получили %v", val.Value())
	}
}

func TestEvalExpression_RegisterAndIncarnation(t *testing.T) {
	e := newEngine(t)
	vars := Vars{
		Register:    map[string]any{"probe": map[string]any{"exit_code": 0}},
		Incarnation: map[string]any{"name": "redis-prod"},
	}

	val, err := e.EvalExpression("register.probe.exit_code == 0 && incarnation.name == 'redis-prod'", vars)
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if val.Value() != true {
		t.Fatalf("ожидали true, получили %v", val.Value())
	}
}

func TestEvalExpression_StdlibSizeContains(t *testing.T) {
	e := newEngine(t)
	vars := Vars{
		Input:    map[string]any{"hosts": []any{"a", "b", "c"}},
		Register: map[string]any{"role": map[string]any{"stdout": "is master node"}},
	}

	val, err := e.EvalExpression("size(input.hosts) > 0 && register.role.stdout.contains('master')", vars)
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if val.Value() != true {
		t.Fatalf("ожидали true, получили %v", val.Value())
	}
}

func TestEvalExpression_CompileError(t *testing.T) {
	e := newEngine(t)

	_, err := e.EvalExpression("input.foo ==", Vars{})
	var ce *ErrCompile
	if !errors.As(err, &ce) {
		t.Fatalf("ожидали *ErrCompile, получили %T: %v", err, err)
	}
}

func TestEvalExpression_EvalError(t *testing.T) {
	e := newEngine(t)

	// Division by zero — a CEL runtime error.
	_, err := e.EvalExpression("input.x / 0", Vars{Input: map[string]any{"x": 1}})
	var ee *ErrEval
	if !errors.As(err, &ee) {
		t.Fatalf("ожидали *ErrEval, получили %T: %v", err, err)
	}
}

func TestEvalInterpolation_SingleBlockNative(t *testing.T) {
	e := newEngine(t)
	vars := Vars{Input: map[string]any{"replicas": 2}}

	out, err := e.EvalInterpolation("${ input.replicas * 2 }", vars)
	if err != nil {
		t.Fatalf("EvalInterpolation: %v", err)
	}
	if out != int64(4) {
		t.Fatalf("ожидали int64(4), получили %v (%T)", out, out)
	}
}

func TestEvalInterpolation_Concat(t *testing.T) {
	e := newEngine(t)
	vars := Vars{Register: map[string]any{"master": map[string]any{"stdout": "10.0.0.5"}}}

	out, err := e.EvalInterpolation("redis-cli replicaof ${ register.master.stdout } 6379", vars)
	if err != nil {
		t.Fatalf("EvalInterpolation: %v", err)
	}
	if out != "redis-cli replicaof 10.0.0.5 6379" {
		t.Fatalf("неверная склейка: %q", out)
	}
}

func TestEvalInterpolation_MultipleBlocks(t *testing.T) {
	e := newEngine(t)
	vars := Vars{Input: map[string]any{"a": 1, "b": 2}}

	out, err := e.EvalInterpolation("${input.a}-${input.b}", vars)
	if err != nil {
		t.Fatalf("EvalInterpolation: %v", err)
	}
	if out != "1-2" {
		t.Fatalf("ожидали \"1-2\", получили %q", out)
	}
}

func TestEvalInterpolation_StringResultNoText(t *testing.T) {
	e := newEngine(t)
	vars := Vars{Input: map[string]any{"greeting": "world"}}

	// Single block, string result: a string is returned (not an int).
	out, err := e.EvalInterpolation("${ input.greeting }", vars)
	if err != nil {
		t.Fatalf("EvalInterpolation: %v", err)
	}
	if out != "world" {
		t.Fatalf("ожидали \"world\", получили %q", out)
	}
}

func TestEvalInterpolation_BalancedBraces(t *testing.T) {
	e := newEngine(t)
	vars := Vars{Input: map[string]any{"m": map[string]any{"k": "v"}}}

	// Inside ${ } there is a map literal with its own {}: the block's closing }
	// is the last one, not the first. The CEL parser determines the balancing.
	out, err := e.EvalInterpolation("${ {'a': 1}['a'] }", vars)
	if err != nil {
		t.Fatalf("EvalInterpolation balanced: %v", err)
	}
	if out != int64(1) {
		t.Fatalf("ожидали int64(1), получили %v (%T)", out, out)
	}
}

func TestEvalInterpolation_BalancedBracesConcat(t *testing.T) {
	e := newEngine(t)

	out, err := e.EvalInterpolation("v=${ {'a': 1}['a'] }!", Vars{})
	if err != nil {
		t.Fatalf("EvalInterpolation: %v", err)
	}
	if out != "v=1!" {
		t.Fatalf("ожидали \"v=1!\", получили %q", out)
	}
}

func TestEvalInterpolation_NestedParens(t *testing.T) {
	e := newEngine(t)
	vars := Vars{Input: map[string]any{"xs": []any{"a", "b"}}}

	out, err := e.EvalInterpolation("count=${ size(input.xs) }", vars)
	if err != nil {
		t.Fatalf("EvalInterpolation: %v", err)
	}
	if out != "count=2" {
		t.Fatalf("ожидали \"count=2\", получили %q", out)
	}
}

func TestEvalInterpolation_Escape(t *testing.T) {
	e := newEngine(t)

	out, err := e.EvalInterpolation(`shell-var literal: \${HOME}`, Vars{})
	if err != nil {
		t.Fatalf("EvalInterpolation: %v", err)
	}
	if out != "shell-var literal: ${HOME}" {
		t.Fatalf("escape не сработал: %q", out)
	}
}

func TestEvalInterpolation_DollarLiteral(t *testing.T) {
	e := newEngine(t)

	// A lone $ is not a marker ([templating.md §2.2]).
	out, err := e.EvalInterpolation("price: $100", Vars{})
	if err != nil {
		t.Fatalf("EvalInterpolation: %v", err)
	}
	if out != "price: $100" {
		t.Fatalf("ожидали литерал, получили %q", out)
	}
}

func TestEvalInterpolation_NoMarker(t *testing.T) {
	e := newEngine(t)

	out, err := e.EvalInterpolation("plain string", Vars{})
	if err != nil {
		t.Fatalf("EvalInterpolation: %v", err)
	}
	if out != "plain string" {
		t.Fatalf("ожидали без изменений, получили %q", out)
	}
}

func TestEvalInterpolation_Unterminated(t *testing.T) {
	e := newEngine(t)

	_, err := e.EvalInterpolation("${ input.foo", Vars{})
	var ce *ErrCompile
	if !errors.As(err, &ce) {
		t.Fatalf("ожидали *ErrCompile для незакрытого блока, получили %T: %v", err, err)
	}
}

func TestEvalInterpolation_ListConcatError(t *testing.T) {
	e := newEngine(t)
	vars := Vars{Input: map[string]any{"xs": []any{1, 2}}}

	// A list next to text — concatenating a structure is forbidden ([templating.md §5]).
	_, err := e.EvalInterpolation("xs: ${ input.xs }!", vars)
	var ee *ErrEval
	if !errors.As(err, &ee) {
		t.Fatalf("ожидали *ErrEval для склейки list, получили %T: %v", err, err)
	}
}

func TestEvalInterpolation_ListNativeSingleBlock(t *testing.T) {
	e := newEngine(t)
	vars := Vars{Input: map[string]any{"xs": []any{"a", "b"}}}

	// Single block without text — the list is returned as a native value
	// (not stringified).
	out, err := e.EvalInterpolation("${ input.xs }", vars)
	if err != nil {
		t.Fatalf("EvalInterpolation: %v", err)
	}
	if _, ok := out.(string); ok {
		t.Fatalf("ожидали нативный list, получили строку %q", out)
	}
	if out == nil {
		t.Fatalf("ожидали ненулевой list, получили nil")
	}
}

func TestUnsupported(t *testing.T) {
	e := newEngine(t)
	cases := map[string]string{
		"vault": "vault('secret/foo') == 'x'",
		"now":   "now() > timestamp('2020-01-01T00:00:00Z')",
	}
	for name, expr := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := e.EvalExpression(expr, Vars{})
			var ue *ErrUnsupported
			if !errors.As(err, &ue) {
				t.Fatalf("ожидали *ErrUnsupported, получили %T: %v", err, err)
			}
		})
	}
}

func TestUnsupportedInInterpolation(t *testing.T) {
	e := newEngine(t)

	_, err := e.EvalInterpolation("ip=${ vault('secret/ip') }", Vars{})
	var ue *ErrUnsupported
	if !errors.As(err, &ue) {
		t.Fatalf("ожидали *ErrUnsupported, получили %T: %v", err, err)
	}
}

func TestCache_HitAndReset(t *testing.T) {
	e := newEngine(t)
	vars := Vars{Input: map[string]any{"foo": "bar"}}

	if _, err := e.EvalExpression("input.foo == 'bar'", vars); err != nil {
		t.Fatalf("первый eval: %v", err)
	}
	// The normalized form of the same expression must map to the same key.
	if _, err := e.EvalExpression("input.foo   ==   'bar'", vars); err != nil {
		t.Fatalf("второй eval: %v", err)
	}

	e.mu.RLock()
	n := len(e.cache)
	e.mu.RUnlock()
	if n != 1 {
		t.Fatalf("ожидали 1 запись в кеше (нормализация), получили %d", n)
	}

	e.Reset()
	e.mu.RLock()
	n = len(e.cache)
	e.mu.RUnlock()
	if n != 0 {
		t.Fatalf("после Reset кеш не пуст: %d", n)
	}
}

func TestNormalizePreservesStringLiterals(t *testing.T) {
	e := newEngine(t)
	vars := Vars{Input: map[string]any{"s": "a  b"}}

	// Spaces inside a string literal are significant; normalization leaves them alone.
	val, err := e.EvalExpression("input.s == 'a  b'", vars)
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if val.Value() != true {
		t.Fatalf("нормализация испортила строковый литерал: %v", val.Value())
	}
}

func TestConcurrentEval(t *testing.T) {
	e := newEngine(t)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			vars := Vars{Input: map[string]any{"n": n}}
			val, err := e.EvalExpression("input.n >= 0", vars)
			if err != nil {
				t.Errorf("goroutine %d: %v", n, err)
				return
			}
			if val.Value() != true {
				t.Errorf("goroutine %d: ожидали true", n)
			}
		}(i)
	}
	wg.Wait()
}
