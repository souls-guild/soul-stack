package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/auth"
	oidcauth "github.com/souls-guild/soul-stack/keeper/internal/auth/oidc"
)

// stubOIDCAuthenticator — a fake OIDC authenticator for the endpoint test.
type stubOIDCAuthenticator struct {
	authz       oidcauth.Authorization
	beginErr    error
	ext         auth.ExternalIdentity
	completeErr error
	gotCode     string
	gotState    string
}

func (s *stubOIDCAuthenticator) BeginLogin(_ context.Context) (oidcauth.Authorization, error) {
	return s.authz, s.beginErr
}

func (s *stubOIDCAuthenticator) CompleteLogin(_ context.Context, code, state string) (auth.ExternalIdentity, error) {
	s.gotCode, s.gotState = code, state
	return s.ext, s.completeErr
}

// mountOIDC brings up a chi router with /auth/oidc/* on the given deps.
func mountOIDC(d *OIDCAuthDeps) http.Handler {
	r := chi.NewRouter()
	r.Route("/auth", func(r chi.Router) {
		registerHumaOIDCLogin(newHumaAuthAPI(r), d)
	})
	return r
}

// doGet — a callback carries the state cookie its login would have set in the
// same browser; the tests of a missing or foreign one build their own request.
func doGet(h http.Handler, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if req.URL.Path == "/auth/oidc/callback" {
		req.AddCookie(&http.Cookie{Name: oidcStateCookieName, Value: req.URL.Query().Get("state")})
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The login binds its state to the browser: an HttpOnly, Secure cookie the
// IdP's cross-site redirect back still carries (Lax), which a sibling domain
// cannot plant (__Host-: Path=/, no Domain).
func TestOIDCLogin_BindsTheStateToTheBrowser(t *testing.T) {
	authn := &stubOIDCAuthenticator{authz: oidcauth.Authorization{RedirectTo: "https://idp.example.com/authorize?state=abc", State: "abc"}}
	rec := doGet(mountOIDC(&OIDCAuthDeps{Authenticator: authn, Mapper: stubMapper{}, Sessions: &loginStubIssuer{}, TTL: time.Hour}), "/auth/oidc/login")
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("Set-Cookie count = %d, want 1", len(cookies))
	}
	c := cookies[0]
	if c.Name != "__Host-soul_oidc_state" || c.Value != "abc" || c.Path != "/" || c.Domain != "" || !c.HttpOnly || !c.Secure ||
		c.SameSite != http.SameSiteLaxMode || c.MaxAge <= 0 {
		t.Errorf("state cookie = %+v", c)
	}
}

// A callback URL opened in a browser that did not start the login — no state
// cookie, or another login's — is refused before the state is used, so a
// stranger's IdP sign-in cannot become this browser's session (login CSRF).
func TestOIDCCallback_RefusesAStateThisBrowserDidNotStart(t *testing.T) {
	for _, c := range []struct {
		name   string
		cookie *http.Cookie
	}{
		{"no state cookie", nil},
		{"another login's state", &http.Cookie{Name: oidcStateCookieName, Value: "MINE"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			issuer := &loginStubIssuer{token: "ey.oidc.jwt"}
			authn := &stubOIDCAuthenticator{ext: auth.ExternalIdentity{AID: "mallory"}}
			h := mountOIDC(&OIDCAuthDeps{
				Authenticator: authn,
				Mapper:        stubMapper{mapped: auth.MappedOperator{AID: "mallory", Roles: []string{"operator"}}},
				Sessions:      issuer,
				TTL:           time.Hour,
			})
			req := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?code=STOLEN&state=THEIRS", nil)
			if c.cookie != nil {
				req.AddCookie(c.cookie)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
			}
			if authn.gotState != "" {
				t.Errorf("the state %q was used despite the mismatch", authn.gotState)
			}
			if len(rec.Result().Cookies()) != 0 {
				t.Errorf("a session cookie was set: %v", rec.Result().Cookies())
			}
		})
	}
}

// TestOIDCLogin_RedirectsToIdP — /auth/oidc/login → 302 to the IdP authorization URL.
func TestOIDCLogin_RedirectsToIdP(t *testing.T) {
	authn := &stubOIDCAuthenticator{authz: oidcauth.Authorization{
		RedirectTo: "https://idp.example.com/authorize?state=abc&code_challenge=xyz",
		State:      "abc",
	}}
	d := &OIDCAuthDeps{Authenticator: authn, Mapper: stubMapper{}, Sessions: &loginStubIssuer{}, TTL: time.Hour, Audit: &authTestAudit{}}

	rec := doGet(mountOIDC(d), "/auth/oidc/login")
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "https://idp.example.com/authorize") {
		t.Errorf("Location = %q, want IdP authorization URL", loc)
	}
}

