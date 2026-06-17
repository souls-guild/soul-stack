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

// TestFlowControl_RegisterAccessible — register.<name>.* резолвится из
// Vars.Register (результаты предыдущих задач, ключевой контекст flow-control).
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
// soulprint.self доступны (flow_context-снапшот, доставляемый Keeper-ом).
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

// TestFlowControl_VaultGuarded — vault() конструктивно недоступен (внешний доступ
// keeper-only): guard возвращает ErrUnsupported, а не молча резолвит.
func TestFlowControl_VaultGuarded(t *testing.T) {
	e := newFlowControlEngine(t)
	_, err := e.EvalPredicate("vault('secret/x').enabled", Vars{})
	var ue *ErrUnsupported
	if !errors.As(err, &ue) {
		t.Fatalf("vault(): ошибка = %v, want *ErrUnsupported", err)
	}
}

// TestFlowControl_NowGuarded — now() недоступен (недетерминизм; симметрия с
// migration-CEL sandbox).
func TestFlowControl_NowGuarded(t *testing.T) {
	e := newFlowControlEngine(t)
	_, err := e.EvalPredicate("now() > now()", Vars{})
	var ue *ErrUnsupported
	if !errors.As(err, &ue) {
		t.Fatalf("now(): ошибка = %v, want *ErrUnsupported", err)
	}
}

// TestFlowControl_HostsAccessorIsolated — soulprint.hosts/soulprint.where —
// cross-host scenario-only аксессор, недоступен Soul-у: compile-error изоляции
// даже при Vars.AllowHosts=true (flow-control форсит allowHosts=false).
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

// TestFlowControl_InternalIdentGuarded — `__`-идентификатор зарезервирован за
// internal-механизмами (нельзя обойти vault()-macro прямым __vault_read).
func TestFlowControl_InternalIdentGuarded(t *testing.T) {
	e := newFlowControlEngine(t)
	_, err := e.EvalPredicate("__vault_read('secret/x', __vault_resolver).enabled", Vars{})
	var ue *ErrUnsupported
	if !errors.As(err, &ue) {
		t.Fatalf("__-ident: ошибка = %v, want *ErrUnsupported", err)
	}
}

// TestFlowControl_StateUndeclared — `state` — migration-only, в flow-control НЕ
// объявлена: обращение → compile-ошибка undeclared reference.
func TestFlowControl_StateUndeclared(t *testing.T) {
	e := newFlowControlEngine(t)
	_, err := e.EvalPredicate("state.foo == 1", Vars{State: map[string]any{"foo": 1}})
	var ce *ErrCompile
	if !errors.As(err, &ce) {
		t.Fatalf("state в flow-control: ошибка = %v, want *ErrCompile (undeclared)", err)
	}
}

// TestFlowControl_FunctionsWithoutIO — функции без I/O (size/contains/has/keys/
// comprehensions/конверсии/duration) доступны.
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

// TestFlowControl_NonBoolPredicate — не-bool результат предиката → ErrEval,
// несущий типизированный признак [ErrPredicateNotBool] (предикат обязан
// возвращать булево). Признак — sentinel, чтобы caller (statepredicate)
// отличал «не bool» от runtime-no-such-key без матчинга текста сообщения.
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

// TestFlowControl_RuntimeErrorNotPredicateNotBool — runtime no-such-key — это
// ErrEval, но НЕ ErrPredicateNotBool: sentinel помечает только «вернул не bool».
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

// TestFlowControl_EmptyPredicateTrue — пустой предикат → true (безусловный
// запуск): `when:` опущен ⇒ задача исполняется.
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

// TestFlowControl_RuntimeErrorOnMissingRegister — обращение к несуществующему
// register.* → runtime-error (ErrEval), а не паника: задача упадёт FAILED по
// таблице ошибок (templating.md §10).
func TestFlowControl_RuntimeErrorOnMissingRegister(t *testing.T) {
	e := newFlowControlEngine(t)
	_, err := e.EvalPredicate("register.nonexistent.changed", Vars{Register: map[string]any{}})
	var ee *ErrEval
	if !errors.As(err, &ee) {
		t.Fatalf("missing register: ошибка = %v, want *ErrEval", err)
	}
}

// TestFlowControl_WithVaultRejected — WithVault в flow-control-режиме отсекается
// конструктором (внешний доступ keeper-only, симметрия с NewMigration).
func TestFlowControl_WithVaultRejected(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{}}
	if _, err := NewFlowControl(WithVault(kv)); err == nil {
		t.Fatalf("NewFlowControl(WithVault): ошибка = nil, want non-nil (guard)")
	}
}
