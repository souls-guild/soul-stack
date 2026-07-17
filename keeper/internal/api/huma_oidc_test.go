package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

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

func doGet(h http.Handler, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestOIDCLogin_RedirectsToIdP — /auth/oidc/login → 302 to the IdP authorization URL.
func TestOIDCLogin_RedirectsToIdP(t *testing.T) {
	authn := &stubOIDCAuthenticator{authz: oidcauth.Authorization{
		RedirectTo: "https://idp.example.com/authorize?state=abc&code_challenge=xyz",
		State:      "abc",
	}}
	d := &OIDCAuthDeps{Authenticator: authn, Mapper: stubMapper{}, Issuer: &loginStubIssuer{}, TTL: time.Hour, Audit: &authTestAudit{}}

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
		Issuer:        issuer,
		TTL:           time.Hour,
		Audit:         aw,
	}

	rec := doGet(mountOIDC(d), "/auth/oidc/callback?code=AUTHCODE&state=STATE123")
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != oidcCallbackSuccessRedirect {
		t.Errorf("Location = %q, want %q", loc, oidcCallbackSuccessRedirect)
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
	// response and SENT on the subsequent same-site navigation to /ui (302).
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
	d := &OIDCAuthDeps{Authenticator: authn, Mapper: stubMapper{}, Issuer: &loginStubIssuer{}, TTL: time.Hour, Audit: &authTestAudit{}}

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
	d := &OIDCAuthDeps{Authenticator: authn, Mapper: stubMapper{err: auth.ErrNoRoleMapping}, Issuer: &loginStubIssuer{}, TTL: time.Hour, Audit: &authTestAudit{}}

	rec := doGet(mountOIDC(d), "/auth/oidc/callback?code=c&state=s")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

// TestOIDCCallback_ProvisioningDisabledIs403 — Mapper ErrProvisioningDisabled →
// 403 (provisioning_allowed_methods policy without oidc; reuses the policy gate).
func TestOIDCCallback_ProvisioningDisabledIs403(t *testing.T) {
	authn := &stubOIDCAuthenticator{ext: auth.ExternalIdentity{AID: "newbie"}}
	d := &OIDCAuthDeps{Authenticator: authn, Mapper: stubMapper{err: auth.ErrProvisioningDisabled}, Issuer: &loginStubIssuer{}, TTL: time.Hour, Audit: &authTestAudit{}}

	rec := doGet(mountOIDC(d), "/auth/oidc/callback?code=c&state=s")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (provisioning disabled)", rec.Code)
	}
}

// TestOIDCCallback_IdPError — IdP returned error=access_denied → 401, no cookie,
// CompleteLogin is not even called.
func TestOIDCCallback_IdPError(t *testing.T) {
	authn := &stubOIDCAuthenticator{}
	d := &OIDCAuthDeps{Authenticator: authn, Mapper: stubMapper{}, Issuer: &loginStubIssuer{}, TTL: time.Hour, Audit: &authTestAudit{}}

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
