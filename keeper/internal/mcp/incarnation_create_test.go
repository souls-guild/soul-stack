package mcp

import (
	"encoding/json"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/shared/audit"
)

func creatorRBAC() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "creator", Operators: []string{"archon-alice"}, Permissions: []string{"incarnation.create"}},
		},
	}
}

func TestToolsCall_IncarnationCreate_Success(t *testing.T) {
	pool := &fakePool{incInsertFn: func(_, _ string) error { return nil }}
	starter := &mcpStarter{}
	h, rec := newTestHandlerFull(t, pool, creatorRBAC(), starter, &mcpResolver{ok: true}, nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"name":"redis-prod","service":"redis","input":{"replicas":3}}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var res toolsCallResult
	_ = json.Unmarshal(resp.Result, &res)
	var out incarnationCreateOutput
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Incarnation != "redis-prod" {
		t.Errorf("incarnation = %q", out.Incarnation)
	}
	// auto_create по умолчанию true (манифест без lifecycle-блока) → apply_id есть.
	if out.ApplyID == nil {
		t.Fatalf("_apply_id is nil (auto_create=true expected)")
	}
	if !audit.IsValidULID(*out.ApplyID) {
		t.Errorf("_apply_id not a ULID: %q", *out.ApplyID)
	}
	// scenario `create` запущен с тем же apply_id и input.
	if starter.calls != 1 {
		t.Fatalf("scenario start calls = %d, want 1", starter.calls)
	}
	if starter.gotSpec.ScenarioName != "create" || starter.gotSpec.ApplyID != *out.ApplyID {
		t.Errorf("run spec mismatch: %+v", starter.gotSpec)
	}
	if starter.gotSpec.Input["replicas"] != float64(3) {
		t.Errorf("input not propagated: %+v", starter.gotSpec.Input)
	}

	// audit: EventIncarnationCreated, source=mcp, payload {name, service, apply_id}.
	if len(rec.events) != 1 {
		t.Fatalf("want 1 audit event, got %d", len(rec.events))
	}
	ev := rec.events[0]
	if ev.EventType != audit.EventIncarnationCreated || ev.Source != audit.SourceMCP {
		t.Errorf("event = %q / source %q", ev.EventType, ev.Source)
	}
	if ev.Payload["name"] != "redis-prod" || ev.Payload["service"] != "redis" || ev.Payload["apply_id"] != *out.ApplyID {
		t.Errorf("audit payload = %+v", ev.Payload)
	}
}

func TestToolsCall_IncarnationCreate_AlreadyExists(t *testing.T) {
	pool := &fakePool{incInsertFn: func(_, _ string) error { return incarnation.ErrIncarnationAlreadyExists }}
	starter := &mcpStarter{}
	h, rec := newTestHandlerFull(t, pool, creatorRBAC(), starter, &mcpResolver{ok: true}, nil)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"name":"redis-prod","service":"redis"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeIncarnationExists {
		t.Errorf("data.code = %q, want incarnation-already-exists", data.Code)
	}
	if starter.calls != 0 {
		t.Error("scenario must NOT start when insert fails")
	}
	if len(rec.events) != 0 {
		t.Error("failed create must not write audit")
	}
}

func TestToolsCall_IncarnationCreate_RBACForbidden(t *testing.T) {
	pool := &fakePool{incInsertFn: func(_, _ string) error {
		t.Error("Create must NOT run when RBAC denies")
		return nil
	}}
	h, rec := newTestHandlerFull(t, pool, nil, &mcpStarter{}, &mcpResolver{ok: true}, nil)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"name":"redis-prod","service":"redis"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeForbidden {
		t.Errorf("data.code = %q, want forbidden", data.Code)
	}
	if len(rec.events) != 0 {
		t.Error("denied create must not write audit")
	}
}

func TestToolsCall_IncarnationCreate_RunnerNotConfigured(t *testing.T) {
	// runner/registry nil → internal-error (паритет REST 500 без runner-а).
	h, _ := newTestHandlerFull(t, &fakePool{}, creatorRBAC(), nil, nil, nil)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"name":"redis-prod","service":"redis"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeInternalError {
		t.Errorf("data.code = %q, want internal-error", data.Code)
	}
}

func TestToolsCall_IncarnationCreate_ServiceNotRegistered(t *testing.T) {
	h, _ := newTestHandlerFull(t, &fakePool{}, creatorRBAC(), &mcpStarter{}, &mcpResolver{ok: false}, nil)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"name":"redis-prod","service":"redis"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Errorf("data.code = %q, want validation-failed", data.Code)
	}
}

func TestToolsCall_IncarnationCreate_InvalidName(t *testing.T) {
	h, _ := newTestHandlerFull(t, &fakePool{}, creatorRBAC(), &mcpStarter{}, &mcpResolver{ok: true}, nil)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"name":"Bad_Name","service":"redis"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Errorf("data.code = %q, want validation-failed", data.Code)
	}
}

func TestToolsCall_IncarnationCreate_InvalidCoven(t *testing.T) {
	pool := &fakePool{incInsertFn: func(_, _ string) error {
		t.Error("Create must NOT run on invalid coven format")
		return nil
	}}
	h, _ := newTestHandlerFull(t, pool, creatorRBAC(), &mcpStarter{}, &mcpResolver{ok: true}, nil)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"name":"redis-prod","service":"redis","covens":["Bad_Coven"]}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Errorf("data.code = %q, want validation-failed", data.Code)
	}
}

