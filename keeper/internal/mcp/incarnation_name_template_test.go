package mcp

// MCP-side guard tests for server-side incarnation-name composition (ADR-0079,
// NIM-177). The point of duplicating the REST cases here is PARITY: both surfaces
// go through the same scenario.ResolveCreatePlan, and the tests pin that they also
// agree on what reaches the DB, the run and the audit — a divergence here means one
// surface started reading the request name again.

import (
	"encoding/json"
	"strings"
	"testing"
)

// nameTemplateYAML is the same create scenario the REST guard tests use, so the
// two suites compose the same name from the same input.
const nameTemplateYAML = `name: create
create: true
name_template: "${input.name}-${input.project}-${input.subproject}-redis-${input.service_type}"
input:
  name:
    type: string
    required: true
  project:
    type: string
    required: true
  subproject:
    type: string
    required: true
  service_type:
    type: string
    default: sentinel
tasks: []
`

// composedNameWant is the name both surfaces must produce for the shared input.
const composedNameWant = "cache-billing-inv-redis-sentinel"

// TestToolsCall_IncarnationCreate_NameComposed — a create call WITHOUT `name`
// succeeds and the composed name reaches the INSERT, the run spec and the tool
// output. Parity with handlers.TestIncarnation_Create_NameComposed.
func TestToolsCall_IncarnationCreate_NameComposed(t *testing.T) {
	pool := &fakePool{incInsertFn: func(_, _ string) error { return nil }}
	starter := &mcpStarterAssert{}
	loader := &mcpLoader{localDir: mcpCreateSnapshot(t, nameTemplateYAML)}
	h, _ := newTestHandlerFull(t, pool, creatorRBAC(), starter, &mcpResolver{ok: true}, loader)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"service":"redis","create_scenario":"create","input":{"name":"cache","project":"billing","subproject":"inv"}}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if got, _ := pool.insertIncArgs[0].(string); got != composedNameWant {
		t.Errorf("INSERT name ($1) = %q, want %q", got, composedNameWant)
	}
	if starter.gotSpec.IncarnationName != composedNameWant {
		t.Errorf("RunSpec.IncarnationName = %q, want %q", starter.gotSpec.IncarnationName, composedNameWant)
	}
	if got := toolIncarnation(t, resp); got != composedNameWant {
		t.Errorf("tool output incarnation = %q, want %q", got, composedNameWant)
	}
}

// TestToolsCall_IncarnationCreate_ComposedNameTooLong — the 63-character ceiling on
// the MCP surface: validation-failed with the same detail prefix REST uses, and
// nothing is created.
func TestToolsCall_IncarnationCreate_ComposedNameTooLong(t *testing.T) {
	pool := &fakePool{incInsertFn: func(_, _ string) error { return nil }}
	starter := &mcpStarterAssert{}
	loader := &mcpLoader{localDir: mcpCreateSnapshot(t, nameTemplateYAML)}
	h, _ := newTestHandlerFull(t, pool, creatorRBAC(), starter, &mcpResolver{ok: true}, loader)

	long := strings.Repeat("x", 30)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"service":"redis","create_scenario":"create","input":{"name":"`+long+`","project":"`+long+`","subproject":"`+long+`"}}`)
	if resp.Error == nil {
		t.Fatalf("expected an error for an over-long composed name, got %+v", resp.Result)
	}
	if !strings.Contains(resp.Error.Message, "composed_name_invalid") {
		t.Errorf("message = %q, want the composed_name_invalid prefix", resp.Error.Message)
	}
	if !strings.Contains(resp.Error.Message, "63") {
		t.Errorf("message = %q, must state the ceiling", resp.Error.Message)
	}
	if len(pool.insertIncArgs) != 0 {
		t.Errorf("insert happened despite an invalid composed name: %v", pool.insertIncArgs)
	}
	if starter.calls != 0 {
		t.Errorf("starter.calls = %d, want 0", starter.calls)
	}
}

// TestToolsCall_IncarnationCreate_ExplicitNameWithTemplate — sending `name` against
// a composing scenario is refused on MCP too (same escalation reasoning as REST).
func TestToolsCall_IncarnationCreate_ExplicitNameWithTemplate(t *testing.T) {
	pool := &fakePool{incInsertFn: func(_, _ string) error { return nil }}
	loader := &mcpLoader{localDir: mcpCreateSnapshot(t, nameTemplateYAML)}
	h, _ := newTestHandlerFull(t, pool, creatorRBAC(), &mcpStarterAssert{}, &mcpResolver{ok: true}, loader)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"name":"my-own","service":"redis","create_scenario":"create","input":{"name":"cache","project":"billing","subproject":"inv"}}`)
	if resp.Error == nil {
		t.Fatalf("expected an error when both name and name_template are present")
	}
	if !strings.Contains(resp.Error.Message, "name_not_composable") {
		t.Errorf("message = %q, want the name_not_composable prefix", resp.Error.Message)
	}
	if len(pool.insertIncArgs) != 0 {
		t.Errorf("insert happened despite a rejected request: %v", pool.insertIncArgs)
	}
}

