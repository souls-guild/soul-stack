package middleware

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
)

const testIssuer = "keeper.test"

var testSigningKey = bytes.Repeat([]byte{0xab}, 32)

func newVerifier(t *testing.T) *keeperjwt.Verifier {
	t.Helper()
	v, err := keeperjwt.NewVerifier(testSigningKey, testIssuer)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v
}

func newToken(t *testing.T, ttl time.Duration) string {
	t.Helper()
	iss, err := keeperjwt.NewIssuer(testSigningKey, testIssuer)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	tok, err := iss.Issue("archon-alice", []string{"cluster-admin"}, ttl, false)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return tok
}

// nextOK — handler, который проверяет, что middleware пропустил запрос
// и пишет 200. Используется для positive-case-ов.
func nextOK(t *testing.T, wantSubject string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, ok := ClaimsFromContext(r.Context())
		if !ok {
			t.Errorf("ClaimsFromContext: ok=false")
			http.Error(w, "no claims", http.StatusInternalServerError)
			return
		}
		if claims.Subject != wantSubject {
			t.Errorf("Subject = %q, want %q", claims.Subject, wantSubject)
		}
		w.WriteHeader(http.StatusOK)
	})
}

// nextShouldNotRun — handler, который должен НЕ вызываться (401-case).
func nextShouldNotRun(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("next handler called for a request that should have been rejected")
	})
}

func assertProblem(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantType string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Errorf("Code = %d, want %d", rec.Code, wantStatus)
	}
	if got := rec.Header().Get("Content-Type"); got != problem.ContentType {
		t.Errorf("Content-Type = %q, want %q", got, problem.ContentType)
	}
	var p problem.Details
	if err := json.NewDecoder(rec.Body).Decode(&p); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if p.Type != wantType {
		t.Errorf("Type = %q, want %q", p.Type, wantType)
	}
	if p.Status != wantStatus {
		t.Errorf("body status = %d, want %d", p.Status, wantStatus)
	}
}

func TestRequireJWT_NoAuthHeader(t *testing.T) {
	v := newVerifier(t)
	h := RequireJWT(v)(nextShouldNotRun(t))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/anything", nil)
	h.ServeHTTP(rec, req)
	assertProblem(t, rec, http.StatusUnauthorized, problem.TypeUnauthenticated)
}

func TestRequireJWT_MalformedHeader(t *testing.T) {
	v := newVerifier(t)
	h := RequireJWT(v)(nextShouldNotRun(t))
	cases := []string{
		"Token abc",
		"Bearer",
		"Bearer ",
		"abc",
	}
	for _, hv := range cases {
		t.Run(hv, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
			req.Header.Set("Authorization", hv)
			h.ServeHTTP(rec, req)
			assertProblem(t, rec, http.StatusUnauthorized, problem.TypeUnauthenticated)
		})
	}
}

func TestRequireJWT_ValidToken(t *testing.T) {
	v := newVerifier(t)
	tok := newToken(t, time.Hour)
	h := RequireJWT(v)(nextOK(t, "archon-alice"))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200", rec.Code)
	}
}

func TestRequireJWT_BearerCaseInsensitive(t *testing.T) {
	v := newVerifier(t)
	tok := newToken(t, time.Hour)
	h := RequireJWT(v)(nextOK(t, "archon-alice"))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Authorization", "bearer "+tok)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200", rec.Code)
	}
}

func TestRequireJWT_BadToken(t *testing.T) {
	v := newVerifier(t)
	h := RequireJWT(v)(nextShouldNotRun(t))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Authorization", "Bearer not-a-jwt")
	h.ServeHTTP(rec, req)
	assertProblem(t, rec, http.StatusUnauthorized, problem.TypeUnauthenticated)
}

// TestRevokedJWT_Returns401 — ADR-014 Amendment 2026-05-27: JWT валиден по
// подписи и exp, но AID Архонта попал в Snapshot.Revoked → RequirePermission
// возвращает 401 с problem-detail `operator-revoked-token` (parity с
// expired). НЕ 403 — токен больше не доверенный.
func TestRevokedJWT_Returns401(t *testing.T) {
	v := newVerifier(t)
	tok := newToken(t, time.Hour)

	// Enforcer с ролью `*` для archon-alice + revoke-меткой на тот же AID:
	// моделирует «уволенный сотрудник с ещё активной ролью в каталоге».
	revokedAt := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)
	e, err := rbactest.NewEnforcer(&rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "cluster-admin", Operators: []string{"archon-alice"}, Permissions: []string{"*"}},
		},
		Revoked: map[string]time.Time{"archon-alice": revokedAt},
	})
	if err != nil {
		t.Fatalf("rbactest.NewEnforcer: %v", err)
	}

	chain := RequireJWT(v)(RequirePermission(e, "operator", "create", NoSelector)(nextShouldNotRun(t)))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/operators", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	chain.ServeHTTP(rec, req)

	assertProblem(t, rec, http.StatusUnauthorized, problem.TypeOperatorRevokedToken)
}

