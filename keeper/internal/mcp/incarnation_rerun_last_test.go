package mcp

// Guard-тесты MCP-tool-а keeper.incarnation.rerun-last (паритет REST
// IncarnationHandler.RerunLastTyped, incarnation_rerun_test.go): снятие
// error_locked + перезапуск последнего упавшего сценария одним действием.
// Покрывают happy-path create/day-2 (202 + apply_id + scenario + audit),
// RBAC-scope-deny по чужому coven, статус-гейт, fail-closed recipe-null и
// валидацию аргументов.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// rerunRBAC — роль с permission incarnation.rerun-last (1:1 с tool-ом),
// привязанная к archon-alice.
func rerunRBAC() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "rerunner", Operators: []string{"archon-alice"}, Permissions: []string{"incarnation.rerun-last"}},
		},
	}
}

// incLocked — incFn, отдающий error_locked-инкарнацию с заданными covens и
// создавшим сценарием. createdScenario "" → NULL (bare-инкарнация). service=redis.
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

// TestToolsCall_IncarnationRerunLast_Success — happy-path create: из error_locked
// снимается блок и стартует ровно один scenario `create` (created_scenario) с
// общим apply_id; ответ {_apply_id, incarnation, scenario}; audit
// incarnation.rerun_last (source=mcp, correlation_id=apply_id) с reason +
// previous_status + scenario.
func TestToolsCall_IncarnationRerunLast_Success(t *testing.T) {
	pool := &fakePool{incFn: incLocked(nil, "create")}
	starter := &mcpStarter{}
	h, rec := newTestHandlerFull(t, pool, rerunRBAC(), starter, &mcpResolver{ok: true}, nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.rerun-last",
		`{"name":"redis-prod","reason":"rerun bootstrap verified"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var res toolsCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var out incarnationRerunLastOutput
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}

	if out.Incarnation != "redis-prod" {
		t.Errorf("incarnation = %q", out.Incarnation)
	}
	if out.Scenario != "create" {
		t.Errorf("scenario = %q, want create", out.Scenario)
	}
	if !audit.IsValidULID(out.ApplyID) {
		t.Errorf("_apply_id not a ULID: %q", out.ApplyID)
	}
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

	if len(rec.events) != 1 {
		t.Fatalf("want 1 audit event, got %d", len(rec.events))
	}
	ev := rec.events[0]
	if ev.EventType != audit.EventIncarnationRerunLast || ev.Source != audit.SourceMCP {
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
	if ev.Payload["scenario"] != "create" {
		t.Errorf("audit scenario = %v, want create", ev.Payload["scenario"])
	}
	if ev.Payload["apply_id"] != out.ApplyID {
		t.Errorf("audit apply_id = %v, want %q", ev.Payload["apply_id"], out.ApplyID)
	}
}

// incLockedSpec — incFn error_locked-инкарнации с заданным spec (проверка проброса
// spec.input в RunSpec.Input на create-пути). created_scenario="create", service=redis.
func incLockedSpec(spec map[string]any) func(string) (*incarnation.Incarnation, error) {
	return func(name string) (*incarnation.Incarnation, error) {
		now := time.Now().UTC()
		cs := "create"
		return &incarnation.Incarnation{
			Name: name, Service: "redis", ServiceVersion: "v1",
			StateSchemaVersion: 1, Status: incarnation.StatusErrorLocked,
			State: map[string]any{}, Spec: spec,
			CreatedScenario: &cs,
			CreatedAt:       now, UpdatedAt: now,
		}, nil
	}
}

// TestToolsCall_IncarnationRerunLast_ReusesStoredInput — create-путь GUARD (паритет
// REST TestRerunLast_ReusesStoredInput_202): rerun-last инкарнации с сохранённым
// в spec.input оператор-input → input проброшен в RunSpec.Input перезапускаемого
// bootstrap-прогона (НЕ nil, НЕ дефолты).
func TestToolsCall_IncarnationRerunLast_ReusesStoredInput(t *testing.T) {
	spec := map[string]any{"input": map[string]any{
		"version":         "8.6.1",
		"shards":          float64(3), // jsonb-число
		"connection_mode": "cluster",
	}}
	pool := &fakePool{incFn: incLockedSpec(spec)}
	starter := &mcpStarter{}
	h, _ := newTestHandlerFull(t, pool, rerunRBAC(), starter, &mcpResolver{ok: true}, nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.rerun-last",
		`{"name":"redis-cluster-prod","reason":"rerun cluster bootstrap"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if starter.calls != 1 {
		t.Fatalf("scenario start calls = %d, want 1", starter.calls)
	}
	gotInput := starter.gotSpec.Input
	if gotInput == nil {
		t.Fatal("RunSpec.Input = nil — stored spec.input НЕ проброшен (create-путь регресс)")
	}
	if gotInput["version"] != "8.6.1" {
		t.Errorf("RunSpec.Input[version] = %v, want 8.6.1 (stored)", gotInput["version"])
	}
	if shards, ok := gotInput["shards"].(float64); !ok || shards != 3 {
		t.Errorf("RunSpec.Input[shards] = %v (%T), want 3", gotInput["shards"], gotInput["shards"])
	}
	if gotInput["connection_mode"] != "cluster" {
		t.Errorf("RunSpec.Input[connection_mode] = %v, want cluster (stored)", gotInput["connection_mode"])
	}
}

// TestToolsCall_IncarnationRerunLast_NoStoredInput_NilInput — create-путь контраст:
// инкарнация без spec.input → RunSpec.Input nil, прогон стартует штатно.
func TestToolsCall_IncarnationRerunLast_NoStoredInput_NilInput(t *testing.T) {
	pool := &fakePool{incFn: incLocked(nil, "create")} // Spec=nil → spec=`{}`
	starter := &mcpStarter{}
	h, _ := newTestHandlerFull(t, pool, rerunRBAC(), starter, &mcpResolver{ok: true}, nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.rerun-last",
		`{"name":"redis-prod","reason":"rerun no-input bootstrap"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if starter.calls != 1 {
		t.Fatalf("scenario start calls = %d, want 1", starter.calls)
	}
	if starter.gotSpec.Input != nil {
		t.Errorf("RunSpec.Input = %v, want nil (spec без input)", starter.gotSpec.Input)
	}
}

// TestToolsCall_IncarnationRerunLast_Day2ReusesRecipeInput — day-2 happy-path
// (паритет REST TestRerunLast_Day2_ReusesRecipeInput_202): последний упавший —
// add_user (≠ created `create`), его input берётся из recipe apply_run → 202,
// RunSpec.ScenarioName=="add_user", Input из recipe (не spec.input), scenario в
// reply/audit == add_user.
func TestToolsCall_IncarnationRerunLast_Day2ReusesRecipeInput(t *testing.T) {
	spec := map[string]any{"input": map[string]any{"version": "8.6.1"}} // НЕ должен просочиться
	pool := &fakePool{
		incFn:          incLockedSpec(spec),
		lastScenarioFn: func(string) (string, error) { return "add_user", nil },
		recipeFn: func(string) ([]byte, error) {
			return []byte(`{"scenario_name":"add_user","input":{"user":"alice"}}`), nil
		},
	}
	starter := &mcpStarter{}
	h, rec := newTestHandlerFull(t, pool, rerunRBAC(), starter, &mcpResolver{ok: true}, nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.rerun-last",
		`{"name":"redis-prod","reason":"rerun add_user verified"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var res toolsCallResult
	_ = json.Unmarshal(resp.Result, &res)
	var out incarnationRerunLastOutput
	_ = json.Unmarshal(res.StructuredContent, &out)

	if out.Scenario != "add_user" {
		t.Errorf("scenario = %q, want add_user", out.Scenario)
	}
	if starter.calls != 1 {
		t.Fatalf("scenario start calls = %d, want 1", starter.calls)
	}
	if starter.gotSpec.ScenarioName != "add_user" {
		t.Errorf("ScenarioName = %q, want add_user", starter.gotSpec.ScenarioName)
	}
	gotInput := starter.gotSpec.Input
	if gotInput == nil || gotInput["user"] != "alice" {
		t.Fatalf("RunSpec.Input = %v, want {user:alice} (recipe.input)", gotInput)
	}
	if _, leaked := gotInput["version"]; leaked {
		t.Error("RunSpec.Input несёт spec.input[version] — day-2 обязан брать recipe.input")
	}
	if rec.events[0].Payload["scenario"] != "add_user" {
		t.Errorf("audit scenario = %v, want add_user", rec.events[0].Payload["scenario"])
	}
}

// TestToolsCall_IncarnationRerunLast_Day2BareIncarnation — bare-инкарнация
// (created_scenario IS NULL) залочена day-2 → rerun-last применим через recipe-путь
// (было: 409). ScenarioName из last-run, Input из recipe.
func TestToolsCall_IncarnationRerunLast_Day2BareIncarnation(t *testing.T) {
	pool := &fakePool{
		incFn:          incLocked(nil, ""), // created_scenario = NULL
		lastScenarioFn: func(string) (string, error) { return "update_acl", nil },
		recipeFn: func(string) ([]byte, error) {
			return []byte(`{"input":{"acl":"readonly"}}`), nil
		},
	}
	starter := &mcpStarter{}
	h, _ := newTestHandlerFull(t, pool, rerunRBAC(), starter, &mcpResolver{ok: true}, nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.rerun-last",
		`{"name":"redis-bare","reason":"rerun bare day-2"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if starter.calls != 1 {
		t.Fatalf("scenario start calls = %d, want 1", starter.calls)
	}
	if starter.gotSpec.ScenarioName != "update_acl" {
		t.Errorf("ScenarioName = %q, want update_acl", starter.gotSpec.ScenarioName)
	}
	if starter.gotSpec.Input["acl"] != "readonly" {
		t.Errorf("RunSpec.Input[acl] = %v, want readonly (recipe.input)", starter.gotSpec.Input["acl"])
	}
}

// TestToolsCall_IncarnationRerunLast_Day2RecipeUnavailable — day-2, но recipe нет
// (recipe IS NULL / apply_run вычищен → ErrNoRows): fail-closed rerun-input-
// unavailable (отдельный код от incarnation-locked, симметрия REST
// TypeRerunInputUnavailable), прогон НЕ стартует, audit НЕ пишется.
func TestToolsCall_IncarnationRerunLast_Day2RecipeUnavailable(t *testing.T) {
	pool := &fakePool{
		incFn:          incLocked(nil, "create"),
		lastScenarioFn: func(string) (string, error) { return "add_user", nil },
		// recipeFn nil → recipe-probe вернёт ErrNoRows (fail-closed).
	}
	starter := &mcpStarter{}
	h, rec := newTestHandlerFull(t, pool, rerunRBAC(), starter, &mcpResolver{ok: true}, nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.rerun-last",
		`{"name":"redis-prod","reason":"rerun add_user"}`)
	if resp.Error == nil {
		t.Fatal("expected rerun-input-unavailable (recipe недоступен)")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeRerunInputUnavailable {
		t.Errorf("data.code = %q, want rerun-input-unavailable", data.Code)
	}
	if starter.calls != 0 {
		t.Error("fail-closed recipe must not start scenario")
	}
	if len(rec.events) != 0 {
		t.Error("fail-closed recipe must not write audit")
	}
}

// TestToolsCall_IncarnationRerunLast_ScopeDeniesForeignCoven — оператор со scope
// `incarnation.rerun-last on coven=dev` НЕ может rerun-нуть prod-инкарнацию
// (covens=[prod]) через MCP. Deny ДО unlock/start: ни прогона, ни audit.
func TestToolsCall_IncarnationRerunLast_ScopeDeniesForeignCoven(t *testing.T) {
	pool := &fakePool{
		incFn:    incLocked([]string{"prod"}, "create"),
		beginErr: errFakeUnexpected{sql: "BeginTx must not run when rerun-scope denies"},
	}
	starter := &mcpStarter{}
	h, rec := newTestHandlerFull(t, pool, scopedRBAC("incarnation.rerun-last on coven=dev"),
		starter, &mcpResolver{ok: true}, nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.rerun-last",
		`{"name":"redis-prod","reason":"x"}`)
	expectForbidden(t, resp, "rerun-last")
	if starter.calls != 0 {
		t.Error("denied rerun must not start scenario")
	}
	if len(rec.events) != 0 {
		t.Error("denied rerun must not write audit")
	}
}

// TestToolsCall_IncarnationRerunLast_ScopeAllowsMatchingCoven — тот же оператор
// (scope coven=prod) МОЖЕТ rerun prod-инкарнацию: НЕ forbidden, прогон стартует.
func TestToolsCall_IncarnationRerunLast_ScopeAllowsMatchingCoven(t *testing.T) {
	pool := &fakePool{incFn: incLocked([]string{"prod"}, "create")}
	starter := &mcpStarter{}
	h, _ := newTestHandlerFull(t, pool, scopedRBAC("incarnation.rerun-last on coven=prod"),
		starter, &mcpResolver{ok: true}, nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.rerun-last",
		`{"name":"redis-prod","reason":"matching scope"}`)
	expectNotForbidden(t, resp, "rerun-last")
	if resp.Error != nil {
		t.Fatalf("matching coven should fully pass: %+v", resp.Error)
	}
	if starter.calls != 1 {
		t.Errorf("scenario start calls = %d, want 1", starter.calls)
	}
}

// TestToolsCall_IncarnationRerunLast_NotErrorLocked — из ready/applying rerun
// отклонён (incarnation-locked), прогон НЕ стартует.
func TestToolsCall_IncarnationRerunLast_NotErrorLocked(t *testing.T) {
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

			resp := callTool(t, h, "archon-alice", "keeper.incarnation.rerun-last",
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

// TestToolsCall_IncarnationRerunLast_NotFound — несуществующая инкарнация → 404.
func TestToolsCall_IncarnationRerunLast_NotFound(t *testing.T) {
	pool := &fakePool{} // incFn nil → SelectByName отдаёт pgx.ErrNoRows
	starter := &mcpStarter{}
	h, _ := newTestHandlerFull(t, pool, rerunRBAC(), starter, &mcpResolver{ok: true}, nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.rerun-last",
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

// TestToolsCall_IncarnationRerunLast_EmptyReason — пустой reason → validation-failed.
func TestToolsCall_IncarnationRerunLast_EmptyReason(t *testing.T) {
	pool := &fakePool{incFn: incLocked(nil, "create")}
	starter := &mcpStarter{}
	h, _ := newTestHandlerFull(t, pool, rerunRBAC(), starter, &mcpResolver{ok: true}, nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.rerun-last",
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

// TestToolsCall_IncarnationRerunLast_RunnerNotConfigured — runner/registry nil →
// internal-error (rerun перезапускает scenario).
func TestToolsCall_IncarnationRerunLast_RunnerNotConfigured(t *testing.T) {
	h, _ := newTestHandlerFull(t, &fakePool{}, rerunRBAC(), nil, nil, nil)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.rerun-last",
		`{"name":"redis-prod","reason":"x"}`)
	if resp.Error == nil {
		t.Fatal("expected internal-error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeInternalError {
		t.Errorf("data.code = %q, want internal-error", data.Code)
	}
}
