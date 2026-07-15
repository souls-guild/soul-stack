package mcp

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
)

// synodAdminCfg — RBAC config granting archon-alice all synod.* permissions
// (ADR-049). Shares the harness (newRoleHandler / callTool / mustToolErrorData /
// roleFakePool) with role_tools_test.go — same mcp package.
func synodAdminCfg() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "synod-admin", Operators: []string{"archon-alice"}, Permissions: []string{
				"synod.create", "synod.update", "synod.delete", "synod.list",
				"synod.add-operator", "synod.remove-operator",
				"synod.grant-role", "synod.revoke-role",
			}},
		},
	}
}

// --- manifest ---

func TestSynodTools_InManifest(t *testing.T) {
	want := []string{
		"keeper.synod.create", "keeper.synod.update", "keeper.synod.delete", "keeper.synod.list",
		"keeper.synod.add-operator", "keeper.synod.remove-operator",
		"keeper.synod.grant-role", "keeper.synod.revoke-role",
	}
	for _, name := range want {
		e, ok := toolByName(name)
		if !ok {
			t.Errorf("%s missing from catalogManifest", name)
			continue
		}
		if e.status != toolStatusImplemented {
			t.Errorf("%s status = %d, want Implemented", name, e.status)
		}
	}
}

// --- nil-guard ---

func TestSynodTools_NilGuard(t *testing.T) {
	h := newRoleHandler(t, synodAdminCfg(), nil) // RBACRoles == nil
	cases := []struct {
		tool string
		args string
	}{
		{"keeper.synod.create", `{"name":"team"}`},
		{"keeper.synod.update", `{"name":"team","description":"x"}`},
		{"keeper.synod.delete", `{"name":"team"}`},
		{"keeper.synod.list", `{}`},
		{"keeper.synod.add-operator", `{"synod":"team","aid":"archon-bob"}`},
		{"keeper.synod.remove-operator", `{"synod":"team","aid":"archon-bob"}`},
		{"keeper.synod.grant-role", `{"synod":"team","role":"viewer"}`},
		{"keeper.synod.revoke-role", `{"synod":"team","role":"viewer"}`},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			resp := callTool(t, h, "archon-alice", tc.tool, tc.args)
			if resp.Error == nil {
				t.Fatal("expected error response")
			}
			if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeInternalError {
				t.Errorf("code = %q, want internal-error", data.Code)
			}
		})
	}
}

// --- RBAC ---

