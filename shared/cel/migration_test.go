package cel

import (
	"errors"
	"testing"
)

func newMigrationEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := NewMigration()
	if err != nil {
		t.Fatalf("NewMigration: %v", err)
	}
	return e
}

// TestMigration_StateAccessible — `state.<path>` доступен и резолвится из
// Vars.State.
func TestMigration_StateAccessible(t *testing.T) {
	e := newMigrationEngine(t)
	out, err := e.EvalExpression("int(state.mb) * 1048576", Vars{
		State: map[string]any{"mb": 512},
	})
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if got := out.Value(); got != int64(536870912) {
		t.Fatalf("результат = %v (%T), want 536870912", got, got)
	}
}

// TestMigration_ContextVarsUndeclared — register/soulprint/essence/input/
// incarnation/vars НЕ объявлены в migration-env: обращение → compile-ошибка
// undeclared reference (sandbox by undeclaration, ADR-019).
func TestMigration_ContextVarsUndeclared(t *testing.T) {
	e := newMigrationEngine(t)
	for _, expr := range []string{
		"register.foo",
		"soulprint.self.os.family",
		"essence.bar",
		"input.baz",
		"incarnation.name",
		"vars.qux",
	} {
		_, err := e.EvalExpression(expr, Vars{State: map[string]any{}})
		var ce *ErrCompile
		if !errors.As(err, &ce) {
			t.Errorf("expr %q: ошибка = %v, want *ErrCompile (undeclared)", expr, err)
		}
	}
}

// TestMigration_VaultGuarded — vault() без KVReader отсекается guard-ом как
// ErrUnsupported (а не undeclared): миграция не тянет секреты.
func TestMigration_VaultGuarded(t *testing.T) {
	e := newMigrationEngine(t)
	_, err := e.EvalExpression("vault('secret/x').password", Vars{State: map[string]any{}})
	var ue *ErrUnsupported
	if !errors.As(err, &ue) {
		t.Fatalf("vault(): ошибка = %v, want *ErrUnsupported", err)
	}
}

// TestMigration_NowGuarded — now() отсекается guard-ом (воспроизводимость
// тестов миграций).
func TestMigration_NowGuarded(t *testing.T) {
	e := newMigrationEngine(t)
	_, err := e.EvalExpression("now()", Vars{State: map[string]any{}})
	var ue *ErrUnsupported
	if !errors.As(err, &ue) {
		t.Fatalf("now(): ошибка = %v, want *ErrUnsupported", err)
	}
}

// TestMigration_LoopVarInScope — foreach-переменная (`as:`) объявляется тем же
// механизмом Vars.Loop/Extend, что и loop:-переменные: `<as>` виден в выражении.
func TestMigration_LoopVarInScope(t *testing.T) {
	e := newMigrationEngine(t)
	out, err := e.EvalExpression("user_name + '-suffix'", Vars{
		State: map[string]any{},
		Loop:  map[string]any{"user_name": "app"},
	})
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if got := out.Value(); got != "app-suffix" {
		t.Fatalf("результат = %v, want app-suffix", got)
	}
}

// TestMigration_StateUndeclaredInRegularEngine — симметрично: обычный (не
// migration) Engine НЕ объявляет `state`, чтобы Vars.State не протекал в
// scenario/destiny-контекст.
func TestMigration_StateUndeclaredInRegularEngine(t *testing.T) {
	e := newEngine(t)
	_, err := e.EvalExpression("state.foo", Vars{State: map[string]any{"foo": 1}})
	var ce *ErrCompile
	if !errors.As(err, &ce) {
		t.Fatalf("state в обычном Engine: ошибка = %v, want *ErrCompile (undeclared)", err)
	}
}

// TestMigration_WithVaultRejected — WithVault(kv) в migration-режиме отсекается
// конструктором (ADR-019: migration-CEL sandbox запрещает vault()). Закрывает
// latent foot-gun: NewMigration принимает ...Option, поэтому без guard-а
// vault() протёк бы в migration-движок.
func TestMigration_WithVaultRejected(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{}}
	_, err := NewMigration(WithVault(kv))
	if err == nil {
		t.Fatalf("NewMigration(WithVault): ошибка = nil, want non-nil (guard сработал)")
	}
}

// TestMigration_NoOptionsOK — NewMigration() без опций строится без ошибки
// (нормальный путь, как зовёт keeper/internal/statemigrate).
func TestMigration_NoOptionsOK(t *testing.T) {
	if _, err := NewMigration(); err != nil {
		t.Fatalf("NewMigration(): %v", err)
	}
}

// TestMigration_RegularVaultEngineNotBroken — регресс: WithVault на ОБЫЧНОМ
// (не migration) Engine продолжает работать (guard триггерит только при
// migration=true && kv!=nil).
func TestMigration_RegularVaultEngineNotBroken(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{
		"secret/db": {"dsn": "postgres://real"},
	}}
	e, err := New(WithVault(kv))
	if err != nil {
		t.Fatalf("New(WithVault): %v", err)
	}
	out, err := e.EvalInterpolation("${ vault('secret/db#dsn') }", Vars{})
	if err != nil {
		t.Fatalf("EvalInterpolation: %v", err)
	}
	if out != "postgres://real" {
		t.Fatalf("vault() = %q, want postgres://real", out)
	}
}