// TestToolsCall_IncarnationCreate_NoTemplate_NameStillRequired — back-compat: with
// no template in play, an omitted name is still a validation error, exactly as
// before ADR-0079.
func TestToolsCall_IncarnationCreate_NoTemplate_NameStillRequired(t *testing.T) {
	pool := &fakePool{incInsertFn: func(_, _ string) error { return nil }}
	loader := &mcpLoader{localDir: mcpCreateSnapshot(t, "name: create\nstate_changes: {}\ntasks: []\n")}
	h, _ := newTestHandlerFull(t, pool, creatorRBAC(), &mcpStarterAssert{}, &mcpResolver{ok: true}, loader)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"service":"redis","create_scenario":"create"}`)
	if resp.Error == nil {
		t.Fatalf("expected an error for a missing name without a template")
	}
	if !strings.Contains(resp.Error.Message, "field 'name' is required") {
		t.Errorf("message = %q, want \"field 'name' is required\"", resp.Error.Message)
	}
	if len(pool.insertIncArgs) != 0 {
		t.Errorf("insert happened for a nameless request: %v", pool.insertIncArgs)
	}
}

// toolIncarnation pulls the `incarnation` echo out of a create tool result.
func toolIncarnation(t *testing.T, resp jsonRPCResponse) string {
	t.Helper()
	raw, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var envelope struct {
		StructuredContent struct {
			Incarnation string `json:"incarnation"`
		} `json:"structuredContent"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	return envelope.StructuredContent.Incarnation
}

// --- NIM-333: a scoped operator creates a templated incarnation ---
//
// These are the end-to-end guards for the change: unlike the REST handler unit
// tests, the MCP tool runs BOTH gates against a real enforcer built from a real
// rbactest snapshot — gate (a) over the request body, gate (b) over the composed
// name. A REST handler test calls CreateTyped directly and therefore exercises
// gate (b) only.

// The demo scenario, and the one that was impossible: an operator whose only
// permission is `incarnation.create on coven=billing` creates an incarnation whose
// name is composed server-side. Before NIM-333 gate (a) saw no `name`, produced the
// empty context, and only an unrestricted role passed.
func TestToolsCall_IncarnationCreate_Templated_ScopedOperatorAllowed(t *testing.T) {
	pool := &fakePool{incInsertFn: func(_, _ string) error { return nil }}
	loader := &mcpLoader{localDir: mcpCreateSnapshot(t, nameTemplateYAML)}
	h, _ := newTestHandlerFull(t, pool, scopedRBAC("incarnation.create on coven=billing"),
		&mcpStarterAssert{}, &mcpResolver{ok: true}, loader)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"service":"redis","covens":["billing"],"create_scenario":"create","input":{"name":"cache","project":"billing","subproject":"inv"}}`)
	if resp.Error != nil {
		t.Fatalf("a coven-scoped operator must create inside its own scope: %+v", resp.Error)
	}
	if got := toolIncarnation(t, resp); got != composedNameWant {
		t.Errorf("tool output incarnation = %q, want %q", got, composedNameWant)
	}
}

// The other direction: a coven outside the ceiling is refused, and nothing is created.
func TestToolsCall_IncarnationCreate_Templated_CovenOutsideCeilingRefused(t *testing.T) {
	pool := &fakePool{incInsertFn: func(_, _ string) error { return nil }}
	loader := &mcpLoader{localDir: mcpCreateSnapshot(t, nameTemplateYAML)}
	h, _ := newTestHandlerFull(t, pool, scopedRBAC("incarnation.create on coven=billing"),
		&mcpStarterAssert{}, &mcpResolver{ok: true}, loader)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"service":"redis","covens":["prod"],"create_scenario":"create","input":{"name":"cache","project":"billing","subproject":"inv"}}`)
	if resp.Error == nil {
		t.Fatalf("coven=prod is outside a coven=billing ceiling — must be refused")
	}
	if len(pool.insertIncArgs) != 0 {
		t.Errorf("nothing may be inserted on a refusal; insertIncArgs=%v", pool.insertIncArgs)
	}
}

