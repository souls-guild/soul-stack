package mcp

import (
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/keeper/internal/shellgate"
)

// The console gate on the MCP choke-point (ADR-0074 amendment, NIM-197):
// `keeper.soul.errand.run` naming `core.cmd.shell` / `core.exec.run` needs
// `soul.console` on top of `errand.run`.
//
// The tests read the outcome through the tool's error code. Without a wired
// ErrandDispatcher an ALLOWED call lands on the nil-guard (internal-error) —
// which is exactly the signal that the gate let it through, since a refusal is a
// `forbidden`.

func shellGateRBAC(perms ...string) *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{{
			Name:        "gate-role",
			Operators:   []string{"archon-alice"},
			Permissions: perms,
		}},
	}
}

func TestMCPErrandRun_ShellGate(t *testing.T) {
	cases := []struct {
		name      string
		mode      shellgate.Mode
		module    string
		perms     []string
		wantCode  string
		wantAllow bool
	}{
		{"window: errand.run alone reaches a shell", shellgate.ModeWarn, "core.cmd.shell", []string{"errand.run"}, mcpCodeInternalError, true},
		{"enforce: errand.run alone is refused", shellgate.ModeEnforce, "core.cmd.shell", []string{"errand.run"}, mcpCodeForbidden, false},
		{"enforce: errand.run + soul.console passes", shellgate.ModeEnforce, "core.cmd.shell", []string{"errand.run", "soul.console"}, mcpCodeInternalError, true},
		{"enforce: core.exec.run is refused too", shellgate.ModeEnforce, "core.exec.run", []string{"errand.run"}, mcpCodeForbidden, false},
		{"enforce: a read-safe module needs no console", shellgate.ModeEnforce, "core.http.probe", []string{"errand.run"}, mcpCodeInternalError, true},
		// The rights stay independent in both directions (ADR-0074(c)): holding
		// only soul.console does not open the Errand path.
		{"enforce: soul.console without errand.run is refused", shellgate.ModeEnforce, "core.cmd.shell", []string{"soul.console"}, mcpCodeForbidden, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, _, _ := newTestHandler(t, &fakePool{}, shellGateRBAC(c.perms...))
			h.deps.ShellGate = shellgate.New(c.mode, nil, nil)

			resp := callTool(t, h, "archon-alice", "keeper.soul.errand.run",
				`{"sid":"web-01.example.com","module":"`+c.module+`","input":{"cmd":"uptime"}}`)
			if resp.Error == nil {
				t.Fatal("expected an error response (no dispatcher is wired)")
			}
			data := mustToolErrorData(t, resp.Error.Data)
			if data.Code != c.wantCode {
				t.Fatalf("code = %q, want %q (allowed=%v)", data.Code, c.wantCode, c.wantAllow)
			}
		})
	}
}

// TestMCPRunCommand_UnaffectedByTheGate — `keeper.soul.run-command` (NIM-147) has
// required `soul.console` from the start and does NOT require `errand.run`, even
// though it rides the Errand transport. Enforcing the gate must not change that:
// an operator holding only `soul.console` keeps the non-interactive console.
func TestMCPRunCommand_UnaffectedByTheGate(t *testing.T) {
	h, _, _ := newTestHandler(t, &fakePool{}, shellGateRBAC("soul.console"))
	h.deps.ShellGate = shellgate.New(shellgate.ModeEnforce, nil, nil)

	resp := callTool(t, h, "archon-alice", "keeper.soul.run-command",
		`{"sid":"web-01.example.com","command":"uptime"}`)
	if resp.Error == nil {
		t.Fatal("expected an error response (no dispatcher is wired)")
	}
	data := mustToolErrorData(t, resp.Error.Data)
	if data.Code == mcpCodeForbidden {
		t.Fatalf("run-command was refused for an operator holding soul.console: %+v", data)
	}
	if data.Code != mcpCodeInternalError {
		t.Fatalf("code = %q, want internal-error (permission passed, no dispatcher)", data.Code)
	}
}
