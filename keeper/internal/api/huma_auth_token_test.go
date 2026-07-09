package api

// Guard-тесты обмена session-cookie→Bearer (NIM-77, POST /auth/token) и
// публичного GET /auth/methods. Ключевой инвариант периметра: cookie НЕ даёт
// доступа к /v1 (RequireJWT читает только Authorization) — регресс здесь ловит
// возврат чтения cookie на /v1.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

// exchangeSigningKey — фикс 32-байтовый HS256-ключ (RFC 7518 минимум) для
// verifier+issuer теста (один ключ = round-trip).
var exchangeSigningKey = []byte("nim77-exchange-signing-key-32byte")

const exchangeIssuer = "keeper.test"

// exchangeCrypto собирает shared verifier+issuer на одном ключе (session-cookie
// и выданный Bearer верифицируются одним verifier — суть Варианта B).
func exchangeCrypto(t *testing.T) (*jwt.Verifier, *jwt.Issuer) {
	t.Helper()
	v, err := jwt.NewVerifier(exchangeSigningKey, exchangeIssuer)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	iss, err := jwt.NewIssuer(exchangeSigningKey, exchangeIssuer)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	return v, iss
}

// fakeRevoked — revocationChecker для теста revoked-ветки.
type fakeRevoked map[string]bool

func (f fakeRevoked) IsRevoked(aid string) bool { return f[aid] }

// authTokenRouter собирает РЕАЛЬНЫЙ Operator API-роутер (buildRouter) со stub-
// хендлерами (как collectRoutes), но с боевыми verifier/enforcer/authToken —
// чтобы проверить и /auth/token, и периметр /v1 в одном дереве.
func authTokenRouter(t *testing.T, verifier *jwt.Verifier, enforcer RBACProvider, authToken *AuthTokenDeps, authMethods AuthMethodsDeps) http.Handler {
	t.Helper()
	return buildRouter(
		verifier,
		nil, // healthH
		stubOperatorHandler(t),
		handlers.NewIncarnationHandler(nil, nil, nil, nil, nil, nil, nil, nil, nil),
		handlers.NewSoulHandler(nil, nil, nil, nil),
		stubRoleHandler(t),
		stubSynodHandler(t),
		stubSigilHandler(t),
		stubSigilKeyHandler(t),
		stubServiceHandler(t),
		stubProvisioningPolicyHandler(t),
		stubAugurHandler(t),
		stubOracleHandler(t),
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, // pushH..heraldH (opt-in, nil)
		handlers.NewModuleCatalogHandler(nil, nil),
		handlers.NewModuleFormPrepHandler(nil, nil),
		handlers.NewPermissionCatalogHandler(nil),
		handlers.NewEventTypeCatalogHandler(nil),
		handlers.NewHeraldTypeCatalogHandler(nil),
		handlers.NewMyPermissionsHandler(nil, nil),
		enforcer,
		nil, nil, nil, nil, nil, nil, nil, // auditWriter..tempoVoyagePreviewLimits
		false,    // webUIEnabled
		nil, nil, // ldapAuth, oidcAuth
		authToken,   // authToken
		authMethods, // authMethods
		nil,         // loginGuard
		apimiddleware.AuthLoginLimitConfig{},
		nil,              // soulStatsStaleFn
		nil,              // clusterH
		&runEventsDeps{}, // runEventsDeps
		nil,              // logger
	)
}

// emptyEnforcer — боевой default-deny enforcer (пустой снимок): любой /v1
// RequireAction/RequirePermission → 403 (не 401). Достаточно для «Bearer принят
// на уровне JWT» (403≠401).
func emptyEnforcer(t *testing.T) *rbac.Enforcer {
	t.Helper()
	e, err := rbac.NewEnforcerFromSnapshot(nil)
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	return e
}

// postExchange шлёт POST /auth/token с cookie soul_session=session (пустой session
// → cookie не ставится) и опц. заголовками.
func postExchange(h http.Handler, session string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/auth/token", http.NoBody)
	if session != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// decodeReply парсит тело успешного обмена.
func decodeReply(t *testing.T, rec *httptest.ResponseRecorder) AuthTokenReply {
	t.Helper()
	var r AuthTokenReply
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode reply: %v; body=%s", err, rec.Body.String())
	}
	return r
}

// --- 1. Инвариант периметра: cookie НЕ даёт доступа к /v1 ---

