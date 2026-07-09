package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// mcpStarterAssert — ScenarioStarter, ДОПОЛНИТЕЛЬНО реализующий assertPreflighter
// (PreflightAssert). Под duck-typing-гейт create-tool-а: assertErr задаёт исход
// pre-flight-а (scenario.ErrAssertFailed → 422 assert_failed; nil → проходит).
// preflightCalls фиксирует факт вызова гейта, calls — реального scenario-start.
type mcpStarterAssert struct {
	assertErr      error
	preflightCalls int
	calls          int
	gotSpec        scenario.RunSpec
}

func (f *mcpStarterAssert) PreflightAssert(_ context.Context, _ scenario.RunSpec) error {
	f.preflightCalls++
	return f.assertErr
}

func (f *mcpStarterAssert) Start(_ context.Context, spec scenario.RunSpec) error {
	f.calls++
	f.gotSpec = spec
	return nil
}

// scenarioMCPValidateRule — scenario `create` с top-level `validate:`-правилом,
// провально для любого input (that: false). Гонит ValidateInput в ветку
// ErrValidateFailed (DSL wave 2), отдельную от ErrInputInvalid.
const scenarioMCPValidateRule = `name: create
state_changes: {}
input:
  replicas:
    type: number
    default: 1
validate:
  - that: "input.replicas > 100"
    message: "replicas must exceed 100"
tasks:
  - name: noop
    module: core.exec.run
    params:
      cmd: echo
    changed_when: "false"
`

// TestToolsCall_IncarnationCreate_TraitsProjectedToSpec — top-level `traits` на
// MCP-create доезжает до spec.traits jsonb-арга INSERT-а (источник истины
// incarnation.traits, проецируемый в souls.traits). Паритет REST
// TestIncarnation_Create_TraitsProjectedToSpec.
func TestToolsCall_IncarnationCreate_TraitsProjectedToSpec(t *testing.T) {
	pool := &fakePool{incInsertFn: func(_, _ string) error { return nil }}
	starter := &mcpStarter{}
	h, _ := newTestHandlerFull(t, pool, creatorRBAC(), starter, &mcpResolver{ok: true}, nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"name":"redis-prod","service":"redis","traits":{"team":"dba","owners":["alice","bob"]}}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if len(pool.insertIncArgs) < 11 {
		t.Fatalf("insertIncArgs len = %d, want ≥11", len(pool.insertIncArgs))
	}
	specBytes, ok := pool.insertIncArgs[4].([]byte)
	if !ok {
		t.Fatalf("insertIncArgs[4] spec = %T, want []byte", pool.insertIncArgs[4])
	}
	var spec map[string]any
	if err := json.Unmarshal(specBytes, &spec); err != nil {
		t.Fatalf("spec not JSON: %v", err)
	}
	traits, ok := spec["traits"].(map[string]any)
	if !ok {
		t.Fatalf("spec.traits = %v (%T), want object", spec["traits"], spec["traits"])
	}
	if traits["team"] != "dba" {
		t.Errorf("spec.traits.team = %v, want dba", traits["team"])
	}
	// traits-колонка ($11) тоже несёт набор (TraitsFromSpec → inc.Traits).
	traitsBytes, ok := pool.insertIncArgs[10].([]byte)
	if !ok {
		t.Fatalf("insertIncArgs[10] traits = %T, want []byte", pool.insertIncArgs[10])
	}
	var col map[string]any
	if err := json.Unmarshal(traitsBytes, &col); err != nil {
		t.Fatalf("traits col not JSON: %v", err)
	}
	if col["team"] != "dba" {
		t.Errorf("incarnation.traits col team = %v, want dba", col["team"])
	}
}

