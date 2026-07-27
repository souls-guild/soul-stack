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
