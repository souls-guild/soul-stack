package mcp

import (
	"encoding/json"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// MCP-tool keeper.soul.traits-assign (ADR-060) — parity with REST POST
// /v1/souls/traits. Reuses covenBulkFakePool (same SQL: COUNT(*) FROM souls
// + WITH chunk … UPDATE souls) and harness newCovenAssignHandler from
// soul_coven_assign_test.go.

// traitsAssignAdminCfg — bare grant soul.traits-assign (unrestricted scope).
func traitsAssignAdminCfg() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "traits-admin", Operators: []string{"archon-alice"}, Permissions: []string{
				"soul.traits-assign",
			}},
		},
	}
}

// traitsAssignDevScopedCfg — operator restricted to coven=dev: may only
// change traits on dev hosts (gate a). trait-key is not a scope dimension.
func traitsAssignDevScopedCfg() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "dev-traits-op", Operators: []string{"archon-dev"}, Permissions: []string{
				"soul.traits-assign on coven=dev",
			}},
		},
	}
}

func decodeTraitsAssignOutput(t *testing.T, resp jsonRPCResponse) soulTraitsAssignOutput {
	t.Helper()
	var res toolsCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var out soulTraitsAssignOutput
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
	return out
}

func TestSoulTraitsAssign_InManifest(t *testing.T) {
	e, ok := toolByName("keeper.soul.traits-assign")
	if !ok {
		t.Fatal("keeper.soul.traits-assign missing from catalogManifest")
	}
	if e.status != toolStatusImplemented {
		t.Errorf("status = %d, want Implemented", e.status)
	}
	var schema map[string]any
	if err := json.Unmarshal(e.decl.InputSchema, &schema); err != nil {
		t.Fatalf("inputSchema not valid JSON: %v", err)
	}
	if e.decl.OutputSchema == nil {
		t.Error("outputSchema missing")
	}
}

