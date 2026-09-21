package mcp

// Guards for the MCP half of the console plane switch (NIM-292).
//
// `keeper.soul.run-command` is the non-interactive console (ADR-0074 amendment,
// NIM-147): same right, same privilege, no tty. So `console.enabled: false` has
// to take it away too. A switch that closed the WebSocket and left this tool
// live would be the worst outcome available here — an operator would read "no
// consoles in this cluster" off their config while an agent kept running
// arbitrary command lines as root.
//
// The shape of the refusal is the second contract: the tool is ABSENT, not
// present-and-refusing, because an agent that could tell "disabled" from
// "unknown" would have learned the cluster has a console plane.

import (
	"encoding/json"
	"testing"
)

func planeSwitchHandler(t *testing.T, enabled bool) *Handler {
	t.Helper()
	h, _, _ := newTestHandler(t, &fakePool{}, shellGateRBAC("soul.console"))
	h.deps.ConsolePlaneEnabled = func() bool { return enabled }
	return h
}

func toolNames(t *testing.T, h *Handler) map[string]bool {
	t.Helper()
	resp, isNot := h.Dispatch(t.Context(), claims("archon-alice"), jsonRPCRequest{
		JSONRPC: "2.0", ID: mustRawID(701), Method: "tools/list",
	})
	if isNot {
		t.Fatal("tools/list must not be a notification")
	}
	if resp.Error != nil {
		t.Fatalf("tools/list failed: %+v", resp.Error)
	}
	var res toolsListResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	names := make(map[string]bool, len(res.Tools))
	for _, d := range res.Tools {
		names[d.Name] = true
	}
	return names
}

// With the plane off the tool is gone from the catalog — and nothing else is.
func TestConsolePlaneSwitch_MCP_OffHidesOnlyThePlaneTools(t *testing.T) {
	on := toolNames(t, planeSwitchHandler(t, true))
	off := toolNames(t, planeSwitchHandler(t, false))

	if !on["keeper.soul.run-command"] {
		t.Fatal("run-command missing from the catalog while the plane is on")
	}
	if off["keeper.soul.run-command"] {
		t.Fatal("run-command still listed while the plane is off — the agent-facing half of the console survived the switch")
	}
	for name := range on {
		if _, isPlane := consolePlaneTools[name]; isPlane {
			continue
		}
		if !off[name] {
			t.Errorf("%s disappeared with the console plane — the switch reached a tool that is not part of it", name)
		}
	}
	if len(on)-len(off) != len(consolePlaneTools) {
		t.Fatalf("catalog shrank by %d tools, want exactly the %d console-plane ones", len(on)-len(off), len(consolePlaneTools))
	}
}

// Calling it by name answers exactly as a typo does.
func TestConsolePlaneSwitch_MCP_OffMakesTheToolUnknown(t *testing.T) {
	h := planeSwitchHandler(t, false)

	disabled := callTool(t, h, "archon-alice", "keeper.soul.run-command",
		`{"sid":"web-01.example.com","command":"uptime"}`)
	if disabled.Error == nil {
		t.Fatal("run-command answered while the plane is off")
	}
	gotCode := mustToolErrorData(t, disabled.Error.Data).Code
	if gotCode != mcpCodeNotFound {
		t.Fatalf("code = %q, want %q — a disabled plane must not be distinguishable from an unknown tool", gotCode, mcpCodeNotFound)
	}
}

// The counterpart that gives the above its meaning: with the plane ON the tool
// gets past the name check and fails for a wiring reason instead. If this ever
// returns not-found too, the guard above stops proving anything.
func TestConsolePlaneSwitch_MCP_OnStillReachesTheTool(t *testing.T) {
	h := planeSwitchHandler(t, true)

	resp := callTool(t, h, "archon-alice", "keeper.soul.run-command",
		`{"sid":"web-01.example.com","command":"uptime"}`)
	if resp.Error == nil {
		t.Fatal("expected an error response (no dispatcher is wired)")
	}
	if got := mustToolErrorData(t, resp.Error.Data).Code; got != mcpCodeInternalError {
		t.Fatalf("code = %q, want internal-error (the tool was reached, nothing is wired behind it)", got)
	}
}

// An absent provider is "on": a keeper.yml written before NIM-292, and every
// harness that does not care about the plane, must behave exactly as before.
func TestConsolePlaneSwitch_MCP_NilProviderKeepsTheToolListed(t *testing.T) {
	h, _, _ := newTestHandler(t, &fakePool{}, shellGateRBAC("soul.console"))
	if h.deps.ConsolePlaneEnabled != nil {
		t.Fatal("the test handler already wires the switch; this guard needs it absent")
	}
	if !toolNames(t, h)["keeper.soul.run-command"] {
		t.Fatal("run-command vanished with no switch configured — absent must mean on")
	}
}
