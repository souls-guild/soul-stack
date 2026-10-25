package middleware

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
)

func authTestLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeLoginGuard — a controllable [LoginGuard] for middleware tests. Simulates
// throttle (Allow), lockout (Locked) and the failure counter (RecordFailure) in-memory.
type fakeLoginGuard struct {
	mu sync.Mutex

	// throttle: the number of remaining allowances per principal (scope:principal).
	// Missing key → allowDefault is used.
	allowance    map[string]int
	allowDefault int

	// lockout: locked principals (scope:principal → retryAfter).
	locked map[string]time.Duration

	// errors for fail-closed/fail-open checks.
	allowErr  error
	lockedErr error

	// failure counter: scope:principal → the RecordFailure count.
	failures      map[string]int
	lockThreshold int // if >0 and failures>=threshold → set locked
}

func newFakeGuard() *fakeLoginGuard {
	return &fakeLoginGuard{
		allowance: map[string]int{}, allowDefault: 1000,
		locked: map[string]time.Duration{}, failures: map[string]int{},
	}
}

func key(scope, principal string) string { return scope + ":" + principal }

func (g *fakeLoginGuard) Allow(_ context.Context, scope, principal string, _ float64, _ int) (bool, time.Duration, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.allowErr != nil {
		return false, 0, g.allowErr
	}
	k := key(scope, principal)
	n, ok := g.allowance[k]
	if !ok {
		n = g.allowDefault
	}
	if n <= 0 {
		return false, 2 * time.Second, nil
	}
	g.allowance[k] = n - 1
	return true, 0, nil
}

func (g *fakeLoginGuard) Locked(_ context.Context, scope, principal string) (bool, time.Duration, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.lockedErr != nil {
		return false, 0, g.lockedErr
	}
	if d, ok := g.locked[key(scope, principal)]; ok {
		return true, d, nil
	}
	return false, 0, nil
}

func (g *fakeLoginGuard) RecordFailure(_ context.Context, scope, principal string, threshold int, _, lockout time.Duration) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	k := key(scope, principal)
	g.failures[k]++
	if g.failures[k] >= threshold {
		g.locked[k] = lockout
		g.failures[k] = 0
		return true, nil
	}
	return false, nil
}

func authTestCfg() AuthLoginLimitConfig {
	return AuthLoginLimitConfig{
		Rate: 1, Burst: 5,
		LockoutThreshold: 3,
		LockoutWindow:    15 * time.Minute,
		LockoutBackoff:   15 * time.Minute,
	}
}

// failingLoginHandler — a handler that always returns 401 (simulates bad creds).
func failingLoginHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
}

func okLoginHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
}

func ldapReq(username string) *http.Request {
	body := `{"username":"` + username + `","password":"x"}`
	r := httptest.NewRequest(http.MethodPost, "/auth/ldap/login", strings.NewReader(body))
	r.RemoteAddr = "203.0.113.7:55555"
	return r
}

// TestAuthLoginLimit_HIGH3_LockoutAfterNFailures — N+1 failed logins within the
// window → the principal is locked (429); a legit login AFTER the lock is lifted — OK.
// The core of HIGH-3 anti-bruteforce.
func TestAuthLoginLimit_HIGH3_LockoutAfterNFailures(t *testing.T) {
	guard := newFakeGuard()
	cfg := authTestCfg() // threshold=3
	mw := AuthLoginLimit(guard, cfg, LDAPUsernameExtractor, true, authTestLogger())

	failing := mw(failingLoginHandler())

	// 3 failures (threshold=3) → on the 3rd, RecordFailure sets lockout.
	for i := 0; i < cfg.LockoutThreshold; i++ {
		rec := httptest.NewRecorder()
		failing.ServeHTTP(rec, ldapReq("alice"))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: expected 401 (handler reached), got %d", i, rec.Code)
		}
	}

	// The next attempt — the principal is locked (by IP AND by username): 429.
	rec := httptest.NewRecorder()
	failing.ServeHTTP(rec, ldapReq("alice"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after threshold failures, expected 429 lockout, got %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Errorf("429 must carry Retry-After")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "problem+json") {
		t.Errorf("429 must be application/problem+json, got %q", ct)
	}

	// Simulate the lock expiring → a legit login (OK handler) passes.
	guard.mu.Lock()
	delete(guard.locked, key(authScopeIP, "203.0.113.7"))
	delete(guard.locked, key(authScopeUser, "alice"))
	guard.mu.Unlock()

	okMW := AuthLoginLimit(guard, cfg, LDAPUsernameExtractor, true, authTestLogger())
	rec = httptest.NewRecorder()
	okMW(okLoginHandler()).ServeHTTP(rec, ldapReq("alice"))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("after lockout window, legit login must pass (204), got %d", rec.Code)
	}
}

