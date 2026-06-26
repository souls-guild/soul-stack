package cel

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// stubKV — герметичный KVReader для тестов vault(). Возвращает заранее заданные
// секреты по relative-пути; missing → ошибка (как Vault без секрета).
type stubKV struct {
	secrets map[string]map[string]any
	calls   []string // история запрошенных путей (проверка резолва пути)
}

func (s *stubKV) ReadKV(_ context.Context, path string) (map[string]any, error) {
	s.calls = append(s.calls, path)
	data, ok := s.secrets[path]
	if !ok {
		return nil, errors.New("vault: KV path not found: " + path)
	}
	return data, nil
}

func newVaultEngine(t *testing.T, kv KVReader) *Engine {
	t.Helper()
	e, err := New(WithVault(kv))
	if err != nil {
		t.Fatalf("New(WithVault): %v", err)
	}
	return e
}

// vault('secret/x') без #field → весь map; доступ к полю через CEL `.field`.
func TestVault_MapThenField(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{
		"secret/redis/admin": {"password": "s3cr3t", "user": "admin"},
	}}
	e := newVaultEngine(t, kv)

	out, err := e.EvalInterpolation("${ vault('secret/redis/admin').password }", Vars{})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if out != "s3cr3t" {
		t.Fatalf("vault().password = %v, want s3cr3t", out)
	}
	if len(kv.calls) != 1 || kv.calls[0] != "secret/redis/admin" {
		t.Fatalf("ReadKV calls = %v, want [secret/redis/admin]", kv.calls)
	}
}

// vault('secret/x#field') → одно поле напрямую (#-суффикс).
func TestVault_HashField(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{
		"secret/redis/admin": {"password": "s3cr3t"},
	}}
	e := newVaultEngine(t, kv)

	out, err := e.EvalInterpolation("${ vault('secret/redis/admin#password') }", Vars{})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if out != "s3cr3t" {
		t.Fatalf("vault(#field) = %v, want s3cr3t", out)
	}
	if kv.calls[0] != "secret/redis/admin" {
		t.Fatalf("ReadKV path = %q, want secret/redis/admin (без #field)", kv.calls[0])
	}
}

// Резолв keeper-side: реальное значение в результате, не ref-строка.
func TestVault_ResolvesRealValue(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{
		"secret/db": {"dsn": "postgres://real-secret-value"},
	}}
	e := newVaultEngine(t, kv)

	out, err := e.EvalInterpolation("${ vault('secret/db#dsn') }", Vars{})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if out != "postgres://real-secret-value" {
		t.Fatalf("результат = %v, want реальное значение секрета (не ref)", out)
	}
}

// Путь vault() из доверенного контекста (vars/incarnation), не строковая склейка.
func TestVault_PathFromContext(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{
		"secret/svc-redis/admin": {"password": "fromctx"},
	}}
	e := newVaultEngine(t, kv)

	vars := Vars{
		Incarnation: map[string]any{"service": "redis"},
		Loop:        map[string]any{}, // нет loop
	}
	out, err := e.EvalInterpolation("${ vault('secret/svc-' + incarnation.service + '/admin').password }", vars)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if out != "fromctx" {
		t.Fatalf("vault() из контекста = %v, want fromctx", out)
	}
	if kv.calls[0] != "secret/svc-redis/admin" {
		t.Fatalf("ReadKV path = %q, want secret/svc-redis/admin (путь резолвлен CEL до ReadKV)", kv.calls[0])
	}
}

// Missing secret → render-ошибка (ErrEval), не паника.
func TestVault_MissingSecret(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{}}
	e := newVaultEngine(t, kv)

	_, err := e.EvalInterpolation("${ vault('secret/nope').password }", Vars{})
	if err == nil {
		t.Fatal("ожидали ошибку для отсутствующего секрета, получили nil")
	}
	var ee *ErrEval
	if !errors.As(err, &ee) {
		t.Fatalf("ожидали *ErrEval, получили %T: %v", err, err)
	}
}

// Missing #field в существующем секрете → render-ошибка.
func TestVault_MissingField(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{
		"secret/redis/admin": {"password": "x"},
	}}
	e := newVaultEngine(t, kv)

	_, err := e.EvalInterpolation("${ vault('secret/redis/admin#nope') }", Vars{})
	if err == nil {
		t.Fatal("ожидали ошибку для отсутствующего поля, получили nil")
	}
}