// TestToolsCall_IncarnationCreate_TraitsProjectedToSouls — проекция
// incarnation.traits → souls.traits хостов-членов ВЫЗВАНА на create (SyncTraitsToHosts
// → CountBulkMatched бьёт по souls). Моделируем 2 члена → проекция исполняется.
func TestToolsCall_IncarnationCreate_TraitsProjectedToSouls(t *testing.T) {
	pool := &fakePool{
		incInsertFn:     func(_, _ string) error { return nil },
		soulBulkCountFn: func() int { return 2 },
	}
	starter := &mcpStarter{}
	h, _ := newTestHandlerFull(t, pool, creatorRBAC(), starter, &mcpResolver{ok: true}, nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"name":"redis-prod","service":"redis","traits":{"team":"dba"}}`)
	// Проекция best-effort: даже если souls-bulk не до конца моделирован,
	// create обязан остаться успешным (инвариант: sync-сбой не валит create).
	if resp.Error != nil {
		t.Fatalf("create must succeed despite projection: %+v", resp.Error)
	}
}

// TestToolsCall_IncarnationCreate_NoTraits_NoSpecKey — без `traits` ключа spec.traits
// нет (отличимо для CEL от «заданы пустыми»). Паритет REST.
func TestToolsCall_IncarnationCreate_NoTraits_NoSpecKey(t *testing.T) {
	pool := &fakePool{incInsertFn: func(_, _ string) error { return nil }}
	h, _ := newTestHandlerFull(t, pool, creatorRBAC(), &mcpStarter{}, &mcpResolver{ok: true}, nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"name":"redis-prod","service":"redis"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	specBytes, _ := pool.insertIncArgs[4].([]byte)
	var spec map[string]any
	_ = json.Unmarshal(specBytes, &spec)
	if _, has := spec["traits"]; has {
		t.Errorf("spec.traits present без traits в запросе: %v", spec)
	}
}

// TestToolsCall_IncarnationCreate_InvalidTraitValue_422 — nested trait-значение
// отбивается доменом (TraitsFromSpec → ValidateTraitDelta) ДО insert. Паритет REST.
func TestToolsCall_IncarnationCreate_InvalidTraitValue_422(t *testing.T) {
	pool := &fakePool{incInsertFn: func(_, _ string) error {
		t.Error("Create must NOT insert on invalid trait value")
		return nil
	}}
	starter := &mcpStarter{}
	h, rec := newTestHandlerFull(t, pool, creatorRBAC(), starter, &mcpResolver{ok: true}, nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"name":"redis-prod","service":"redis","traits":{"bad":{"nested":1}}}`)
	if resp.Error == nil {
		t.Fatal("expected validation error for nested trait value")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Errorf("data.code = %q, want validation-failed", data.Code)
	}
	if starter.calls != 0 {
		t.Error("invalid trait must not start scenario")
	}
	if len(rec.events) != 0 {
		t.Error("invalid trait must not write audit")
	}
}

// TestToolsCall_IncarnationCreate_ValidateRuleFails_422 — top-level `validate:`-
// правило сценария create не сходится на смерженном input → validation-failed
// (scenario.ErrValidateFailed), ОТДЕЛЬНО от input_invalid. scenario НЕ
// запускается, audit не пишется. Паритет REST validation_failed-ветки CreateTyped.
func TestToolsCall_IncarnationCreate_ValidateRuleFails_422(t *testing.T) {
	pool := &fakePool{incInsertFn: func(_, _ string) error {
		t.Error("Create must NOT insert when validate rule fails")
		return nil
	}}
	starter := &mcpStarter{}
	loader := &mcpLoader{localDir: mcpCreateSnapshot(t, scenarioMCPValidateRule)}
	h, rec := newTestHandlerFull(t, pool, creatorRBAC(), starter, &mcpResolver{ok: true}, loader)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"name":"redis-prod","service":"redis","create_scenario":"create","input":{"replicas":3}}`)
	if resp.Error == nil {
		t.Fatal("expected validation_failed for failing validate rule")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Errorf("data.code = %q, want validation-failed", data.Code)
	}
	if starter.calls != 0 {
		t.Error("validate-fail must not start scenario")
	}
	if len(rec.events) != 0 {
		t.Error("validate-fail must not write audit")
	}
}

// TestToolsCall_IncarnationCreate_AssertFails_422 — pre-flight assert-гейт
// (форма A): assert-предикат сценария create не сошёлся → validation-failed
// СИНХРОННО, БЕЗ insert / scenario-start / audit. Паритет REST assert_failed-
// ветки CreateTyped (отказ на этапе модели, не postfactum error_locked).
func TestToolsCall_IncarnationCreate_AssertFails_422(t *testing.T) {
	pool := &fakePool{incInsertFn: func(_, _ string) error {
		t.Error("Create must NOT insert when pre-flight assert fails")
		return nil
	}}
	starter := &mcpStarterAssert{assertErr: scenario.ErrAssertFailed}
	h, rec := newTestHandlerFull(t, pool, creatorRBAC(), starter, &mcpResolver{ok: true}, nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"name":"redis-prod","service":"redis"}`)
	if resp.Error == nil {
		t.Fatal("expected assert_failed (422)")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Errorf("data.code = %q, want validation-failed", data.Code)
	}
	if starter.preflightCalls != 1 {
		t.Errorf("preflight calls = %d, want 1", starter.preflightCalls)
	}
	if starter.calls != 0 {
		t.Error("assert-fail must not start scenario")
	}
	if len(pool.insertIncArgs) != 0 {
		t.Error("assert-fail must not insert incarnation")
	}
	if len(rec.events) != 0 {
		t.Error("assert-fail must not write audit")
	}
}