// TestToolsCall_IncarnationCreate_ScopeDeniesForeignCoven — оператор со scope
// `incarnation.create on coven=dev` НЕ может создать incarnation с covens=[prod]
// (least-privilege, паритет REST IncarnationCreateScopeSelector). Deny ДО
// insert: fail-closed, ни вставки, ни scenario-start, ни audit.
func TestToolsCall_IncarnationCreate_ScopeDeniesForeignCoven(t *testing.T) {
	pool := &fakePool{incInsertFn: func(_, _ string) error {
		t.Error("Create must NOT run when create-scope denies")
		return nil
	}}
	starter := &mcpStarter{}
	h, rec := newTestHandlerFull(t, pool, scopedRBAC("incarnation.create on coven=dev"),
		starter, &mcpResolver{ok: true}, nil)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"name":"redis-svc","service":"redis","covens":["prod"]}`)
	if resp.Error == nil {
		t.Fatal("expected forbidden")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeForbidden {
		t.Errorf("data.code = %q, want forbidden", data.Code)
	}
	if starter.calls != 0 {
		t.Error("denied create must not start scenario")
	}
	if len(rec.events) != 0 {
		t.Error("denied create must not write audit")
	}
}

// TestToolsCall_IncarnationCreate_ScopeAllowsMatchingCoven — тот же оператор
// (scope coven=dev) МОЖЕТ создать incarnation с covens=[dev].
func TestToolsCall_IncarnationCreate_ScopeAllowsMatchingCoven(t *testing.T) {
	pool := &fakePool{incInsertFn: func(_, _ string) error { return nil }}
	starter := &mcpStarter{}
	h, _ := newTestHandlerFull(t, pool, scopedRBAC("incarnation.create on coven=dev"),
		starter, &mcpResolver{ok: true}, nil)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"name":"redis-svc","service":"redis","covens":["dev"]}`)
	if resp.Error != nil {
		t.Fatalf("matching coven should pass: %+v", resp.Error)
	}
	if starter.calls != 1 {
		t.Errorf("scenario start calls = %d, want 1", starter.calls)
	}
}

// TestToolsCall_IncarnationCreate_WildcardAnyCoven — `*` (cluster-admin)
// создаёт incarnation с любыми covens (no-regression).
func TestToolsCall_IncarnationCreate_WildcardAnyCoven(t *testing.T) {
	pool := &fakePool{incInsertFn: func(_, _ string) error { return nil }}
	starter := &mcpStarter{}
	h, _ := newTestHandlerFull(t, pool, wildcardRBAC(),
		starter, &mcpResolver{ok: true}, nil)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"name":"redis-svc","service":"redis","covens":["prod","staging"]}`)
	if resp.Error != nil {
		t.Fatalf("wildcard should pass: %+v", resp.Error)
	}
	if starter.calls != 1 {
		t.Errorf("scenario start calls = %d, want 1", starter.calls)
	}
}

// scenarioMCPRequiredInput — scenario `create` с required-полем `name`.
const scenarioMCPRequiredInput = `name: create
state_changes: {}
input:
  name:
    type: string
    required: true
tasks:
  - name: noop
    module: core.exec.run
    params:
      cmd: echo
    changed_when: "false"
`

// TestToolsCall_IncarnationCreate_RequiredInputMissing — MCP-паритет REST:
// create без required-поля отвергается sync (validation-failed), scenario НЕ
// запускается, audit не пишется.
func TestToolsCall_IncarnationCreate_RequiredInputMissing(t *testing.T) {
	pool := &fakePool{incInsertFn: func(_, _ string) error {
		t.Error("Create must NOT insert when input invalid")
		return nil
	}}
	starter := &mcpStarter{}
	loader := &mcpLoader{scenarioYAML: scenarioMCPRequiredInput}
	h, rec := newTestHandlerFull(t, pool, creatorRBAC(), starter, &mcpResolver{ok: true}, loader)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"name":"ba","service":"redis","input":{}}`)
	if resp.Error == nil {
		t.Fatal("expected validation error for missing required input")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Errorf("data.code = %q, want validation-failed", data.Code)
	}
	if starter.calls != 0 {
		t.Error("scenario must NOT start when input invalid")
	}
	if len(rec.events) != 0 {
		t.Error("invalid input must not write audit")
	}
}

// TestToolsCall_IncarnationCreate_RequiredInputProvided — required передан →
// проходит, scenario запускается.
func TestToolsCall_IncarnationCreate_RequiredInputProvided(t *testing.T) {
	pool := &fakePool{incInsertFn: func(_, _ string) error { return nil }}
	starter := &mcpStarter{}
	loader := &mcpLoader{scenarioYAML: scenarioMCPRequiredInput}
	h, _ := newTestHandlerFull(t, pool, creatorRBAC(), starter, &mcpResolver{ok: true}, loader)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"name":"ba","service":"redis","input":{"name":"alice"}}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if starter.calls != 1 {
		t.Fatalf("scenario start calls = %d, want 1", starter.calls)
	}
}
