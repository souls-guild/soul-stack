package cel

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// stubKV is a hermetic KVReader for vault() tests. Returns preset secrets by
// relative path; missing → error (like Vault without the secret).
type stubKV struct {
	secrets map[string]map[string]any
	calls   []string // history of requested paths (verifies path resolution)
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

// vault('secret/x') without #field → the whole map; field access via CEL `.field`.
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

// vault('secret/x#field') → a single field directly (#-suffix).
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
		t.Fatalf("ReadKV path = %q, want secret/redis/admin (without #field)", kv.calls[0])
	}
}

// Keeper-side resolve: the real value in the result, not a ref string.
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
		t.Fatalf("result = %v, want the real secret value (not a ref)", out)
	}
}

// vault() path from trusted context (vars/incarnation), not string concatenation.
func TestVault_PathFromContext(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{
		"secret/svc-redis/admin": {"password": "fromctx"},
	}}
	e := newVaultEngine(t, kv)

	vars := Vars{
		Incarnation: map[string]any{"service": "redis"},
		Loop:        map[string]any{}, // no loop
	}
	out, err := e.EvalInterpolation("${ vault('secret/svc-' + incarnation.service + '/admin').password }", vars)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if out != "fromctx" {
		t.Fatalf("vault() from context = %v, want fromctx", out)
	}
	if kv.calls[0] != "secret/svc-redis/admin" {
		t.Fatalf("ReadKV path = %q, want secret/svc-redis/admin (path resolved by CEL before ReadKV)", kv.calls[0])
	}
}

// Missing secret → render error (ErrEval), not a panic.
func TestVault_MissingSecret(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{}}
	e := newVaultEngine(t, kv)

	_, err := e.EvalInterpolation("${ vault('secret/nope').password }", Vars{})
	if err == nil {
		t.Fatal("expected an error for a missing secret, got nil")
	}
	var ee *ErrEval
	if !errors.As(err, &ee) {
		t.Fatalf("expected *ErrEval, got %T: %v", err, err)
	}
}

// Missing #field in an existing secret → render error.
func TestVault_MissingField(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{
		"secret/redis/admin": {"password": "x"},
	}}
	e := newVaultEngine(t, kv)

	_, err := e.EvalInterpolation("${ vault('secret/redis/admin#nope') }", Vars{})
	if err == nil {
		t.Fatal("expected an error for a missing field, got nil")
	}
}

// vault() compiles (guard lifted) when the Engine has a KVReader.
func TestVault_GuardLiftedWithReader(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{"secret/x": {"v": "1"}}}
	e := newVaultEngine(t, kv)

	_, err := e.EvalExpression("vault('secret/x').v == '1'", Vars{})
	if err != nil {
		t.Fatalf("vault() should compile with a KVReader, got: %v", err)
	}
}

// Without a KVReader vault() stays ErrUnsupported (guard kept).
func TestVault_GuardKeptWithoutReader(t *testing.T) {
	e := newEngine(t) // no WithVault
	_, err := e.EvalExpression("vault('secret/x').v == '1'", Vars{})
	var ue *ErrUnsupported
	if !errors.As(err, &ue) {
		t.Fatalf("without a KVReader expected *ErrUnsupported, got %T: %v", err, err)
	}
}

// ctx from Vars is propagated to ReadKV (cancel/timeout).
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
		t.Fatal("ctx from Vars.Ctx did not reach ReadKV")
	}
}

type captureCtxKV struct {
	onRead func(context.Context)
}

func (c captureCtxKV) ReadKV(ctx context.Context, _ string) (map[string]any, error) {
	c.onRead(ctx)
	return map[string]any{"v": "ok"}, nil
}

// ── security-blocker: bypassing macro vault() via direct __vault_read ────────

// A direct __vault_read(path, __vault_resolver) call in an author expression would
// read ANY path, bypassing macro vault()/guard/mask. The guard on the `__`
// identifier must reject such an expression BEFORE compile (ErrUnsupported) and NOT
// hit kv.
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
			t.Fatalf("%q: expected *ErrUnsupported (bypassing the vault() macro), got %T: %v", expr, err, err)
		}
	}
	if len(kv.calls) != 0 {
		t.Fatalf("kv must not be invoked when __vault_read is rejected, calls=%v", kv.calls)
	}
}

// A bare reference to the resolver var __vault_resolver is also rejected.
func TestVault_DirectResolverVarRejected(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{"secret/x": {"v": "1"}}}
	e := newVaultEngine(t, kv)

	_, err := e.EvalExpression("__vault_resolver != null", Vars{})
	var ue *ErrUnsupported
	if !errors.As(err, &ue) {
		t.Fatalf("expected *ErrUnsupported for bare __vault_resolver, got %T: %v", err, err)
	}
}