// TestAuthLoginLimit_HIGH3_ThrottleExhausted — exhausting the token-bucket rate
// throttle → 429 BEFORE the handler (next not called).
func TestAuthLoginLimit_HIGH3_ThrottleExhausted(t *testing.T) {
	guard := newFakeGuard()
	guard.allowDefault = 0 // the very first Allow → not allowed
	cfg := authTestCfg()

	reached := false
	mw := AuthLoginLimit(guard, cfg, LDAPUsernameExtractor, true, authTestLogger())
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNoContent)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, ldapReq("bob"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("throttle-exhausted expected 429, got %d", rec.Code)
	}
	if reached {
		t.Errorf("handler must NOT be reached when throttled")
	}
}

// TestAuthLoginLimit_HIGH3_LockoutFailClosed — a Redis error on the Locked check →
// fail-CLOSED (429): the login perimeter does not open to bruteforce when Redis is unavailable.
func TestAuthLoginLimit_HIGH3_LockoutFailClosed(t *testing.T) {
	guard := newFakeGuard()
	guard.lockedErr = context.DeadlineExceeded // Redis unavailable
	cfg := authTestCfg()

	reached := false
	mw := AuthLoginLimit(guard, cfg, LDAPUsernameExtractor, true, authTestLogger())
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNoContent)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, ldapReq("carol"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("lockout check error must fail-CLOSED (429), got %d", rec.Code)
	}
	if reached {
		t.Errorf("handler must NOT be reached on fail-closed lockout")
	}
}

// TestAuthLoginLimit_NilGuardPassthrough — guard=nil (no Redis) → passthrough
// (login without throttle, OPTIONAL tier).
func TestAuthLoginLimit_NilGuardPassthrough(t *testing.T) {
	reached := false
	mw := AuthLoginLimit(nil, authTestCfg(), LDAPUsernameExtractor, true, authTestLogger())
	mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(httptest.NewRecorder(), ldapReq("dave"))
	if !reached {
		t.Errorf("nil guard must passthrough to handler")
	}
}

// TestAuthLoginLimit_SuccessDoesNotCountFailure — a successful login (204) does NOT
// increment the failure counter (provision/role-change — not bruteforce).
func TestAuthLoginLimit_SuccessDoesNotCountFailure(t *testing.T) {
	guard := newFakeGuard()
	cfg := authTestCfg()
	mw := AuthLoginLimit(guard, cfg, LDAPUsernameExtractor, true, authTestLogger())
	for i := 0; i < 10; i++ {
		rec := httptest.NewRecorder()
		mw(okLoginHandler()).ServeHTTP(rec, ldapReq("erin"))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("iter %d: success must stay 204, got %d", i, rec.Code)
		}
	}
	guard.mu.Lock()
	defer guard.mu.Unlock()
	if guard.failures[key(authScopeUser, "erin")] != 0 {
		t.Errorf("successful logins must not increment failure counter")
	}
}

// TestAuthLoginLimit_OIDCLoginNoUsernameNoFailure — OIDC login (302, no
// username, extractUsername=nil): only per-IP throttle, a 302 success does not count
// as a failure (isAuthFailure(302)=false).
func TestAuthLoginLimit_OIDCLoginRedirectNotFailure(t *testing.T) {
	guard := newFakeGuard()
	cfg := authTestCfg()
	mw := AuthLoginLimit(guard, cfg, nil, true, authTestLogger())
	redirect := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://idp/authorize")
		w.WriteHeader(http.StatusFound)
	})
	r := httptest.NewRequest(http.MethodGet, "/auth/oidc/login", nil)
	r.RemoteAddr = "198.51.100.4:40000"
	rec := httptest.NewRecorder()
	mw(redirect).ServeHTTP(rec, r)
	if rec.Code != http.StatusFound {
		t.Fatalf("oidc login redirect expected 302, got %d", rec.Code)
	}
	guard.mu.Lock()
	defer guard.mu.Unlock()
	if guard.failures[key(authScopeIP, "198.51.100.4")] != 0 {
		t.Errorf("302 redirect must not count as login failure")
	}
}

