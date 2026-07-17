package api

// Guard tests for the session-cookie→Bearer exchange (NIM-77, POST
// /auth/token) and the public GET /auth/methods. Key perimeter invariant:
// the cookie does NOT grant access to /v1 (RequireJWT reads only
// Authorization) — a regression here catches any cookie-read regression on
// /v1.

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

// exchangeSigningKey — a fixed 32-byte HS256 key (RFC 7518 minimum) for the
// verifier+issuer of the test (one key = round-trip).
var exchangeSigningKey = []byte("nim77-exchange-signing-key-32byte")

const exchangeIssuer = "keeper.test"

// exchangeCrypto builds a shared verifier+issuer on one key (session-cookie
// and the issued Bearer are verified by the same verifier — the point of
// Variant B).
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

// fakeRevoked — a revocationChecker for the revoked-branch test.
type fakeRevoked map[string]bool

func (f fakeRevoked) IsRevoked(aid string) bool { return f[aid] }

// authTokenRouter assembles the REAL Operator API router (buildRouter) with
// stub handlers (like collectRoutes), but with live verifier/enforcer/
// authToken — so both /auth/token and the /v1 perimeter can be checked in
// one tree.
func authTokenRouter(t *testing.T, verifier *jwt.Verifier, enforcer RBACProvider, authToken *AuthTokenDeps, authMethods AuthMethodsDeps) http.Handler {
	t.Helper()
	return buildRouter(
		verifier,
		nil, // healthH
		stubOperatorHandler(t),
		handlers.NewIncarnationHandler(nil, nil, nil, nil, nil, nil, nil, nil, nil),
		handlers.NewSoulHandler(nil, nil, nil, nil),
		handlers.TelemetrySpecStub(),
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

// emptyEnforcer — a live default-deny enforcer (empty snapshot): any /v1
// RequireAction/RequirePermission → 403 (not 401). Enough to confirm "Bearer
// accepted at the JWT level" (403≠401).
func emptyEnforcer(t *testing.T) *rbac.Enforcer {
	t.Helper()
	e, err := rbac.NewEnforcerFromSnapshot(nil)
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	return e
}

// postExchange sends POST /auth/token with cookie soul_session=session (empty
// session → cookie is not set) and optional headers.
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

// decodeReply parses the body of a successful exchange.
func decodeReply(t *testing.T, rec *httptest.ResponseRecorder) AuthTokenReply {
	t.Helper()
	var r AuthTokenReply
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode reply: %v; body=%s", err, rec.Body.String())
	}
	return r
}

// --- 1. Perimeter invariant: cookie does NOT grant access to /v1 ---

// TestAuthPerimeter_V1CookieOnly_401 — a request to /v1 with ONLY a valid
// session cookie and WITHOUT Authorization → 401. The cookie is not read on
// /v1 (RequireJWT).
func TestAuthPerimeter_V1CookieOnly_401(t *testing.T) {
	verifier, issuer := exchangeCrypto(t)
	session, err := issuer.Issue("archon-alice", []string{"cluster-admin"}, time.Hour, false)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	td := &AuthTokenDeps{Verifier: verifier, Issuer: issuer, TTL: 10 * time.Minute}
	h := authTokenRouter(t, verifier, emptyEnforcer(t), td, AuthMethodsDeps{})

	req := httptest.NewRequest(http.MethodGet, "/v1/souls", http.NoBody)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session}) // ONLY the cookie, no Authorization
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /v1/souls with cookie-only = %d, want 401 (cookie must NOT grant /v1 access); body=%s",
			rec.Code, rec.Body.String())
	}
}

// --- 2. Round-trip: exchange → Verify + acceptance on /v1 ---

