package cel

import (
	"errors"
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// TestDefault_PresentSelect — default(x, fb) with a present select field →
// the value x itself (fallback unreachable).
func TestDefault_PresentSelect(t *testing.T) {
	e := newEngine(t)

	out, err := e.EvalExpression(`default(essence.tls_enable, false)`, Vars{Essence: map[string]any{
		"tls_enable": true,
	}})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if got := out.Value(); got != true {
		t.Fatalf("default(present) = %v, want true (значение x, не fallback)", got)
	}
}

// TestDefault_AbsentSelect — ★ KEY test: CEL eagerness is bypassed.
// default(essence.tls_enable, false) with an ABSENT key does not throw
// "no such key" (as a plain function would under eager arg eval), but returns
// the fallback. This is the whole point of the macro mechanism (compile-time
// rewrite to has(x)?x:y before eval).
func TestDefault_AbsentSelect(t *testing.T) {
	e := newEngine(t)

	out, err := e.EvalExpression(`default(essence.tls_enable, false)`, Vars{Essence: map[string]any{}})
	if err != nil {
		t.Fatalf("eval (отсутствующий ключ НЕ должен бросать — жадность обойдена macro): %v", err)
	}
	if got := out.Value(); got != false {
		t.Fatalf("default(absent) = %v, want false (fallback)", got)
	}
}

// TestDefault_NestedAbsentFinalKey — nested select default(a.b.c, fb): when the
// FINAL key c is absent (parents a.b present) → fb. Full parity with direct
// has(a.b.c)?a.b.c:fb (CEL has() checks presence of the final field).
func TestDefault_NestedAbsentFinalKey(t *testing.T) {
	e := newEngine(t)

	out, err := e.EvalExpression(`default(essence.a.b.c, "fb")`, Vars{Essence: map[string]any{
		"a": map[string]any{"b": map[string]any{}},
	}})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if got := out.Value(); got != "fb" {
		t.Fatalf("default(nested absent final) = %v, want \"fb\"", got)
	}

	// Final key present → its value.
	out, err = e.EvalExpression(`default(essence.a.b.c, "fb")`, Vars{Essence: map[string]any{
		"a": map[string]any{"b": map[string]any{"c": "real"}},
	}})
	if err != nil {
		t.Fatalf("eval (present): %v", err)
	}
	if got := out.Value(); got != "real" {
		t.Fatalf("default(nested present) = %v, want \"real\"", got)
	}
}

// TestDefault_IntWrap — int(default(essence.tls_port, 7379)): the int() wrapper
// around default() works (use-site cluster.yml: tls_port). Absent → default
// 7379; present → the value.
func TestDefault_IntWrap(t *testing.T) {
	e := newEngine(t)

	out, err := e.EvalExpression(`int(default(essence.tls_port, 7379))`, Vars{Essence: map[string]any{}})
	if err != nil {
		t.Fatalf("eval (absent): %v", err)
	}
	if got := out.Value(); got != int64(7379) {
		t.Fatalf("int(default(absent)) = %v, want 7379", got)
	}

	out, err = e.EvalExpression(`int(default(essence.tls_port, 7379))`, Vars{Essence: map[string]any{
		"tls_port": int64(6380),
	}})
	if err != nil {
		t.Fatalf("eval (present): %v", err)
	}
	if got := out.Value(); got != int64(6380) {
		t.Fatalf("int(default(present)) = %v, want 6380", got)
	}
}

// TestDefault_EmptyMapFallback — default(input.redis_settings, {}) → absence
// yields an empty map (use-site cluster.yml: redis_settings).
func TestDefault_EmptyMapFallback(t *testing.T) {
	e := newEngine(t)

	out, err := e.EvalInterpolation(`${ default(input.redis_settings, {}) }`, Vars{Input: map[string]any{}})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	got, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("результат типа %T, want map[string]any", out)
	}
	if len(got) != 0 {
		t.Fatalf("default(absent, {}) = %v, want пустой map", got)
	}

	// A present map passes through.
	out, err = e.EvalInterpolation(`${ default(input.redis_settings, {}) }`, Vars{Input: map[string]any{
		"redis_settings": map[string]any{"maxmemory": "256mb"},
	}})
	if err != nil {
		t.Fatalf("eval (present): %v", err)
	}
	want := map[string]any{"maxmemory": "256mb"}
	if got := out.(map[string]any); !reflect.DeepEqual(got, want) {
		t.Fatalf("default(present, {}) = %v, want %v", got, want)
	}
}

// TestDefault_IdentRoot — bare root identifier: default(input, {}). The root is
// always present in the activation ([Vars.activation] binds it as an empty map);
// has(ident) does not compile in CEL → the macro expands to x itself. The
// fallback is unreachable; the root value is returned.
func TestDefault_IdentRoot(t *testing.T) {
	e := newEngine(t)

	out, err := e.EvalInterpolation(`${ default(input, {}) }`, Vars{Input: map[string]any{"k": "v"}})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	want := map[string]any{"k": "v"}
	if got := out.(map[string]any); !reflect.DeepEqual(got, want) {
		t.Fatalf("default(ident root) = %v, want %v (сам корень, fallback недостижим)", got, want)
	}
}