// TestToolsCall_IncarnationCreate_AssertPasses_Inserts — assert сошёлся → create
// проходит: pre-flight вызван, scenario стартует ровно один раз. Гарантирует, что
// гейт не ложно-режет happy path.
func TestToolsCall_IncarnationCreate_AssertPasses_Inserts(t *testing.T) {
	pool := &fakePool{incInsertFn: func(_, _ string) error { return nil }}
	starter := &mcpStarterAssert{assertErr: nil}
	h, _ := newTestHandlerFull(t, pool, creatorRBAC(), starter, &mcpResolver{ok: true}, nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"name":"redis-prod","service":"redis"}`)
	if resp.Error != nil {
		t.Fatalf("assert pass should create: %+v", resp.Error)
	}
	if starter.preflightCalls != 1 {
		t.Errorf("preflight calls = %d, want 1", starter.preflightCalls)
	}
	if starter.calls != 1 {
		t.Errorf("scenario start calls = %d, want 1", starter.calls)
	}
}

// --- create_scenario (механизм нескольких create-сценариев, Вариант A) ---

// TestToolsCall_IncarnationCreate_CreateScenarioInvalidName — синтаксически битое
// имя create_scenario (traversal/мусор) отбивается до резолва набора как
// validation-failed (ErrCreateScenarioNotEligible), insert НЕ выполняется. Гейт
// валиден только при наличии loader-а. strictUnmarshal сперва не режет (поле
// объявлено), отбой даёт ValidateCreateScenarioChoice.
func TestToolsCall_IncarnationCreate_CreateScenarioInvalidName(t *testing.T) {
	pool := &fakePool{incInsertFn: func(_, _ string) error {
		t.Error("Create must NOT insert on invalid create_scenario")
		return nil
	}}
	starter := &mcpStarter{}
	loader := &mcpLoader{}
	h, rec := newTestHandlerFull(t, pool, creatorRBAC(), starter, &mcpResolver{ok: true}, loader)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"name":"redis-prod","service":"redis","create_scenario":"..bad"}`)
	if resp.Error == nil {
		t.Fatal("expected validation-failed for invalid create_scenario")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Errorf("data.code = %q, want validation-failed", data.Code)
	}
	if starter.calls != 0 {
		t.Error("invalid create_scenario must not start scenario")
	}
	if len(rec.events) != 0 {
		t.Error("invalid create_scenario must not write audit")
	}
}

// TestToolsCall_IncarnationCreate_CreateScenarioNotEligible — валидное по форме,
// но НЕ входящее в create-набор сервиса имя (нет `create: true`, в снапшоте
// отсутствует) → validation-failed, insert НЕ выполняется. Защита от bootstrap-а
// инкарнации произвольным (например операционным) сценарием.
func TestToolsCall_IncarnationCreate_CreateScenarioNotEligible(t *testing.T) {
	pool := &fakePool{incInsertFn: func(_, _ string) error {
		t.Error("Create must NOT insert on non-eligible create_scenario")
		return nil
	}}
	starter := &mcpStarter{}
	loader := &mcpLoader{}
	h, rec := newTestHandlerFull(t, pool, creatorRBAC(), starter, &mcpResolver{ok: true}, loader)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"name":"redis-prod","service":"redis","create_scenario":"add_user"}`)
	if resp.Error == nil {
		t.Fatal("expected validation-failed for non-eligible create_scenario")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Errorf("data.code = %q, want validation-failed", data.Code)
	}
	if starter.calls != 0 {
		t.Error("non-eligible create_scenario must not start scenario")
	}
	if len(rec.events) != 0 {
		t.Error("non-eligible create_scenario must not write audit")
	}
}

// TestToolsCall_IncarnationCreate_ExplicitCreate — явный create_scenario=create
// (помечен create:true на диске) → прогон стартует ИМЕННО `create`, имя
// сохраняется в incarnation.created_scenario ($12). Контраст к bare/required.
func TestToolsCall_IncarnationCreate_ExplicitCreate(t *testing.T) {
	pool := &fakePool{incInsertFn: func(_, _ string) error { return nil }}
	starter := &mcpStarter{}
	loader := &mcpLoader{localDir: mcpCreateSnapshot(t, "name: create\nstate_changes: {}\ntasks: []\n")}
	h, _ := newTestHandlerFull(t, pool, creatorRBAC(), starter, &mcpResolver{ok: true}, loader)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"name":"redis-prod","service":"redis","create_scenario":"create"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if starter.gotSpec.ScenarioName != "create" {
		t.Errorf("RunSpec.ScenarioName = %q, want create", starter.gotSpec.ScenarioName)
	}
	// created_scenario-колонка ($12) = create.
	if len(pool.insertIncArgs) < 12 {
		t.Fatalf("insertIncArgs len = %d, want ≥12", len(pool.insertIncArgs))
	}
	if cs, _ := pool.insertIncArgs[11].(string); cs != "create" {
		t.Errorf("created_scenario col = %q, want create", cs)
	}
}

// TestToolsCall_IncarnationCreate_EmptyChoice_HasScenarios_Required — опущенный
// create_scenario при НЕПУСТОМ create-наборе → validation-failed
// (create_scenario_required, Фаза 2): incarnation НЕ создаётся, прогон НЕ
// стартует. Регресс = вернулся back-compat-дефолт `create`.
func TestToolsCall_IncarnationCreate_EmptyChoice_HasScenarios_Required(t *testing.T) {
	pool := &fakePool{incInsertFn: func(_, _ string) error {
		t.Error("Create must NOT insert when create_scenario required")
		return nil
	}}
	starter := &mcpStarter{}
	loader := &mcpLoader{localDir: mcpCreateSnapshot(t, "name: create\nstate_changes: {}\ntasks: []\n")}
	h, rec := newTestHandlerFull(t, pool, creatorRBAC(), starter, &mcpResolver{ok: true}, loader)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"name":"redis-prod","service":"redis"}`)
	if resp.Error == nil {
		t.Fatal("expected create_scenario_required (набор непуст, выбор пуст)")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Errorf("data.code = %q, want validation-failed", data.Code)
	}
	if starter.calls != 0 {
		t.Error("required-422 must not start scenario")
	}
	if len(rec.events) != 0 {
		t.Error("required-422 must not write audit")
	}
}

