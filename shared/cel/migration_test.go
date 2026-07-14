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

// TestMigration_StateAccessible — `state.<path>` is available and resolves from
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
// incarnation/vars are NOT declared in the migration env: access → compile-error
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

// TestMigration_VarsStillUndeclared_AfterVarLayer — the var→var feature (VarRefs +
// resolveVarLayer, scenario/destiny phase) does NOT touch migration-CEL (case #11):
// `vars` stays undeclared in the migration env, so `${ vars.x }` in set.value →
// compile-error undeclared reference, as before the feature. VarRefs lives in the
// regular Engine (New), not the migration Engine; the var layer exists only in the
// scenario/destiny pass. Guard test against a "var→var leaked into migration" regression.
func TestMigration_VarsStillUndeclared_AfterVarLayer(t *testing.T) {
	e := newMigrationEngine(t)
	// Interpolation `${ vars.x }` in set.value: the `vars.x` block compiles in the
	// migration env, where `vars` is not declared → ErrCompile (undeclared).
	_, err := e.EvalInterpolation("${ vars.x }", Vars{State: map[string]any{}})
	var ce *ErrCompile
	if !errors.As(err, &ce) {
		t.Fatalf("migration ${ vars.x }: ошибка = %v, want *ErrCompile (vars undeclared, фича var→var не задела миграцию)", err)
	}
}

// TestMigration_VaultGuarded — vault() without a KVReader is cut by the guard as
// ErrUnsupported (not undeclared): a migration doesn't pull secrets.
func TestMigration_VaultGuarded(t *testing.T) {
	e := newMigrationEngine(t)
	_, err := e.EvalExpression("vault('secret/x').password", Vars{State: map[string]any{}})
	var ue *ErrUnsupported
	if !errors.As(err, &ue) {
		t.Fatalf("vault(): ошибка = %v, want *ErrUnsupported", err)
	}
}

// TestMigration_NowGuarded — now() is cut by the guard (reproducibility of
// migration tests).
func TestMigration_NowGuarded(t *testing.T) {
	e := newMigrationEngine(t)
	_, err := e.EvalExpression("now()", Vars{State: map[string]any{}})
	var ue *ErrUnsupported
	if !errors.As(err, &ue) {
		t.Fatalf("now(): ошибка = %v, want *ErrUnsupported", err)
	}
}

// TestMigration_LoopVarInScope — the foreach variable (`as:`) is declared by the
// same Vars.Loop/Extend mechanism as loop: variables: `<as>` is visible in the expression.
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

// TestMigration_StateUndeclaredInRegularEngine — symmetric: the regular (non-
// migration) Engine does NOT declare `state`, so Vars.State doesn't leak into the
// scenario/destiny context.
func TestMigration_StateUndeclaredInRegularEngine(t *testing.T) {
	e := newEngine(t)
	_, err := e.EvalExpression("state.foo", Vars{State: map[string]any{"foo": 1}})
	var ce *ErrCompile
	if !errors.As(err, &ce) {
		t.Fatalf("state в обычном Engine: ошибка = %v, want *ErrCompile (undeclared)", err)
	}
}

// TestMigration_WithVaultRejected — WithVault(kv) in migration mode is rejected by
// the constructor (ADR-019: the migration-CEL sandbox forbids vault()). Closes a
// latent foot-gun: NewMigration takes ...Option, so without the guard vault() would
// leak into the migration engine.
func TestMigration_WithVaultRejected(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{}}
	_, err := NewMigration(WithVault(kv))
	if err == nil {
		t.Fatalf("NewMigration(WithVault): ошибка = nil, want non-nil (guard сработал)")
	}
}

// TestMigration_NoOptionsOK — NewMigration() with no options builds without error
// (the normal path, as keeper/internal/statemigrate calls it).
func TestMigration_NoOptionsOK(t *testing.T) {
	if _, err := NewMigration(); err != nil {
		t.Fatalf("NewMigration(): %v", err)
	}
}

// TestMigration_RegularVaultEngineNotBroken — regression: WithVault on the REGULAR
// (non-migration) Engine keeps working (the guard triggers only when
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
