package mcp

import (
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/shared/audit"
)

func runnerRBAC() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "runner", Operators: []string{"archon-alice"}, Permissions: []string{"incarnation.run"}},
		},
	}
}

func TestToolsCall_IncarnationRun_Success(t *testing.T) {
	pool := &fakePool{incFn: incWithStatus(incarnation.StatusReady)}
	starter := &mcpStarter{}
	h, rec := newTestHandlerFull(t, pool, runnerRBAC(), starter, &mcpResolver{ok: true}, nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.run",
		`{"name":"redis-prod","scenario":"rotate","input":{"force":true}}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var res toolsCallResult
	_ = json.Unmarshal(resp.Result, &res)
	var out incarnationRunOutput
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Incarnation != "redis-prod" || out.Scenario != "rotate" {
		t.Errorf("output echo wrong: %+v", out)
	}
	if !audit.IsValidULID(out.ApplyID) {
		t.Errorf("_apply_id not ULID: %q", out.ApplyID)
	}
	if starter.calls != 1 || starter.gotSpec.ScenarioName != "rotate" || starter.gotSpec.ApplyID != out.ApplyID {
		t.Errorf("run spec mismatch: %+v (calls=%d)", starter.gotSpec, starter.calls)
	}

	if len(rec.events) != 1 {
		t.Fatalf("want 1 audit event, got %d", len(rec.events))
	}
	ev := rec.events[0]
	if ev.EventType != audit.EventIncarnationScenarioStarted || ev.Source != audit.SourceMCP {
		t.Errorf("event = %q / source %q", ev.EventType, ev.Source)
	}
	if ev.Payload["scenario"] != "rotate" || ev.Payload["apply_id"] != out.ApplyID {
		t.Errorf("audit payload = %+v", ev.Payload)
	}
}

func TestToolsCall_IncarnationRun_ErrorLocked(t *testing.T) {
	// error_locked → fast probe rejection incarnation-locked before the run starts.
	pool := &fakePool{incFn: incWithStatus(incarnation.StatusErrorLocked)}
	starter := &mcpStarter{}
	h, rec := newTestHandlerFull(t, pool, runnerRBAC(), starter, &mcpResolver{ok: true}, nil)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.run",
		`{"name":"redis-prod","scenario":"rotate"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeIncarnationLocked {
		t.Errorf("data.code = %q, want incarnation-locked", data.Code)
	}
	if starter.calls != 0 {
		t.Error("scenario must NOT start for error_locked incarnation")
	}
	if len(rec.events) != 0 {
		t.Error("locked run must not write audit")
	}
}

func TestToolsCall_IncarnationRun_NotFound(t *testing.T) {
	pool := &fakePool{incFn: func(string) (*incarnation.Incarnation, error) { return nil, pgx.ErrNoRows }}
	h, _ := newTestHandlerFull(t, pool, runnerRBAC(), &mcpStarter{}, &mcpResolver{ok: true}, nil)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.run",
		`{"name":"ghost","scenario":"rotate"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeNotFound {
		t.Errorf("data.code = %q, want not-found", data.Code)
	}
}

func TestToolsCall_IncarnationRun_RBACForbidden(t *testing.T) {
	// RBAC empty → deny. SelectByName RESOLVES scope (covens ∪ {name}) for the
	// OR-Check (mirrors REST middleware), then the enforcer denies → forbidden.
	pool := &fakePool{incFn: incWithStatus(incarnation.StatusReady)}
	h, rec := newTestHandlerFull(t, pool, nil, &mcpStarter{}, &mcpResolver{ok: true}, nil)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.run",
		`{"name":"redis-prod","scenario":"rotate"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeForbidden {
		t.Errorf("data.code = %q, want forbidden", data.Code)
	}
	if len(rec.events) != 0 {
		t.Error("denied run must not write audit")
	}
}

func TestToolsCall_IncarnationRun_InvalidScenario(t *testing.T) {
	h, _ := newTestHandlerFull(t, &fakePool{}, runnerRBAC(), &mcpStarter{}, &mcpResolver{ok: true}, nil)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.run",
		`{"name":"redis-prod","scenario":"Bad-Scenario"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Errorf("data.code = %q, want validation-failed", data.Code)
	}
}
