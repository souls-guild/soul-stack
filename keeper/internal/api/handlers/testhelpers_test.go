package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
)

// Package-level test helpers for the handlers domain. Extracted from operator_test.go
// during the handler-native turn of operator (T5d): operator no longer carries (w,r)
// tests; the helpers stay shared across the other domains (augur/oracle/role/soul/… —
// whose (w,r) handlers are still in place).

// withClaims puts keeperjwt.Claims directly into the context, bypassing RequireJWT.
func withClaims(r *http.Request, subject string) *http.Request {
	c := &keeperjwt.Claims{Subject: subject}
	return r.WithContext(middleware.InjectClaimsForTest(r.Context(), c))
}

// newChiRequest builds a request with chi URL params so chi.URLParam(r, key) in the
// handler works in a unit test without standing up the router.
func newChiRequest(method, path string, body *bytes.Reader, key, value string) *http.Request {
	var b *bytes.Reader
	if body != nil {
		b = body
	} else {
		b = bytes.NewReader(nil)
	}
	r := httptest.NewRequest(method, path, b)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(key, value)
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
	return r
}

// assertProblem checks the response is problem+json with the expected status and type.
// Shared helper for domains that still keep (w,r) tests (oracle/sigil-key/soul);
// extracted from sigil_test.go during the handler-native turn of sigil (T5d).
func assertProblem(t *testing.T, w *httptest.ResponseRecorder, wantStatus int, wantType string) {
	t.Helper()
	if w.Code != wantStatus {
		t.Fatalf("status = %d, want %d (body %s)", w.Code, wantStatus, w.Body.String())
	}
	var p problem.Details
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode problem: %v (body %s)", err, w.Body.String())
	}
	if p.Type != wantType {
		t.Errorf("problem.type = %q, want %q", p.Type, wantType)
	}
}