// vault() компилится (guard снят) при Engine с KVReader.
func TestVault_GuardLiftedWithReader(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{"secret/x": {"v": "1"}}}
	e := newVaultEngine(t, kv)

	_, err := e.EvalExpression("vault('secret/x').v == '1'", Vars{})
	if err != nil {
		t.Fatalf("vault() должен компилиться с KVReader, получили: %v", err)
	}
}

// Без KVReader vault() остаётся ErrUnsupported (guard сохранён).
func TestVault_GuardKeptWithoutReader(t *testing.T) {
	e := newEngine(t) // без WithVault
	_, err := e.EvalExpression("vault('secret/x').v == '1'", Vars{})
	var ue *ErrUnsupported
	if !errors.As(err, &ue) {
		t.Fatalf("без KVReader ожидали *ErrUnsupported, получили %T: %v", err, err)
	}
}

// ctx из Vars прокидывается в ReadKV (отмена/таймаут).
func TestVault_CtxPropagation(t *testing.T) {
	type ctxKey struct{}
	captured := make(chan context.Context, 1)
	kv := captureCtxKV{onRead: func(ctx context.Context) { captured <- ctx }}
	e := newVaultEngine(t, kv)

	ctx := context.WithValue(context.Background(), ctxKey{}, "marker")
	_, err := e.EvalInterpolation("${ vault('secret/x#v') }", Vars{Ctx: ctx})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	got := <-captured
	if got.Value(ctxKey{}) != "marker" {
		t.Fatal("ctx из Vars.Ctx не дошёл до ReadKV")
	}
}

type captureCtxKV struct {
	onRead func(context.Context)
}

func (c captureCtxKV) ReadKV(ctx context.Context, _ string) (map[string]any, error) {
	c.onRead(ctx)
	return map[string]any{"v": "ok"}, nil
}

// ── security-blocker: обход macro vault() через прямой __vault_read ──────────

// Прямой вызов __vault_read(path, __vault_resolver) в авторском выражении читал
// бы ЛЮБОЙ путь, минуя macro vault()/guard/mask. guard на `__`-идентификатор
// должен отвергать такое выражение ДО compile (ErrUnsupported), и НЕ дёргать kv.
func TestVault_DirectInternalReadRejected(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{
		"secret/redis/admin": {"password": "s3cr3t"},
	}}
	e := newVaultEngine(t, kv)

	exprs := []string{
		"${ __vault_read('secret/redis/admin', __vault_resolver).password }",
		"${ __vault_read('secret/redis/admin#password', __vault_resolver) }",
		"__vault_read('secret/x', __vault_resolver).password == 's'",
	}
	for _, expr := range exprs {
		_, err := e.EvalInterpolation(expr, Vars{})
		if err == nil {
			_, err = e.EvalExpression(expr, Vars{})
		}
		var ue *ErrUnsupported
		if !errors.As(err, &ue) {
			t.Fatalf("%q: ожидали *ErrUnsupported (обход macro vault()), получили %T: %v", expr, err, err)
		}
	}
	if len(kv.calls) != 0 {
		t.Fatalf("kv не должен дёргаться при отвергнутом __vault_read, calls=%v", kv.calls)
	}
}

// Голая ссылка на resolver-переменную __vault_resolver тоже отвергается.
func TestVault_DirectResolverVarRejected(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{"secret/x": {"v": "1"}}}
	e := newVaultEngine(t, kv)

	_, err := e.EvalExpression("__vault_resolver != null", Vars{})
	var ue *ErrUnsupported
	if !errors.As(err, &ue) {
		t.Fatalf("ожидали *ErrUnsupported для голого __vault_resolver, получили %T: %v", err, err)
	}
}

// guard на `__` действует и БЕЗ KVReader (vault-функция не зарегистрирована):
// `__`-идентификатор зарезервирован независимо от наличия vault-клиента.
func TestVault_InternalIdentRejectedWithoutReader(t *testing.T) {
	e := newEngine(t) // без WithVault
	_, err := e.EvalExpression("__vault_read('secret/x', __vault_resolver).v == '1'", Vars{})
	var ue *ErrUnsupported
	if !errors.As(err, &ue) {
		t.Fatalf("без KVReader ожидали *ErrUnsupported для __vault_read, получили %T: %v", err, err)
	}
}

