package scenario

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/maintenance/maintenancetest"
)

// ★ The scenario's `in_maintenance_window()` and `incarnation.maintenance_window.*`
// read the cluster default exactly as the same slots as the incarnation's own
// windows (NIM-1138), on an existing incarnation and on the create path, where
// the incarnation has no row yet and so the default alone.
func TestMaintenanceAt_DefaultWindowsRenderAsOwnWindows(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	slot, _ := maintenancetest.Slot(time.Now())

	pass := map[string]any{}
	create := map[string]any{}
	for _, m := range maintenancetest.Modes(slot) {
		mw := maintenanceAt(context.Background(), &maintenancetest.DB{Windows: m.Own}, m.Defaults, maintenancetest.Incarnation, log)
		if mw == nil {
			t.Fatalf("%s: no window rendered", m.Name)
		}
		pass[m.Name] = *mw

		if m.Name == maintenancetest.AsOwn {
			// An incarnation being created has no windows of its own: its
			// answer under its own windows is the existing incarnation's.
			create[m.Name] = *mw
			continue
		}
		r := &Runner{deps: Deps{MaintenanceDefaults: m.Defaults}, logger: log}
		pre := r.preflightMaintenance(context.Background(), nil)
		if pre == nil {
			t.Fatalf("%s: no pre-flight window on the create path", m.Name)
		}
		create[m.Name] = *pre
	}
	maintenancetest.Agree(t, "the render pass's maintenance window", pass)
	maintenancetest.Agree(t, "the create path's pre-flight window", create)
}
