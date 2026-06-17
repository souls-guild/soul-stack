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

func unlockerRBAC() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "unlocker", Operators: []string{"archon-alice"}, Permissions: []string{"incarnation.unlock"}},
		},
	}
}

// incWithStatus — backing incFn, отдающий inc в заданном статусе (для FOR
// UPDATE-select unlock-а).
func incWithStatus(status incarnation.Status) func(string) (*incarnation.Incarnation, error) {
	return func(name string) (*incarnation.Incarnation, error) {
		now := time.Now().UTC()
		return &incarnation.Incarnation{Name: name, Service: "redis", ServiceVersion: "v1",
			StateSchemaVersion: 1, Status: status, State: map[string]any{}, CreatedAt: now, UpdatedAt: now}, nil
	}
}

func TestToolsCall_IncarnationUnlock_Success(t *testing.T) {
	pool := &fakePool{incFn: incWithStatus(incarnation.StatusErrorLocked)}
	h, rec := newTestHandlerFull(t, pool, unlockerRBAC(), nil, nil, nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.unlock",
		`{"name":"redis-prod","reason":"manual recovery"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var res toolsCallResult
	_ = json.Unmarshal(resp.Result, &res)
	var out incarnationUnlockOutput
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.PreviousStatus != "error_locked" || out.Status != "ready" {
		t.Errorf("status transition wrong: %+v", out)
	}
	if out.UnlockedByAID != "archon-alice" {
		t.Errorf("unlocked_by = %q", out.UnlockedByAID)
	}

	// audit: EventIncarnationUnlocked, source=mcp, payload {name, previous_status, reason}.
	if len(rec.events) != 1 {
		t.Fatalf("want 1 audit event, got %d", len(rec.events))
	}
	ev := rec.events[0]
	if ev.EventType != audit.EventIncarnationUnlocked {
		t.Errorf("event_type = %q", ev.EventType)
	}
	if ev.Source != audit.SourceMCP {
		t.Errorf("source = %q, want mcp", ev.Source)
	}
	if ev.ArchonAID != "archon-alice" {
		t.Errorf("archon_aid = %q", ev.ArchonAID)
	}
	if ev.Payload["previous_status"] != "error_locked" || ev.Payload["reason"] != "manual recovery" {
		t.Errorf("audit payload = %+v", ev.Payload)
	}
}

func TestToolsCall_IncarnationUnlock_NotLocked(t *testing.T) {
	// ready → ErrIncarnationNotLocked → incarnation-locked.
	pool := &fakePool{incFn: incWithStatus(incarnation.StatusReady)}
	h, rec := newTestHandlerFull(t, pool, unlockerRBAC(), nil, nil, nil)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.unlock",
		`{"name":"redis-prod","reason":"x"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeIncarnationLocked {
		t.Errorf("data.code = %q, want incarnation-locked", data.Code)
	}
	if len(rec.events) != 0 {
		t.Errorf("failed unlock must not write audit, got %d", len(rec.events))
	}
}

func TestToolsCall_IncarnationUnlock_NotFound(t *testing.T) {
	pool := &fakePool{incFn: func(string) (*incarnation.Incarnation, error) { return nil, pgx.ErrNoRows }}
	h, _ := newTestHandlerFull(t, pool, unlockerRBAC(), nil, nil, nil)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.unlock",
		`{"name":"ghost","reason":"x"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeNotFound {
		t.Errorf("data.code = %q, want not-found", data.Code)
	}
}

func TestToolsCall_IncarnationUnlock_RBACForbidden(t *testing.T) {
	pool := &fakePool{
		incFn:    incWithStatus(incarnation.StatusErrorLocked),
		beginErr: errFakeUnexpected{sql: "BeginTx must not be called when RBAC denies"},
	}
	h, rec := newTestHandlerFull(t, pool, nil, nil, nil, nil)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.unlock",
		`{"name":"redis-prod","reason":"x"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeForbidden {
		t.Errorf("data.code = %q, want forbidden", data.Code)
	}
	if len(rec.events) != 0 {
		t.Errorf("denied unlock must not write audit")
	}
}

func TestToolsCall_IncarnationUnlock_MissingReason(t *testing.T) {
	h, _ := newTestHandlerFull(t, &fakePool{}, unlockerRBAC(), nil, nil, nil)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.unlock", `{"name":"redis-prod"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Errorf("data.code = %q, want validation-failed", data.Code)
	}
}
