package mcp

// Guard-тесты MCP-tool-а keeper.incarnation.rerun-create (паритет REST
// IncarnationHandler.RerunCreateTyped, incarnation_rerun_test.go): снятие
// error_locked + перезапуск создавшего сценария одним действием. Покрывают
// happy-path (202 + apply_id + audit), RBAC-scope-deny по чужому coven,
// статус-гейт (ErrIncarnationNotErrorLocked), scope=created-scenario
// (ErrRerunScenarioNotCreate) и валидацию аргументов.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// rerunRBAC — роль с permission incarnation.create-rerun (1:1 с tool-ом),
// привязанная к archon-alice.
func rerunRBAC() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "rerunner", Operators: []string{"archon-alice"}, Permissions: []string{"incarnation.create-rerun"}},
		},
	}
}

// incLocked — incFn, отдающий error_locked-инкарнацию с заданными covens и
// создавшим сценарием (для FOR UPDATE-select UnlockForRerun: status=error_locked,
// created_scenario). createdScenario "" → NULL (bare-инкарнация); иначе указатель
// на имя. service=redis.
func incLocked(covens []string, createdScenario string) func(string) (*incarnation.Incarnation, error) {
	return func(name string) (*incarnation.Incarnation, error) {
		now := time.Now().UTC()
		var cs *string
		if createdScenario != "" {
			cs = &createdScenario
		}
		return &incarnation.Incarnation{
			Name: name, Service: "redis", ServiceVersion: "v1",
			StateSchemaVersion: 1, Status: incarnation.StatusErrorLocked,
			State: map[string]any{}, Covens: covens,
			CreatedScenario: cs,
			CreatedAt:       now, UpdatedAt: now,
		}, nil
	}
}

