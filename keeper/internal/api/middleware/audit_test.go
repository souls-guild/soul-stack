package middleware

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// captureWriter — audit.Writer-stub: захватывает все записанные события.
type captureWriter struct {
	mu     sync.Mutex
	events []*audit.Event
	err    error
}

func (c *captureWriter) Write(_ context.Context, ev *audit.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	// Копия, чтобы caller-овский Event не мутировался.
	cp := *ev
	c.events = append(c.events, &cp)
	return nil
}

func (c *captureWriter) Events() []*audit.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*audit.Event, len(c.events))
	copy(out, c.events)
	return out
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func TestAudit_WritesOnSuccess(t *testing.T) {
	w := &captureWriter{}
	called := false
	next := http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		called = true
		rw.WriteHeader(http.StatusOK)
	})
	builder := func(_ *http.Request, _ int) map[string]any {
		return map[string]any{"foo": "bar"}
	}
	h := Audit(w, audit.EventOperatorCreated, builder, discardLogger())(next)

	rec := httptest.NewRecorder()
	req := withClaims(httptest.NewRequest(http.MethodPost, "/v1/operators", nil), "archon-alice")
	h.ServeHTTP(rec, req)

	if !called {
		t.Errorf("next not called")
	}
	evs := w.Events()
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	if evs[0].EventType != audit.EventOperatorCreated {
		t.Errorf("EventType = %q", evs[0].EventType)
	}
	if evs[0].Source != audit.SourceAPI {
		t.Errorf("Source = %q, want %q", evs[0].Source, audit.SourceAPI)
	}
	if evs[0].ArchonAID != "archon-alice" {
		t.Errorf("ArchonAID = %q", evs[0].ArchonAID)
	}
	if evs[0].Payload["foo"] != "bar" {
		t.Errorf("payload = %v", evs[0].Payload)
	}
}

func TestAudit_SkipsOnNon2xx(t *testing.T) {
	w := &captureWriter{}
	next := http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusForbidden)
	})
	h := Audit(w, audit.EventOperatorCreated, nil, discardLogger())(next)
	rec := httptest.NewRecorder()
	req := withClaims(httptest.NewRequest(http.MethodPost, "/v1/operators", nil), "archon-alice")
	h.ServeHTTP(rec, req)

	if len(w.Events()) != 0 {
		t.Errorf("events on 403: %d, want 0", len(w.Events()))
	}
}

func TestAudit_MergesHandlerPayload(t *testing.T) {
	w := &captureWriter{}
	next := http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		SetAuditPayload(r, AuditPayload{"aid": "archon-bob", "display_name": "Bob"})
		rw.WriteHeader(http.StatusCreated)
	})
	builder := func(_ *http.Request, _ int) map[string]any {
		return map[string]any{"created_by_aid": "archon-alice"}
	}
	h := Audit(w, audit.EventOperatorCreated, builder, discardLogger())(next)

	rec := httptest.NewRecorder()
	req := withClaims(httptest.NewRequest(http.MethodPost, "/v1/operators", nil), "archon-alice")
	h.ServeHTTP(rec, req)

	evs := w.Events()
	if len(evs) != 1 {
		t.Fatalf("events = %d", len(evs))
	}
	p := evs[0].Payload
	if p["created_by_aid"] != "archon-alice" {
		t.Errorf("created_by_aid = %v", p["created_by_aid"])
	}
	if p["aid"] != "archon-bob" {
		t.Errorf("aid = %v", p["aid"])
	}
	if p["display_name"] != "Bob" {
		t.Errorf("display_name = %v", p["display_name"])
	}
}

func TestAudit_NoClaimsSkips(t *testing.T) {
	w := &captureWriter{}
	next := http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusOK)
	})
	h := Audit(w, audit.EventOperatorCreated, nil, discardLogger())(next)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/operators", nil) // без claims
	h.ServeHTTP(rec, req)
	if len(w.Events()) != 0 {
		t.Errorf("event written without claims: %d", len(w.Events()))
	}
}

func TestAudit_StatusRecorderImplicit200(t *testing.T) {
	// handler ничего не вызвал у WriteHeader — Write() из stdlib делает 200.
	w := &captureWriter{}
	next := http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		_, _ = rw.Write([]byte("ok"))
	})
	h := Audit(w, audit.EventOperatorCreated, nil, discardLogger())(next)
	rec := httptest.NewRecorder()
	req := withClaims(httptest.NewRequest(http.MethodPost, "/v1/operators", nil), "archon-x")
	h.ServeHTTP(rec, req)
	if len(w.Events()) != 1 {
		t.Errorf("implicit 200 should still trigger audit; got %d events", len(w.Events()))
	}
}
