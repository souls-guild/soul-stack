package mcp

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
)

func listerRBAC() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "lister", Operators: []string{"archon-alice"}, Permissions: []string{"incarnation.list"}},
		},
	}
}

func mustListOutput(t *testing.T, resp jsonRPCResponse) incarnationListOutput {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var res toolsCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var out incarnationListOutput
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
	return out
}

func TestToolsCall_IncarnationList_Success(t *testing.T) {
	now := time.Now().UTC()
	pool := &fakePool{
		incListFn: func(_ incarnation.ListFilter) ([]*incarnation.Incarnation, int) {
			return []*incarnation.Incarnation{
				{Name: "redis-prod", Service: "redis", ServiceVersion: "v1", StateSchemaVersion: 1,
					Spec: map[string]any{"replicas": float64(2)}, Status: incarnation.StatusReady,
					CreatedAt: now, UpdatedAt: now},
				{Name: "pg-prod", Service: "postgres", ServiceVersion: "v2", StateSchemaVersion: 3,
					Status: incarnation.StatusApplying, CreatedAt: now, UpdatedAt: now},
			}, 2
		},
	}
	h, _, rec := newTestHandler(t, pool, listerRBAC())

	out := mustListOutput(t, callTool(t, h, "archon-alice", "keeper.incarnation.list", `{}`))
	if out.Total != 2 || len(out.Items) != 2 {
		t.Fatalf("total=%d items=%d, want 2/2", out.Total, len(out.Items))
	}
	if out.Limit != listDefaultLimit || out.Offset != 0 {
		t.Errorf("offset/limit defaults: %d/%d", out.Offset, out.Limit)
	}
	if out.Items[0].Name != "redis-prod" || out.Items[1].Service != "postgres" {
		t.Errorf("items mismatch: %+v", out.Items)
	}
	if out.Items[0].Spec["replicas"] != float64(2) {
		t.Errorf("item spec lost: %+v", out.Items[0].Spec)
	}
	// reads НЕ аудируются (паритет REST List).
	if len(rec.events) != 0 {
		t.Errorf("list must not write audit, got %d", len(rec.events))
	}
}

func TestToolsCall_IncarnationList_Empty(t *testing.T) {
	// nil incListFn → пустой список, total=0. Items НЕ должен быть nil
	// (make-инициализация) — JSON-массив `[]`, не `null`.
	h, _, _ := newTestHandler(t, &fakePool{}, listerRBAC())
	out := mustListOutput(t, callTool(t, h, "archon-alice", "keeper.incarnation.list", `{}`))
	if out.Total != 0 || len(out.Items) != 0 {
		t.Fatalf("want empty, got total=%d items=%d", out.Total, len(out.Items))
	}
	var res toolsCallResult
	_ = json.Unmarshal(callTool(t, h, "archon-alice", "keeper.incarnation.list", `{}`).Result, &res)
	if contains(string(res.StructuredContent), `"items":null`) {
		t.Error("items must serialize as [] not null")
	}
}

func TestToolsCall_IncarnationList_RBACForbidden(t *testing.T) {
	called := false
	pool := &fakePool{
		incListFn: func(_ incarnation.ListFilter) ([]*incarnation.Incarnation, int) {
			called = true
			return nil, 0
		},
	}
	h, _, _ := newTestHandler(t, pool, nil) // пустой RBAC → deny
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.list", `{}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeForbidden {
		t.Errorf("data.code = %q, want forbidden", data.Code)
	}
	if called {
		t.Error("SelectAll must NOT be called when RBAC denies")
	}
}

func TestToolsCall_IncarnationList_InvalidStatus(t *testing.T) {
	h, _, _ := newTestHandler(t, &fakePool{}, listerRBAC())
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.list", `{"status":"zombie"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Errorf("data.code = %q, want validation-failed", data.Code)
	}
}

func TestToolsCall_IncarnationList_BadLimit(t *testing.T) {
	h, _, _ := newTestHandler(t, &fakePool{}, listerRBAC())
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.list", `{"limit":0}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Errorf("data.code = %q, want validation-failed", data.Code)
	}
}

func TestToolsCall_IncarnationList_SecretsMasked(t *testing.T) {
	const masked = "***MASKED***"
	pool := &fakePool{
		incListFn: func(_ incarnation.ListFilter) ([]*incarnation.Incarnation, int) {
			return []*incarnation.Incarnation{
				{Name: "redis-prod", Service: "redis", ServiceVersion: "v1", StateSchemaVersion: 1,
					Spec:   map[string]any{"password": "hunter2", "replicas": float64(1)},
					State:  map[string]any{"tls_cert": "vault:secret/redis/tls"},
					Status: incarnation.StatusReady, CreatedAt: time.Now(), UpdatedAt: time.Now()},
			}, 1
		},
	}
	h, _, _ := newTestHandler(t, pool, listerRBAC())
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.list", `{}`)
	out := mustListOutput(t, resp)
	if out.Items[0].Spec["password"] != masked {
		t.Errorf("spec.password not masked: %v", out.Items[0].Spec["password"])
	}
	if out.Items[0].State["tls_cert"] != masked {
		t.Errorf("state.tls_cert not masked: %v", out.Items[0].State["tls_cert"])
	}
	if out.Items[0].Spec["replicas"] != float64(1) {
		t.Errorf("non-secret replicas mutated: %v", out.Items[0].Spec["replicas"])
	}
	var res toolsCallResult
	_ = json.Unmarshal(resp.Result, &res)
	if contains(string(res.StructuredContent), "hunter2") || contains(string(res.StructuredContent), "vault:secret/redis/tls") {
		t.Error("raw secret leaked into list output")
	}
}