// TestToolsCall_IncarnationCreate_BareNoScenario — сервис БЕЗ create-сценариев
// (пустой снапшот) + опущенный create_scenario → bare-инкарнация: создаётся
// (insert), прогон НЕ стартует, apply_id отсутствует, created_scenario col = NULL.
func TestToolsCall_IncarnationCreate_BareNoScenario(t *testing.T) {
	inserted := 0
	pool := &fakePool{incInsertFn: func(_, _ string) error { inserted++; return nil }}
	starter := &mcpStarter{}
	loader := &mcpLoader{localDir: mcpEmptyCreateSnapshot(t)}
	h, _ := newTestHandlerFull(t, pool, creatorRBAC(), starter, &mcpResolver{ok: true}, loader)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"name":"redis-bare","service":"redis"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	// Инкарнация создана БЕЗ прогона.
	if inserted != 1 {
		t.Errorf("insert calls = %d, want 1 (bare создаётся)", inserted)
	}
	if starter.calls != 0 {
		t.Errorf("scenario start calls = %d, want 0 (bare без прогона)", starter.calls)
	}
	// created_scenario col ($12) = NULL (nil).
	if len(pool.insertIncArgs) < 12 {
		t.Fatalf("insertIncArgs len = %d, want ≥12", len(pool.insertIncArgs))
	}
	if pool.insertIncArgs[11] != nil {
		t.Errorf("created_scenario col = %v, want nil (NULL для bare)", pool.insertIncArgs[11])
	}
	// apply_id отсутствует в выводе (omitempty).
	var res toolsCallResult
	_ = json.Unmarshal(resp.Result, &res)
	var out incarnationCreateOutput
	_ = json.Unmarshal(res.StructuredContent, &out)
	if out.ApplyID != nil {
		t.Errorf("_apply_id = %v, want nil (bare без прогона)", *out.ApplyID)
	}
}

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
	loader := &mcpLoader{localDir: mcpCreateSnapshot(t, scenarioMCPRequiredInput)}
	h, rec := newTestHandlerFull(t, pool, creatorRBAC(), starter, &mcpResolver{ok: true}, loader)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"name":"ba","service":"redis","create_scenario":"create","input":{}}`)
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
	loader := &mcpLoader{localDir: mcpCreateSnapshot(t, scenarioMCPRequiredInput)}
	h, _ := newTestHandlerFull(t, pool, creatorRBAC(), starter, &mcpResolver{ok: true}, loader)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"name":"ba","service":"redis","create_scenario":"create","input":{"name":"alice"}}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if starter.calls != 1 {
		t.Fatalf("scenario start calls = %d, want 1", starter.calls)
	}
}