// TestAuthPerimeter_V1CookieOnly_401 — запрос на /v1 с ТОЛЬКО валидной session-
// cookie и БЕЗ Authorization → 401. Cookie не читается на /v1 (RequireJWT).
func TestAuthPerimeter_V1CookieOnly_401(t *testing.T) {
	verifier, issuer := exchangeCrypto(t)
	session, err := issuer.Issue("archon-alice", []string{"cluster-admin"}, time.Hour, false)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	td := &AuthTokenDeps{Verifier: verifier, Issuer: issuer, TTL: 10 * time.Minute}
	h := authTokenRouter(t, verifier, emptyEnforcer(t), td, AuthMethodsDeps{})

	req := httptest.NewRequest(http.MethodGet, "/v1/souls", http.NoBody)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session}) // ТОЛЬКО cookie, без Authorization
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /v1/souls с cookie-only = %d, want 401 (cookie НЕ даёт доступа к /v1); body=%s",
			rec.Code, rec.Body.String())
	}
}

// --- 2. Round-trip: обмен → Verify + принятие на /v1 ---

// TestAuthTokenExchange_ValidCookie_RoundTrip — валидная cookie → 200, token
// непустой, expires_at≈now+ttl; выданный token принимается Verify И проходит
// RequireJWT на /v1 (403≠401 от RBAC = «Bearer принят»).
func TestAuthTokenExchange_ValidCookie_RoundTrip(t *testing.T) {
	verifier, issuer := exchangeCrypto(t)
	session, _ := issuer.Issue("archon-alice", []string{"cluster-admin"}, time.Hour, false)
	td := &AuthTokenDeps{Verifier: verifier, Issuer: issuer, TTL: 10 * time.Minute}
	h := authTokenRouter(t, verifier, emptyEnforcer(t), td, AuthMethodsDeps{})

	rec := postExchange(h, session, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /auth/token валидная cookie = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	reply := decodeReply(t, rec)
	if reply.Token == "" {
		t.Fatal("token пуст")
	}
	if d := time.Until(reply.ExpiresAt); d < 9*time.Minute || d > 11*time.Minute {
		t.Errorf("expires_at через %s, want ≈10m", d)
	}

	// Round-trip: выданный token верифицируется тем же verifier.
	claims, err := verifier.Verify(reply.Token)
	if err != nil {
		t.Fatalf("выданный Bearer не проходит Verify: %v", err)
	}
	if claims.Subject != "archon-alice" {
		t.Errorf("sub выданного токена = %q, want archon-alice", claims.Subject)
	}
	// F2: expires_at ТОЧНО совпадает с exp внутри токена (выведен self-verify, не
	// вторым time.Now().Add(ttl)) — клиент планирует re-exchange по верному дедлайну.
	if !reply.ExpiresAt.Equal(claims.ExpiresAt) {
		t.Errorf("expires_at=%s != exp токена=%s (должны совпадать точно)", reply.ExpiresAt, claims.ExpiresAt)
	}

	// И принимается на реальном /v1-роуте как Bearer (403 от RBAC ≠ 401).
	req := httptest.NewRequest(http.MethodGet, "/v1/souls", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+reply.Token)
	vrec := httptest.NewRecorder()
	h.ServeHTTP(vrec, req)
	if vrec.Code == http.StatusUnauthorized {
		t.Errorf("Bearer из обмена → 401 на /v1 (должен пройти RequireJWT); body=%s", vrec.Body.String())
	}
}

// --- 3. Плохие/отсутствующие cookie → 401 ---

func TestAuthTokenExchange_BadCookies_401(t *testing.T) {
	verifier, issuer := exchangeCrypto(t)
	td := &AuthTokenDeps{Verifier: verifier, Issuer: issuer, TTL: 10 * time.Minute}
	h := authTokenRouter(t, verifier, emptyEnforcer(t), td, AuthMethodsDeps{})

	// Протухшая cookie отбивается на ШАГЕ 3 (Verify → ErrExpiredToken), а НЕ на
	// ttl≤0-guard шага 5 (тот defensive/недостижим без leeway в Verify).
	expired, _ := issuer.Issue("archon-alice", nil, time.Millisecond, false)
	time.Sleep(5 * time.Millisecond)

	cases := []struct {
		name    string
		session string
	}{
		{"нет cookie", ""},
		{"битая cookie", "not-a-jwt"},
		{"чужая подпись", strings.Repeat("a", 20) + ".b.c"},
		{"протухшая cookie (Verify-expired, шаг 3)", expired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postExchange(h, tc.session, nil)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s: status = %d, want 401; body=%s", tc.name, rec.Code, rec.Body.String())
			}
			// raw jwt-library-текст не утекает (anti-oracle).
			if strings.Contains(rec.Body.String(), "golang-jwt") || strings.Contains(rec.Body.String(), "token contains") {
				t.Errorf("%s: тело содержит raw-причину: %s", tc.name, rec.Body.String())
			}
		})
	}
}

// --- 4. Субъект ТОЛЬКО из claims cookie ---

