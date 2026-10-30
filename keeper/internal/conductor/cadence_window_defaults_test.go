package conductor

import (
	"context"
	"fmt"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/cadence"
	"github.com/souls-guild/soul-stack/keeper/internal/maintenance"
	"github.com/souls-guild/soul-stack/keeper/internal/maintenance/maintenancetest"
)

// ★ The Cadence tick holds a child to the cluster default exactly as to the same
// slots as the incarnation's own windows (NIM-1138), through the spawner's own
// gate: the deferred start, and the skip with its list when the targets' windows
// never meet — where the other target's own weekly window still replaces the
// default for it.
func TestProcessOne_DefaultWindowsHoldTheSpawnAsOwnWindows(t *testing.T) {
	now := nov(2, 14, 0)
	slot, _ := maintenancetest.Slot(now)

	deferred := map[string]any{}
	skipped := map[string]any{}
	for _, m := range maintenancetest.Modes(slot) {
		tx := &spawnFakeTx{windows: m.Own}
		s := newSpawnerFor(tx, stubResolver{out: []string{maintenancetest.Incarnation}}, nil)
		s.SetMaintenanceDefaults(m.Defaults)
		_, spawned, err := s.processOne(context.Background(), tx, cronScenarioCadence(cadence.OverlapPolicySkip, true), now)
		if err != nil || !spawned {
			t.Fatalf("%s: spawned=%v err=%v", m.Name, spawned, err)
		}
		scheduleAt, status := insertedStart(t, tx)
		deferred[m.Name] = fmt.Sprint(scheduleAt, " ", status)

		tx = &spawnFakeTx{windows: append([]maintenance.Window{window("redis-a", []string{"mon"}, "02:00", 60)}, m.Own...)}
		s = newSpawnerFor(tx, stubResolver{out: []string{"redis-a", maintenancetest.Incarnation}}, nil)
		s.SetMaintenanceDefaults(m.Defaults)
		rec, spawned, err := s.processOne(context.Background(), tx, cronScenarioCadence(cadence.OverlapPolicyParallel, true), now)
		if err != nil {
			t.Fatalf("%s: processOne: %v", m.Name, err)
		}
		var rows any
		if rec != nil && rec.noWindow != nil {
			rows = rec.noWindow.rows
		}
		skipped[m.Name] = fmt.Sprint(spawned, rows)
	}
	maintenancetest.Agree(t, "the Cadence spawn's start", deferred)

	// A recipe without in_maintenance_window is held to no window, the default
	// included: its child starts at once.
	tx := &spawnFakeTx{}
	s := newSpawnerFor(tx, stubResolver{out: []string{maintenancetest.Incarnation}}, nil)
	s.SetMaintenanceDefaults(maintenancetest.Modes(slot)[0].Defaults)
	if _, spawned, err := s.processOne(context.Background(), tx, cronScenarioCadence(cadence.OverlapPolicySkip, false), now); err != nil || !spawned {
		t.Fatalf("spawned=%v err=%v", spawned, err)
	}
	if scheduleAt, status := insertedStart(t, tx); scheduleAt != nil || status != "pending" {
		t.Errorf("a recipe without the flag under the default: schedule_at=%v status=%s, want none, pending", scheduleAt, status)
	}
	maintenancetest.Agree(t, "cadence.skipped_no_window", skipped)
}