// Declaring one coven the caller holds AND one it does not is refused WHOLE. This is
// the case gate (a) alone lets through — it ORs over the declared covens and matches
// on `billing` — so it is gate (b)'s AND that answers. The same shape NIM-209/232
// settled on membership binding: a bulk claim is admitted whole or refused whole,
// never quietly trimmed to the part the caller may have.
//
// The named-create equivalent is still open (NIM-338): there gate (b) does not run.
func TestToolsCall_IncarnationCreate_Templated_MixedCovensRefusedWhole(t *testing.T) {
	pool := &fakePool{incInsertFn: func(_, _ string) error { return nil }}
	loader := &mcpLoader{localDir: mcpCreateSnapshot(t, nameTemplateYAML)}
	h, _ := newTestHandlerFull(t, pool, scopedRBAC("incarnation.create on coven=billing"),
		&mcpStarterAssert{}, &mcpResolver{ok: true}, loader)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"service":"redis","covens":["billing","prod"],"create_scenario":"create","input":{"name":"cache","project":"billing","subproject":"inv"}}`)
	if resp.Error == nil {
		t.Fatalf("mixed covens must NOT create: `prod` is outside the ceiling and the request is refused whole")
	}
	if len(pool.insertIncArgs) != 0 {
		t.Errorf("nothing may be inserted; insertIncArgs=%v", pool.insertIncArgs)
	}
}

// A service-scoped role works on the same terms — `service` is required by the tool
// schema, so gate (a) always has it.
func TestToolsCall_IncarnationCreate_Templated_ServiceScopedAllowed(t *testing.T) {
	pool := &fakePool{incInsertFn: func(_, _ string) error { return nil }}
	loader := &mcpLoader{localDir: mcpCreateSnapshot(t, nameTemplateYAML)}
	h, _ := newTestHandlerFull(t, pool, scopedRBAC("incarnation.create on service=redis"),
		&mcpStarterAssert{}, &mcpResolver{ok: true}, loader)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"service":"redis","create_scenario":"create","input":{"name":"cache","project":"billing","subproject":"inv"}}`)
	if resp.Error != nil {
		t.Fatalf("a service-scoped operator must be able to create: %+v", resp.Error)
	}
}

// A role scoped ONLY by `incarnation=` still cannot create a templated incarnation:
// that dimension is absent from gate (a)'s context by construction, so gate (a)
// denies before composition ever happens. Fail-closed and deliberate — pinned here
// because it is the one shape of scoped role this change does NOT enable, and it
// belongs in the upgrade notes rather than being discovered in a demo.
func TestToolsCall_IncarnationCreate_Templated_IncarnationScopedRoleStillDenied(t *testing.T) {
	pool := &fakePool{incInsertFn: func(_, _ string) error { return nil }}
	loader := &mcpLoader{localDir: mcpCreateSnapshot(t, nameTemplateYAML)}
	h, _ := newTestHandlerFull(t, pool, scopedRBAC("incarnation.create on incarnation="+composedNameWant),
		&mcpStarterAssert{}, &mcpResolver{ok: true}, loader)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"service":"redis","create_scenario":"create","input":{"name":"cache","project":"billing","subproject":"inv"}}`)
	if resp.Error == nil {
		t.Fatalf("a role scoped only by incarnation= cannot be satisfied before the name exists — must be refused")
	}
	if len(pool.insertIncArgs) != 0 {
		t.Errorf("nothing may be inserted; insertIncArgs=%v", pool.insertIncArgs)
	}
}

// An unrestricted role is unaffected — the path it always had still works.
func TestToolsCall_IncarnationCreate_Templated_WildcardUnaffected(t *testing.T) {
	pool := &fakePool{incInsertFn: func(_, _ string) error { return nil }}
	loader := &mcpLoader{localDir: mcpCreateSnapshot(t, nameTemplateYAML)}
	h, _ := newTestHandlerFull(t, pool, wildcardRBAC(),
		&mcpStarterAssert{}, &mcpResolver{ok: true}, loader)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.create",
		`{"service":"redis","covens":["prod"],"create_scenario":"create","input":{"name":"cache","project":"billing","subproject":"inv"}}`)
	if resp.Error != nil {
		t.Fatalf("`*` must be unaffected: %+v", resp.Error)
	}
}