// TestAuthTokenExchange_ValidCookie_RoundTrip — a valid cookie → 200, token
// non-empty, expires_at≈now+ttl; the issued token passes Verify AND
// RequireJWT on /v1 (403≠401 from RBAC means "Bearer accepted").
func TestAuthTokenExchange_ValidCookie_RoundTrip(t *testing.T) {
	verifier, issuer := exchangeCrypto(t)
	session, _ := issuer.Issue("archon-alice", []string{"cluster-admin"}, time.Hour, false)
	td := &AuthTokenDeps{Verifier: verifier, Issuer: issuer, TTL: 10 * time.Minute}
	h := authTokenRouter(t, verifier, emptyEnforcer(t), td, AuthMethodsDeps{})

	rec := postExchange(h, session, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /auth/token valid cookie = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	reply := decodeReply(t, rec)
	if reply.Token == "" {
		t.Fatal("token is empty")
	}
	if d := time.Until(reply.ExpiresAt); d < 9*time.Minute || d > 11*time.Minute {
		t.Errorf("expires_at in %s, want ≈10m", d)
	}

	// Round-trip: the issued token is verified by the same verifier.
	claims, err := verifier.Verify(reply.Token)
	if err != nil {
		t.Fatalf("issued Bearer does not pass Verify: %v", err)
	}
	if claims.Subject != "archon-alice" {
		t.Errorf("sub of issued token = %q, want archon-alice", claims.Subject)
	}
	// F2: expires_at EXACTLY matches exp inside the token (derived from
	// self-verify, not a second time.Now().Add(ttl)) — the client plans the
	// re-exchange against the correct deadline.
	if !reply.ExpiresAt.Equal(claims.ExpiresAt) {
		t.Errorf("expires_at=%s != token exp=%s (must match exactly)", reply.ExpiresAt, claims.ExpiresAt)
	}

	// And it is accepted on a real /v1 route as Bearer (403 from RBAC ≠ 401).
	req := httptest.NewRequest(http.MethodGet, "/v1/souls", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+reply.Token)
	vrec := httptest.NewRecorder()
	h.ServeHTTP(vrec, req)
	if vrec.Code == http.StatusUnauthorized {
		t.Errorf("Bearer from exchange → 401 on /v1 (must pass RequireJWT); body=%s", vrec.Body.String())
	}
}

// --- 3. Bad/missing cookies → 401 ---

func TestAuthTokenExchange_BadCookies_401(t *testing.T) {
	verifier, issuer := exchangeCrypto(t)
	td := &AuthTokenDeps{Verifier: verifier, Issuer: issuer, TTL: 10 * time.Minute}
	h := authTokenRouter(t, verifier, emptyEnforcer(t), td, AuthMethodsDeps{})

	// An expired cookie is rejected at STEP 3 (Verify → ErrExpiredToken), NOT
	// at the ttl≤0 guard of step 5 (that one is defensive/unreachable without
	// leeway in Verify).
	expired, _ := issuer.Issue("archon-alice", nil, time.Millisecond, false)
	time.Sleep(5 * time.Millisecond)

	cases := []struct {
		name    string
		session string
	}{
		{"no cookie", ""},
		{"malformed cookie", "not-a-jwt"},
		{"foreign signature", strings.Repeat("a", 20) + ".b.c"},
		{"expired cookie (Verify-expired, step 3)", expired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postExchange(h, tc.session, nil)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s: status = %d, want 401; body=%s", tc.name, rec.Code, rec.Body.String())
			}
			// The raw jwt-library text must not leak (anti-oracle).
			if strings.Contains(rec.Body.String(), "golang-jwt") || strings.Contains(rec.Body.String(), "token contains") {
				t.Errorf("%s: body contains raw cause: %s", tc.name, rec.Body.String())
			}
		})
	}
}

// --- 4. Subject ONLY from cookie claims ---

// TestAuthTokenExchange_SubjectFromClaimsOnly — an attempt to force a
// different aid via body/query/header does NOT change the sub of the issued
// token (no privilege escalation).
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
		t.Errorf("sub = %q, want archon-real (must not be taken from body/query/header)", claims.Subject)
	}
	if len(claims.Roles) != 1 || claims.Roles[0] != "read-only" {
		t.Errorf("roles = %v, want [read-only] (from cookie claims, not from body)", claims.Roles)
	}
}

// --- 4b. bootstrap_initial survives the exchange ---

// TestAuthTokenExchange_BootstrapInitialRoundTrip — the bootstrap_initial
// flag from the cookie carries over into the issued token as-is
// (true→true, false→false): it upholds the invariant "the last bootstrap
// operator cannot be removed" (ADR-013); resetting/raising it during the
// exchange would break that.
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
			t.Errorf("issued token bootstrap_initial = %v, want %v (flag from cookie claims)",
				claims.BootstrapInitial, bootstrap)
		}
	}
}

// --- 5. Revoked ---

// TestAuthTokenExchange_Revoked_401 — a cookie of a revoked AID → 401, the
// issuer is NOT called (no token issued).
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
		t.Errorf("revoked → issuer called with %q (no token should be issued)", spy.gotAID)
	}
}

// --- 6. Sec-Fetch allowlist (block ONLY cross-site) ---

