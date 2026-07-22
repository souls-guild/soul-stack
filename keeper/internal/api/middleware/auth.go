// Package middleware provides the Operator API HTTP middleware.
//
// The JWT-auth middleware ([RequireJWT]) extracts `Authorization: Bearer …`,
// verifies it via [jwt.Verifier], and places claims in the request context for
// downstream handlers (RBAC / audit / endpoints).
//
// Health/meta endpoints (`/healthz`, `/readyz`, `/metrics`) and the public
// shell of the doc viewer (`/docs`, `/docs/assets/*`) do **not** carry this middleware —
// they are outside the `/v1/*` chain (see operator-api.md § Health / Meta). The spec
// `/openapi.yaml` itself is NOW behind `RequireJWT` (outside `/v1`, without RBAC/audit), see
// router.go.
package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
)

// sseQueryTokenParam — the query-param name through which SSE endpoints
// accept a JWT in place of the `Authorization` header.
//
// EXCEPTION for browser-native EventSource: the EventSource spec does NOT
// allow custom headers, so the UI can't send
// `Authorization: Bearer …` on the SSE channel (`text/event-stream`). This is
// the only sanctioned way to pass a JWT in the URL; for general
// use (any non-SSE request) it's deliberately ignored — a token in the
// URL would leak into access logs / referer / history.
const sseQueryTokenParam = "access_token"

// claimsCtxKey — a non-exported type for the context key to avoid
// accidental cross-package collisions (the Go idiom for context keys).
type claimsCtxKey struct{}

// RequireJWT — a middleware factory: returns middleware that
// extracts the Bearer token, validates it via v.Verify, and passes the request
// on with claims in the context. On any error it returns 401 in
// problem+json form.
//
// The `Authorization: Bearer <token>` header is accepted. The only
// exception is SSE endpoints (see [isSSERequest]): for them, if the
// header is absent, the token is taken from query-param `access_token` (EXCEPTION for
// browser EventSource, which can't do custom headers — NOT for general
// use). On any non-SSE request the query-param is ignored.
func RequireJWT(v *jwt.Verifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := extractToken(r)
			if !ok {
				problem.Write(w, problem.New(
					problem.TypeUnauthenticated,
					r.URL.Path,
					"missing or malformed Authorization header (expect: Bearer <jwt>)",
				))
				return
			}

			claims, err := v.Verify(token)
			if err != nil {
				// detail strings are built ONLY via jwt.ClassifyVerifyErr —
				// never forward err.Error() into the HTTP response (a raw
				// golang-jwt/v5 message = an oracle-attack surface).
				problem.Write(w, problem.New(
					problem.TypeUnauthenticated,
					r.URL.Path,
					jwt.ClassifyVerifyErr(err),
				))
				return
			}

			ctx := context.WithValue(r.Context(), claimsCtxKey{}, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// extractToken pulls the JWT from the request. Priority — `Authorization: Bearer`.
// If the header is absent but the request is SSE (see [isSSERequest]), the
// query-param `access_token` is allowed (browser EventSource limit). On any non-SSE
// request the query-param is NOT read: a token in the URL is a security floor only for
// the streaming channel.
func extractToken(r *http.Request) (string, bool) {
	if tok, ok := jwt.ParseBearerToken(r.Header.Get("Authorization")); ok {
		return tok, true
	}
	if isSSERequest(r) {
		if tok := r.URL.Query().Get(sseQueryTokenParam); tok != "" {
			return tok, true
		}
	}
	return "", false
}

// isSSERequest — true for the SSE channel: GET with `Accept: text/event-stream`
// OR a path ending in `/events` (`GET /v1/voyages/{id}/events`).
// A query token is allowed strictly on such requests — not on mutating methods
// (POST/PUT/DELETE/PATCH) and not on plain GETs.
func isSSERequest(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		return true
	}
	return strings.HasSuffix(r.URL.Path, "/events")
}

// ClaimsFromContext returns the claims placed by [RequireJWT] in the context.
// ok=false means the middleware didn't run (e.g. a handler
// attached directly to the router without the auth chain) — for a `/v1/*` endpoint
// that signals a server misconfiguration.
func ClaimsFromContext(ctx context.Context) (*jwt.Claims, bool) {
	c, ok := ctx.Value(claimsCtxKey{}).(*jwt.Claims)
	return c, ok
}

// InjectClaimsForTest places claims in the context directly — a helper for
// handler unit tests that don't stand up the RequireJWT middleware.
// Do not use outside *_test.go code: production handlers must
// receive claims only through the RequireJWT chain.
func InjectClaimsForTest(ctx context.Context, c *jwt.Claims) context.Context {
	return context.WithValue(ctx, claimsCtxKey{}, c)
}

// WithClaims places claims in the context. Used for in-memory invocation of
// HTTP handlers from MCP tools (httptest.Recorder + ClaimsFromContext —
// claims come not from RequireJWT but from the MCP auth chain).
//
// It differs from [InjectClaimsForTest] only in namespace (test-only vs prod-
// accessible); the implementation is the same. We keep both so test call sites don't
// change meaning (the test helper stays a test helper).
func WithClaims(ctx context.Context, c *jwt.Claims) context.Context {
	return context.WithValue(ctx, claimsCtxKey{}, c)
}
