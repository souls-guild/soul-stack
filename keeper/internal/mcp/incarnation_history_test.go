package mcp

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
)

func historianRBAC() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "historian", Operators: []string{"archon-alice"}, Permissions: []string{"incarnation.history"}},
		},
	}
}

func mustHistoryOutput(t *testing.T, resp jsonRPCResponse) incarnationHistoryOutput {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var res toolsCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var out incarnationHistoryOutput
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
	return out
}

// readyInc — backing для existence-probe (SelectByName).
func readyInc(name string) (*incarnation.Incarnation, error) {
	now := time.Now().UTC()
	return &incarnation.Incarnation{Name: name, Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: incarnation.StatusReady, CreatedAt: now, UpdatedAt: now}, nil
}

func TestToolsCall_IncarnationHistory_Success(t *testing.T) {
	now := time.Now().UTC()
	pool := &fakePool{
		incFn: func(name string) (*incarnation.Incarnation, error) { return readyInc(name) },
		historyFn: func(_ string, _ incarnation.HistoryFilter) ([]*incarnation.HistoryEntry, int) {
			return []*incarnation.HistoryEntry{
				{HistoryID: "h1", Scenario: "create", ApplyID: "a1",
					StateBefore: map[string]any{}, StateAfter: map[string]any{"leader": "redis-01"}, At: now},
			}, 1
		},
	}
	h, _, rec := newTestHandler(t, pool, historianRBAC())

	out := mustHistoryOutput(t, callTool(t, h, "archon-alice", "keeper.incarnation.history", `{"name":"redis-prod"}`))
	if out.Total != 1 || len(out.Items) != 1 {
		t.Fatalf("total=%d items=%d, want 1/1", out.Total, len(out.Items))
	}
	if out.Items[0].Scenario != "create" || out.Items[0].ApplyID != "a1" {
		t.Errorf("entry mismatch: %+v", out.Items[0])
	}
	if out.Items[0].StateAfter["leader"] != "redis-01" {
		t.Errorf("state_after lost: %+v", out.Items[0].StateAfter)
	}
	if len(rec.events) != 0 {
		t.Errorf("history must not write audit, got %d", len(rec.events))
	}
}

func TestToolsCall_IncarnationHistory_NotFound(t *testing.T) {
	// incFn nil → existence-probe SelectByName → ErrNoRows → not-found
	// (history НЕ должна вернуть пустую страницу для несуществующей name).
	historyCalled := false
	pool := &fakePool{
		historyFn: func(_ string, _ incarnation.HistoryFilter) ([]*incarnation.HistoryEntry, int) {
			historyCalled = true
			return nil, 0
		},
	}
	pool.incFn = func(string) (*incarnation.Incarnation, error) { return nil, pgx.ErrNoRows }
	h, _, _ := newTestHandler(t, pool, historianRBAC())

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.history", `{"name":"ghost"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeNotFound {
		t.Errorf("data.code = %q, want not-found", data.Code)
	}
	if historyCalled {
		t.Error("HistorySelectByName must NOT run for non-existent incarnation")
	}
}

func TestToolsCall_IncarnationHistory_RBACForbidden(t *testing.T) {
	// RBAC пуст → deny. Existence-probe РЕЗОЛВИТ scope (covens ∪ {name}) для
	// OR-Check (зеркало REST middleware), затем enforcer отказывает → forbidden.
	pool := &fakePool{incFn: func(name string) (*incarnation.Incarnation, error) {
		return readyInc(name)
	}}
	h, _, _ := newTestHandler(t, pool, nil)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.history", `{"name":"redis-prod"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeForbidden {
		t.Errorf("data.code = %q, want forbidden", data.Code)
	}
}

func TestToolsCall_IncarnationHistory_BadApplyID(t *testing.T) {
	h, _, _ := newTestHandler(t, &fakePool{}, historianRBAC())
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.history", `{"name":"redis-prod","apply_id":"not-a-ulid"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeMalformedRequest {
		t.Errorf("data.code = %q, want malformed-request", data.Code)
	}
}

func TestToolsCall_IncarnationHistory_SecretsMasked(t *testing.T) {
	const masked = "***MASKED***"
	pool := &fakePool{
		incFn: func(name string) (*incarnation.Incarnation, error) { return readyInc(name) },
		historyFn: func(_ string, _ incarnation.HistoryFilter) ([]*incarnation.HistoryEntry, int) {
			return []*incarnation.HistoryEntry{
				{HistoryID: "h1", Scenario: "rotate", ApplyID: "a1", At: time.Now(),
					StateBefore: map[string]any{"password": "old-secret"},
					StateAfter:  map[string]any{"token": "vault:secret/x"}},
			}, 1
		},
	}
	h, _, _ := newTestHandler(t, pool, historianRBAC())
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.history", `{"name":"redis-prod"}`)
	out := mustHistoryOutput(t, resp)
	if out.Items[0].StateBefore["password"] != masked {
		t.Errorf("state_before.password not masked: %v", out.Items[0].StateBefore["password"])
	}
	if out.Items[0].StateAfter["token"] != masked {
		t.Errorf("state_after.token not masked: %v", out.Items[0].StateAfter["token"])
	}
	var res toolsCallResult
	_ = json.Unmarshal(resp.Result, &res)
	if contains(string(res.StructuredContent), "old-secret") || contains(string(res.StructuredContent), "vault:secret/x") {
		t.Error("raw secret leaked into history output")
	}
}
