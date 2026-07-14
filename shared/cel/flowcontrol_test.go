package cel

import (
	"errors"
	"testing"
)

func newFlowControlEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := NewFlowControl()
	if err != nil {
		t.Fatalf("NewFlowControl: %v", err)
	}
	return e
}

// TestFlowControl_RegisterAccessible — register.<name>.* resolves from
// Vars.Register (results of previous tasks, key flow-control context).
func TestFlowControl_RegisterAccessible(t *testing.T) {
	e := newFlowControlEngine(t)
	ok, err := e.EvalPredicate("register.probe.exit_code == 0", Vars{
		Register: map[string]any{"probe": map[string]any{"exit_code": 0}},
	})
	if err != nil {
		t.Fatalf("EvalPredicate: %v", err)
	}
	if !ok {
		t.Fatalf("предикат = false, want true")
	}
}

// TestFlowControl_ContextVarsAccessible — input/vars/essence/incarnation +
// soulprint.self are available (the flow_context snapshot delivered by Keeper).
func TestFlowControl_ContextVarsAccessible(t *testing.T) {
	e := newFlowControlEngine(t)
	vars := Vars{
		Input:         map[string]any{"do_restart": true},
		Vars:          map[string]any{"n": 3},
		Essence:       map[string]any{"redis": map[string]any{"maxmemory": "512mb"}},
		Incarnation:   map[string]any{"name": "redis-prod"},
		SoulprintSelf: map[string]any{"os": map[string]any{"family": "debian"}},
	}
	for _, expr := range []string{
		"input.do_restart",
		"vars.n == 3",
		"essence.redis.maxmemory == '512mb'",
		"incarnation.name == 'redis-prod'",
		"soulprint.self.os.family == 'debian'",
	} {
		ok, err := e.EvalPredicate(expr, vars)
		if err != nil {
			t.Errorf("expr %q: %v", expr, err)
			continue
		}
		if !ok {
			t.Errorf("expr %q = false, want true", expr)
		}
	}
}

// TestFlowControl_VaultGuarded — vault() is structurally unavailable (external
// access is keeper-only): the guard returns ErrUnsupported rather than silently resolving.
func TestFlowControl_VaultGuarded(t *testing.T) {
	e := newFlowControlEngine(t)
	_, err := e.EvalPredicate("vault('secret/x').enabled", Vars{})
	var ue *ErrUnsupported
	if !errors.As(err, &ue) {
		t.Fatalf("vault(): ошибка = %v, want *ErrUnsupported", err)
	}
}

// TestFlowControl_NowGuarded — now() is unavailable (nondeterminism; symmetric with
// the migration-CEL sandbox).
func TestFlowControl_NowGuarded(t *testing.T) {
	e := newFlowControlEngine(t)
	_, err := e.EvalPredicate("now() > now()", Vars{})
	var ue *ErrUnsupported
	if !errors.As(err, &ue) {
		t.Fatalf("now(): ошибка = %v, want *ErrUnsupported", err)
	}
}

// TestFlowControl_HostsAccessorIsolated — soulprint.hosts/soulprint.where is a
// cross-host scenario-only accessor, unavailable to the Soul: an isolation
// compile-error even with Vars.AllowHosts=true (flow-control forces allowHosts=false).
func TestFlowControl_HostsAccessorIsolated(t *testing.T) {
	e := newFlowControlEngine(t)
	for _, expr := range []string{
		"soulprint.hosts.size() > 0",
		"size(soulprint.where(\"role == 'primary'\")) > 0",
	} {
		_, err := e.EvalPredicate(expr, Vars{AllowHosts: true})
		var ue *ErrUnsupported
		if !errors.As(err, &ue) {
			t.Errorf("expr %q: ошибка = %v, want *ErrUnsupported (изоляция host-аксессора)", expr, err)
		}
	}
}

// TestFlowControl_InternalIdentGuarded — the `__` identifier is reserved for
// internal mechanisms (you can't bypass the vault() macro with a direct __vault_read).
func TestFlowControl_InternalIdentGuarded(t *testing.T) {
	e := newFlowControlEngine(t)
	_, err := e.EvalPredicate("__vault_read('secret/x', __vault_resolver).enabled", Vars{})
	var ue *ErrUnsupported
	if !errors.As(err, &ue) {
		t.Fatalf("__-ident: ошибка = %v, want *ErrUnsupported", err)
	}
}