func TestSynodTools_RBACForbidden(t *testing.T) {
	// archon-alice has no synod.* permissions (empty RBAC → deny all).
	h := newRoleHandler(t, nil, &roleFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.synod.create", `{"name":"team"}`)
	if resp.Error == nil {
		t.Fatal("expected forbidden error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeForbidden {
		t.Errorf("code = %q, want forbidden", data.Code)
	}
}

// --- validation ---

func TestSynodTools_Validation(t *testing.T) {
	h := newRoleHandler(t, synodAdminCfg(), &roleFakePool{})
	cases := []struct {
		name string
		tool string
		args string
	}{
		{"create-no-name", "keeper.synod.create", `{}`},
		{"update-no-name", "keeper.synod.update", `{"description":"x"}`},
		{"update-no-description", "keeper.synod.update", `{"name":"team"}`},
		{"delete-no-name", "keeper.synod.delete", `{}`},
		{"add-no-synod", "keeper.synod.add-operator", `{"aid":"archon-bob"}`},
		{"add-no-aid", "keeper.synod.add-operator", `{"synod":"team"}`},
		{"add-bad-aid", "keeper.synod.add-operator", `{"synod":"team","aid":".bob"}`},
		{"remove-bad-aid", "keeper.synod.remove-operator", `{"synod":"team","aid":"BOB"}`},
		{"grant-no-role", "keeper.synod.grant-role", `{"synod":"team"}`},
		{"revoke-no-synod", "keeper.synod.revoke-role", `{"role":"viewer"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := callTool(t, h, "archon-alice", tc.tool, tc.args)
			if resp.Error == nil {
				t.Fatal("expected validation error")
			}
			if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
				t.Errorf("code = %q, want validation-failed", data.Code)
			}
		})
	}
}

// --- success (create) ---

// Create-success via a synod-aware fake: CreateSynod does BeginTx → INSERT
// synods → Commit; roleFakePool returns the INSERT as a no-op success.
// Checks the 2xx result + the audit-event emission (ADR-022 parity role.*).
func TestSynodTools_Create_Success(t *testing.T) {
	h, rec := newRoleHandlerRec(t, synodAdminCfg(), &roleFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.synod.create",
		`{"name":"ops-team","description":"ops"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if len(rec.events) != 1 || string(rec.events[0].EventType) != "synod.created" {
		t.Errorf("audit events = %+v, want one synod.created", rec.events)
	}
}

// Guard test for the cap: keeper.synod.create with description longer than
// [rbac.SynodDescriptionMaxLen] → validation-failed (parity with HTTP-422
// and keeper.synod.update). Without this check the create path silently
// writes a bloated payload. Boundary pair:
//   - exactly MaxLen → passes (NOT validation-failed): catches `>=` instead
//     of `>` and off-by-one (MaxLen-1);
//   - MaxLen+1 → validation-failed: catches the cap being dropped entirely.
func TestSynodTools_Create_DescriptionCap(t *testing.T) {
	t.Run("at-limit-ok", func(t *testing.T) {
		h := newRoleHandler(t, synodAdminCfg(), &roleFakePool{})
		desc := strings.Repeat("a", rbac.SynodDescriptionMaxLen)
		resp := callTool(t, h, "archon-alice", "keeper.synod.create",
			`{"name":"ops-team","description":"`+desc+`"}`)
		if resp.Error != nil {
			if data := mustToolErrorData(t, resp.Error.Data); data.Code == mcpCodeValidationFailed {
				t.Fatal("description at MaxLen rejected as validation-failed")
			}
		}
	})
	t.Run("over-limit-rejected", func(t *testing.T) {
		h := newRoleHandler(t, synodAdminCfg(), &roleFakePool{})
		desc := strings.Repeat("a", rbac.SynodDescriptionMaxLen+1)
		resp := callTool(t, h, "archon-alice", "keeper.synod.create",
			`{"name":"ops-team","description":"`+desc+`"}`)
		if resp.Error == nil {
			t.Fatal("expected validation error for over-cap description")
		}
		if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
			t.Errorf("code = %q, want validation-failed", data.Code)
		}
	})
}

// --- update (ADR-049 amend) ---

func TestSynodTools_Update_Forbidden(t *testing.T) {
	// archon-alice has no synod.update (empty RBAC → deny all) → forbidden.
	h := newRoleHandler(t, nil, &roleFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.synod.update",
		`{"name":"team","description":"x"}`)
	if resp.Error == nil {
		t.Fatal("expected forbidden error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeForbidden {
		t.Errorf("code = %q, want forbidden", data.Code)
	}
}

// Update-success: UpdateSynodDescription does UPDATE synods (roleFakePool
// returns OK 1 → synod found), emits synod.updated (ADR-022 parity).
func TestSynodTools_Update_Success(t *testing.T) {
	h, rec := newRoleHandlerRec(t, synodAdminCfg(), &roleFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.synod.update",
		`{"name":"ops-team","description":"new desc"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if len(rec.events) != 1 || string(rec.events[0].EventType) != "synod.updated" {
		t.Errorf("audit events = %+v, want one synod.updated", rec.events)
	}
}

// Guard test for the cap: keeper.synod.update with description longer than
// [rbac.SynodDescriptionMaxLen] → validation-failed (parity with HTTP-422).
// Without this check the two write paths diverge: HTTP truncates, MCP
// silently writes a bloated payload. Mutation-resistance boundary pair:
//   - exactly MaxLen → passes (NOT validation-failed): catches `>=` instead
//     of `>` and off-by-one (MaxLen-1);
//   - MaxLen+1 → validation-failed: catches the cap being dropped entirely.
func TestSynodTools_Update_DescriptionCap(t *testing.T) {
	t.Run("at-limit-ok", func(t *testing.T) {
		h := newRoleHandler(t, synodAdminCfg(), &roleFakePool{})
		desc := strings.Repeat("a", rbac.SynodDescriptionMaxLen)
		resp := callTool(t, h, "archon-alice", "keeper.synod.update",
			`{"name":"ops-team","description":"`+desc+`"}`)
		if resp.Error != nil {
			if data := mustToolErrorData(t, resp.Error.Data); data.Code == mcpCodeValidationFailed {
				t.Fatal("description at MaxLen rejected as validation-failed")
			}
		}
	})
	t.Run("over-limit-rejected", func(t *testing.T) {
		h := newRoleHandler(t, synodAdminCfg(), &roleFakePool{})
		desc := strings.Repeat("a", rbac.SynodDescriptionMaxLen+1)
		resp := callTool(t, h, "archon-alice", "keeper.synod.update",
			`{"name":"ops-team","description":"`+desc+`"}`)
		if resp.Error == nil {
			t.Fatal("expected validation error for over-cap description")
		}
		if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
			t.Errorf("code = %q, want validation-failed", data.Code)
		}
	})
}
