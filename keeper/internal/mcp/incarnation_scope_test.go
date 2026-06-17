package mcp

import (
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
)

// incWithCovens — incFn, отдающий incarnation с заданными covens (для
// RBAC-scope-проверки: эффективный scope = covens ∪ {name}). Status=ready,
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

// scopedRBAC — роль с одним scoped permission (например
// `incarnation.run on coven=dev`), привязанная к archon-alice.
func scopedRBAC(perm string) *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "scoped", Operators: []string{"archon-alice"}, Permissions: []string{perm}},
		},
	}
}

// wildcardRBAC — `*` (cluster-admin), матчит любой scope.
func wildcardRBAC() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "admin", Operators: []string{"archon-alice"}, Permissions: []string{"*"}},
		},
	}
}

// expectForbidden / expectAllowed — общие ассерты для scope-кейсов: проверяют
// MCP-error-канал, не бизнес-результат (бизнес-успех проверяют per-tool
// success-тесты). allowed = НЕ forbidden (может быть успех ИЛИ иная бизнес-
// ошибка, но НЕ RBAC-deny).
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

// TestToolsCall_IncarnationScope_DevCannotTouchProd — оператор со scope
// `incarnation.<action> on coven=dev` НЕ может run/upgrade/destroy/get/history/
// unlock prod-incarnation (covens=[prod]) через MCP. Зеркало REST negative-
// теста TestRequirePermissionMulti_Negative_DevCannotTouchProd: без OR-Check
// по covens ∪ {name} под-привилегированный оператор обходил бы REST-защиту.
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
		// unlock-FOR-UPDATE-select вернёт ту же prod-строку, но RBAC-probe
		// отказывает ДО BeginTx.
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

// TestToolsCall_IncarnationScope_MatchingCovenPasses — позитив: scope coven=prod
// матчит prod-incarnation (covens=[prod]) → НЕ forbidden (run проходит RBAC).
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

// TestToolsCall_IncarnationScope_NameAsCovenPasses — scope coven=<name> матчит
// через корневую Coven-метку (covens ∪ {name}, ADR-008): даже если declared
// covens пуст, имя incarnation работает как coven-метка.
func TestToolsCall_IncarnationScope_NameAsCovenPasses(t *testing.T) {
	noCovens := incWithCovens(nil)
	h, _ := newTestHandlerFull(t, &fakePool{incFn: noCovens}, scopedRBAC("incarnation.run on coven=redis-prod"),
		&mcpStarter{}, &mcpResolver{ok: true}, nil)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.run",
		`{"name":"redis-prod","scenario":"rotate"}`)
	expectNotForbidden(t, resp, "run")
	if resp.Error != nil {
		t.Fatalf("name-as-coven should fully pass: %+v", resp.Error)
	}
}

// TestToolsCall_IncarnationScope_BarePermissionPasses — bare (non-scoped)
// permission матчит любую incarnation независимо от covens (паритет REST
// no-regression-теста).
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

// TestToolsCall_IncarnationScope_WildcardPasses — `*` (cluster-admin) матчит
// любую incarnation (паритет REST wildcard-no-regression).
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