// TestAuthTokenExchange_SubjectFromClaimsOnly — попытка навязать иной aid через
// body/query/header НЕ меняет sub выданного токена (нет privilege-escalation).
func TestAuthTokenExchange_SubjectFromClaimsOnly(t *testing.T) {
	verifier, issuer := exchangeCrypto(t)
	session, _ := issuer.Issue("archon-real", []string{"read-only"}, time.Hour, false)
	td := &AuthTokenDeps{Verifier: verifier, Issuer: issuer, TTL: 10 * time.Minute}
	h := authTokenRouter(t, verifier, emptyEnforcer(t), td, AuthMethodsDeps{})

	req := httptest.NewRequest(http.MethodPost, "/auth/token?aid=archon-attacker&sub=archon-attacker",
		strings.NewReader(`{"aid":"archon-attacker","sub":"archon-attacker","roles":["cluster-admin"]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Aid", "archon-attacker")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	claims, err := verifier.Verify(decodeReply(t, rec).Token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Subject != "archon-real" {
		t.Errorf("sub = %q, want archon-real (не должен браться из body/query/header)", claims.Subject)
	}
	if len(claims.Roles) != 1 || claims.Roles[0] != "read-only" {
		t.Errorf("roles = %v, want [read-only] (из claims cookie, не из body)", claims.Roles)
	}
}

// --- 4b. bootstrap_initial переживает обмен ---

// TestAuthTokenExchange_BootstrapInitialRoundTrip — флаг bootstrap_initial из
// cookie переносится в выданный токен как есть (true→true, false→false): он держит
// инвариант «нельзя удалить последнего bootstrap-оператора» (ADR-013), сброс/
// подъём при обмене сломал бы его.
func TestAuthTokenExchange_BootstrapInitialRoundTrip(t *testing.T) {
	verifier, issuer := exchangeCrypto(t)
	td := &AuthTokenDeps{Verifier: verifier, Issuer: issuer, TTL: 10 * time.Minute}
	h := authTokenRouter(t, verifier, emptyEnforcer(t), td, AuthMethodsDeps{})

	for _, bootstrap := range []bool{true, false} {
		session, _ := issuer.Issue("archon-root", []string{"cluster-admin"}, time.Hour, bootstrap)
		rec := postExchange(h, session, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("bootstrap=%v: status = %d, want 200; body=%s", bootstrap, rec.Code, rec.Body.String())
		}
		claims, err := verifier.Verify(decodeReply(t, rec).Token)
		if err != nil {
			t.Fatalf("bootstrap=%v: Verify: %v", bootstrap, err)
		}
		if claims.BootstrapInitial != bootstrap {
			t.Errorf("bootstrap_initial выданного токена = %v, want %v (флаг из claims cookie)",
				claims.BootstrapInitial, bootstrap)
		}
	}
}

// --- 5. Revoked ---

// TestAuthTokenExchange_Revoked_401 — cookie ревокнутого AID → 401, issuer НЕ
// вызван (токен не выдан).
func TestAuthTokenExchange_Revoked_401(t *testing.T) {
	verifier, issuer := exchangeCrypto(t)
	session, _ := issuer.Issue("archon-fired", []string{"cluster-admin"}, time.Hour, false)
	spy := &loginStubIssuer{token: "must-not-be-issued"}
	td := &AuthTokenDeps{
		Verifier: verifier,
		Issuer:   spy,
		TTL:      10 * time.Minute,
		Revoked:  fakeRevoked{"archon-fired": true},
	}
	h := authTokenRouter(t, verifier, emptyEnforcer(t), td, AuthMethodsDeps{})

	rec := postExchange(h, session, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked AID: status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	if spy.gotAID != "" {
		t.Errorf("revoked → issuer вызван с %q (токен не должен выдаваться)", spy.gotAID)
	}
}

// --- 6. Sec-Fetch allowlist (режем ТОЛЬКО cross-site) ---

// TestAuthTokenExchange_SecFetchAllowlist — Sec-Fetch-Site defense-in-depth
// пиннит, что отсекается РОВНО cross-site (403); same-origin/same-site/none и
// отсутствие заголовка — пропуск (200 при валидной cookie). Origin намеренно НЕ
// валидируем: защиту несут SameSite=Strict + Path=/auth + HttpOnly на самой cookie.
func TestAuthTokenExchange_SecFetchAllowlist(t *testing.T) {
	verifier, issuer := exchangeCrypto(t)
	session, _ := issuer.Issue("archon-alice", nil, time.Hour, false)
	td := &AuthTokenDeps{Verifier: verifier, Issuer: issuer, TTL: 10 * time.Minute}
	h := authTokenRouter(t, verifier, emptyEnforcer(t), td, AuthMethodsDeps{})

	cases := []struct {
		name       string
		secFetch   string // "" → заголовок не выставляется
		setHeader  bool
		wantStatus int
	}{
		{"cross-site → 403", "cross-site", true, http.StatusForbidden},
		{"same-origin → 200", "same-origin", true, http.StatusOK},
		{"same-site → 200", "same-site", true, http.StatusOK},
		{"none → 200", "none", true, http.StatusOK},
		{"нет заголовка → 200", "", false, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hdr map[string]string
			if tc.setHeader {
				hdr = map[string]string{"Sec-Fetch-Site": tc.secFetch}
			}
			rec := postExchange(h, session, hdr)
			if rec.Code != tc.wantStatus {
				t.Fatalf("Sec-Fetch-Site=%q: status = %d, want %d; body=%s",
					tc.secFetch, rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

// --- 7. TTL-cap на cookie.exp ---

// TestAuthTokenExchange_TTLCap — cookie с remaining < exchange_ttl → exp
// выданного токена ≈ cookie.exp (капнут, не 10m).
func TestAuthTokenExchange_TTLCap(t *testing.T) {
	verifier, issuer := exchangeCrypto(t)
	// Cookie живёт 30s; exchange_ttl = 10m → выданный exp ≈ 30s.
	session, _ := issuer.Issue("archon-alice", nil, 30*time.Second, false)
	td := &AuthTokenDeps{Verifier: verifier, Issuer: issuer, TTL: 10 * time.Minute}
	h := authTokenRouter(t, verifier, emptyEnforcer(t), td, AuthMethodsDeps{})

	rec := postExchange(h, session, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if d := time.Until(decodeReply(t, rec).ExpiresAt); d > time.Minute {
		t.Errorf("expires_at через %s — не капнут на cookie.exp (~30s), want <1m", d)
	}
	// remaining<=0 (протухшая cookie) → 401 покрыт TestAuthTokenExchange_BadCookies_401.
}

// --- 8. GET /auth/methods (публичный) ---

// TestAuthMethods_PublicBooleans — /auth/methods без Authorization → 200 с
// booleans по deps.
func TestAuthMethods_PublicBooleans(t *testing.T) {
	h := authTokenRouter(t, nil, emptyEnforcer(t), nil, AuthMethodsDeps{Password: true, LDAP: true, OIDC: false})

	req := httptest.NewRequest(http.MethodGet, "/auth/methods", http.NoBody) // без Authorization
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /auth/methods = %d, want 200 (публичный); body=%s", rec.Code, rec.Body.String())
	}
	var r AuthMethodsReply
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if !r.Password || !r.LDAP || r.OIDC {
		t.Errorf("methods = %+v, want {password:true, ldap:true, oidc:false}", r)
	}
}

// TestAuthMethods_PasswordAlwaysTrue — F1 (контракт Q4/ADR-058): password=true при
// ЛЮБЫХ ldap/oidc (вставка operator-JWT в localStorage доступна всегда, не привязана
// к endpoint). ldap/oidc отражают deps как есть.
func TestAuthMethods_PasswordAlwaysTrue(t *testing.T) {
	combos := []struct{ ldap, oidc bool }{
		{false, false}, {true, false}, {false, true}, {true, true},
	}
	for _, c := range combos {
		h := authTokenRouter(t, nil, emptyEnforcer(t), nil,
			AuthMethodsDeps{Password: true, LDAP: c.ldap, OIDC: c.oidc})
		req := httptest.NewRequest(http.MethodGet, "/auth/methods", http.NoBody)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("ldap=%v oidc=%v: status = %d, want 200", c.ldap, c.oidc, rec.Code)
		}
		var r AuthMethodsReply
		if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
			t.Fatalf("ldap=%v oidc=%v: decode: %v", c.ldap, c.oidc, err)
		}
		if !r.Password {
			t.Errorf("ldap=%v oidc=%v: password=false, want ВСЕГДА true (Q4/ADR-058)", c.ldap, c.oidc)
		}
		if r.LDAP != c.ldap || r.OIDC != c.oidc {
			t.Errorf("ldap=%v oidc=%v: methods не отражают deps: got {ldap:%v, oidc:%v}", c.ldap, c.oidc, r.LDAP, r.OIDC)
		}
	}
}

// --- 9. Path=/auth на session-cookie (гард атрибута) ---

// TestNewSessionCookie_PathScopedToAuth — cookie сужена до Path=/auth (NIM-77):
// браузер не шлёт её на /v1//mcp//docs.
func TestNewSessionCookie_PathScopedToAuth(t *testing.T) {
	c := newSessionCookie("ey.tok.jwt", time.Hour)
	if c.Path != "/auth" {
		t.Errorf("session-cookie Path = %q, want /auth (сужение с /)", c.Path)
	}
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode {
		t.Errorf("session-cookie ослабила атрибуты: HttpOnly=%v Secure=%v SameSite=%v", c.HttpOnly, c.Secure, c.SameSite)
	}
}