// TestFlowControl_StateUndeclared — `state` is migration-only, NOT declared in
// flow-control: accessing it → compile-error undeclared reference.
func TestFlowControl_StateUndeclared(t *testing.T) {
	e := newFlowControlEngine(t)
	_, err := e.EvalPredicate("state.foo == 1", Vars{State: map[string]any{"foo": 1}})
	var ce *ErrCompile
	if !errors.As(err, &ce) {
		t.Fatalf("state в flow-control: ошибка = %v, want *ErrCompile (undeclared)", err)
	}
}

// TestFlowControl_FunctionsWithoutIO — functions without I/O (size/contains/has/keys/
// comprehensions/conversions/duration) are available.
func TestFlowControl_FunctionsWithoutIO(t *testing.T) {
	e := newFlowControlEngine(t)
	vars := Vars{
		Register: map[string]any{
			"probe": map[string]any{
				"stdout": "master",
				"items":  []any{1, 2, 3},
			},
		},
	}
	cases := []string{
		"size(register.probe.items) == 3",
		"register.probe.stdout.contains('mast')",
		"has(register.probe.stdout)",
		"register.probe.items.exists(x, x == 2)",
		"register.probe.items.all(x, x > 0)",
		"register.probe.items.filter(x, x > 1).size() == 2",
		"int('7') + 1 == 8",
		"string(42) == '42'",
		"bool('true')",
		"duration('30s') < duration('1m')",
	}
	for _, expr := range cases {
		ok, err := e.EvalPredicate(expr, vars)
		if err != nil {
			t.Errorf("expr %q: %v", expr, err)
			continue
		}
		if !ok {
			t.Errorf("expr %q = false, want true", expr)
		}
	}
}

// TestFlowControl_NonBoolPredicate — a non-bool predicate result → ErrEval carrying
// the typed marker [ErrPredicateNotBool] (a predicate must return a boolean). The
// marker is a sentinel so the caller (statepredicate) can tell "not bool" from
// runtime-no-such-key without matching message text.
func TestFlowControl_NonBoolPredicate(t *testing.T) {
	e := newFlowControlEngine(t)
	_, err := e.EvalPredicate("register.probe.exit_code", Vars{
		Register: map[string]any{"probe": map[string]any{"exit_code": 0}},
	})
	var ee *ErrEval
	if !errors.As(err, &ee) {
		t.Fatalf("не-bool предикат: ошибка = %v, want *ErrEval", err)
	}
	if !errors.Is(err, ErrPredicateNotBool) {
		t.Fatalf("не-bool предикат: errors.Is(err, ErrPredicateNotBool)=false, err=%v", err)
	}
}

// TestFlowControl_RuntimeErrorNotPredicateNotBool — a runtime no-such-key is
// ErrEval but NOT ErrPredicateNotBool: the sentinel marks only "returned non-bool".
func TestFlowControl_RuntimeErrorNotPredicateNotBool(t *testing.T) {
	e := newFlowControlEngine(t)
	_, err := e.EvalPredicate("register.nonexistent.changed", Vars{Register: map[string]any{}})
	var ee *ErrEval
	if !errors.As(err, &ee) {
		t.Fatalf("missing register: ошибка = %v, want *ErrEval", err)
	}
	if errors.Is(err, ErrPredicateNotBool) {
		t.Fatalf("no-such-key ошибочно помечен ErrPredicateNotBool: %v", err)
	}
}

// TestFlowControl_EmptyPredicateTrue — an empty predicate → true (unconditional
// run): `when:` omitted ⇒ the task executes.
func TestFlowControl_EmptyPredicateTrue(t *testing.T) {
	e := newFlowControlEngine(t)
	ok, err := e.EvalPredicate("", Vars{})
	if err != nil {
		t.Fatalf("EvalPredicate(''): %v", err)
	}
	if !ok {
		t.Fatalf("пустой предикат = false, want true")
	}
}

// TestFlowControl_RuntimeErrorOnMissingRegister — accessing a nonexistent
// register.* → runtime error (ErrEval), not a panic: the task fails FAILED per the
// error table (templating.md §10).
func TestFlowControl_RuntimeErrorOnMissingRegister(t *testing.T) {
	e := newFlowControlEngine(t)
	_, err := e.EvalPredicate("register.nonexistent.changed", Vars{Register: map[string]any{}})
	var ee *ErrEval
	if !errors.As(err, &ee) {
		t.Fatalf("missing register: ошибка = %v, want *ErrEval", err)
	}
}

// TestFlowControl_WithVaultRejected — WithVault in flow-control mode is rejected by
// the constructor (external access is keeper-only, symmetric with NewMigration).
func TestFlowControl_WithVaultRejected(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{}}
	if _, err := NewFlowControl(WithVault(kv)); err == nil {
		t.Fatalf("NewFlowControl(WithVault): ошибка = nil, want non-nil (guard)")
	}
}
