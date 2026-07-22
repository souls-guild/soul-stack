package mcp

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/keeper/internal/statemigrate"
	"github.com/souls-guild/soul-stack/shared/audit"
)

func upgraderRBAC() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "upgrader", Operators: []string{"archon-alice"}, Permissions: []string{"incarnation.upgrade"}},
		},
	}
}

// incAtVersion — backing incFn for upgrade: SelectByName (full row) and the
// FOR UPDATE select read the same inc (serviceVersion / schema / status).
func incAtVersion(serviceVer string, schema int, status incarnation.Status) func(string) (*incarnation.Incarnation, error) {
	return func(name string) (*incarnation.Incarnation, error) {
		now := time.Now().UTC()
		return &incarnation.Incarnation{Name: name, Service: "redis", ServiceVersion: serviceVer,
			StateSchemaVersion: schema, Status: status, State: map[string]any{}, CreatedAt: now, UpdatedAt: now}, nil
	}
}

func oneStepChain(t *testing.T) statemigrate.Chain {
	t.Helper()
	mig, err := statemigrate.Parse([]byte("from_version: 1\nto_version: 2\ntransform:\n  - set:\n      path: state.foo\n      value: bar\n"))
	if err != nil {
		t.Fatalf("parse migration: %v", err)
	}
	return statemigrate.Chain{mig}
}

func TestToolsCall_IncarnationUpgrade_Success(t *testing.T) {
	pool := &fakePool{incFn: incAtVersion("v1", 1, incarnation.StatusReady)}
	loader := &mcpLoader{targetSchema: 2, chain: oneStepChain(t)}
	h, rec := newTestHandlerFull(t, pool, upgraderRBAC(), nil, &mcpResolver{ok: true}, loader)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.upgrade",
		`{"name":"redis-prod","to_version":"v2"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var res toolsCallResult
	_ = json.Unmarshal(resp.Result, &res)
	var out incarnationUpgradeOutput
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !audit.IsValidULID(out.ApplyID) {
		t.Errorf("_apply_id not ULID: %q", out.ApplyID)
	}

	if len(rec.events) != 1 {
		t.Fatalf("want 1 audit event, got %d", len(rec.events))
	}
	ev := rec.events[0]
	if ev.EventType != audit.EventIncarnationUpgradeStarted || ev.Source != audit.SourceMCP {
		t.Errorf("event = %q / source %q", ev.EventType, ev.Source)
	}
	if ev.Payload["to_version"] != "v2" || ev.Payload["apply_id"] != out.ApplyID {
		t.Errorf("audit payload = %+v", ev.Payload)
	}
}

func TestToolsCall_IncarnationUpgrade_Downgrade(t *testing.T) {
	// Current schema 3, target 2 → ErrDowngradeViaRef (prepare) → incarnation-locked.
	pool := &fakePool{incFn: incAtVersion("v3", 3, incarnation.StatusReady)}
	loader := &mcpLoader{targetSchema: 2}
	h, rec := newTestHandlerFull(t, pool, upgraderRBAC(), nil, &mcpResolver{ok: true}, loader)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.upgrade",
		`{"name":"redis-prod","to_version":"v2"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeIncarnationLocked {
		t.Errorf("data.code = %q, want incarnation-locked", data.Code)
	}
	if len(rec.events) != 0 {
		t.Error("failed upgrade must not write audit")
	}
}

func TestToolsCall_IncarnationUpgrade_Noop(t *testing.T) {
	// Same ref AND same schema → ErrUpgradeNoop → validation-failed.
	pool := &fakePool{incFn: incAtVersion("v1", 1, incarnation.StatusReady)}
	loader := &mcpLoader{targetSchema: 1, chain: statemigrate.Chain{}}
	h, _ := newTestHandlerFull(t, pool, upgraderRBAC(), nil, &mcpResolver{ok: true}, loader)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.upgrade",
		`{"name":"redis-prod","to_version":"v1"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Errorf("data.code = %q, want validation-failed", data.Code)
	}
}

func TestToolsCall_IncarnationUpgrade_ChainBroken(t *testing.T) {
	pool := &fakePool{incFn: incAtVersion("v1", 1, incarnation.StatusReady)}
	loader := &mcpLoader{targetSchema: 3, chainErr: artifact.ErrMigrationChainBroken}
	h, _ := newTestHandlerFull(t, pool, upgraderRBAC(), nil, &mcpResolver{ok: true}, loader)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.upgrade",
		`{"name":"redis-prod","to_version":"v3"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Errorf("data.code = %q, want validation-failed", data.Code)
	}
}

func TestToolsCall_IncarnationUpgrade_Locked(t *testing.T) {
	// Status error_locked → tx-level ErrIncarnationLocked → incarnation-locked
	// (same path covers the migration_failed status, see errors.go §
	// mcpCodeMigrationFailed). PrepareUpgrade succeeds (status isn't read
	// before the tx).
	pool := &fakePool{incFn: incAtVersion("v1", 1, incarnation.StatusErrorLocked)}
	loader := &mcpLoader{targetSchema: 2, chain: oneStepChain(t)}
	h, rec := newTestHandlerFull(t, pool, upgraderRBAC(), nil, &mcpResolver{ok: true}, loader)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.upgrade",
		`{"name":"redis-prod","to_version":"v2"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeIncarnationLocked {
		t.Errorf("data.code = %q, want incarnation-locked", data.Code)
	}
	if len(rec.events) != 0 {
		t.Error("locked upgrade must not write audit")
	}
}

func TestToolsCall_IncarnationUpgrade_NotFound(t *testing.T) {
	pool := &fakePool{incFn: func(string) (*incarnation.Incarnation, error) { return nil, pgx.ErrNoRows }}
	h, _ := newTestHandlerFull(t, pool, upgraderRBAC(), nil, &mcpResolver{ok: true}, &mcpLoader{targetSchema: 2})
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.upgrade",
		`{"name":"ghost","to_version":"v2"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeNotFound {
		t.Errorf("data.code = %q, want not-found", data.Code)
	}
}

func TestToolsCall_IncarnationUpgrade_RBACForbidden(t *testing.T) {
	// RBAC empty → deny. SelectByName RESOLVES scope (covens ∪ {name}) for the
	// OR-Check (mirrors REST middleware), then the enforcer denies → forbidden.
	pool := &fakePool{incFn: incAtVersion("v1", 1, incarnation.StatusReady)}
	h, rec := newTestHandlerFull(t, pool, nil, nil, &mcpResolver{ok: true}, &mcpLoader{targetSchema: 2})
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.upgrade",
		`{"name":"redis-prod","to_version":"v2"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeForbidden {
		t.Errorf("data.code = %q, want forbidden", data.Code)
	}
	if len(rec.events) != 0 {
		t.Error("denied upgrade must not write audit")
	}
}

func TestToolsCall_IncarnationUpgrade_LoaderNotConfigured(t *testing.T) {
	h, _ := newTestHandlerFull(t, &fakePool{}, upgraderRBAC(), nil, nil, nil)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.upgrade",
		`{"name":"redis-prod","to_version":"v2"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeInternalError {
		t.Errorf("data.code = %q, want internal-error", data.Code)
	}
}