// guard не должен ложно-срабатывать на `__`-подстроку ВНУТРИ строкового литерала
// (это данные, не идентификатор) и на легальный vault().
func TestVault_InternalGuardNoFalsePositive(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{"secret/db__primary/x": {"dsn": "v"}}}
	e := newVaultEngine(t, kv)

	// `__` ВНУТРИ литерала-пути — данные, не идентификатор; vault() легален и
	// guard не должен это отвергать.
	out, err := e.EvalInterpolation("${ vault('secret/db__primary/x#dsn') }", Vars{})
	if err != nil {
		t.Fatalf("ложное срабатывание guard на `__` в литерале-пути: %v", err)
	}
	if out != "v" {
		t.Fatalf("неожиданный результат: %v", out)
	}
}

// ── path-leak: missing-secret/missing-field — путь в ref-форме, маскируется ──

// Текст ошибки missing-secret должен нести путь в ref-форме vault:secret/...,
// чтобы audit.MaskSecrets замаскировал всю строку (status_details/error_summary).
func TestVault_MissingSecretErrorMasked(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{}}
	e := newVaultEngine(t, kv)

	_, err := e.EvalInterpolation("${ vault('secret/redis/admin').password }", Vars{})
	if err == nil {
		t.Fatal("ожидали ошибку для отсутствующего секрета")
	}
	assertVaultPathMasked(t, err.Error(), "secret/redis/admin")
}

// Текст ошибки missing-field тоже несёт путь в ref-форме (имя поля — не секрет,
// но путь — наводка на секрет-локацию).
func TestVault_MissingFieldErrorMasked(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{
		"secret/redis/admin": {"password": "x"},
	}}
	e := newVaultEngine(t, kv)

	_, err := e.EvalInterpolation("${ vault('secret/redis/admin#nope') }", Vars{})
	if err == nil {
		t.Fatal("ожидали ошибку для отсутствующего поля")
	}
	assertVaultPathMasked(t, err.Error(), "secret/redis/admin")
}

// assertVaultPathMasked проверяет, что (а) текст ошибки несёт путь в ref-форме
// vault:<path>, и (б) при прогоне через audit.MaskSecrets вся строка целиком
// заменяется на ***MASKED*** — путь не утекает в наблюдаемые каналы.
func assertVaultPathMasked(t *testing.T, errText, path string) {
	t.Helper()
	if !strings.Contains(errText, "vault:"+path) {
		t.Fatalf("текст ошибки не несёт путь в ref-форме vault:%s: %q", path, errText)
	}
	masked := audit.MaskSecrets(map[string]any{"error_summary": errText})
	got, _ := masked["error_summary"].(string)
	if got != "***MASKED***" {
		t.Fatalf("audit.MaskSecrets не замаскировал ошибку с vault-путём: %q", got)
	}
	if strings.Contains(got, path) {
		t.Fatalf("путь %q утёк после маскинга: %q", path, got)
	}
}

// Plaintext-секрет не попадает в текст ошибки (missing-field не печатает
// значения других полей секрета).
func TestVault_MissingFieldNoSecretLeak(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{
		"secret/redis/admin": {"password": "TOP-SECRET-VALUE"},
	}}
	e := newVaultEngine(t, kv)

	_, err := e.EvalInterpolation("${ vault('secret/redis/admin#nope') }", Vars{})
	if err == nil {
		t.Fatal("ожидали ошибку")
	}
	if strings.Contains(err.Error(), "TOP-SECRET-VALUE") {
		t.Fatalf("plaintext-секрет утёк в текст ошибки: %q", err.Error())
	}
}

// ── path-format: vault('foo') без слеша → понятная ошибка формата ───────────