// TestAuthTokenExchange_SecFetchAllowlist — Sec-Fetch-Site defense-in-depth
// pins that EXACTLY cross-site is rejected (403); same-origin/same-site/none
// and an absent header — pass through (200 with a valid cookie). Origin is
// deliberately NOT validated: protection comes from SameSite=Strict +
// Path=/auth + HttpOnly on the cookie itself.
func TestAuthTokenExchange_SecFetchAllowlist(t *testing.T) {
	verifier, issuer := exchangeCrypto(t)
	session, _ := issuer.Issue("archon-alice", nil, time.Hour, false)
	td := &AuthTokenDeps{Verifier: verifier, Issuer: issuer, TTL: 10 * time.Minute}
	h := authTokenRouter(t, verifier, emptyEnforcer(t), td, AuthMethodsDeps{})

	cases := []struct {
		name       string
		secFetch   string // "" → the header is not set
		setHeader  bool
		wantStatus int
	}{
		{"cross-site → 403", "cross-site", true, http.StatusForbidden},
		{"same-origin → 200", "same-origin", true, http.StatusOK},
		{"same-site → 200", "same-site", true, http.StatusOK},
		{"none → 200", "none", true, http.StatusOK},
		{"no header → 200", "", false, http.StatusOK},
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

// --- 7. TTL cap on cookie.exp ---

// TestAuthTokenExchange_TTLCap — a cookie with remaining < exchange_ttl →
// the issued token's exp ≈ cookie.exp (capped, not 10m).
func TestAuthTokenExchange_TTLCap(t *testing.T) {
	verifier, issuer := exchangeCrypto(t)
	// Cookie lives 30s; exchange_ttl = 10m → issued exp ≈ 30s.
	session, _ := issuer.Issue("archon-alice", nil, 30*time.Second, false)
	td := &AuthTokenDeps{Verifier: verifier, Issuer: issuer, TTL: 10 * time.Minute}
	h := authTokenRouter(t, verifier, emptyEnforcer(t), td, AuthMethodsDeps{})

	rec := postExchange(h, session, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if d := time.Until(decodeReply(t, rec).ExpiresAt); d > time.Minute {
		t.Errorf("expires_at in %s — not capped to cookie.exp (~30s), want <1m", d)
	}
	// remaining<=0 (expired cookie) → 401 is covered by TestAuthTokenExchange_BadCookies_401.
}

// --- 8. GET /auth/methods (public) ---

// TestAuthMethods_PublicBooleans — /auth/methods without Authorization → 200
// with booleans from deps.
func TestAuthMethods_PublicBooleans(t *testing.T) {
	h := authTokenRouter(t, nil, emptyEnforcer(t), nil, AuthMethodsDeps{Password: true, LDAP: true, OIDC: false})

	req := httptest.NewRequest(http.MethodGet, "/auth/methods", http.NoBody) // no Authorization
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /auth/methods = %d, want 200 (public); body=%s", rec.Code, rec.Body.String())
	}
	var r AuthMethodsReply
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if !r.Password || !r.LDAP || r.OIDC {
		t.Errorf("methods = %+v, want {password:true, ldap:true, oidc:false}", r)
	}
}

// TestAuthMethods_PasswordAlwaysTrue — F1 (Q4/ADR-058 contract): password=true
// for ANY ldap/oidc combo (pasting the operator JWT into localStorage is
// always available, not tied to the endpoint). ldap/oidc reflect deps as-is.
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
			t.Errorf("ldap=%v oidc=%v: password=false, want ALWAYS true (Q4/ADR-058)", c.ldap, c.oidc)
		}
		if r.LDAP != c.ldap || r.OIDC != c.oidc {
			t.Errorf("ldap=%v oidc=%v: methods do not reflect deps: got {ldap:%v, oidc:%v}", c.ldap, c.oidc, r.LDAP, r.OIDC)
		}
	}
}

// --- 9. Path=/auth on the session cookie (attribute guard) ---

// TestNewSessionCookie_PathScopedToAuth — the cookie is scoped to Path=/auth
// (NIM-77): the browser does not send it on /v1//mcp//docs.
func TestNewSessionCookie_PathScopedToAuth(t *testing.T) {
	c := newSessionCookie("ey.tok.jwt", time.Hour)
	if c.Path != "/auth" {
		t.Errorf("session-cookie Path = %q, want /auth (narrowed from /)", c.Path)
	}
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode {
		t.Errorf("session-cookie weakened attributes: HttpOnly=%v Secure=%v SameSite=%v", c.HttpOnly, c.Secure, c.SameSite)
	}
}
