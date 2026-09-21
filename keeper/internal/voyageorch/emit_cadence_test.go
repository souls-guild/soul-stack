package voyageorch

import (
	"context"
	"sync"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// capturingWriter — captures written audit events for payload verification.
type capturingWriter struct {
	mu     sync.Mutex
	events []*audit.Event
}

func (w *capturingWriter) Write(_ context.Context, ev *audit.Event) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.events = append(w.events, ev)
	return nil
}

func (w *capturingWriter) last() *audit.Event {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.events) == 0 {
		return nil
	}
	return w.events[len(w.events)-1]
}

func strPtr(s string) *string { return &s }

// TestEmitFinalized_CadenceID — core of fix (ADR-052 §l amend, Variant b):
// emitFinalized carries cadence_id in terminal payload when Voyage spawned by
// schedule (run.CadenceID != nil), and does NOT carry for manual Voyage (nil).
func TestEmitFinalized_CadenceID(t *testing.T) {
	const cadenceULID = "01JZZZZ0CADENCE000000000000"

	cases := []struct {
		name          string
		kind          voyage.Kind
		status        voyage.Status
		cadenceID     *string
		wantEvent     audit.EventType
		wantCadenceID string // "" → key absent
	}{
		{"scenario completed by cadence", voyage.KindScenario, voyage.StatusSucceeded, strPtr(cadenceULID), audit.EventScenarioRunCompleted, cadenceULID},
		{"scenario failed by cadence", voyage.KindScenario, voyage.StatusFailed, strPtr(cadenceULID), audit.EventScenarioRunFailed, cadenceULID},
		{"scenario partial by cadence", voyage.KindScenario, voyage.StatusPartialFailed, strPtr(cadenceULID), audit.EventScenarioRunPartialFailed, cadenceULID},
		{"command completed by cadence", voyage.KindCommand, voyage.StatusSucceeded, strPtr(cadenceULID), audit.EventCommandRunCompleted, cadenceULID},
		{"command failed by cadence", voyage.KindCommand, voyage.StatusFailed, strPtr(cadenceULID), audit.EventCommandRunFailed, cadenceULID},
		// Manual Voyage (CadenceID nil) — key cadence_id absent.
		{"scenario manual", voyage.KindScenario, voyage.StatusSucceeded, nil, audit.EventScenarioRunCompleted, ""},
		{"command manual", voyage.KindCommand, voyage.StatusSucceeded, nil, audit.EventCommandRunCompleted, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cw := &capturingWriter{}
			w := &VoyageWorker{KID: "kid-1", Logger: quietLogger(), Audit: cw}
			run := &voyage.Voyage{
				VoyageID:     "vy_1",
				Kind:         c.kind,
				TotalBatches: 1,
				CadenceID:    c.cadenceID,
			}
			summary := &voyage.Summary{Total: 3, Succeeded: 3}

			w.emitFinalized(run, c.status, summary, "")

			ev := cw.last()
			if ev == nil {
				t.Fatal("emitFinalized did not write event")
			}
			if ev.EventType != c.wantEvent {
				t.Fatalf("event_type = %q, want %q", ev.EventType, c.wantEvent)
			}
			got, present := ev.Payload["cadence_id"]
			if c.wantCadenceID == "" {
				if present {
					t.Fatalf("manual Voyage must not carry cadence_id, got %v", got)
				}
				return
			}
			if !present {
				t.Fatal("cadence-Voyage must carry cadence_id in terminal payload")
			}
			if got != c.wantCadenceID {
				t.Fatalf("cadence_id = %v, want %q", got, c.wantCadenceID)
			}
		})
	}
}