// The `__` guard applies even WITHOUT a KVReader (vault function not registered):
// the `__` identifier is reserved regardless of a vault client being present.
func TestVault_InternalIdentRejectedWithoutReader(t *testing.T) {
	e := newEngine(t) // no WithVault
	_, err := e.EvalExpression("__vault_read('secret/x', __vault_resolver).v == '1'", Vars{})
	var ue *ErrUnsupported
	if !errors.As(err, &ue) {
		t.Fatalf("without a KVReader expected *ErrUnsupported for __vault_read, got %T: %v", err, err)
	}
}

// The guard must not false-positive on a `__` substring INSIDE a string literal
// (that is data, not an identifier) nor on a legitimate vault().
func TestVault_InternalGuardNoFalsePositive(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{"secret/db__primary/x": {"dsn": "v"}}}
	e := newVaultEngine(t, kv)

	// `__` INSIDE a path literal is data, not an identifier; vault() is legit and
	// the guard must not reject it.
	out, err := e.EvalInterpolation("${ vault('secret/db__primary/x#dsn') }", Vars{})
	if err != nil {
		t.Fatalf("false-positive guard trigger on `__` in a path literal: %v", err)
	}
	if out != "v" {
		t.Fatalf("unexpected result: %v", out)
	}
}

// ── vault-not-found — actionable path, survives masking (NIM-73) ─────────────

// The missing-secret error text carries the path in FLAT form (secret/…, without
// the vault: prefix) and survives production masking of status_details/error_summary:
// the operator sees WHAT to provision, not `***MASKED***`.
func TestVault_MissingSecretErrorActionable(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{}}
	e := newVaultEngine(t, kv)

	_, err := e.EvalInterpolation("${ vault('secret/redis/admin').password }", Vars{})
	if err == nil {
		t.Fatal("expected an error for a missing secret")
	}
	assertVaultPathActionable(t, err.Error(), "secret/redis/admin")
}

// The #field form (like redis add_user: users/<name>#password): the text names
// both the path and the required field; the flat form survives masking.
func TestVault_MissingSecretWithFieldActionable(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{}}
	e := newVaultEngine(t, kv)

	_, err := e.EvalInterpolation("${ vault('secret/redis/nosql/users/alice#password') }", Vars{})
	if err == nil {
		t.Fatal("expected an error for a missing user secret")
	}
	assertVaultPathActionable(t, err.Error(), "secret/redis/nosql/users/alice")
	if !strings.Contains(err.Error(), "password") {
		t.Fatalf("error text does not name the required field password: %q", err.Error())
	}
}

// Missing #field in an existing secret: path + field name in flat form, survives
// masking; other fields' values do not leak (see …NoSecretLeak).
func TestVault_MissingFieldErrorActionable(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{
		"secret/redis/admin": {"password": "x"},
	}}
	e := newVaultEngine(t, kv)

	_, err := e.EvalInterpolation("${ vault('secret/redis/admin#nope') }", Vars{})
	if err == nil {
		t.Fatal("expected an error for a missing field")
	}
	assertVaultPathActionable(t, err.Error(), "secret/redis/admin")
	if !strings.Contains(err.Error(), "nope") {
		t.Fatalf("error text does not name the missing field nope: %q", err.Error())
	}
}

// assertVaultPathActionable: the vault() error text (a) carries the path in FLAT
// form, (b) does NOT carry a vault: ref marker (else masking would eat the whole
// string), (c) survives production masking (audit.MaskSecretsSealed — the same as
// status_details/error_summary in lockIncarnation, with a non-empty seal set
// without an error key): the result is NOT `***MASKED***` and still contains the path.
func assertVaultPathActionable(t *testing.T, errText, path string) {
	t.Helper()
	if !strings.Contains(errText, path) {
		t.Fatalf("error text does not carry the path %q: %q", path, errText)
	}
	if strings.Contains(errText, "vault:"+path) {
		t.Fatalf("error text carries the vault:-ref form (masking would eat it whole): %q", errText)
	}
	masked := audit.MaskSecretsSealed(
		map[string]any{"error": errText},
		audit.SealOpts{Sealed: map[string]bool{"config.password": true}},
	)
	got, _ := masked["error"].(string)
	if got == "***MASKED***" {
		t.Fatalf("actionable error was masked entirely: %q", got)
	}
	if !strings.Contains(got, path) {
		t.Fatalf("path %q disappeared after masking: %q", path, got)
	}
}

// A plaintext secret does not reach the error text (missing-field does not print
// other fields' values of the secret).
func TestVault_MissingFieldNoSecretLeak(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{
		"secret/redis/admin": {"password": "TOP-SECRET-VALUE"},
	}}
	e := newVaultEngine(t, kv)

	_, err := e.EvalInterpolation("${ vault('secret/redis/admin#nope') }", Vars{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "TOP-SECRET-VALUE") {
		t.Fatalf("plaintext secret leaked into the error text: %q", err.Error())
	}
}

// ── path-format: vault('foo') without a slash → a clear format error ─────────

