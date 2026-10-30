package mcp

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
)

// TestToolsCall_MaintenanceWindow_KindsCannotBeMixed is the MCP half of the
// refusal REST answers 422 (NIM-1137): the same field is named, with
// validation-failed, on create and on update, and nothing is written.
func TestToolsCall_MaintenanceWindow_KindsCannotBeMixed(t *testing.T) {
	pool := &fakePool{incFn: incWithStatus(incarnation.StatusReady)}
	h, rec := newTestHandlerFull(t, pool, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "maint", Operators: []string{"archon-alice"}, Permissions: []string{"incarnation.maintenance-set"}},
	}}, nil, nil, nil)

	for name, tc := range map[string]struct {
		tool, args, field string
	}{
		"create once with days": {"keeper.incarnation.maintenance-window.create",
			`{"id":"redis-prod","kind":"once","starts_at":"2026-11-05T22:00:00Z","ends_at":"2026-11-06T04:00:00Z","days":["mon"]}`, "field 'days'"},
		"create weekly with ends_at": {"keeper.incarnation.maintenance-window.create",
			`{"id":"redis-prod","days":["mon"],"start_time":"02:00","duration_minutes":60,"timezone":"UTC","ends_at":"2026-11-06T04:00:00Z"}`, "field 'ends_at'"},
		"create once with an empty timezone": {"keeper.incarnation.maintenance-window.create",
			`{"id":"redis-prod","kind":"once","starts_at":"2026-11-05T22:00:00Z","ends_at":"2026-11-06T04:00:00Z","timezone":""}`, "field 'timezone'"},
		"create weekly with empty days and a starts_at": {"keeper.incarnation.maintenance-window.create",
			`{"id":"redis-prod","kind":"weekly","starts_at":"","days":["mon"],"start_time":"02:00","duration_minutes":60,"timezone":"UTC"}`, "field 'starts_at'"},
		"update once with timezone": {"keeper.incarnation.maintenance-window.update",
			`{"id":"redis-prod","window_id":"0b9f7c1e-3d2a-4c5b-9e8f-1a2b3c4d5e6f","kind":"once","starts_at":"2026-11-05T22:00:00Z","ends_at":"2026-11-06T04:00:00Z","timezone":"UTC"}`, "field 'timezone'"},
	} {
		resp := callTool(t, h, "archon-alice", tc.tool, tc.args)
		if resp.Error == nil {
			t.Errorf("%s: accepted, want validation-failed", name)
			continue
		}
		if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed ||
			!strings.Contains(resp.Error.Message, tc.field) {
			t.Errorf("%s: %s %q, want %s naming %s", name, data.Code, resp.Error.Message, mcpCodeValidationFailed, tc.field)
		}
	}
	if len(rec.events) != 0 {
		t.Errorf("audit = %v, want nothing written", rec.events)
	}
}
