package voyage

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/maintenance/maintenancetest"
)

// ★ The claim's start gate holds an incarnation without windows of its own to the
// cluster default exactly as it would hold it to the same slots as its own
// windows (NIM-1138).
func TestMaintenanceGate_DefaultWindowsHoldAsOwnWindows(t *testing.T) {
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	slot, _ := maintenancetest.Slot(now)
	ids, _ := json.Marshal([]string{maintenancetest.Incarnation})
	answers := map[string]any{}
	for _, m := range maintenancetest.Modes(slot) {
		v := &Voyage{TargetResolved: ids, InMaintenanceWindow: true}
		verdict, err := MaintenanceGate{Defaults: m.Defaults}.Decide(context.Background(), &maintenancetest.DB{Windows: m.Own}, v, now)
		if err != nil {
			t.Fatalf("%s: Decide: %v", m.Name, err)
		}
		answers[m.Name] = verdict.DeferTo.UTC().String() + " " + verdict.Reason
	}
	maintenancetest.Agree(t, "the claim's start gate", answers)

	// A run that did not ask for the window starts now under every mode.
	for _, m := range maintenancetest.Modes(slot) {
		v := &Voyage{TargetResolved: ids}
		verdict, err := MaintenanceGate{Defaults: m.Defaults}.Decide(context.Background(), &maintenancetest.DB{Windows: m.Own}, v, now)
		if err != nil || !verdict.DeferTo.IsZero() || verdict.NoStart {
			t.Errorf("%s: a run without in_maintenance_window got %+v, %v; want it to start now", m.Name, verdict, err)
		}
	}
}