func TestSoulTraitsAssign_NilSoulDB(t *testing.T) {
	h, _ := newCovenAssignHandler(t, traitsAssignAdminCfg(), nil) // SoulDB == nil
	resp := callTool(t, h, "archon-alice", "keeper.soul.traits-assign",
		`{"mode":"merge","traits":{"x":"y"},"selector":{"all":true}}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeInternalError {
		t.Errorf("code = %q, want internal-error", data.Code)
	}
}

func TestSoulTraitsAssign_MergeSuccess(t *testing.T) {
	pool := &covenBulkFakePool{matched: 5, changed: 5}
	h, rec := newCovenAssignHandler(t, traitsAssignAdminCfg(), pool)
	resp := callTool(t, h, "archon-alice", "keeper.soul.traits-assign",
		`{"mode":"merge","traits":{"namespace":"dba","tier":1},"selector":{"all":true}}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	out := decodeTraitsAssignOutput(t, resp)
	if out.Mode != "merge" {
		t.Errorf("mode = %q, want merge", out.Mode)
	}
	if out.Matched != 5 || out.Changed != 5 {
		t.Errorf("matched/changed = %d/%d, want 5/5", out.Matched, out.Changed)
	}
	if len(out.Keys) != 2 || out.Keys[0] != "namespace" || out.Keys[1] != "tier" {
		t.Errorf("keys = %v, want [namespace tier]", out.Keys)
	}
	ev := requireSingleAudit(t, rec, string(audit.EventSoulTraitsChanged))
	if ev.Source != audit.SourceMCP {
		t.Errorf("audit source = %q, want mcp", ev.Source)
	}
	// trait VALUES are NOT put in the audit payload.
	raw, _ := json.Marshal(ev.Payload)
	if containsSubstrMCP(string(raw), `"dba"`) {
		t.Errorf("audit payload содержит trait-значение: %s", raw)
	}
}

// TestSoulTraitsAssign_DefaultMode_Merge — mode omitted → merge.
func TestSoulTraitsAssign_DefaultMode_Merge(t *testing.T) {
	pool := &covenBulkFakePool{matched: 1, changed: 1}
	h, _ := newCovenAssignHandler(t, traitsAssignAdminCfg(), pool)
	resp := callTool(t, h, "archon-alice", "keeper.soul.traits-assign",
		`{"traits":{"x":"y"},"selector":{"all":true}}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if decodeTraitsAssignOutput(t, resp).Mode != "merge" {
		t.Error("default mode != merge")
	}
}

func TestSoulTraitsAssign_RemoveSuccess(t *testing.T) {
	pool := &covenBulkFakePool{matched: 3, changed: 2}
	h, rec := newCovenAssignHandler(t, traitsAssignAdminCfg(), pool)
	resp := callTool(t, h, "archon-alice", "keeper.soul.traits-assign",
		`{"mode":"remove","keys":["drop-me"],"selector":{"all":true}}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	out := decodeTraitsAssignOutput(t, resp)
	if out.Mode != "remove" || out.Matched != 3 || out.Changed != 2 {
		t.Errorf("out = %+v, want remove/3/2", out)
	}
	requireSingleAudit(t, rec, string(audit.EventSoulTraitsChanged))
}

func TestSoulTraitsAssign_ReplaceSuccess(t *testing.T) {
	pool := &covenBulkFakePool{matched: 2, changed: 2}
	h, _ := newCovenAssignHandler(t, traitsAssignAdminCfg(), pool)
	resp := callTool(t, h, "archon-alice", "keeper.soul.traits-assign",
		`{"mode":"replace","traits":{"only":"this"},"selector":{"all":true}}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if decodeTraitsAssignOutput(t, resp).Mode != "replace" {
		t.Error("mode != replace")
	}
}

func TestSoulTraitsAssign_DryRun(t *testing.T) {
	pool := &covenBulkFakePool{
		matched:  7,
		chunkErr: errFakeUnexpected{sql: "dry_run must NOT run chunk UPDATE"},
	}
	h, rec := newCovenAssignHandler(t, traitsAssignAdminCfg(), pool)
	resp := callTool(t, h, "archon-alice", "keeper.soul.traits-assign",
		`{"mode":"merge","traits":{"x":"y"},"selector":{"all":true},"dry_run":true}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	out := decodeTraitsAssignOutput(t, resp)
	if !out.DryRun || out.Matched != 7 || out.Changed != 0 {
		t.Errorf("out = %+v, want dry_run/7/0", out)
	}
	ev := requireSingleAudit(t, rec, string(audit.EventSoulTraitsChanged))
	if ev.Payload["dry_run"] != true {
		t.Errorf("audit dry_run = %v, want true", ev.Payload["dry_run"])
	}
}

// --- validation ---

func TestSoulTraitsAssign_InvalidMode(t *testing.T) {
	h, _ := newCovenAssignHandler(t, traitsAssignAdminCfg(), &covenBulkFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.soul.traits-assign",
		`{"mode":"append","traits":{"x":"y"},"selector":{"all":true}}`)
	requireValidationFailed(t, resp)
}

func TestSoulTraitsAssign_BadKey(t *testing.T) {
	h, _ := newCovenAssignHandler(t, traitsAssignAdminCfg(), &covenBulkFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.soul.traits-assign",
		`{"mode":"merge","traits":{"Bad_Key":"v"},"selector":{"all":true}}`)
	requireValidationFailed(t, resp)
}

func TestSoulTraitsAssign_NestedValue(t *testing.T) {
	h, _ := newCovenAssignHandler(t, traitsAssignAdminCfg(), &covenBulkFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.soul.traits-assign",
		`{"mode":"merge","traits":{"k":{"nested":1}},"selector":{"all":true}}`)
	requireValidationFailed(t, resp)
}

func TestSoulTraitsAssign_XOR_KeysForMerge(t *testing.T) {
	h, _ := newCovenAssignHandler(t, traitsAssignAdminCfg(), &covenBulkFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.soul.traits-assign",
		`{"mode":"merge","keys":["x"],"selector":{"all":true}}`)
	requireValidationFailed(t, resp)
}

func TestSoulTraitsAssign_RemoveEmptyKeys(t *testing.T) {
	h, _ := newCovenAssignHandler(t, traitsAssignAdminCfg(), &covenBulkFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.soul.traits-assign",
		`{"mode":"remove","selector":{"all":true}}`)
	requireValidationFailed(t, resp)
}

func TestSoulTraitsAssign_EmptySelector(t *testing.T) {
	h, _ := newCovenAssignHandler(t, traitsAssignAdminCfg(), &covenBulkFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.soul.traits-assign",
		`{"mode":"merge","traits":{"x":"y"},"selector":{}}`)
	requireValidationFailed(t, resp)
}

func TestSoulTraitsAssign_UnknownArg(t *testing.T) {
	h, _ := newCovenAssignHandler(t, traitsAssignAdminCfg(), &covenBulkFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.soul.traits-assign",
		`{"mode":"merge","traits":{"x":"y"},"selector":{"all":true},"extra":1}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeMalformedRequest {
		t.Errorf("code = %q, want malformed-request", data.Code)
	}
}

// --- RBAC / scope (security) ---

// TestSoulTraitsAssign_RBACForbidden — operator without soul.traits-assign →
// deny at the permission layer, DB untouched, no audit written.
func TestSoulTraitsAssign_RBACForbidden(t *testing.T) {
	h, rec := newCovenAssignHandler(t, nil, &covenBulkFakePool{
		countErr: errFakeUnexpected{sql: "traits-assign must NOT query when RBAC denies"},
	})
	resp := callTool(t, h, "archon-alice", "keeper.soul.traits-assign",
		`{"mode":"merge","traits":{"x":"y"},"selector":{"all":true}}`)
	if resp.Error == nil {
		t.Fatal("expected forbidden error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeForbidden {
		t.Errorf("code = %q, want forbidden", data.Code)
	}
	if len(rec.events) != 0 {
		t.Error("denied traits-assign must not write audit")
	}
}

// TestSoulTraitsAssign_ScopedOperator_ScopeApplied — GUARD least-privilege
// (gate a): a coven-scoped (dev) operator passes the permission check
// (trait-key is not a scope dimension, no gate b), but the service layer
// still receives coven-scope=[dev] (scope_applied=true in audit, scope array
// in COUNT-args). Without this, bulk would bypass least-privilege.
func TestSoulTraitsAssign_ScopedOperator_ScopeApplied(t *testing.T) {
	pool := &covenBulkFakePool{matched: 2, changed: 2}
	h, rec := newCovenAssignHandler(t, traitsAssignDevScopedCfg(), pool)
	resp := callTool(t, h, "archon-dev", "keeper.soul.traits-assign",
		`{"mode":"merge","traits":{"x":"y"},"selector":{"all":true}}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if len(rec.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(rec.events))
	}
	ev := rec.events[0]
	if ev.EventType != audit.EventSoulTraitsChanged || ev.ArchonAID != "archon-dev" {
		t.Errorf("event = %q / aid %q", ev.EventType, ev.ArchonAID)
	}
	if ev.Payload["scope_applied"] != true {
		t.Errorf("audit scope_applied = %v, want true (restricted dev-op)", ev.Payload["scope_applied"])
	}
	// scope predicate (coven && ARRAY[dev]) made it into COUNT-args.
	foundScope := false
	for _, a := range pool.gotCountArgs {
		if arr, ok := a.([]string); ok && len(arr) == 1 && arr[0] == "dev" {
			foundScope = true
		}
	}
	if !foundScope {
		t.Errorf("scope predicate [dev] not in COUNT-args: %v", pool.gotCountArgs)
	}
}

func containsSubstrMCP(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
