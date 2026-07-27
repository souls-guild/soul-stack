package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
)

// wsRequest builds a WebSocket upgrade request with the given
// Sec-WebSocket-Protocol offer.
func wsRequest(path, subprotocols string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	if subprotocols != "" {
		r.Header.Set("Sec-WebSocket-Protocol", subprotocols)
	}
	return r
}

func TestWS_SubprotocolToken_Valid_200(t *testing.T) {
	v := newVerifier(t)
	tok := newToken(t, time.Hour)
	h := RequireJWT(v)(nextOK(t, "archon-alice"))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, wsRequest("/v1/console", "soul-stack.console.v1, bearer."+tok))

	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, want 200", rec.Code)
	}
}

// The browser sends the offer without a space after the comma just as often —
// RFC 7230 list parsing must tolerate arbitrary OWS.
func TestWS_SubprotocolToken_NoSpace_200(t *testing.T) {
	v := newVerifier(t)
	tok := newToken(t, time.Hour)
	h := RequireJWT(v)(nextOK(t, "archon-alice"))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, wsRequest("/v1/console", "soul-stack.console.v1,bearer."+tok))

	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, want 200", rec.Code)
	}
}

func TestWS_SubprotocolToken_Invalid_401(t *testing.T) {
	v := newVerifier(t)
	h := RequireJWT(v)(nextShouldNotRun(t))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, wsRequest("/v1/console", "soul-stack.console.v1, bearer.not-a-jwt"))

	assertProblem(t, rec, http.StatusUnauthorized, problem.TypeUnauthenticated)
}

func TestWS_NoSubprotocolToken_401(t *testing.T) {
	v := newVerifier(t)
	h := RequireJWT(v)(nextShouldNotRun(t))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, wsRequest("/v1/console", "soul-stack.console.v1"))

	assertProblem(t, rec, http.StatusUnauthorized, problem.TypeUnauthenticated)
}

// The subprotocol token is scoped to the WebSocket handshake exactly like the
// SSE query-token is scoped to `text/event-stream`: a plain GET carrying the
// same header must not authenticate, or the exception becomes a general
// header-shaped bypass.
func TestWS_SubprotocolToken_OnNonUpgrade_Ignored(t *testing.T) {
	v := newVerifier(t)
	tok := newToken(t, time.Hour)
	h := RequireJWT(v)(nextShouldNotRun(t))

	r := httptest.NewRequest(http.MethodGet, "/v1/console", nil)
	r.Header.Set("Sec-WebSocket-Protocol", "soul-stack.console.v1, bearer."+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	assertProblem(t, rec, http.StatusUnauthorized, problem.TypeUnauthenticated)
}

func TestWS_SubprotocolToken_OnPost_Ignored(t *testing.T) {
	v := newVerifier(t)
	tok := newToken(t, time.Hour)
	h := RequireJWT(v)(nextShouldNotRun(t))

	r := httptest.NewRequest(http.MethodPost, "/v1/console", nil)
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Sec-WebSocket-Protocol", "soul-stack.console.v1, bearer."+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	assertProblem(t, rec, http.StatusUnauthorized, problem.TypeUnauthenticated)
}

// An explicit Authorization header still wins — the subprotocol is a fallback
// for the browser WebSocket API, not a replacement.
func TestWS_BearerHeaderStillWorks(t *testing.T) {
	v := newVerifier(t)
	tok := newToken(t, time.Hour)
	h := RequireJWT(v)(nextOK(t, "archon-alice"))

	r := wsRequest("/v1/console", "soul-stack.console.v1")
	r.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, want 200", rec.Code)
	}
}
