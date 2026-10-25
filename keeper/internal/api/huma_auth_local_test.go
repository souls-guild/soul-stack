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

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/auth"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
)

type stubLocalAuthenticator struct {
	op      auth.MappedOperator
	token   string
	err     error
	calls   int
	gotAID  string
	gotPass string
	gotTTL  time.Duration
}

func (s *stubLocalAuthenticator) Login(_ context.Context, aid, password string, ttl time.Duration, _ operator.ClientMeta) (auth.MappedOperator, string, error) {
	s.calls++
	s.gotAID, s.gotPass, s.gotTTL = aid, password, ttl
	return s.op, s.token, s.err
}

func mountLocalLogin(d *LocalAuthDeps) http.Handler {
	r := chi.NewRouter()
	r.Route("/auth", func(r chi.Router) {
		registerHumaLocalLogin(newHumaAuthAPI(r), d)
	})
	return r
}

func postLocalLogin(h http.Handler, aid, password string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(LocalLoginRequest{AID: aid, Password: password})
	req := httptest.NewRequest(http.MethodPost, "/auth/local/login", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// A local login mints the same session the federated ones do: the
// `soul_session` cookie (HttpOnly, Secure, Strict, Path=/auth) with no token in
// the body, and one operator.login event naming the method.
func TestLocalLogin_SetsTheSessionCookie(t *testing.T) {
	authn := &stubLocalAuthenticator{op: auth.MappedOperator{AID: "alice", Roles: []string{"cluster-admin"}}, token: "ey.local.jwt"}
	aw := &authTestAudit{}
	rec := postLocalLogin(mountLocalLogin(&LocalAuthDeps{Authenticator: authn, TTL: time.Hour, Audit: aw}), "alice", "correct horse battery")

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("Set-Cookie count = %d, want 1", len(cookies))
	}
	c := cookies[0]
	if c.Name != sessionCookieName || c.Value != "ey.local.jwt" || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Path != "/auth" {
		t.Fatalf("cookie = %+v, want the soul_session cookie of newSessionCookie", c)
	}
	if strings.Contains(rec.Body.String(), "ey.local.jwt") {
		t.Fatal("the JWT is in the body; delivery is cookie-only")
	}
	if authn.gotPass != "correct horse battery" || authn.gotTTL != time.Hour {
		t.Fatalf("the authenticator got password %q ttl %s, want the request's and the configured TTL", authn.gotPass, authn.gotTTL)
	}
	if len(aw.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(aw.events))
	}
	ev := aw.events[0]
	if ev.EventType != "operator.login" || ev.ArchonAID != "alice" || ev.Payload["method"] != "local" {
		t.Fatalf("audit = %+v, want operator.login by alice with method local", ev)
	}
	for k := range ev.Payload {
		if strings.Contains(k, "password") {
			t.Fatalf("audit payload carries %q", k)
		}
	}
}

func TestLocalLogin_Refusals(t *testing.T) {
	for _, c := range []struct {
		name     string
		err      error
		status   int
		typ      string
		auditLen int
	}{
		{"bad credentials", auth.ErrAuthFailed, http.StatusUnauthorized, problem.TypeUnauthenticated, 0},
		{"revoked", auth.ErrOperatorRevoked, http.StatusUnauthorized, problem.TypeOperatorRevokedToken, 0},
		{"store down", errors.New("pg: connection refused"), http.StatusInternalServerError, problem.TypeInternalError, 0},
	} {
		aw := &authTestAudit{}
		rec := postLocalLogin(mountLocalLogin(&LocalAuthDeps{
			Authenticator: &stubLocalAuthenticator{err: c.err}, TTL: time.Hour, Audit: aw,
		}), "alice", "pw")
		if rec.Code != c.status {
			t.Errorf("%s: status = %d, want %d; body=%s", c.name, rec.Code, c.status, rec.Body.String())
			continue
		}
		var p struct {
			Type   string `json:"type"`
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &p)
		if p.Type != c.typ {
			t.Errorf("%s: problem type = %q, want %q", c.name, p.Type, c.typ)
		}
		if strings.Contains(p.Detail, "pg:") {
			t.Errorf("%s: detail leaks the cause: %q", c.name, p.Detail)
		}
		if len(rec.Result().Cookies()) != 0 || len(aw.events) != c.auditLen {
			t.Errorf("%s: cookie or audit written on a refusal", c.name)
		}
	}
}

