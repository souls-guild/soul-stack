package toll

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// fakeDegradedReader — controllable [DegradedReader] for middleware tests.
type fakeDegradedReader struct {
	degraded bool
	err      error
}

func (f *fakeDegradedReader) IsDegraded(_ context.Context) (bool, error) {
	return f.degraded, f.err
}

func TestDegradedMiddleware_Passthrough_WhenNotDegraded(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})
	mw := DegradedMiddleware(&fakeDegradedReader{degraded: false}, newTestLogger())(next)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations/foo/scenarios/run", nil)
	mw.ServeHTTP(rec, req)
	if !called {
		t.Fatal("next handler not called when !degraded")
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected %d, got %d", http.StatusNoContent, rec.Code)
	}
}

func TestDegradedMiddleware_Blocks503_WhenDegraded(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
	})
	mw := DegradedMiddleware(&fakeDegradedReader{degraded: true}, newTestLogger())(next)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/push/apply", nil)
	mw.ServeHTTP(rec, req)
	if called {
		t.Fatal("next handler should not be called when degraded")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Fatal("expected Retry-After header")
	} else if _, err := strconv.Atoi(got); err != nil {
		t.Fatalf("Retry-After should be number, got %q", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("expected Content-Type application/problem+json, got %q", ct)
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("body decode: %v", err)
	}
	if body["type"] != "https://soul-stack.com/errors/cluster-degraded" {
		t.Fatalf("expected type cluster-degraded, got %v", body["type"])
	}
	if body["status"].(float64) != 503 {
		t.Fatalf("expected status 503 in body, got %v", body["status"])
	}
}

func TestDegradedMiddleware_ReaderError_FailOpen(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	mw := DegradedMiddleware(&fakeDegradedReader{err: errors.New("redis down")}, newTestLogger())(next)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations/foo/scenarios/run", nil)
	mw.ServeHTTP(rec, req)
	if !called {
		t.Fatal("on reader-error expected fail-open (next called)")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 fail-open, got %d", rec.Code)
	}
}

func TestDegradedMiddleware_NilReader_Passthrough(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusAccepted)
	})
	mw := DegradedMiddleware(nil, newTestLogger())(next)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/push/apply", nil)
	mw.ServeHTTP(rec, req)
	if !called {
		t.Fatal("nil-reader → middleware should be no-op")
	}
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", rec.Code)
	}
}

func TestNoopDegradedReader_AlwaysFalse(t *testing.T) {
	r := NoopDegradedReader{}
	got, err := r.IsDegraded(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got {
		t.Fatal("NoopDegradedReader should return false")
	}
}
