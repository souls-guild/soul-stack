package mcp

import (
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
)

// incWithCovens — an incFn that returns an incarnation with the given covens
// (for the RBAC scope check: effective scope = covens ∪ {name}). Status=ready,
// service=redis.
func incWithCovens(covens []string) func(string) (*incarnation.Incarnation, error) {
	return func(name string) (*incarnation.Incarnation, error) {
		now := time.Now().UTC()
		return &incarnation.Incarnation{
			Name: name, Service: "redis", ServiceVersion: "v1",
			StateSchemaVersion: 1, Status: incarnation.StatusReady,
			State: map[string]any{}, Covens: covens,
			CreatedAt: now, UpdatedAt: now,
		}, nil
	}
}

// scopedRBAC — a role with a single scoped permission (e.g.
// `incarnation.run on coven=dev`), bound to archon-alice.
func scopedRBAC(perm string) *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "scoped", Operators: []string{"archon-alice"}, Permissions: []string{perm}},
		},
	}
}

// wildcardRBAC — `*` (cluster-admin), matches any scope.
func wildcardRBAC() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "admin", Operators: []string{"archon-alice"}, Permissions: []string{"*"}},
		},
	}
}

// expectForbidden / expectAllowed — shared assertions for scope cases: they
// check the MCP error channel, not the business result (business success is
// checked by the per-tool success tests). allowed = NOT forbidden (may be
// success OR some other business error, but NOT an RBAC deny).
func expectForbidden(t *testing.T, resp jsonRPCResponse, tool string) {
	t.Helper()
	if resp.Error == nil {
		t.Fatalf("%s: expected forbidden, got success", tool)
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeForbidden {
		t.Errorf("%s: data.code = %q, want forbidden", tool, data.Code)
	}
}

func expectNotForbidden(t *testing.T, resp jsonRPCResponse, tool string) {
	t.Helper()
	if resp.Error != nil {
		if data := mustToolErrorData(t, resp.Error.Data); data.Code == mcpCodeForbidden {
			t.Errorf("%s: unexpected forbidden (scope should pass)", tool)
		}
	}
}

// TestToolsCall_IncarnationScope_DevCannotTouchProd — an operator with scope
// `incarnation.<action> on coven=dev` CANNOT run/upgrade/destroy/get/history/
// unlock a prod incarnation (covens=[prod]) via MCP. Mirrors the REST negative
// test TestRequirePermissionMulti_Negative_DevCannotTouchProd: without the
// OR-check over covens ∪ {name}, an under-privileged operator would bypass
// the REST protection.
func TestToolsCall_IncarnationScope_DevCannotTouchProd(t *testing.T) {
	prod := incWithCovens([]string{"prod"})

	t.Run("run", func(t *testing.T) {
		h, _ := newTestHandlerFull(t, &fakePool{incFn: prod}, scopedRBAC("incarnation.run on coven=dev"),
			&mcpStarter{}, &mcpResolver{ok: true}, nil)
		resp := callTool(t, h, "archon-alice", "keeper.incarnation.run",
			`{"name":"redis-prod","scenario":"rotate"}`)
		expectForbidden(t, resp, "run")
	})

	t.Run("upgrade", func(t *testing.T) {
		h, _ := newTestHandlerFull(t, &fakePool{incFn: prod}, scopedRBAC("incarnation.upgrade on coven=dev"),
			nil, &mcpResolver{ok: true}, &mcpLoader{targetSchema: 2})
		resp := callTool(t, h, "archon-alice", "keeper.incarnation.upgrade",
			`{"name":"redis-prod","to_version":"v2"}`)
		expectForbidden(t, resp, "upgrade")
	})

	t.Run("destroy", func(t *testing.T) {
		destroyer := &mcpDestroyer{}
		h, _ := newTestHandlerDestroy(t, &fakePool{incFn: prod}, scopedRBAC("incarnation.destroy on coven=dev"),
			destroyer, true)
		resp := callTool(t, h, "archon-alice", "keeper.incarnation.destroy",
			`{"name":"redis-prod","allow_destroy":false}`)
		expectForbidden(t, resp, "destroy")
		if destroyer.calls != 0 {
			t.Error("denied destroy must not start teardown")
		}
	})

	t.Run("get", func(t *testing.T) {
		h, _, _ := newTestHandler(t, &fakePool{incFn: prod}, scopedRBAC("incarnation.get on coven=dev"))
		resp := callTool(t, h, "archon-alice", "keeper.incarnation.get", `{"name":"redis-prod"}`)
		expectForbidden(t, resp, "get")
	})

	t.Run("history", func(t *testing.T) {
		h, _, _ := newTestHandler(t, &fakePool{incFn: prod}, scopedRBAC("incarnation.history on coven=dev"))
		resp := callTool(t, h, "archon-alice", "keeper.incarnation.history", `{"name":"redis-prod"}`)
		expectForbidden(t, resp, "history")
	})

	t.Run("unlock", func(t *testing.T) {
		// the unlock FOR-UPDATE select would return the same prod row, but the
		// RBAC probe denies BEFORE BeginTx.
		pool := &fakePool{
			incFn:    incWithCovens([]string{"prod"}),
			beginErr: errFakeUnexpected{sql: "BeginTx must not run when scope denies"},
		}
		h, _ := newTestHandlerFull(t, pool, scopedRBAC("incarnation.unlock on coven=dev"), nil, nil, nil)
		resp := callTool(t, h, "archon-alice", "keeper.incarnation.unlock",
			`{"name":"redis-prod","reason":"x"}`)
		expectForbidden(t, resp, "unlock")
	})
}

// TestToolsCall_IncarnationScope_MatchingCovenPasses — positive case: scope
// coven=prod matches the prod incarnation (covens=[prod]) → NOT forbidden
// (run passes RBAC).
func TestToolsCall_IncarnationScope_MatchingCovenPasses(t *testing.T) {
	prod := incWithCovens([]string{"prod"})
	h, _ := newTestHandlerFull(t, &fakePool{incFn: prod}, scopedRBAC("incarnation.run on coven=prod"),
		&mcpStarter{}, &mcpResolver{ok: true}, nil)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.run",
		`{"name":"redis-prod","scenario":"rotate"}`)
	expectNotForbidden(t, resp, "run")
	if resp.Error != nil {
		t.Fatalf("matching coven should fully pass: %+v", resp.Error)
	}
}