// TestLDAPUsernameExtractor_RestoresBody — the extractor reads the username and RETURNS
// the whole body to the handler (the handler re-reads the password).
func TestLDAPUsernameExtractor_RestoresBody(t *testing.T) {
	r := ldapReq("frank")
	got := LDAPUsernameExtractor(r)
	if got != "frank" {
		t.Fatalf("extracted username = %q, want frank", got)
	}
	body, _ := io.ReadAll(r.Body)
	if !strings.Contains(string(body), `"password":"x"`) {
		t.Errorf("body must be restored for handler, got %q", body)
	}
}

// TestWriteAuth429_AntiOracle — the 429 detail does not reveal the scope/reason (anti-oracle).
func TestWriteAuth429_AntiOracle(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/auth/ldap/login", nil)
	writeAuth429(rec, r, 5*time.Second)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(strings.ToLower(body), "username") || strings.Contains(strings.ToLower(body), "ip") {
		t.Errorf("429 detail must not reveal scope (anti-oracle), got %q", body)
	}
	if !strings.Contains(body, problem.TypeAuthThrottled) {
		t.Errorf("429 must carry TypeAuthThrottled, got %q", body)
	}
}

// The local login's per-AID key is the AID the handler looks up (NIM-1051):
// "Alice" and " alice" must land on one lockout counter, and the body must
// still reach the handler.
func TestLocalAIDExtractor_NormalizesAndRestoresBody(t *testing.T) {
	extract := LocalAIDExtractor(func(s string) string { return strings.ToLower(strings.TrimSpace(s)) })
	for _, raw := range []string{"alice", "Alice", "  ALICE "} {
		r := httptest.NewRequest(http.MethodPost, "/auth/local/login",
			strings.NewReader(`{"aid":"`+raw+`","password":"x"}`))
		if got := extract(r); got != "alice" {
			t.Errorf("extract(%q) = %q, want alice", raw, got)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"password":"x"`) {
			t.Errorf("body not restored for the handler: %q", body)
		}
	}
	if got := extract(httptest.NewRequest(http.MethodPost, "/auth/local/login", strings.NewReader(`not json`))); got != "" {
		t.Errorf("extract(non-JSON) = %q, want empty (per-IP still applies)", got)
	}
}

func TestClaimsSubjectExtractor(t *testing.T) {
	r := httptest.NewRequest(http.MethodPut, "/v1/me/password", nil)
	if got := ClaimsSubjectExtractor(r); got != "" {
		t.Fatalf("no claims: %q, want empty", got)
	}
	r = r.WithContext(InjectClaimsForTest(r.Context(), &jwt.Claims{Subject: "alice"}))
	if got := ClaimsSubjectExtractor(r); got != "alice" {
		t.Fatalf("claims of alice: %q, want alice", got)
	}
}

// Every extractor-keyed route refuses a body past what the key is read from,
// before the guard and the handler (NIM-1051): padding must not take a guess
// past the per-username layer.
func TestAuthLoginLimit_RefusesAnOversizedBody(t *testing.T) {
	for name, extract := range map[string]func(*http.Request) string{
		"ldap":  LDAPUsernameExtractor,
		"local": LocalAIDExtractor(strings.ToLower),
		"me":    ClaimsSubjectExtractor,
	} {
		reached := false
		h := AuthLoginLimit(newFakeGuard(), AuthLoginLimitConfig{Rate: 100, Burst: 100, LockoutThreshold: 5, LockoutWindow: time.Minute, LockoutBackoff: time.Minute},
			extract, true, slog.Default())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			reached = true
			w.WriteHeader(http.StatusNoContent)
		}))
		body := `{"username":"alice","aid":"alice","password":"x"}` + strings.Repeat(" ", maxLoginBodySnoop)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(body)))
		if rec.Code != http.StatusRequestEntityTooLarge || reached {
			t.Errorf("%s: status %d, handler reached %v; want 413 before the handler", name, rec.Code, reached)
		}
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{"username":"alice","aid":"alice","password":"x"}`)))
		if rec.Code != http.StatusNoContent {
			t.Errorf("%s: a normal body got %d, want it through", name, rec.Code)
		}
	}
}
