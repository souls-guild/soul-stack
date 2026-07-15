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

// mcpStarterAssert is a ScenarioStarter that also implements assertPreflighter
// (PreflightAssert), for the create-tool's duck-typing gate: assertErr sets
// the pre-flight outcome (scenario.ErrAssertFailed → 422 assert_failed; nil →
// pass). preflightCalls counts gate invocations, calls counts actual
// scenario starts.
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

// scenarioMCPValidateRule is a `create` scenario with a top-level `validate:`
// rule that fails for any input (that: false). Drives ValidateInput into the
// ErrValidateFailed branch (DSL wave 2), distinct from ErrInputInvalid.
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

// TestToolsCall_IncarnationCreate_TraitsProjectedToSpec — top-level `traits`
// on MCP-create reaches the spec.traits jsonb INSERT arg (source of truth
// incarnation.traits, projected to souls.traits). REST parity with
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
	// traits column ($11) also carries the set (TraitsFromSpec → inc.Traits).
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

// TestToolsCall_IncarnationCreate_TraitsProjectedToSouls — the
// incarnation.traits → souls.traits projection to member hosts is invoked on
// create (SyncTraitsToHosts → CountBulkMatched hits souls). Mocks 2 members
// so the projection runs.
func TestToolsCall_IncarnationCreate_TraitsProjectedToSouls(t *testing.T) {
	pool := &fakePool{
		incInsertFn:     func(_, _ string) error { return nil },
		soulBulkCountFn: func() int { return 2 },
	}
	starter := &mcpStarter{}
	h, _ := newTestHandlerFull(t, pool, creatorRBAC(), starter, &mcpResolver{ok: true}, nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"name":"redis-prod","service":"redis","traits":{"team":"dba"}}`)
	// Projection is best-effort: even with an incompletely mocked souls-bulk,
	// create must still succeed (invariant: a sync failure must not fail create).
	if resp.Error != nil {
		t.Fatalf("create must succeed despite projection: %+v", resp.Error)
	}
}

// TestToolsCall_IncarnationCreate_NoTraits_NoSpecKey — without a `traits`
// key, spec.traits is absent (distinguishable in CEL from "set empty"). REST
// parity.
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

// TestToolsCall_IncarnationCreate_InvalidTraitValue_422 — a nested trait
// value is rejected by the domain (TraitsFromSpec → ValidateTraitDelta)
// before insert. REST parity.
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

// TestToolsCall_IncarnationCreate_ValidateRuleFails_422 — the create
// scenario's top-level `validate:` rule failing on the merged input produces
// validation-failed (scenario.ErrValidateFailed), distinct from
// input_invalid. scenario does not start, audit is not written. REST parity
// with CreateTyped's validation_failed branch.
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

// TestToolsCall_IncarnationCreate_AssertFails_422 — pre-flight assert gate
// (form A): a failing create-scenario assert predicate produces
// validation-failed SYNCHRONOUSLY, with no insert / scenario-start / audit.
// REST parity with CreateTyped's assert_failed branch (rejected at model
// stage, not postfactum error_locked).
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

// TestToolsCall_IncarnationCreate_AssertPasses_Inserts — a passing assert
// lets create through: pre-flight is called, scenario starts exactly once.
// Guards against the gate false-failing the happy path.
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

// --- create_scenario (multiple create-scenarios mechanism, Variant A) ---

// TestToolsCall_IncarnationCreate_CreateScenarioInvalidName — a
// syntactically malformed create_scenario name (traversal/garbage) is
// rejected before set resolution as validation-failed
// (ErrCreateScenarioNotEligible); insert does not run. The gate only applies
// when a loader is present. strictUnmarshal doesn't reject it first (the
// field is declared) — ValidateCreateScenarioChoice does.
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

// TestToolsCall_IncarnationCreate_CreateScenarioNotEligible — a well-formed
// name that is NOT in the service's create set (missing `create: true`,
// absent from the snapshot) produces validation-failed; insert does not run.
// Guards against bootstrapping an incarnation with an arbitrary operational
// scenario.
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

// TestToolsCall_IncarnationCreate_ExplicitCreate — an explicit
// create_scenario=create (marked create:true on disk) starts EXACTLY
// `create`, with the name saved to incarnation.created_scenario ($12).
// Contrast to bare/required.
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
	// created_scenario column ($12) = create.
	if len(pool.insertIncArgs) < 12 {
		t.Fatalf("insertIncArgs len = %d, want ≥12", len(pool.insertIncArgs))
	}
	if cs, _ := pool.insertIncArgs[11].(string); cs != "create" {
		t.Errorf("created_scenario col = %q, want create", cs)
	}
}

// TestToolsCall_IncarnationCreate_EmptyChoice_HasScenarios_Required — an
// omitted create_scenario with a NON-EMPTY create set produces
// validation-failed (create_scenario_required, Phase 2): the incarnation is
// NOT created, no run starts. A regression would mean the back-compat
// default `create` came back.
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

// TestToolsCall_IncarnationCreate_BareNoScenario — a service with NO create
// scenarios (empty snapshot) plus an omitted create_scenario yields a bare
// incarnation: it is created (insert), no run starts, apply_id is absent,
// created_scenario col = NULL.
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
	// Incarnation created WITHOUT a run.
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
	// apply_id absent from output (omitempty).
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
	// auto_create defaults to true (manifest without a lifecycle block) → apply_id is set.
	if out.ApplyID == nil {
		t.Fatalf("_apply_id is nil (auto_create=true expected)")
	}
	if !audit.IsValidULID(*out.ApplyID) {
		t.Errorf("_apply_id not a ULID: %q", *out.ApplyID)
	}
	// scenario `create` started with the same apply_id and input.
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
	// runner/registry nil → internal-error (REST parity, 500 with no runner).
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

// TestToolsCall_IncarnationCreate_ScopeDeniesForeignCoven — an operator
// scoped to `incarnation.create on coven=dev` CANNOT create an incarnation
// with covens=[prod] (least-privilege, REST parity with
// IncarnationCreateScopeSelector). Deny happens before insert: fail-closed,
// no insert, no scenario-start, no audit.
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

// TestToolsCall_IncarnationCreate_ScopeAllowsMatchingCoven — the same
// operator (scope coven=dev) CAN create an incarnation with covens=[dev].
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
// creates an incarnation with any covens (no-regression).
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

// scenarioMCPRequiredInput is a `create` scenario with a required field `name`.
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

// TestToolsCall_IncarnationCreate_RequiredInputMissing — MCP parity with
// REST: create without a required field is rejected synchronously
// (validation-failed), scenario does not start, audit is not written.
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

// TestToolsCall_IncarnationCreate_RequiredInputProvided — a provided
// required field passes and the scenario starts.
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