// TestRevokedJWT_Returns401_MultiSelector — то же, что TestRevokedJWT_Returns401,
// но через RequirePermissionMulti (используется на /incarnations/{name}-роутах).
// Revoke-short-circuit-проверка должна сработать на первой же итерации, не
// деградировать до 403.
func TestRevokedJWT_Returns401_MultiSelector(t *testing.T) {
	v := newVerifier(t)
	tok := newToken(t, time.Hour)

	e, err := rbactest.NewEnforcer(&rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "cluster-admin", Operators: []string{"archon-alice"}, Permissions: []string{"*"}},
		},
		Revoked: map[string]time.Time{"archon-alice": time.Now()},
	})
	if err != nil {
		t.Fatalf("rbactest.NewEnforcer: %v", err)
	}

	multi := func(_ *http.Request) []map[string]string {
		return []map[string]string{
			{"service": "redis"},
			{"service": "vault"},
		}
	}
	chain := RequireJWT(v)(RequirePermissionMulti(e, "incarnation", "create", multi)(nextShouldNotRun(t)))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	chain.ServeHTTP(rec, req)

	assertProblem(t, rec, http.StatusUnauthorized, problem.TypeOperatorRevokedToken)
}

// TestSSE_QueryToken_Valid_200 — EventSource не умеет custom-заголовки,
// поэтому SSE-канал принимает JWT в query-param `access_token`. Валидный
// токен на `GET .../events?access_token=<jwt>` → 200 + claims в context.
func TestSSE_QueryToken_Valid_200(t *testing.T) {
	v := newVerifier(t)
	tok := newToken(t, time.Hour)
	h := RequireJWT(v)(nextOK(t, "archon-alice"))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/voyages/run-1/events?access_token="+tok, nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200", rec.Code)
	}
}

// TestSSE_QueryToken_Valid_200_AcceptHeader — SSE-канал распознаётся также
// по `Accept: text/event-stream` (путь без суффикса /events).
func TestSSE_QueryToken_Valid_200_AcceptHeader(t *testing.T) {
	v := newVerifier(t)
	tok := newToken(t, time.Hour)
	h := RequireJWT(v)(nextOK(t, "archon-alice"))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/voyages/run-1/stream?access_token="+tok, nil)
	req.Header.Set("Accept", "text/event-stream")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200", rec.Code)
	}
}

// TestSSE_QueryToken_Invalid_401 — невалидный query-token на SSE-канале →
// 401 (тот же verifier, что и для Bearer).
func TestSSE_QueryToken_Invalid_401(t *testing.T) {
	v := newVerifier(t)
	h := RequireJWT(v)(nextShouldNotRun(t))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/voyages/run-1/events?access_token=not-a-jwt", nil)
	h.ServeHTTP(rec, req)
	assertProblem(t, rec, http.StatusUnauthorized, problem.TypeUnauthenticated)
}

// TestSSE_QueryToken_OnNonSSE_Ignored — query-token на обычном GET (не
// /events, без Accept SSE) НЕ принимается: токен в URL разрешён строго для
// потокового канала. Валидный JWT в query → всё равно 401.
func TestSSE_QueryToken_OnNonSSE_Ignored(t *testing.T) {
	v := newVerifier(t)
	tok := newToken(t, time.Hour)
	h := RequireJWT(v)(nextShouldNotRun(t))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/voyages/run-1?access_token="+tok, nil)
	h.ServeHTTP(rec, req)
	assertProblem(t, rec, http.StatusUnauthorized, problem.TypeUnauthenticated)
}

// TestSSE_QueryToken_OnPost_Ignored — query-token на mutating-методе (POST)
// игнорируется даже с валидным JWT: query-token только для GET + SSE.
func TestSSE_QueryToken_OnPost_Ignored(t *testing.T) {
	v := newVerifier(t)
	tok := newToken(t, time.Hour)
	h := RequireJWT(v)(nextShouldNotRun(t))
	rec := httptest.NewRecorder()
	// POST на путь с /events-суффиксом + валидный токен в query — всё равно 401.
	req := httptest.NewRequest(http.MethodPost, "/v1/voyages/run-1/events?access_token="+tok, nil)
	h.ServeHTTP(rec, req)
	assertProblem(t, rec, http.StatusUnauthorized, problem.TypeUnauthenticated)
}

// TestSSE_BearerStillWorks — обычный `Authorization: Bearer` на SSE-канале
// продолжает работать (header имеет приоритет над query-param).
func TestSSE_BearerStillWorks(t *testing.T) {
	v := newVerifier(t)
	tok := newToken(t, time.Hour)
	h := RequireJWT(v)(nextOK(t, "archon-alice"))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/voyages/run-1/events", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200", rec.Code)
	}
}

func TestClaimsFromContext_Missing(t *testing.T) {
	if _, ok := ClaimsFromContext(testRequestContext()); ok {
		t.Errorf("ClaimsFromContext on bare context: ok=true, want false")
	}
}

func testRequestContext() (ctx contextLike) {
	return httptest.NewRequest(http.MethodGet, "/", nil).Context()
}

// contextLike — alias для context.Context (импорт через test-helper, чтобы
// тестовый код оставался декларативным).
type contextLike = interface {
	Deadline() (time.Time, bool)
	Done() <-chan struct{}
	Err() error
	Value(any) any
}