// TestDefault_NonSelectArgCompileError — negative: a non-select/non-ident first
// argument → compile error (default over an expression has no
// value-or-default semantics). Covers size()/arithmetic/index-access.
func TestDefault_NonSelectArgCompileError(t *testing.T) {
	e := newEngine(t)

	for _, expr := range []string{
		`default(size(input.x), 0)`,     // function call
		`default(input.a + input.b, 0)`, // arithmetic
		`default(input['k'], 0)`,        // index access (CallKind _[_], not select)
	} {
		_, err := e.EvalExpression(expr, Vars{Input: map[string]any{"x": []any{int64(1)}}})
		if err == nil {
			t.Fatalf("%s: ожидалась compile-ошибка (не-select arg0), получено nil", expr)
		}
		var ce *ErrCompile
		if !errors.As(err, &ce) {
			t.Fatalf("%s: ошибка = %v, want *ErrCompile", expr, err)
		}
	}
}

// TestDefault_AvailableInFlowControl — default() is available in the Soul-side
// flow-control sandbox ([ADR-012(d)]): a pure macro, same env as merge()/glob().
func TestDefault_AvailableInFlowControl(t *testing.T) {
	e := newFlowControlEngine(t)

	// Absent register key via default → fallback (eagerness bypassed in the
	// flow-control env too).
	out, err := e.EvalExpression(`default(register.maybe, "fb") == "fb"`, Vars{Register: map[string]any{}})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if got := out.Value(); got != true {
		t.Fatalf("default в flow-control = %v, want true", got)
	}
}

// TestDefault_UndeclaredInMigration — negative: migration-CEL ([ADR-019]) is
// hermetic — default() is NOT registered (see buildEngine). A call →
// compile error, symmetric to the merge()/glob() guard of the migration env
// (minimal surface area; extending it requires a separate ADR).
func TestDefault_UndeclaredInMigration(t *testing.T) {
	e := newMigrationEngine(t)

	_, err := e.EvalExpression(`default(state.x, 0)`, Vars{State: map[string]any{}})
	if err == nil {
		t.Fatal("default() в migration-env: ожидалась compile-ошибка (не зарегистрирована), получено nil")
	}
	var ce *ErrCompile
	if !errors.As(err, &ce) {
		t.Fatalf("default() в migration-env: ошибка = %v, want *ErrCompile", err)
	}
}

// TestDefault_SecretMaskedSameAsDirectVault — ★ masking guard (like merge's
// TestMerge_SecretMaskedSameAsDirectVault): a secret injected via
// default(essence.x, vault('…#password')) under a sensitively-named key is
// masked by the output layer (shared/audit.MaskSecrets) IDENTICALLY to a direct
// ${ vault(...) }. default() is sugar over has()?:, and does not rename the
// destination key, so it neither widens nor narrows the masking boundary. Proves
// both sides: the fallback branch (essence.x absent → vault) and identity with
// direct vault; non-secret values pass without over-masking.
func TestDefault_SecretMaskedSameAsDirectVault(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{
		"secret/redis/admin": {"password": "s3cr3t-plaintext"},
	}}
	e := newVaultEngine(t, kv)

	// Baseline: direct ${ vault(...) } under the `password` key.
	direct, err := e.EvalInterpolation("${ vault('secret/redis/admin#password') }", Vars{})
	if err != nil {
		t.Fatalf("eval direct vault: %v", err)
	}
	if direct != "s3cr3t-plaintext" {
		t.Fatalf("direct vault резолвнул %v, want plaintext (keeper-side)", direct)
	}
	maskedDirect := audit.MaskSecrets(map[string]any{"password": direct})
	if maskedDirect["password"] == "s3cr3t-plaintext" {
		t.Fatal("эталон: прямой vault-секрет НЕ замаскирован — слой маскинга сломан")
	}

	// Same secret via default(essence.admin_password, vault(...)): the essence
	// key is absent → the vault branch is taken, result under the `password` key.
	resolved, err := e.EvalInterpolation(
		"${ default(essence.admin_password, vault('secret/redis/admin#password')) }",
		Vars{Essence: map[string]any{}},
	)
	if err != nil {
		t.Fatalf("eval default+vault: %v", err)
	}
	if resolved != "s3cr3t-plaintext" {
		t.Fatalf("default(absent, vault) = %v, want plaintext (vault-ветвь резолвится keeper-side)", resolved)
	}
	maskedResolved := audit.MaskSecrets(map[string]any{"password": resolved})
	// Key assertion: masked IDENTICALLY to direct vault.
	if maskedResolved["password"] != maskedDirect["password"] {
		t.Fatalf("default-секрет замаскирован как %v, прямой как %v — РАСХОЖДЕНИЕ (секрет течёт через default)",
			maskedResolved["password"], maskedDirect["password"])
	}
	if maskedResolved["password"] == "s3cr3t-plaintext" {
		t.Fatal("default-секрет НЕ замаскирован — секрет протекает в выходной слой через default()")
	}

	// A non-secret value via default is NOT over-masked.
	nonSecret, err := e.EvalInterpolation(`${ default(essence.maxmemory, "256mb") }`, Vars{Essence: map[string]any{}})
	if err != nil {
		t.Fatalf("eval non-secret default: %v", err)
	}
	maskedNonSecret := audit.MaskSecrets(map[string]any{"maxmemory": nonSecret})
	if maskedNonSecret["maxmemory"] != "256mb" {
		t.Fatalf("несекретное значение замаскировано: %v (over-masking)", maskedNonSecret["maxmemory"])
	}
}
