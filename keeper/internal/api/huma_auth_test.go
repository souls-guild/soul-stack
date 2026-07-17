package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/auth"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// stubAuthenticator — a fake LDAP authenticator for the endpoint test.
type stubAuthenticator struct {
	ext auth.ExternalIdentity
	err error
}

func (s stubAuthenticator) Authenticate(_ context.Context, _, _ string) (auth.ExternalIdentity, error) {
	return s.ext, s.err
}

// stubMapper — a fake Mapper.
type stubMapper struct {
	mapped auth.MappedOperator
	err    error
}

func (s stubMapper) Map(_ context.Context, _ auth.ExternalIdentity) (auth.MappedOperator, error) {
	return s.mapped, s.err
}

// loginStubIssuer — a fake JWT issuer.
type loginStubIssuer struct {
	token    string
	err      error
	gotAID   string
	gotRoles []string
}

func (s *loginStubIssuer) Issue(aid string, roles []string, _ time.Duration, _ bool) (string, error) {
	s.gotAID = aid
	s.gotRoles = roles
	return s.token, s.err
}

// authTestAudit — an in-memory audit.Writer for the endpoint test.
type authTestAudit struct{ events []*audit.Event }

func (a *authTestAudit) Write(_ context.Context, ev *audit.Event) error {
	a.events = append(a.events, ev)
	return nil
}

// mountLogin brings up a chi router with /auth/ldap/login on the given deps.
func mountLogin(d *LDAPAuthDeps) http.Handler {
	r := chi.NewRouter()
	r.Route("/auth", func(r chi.Router) {
		registerHumaLDAPLogin(newHumaAuthAPI(r), d)
	})
	return r
}

func doLogin(h http.Handler) *httptest.ResponseRecorder {
	body, _ := json.Marshal(LDAPLoginRequest{Username: "alice", Password: "secret"})
	req := httptest.NewRequest(http.MethodPost, "/auth/ldap/login", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestLDAPLogin_SetsSecureCookie — happy path: cookie HttpOnly+Secure+SameSite=
// Strict, empty body, audit operator.login written, JWT issued with roles.
func TestLDAPLogin_SetsSecureCookie(t *testing.T) {
	issuer := &loginStubIssuer{token: "ey.tok.jwt"}
	aw := &authTestAudit{}
	d := &LDAPAuthDeps{
		Authenticator: stubAuthenticator{ext: auth.ExternalIdentity{AID: "alice", Groups: []string{"ops"}}},
		Mapper:        stubMapper{mapped: auth.MappedOperator{AID: "alice", Roles: []string{"cluster-admin"}, Provisioned: true}},
		Issuer:        issuer,
		TTL:           time.Hour,
		Audit:         aw,
	}
	rec := doLogin(mountLogin(d))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected exactly 1 Set-Cookie, got %d", len(cookies))
	}
	c := cookies[0]
	if c.Name != sessionCookieName {
		t.Errorf("cookie name = %q, want %q", c.Name, sessionCookieName)
	}
	if c.Value != "ey.tok.jwt" {
		t.Errorf("cookie value = %q, want JWT", c.Value)
	}
	if !c.HttpOnly {
		t.Errorf("cookie must be HttpOnly")
	}
	if !c.Secure {
		t.Errorf("cookie must be Secure")
	}
	if c.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie SameSite = %v, want Strict", c.SameSite)
	}
	if c.Path != "/auth" {
		t.Errorf("cookie Path = %q, want /auth (NIM-77: сужение с /)", c.Path)
	}
	// The JWT token is NOT in the body (cookie-only delivery).
	if strings.Contains(rec.Body.String(), "ey.tok.jwt") {
		t.Errorf("JWT must NOT be in response body (cookie-only delivery)")
	}
	// audit operator.login written.
	if len(aw.events) != 1 || aw.events[0].EventType != audit.EventOperatorLogin {
		t.Fatalf("expected exactly one operator.login audit event, got %v", aw.events)
	}
	if aw.events[0].ArchonAID != "alice" {
		t.Errorf("audit ArchonAID = %q, want alice", aw.events[0].ArchonAID)
	}
	// payload without the password.
	if pw, ok := aw.events[0].Payload["password"]; ok {
		t.Errorf("audit payload must NOT contain password, got %v", pw)
	}
	// JWT issued with roles from the mapping.
	if issuer.gotAID != "alice" || len(issuer.gotRoles) != 1 || issuer.gotRoles[0] != "cluster-admin" {
		t.Errorf("issuer got aid=%q roles=%v, want alice/[cluster-admin]", issuer.gotAID, issuer.gotRoles)
	}
}

// TestLDAPLogin_AuthFailedIs401 — ErrAuthFailed → 401, no cookie, no audit.
func TestLDAPLogin_AuthFailedIs401(t *testing.T) {
	aw := &authTestAudit{}
	d := &LDAPAuthDeps{
		Authenticator: stubAuthenticator{err: auth.ErrAuthFailed},
		Mapper:        stubMapper{},
		Issuer:        &loginStubIssuer{token: "x"},
		TTL:           time.Hour,
		Audit:         aw,
	}
	rec := doLogin(mountLogin(d))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Errorf("failed auth must not set a cookie")
	}
	if len(aw.events) != 0 {
		t.Errorf("failed auth must not write login audit")
	}
}

// TestLDAPLogin_NoRoleMappingIs403 — ErrNoRoleMapping → 403.
func TestLDAPLogin_NoRoleMappingIs403(t *testing.T) {
	d := &LDAPAuthDeps{
		Authenticator: stubAuthenticator{ext: auth.ExternalIdentity{AID: "bob"}},
		Mapper:        stubMapper{err: auth.ErrNoRoleMapping},
		Issuer:        &loginStubIssuer{token: "x"},
		TTL:           time.Hour,
		Audit:         &authTestAudit{},
	}
	rec := doLogin(mountLogin(d))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (no mapped group)", rec.Code)
	}
}

// TestLDAPLogin_RevokedIs403 — ErrOperatorRevoked → 403, JWT NOT issued.
func TestLDAPLogin_RevokedIs403(t *testing.T) {
	issuer := &loginStubIssuer{token: "x"}
	d := &LDAPAuthDeps{
		Authenticator: stubAuthenticator{ext: auth.ExternalIdentity{AID: "carol"}},
		Mapper:        stubMapper{err: auth.ErrOperatorRevoked},
		Issuer:        issuer,
		TTL:           time.Hour,
		Audit:         &authTestAudit{},
	}
	rec := doLogin(mountLogin(d))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (revoked)", rec.Code)
	}
	if issuer.gotAID != "" {
		t.Errorf("revoked operator must not get a JWT (issuer called with %q)", issuer.gotAID)
	}
}

// TestLDAPLogin_NilDepsNoMount — d=nil → the route is not mounted (404).
func TestLDAPLogin_NilDepsNoMount(t *testing.T) {
	var d *LDAPAuthDeps
	rec := doLogin(mountLogin(d))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("nil LDAPAuth must not mount the route, got status %d", rec.Code)
	}
}

// compile-time: the sentinel set is covered (guard against refactoring the auth errors).
var _ = []error{auth.ErrAuthFailed, auth.ErrNoRoleMapping, auth.ErrOperatorRevoked, errors.New("")}
