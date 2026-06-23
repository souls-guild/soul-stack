package cel

import (
	"errors"
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// TestDefault_PresentSelect — default(x, fb) при присутствующем select-поле →
// само значение x (fallback недостижим).
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

// TestDefault_AbsentSelect — ★ КЛЮЧЕВОЙ тест: жадность CEL обойдена.
// default(essence.tls_enable, false) при ОТСУТСТВУЮЩЕМ ключе НЕ бросает
// «no such key» (как упала бы обычная функция при eager-eval аргумента), а
// возвращает fallback. Это и есть смысл macro-механизма (compile-time rewrite в
// has(x)?x:y до eval).
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

// TestDefault_NestedAbsentFinalKey — вложенный select default(a.b.c, fb): при
// отсутствии ФИНАЛЬНОГО ключа c (родители a.b присутствуют) → fb. Полная parity
// с прямым has(a.b.c)?a.b.c:fb (CEL has() проверяет присутствие финального поля).
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

	// Финальный ключ присутствует → его значение.
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

// TestDefault_IntWrap — int(default(essence.tls_port, 7379)): обёртка int()
// снаружи default() работает (use-site cluster.yml: tls_port). Отсутствие →
// дефолт 7379; присутствие → значение.
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

// TestDefault_EmptyMapFallback — default(input.redis_settings, {}) → отсутствие
// даёт пустой map (use-site cluster.yml: redis_settings).
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

	// Присутствующий map проходит насквозь.
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

// TestDefault_IdentRoot — голый идентификатор-корень: default(input, {}). Корень
// всегда присутствует в активации ([Vars.activation] биндит его как пустой map),
// has(ident) в CEL не компилируется → макрос разворачивает в сам x. Fallback
// недостижим, возвращается значение корня.
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

// TestDefault_NonSelectArgCompileError — negative: не-select/не-ident первый
// аргумент → compile-ошибка (default над выражением не имеет семантики
// «значения-или-дефолта»). Покрывает size()/арифметику/индекс-доступ.
func TestDefault_NonSelectArgCompileError(t *testing.T) {
	e := newEngine(t)

	for _, expr := range []string{
		`default(size(input.x), 0)`,     // вызов функции
		`default(input.a + input.b, 0)`, // арифметика
		`default(input['k'], 0)`,        // индекс-доступ (CallKind _[_], не select)
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

// TestDefault_AvailableInFlowControl — default() доступна в Soul-side
// flow-control sandbox ([ADR-012(d)]): pure macro, тот же env, что merge()/glob().
func TestDefault_AvailableInFlowControl(t *testing.T) {
	e := newFlowControlEngine(t)

	// Отсутствующий register-ключ через default → fallback (жадность обойдена и
	// в flow-control env).
	out, err := e.EvalExpression(`default(register.maybe, "fb") == "fb"`, Vars{Register: map[string]any{}})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if got := out.Value(); got != true {
		t.Fatalf("default в flow-control = %v, want true", got)
	}
}

// TestDefault_UndeclaredInMigration — negative: migration-CEL ([ADR-019])
// hermetic — default() НЕ зарегистрирована (см. buildEngine). Вызов →
// compile-ошибка, симметрично merge()/glob()-guard-у миграционного env (минимум
// surface area, расширение требует отдельного ADR).
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

// TestDefault_SecretMaskedSameAsDirectVault — ★ masking-guard (как merge
// TestMerge_SecretMaskedSameAsDirectVault): секрет, подставленный через
// default(essence.x, vault('…#password')) под sensitive-именованным ключом,
// маскируется выходным слоем (shared/audit.MaskSecrets) ИДЕНТИЧНО прямому
// ${ vault(...) }. default() — сахар над has()?:, ключ назначения не
// переименовывает, поэтому границу маскинга не расширяет/не сужает. Доказывает
// обе стороны: fallback-ветвь (essence.x отсутствует → vault) и идентичность
// прямому vault; несекретные значения проходят без over-masking.
func TestDefault_SecretMaskedSameAsDirectVault(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{
		"secret/redis/admin": {"password": "s3cr3t-plaintext"},
	}}
	e := newVaultEngine(t, kv)

	// Эталон: прямой ${ vault(...) } под ключом `password`.
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

	// Тот же секрет через default(essence.admin_password, vault(...)): essence-
	// ключ отсутствует → берётся vault-ветвь, результат под ключом `password`.
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
	// Главное утверждение: замаскирован ИДЕНТИЧНО прямому vault.
	if maskedResolved["password"] != maskedDirect["password"] {
		t.Fatalf("default-секрет замаскирован как %v, прямой как %v — РАСХОЖДЕНИЕ (секрет течёт через default)",
			maskedResolved["password"], maskedDirect["password"])
	}
	if maskedResolved["password"] == "s3cr3t-plaintext" {
		t.Fatal("default-секрет НЕ замаскирован — секрет протекает в выходной слой через default()")
	}

	// Несекретное значение через default НЕ over-маскируется.
	nonSecret, err := e.EvalInterpolation(`${ default(essence.maxmemory, "256mb") }`, Vars{Essence: map[string]any{}})
	if err != nil {
		t.Fatalf("eval non-secret default: %v", err)
	}
	maskedNonSecret := audit.MaskSecrets(map[string]any{"maxmemory": nonSecret})
	if maskedNonSecret["maxmemory"] != "256mb" {
		t.Fatalf("несекретное значение замаскировано: %v (over-masking)", maskedNonSecret["maxmemory"])
	}
}