// TestOIDCCallback_SetsSecureCookieAndRedirects — ★ happy: 302 to the UI + a cookie
// HttpOnly+Secure+SameSite=Lax (Lax — to survive the cross-site redirect from the IdP);
// JWT in the cookie, not in the body; audit operator.login(method=oidc) recorded.
func TestOIDCCallback_SetsSecureCookieAndRedirects(t *testing.T) {
	issuer := &loginStubIssuer{token: "ey.oidc.jwt"}
	aw := &authTestAudit{}
	authn := &stubOIDCAuthenticator{ext: auth.ExternalIdentity{AID: "alice", Groups: []string{"ops"}}}
	d := &OIDCAuthDeps{
		Authenticator: authn,
		Mapper:        stubMapper{mapped: auth.MappedOperator{AID: "alice", Roles: []string{"cluster-admin"}, Provisioned: true}},
		Sessions:      issuer,
		TTL:           time.Hour,
		Audit:         aw,
	}

	rec := doGet(mountOIDC(d), "/auth/oidc/callback?code=AUTHCODE&state=STATE123")
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", rec.Code, rec.Body.String())
	}
	// The literal, not the constant: the web login page exchanges the cookie
	// only when it lands with sso=done (ADR-058 amendment NIM-1050).
	if loc := rec.Header().Get("Location"); loc != "/ui/login?sso=done" {
		t.Errorf("Location = %q, want /ui/login?sso=done", loc)
	}
	// authenticator received code+state from the query.
	if authn.gotCode != "AUTHCODE" || authn.gotState != "STATE123" {
		t.Errorf("authenticator got code=%q state=%q, want AUTHCODE/STATE123", authn.gotCode, authn.gotState)
	}

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected exactly 1 Set-Cookie, got %d", len(cookies))
	}
	c := cookies[0]
	if c.Name != sessionCookieName || c.Value != "ey.oidc.jwt" {
		t.Errorf("cookie = %q=%q, want %q=JWT", c.Name, c.Value, sessionCookieName)
	}
	if !c.HttpOnly {
		t.Errorf("cookie must be HttpOnly")
	}
	if !c.Secure {
		t.Errorf("cookie must be Secure")
	}
	// MED fix (2026-06-24): SameSite unified to Strict (parity LDAP,
	// ADR-058(g)). Strict is safe on the callback: the cookie is SET on the callback
	// response; the UI login page the 302 lands on sends it, same-site, to
	// POST /auth/token (Path=/auth keeps it off the navigation itself).
	if c.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie SameSite = %v, want Strict (unified with LDAP, MED fix)", c.SameSite)
	}
	if strings.Contains(rec.Body.String(), "ey.oidc.jwt") {
		t.Errorf("JWT must NOT be in response body (cookie-only)")
	}
	if len(aw.events) != 1 {
		t.Fatalf("expected exactly one audit event, got %d", len(aw.events))
	}
	if m, _ := aw.events[0].Payload["method"].(string); m != "oidc" {
		t.Errorf("audit method = %q, want oidc", m)
	}
}

// TestOIDCCallback_AuthFailedIs401 — CompleteLogin ErrAuthFailed → 401, no cookie.
func TestOIDCCallback_AuthFailedIs401(t *testing.T) {
	authn := &stubOIDCAuthenticator{completeErr: auth.ErrAuthFailed}
	d := &OIDCAuthDeps{Authenticator: authn, Mapper: stubMapper{}, Sessions: &loginStubIssuer{}, TTL: time.Hour, Audit: &authTestAudit{}}

	rec := doGet(mountOIDC(d), "/auth/oidc/callback?code=c&state=s")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Errorf("failed callback must not set a cookie")
	}
}