func TestVault_PathFormatValidation(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{}}
	e := newVaultEngine(t, kv)

	bad := []string{"foo", "secret/", "/", "#field"}
	for _, p := range bad {
		_, err := e.EvalInterpolation("${ vault('"+p+"') }", Vars{})
		if err == nil {
			t.Fatalf("vault('%s'): ожидали ошибку формата пути", p)
		}
	}
	// kv не должен дёргаться при невалидном формате (ошибка до ReadKV).
	if len(kv.calls) != 0 {
		t.Fatalf("ReadKV дёрнулся при невалидном формате пути: %v", kv.calls)
	}
}

// ── concurrency: общий Engine + параллельные vault() с разным ctx ───────────

// Общий Engine, параллельные vault()-прогоны с разными секретами и разными ctx.
// Закрепляет заявленную concurrency-safety (per-eval resolver в активации, kv
// immutable). Гонять под `go test -race`.
func TestVault_ConcurrentEvals(t *testing.T) {
	kv := &concurrentKV{secrets: map[string]map[string]any{
		"secret/a": {"v": "AAA"},
		"secret/b": {"v": "BBB"},
		"secret/c": {"v": "CCC"},
	}}
	e := newVaultEngine(t, kv)

	cases := []struct{ path, want string }{
		{"secret/a", "AAA"}, {"secret/b", "BBB"}, {"secret/c", "CCC"},
	}

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		c := cases[i%len(cases)]
		wg.Add(1)
		go func(path, want string) {
			defer wg.Done()
			out, err := e.EvalInterpolation("${ vault('"+path+"#v') }", Vars{Ctx: context.Background()})
			if err != nil {
				t.Errorf("vault('%s'): %v", path, err)
				return
			}
			if out != want {
				t.Errorf("vault('%s') = %q, want %q (перепутан per-eval контекст)", path, out, want)
			}
		}(c.path, c.want)
	}
	wg.Wait()
}

// concurrentKV — потокобезопасный KVReader для concurrency-теста (read-only
// карта, без записи в общий slice calls).
type concurrentKV struct {
	secrets map[string]map[string]any
}

func (c *concurrentKV) ReadKV(_ context.Context, path string) (map[string]any, error) {
	data, ok := c.secrets[path]
	if !ok {
		return nil, errors.New("vault: KV path not found: vault:" + path)
	}
	return data, nil
}

// ── per-render-pass memo: дедуп vault()-резолвов в одном проходе ─────────────

// countingKV — KVReader со счётчиком backend-вызовов по пути (проверка дедупа).
type countingKV struct {
	secrets map[string]map[string]any
	mu      sync.Mutex
	count   map[string]int
}

func newCountingKV(secrets map[string]map[string]any) *countingKV {
	return &countingKV{secrets: secrets, count: map[string]int{}}
}

func (c *countingKV) ReadKV(_ context.Context, path string) (map[string]any, error) {
	c.mu.Lock()
	c.count[path]++
	c.mu.Unlock()
	data, ok := c.secrets[path]
	if !ok {
		return nil, errors.New("vault: KV path not found: " + path)
	}
	return data, nil
}

func (c *countingKV) calls(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count[path]
}

// Повторный vault(тот-же-путь) в ОДНОМ render-pass = ровно 1 backend-вызов.
func TestVaultMemo_SamePathOneBackendCall(t *testing.T) {
	kv := newCountingKV(map[string]map[string]any{
		"secret/redis/admin": {"password": "s3cr3t"},
	})
	e := newVaultEngine(t, kv)

	ctx := WithVaultMemo(context.Background())
	const expr = "${ vault('secret/redis/admin#password') }"
	for i := 0; i < 16; i++ { // redis-масштаб: десятки одинаковых vault()
		out, err := e.EvalInterpolation(expr, Vars{Ctx: ctx})
		if err != nil {
			t.Fatalf("eval #%d: %v", i, err)
		}
		if out != "s3cr3t" {
			t.Fatalf("eval #%d = %v, want s3cr3t", i, out)
		}
	}
	if got := kv.calls("secret/redis/admin"); got != 1 {
		t.Fatalf("backend-вызовов ReadKV = %d, want 1 (memo дедуп в одном pass)", got)
	}
}

