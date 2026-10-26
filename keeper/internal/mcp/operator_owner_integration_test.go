//go:build integration

// keeper.operator.set-owner (ADR-014 amendment NIM-1057) over the real MCP
// server and Postgres: the owner lands, the audit payload names the new and
// the previous owner, and clearing writes the same event without the owner.

package mcp

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
)

func TestIntegration_OperatorSetOwner_AuditNamesNewAndPrevious(t *testing.T) {
	truncateOperators(t)
	ctx := context.Background()
	seedOperator(t, "archon-admin", "")
	seedOperator(t, "dba-sd", "archon-admin")
	for _, aid := range []string{"alice", "bob"} {
		if err := operator.Insert(ctx, integrationPool, &operator.Operator{AID: aid, DisplayName: aid, AuthMethod: operator.AuthMethodLocal}); err != nil {
			t.Fatalf("seed %s: %v", aid, err)
		}
	}
	if _, err := integrationPool.Exec(ctx, `INSERT INTO synods (name) VALUES ('team')`); err != nil {
		t.Fatalf("seed synod: %v", err)
	}

	base, stop := startMCPServer(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "owners", Operators: []string{"archon-admin"}, Permissions: []string{"operator.set-owner"}},
	}})
	defer stop()
	tok := newToken(t, "archon-admin", []string{"owners"})
	set := func(args map[string]any) {
		t.Helper()
		if resp := callRoleTool(t, base, tok, "keeper.operator.set-owner", args); resp.Error != nil {
			t.Fatalf("set-owner %v: %+v", args, resp.Error)
		}
	}
	owner := func() (aid, synod *string) {
		t.Helper()
		if err := integrationPool.QueryRow(ctx, `SELECT owner_aid, owner_synod FROM operators WHERE aid = 'dba-sd'`).Scan(&aid, &synod); err != nil {
			t.Fatalf("read owner: %v", err)
		}
		return aid, synod
	}
	lastPayload := func() map[string]any {
		t.Helper()
		var p map[string]any
		if err := integrationPool.QueryRow(ctx,
			`SELECT payload FROM audit_log WHERE event_type = 'operator.owner-set' AND source = 'mcp' ORDER BY created_at DESC, audit_id DESC LIMIT 1`).Scan(&p); err != nil {
			t.Fatalf("read audit: %v", err)
		}
		return p
	}

	set(map[string]any{"aid": "dba-sd", "owner_aid": "alice"})
	if a, s := owner(); a == nil || *a != "alice" || s != nil {
		t.Fatalf("owner after set = %v / %v", a, s)
	}
	if p := lastPayload(); p["owner_aid"] != "alice" || p["previous_owner_aid"] != nil || p["previous_owner_synod"] != nil {
		t.Errorf("first set payload = %v", p)
	}

	set(map[string]any{"aid": "dba-sd", "owner_synod": "team"})
	if p := lastPayload(); p["owner_synod"] != "team" || p["owner_aid"] != nil || p["previous_owner_aid"] != "alice" {
		t.Errorf("replace payload = %v, want owner_synod team, previous_owner_aid alice", p)
	}

	set(map[string]any{"aid": "dba-sd"})
	if a, s := owner(); a != nil || s != nil {
		t.Fatalf("owner after clear = %v / %v", a, s)
	}
	if p := lastPayload(); p["owner_aid"] != nil || p["owner_synod"] != nil || p["previous_owner_synod"] != "team" {
		t.Errorf("clear payload = %v, want no owner, previous_owner_synod team", p)
	}

	if code := mcpErrCode(t, callRoleTool(t, base, tok, "keeper.operator.set-owner", map[string]any{"aid": "dba-sd", "owner_aid": "archon-admin"})); code != mcpCodeValidationFailed {
		t.Errorf("a machine operator as owner: code = %q, want %q", code, mcpCodeValidationFailed)
	}
	if code := mcpErrCode(t, callRoleTool(t, base, newToken(t, "bob", nil), "keeper.operator.set-owner", map[string]any{"aid": "dba-sd", "owner_aid": "bob"})); code != mcpCodeForbidden {
		t.Errorf("without operator.set-owner: code = %q, want %q", code, mcpCodeForbidden)
	}
}
