package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/auth"
)

// auth.insecure_cookies (NIM-1063) lifts Secure from the session cookie of a
// local-password or LDAP login and nothing else: the rest of the cookie is unchanged,
// and OIDC — which cannot finish over plain HTTP anyway — keeps Secure on both
// its cookies.

// sessionCookieOf returns the only soul_session cookie a response set, and its
// raw Set-Cookie line, so an attribute is checked both parsed and as sent.
func sessionCookieOf(t *testing.T, rec *httptest.ResponseRecorder) (*http.Cookie, string) {
	t.Helper()
	for _, raw := range rec.Result().Header.Values("Set-Cookie") {
		if strings.HasPrefix(raw, sessionCookieName+"=") {
			for _, c := range rec.Result().Cookies() {
				if c.Name == sessionCookieName {
					return c, raw
				}
			}
		}
	}
	t.Fatalf("no %s cookie in the response; Set-Cookie = %q", sessionCookieName, rec.Result().Header.Values("Set-Cookie"))
	return nil, ""
}

// assertSessionCookie checks the attributes the flag must never touch, and
// Secure against want.
func assertSessionCookie(t *testing.T, c *http.Cookie, raw string, wantSecure bool) {
	t.Helper()
	if c.Secure != wantSecure || strings.Contains(raw, "; Secure") != wantSecure {
		t.Fatalf("Secure = %v (raw %q), want %v", c.Secure, raw, wantSecure)
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/auth" {
		t.Fatalf("cookie = %+v: HttpOnly, SameSite=Strict and Path=/auth must stay whatever the flag says", c)
	}
}

func TestInsecureCookies_LocalLogin(t *testing.T) {
	for _, insecure := range []bool{false, true} {
		authn := &stubLocalAuthenticator{op: auth.MappedOperator{AID: "alice", Roles: []string{"cluster-admin"}}, token: "ey.local.jwt"}
		rec := postLocalLogin(mountLocalLogin(&LocalAuthDeps{Authenticator: authn, TTL: time.Hour, InsecureCookies: insecure, Audit: &authTestAudit{}}), "alice", "correct horse battery")
		if rec.Code != http.StatusNoContent {
			t.Fatalf("insecure=%v: status = %d, want 204; body=%s", insecure, rec.Code, rec.Body.String())
		}
		c, raw := sessionCookieOf(t, rec)
		assertSessionCookie(t, c, raw, !insecure)
	}
}

func TestInsecureCookies_LDAPLogin(t *testing.T) {
	for _, insecure := range []bool{false, true} {
		d := &LDAPAuthDeps{
			Authenticator:   stubAuthenticator{ext: auth.ExternalIdentity{AID: "alice", Groups: []string{"ops"}}},
			Mapper:          stubMapper{mapped: auth.MappedOperator{AID: "alice", Roles: []string{"cluster-admin"}, Provisioned: true}},
			Sessions:        &loginStubIssuer{token: "ey.tok.jwt"},
			TTL:             time.Hour,
			InsecureCookies: insecure,
			Audit:           &authTestAudit{},
		}
		rec := doLogin(mountLogin(d))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("insecure=%v: status = %d, want 204; body=%s", insecure, rec.Code, rec.Body.String())
		}
		c, raw := sessionCookieOf(t, rec)
		assertSessionCookie(t, c, raw, !insecure)
	}
}

// Built without a flag, so this catches a hard-coded true only; a flag wired
// into OIDC is caught by TestInsecureCookies_OIDCSourceKeepsSecure.
func TestInsecureCookies_OIDCCallbackStaysSecure(t *testing.T) {
	d := &OIDCAuthDeps{
		Authenticator: &stubOIDCAuthenticator{ext: auth.ExternalIdentity{AID: "alice", Groups: []string{"ops"}}},
		Mapper:        stubMapper{mapped: auth.MappedOperator{AID: "alice", Roles: []string{"cluster-admin"}, Provisioned: true}},
		Sessions:      &loginStubIssuer{token: "ey.oidc.jwt"},
		TTL:           time.Hour,
		Audit:         &authTestAudit{},
	}
	rec := doGet(mountOIDC(d), "/auth/oidc/callback?code=AUTHCODE&state=STATE123")
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", rec.Code, rec.Body.String())
	}
	c, raw := sessionCookieOf(t, rec)
	assertSessionCookie(t, c, raw, true)
}