// TestToolsCall_IncarnationRerunCreate_Success — happy-path: из error_locked
// снимается блок и стартует ровно один scenario `create` (created_scenario) с
// общим apply_id; ответ {_apply_id, incarnation}; audit incarnation.create_rerun
// (source=mcp, correlation_id=apply_id) с reason + previous_status=error_locked.
func TestToolsCall_IncarnationRerunCreate_Success(t *testing.T) {
	pool := &fakePool{incFn: incLocked(nil, "create")}
	starter := &mcpStarter{}
	h, rec := newTestHandlerFull(t, pool, rerunRBAC(), starter, &mcpResolver{ok: true}, nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.rerun-create",
		`{"name":"redis-prod","reason":"rerun bootstrap verified"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var res toolsCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var out incarnationRerunCreateOutput
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}

	if out.Incarnation != "redis-prod" {
		t.Errorf("incarnation = %q", out.Incarnation)
	}
	if !audit.IsValidULID(out.ApplyID) {
		t.Errorf("_apply_id not a ULID: %q", out.ApplyID)
	}
	// Ровно один новый create-прогон с тем же apply_id и FromLocked=true.
	if starter.calls != 1 {
		t.Fatalf("scenario start calls = %d, want 1", starter.calls)
	}
	if starter.gotSpec.ScenarioName != "create" {
		t.Errorf("ScenarioName = %q, want create", starter.gotSpec.ScenarioName)
	}
	if starter.gotSpec.ApplyID != out.ApplyID {
		t.Errorf("run apply_id = %q, want %q", starter.gotSpec.ApplyID, out.ApplyID)
	}
	if !starter.gotSpec.FromLocked {
		t.Error("rerun must start scenario with FromLocked=true")
	}
	if starter.gotSpec.ServiceRef.Ref != "v1" {
		t.Errorf("ServiceRef.Ref = %q, want v1 (incarnation.service_version)", starter.gotSpec.ServiceRef.Ref)
	}

	// audit incarnation.create_rerun, source=mcp, correlation_id=apply_id.
	if len(rec.events) != 1 {
		t.Fatalf("want 1 audit event, got %d", len(rec.events))
	}
	ev := rec.events[0]
	if ev.EventType != audit.EventIncarnationCreateRerun || ev.Source != audit.SourceMCP {
		t.Errorf("event = %q / source %q", ev.EventType, ev.Source)
	}
	if ev.CorrelationID != out.ApplyID {
		t.Errorf("correlation_id = %q, want %q", ev.CorrelationID, out.ApplyID)
	}
	if ev.Payload["reason"] != "rerun bootstrap verified" {
		t.Errorf("audit reason = %v", ev.Payload["reason"])
	}
	if ev.Payload["previous_status"] != "error_locked" {
		t.Errorf("audit previous_status = %v, want error_locked", ev.Payload["previous_status"])
	}
	if ev.Payload["apply_id"] != out.ApplyID {
		t.Errorf("audit apply_id = %v, want %q", ev.Payload["apply_id"], out.ApplyID)
	}
}

// TestToolsCall_IncarnationRerunCreate_ScopeDeniesForeignCoven — оператор со scope
// `incarnation.create-rerun on coven=dev` НЕ может rerun-нуть prod-инкарнацию
// (covens=[prod]) через MCP. Зеркало REST scope-защиты: без OR-Check по covens ∪
// {name} под-привилегированный оператор обходил бы REST через MCP. Deny ДО
// unlock/start: ни прогона, ни audit.
func TestToolsCall_IncarnationRerunCreate_ScopeDeniesForeignCoven(t *testing.T) {
	pool := &fakePool{
		incFn:    incLocked([]string{"prod"}, "create"),
		beginErr: errFakeUnexpected{sql: "BeginTx must not run when rerun-scope denies"},
	}
	starter := &mcpStarter{}
	h, rec := newTestHandlerFull(t, pool, scopedRBAC("incarnation.create-rerun on coven=dev"),
		starter, &mcpResolver{ok: true}, nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.rerun-create",
		`{"name":"redis-prod","reason":"x"}`)
	expectForbidden(t, resp, "rerun-create")
	if starter.calls != 0 {
		t.Error("denied rerun must not start scenario")
	}
	if len(rec.events) != 0 {
		t.Error("denied rerun must not write audit")
	}
}

// TestToolsCall_IncarnationRerunCreate_ScopeAllowsMatchingCoven — тот же оператор
// (scope coven=prod) МОЖЕТ rerun prod-инкарнацию: НЕ forbidden, прогон стартует.
func TestToolsCall_IncarnationRerunCreate_ScopeAllowsMatchingCoven(t *testing.T) {
	pool := &fakePool{incFn: incLocked([]string{"prod"}, "create")}
	starter := &mcpStarter{}
	h, _ := newTestHandlerFull(t, pool, scopedRBAC("incarnation.create-rerun on coven=prod"),
		starter, &mcpResolver{ok: true}, nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.rerun-create",
		`{"name":"redis-prod","reason":"matching scope"}`)
	expectNotForbidden(t, resp, "rerun-create")
	if resp.Error != nil {
		t.Fatalf("matching coven should fully pass: %+v", resp.Error)
	}
	if starter.calls != 1 {
		t.Errorf("scenario start calls = %d, want 1", starter.calls)
	}
}

// TestToolsCall_IncarnationRerunCreate_NotErrorLocked — из ready/applying rerun
// отклонён (incarnation-locked), прогон НЕ стартует. UnlockForRerun читает
// статус под FOR UPDATE → ErrIncarnationNotErrorLocked.
func TestToolsCall_IncarnationRerunCreate_NotErrorLocked(t *testing.T) {
	for _, status := range []incarnation.Status{incarnation.StatusReady, incarnation.StatusApplying} {
		t.Run(string(status), func(t *testing.T) {
			pool := &fakePool{incFn: func(name string) (*incarnation.Incarnation, error) {
				now := time.Now().UTC()
				cs := "create"
				return &incarnation.Incarnation{
					Name: name, Service: "redis", ServiceVersion: "v1",
					StateSchemaVersion: 1, Status: status,
					State: map[string]any{}, CreatedScenario: &cs,
					CreatedAt: now, UpdatedAt: now,
				}, nil
			}}
			starter := &mcpStarter{}
			h, rec := newTestHandlerFull(t, pool, rerunRBAC(), starter, &mcpResolver{ok: true}, nil)

			resp := callTool(t, h, "archon-alice", "keeper.incarnation.rerun-create",
				`{"name":"redis-prod","reason":"x"}`)
			if resp.Error == nil {
				t.Fatalf("status=%s: expected incarnation-locked", status)
			}
			if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeIncarnationLocked {
				t.Errorf("status=%s: data.code = %q, want incarnation-locked", status, data.Code)
			}
			if starter.calls != 0 {
				t.Errorf("status=%s: scenario start calls = %d, want 0", status, starter.calls)
			}
			if len(rec.events) != 0 {
				t.Errorf("status=%s: rejected rerun must not write audit", status)
			}
		})
	}
}

// TestToolsCall_IncarnationRerunCreate_ScenarioNotCreate — error_locked, но
// последний упавший сценарий (state_history) НЕ создавший (add_user ≠ create) →
// incarnation-locked (ErrRerunScenarioNotCreate), прогон НЕ стартует. rerun-create
// перезапускает строго bootstrap, а не фактически провалившуюся day-2 операцию.
func TestToolsCall_IncarnationRerunCreate_ScenarioNotCreate(t *testing.T) {
	pool := &fakePool{
		incFn:          incLocked(nil, "create"),
		lastScenarioFn: func(string) (string, error) { return "add_user", nil },
	}
	starter := &mcpStarter{}
	h, rec := newTestHandlerFull(t, pool, rerunRBAC(), starter, &mcpResolver{ok: true}, nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.rerun-create",
		`{"name":"redis-prod","reason":"rerun add_user verified"}`)
	if resp.Error == nil {
		t.Fatal("expected incarnation-locked (last scenario != create)")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeIncarnationLocked {
		t.Errorf("data.code = %q, want incarnation-locked", data.Code)
	}
	if starter.calls != 0 {
		t.Error("scenario-not-create must not start scenario")
	}
	if len(rec.events) != 0 {
		t.Error("scenario-not-create must not write audit")
	}
}

// TestToolsCall_IncarnationRerunCreate_BareIncarnation_409 — GUARD Фаза 2 (паритет
// REST TestRerunCreate_BareIncarnation_409): bare-инкарнация (created_scenario IS
// NULL — создана БЕЗ bootstrap-сценария) в error_locked → incarnation-locked
// (ErrRerunScenarioNotCreate), прогон НЕ стартует. rerun-create неприменим:
// перезапускать нечего. Регресс = NULL коалесцируется в `create` и rerun запускает
// несуществующий bootstrap. incLocked(nil, "") даёт CreatedScenario=nil →
// rerunForUpdateRow проецирует NULL → UnlockForRerun отказывает ДО lastScenario-проверки.
func TestToolsCall_IncarnationRerunCreate_BareIncarnation_409(t *testing.T) {
	pool := &fakePool{incFn: incLocked(nil, "")}
	starter := &mcpStarter{}
	h, rec := newTestHandlerFull(t, pool, rerunRBAC(), starter, &mcpResolver{ok: true}, nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.rerun-create",
		`{"name":"redis-bare","reason":"rerun bare"}`)
	if resp.Error == nil {
		t.Fatal("expected incarnation-locked (bare-инкарнация: created_scenario IS NULL)")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeIncarnationLocked {
		t.Errorf("data.code = %q, want incarnation-locked", data.Code)
	}
	if starter.calls != 0 {
		t.Errorf("scenario start calls = %d, want 0 (bare → rerun неприменим)", starter.calls)
	}
	if len(rec.events) != 0 {
		t.Error("bare rerun must not write audit")
	}
}

// TestToolsCall_IncarnationRerunCreate_NotFound — несуществующая инкарнация → 404,
// прогон НЕ стартует.
func TestToolsCall_IncarnationRerunCreate_NotFound(t *testing.T) {
	pool := &fakePool{} // incFn nil → SelectByName отдаёт pgx.ErrNoRows
	starter := &mcpStarter{}
	h, _ := newTestHandlerFull(t, pool, rerunRBAC(), starter, &mcpResolver{ok: true}, nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.rerun-create",
		`{"name":"ghost","reason":"x"}`)
	if resp.Error == nil {
		t.Fatal("expected not-found")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeNotFound {
		t.Errorf("data.code = %q, want not-found", data.Code)
	}
	if starter.calls != 0 {
		t.Errorf("scenario start calls = %d, want 0", starter.calls)
	}
}

// TestToolsCall_IncarnationRerunCreate_EmptyReason — пустой reason → validation-
// failed (явное подтверждение обязательно), прогон НЕ стартует.
func TestToolsCall_IncarnationRerunCreate_EmptyReason(t *testing.T) {
	pool := &fakePool{incFn: incLocked(nil, "create")}
	starter := &mcpStarter{}
	h, _ := newTestHandlerFull(t, pool, rerunRBAC(), starter, &mcpResolver{ok: true}, nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.rerun-create",
		`{"name":"redis-prod","reason":""}`)
	if resp.Error == nil {
		t.Fatal("expected validation-failed for empty reason")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Errorf("data.code = %q, want validation-failed", data.Code)
	}
	if starter.calls != 0 {
		t.Error("empty reason must not start scenario")
	}
}

// TestToolsCall_IncarnationRerunCreate_RunnerNotConfigured — runner/registry nil →
// internal-error (паритет REST 500 без runner-а): rerun перезапускает scenario.
func TestToolsCall_IncarnationRerunCreate_RunnerNotConfigured(t *testing.T) {
	h, _ := newTestHandlerFull(t, &fakePool{}, rerunRBAC(), nil, nil, nil)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.rerun-create",
		`{"name":"redis-prod","reason":"x"}`)
	if resp.Error == nil {
		t.Fatal("expected internal-error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeInternalError {
		t.Errorf("data.code = %q, want internal-error", data.Code)
	}
}
