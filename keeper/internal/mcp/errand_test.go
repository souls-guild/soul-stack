package mcp

import (
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
)

// Errand-tools требуют конкретные [*errand.Dispatcher] / [*errand.Store]
// (на pgxpool.Pool), поэтому полный happy-path покрывается интеграционными
// тестами errand/dispatcher_test.go и api/handlers/errand_test.go. На уровне
// MCP unit-теста покрываем:
//
//   1. NilGuard — ErrandDispatcher / ErrandStore == nil → internal-error
//      (паттерн RoleTools_NilGuard).
//   2. Catalog — три tool-name присутствуют в манифесте (TestDispatch_ToolsList_HasAllTools).
//
// Sync/async успехи, маппинг sentinel-ов, RBAC-deny → покрывается интеграциями.

// errandAdminCfg — RBAC-конфиг с полным набором errand-permissions для
// caller-а archon-alice. Используется в nil-guard-тесте, чтобы RBAC.Check
// (если бы дошло до него) не блокировал tool.
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
	// newTestHandler не выставляет ErrandDispatcher/ErrandStore → ожидаем
	// internal-error «errand orchestrator is not configured».
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
