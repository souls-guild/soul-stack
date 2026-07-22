package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// D1: an MCP-initiated Voyage must write audit source=mcp, not api. The source is
// threaded through ctx (middleware.WithScenarioInvocationSource); the REST handler
// Create/Cancel reads it in emitCreated/emitCancelled. A normal HTTP request (ctx
// without the key) keeps the default api — Operator-API behavior is unchanged.

// captureAudit is an [audit.Writer] mock accumulating written events (the base
// newVoyageHandler passes a nil writer; source checks need a real one).
type captureAudit struct {
	mu     sync.Mutex
	events []*audit.Event
}

func (c *captureAudit) Write(_ context.Context, ev *audit.Event) error {
	c.mu.Lock()
	c.events = append(c.events, ev)
	c.mu.Unlock()
	return nil
}

func newVoyageHandlerWithAudit(store *fakeVoyageStore, sc VoyageScenarioResolver, cmd VoyageCommandResolver, enf middleware.PermissionChecker, aw audit.Writer) *VoyageHandler {
	return NewVoyageHandler(store, sc, cmd, nil, enf, nil /*scoper*/, aw, nil /*tidingInvalidator*/, 0 /*maxScope*/, 0 /*maxBatchSize → unlimited*/, nil)
}

func TestVoyageCreate_Scenario_AuditSourceMCP(t *testing.T) {
	aw := &captureAudit{}
	h := newVoyageHandlerWithAudit(&fakeVoyageStore{}, &fakeVoyageScenarioResolver{out: []string{"inc-a"}}, &fakeVoyageCommandResolver{}, allowAll(), aw)

	r := voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"scenario","scenario_name":"deploy","target":{"service":"web"}}`)
	r = r.WithContext(middleware.WithScenarioInvocationSource(r.Context(), audit.SourceMCP))
	rec := httptest.NewRecorder()
	h.Create(rec, r)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if len(aw.events) != 1 || aw.events[0].EventType != audit.EventScenarioRunStarted {
		t.Fatalf("want 1 scenario-started event, got %+v", aw.events)
	}
	if aw.events[0].Source != audit.SourceMCP {
		t.Errorf("source = %q, want mcp", aw.events[0].Source)
	}
}

func TestVoyageCreate_Command_AuditSourceMCP(t *testing.T) {
	aw := &captureAudit{}
	h := newVoyageHandlerWithAudit(&fakeVoyageStore{}, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{out: []string{"host-a"}}, allowAll(), aw)

	r := voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"command","module":"core.cmd.shell","target":{"sids":["host-a"]}}`)
	r = r.WithContext(middleware.WithScenarioInvocationSource(r.Context(), audit.SourceMCP))
	rec := httptest.NewRecorder()
	h.Create(rec, r)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if len(aw.events) != 1 || aw.events[0].EventType != audit.EventCommandRunInvoked {
		t.Fatalf("want 1 command-invoked event, got %+v", aw.events)
	}
	if aw.events[0].Source != audit.SourceMCP {
		t.Errorf("source = %q, want mcp", aw.events[0].Source)
	}
}

func TestVoyageCancel_AuditSourceMCP(t *testing.T) {
	id := "01HF7Z5G8Q5KQ8X7Y2N3R4M5P6"
	store := &fakeVoyageStore{
		selectByID: func(string) pgx.Row {
			return voyageFullRow{vals: voyageRowVals(id, voyage.KindScenario, voyage.StatusPending)}
		},
	}
	aw := &captureAudit{}
	h := newVoyageHandlerWithAudit(store, &fakeVoyageScenarioResolver{}, &fakeVoyageCommandResolver{}, allowAll(), aw)

	r := voyageReqID(http.MethodDelete, "/v1/voyages/"+id, id, "")
	r = r.WithContext(middleware.WithScenarioInvocationSource(r.Context(), audit.SourceMCP))
	rec := httptest.NewRecorder()
	h.Cancel(rec, r)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if len(aw.events) != 1 || aw.events[0].EventType != audit.EventScenarioRunCancelled {
		t.Fatalf("want 1 scenario-cancelled event, got %+v", aw.events)
	}
	if aw.events[0].Source != audit.SourceMCP {
		t.Errorf("source = %q, want mcp", aw.events[0].Source)
	}
}

// TestVoyageCreate_AuditDefaultsToAPI — a normal HTTP request (ctx without the source
// key) keeps the default api: the D1 change doesn't alter Operator-API behavior.
func TestVoyageCreate_AuditDefaultsToAPI(t *testing.T) {
	aw := &captureAudit{}
	h := newVoyageHandlerWithAudit(&fakeVoyageStore{}, &fakeVoyageScenarioResolver{out: []string{"inc-a"}}, &fakeVoyageCommandResolver{}, allowAll(), aw)

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"scenario","scenario_name":"deploy","target":{"service":"web"}}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if len(aw.events) != 1 || aw.events[0].Source != audit.SourceAPI {
		t.Fatalf("source must default to api, got %+v", aw.events)
	}
}
