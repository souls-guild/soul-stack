package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
)

func TestWriteUnauthenticated(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/foo", nil)
	WriteUnauthenticated(rec, req, "no token")

	checkProblem(t, rec, http.StatusUnauthorized, problem.TypeUnauthenticated, "/v1/foo", "no token")
}

func TestWriteForbidden(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/operators", nil)
	WriteForbidden(rec, req, "missing permission operator.create")

	checkProblem(t, rec, http.StatusForbidden, problem.TypeForbidden, "/v1/operators",
		"missing permission operator.create")
}

func TestWriteNotFound(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/foo", nil)
	WriteNotFound(rec, req, "no such resource")

	checkProblem(t, rec, http.StatusNotFound, problem.TypeNotFound, "/v1/foo", "no such resource")
}

func TestWriteMalformed(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/foo", nil)
	WriteMalformed(rec, req, "invalid JSON")

	checkProblem(t, rec, http.StatusBadRequest, problem.TypeMalformedRequest, "/v1/foo", "invalid JSON")
}

func TestWriteInternal(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/foo", nil)
	WriteInternal(rec, req)

	checkProblem(t, rec, http.StatusInternalServerError, problem.TypeInternalError, "/v1/foo", "")
}

func checkProblem(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantType, wantInstance, wantDetailContains string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Errorf("Code = %d, want %d", rec.Code, wantStatus)
	}
	if got := rec.Header().Get("Content-Type"); got != problem.ContentType {
		t.Errorf("Content-Type = %q, want %q", got, problem.ContentType)
	}
	var p problem.Details
	if err := json.NewDecoder(rec.Body).Decode(&p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.Type != wantType {
		t.Errorf("Type = %q, want %q", p.Type, wantType)
	}
	if p.Instance != wantInstance {
		t.Errorf("Instance = %q, want %q", p.Instance, wantInstance)
	}
	if wantDetailContains != "" && p.Detail != wantDetailContains {
		t.Errorf("Detail = %q, want %q", p.Detail, wantDetailContains)
	}
}