// TestToolsCall_IncarnationScope_NameAsCovenDenied — NIM-124: the incarnation
// name is NOT a Coven, so scope coven=<name> no longer matches (must migrate to
// incarnation=<name>). Fail-closed: forbidden.
func TestToolsCall_IncarnationScope_NameAsCovenDenied(t *testing.T) {
	noCovens := incWithCovens(nil)
	h, _ := newTestHandlerFull(t, &fakePool{
		incFn:    noCovens,
		beginErr: errFakeUnexpected{sql: "BeginTx must not run when scope denies"},
	}, scopedRBAC("incarnation.run on coven=redis-prod"),
		&mcpStarter{}, &mcpResolver{ok: true}, nil)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.run",
		`{"name":"redis-prod","scenario":"rotate"}`)
	expectForbidden(t, resp, "run")
}

// TestToolsCall_IncarnationScope_NameAsIncarnationPasses — NIM-124: scope by the
// incarnation's own name is the incarnation=<name> dimension, and it passes.
func TestToolsCall_IncarnationScope_NameAsIncarnationPasses(t *testing.T) {
	noCovens := incWithCovens(nil)
	h, _ := newTestHandlerFull(t, &fakePool{incFn: noCovens}, scopedRBAC("incarnation.run on incarnation=redis-prod"),
		&mcpStarter{}, &mcpResolver{ok: true}, nil)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.run",
		`{"name":"redis-prod","scenario":"rotate"}`)
	expectNotForbidden(t, resp, "run")
	if resp.Error != nil {
		t.Fatalf("incarnation=<name> should fully pass: %+v", resp.Error)
	}
}

// TestToolsCall_IncarnationScope_BarePermissionPasses — a bare (non-scoped)
// permission matches any incarnation regardless of covens (parity with the
// REST no-regression test).
func TestToolsCall_IncarnationScope_BarePermissionPasses(t *testing.T) {
	prod := incWithCovens([]string{"prod"})
	h, _ := newTestHandlerFull(t, &fakePool{incFn: prod}, scopedRBAC("incarnation.run"),
		&mcpStarter{}, &mcpResolver{ok: true}, nil)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.run",
		`{"name":"redis-prod","scenario":"rotate"}`)
	expectNotForbidden(t, resp, "run")
	if resp.Error != nil {
		t.Fatalf("bare permission should fully pass: %+v", resp.Error)
	}
}

// TestToolsCall_IncarnationScope_WildcardPasses — `*` (cluster-admin) matches
// any incarnation (parity with the REST wildcard-no-regression test).
func TestToolsCall_IncarnationScope_WildcardPasses(t *testing.T) {
	prod := incWithCovens([]string{"prod"})
	h, _ := newTestHandlerFull(t, &fakePool{incFn: prod}, wildcardRBAC(),
		&mcpStarter{}, &mcpResolver{ok: true}, nil)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.run",
		`{"name":"redis-prod","scenario":"rotate"}`)
	expectNotForbidden(t, resp, "run")
	if resp.Error != nil {
		t.Fatalf("wildcard should fully pass: %+v", resp.Error)
	}
}
