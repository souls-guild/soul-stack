package mcp

// Guard tests for the keeper.incarnation.rerun-last MCP tool (REST parity
// with IncarnationHandler.RerunLastTyped, incarnation_rerun_test.go): lifting
// error_locked + restarting the last failed scenario in one action. Covers
// the create/day-2 happy path (202 + apply_id + scenario + audit), RBAC
// scope-deny on a foreign coven, the status gate, fail-closed recipe-null,
// and argument validation.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// rerunRBAC — role with permission incarnation.rerun-last (1:1 with the
// tool), bound to archon-alice.
func rerunRBAC() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "rerunner", Operators: []string{"archon-alice"}, Permissions: []string{"incarnation.rerun-last"}},
		},
	}
}

// incLocked — incFn returning an error_locked incarnation with given covens
// and creating scenario. createdScenario "" → NULL (bare incarnation). service=redis.
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

// TestToolsCall_IncarnationRerunLast_Success — happy-path create: lifts the
// error_locked block and starts exactly one `create` scenario
// (created_scenario) with a shared apply_id; response {_apply_id,
// incarnation, scenario}; audit incarnation.rerun_last (source=mcp,
// correlation_id=apply_id) with reason + previous_status + scenario.
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

// incLockedSpec — incFn for an error_locked incarnation with a given spec
// (checks that spec.input propagates into RunSpec.Input on the create path).
// created_scenario="create", service=redis.
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

// TestToolsCall_IncarnationRerunLast_ReusesStoredInput — create-path GUARD
// (REST parity with TestRerunLast_ReusesStoredInput_202): rerun-last on an
// incarnation with operator input stored in spec.input → input propagates
// into RunSpec.Input for the restarted bootstrap run (NOT nil, NOT defaults).
func TestToolsCall_IncarnationRerunLast_ReusesStoredInput(t *testing.T) {
	spec := map[string]any{"input": map[string]any{
		"version":         "8.6.1",
		"shards":          float64(3), // jsonb number
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
		t.Fatal("RunSpec.Input = nil - stored spec.input NOT propagated (create-path regression)")
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

// TestToolsCall_IncarnationRerunLast_NoStoredInput_NilInput — create-path
// contrast: an incarnation without spec.input → RunSpec.Input nil, run
// starts normally.
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
		t.Errorf("RunSpec.Input = %v, want nil (spec without input)", starter.gotSpec.Input)
	}
}

// TestToolsCall_IncarnationRerunLast_Day2ReusesRecipeInput — day-2 happy path
// (REST parity with TestRerunLast_Day2_ReusesRecipeInput_202): the last
// failure was add_user (≠ created `create`), its input comes from the
// recipe apply_run → 202, RunSpec.ScenarioName=="add_user", Input from the
// recipe (not spec.input), scenario in reply/audit == add_user.
func TestToolsCall_IncarnationRerunLast_Day2ReusesRecipeInput(t *testing.T) {
	spec := map[string]any{"input": map[string]any{"version": "8.6.1"}} // must NOT leak through
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
		t.Error("RunSpec.Input carries spec.input[version] - operational reruns must take recipe.input")
	}
	if rec.events[0].Payload["scenario"] != "add_user" {
		t.Errorf("audit scenario = %v, want add_user", rec.events[0].Payload["scenario"])
	}
	// day-2 recipe without from_upgrade → RunSpec.FromUpgrade=false.
	if starter.gotSpec.FromUpgrade {
		t.Error("RunSpec.FromUpgrade = true, want false (recipe without from_upgrade)")
	}
}

// TestToolsCall_IncarnationRerunLast_Day2FromUpgrade — MAJOR guard (ADR-0068,
// REST parity): MCP rerun-last on a run with recipe.from_upgrade=true
// propagates FromUpgrade=true into RunSpec → restart from upgrade/<slug>/,
// not scenario/.
func TestToolsCall_IncarnationRerunLast_Day2FromUpgrade(t *testing.T) {
	pool := &fakePool{
		incFn:          incLockedSpec(map[string]any{}),
		lastScenarioFn: func(string) (string, error) { return "to_v2", nil },
		recipeFn: func(string) ([]byte, error) {
			return []byte(`{"scenario_name":"to_v2","from_upgrade":true,"input":{}}`), nil
		},
	}
	starter := &mcpStarter{}
	h, _ := newTestHandlerFull(t, pool, rerunRBAC(), starter, &mcpResolver{ok: true}, nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.rerun-last",
		`{"name":"redis-prod","reason":"rerun upgrade verified ok"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if starter.calls != 1 {
		t.Fatalf("scenario start calls = %d, want 1", starter.calls)
	}
	if starter.gotSpec.ScenarioName != "to_v2" {
		t.Errorf("ScenarioName = %q, want to_v2", starter.gotSpec.ScenarioName)
	}
	if !starter.gotSpec.FromUpgrade {
		t.Error("RunSpec.FromUpgrade = false, want true (recipe.from_upgrade -> rerun from upgrade/)")
	}
}

// TestToolsCall_IncarnationRerunLast_Day2BareIncarnation — a bare incarnation
// (created_scenario IS NULL) locked at day-2 → rerun-last works via the
// recipe path (previously: 409). ScenarioName from the last run, Input from
// the recipe.
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

// TestToolsCall_IncarnationRerunLast_Day2RecipeUnavailable — day-2, but no
// recipe (recipe IS NULL / apply_run purged → ErrNoRows): fail-closed
// rerun-input-unavailable (a distinct code from incarnation-locked,
// symmetric with REST TypeRerunInputUnavailable), run does NOT start, audit
// is NOT written.
func TestToolsCall_IncarnationRerunLast_Day2RecipeUnavailable(t *testing.T) {
	pool := &fakePool{
		incFn:          incLocked(nil, "create"),
		lastScenarioFn: func(string) (string, error) { return "add_user", nil },
		// recipeFn nil → recipe-probe returns ErrNoRows (fail-closed).
	}
	starter := &mcpStarter{}
	h, rec := newTestHandlerFull(t, pool, rerunRBAC(), starter, &mcpResolver{ok: true}, nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.rerun-last",
		`{"name":"redis-prod","reason":"rerun add_user"}`)
	if resp.Error == nil {
		t.Fatal("expected rerun-input-unavailable (recipe unavailable)")
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

// TestToolsCall_IncarnationRerunLast_ScopeDeniesForeignCoven — an operator
// with scope `incarnation.rerun-last on coven=dev` cannot rerun a prod
// incarnation (covens=[prod]) via MCP. Deny happens BEFORE unlock/start: no
// run, no audit.
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

// TestToolsCall_IncarnationRerunLast_ScopeAllowsMatchingCoven — the same
// operator (scope coven=prod) CAN rerun a prod incarnation: NOT forbidden,
// run starts.
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

// TestToolsCall_IncarnationRerunLast_NotErrorLocked — rerun from ready/applying
// is rejected (incarnation-locked), the run does NOT start.
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

// TestToolsCall_IncarnationRerunLast_NotFound — a nonexistent incarnation → 404.
func TestToolsCall_IncarnationRerunLast_NotFound(t *testing.T) {
	pool := &fakePool{} // incFn nil → SelectByName returns pgx.ErrNoRows
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

// TestToolsCall_IncarnationRerunLast_EmptyReason — empty reason → validation-failed.
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

// TestToolsCall_IncarnationRerunLast_RunnerNotConfigured — runner/registry
// nil → internal-error (rerun restarts the scenario).
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