// auth.local absent → no route; present → the route and the `local` flag in
// /auth/methods (the login form's switch).
func TestLocalLogin_MountedOnlyWithAuthLocal(t *testing.T) {
	without := authTokenRouter(t, nil, emptyEnforcer(t), nil, AuthMethodsDeps{Password: true})
	if rec := postLocalLogin(without, "alice", "pw"); rec.Code != http.StatusNotFound {
		t.Fatalf("without auth.local: POST /auth/local/login = %d, want 404", rec.Code)
	}

	deps := &LocalAuthDeps{Authenticator: &stubLocalAuthenticator{op: auth.MappedOperator{AID: "alice"}, token: "x"}, TTL: time.Hour}
	with := authTokenRouter(t, nil, emptyEnforcer(t), nil, AuthMethodsDeps{Password: true, Local: true}, withLocalAuth(deps))
	if rec := postLocalLogin(with, "alice", "pw"); rec.Code != http.StatusNoContent {
		t.Fatalf("with auth.local: POST /auth/local/login = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest(http.MethodGet, "/auth/methods", http.NoBody)
	rec := httptest.NewRecorder()
	with.ServeHTTP(rec, req)
	var r AuthMethodsReply
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil || !r.Local || !r.Password {
		t.Fatalf("/auth/methods = %s (%v), want local and password true", rec.Body.String(), err)
	}
}

// lockingGuard — a LoginGuard that reports the listed user principals as
// locked and records every principal it is asked about.
type lockingGuard struct {
	locked map[string]bool
	asked  []string
}

func (g *lockingGuard) Allow(_ context.Context, _, _ string, _ float64, _ int) (bool, time.Duration, error) {
	return true, 0, nil
}

func (g *lockingGuard) Locked(_ context.Context, scope, principal string) (bool, time.Duration, error) {
	g.asked = append(g.asked, scope+":"+principal)
	return scope == "user" && g.locked[principal], time.Minute, nil
}

func (g *lockingGuard) RecordFailure(context.Context, string, string, int, time.Duration, time.Duration) (bool, error) {
	return false, nil
}

// POST /auth/local/login runs under AuthLoginLimit keyed by the AID the handler
// will look up: a lockout of "alice" stops " Alice " before the password is
// checked, and a body padded past what the key is read from is refused rather
// than let through under the per-IP layer alone (validator finding, NIM-1051).
func TestLocalLogin_RunsUnderTheLoginLimitKeyedByTheNormalizedAID(t *testing.T) {
	authn := &stubLocalAuthenticator{op: auth.MappedOperator{AID: "alice"}, token: "x"}
	guard := &lockingGuard{locked: map[string]bool{"alice": true}}
	h := authRouterWithGuard(t, nil, emptyEnforcer(t), nil, AuthMethodsDeps{Password: true, Local: true}, guard,
		withLocalAuth(&LocalAuthDeps{Authenticator: authn, TTL: time.Hour}))

	if rec := postLocalLogin(h, " Alice ", "correct horse battery"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("locked AID in another spelling: %d, want 429; body=%s", rec.Code, rec.Body.String())
	}

	body := `{"aid":"alice","password":"correct horse battery"}` + strings.Repeat(" ", 5000)
	req := httptest.NewRequest(http.MethodPost, "/auth/local/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("padded login body: %d, want 413; body=%s", rec.Code, rec.Body.String())
	}
	if authn.calls != 0 {
		t.Fatalf("the authenticator was reached %d times past a lockout or a padded body", authn.calls)
	}
}

// PUT /v1/me/password runs under the same limiter keyed by the caller: a locked
// AID cannot keep guessing its current password with a Bearer.
func TestMyPasswordChange_RunsUnderTheLoginLimitKeyedByTheCaller(t *testing.T) {
	verifier, issuer := exchangeCrypto(t)
	token, err := issuer.Issue("01JTESTJT1000000000000000A", "alice", nil, time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	guard := &lockingGuard{locked: map[string]bool{"alice": true}}
	h := authRouterWithGuard(t, verifier, emptyEnforcer(t), nil, AuthMethodsDeps{Password: true}, guard)

	req := httptest.NewRequest(http.MethodPut, "/v1/me/password",
		strings.NewReader(`{"current_password":"guess","new_password":"a brand new password"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("locked caller: %d, want 429; body=%s", rec.Code, rec.Body.String())
	}
	found := false
	for _, a := range guard.asked {
		if a == "user:alice" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the limiter asked about %v, want the caller user:alice", guard.asked)
	}
}
