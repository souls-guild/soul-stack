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

// Пакетные test-helper-ы handlers-домена. Извлечены из operator_test.go при
// handler-native-развороте operator (T5d): operator больше не несёт (w,r)-тесты,
// helper-ы остаются общими для остальных доменов (augur/oracle/role/soul/… —
// (w,r)-handler-ы которых ещё на месте).

// withClaims кладёт keeperjwt.Claims в context напрямую, минуя RequireJWT.
func withClaims(r *http.Request, subject string) *http.Request {
	c := &keeperjwt.Claims{Subject: subject}
	return r.WithContext(middleware.InjectClaimsForTest(r.Context(), c))
}

// newChiRequest строит request с chi-URL-params, чтобы chi.URLParam(r, key) в
// handler-е работал в unit-тесте без поднятия роутера.
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

// assertProblem проверяет, что ответ — problem+json с ожидаемыми статусом и type.
// Общий helper для доменов с ещё-сохранёнными (w,r)-тестами (oracle/sigil-key/soul);
// извлечён из sigil_test.go при handler-native-развороте sigil (T5d).
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
