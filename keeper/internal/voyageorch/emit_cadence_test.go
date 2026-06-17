package voyageorch

import (
	"context"
	"sync"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// capturingWriter — захватывает записанные audit-события для проверки payload.
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

// TestEmitFinalized_CadenceID — ядро фикса (ADR-052 §l amend, Вариант б):
// emitFinalized несёт cadence_id в payload терминала, когда Voyage спавнен
// расписанием (run.CadenceID != nil), и НЕ несёт для ручного Voyage (nil).
func TestEmitFinalized_CadenceID(t *testing.T) {
	const cadenceULID = "01JZZZZ0CADENCE000000000000"

	cases := []struct {
		name          string
		kind          voyage.Kind
		status        voyage.Status
		cadenceID     *string
		wantEvent     audit.EventType
		wantCadenceID string // "" → ключ отсутствует
	}{
		{"scenario completed by cadence", voyage.KindScenario, voyage.StatusSucceeded, strPtr(cadenceULID), audit.EventScenarioRunCompleted, cadenceULID},
		{"scenario failed by cadence", voyage.KindScenario, voyage.StatusFailed, strPtr(cadenceULID), audit.EventScenarioRunFailed, cadenceULID},
		{"scenario partial by cadence", voyage.KindScenario, voyage.StatusPartialFailed, strPtr(cadenceULID), audit.EventScenarioRunPartialFailed, cadenceULID},
		{"command completed by cadence", voyage.KindCommand, voyage.StatusSucceeded, strPtr(cadenceULID), audit.EventCommandRunCompleted, cadenceULID},
		{"command failed by cadence", voyage.KindCommand, voyage.StatusFailed, strPtr(cadenceULID), audit.EventCommandRunFailed, cadenceULID},
		// Ручной Voyage (CadenceID nil) — ключ cadence_id отсутствует.
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
				t.Fatal("emitFinalized не записал событие")
			}
			if ev.EventType != c.wantEvent {
				t.Fatalf("event_type = %q, want %q", ev.EventType, c.wantEvent)
			}
			got, present := ev.Payload["cadence_id"]
			if c.wantCadenceID == "" {
				if present {
					t.Fatalf("ручной Voyage не должен нести cadence_id, получил %v", got)
				}
				return
			}
			if !present {
				t.Fatal("cadence-Voyage должен нести cadence_id в payload терминала")
			}
			if got != c.wantCadenceID {
				t.Fatalf("cadence_id = %v, want %q", got, c.wantCadenceID)
			}
		})
	}
}