// OIDC ignores auth.insecure_cookies (ADR-058(l)), and the docs that call the
// login methods symmetric invite wiring the flag in. So the OIDC source passes
// the predeclared false to newSessionCookie, never sets Secure after it, and
// never names the flag. Its source in keeper.yml is fenced off on the daemon
// side by TestInsecureCookies_ReadOnlyByLocalAndLDAP.
func TestInsecureCookies_OIDCSourceKeepsSecure(t *testing.T) {
	names, err := filepath.Glob("*oidc*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	calls := 0
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			for _, id := range declaredNames(n) {
				if id.Name == "false" {
					t.Errorf("%s: declares a local false, which shadows the predeclared one", fset.Position(id.Pos()))
				}
			}
			switch n := n.(type) {
			case *ast.CallExpr:
				if fn, ok := n.Fun.(*ast.Ident); !ok || fn.Name != "newSessionCookie" {
					return true
				}
				calls++
				if len(n.Args) != 3 {
					t.Errorf("%s: newSessionCookie with %d args, want 3", fset.Position(n.Pos()), len(n.Args))
					return true
				}
				if arg, ok := n.Args[2].(*ast.Ident); !ok || arg.Name != "false" {
					t.Errorf("%s: newSessionCookie's insecure is not the literal false — OIDC must keep Secure", fset.Position(n.Pos()))
				}
			case *ast.AssignStmt:
				for _, lhs := range n.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "Secure" {
						t.Errorf("%s: assigns .Secure after the builder — OIDC must keep Secure", fset.Position(sel.Pos()))
					}
				}
			case *ast.Ident:
				if n.Name == "InsecureCookies" {
					t.Errorf("%s: names InsecureCookies — OIDC must not take the flag", fset.Position(n.Pos()))
				}
			}
			return true
		})
	}
	if calls == 0 {
		t.Fatalf("no newSessionCookie call in %v — this guard now guards nothing", names)
	}
}

// declaredNames returns the names n introduces in its file, so a shadowing
// `false` is caught without type information. A package-level one declared in
// another file is out of its reach.
func declaredNames(n ast.Node) []*ast.Ident {
	var out []*ast.Ident
	add := func(exprs ...ast.Expr) {
		for _, e := range exprs {
			if id, ok := e.(*ast.Ident); ok {
				out = append(out, id)
			}
		}
	}
	switch n := n.(type) {
	case *ast.AssignStmt:
		if n.Tok == token.DEFINE {
			add(n.Lhs...)
		}
	case *ast.RangeStmt:
		if n.Tok == token.DEFINE {
			add(n.Key, n.Value)
		}
	case *ast.ValueSpec:
		out = append(out, n.Names...)
	case *ast.Field:
		out = append(out, n.Names...)
	}
	return out
}

// A __Host- cookie without Secure is rejected by the browser outright, so the
// flag cannot make OIDC work over HTTP and must not pretend to.
func TestInsecureCookies_OIDCStateCookieStaysSecure(t *testing.T) {
	d := &OIDCAuthDeps{Authenticator: &stubOIDCAuthenticator{}, Mapper: stubMapper{}, Sessions: &loginStubIssuer{}, TTL: time.Hour}
	rec := doGet(mountOIDC(d), "/auth/oidc/login")
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == oidcStateCookieName {
			if !c.Secure || !strings.HasPrefix(c.Name, "__Host-") {
				t.Fatalf("state cookie = %+v, want a Secure __Host- cookie", c)
			}
			return
		}
	}
	t.Fatalf("no %s cookie in the login response", oidcStateCookieName)
}
