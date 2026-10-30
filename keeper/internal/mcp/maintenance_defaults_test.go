package mcp

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/maintenance"
	"github.com/souls-guild/soul-stack/keeper/internal/maintenance/maintenancetest"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
)

// ★ The MCP readers hold an incarnation without windows of its own to the cluster
// default exactly as to the same slots as its own windows (NIM-1138), as REST
// does: keeper.incarnation.run in the window, and the status of
// keeper.incarnation.maintenance-window.list, which also shows the default and
// its source.
func TestMCPReaders_DefaultWindowsHoldAsOwnWindows(t *testing.T) {
	slot, _ := maintenancetest.Slot(time.Now())
	rbac := &rbactest.Config{Roles: []rbactest.Role{{Name: "runner", Operators: []string{"archon-alice"},
		Permissions: []string{"incarnation.run", "incarnation.get"}}}}

	run, status := map[string]any{}, map[string]any{}
	for _, m := range maintenancetest.Modes(slot) {
		pool := &fakePool{incFn: incWithStatus(incarnation.StatusReady), windows: m.Own}
		h, _ := newTestHandlerFull(t, pool, rbac, &mcpStarter{}, &mcpResolver{ok: true}, nil)
		h.deps.MaintenanceDefaults = m.Defaults

		var out incarnationRunOutput
		structured(t, callTool(t, h, "archon-alice", "keeper.incarnation.run",
			`{"id":"redis-prod","scenario":"rotate","in_maintenance_window":true}`), &out)
		run[m.Name] = out.ScheduleAt
		if out.ScheduleAt != nil {
			run[m.Name] = out.ScheduleAt.UTC().String()
		}

		var list maintenanceWindowListOutput
		structured(t, callTool(t, h, "archon-alice", "keeper.incarnation.maintenance-window.list", `{"id":"redis-prod"}`), &list)
		status[m.Name] = [2]any{list.Status.OpenNow, list.Status.NextStart}

		wantSource, wantShown := map[string]string{
			maintenancetest.ByDefault: maintenance.SourceDefault, maintenancetest.AsOwn: maintenance.SourceOwn,
			maintenancetest.Nowhere: maintenance.SourceNone,
		}[m.Name], 0
		if m.Name == maintenancetest.ByDefault {
			wantShown = 1
		}
		if list.Status.Source != wantSource || len(list.Defaults) != wantShown {
			t.Errorf("%s: source %q with %d defaults, want %q with %d", m.Name, list.Status.Source, len(list.Defaults), wantSource, wantShown)
		}
	}
	maintenancetest.Agree(t, "keeper.incarnation.run in the window", run)
	maintenancetest.Agree(t, "the maintenance-window.list status", status)
}

func structured(t *testing.T, resp jsonRPCResponse, out any) {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("tool error: %+v", resp.Error)
	}
	var res toolsCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(res.StructuredContent, out); err != nil {
		t.Fatalf("structured content: %v", err)
	}
}