// TestOIDCCallback_NoRoleMappingIs403 — Mapper ErrNoRoleMapping → 403.
//
//nolint:dupl // parity with the sibling ProvisioningDisabled case
func TestOIDCCallback_NoRoleMappingIs403(t *testing.T) {
	authn := &stubOIDCAuthenticator{ext: auth.ExternalIdentity{AID: "bob"}}
	d := &OIDCAuthDeps{Authenticator: authn, Mapper: stubMapper{err: auth.ErrNoRoleMapping}, Sessions: &loginStubIssuer{}, TTL: time.Hour, Audit: &authTestAudit{}}

	rec := doGet(mountOIDC(d), "/auth/oidc/callback?code=c&state=s")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

// TestOIDCCallback_RevokedIs401 — Mapper ErrOperatorRevoked → 401, JWT NOT issued.
//
// 401, not 403 (NIM-421): parity with TestLDAPLogin_RevokedIs401 and with what
// apimiddleware.RejectRevoked answers on every Bearer route. The two 403 cases
// around this one (NoRoleMapping, ProvisioningDisabled) are the states that
// legitimately stay 403 — this is the border, so it is pinned from both sides.
func TestOIDCCallback_RevokedIs401(t *testing.T) {
	authn := &stubOIDCAuthenticator{ext: auth.ExternalIdentity{AID: "carol"}}
	issuer := &loginStubIssuer{token: "x"}
	d := &OIDCAuthDeps{Authenticator: authn, Mapper: stubMapper{err: auth.ErrOperatorRevoked}, Sessions: issuer, TTL: time.Hour, Audit: &authTestAudit{}}

	rec := doGet(mountOIDC(d), "/auth/oidc/callback?code=c&state=s")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (revoked)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), problem.TypeOperatorRevokedToken) {
		t.Errorf("body must carry the %s problem type, got %s",
			problem.TypeOperatorRevokedToken, rec.Body.String())
	}
	if issuer.gotAID != "" {
		t.Errorf("revoked operator must not get a JWT (issuer called with %q)", issuer.gotAID)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Errorf("revoked operator must not get a session cookie")
	}
}

// TestOIDCCallback_ProvisioningDisabledIs403 — Mapper ErrProvisioningDisabled →
// 403 (provisioning_allowed_methods policy without oidc; reuses the policy gate).
func TestOIDCCallback_ProvisioningDisabledIs403(t *testing.T) {
	authn := &stubOIDCAuthenticator{ext: auth.ExternalIdentity{AID: "newbie"}}
	d := &OIDCAuthDeps{Authenticator: authn, Mapper: stubMapper{err: auth.ErrProvisioningDisabled}, Sessions: &loginStubIssuer{}, TTL: time.Hour, Audit: &authTestAudit{}}

	rec := doGet(mountOIDC(d), "/auth/oidc/callback?code=c&state=s")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (provisioning disabled)", rec.Code)
	}
}

// TestOIDCCallback_IdPError — IdP returned error=access_denied → 401, no cookie,
// CompleteLogin is not even called.
func TestOIDCCallback_IdPError(t *testing.T) {
	authn := &stubOIDCAuthenticator{}
	d := &OIDCAuthDeps{Authenticator: authn, Mapper: stubMapper{}, Sessions: &loginStubIssuer{}, TTL: time.Hour, Audit: &authTestAudit{}}

	rec := doGet(mountOIDC(d), "/auth/oidc/callback?error=access_denied&state=s")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (idp error)", rec.Code)
	}
	if authn.gotCode != "" {
		t.Errorf("IdP-error callback must short-circuit before CompleteLogin")
	}
}

// TestOIDC_NilDepsNoMount — d=nil → routes are not mounted (404).
func TestOIDC_NilDepsNoMount(t *testing.T) {
	var d *OIDCAuthDeps
	h := mountOIDC(d)
	if rec := doGet(h, "/auth/oidc/login"); rec.Code != http.StatusNotFound {
		t.Errorf("nil OIDCAuth must not mount /login, got %d", rec.Code)
	}
	if rec := doGet(h, "/auth/oidc/callback?code=c&state=s"); rec.Code != http.StatusNotFound {
		t.Errorf("nil OIDCAuth must not mount /callback, got %d", rec.Code)
	}
}

// compile-time: the stub implements the narrow oidcAuthenticator.
var _ oidcAuthenticator = (*stubOIDCAuthenticator)(nil)
