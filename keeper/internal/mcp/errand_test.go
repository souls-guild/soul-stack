package mcp

import (
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
)

// Errand-tools require concrete [*errand.Dispatcher] / [*errand.Store]
// (over pgxpool.Pool), so the full happy path is covered by the
// errand/dispatcher_test.go and api/handlers/errand_test.go integration
// tests. At the MCP unit-test level we cover:
//
//   1. NilGuard — ErrandDispatcher / ErrandStore == nil → internal-error
//      (RoleTools_NilGuard pattern).
//   2. Catalog — three tool names present in the manifest
//      (TestDispatch_ToolsList_HasAllTools).
//
// Sync/async success paths, sentinel mapping, RBAC-deny are covered by
// the integration tests.

// errandAdminCfg — RBAC config granting caller archon-alice the full set of
// errand permissions. Used in the nil-guard test so RBAC.Check (if reached)
// doesn't block the tool.
func errandAdminCfg() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{
				Name:        "errand-admin",
				Operators:   []string{"archon-alice"},
				Permissions: []string{"errand.run", "errand.list"},
			},
		},
	}
}

func TestErrandTools_NilGuard(t *testing.T) {
	// newTestHandler doesn't set ErrandDispatcher/ErrandStore → expect
	// internal-error "errand orchestrator is not configured".
	h, _, _ := newTestHandler(t, &fakePool{}, errandAdminCfg())
	cases := []struct {
		tool string
		args string
	}{
		{"keeper.soul.errand.run", `{"sid":"web-01.example.com","module":"core.cmd.shell","input":{"command":"uptime"}}`},
		{"keeper.errand.list", `{}`},
		{"keeper.errand.get", `{"errand_id":"01HF7Z5G8Q5KQ8X7Y2N3R4M5P6"}`},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			resp := callTool(t, h, "archon-alice", tc.tool, tc.args)
			if resp.Error == nil {
				t.Fatal("expected error response")
			}
			data := mustToolErrorData(t, resp.Error.Data)
			if data.Code != mcpCodeInternalError {
				t.Errorf("code = %q, want internal-error", data.Code)
			}
		})
	}
}
