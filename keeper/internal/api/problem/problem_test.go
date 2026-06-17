package problem

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestNew_FillsTitleAndStatus(t *testing.T) {
	p := New(TypeUnauthenticated, "/v1/foo", "missing Authorization")
	if p.Title != "Authentication required" {
		t.Errorf("Title = %q", p.Title)
	}
	if p.Status != 401 {
		t.Errorf("Status = %d, want 401", p.Status)
	}
	if p.Instance != "/v1/foo" {
		t.Errorf("Instance = %q", p.Instance)
	}
	if p.Detail != "missing Authorization" {
		t.Errorf("Detail = %q", p.Detail)
	}
	if p.Type != TypeUnauthenticated {
		t.Errorf("Type = %q", p.Type)
	}
}

func TestWrite_SetsContentType(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, New(TypeNotFound, "/v1/incarnations/x", "no such incarnation"))

	if got, want := rec.Header().Get("Content-Type"), ContentType; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	if rec.Code != 404 {
		t.Errorf("Code = %d, want 404", rec.Code)
	}

	var p Details
	if err := json.NewDecoder(rec.Body).Decode(&p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.Type != TypeNotFound || p.Status != 404 {
		t.Errorf("body shape: %+v", p)
	}
	// JSON не должен начинаться с BOM (0xEF 0xBB 0xBF).
	body := rec.Body.Bytes()
	if len(body) >= 3 && body[0] == 0xEF && body[1] == 0xBB && body[2] == 0xBF {
		t.Errorf("body has UTF-8 BOM")
	}
}

func TestWrite_AllKnownTypes(t *testing.T) {
	cases := []struct {
		t      string
		status int
	}{
		{TypeUnauthenticated, 401},
		{TypeForbidden, 403},
		{TypeNotFound, 404},
		{TypeMalformedRequest, 400},
		{TypeValidationFailed, 422},
		{TypeInternalError, 500},
		{TypeOperatorExists, 409},
		{TypeOperatorRevoked, 409},
		{TypeWouldLockOutCluster, 409},
	}
	for _, c := range cases {
		t.Run(c.t, func(t *testing.T) {
			rec := httptest.NewRecorder()
			Write(rec, New(c.t, "/v1/x", ""))
			if rec.Code != c.status {
				t.Errorf("Code = %d, want %d", rec.Code, c.status)
			}
			if rec.Header().Get("Content-Type") != ContentType {
				t.Errorf("Content-Type = %q", rec.Header().Get("Content-Type"))
			}
		})
	}
}
