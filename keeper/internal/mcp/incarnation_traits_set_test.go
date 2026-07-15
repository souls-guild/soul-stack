package mcp

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// MCP tool keeper.incarnation.traits-set (ADR-060 amend R1) — REST parity
// with PUT /v1/incarnations/{name}/traits. Relocated per-soul → per-incarnation.

func traitsSetRBAC() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "traits-setter", Operators: []string{"archon-alice"}, Permissions: []string{"incarnation.traits-set"}},
		},
	}
}

// incForTraits is an incFn returning a ready incarnation with the given
// traits/covens (for the full-row FOR UPDATE select in UpdateTraits + scope
// resolution).
func incForTraits(traits map[string]any) func(string) (*incarnation.Incarnation, error) {
	return func(name string) (*incarnation.Incarnation, error) {
		now := time.Now().UTC()
		return &incarnation.Incarnation{
			Name: name, Service: "redis", ServiceVersion: "v1",
			StateSchemaVersion: 1, Status: incarnation.StatusReady,
			State: map[string]any{}, Traits: traits, CreatedAt: now, UpdatedAt: now,
		}, nil
	}
}

func decodeTraitsSetOutput(t *testing.T, resp jsonRPCResponse) incarnationTraitsSetOutput {
	t.Helper()
	var res toolsCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var out incarnationTraitsSetOutput
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
	return out
}

func TestIncarnationTraitsSet_InManifest(t *testing.T) {
	e, ok := toolByName("keeper.incarnation.traits-set")
	if !ok {
		t.Fatal("keeper.incarnation.traits-set missing from catalogManifest")
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

func TestIncarnationTraitsSet_Success(t *testing.T) {
	pool := &fakePool{incFn: incForTraits(map[string]any{"team": "dba"})}
	h, rec := newTestHandlerFull(t, pool, traitsSetRBAC(), nil, nil, nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.traits-set",
		`{"name":"redis-prod","traits":{"env":"prod","az":"a"}}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	out := decodeTraitsSetOutput(t, resp)
	if out.Incarnation != "redis-prod" {
		t.Errorf("incarnation = %q", out.Incarnation)
	}
	if len(out.Keys) != 2 || out.Keys[0] != "az" || out.Keys[1] != "env" {
		t.Errorf("keys = %v, want [az env] (sorted)", out.Keys)
	}

	// audit: EventIncarnationTraitsChanged, source=mcp, payload {name, old_keys, new_keys}.
	if len(rec.events) != 1 {
		t.Fatalf("want 1 audit event, got %d", len(rec.events))
	}
	ev := rec.events[0]
	if ev.EventType != audit.EventIncarnationTraitsChanged {
		t.Errorf("event_type = %q", ev.EventType)
	}
	if ev.Source != audit.SourceMCP {
		t.Errorf("source = %q, want mcp", ev.Source)
	}
	oldKeys, _ := ev.Payload["old_keys"].([]string)
	if len(oldKeys) != 1 || oldKeys[0] != "team" {
		t.Errorf("audit old_keys = %v, want [team]", ev.Payload["old_keys"])
	}
}

func TestIncarnationTraitsSet_InvalidValue(t *testing.T) {
	// A nested trait value is rejected by the domain BEFORE mutation.
	pool := &fakePool{
		incFn:    incForTraits(nil),
		beginErr: errFakeUnexpected{sql: "BeginTx must not be called on invalid trait value"},
	}
	h, rec := newTestHandlerFull(t, pool, traitsSetRBAC(), nil, nil, nil)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.traits-set",
		`{"name":"redis-prod","traits":{"bad":{"nested":1}}}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Errorf("code = %q, want validation-failed", data.Code)
	}
	if len(rec.events) != 0 {
		t.Errorf("invalid traits must not write audit")
	}
}

func TestIncarnationTraitsSet_NotFound(t *testing.T) {
	pool := &fakePool{incFn: func(string) (*incarnation.Incarnation, error) { return nil, pgx.ErrNoRows }}
	h, _ := newTestHandlerFull(t, pool, traitsSetRBAC(), nil, nil, nil)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.traits-set",
		`{"name":"ghost","traits":{"team":"dba"}}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeNotFound {
		t.Errorf("code = %q, want not-found", data.Code)
	}
}

func TestIncarnationTraitsSet_RBACForbidden(t *testing.T) {
	// Operator without incarnation.traits-set → deny BEFORE mutation (BeginTx forbidden).
	pool := &fakePool{
		incFn:    incForTraits(nil),
		beginErr: errFakeUnexpected{sql: "BeginTx must not be called when RBAC denies"},
	}
	h, rec := newTestHandlerFull(t, pool, nil, nil, nil, nil)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.traits-set",
		`{"name":"redis-prod","traits":{"team":"dba"}}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeForbidden {
		t.Errorf("code = %q, want forbidden", data.Code)
	}
	if len(rec.events) != 0 {
		t.Errorf("denied traits-set must not write audit")
	}
}