// Разные #field одного секрета бьют один backend-вызов (ReadKV видит body без
// #field), но каждое поле резолвится корректно из кешированного map.
func TestVaultMemo_DifferentFieldsSameSecretOneCall(t *testing.T) {
	kv := newCountingKV(map[string]map[string]any{
		"secret/redis/inc": {"password": "PWD", "tls": "TLS-CERT"},
	})
	e := newVaultEngine(t, kv)

	ctx := WithVaultMemo(context.Background())
	pwd, err := e.EvalInterpolation("${ vault('secret/redis/inc#password') }", Vars{Ctx: ctx})
	if err != nil {
		t.Fatalf("eval #password: %v", err)
	}
	tls, err := e.EvalInterpolation("${ vault('secret/redis/inc#tls') }", Vars{Ctx: ctx})
	if err != nil {
		t.Fatalf("eval #tls: %v", err)
	}
	if pwd != "PWD" || tls != "TLS-CERT" {
		t.Fatalf("разные #field перепутаны: password=%v tls=%v", pwd, tls)
	}
	if got := kv.calls("secret/redis/inc"); got != 1 {
		t.Fatalf("backend-вызовов = %d, want 1 (один ReadKV на секрет, поля из кеша)", got)
	}
}

// Разные render-pass-ы НЕ делят кеш: каждый pass со своим memo-ctx → отдельный
// backend-вызов. Закрепляет per-pass scope (нет межзапросной утечки/stale).
func TestVaultMemo_SeparatePassesDoNotShareCache(t *testing.T) {
	kv := newCountingKV(map[string]map[string]any{
		"secret/redis/admin": {"password": "s3cr3t"},
	})
	e := newVaultEngine(t, kv)

	const expr = "${ vault('secret/redis/admin#password') }"
	for pass := 0; pass < 3; pass++ {
		ctx := WithVaultMemo(context.Background()) // новый pass = новый memo
		if _, err := e.EvalInterpolation(expr, Vars{Ctx: ctx}); err != nil {
			t.Fatalf("pass #%d: %v", pass, err)
		}
		if _, err := e.EvalInterpolation(expr, Vars{Ctx: ctx}); err != nil {
			t.Fatalf("pass #%d (повтор): %v", pass, err)
		}
	}
	// 3 pass-а × дедуп внутри pass-а = 3 backend-вызова (не 1, не 6).
	if got := kv.calls("secret/redis/admin"); got != 3 {
		t.Fatalf("backend-вызовов = %d, want 3 (по одному на pass, кеш не общий)", got)
	}
}

// Без WithVaultMemo (ctx без кеша — soul-lint/Trial/прямой unit-eval) поведение
// прежнее: каждый vault() бьёт backend. Memo — оптимизация, не контракт.
func TestVaultMemo_NoMemoEveryCallHitsBackend(t *testing.T) {
	kv := newCountingKV(map[string]map[string]any{
		"secret/redis/admin": {"password": "s3cr3t"},
	})
	e := newVaultEngine(t, kv)

	const expr = "${ vault('secret/redis/admin#password') }"
	for i := 0; i < 4; i++ {
		if _, err := e.EvalInterpolation(expr, Vars{Ctx: context.Background()}); err != nil {
			t.Fatalf("eval #%d: %v", i, err)
		}
	}
	if got := kv.calls("secret/redis/admin"); got != 4 {
		t.Fatalf("backend-вызовов = %d, want 4 (без memo — каждый вызов бьёт Vault)", got)
	}
}

// Ошибка ReadKV НЕ кешируется: retry того же пути в pass-е повторяет чтение
// (транзиентный сбой Vault не «залипает» на весь прогон).
func TestVaultMemo_ErrorsNotCached(t *testing.T) {
	kv := newCountingKV(map[string]map[string]any{}) // секрета нет → ошибка
	e := newVaultEngine(t, kv)

	ctx := WithVaultMemo(context.Background())
	const expr = "${ vault('secret/redis/admin#password') }"
	for i := 0; i < 3; i++ {
		if _, err := e.EvalInterpolation(expr, Vars{Ctx: ctx}); err == nil {
			t.Fatalf("eval #%d: ожидали ошибку missing-secret", i)
		}
	}
	if got := kv.calls("secret/redis/admin"); got != 3 {
		t.Fatalf("backend-вызовов = %d, want 3 (ошибки не кешируются)", got)
	}
}