func TestVault_PathFormatValidation(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{}}
	e := newVaultEngine(t, kv)

	bad := []string{"foo", "secret/", "/", "#field"}
	for _, p := range bad {
		_, err := e.EvalInterpolation("${ vault('"+p+"') }", Vars{})
		if err == nil {
			t.Fatalf("vault('%s'): expected a path-format error", p)
		}
	}
	// kv must not be hit on an invalid format (error before ReadKV).
	if len(kv.calls) != 0 {
		t.Fatalf("ReadKV was invoked with an invalid path format: %v", kv.calls)
	}
}

// ── concurrency: shared Engine + parallel vault() with different ctx ─────────

// Shared Engine, parallel vault() runs with different secrets and different ctx.
// Pins the claimed concurrency-safety (per-eval resolver in the activation, kv
// immutable). Run under `go test -race`.
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
				t.Errorf("vault('%s') = %q, want %q (per-eval context mixed up)", path, out, want)
			}
		}(c.path, c.want)
	}
	wg.Wait()
}

// concurrentKV is a thread-safe KVReader for the concurrency test (read-only map,
// no writes to a shared calls slice).
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

// ── per-render-pass memo: dedup of vault() resolves within one pass ──────────

// countingKV is a KVReader with a per-path backend-call counter (dedup check).
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

// Repeated vault(same-path) within ONE render pass = exactly 1 backend call.
func TestVaultMemo_SamePathOneBackendCall(t *testing.T) {
	kv := newCountingKV(map[string]map[string]any{
		"secret/redis/admin": {"password": "s3cr3t"},
	})
	e := newVaultEngine(t, kv)

	ctx := WithVaultMemo(context.Background())
	const expr = "${ vault('secret/redis/admin#password') }"
	for i := 0; i < 16; i++ { // redis scale: dozens of identical vault()
		out, err := e.EvalInterpolation(expr, Vars{Ctx: ctx})
		if err != nil {
			t.Fatalf("eval #%d: %v", i, err)
		}
		if out != "s3cr3t" {
			t.Fatalf("eval #%d = %v, want s3cr3t", i, out)
		}
	}
	if got := kv.calls("secret/redis/admin"); got != 1 {
		t.Fatalf("backend calls to ReadKV = %d, want 1 (memo dedup within one pass)", got)
	}
}

// Different #field of one secret hit a single backend call (ReadKV sees the body
// without #field), but each field resolves correctly from the cached map.
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
		t.Fatalf("different #field values mixed up: password=%v tls=%v", pwd, tls)
	}
	if got := kv.calls("secret/redis/inc"); got != 1 {
		t.Fatalf("backend calls = %d, want 1 (one ReadKV per secret, fields from cache)", got)
	}
}

// Different render passes do NOT share the cache: each pass with its own memo-ctx →
// a separate backend call. Pins per-pass scope (no cross-request leak/stale).
func TestVaultMemo_SeparatePassesDoNotShareCache(t *testing.T) {
	kv := newCountingKV(map[string]map[string]any{
		"secret/redis/admin": {"password": "s3cr3t"},
	})
	e := newVaultEngine(t, kv)

	const expr = "${ vault('secret/redis/admin#password') }"
	for pass := 0; pass < 3; pass++ {
		ctx := WithVaultMemo(context.Background()) // new pass = new memo
		if _, err := e.EvalInterpolation(expr, Vars{Ctx: ctx}); err != nil {
			t.Fatalf("pass #%d: %v", pass, err)
		}
		if _, err := e.EvalInterpolation(expr, Vars{Ctx: ctx}); err != nil {
			t.Fatalf("pass #%d (repeat): %v", pass, err)
		}
	}
	// 3 passes × dedup within a pass = 3 backend calls (not 1, not 6).
	if got := kv.calls("secret/redis/admin"); got != 3 {
		t.Fatalf("backend calls = %d, want 3 (one per pass, cache not shared)", got)
	}
}

// Without WithVaultMemo (ctx without a cache — soul-lint/Trial/direct unit-eval) the
// behavior is unchanged: every vault() hits the backend. Memo is an optimization,
// not a contract.
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
		t.Fatalf("backend calls = %d, want 4 (without memo -- every call hits Vault)", got)
	}
}

// A ReadKV error is NOT cached: a retry of the same path within a pass repeats the
// read (a transient Vault failure does not "stick" for the whole run).
func TestVaultMemo_ErrorsNotCached(t *testing.T) {
	kv := newCountingKV(map[string]map[string]any{}) // no secret → error
	e := newVaultEngine(t, kv)

	ctx := WithVaultMemo(context.Background())
	const expr = "${ vault('secret/redis/admin#password') }"
	for i := 0; i < 3; i++ {
		if _, err := e.EvalInterpolation(expr, Vars{Ctx: ctx}); err == nil {
			t.Fatalf("eval #%d: expected a missing-secret error", i)
		}
	}
	if got := kv.calls("secret/redis/admin"); got != 3 {
		t.Fatalf("backend calls = %d, want 3 (errors are not cached)", got)
	}
}
