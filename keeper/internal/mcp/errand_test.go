package mcp

import (
	"fmt"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/errand"
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

// TestMapErrandDispatchError_DryRunCapability — the MCP twin of the REST mapping
// (handlers/errand.go::dispatchError): the dry-run capability gate (NIM-456)
// surfaces as code=soul-capability-unsupported on both surfaces, not as the
// catch-all internal-error. The two causes keep distinct messages so an agent
// reading the reply knows whether to upgrade an agent binary or report an outage.
func TestMapErrandDispatchError_DryRunCapability(t *testing.T) {
	h, _, _ := newTestHandler(t, &fakePool{}, errandAdminCfg())

	for _, tc := range []struct {
		name        string
		err         error
		wantMessage string
	}{
		{
			name:        "not announced",
			err:         fmt.Errorf("host h did not announce it: %w", errand.ErrDryRunNotAnnounced),
			wantMessage: "predates the flag and needs updating",
		},
		{
			name:        "unverifiable",
			err:         fmt.Errorf("no checker: %w", errand.ErrDryRunUnverifiable),
			wantMessage: "presence source is unavailable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.mapErrandDispatchError(nil, "keeper.soul.errand.run", tc.err)
			if resp.Error == nil {
				t.Fatal("expected an error response")
			}
			data := mustToolErrorData(t, resp.Error.Data)
			if data.Code != mcpCodeSoulCapabilityUnsupported {
				t.Errorf("code = %q, want %q", data.Code, mcpCodeSoulCapabilityUnsupported)
			}
			if !strings.Contains(resp.Error.Message, tc.wantMessage) {
				t.Errorf("message = %q, does not carry %q", resp.Error.Message, tc.wantMessage)
			}
			if !strings.Contains(resp.Error.Message, "dry_run") {
				t.Errorf("message = %q, does not name dry_run", resp.Error.Message)
			}
		})
	}
}

// TestMapErrandDispatchError_NotConnectedStillNotFound — the new cases must not
// have shadowed the existing one: a Soul that is not connected is still not-found,
// which is a different operator action from "too old for dry_run".
func TestMapErrandDispatchError_NotConnectedStillNotFound(t *testing.T) {
	h, _, _ := newTestHandler(t, &fakePool{}, errandAdminCfg())

	resp := h.mapErrandDispatchError(nil, "keeper.soul.errand.run", errand.ErrSoulNotConnected)
	if resp.Error == nil {
		t.Fatal("expected an error response")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeNotFound {
		t.Errorf("code = %q, want %q", data.Code, mcpCodeNotFound)
	}
}

// TestErrandRunArgs_DryRunTag — the wire name of the flag on the MCP surface.
// `dry_run` crosses from the tool arguments into errand.DispatchRequest through
// this one struct tag (errand.go:29 → :109); a rename or typo drops it silently,
// the capability gate never fires, and an outdated Soul applies for real while
// every gate test stays green. Cheap guard for a hop nothing else covers.
func TestErrandRunArgs_DryRunTag(t *testing.T) {
	var a errandRunArgs
	if err := strictUnmarshal([]byte(`{"sid":"web-01.example.com","module":"core.cmd.shell","dry_run":true}`), &a); err != nil {
		t.Fatalf("strictUnmarshal: %v", err)
	}
	if !a.DryRun {
		t.Error("* dry_run:true in the arguments did not reach errandRunArgs.DryRun - check the json tag")
	}
	// And the tool must accept the key at all: strict unmarshal rejects unknown
	// fields, so a manifest advertising `dry_run` against a struct that lost it
	// would fail above rather than here — this pins the pair together.
	if !strings.Contains(string(schemaErrandRunInput), `"dry_run"`) {
		t.Error("schemaErrandRunInput does not declare dry_run, so the tool would reject the flag it accepts")
	}
	if !strings.Contains(string(schemaErrandRunInput), "soul-capability-unsupported") {
		t.Error("schemaErrandRunInput's dry_run description does not mention the capability refusal - " +
			"this text is what an agent reads before choosing to send the flag")
	}
}

// TestMapErrandDispatchError_DryRunVerbShell — the MCP mirror of the REST 400. An MCP
// client is usually another agent, and the two surfaces answering differently for the
// same request is exactly how an agent "works around" a refusal by switching transport.
// malformed-request, not one of the capability codes: no cluster or agent state can make
// this pair work.
func TestMapErrandDispatchError_DryRunVerbShell(t *testing.T) {
	h, _, _ := newTestHandler(t, &fakePool{}, errandAdminCfg())

	// Wrapped, as a dispatcher on the way out would carry it.
	err := fmt.Errorf("dispatch host.test: %w", &errand.DryRunVerbShellError{Module: "core.exec.run"})

	resp := h.mapErrandDispatchError(nil, "keeper.soul.errand.run", err)
	if resp.Error == nil {
		t.Fatal("expected an error response")
	}
	data := mustToolErrorData(t, resp.Error.Data)
	if data.Code != mcpCodeMalformedRequest {
		t.Errorf("code = %q, want %q (a capability code would send the agent to upgrade a Soul that refuses this too; "+
			"the internal code would report our bug for the caller's mistake)", data.Code, mcpCodeMalformedRequest)
	}
	for _, want := range []string{"core.exec.run", "dry_run", "errand_dry_run_unsupported"} {
		if !strings.Contains(resp.Error.Message, want) {
			t.Errorf("message = %q, does not carry %q", resp.Error.Message, want)
		}
	}
}
