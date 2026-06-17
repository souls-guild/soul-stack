package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// mcpDestroyer — мок [handlers.DestroyStarter]: фиксирует teardown-spec + число
// вызовов StartDestroy.
type mcpDestroyer struct {
	gotSpec scenario.RunSpec
	calls   int
	err     error
}

func (f *mcpDestroyer) StartDestroy(_ context.Context, spec scenario.RunSpec) error {
	f.calls++
	f.gotSpec = spec
	return f.err
}

func destroyerRBAC() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "destroyer", Operators: []string{"archon-alice"}, Permissions: []string{"incarnation.destroy"}},
		},
	}
}

// newTestHandlerDestroy собирает Handler с полным destroy-стеком (destroyer +
// registry + loader). hasScenario управляет наличием scenario `destroy` в
// снапшоте (mcpLoader.ReadFile).
func newTestHandlerDestroy(t *testing.T, pool *fakePool, rbacCfg *rbactest.Config, destroyer handlers.DestroyStarter, hasScenario bool) (*Handler, *recordingAudit) {
	t.Helper()
	enf, err := rbactest.NewEnforcer(rbacCfg)
	if err != nil {
		t.Fatalf("NewEnforcer: %v", err)
	}
	svc, err := operator.NewService(operator.ServiceDeps{
		Pool:       pool,
		Issuer:     &fakeIssuer{},
		RBAC:       enf,
		TTLDefault: time.Hour,
		Logger:     slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	rec := &recordingAudit{}
	h, err := NewHandler(HandlerDeps{
		OperatorSvc:       svc,
		RBAC:              enf,
		AuditWriter:       rec,
		Logger:            slog.New(slog.NewJSONHandler(io.Discard, nil)),
		IncarnationDB:     pool,
		ScenarioDestroyer: destroyer,
		ServiceRegistry:   &mcpResolver{ok: true},
		ServiceLoader:     &mcpLoader{hasDestroyScenario: hasScenario},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h, rec
}

// --- teardown-путь (allow_destroy=false, scenario есть) ---------------

func TestToolsCall_IncarnationDestroy_Teardown_Success(t *testing.T) {
	pool := &fakePool{incFn: incWithStatus(incarnation.StatusReady)}
	destroyer := &mcpDestroyer{}
	h, rec := newTestHandlerDestroy(t, pool, destroyerRBAC(), destroyer, true)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.destroy",
		`{"name":"redis-prod","allow_destroy":false}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var res toolsCallResult
	_ = json.Unmarshal(resp.Result, &res)
	var out incarnationDestroyOutput
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !audit.IsValidULID(out.ApplyID) {
		t.Errorf("_apply_id not ULID: %q", out.ApplyID)
	}
	if destroyer.calls != 1 || destroyer.gotSpec.ApplyID != out.ApplyID {
		t.Errorf("teardown spec mismatch: %+v (calls=%d)", destroyer.gotSpec, destroyer.calls)
	}
	// Teardown катится развёрнутой версией сервиса.
	if destroyer.gotSpec.ServiceRef.Ref != "v1" {
		t.Errorf("teardown ServiceRef.Ref = %q, want v1", destroyer.gotSpec.ServiceRef.Ref)
	}
	// audit destroy_started (source=mcp), force=false.
	if !recHasEvent(rec, audit.EventIncarnationDestroyStarted) {
		t.Errorf("ожидался destroy_started")
	}
	ev := recEvent(rec, audit.EventIncarnationDestroyStarted)
	if ev == nil || ev.Source != audit.SourceMCP {
		t.Errorf("destroy_started source = %v, want mcp", ev)
	}
	if ev.Payload["force"] != false {
		t.Errorf("destroy_started force = %v, want false", ev.Payload["force"])
	}
}

// --- 422: allow_destroy=false и нет scenario ---------------------------

func TestToolsCall_IncarnationDestroy_NoScenario_NoForce(t *testing.T) {
	pool := &fakePool{incFn: incWithStatus(incarnation.StatusReady)}
	destroyer := &mcpDestroyer{}
	h, rec := newTestHandlerDestroy(t, pool, destroyerRBAC(), destroyer, false)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.destroy",
		`{"name":"redis-prod","allow_destroy":false}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Errorf("data.code = %q, want validation-failed", data.Code)
	}
	if destroyer.calls != 0 {
		t.Error("teardown must NOT start (отказ pre-check)")
	}
	if len(rec.events) != 0 {
		t.Error("отказ pre-check не должен писать audit")
	}
}

// --- force-путь (allow_destroy=true, без scenario) → DELETE -----------

func TestToolsCall_IncarnationDestroy_Force_Delete(t *testing.T) {
	pool := &fakePool{incFn: incWithStatus(incarnation.StatusReady)}
	destroyer := &mcpDestroyer{}
	h, rec := newTestHandlerDestroy(t, pool, destroyerRBAC(), destroyer, false)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.destroy",
		`{"name":"redis-prod","allow_destroy":true}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if destroyer.calls != 0 {
		t.Error("force пропускает teardown")
	}
	// destroy_started (force=true) + destroy_completed (force-DELETE).
	if !recHasEvent(rec, audit.EventIncarnationDestroyStarted) {
		t.Errorf("ожидался destroy_started")
	}
	if !recHasEvent(rec, audit.EventIncarnationDestroyCompleted) {
		t.Errorf("ожидался destroy_completed (force-DELETE)")
	}
	if ev := recEvent(rec, audit.EventIncarnationDestroyStarted); ev == nil || ev.Payload["force"] != true {
		t.Errorf("destroy_started force payload = %v, want true", ev)
	}
}

// --- force-DELETE no-op (RowsAffected==0) → success, без completed ----

func TestToolsCall_IncarnationDestroy_Force_DeleteNoOp(t *testing.T) {
	pool := &fakePool{
		incFn:     incWithStatus(incarnation.StatusReady),
		deleteTag: pgconn.NewCommandTag("DELETE 0"),
	}
	h, rec := newTestHandlerDestroy(t, pool, destroyerRBAC(), &mcpDestroyer{}, false)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.destroy",
		`{"name":"redis-prod","allow_destroy":true}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if recHasEvent(rec, audit.EventIncarnationDestroyCompleted) {
		t.Errorf("destroy_completed не должен писаться при no-op DELETE")
	}
}

// --- 409: статус не допускает destroy (applying) ----------------------

func TestToolsCall_IncarnationDestroy_NotDestroyable(t *testing.T) {
	pool := &fakePool{incFn: incWithStatus(incarnation.StatusApplying)}
	destroyer := &mcpDestroyer{}
	h, _ := newTestHandlerDestroy(t, pool, destroyerRBAC(), destroyer, true)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.destroy",
		`{"name":"redis-prod","allow_destroy":false}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeIncarnationLocked {
		t.Errorf("data.code = %q, want incarnation-locked", data.Code)
	}
	if destroyer.calls != 0 {
		t.Error("applying не запускает teardown")
	}
}

// --- 404: incarnation не существует ------------------------------------

func TestToolsCall_IncarnationDestroy_NotFound(t *testing.T) {
	pool := &fakePool{incFn: func(string) (*incarnation.Incarnation, error) { return nil, pgx.ErrNoRows }}
	h, _ := newTestHandlerDestroy(t, pool, destroyerRBAC(), &mcpDestroyer{}, true)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.destroy",
		`{"name":"ghost","allow_destroy":false}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeNotFound {
		t.Errorf("data.code = %q, want not-found", data.Code)
	}
}

// --- RBAC forbidden ---------------------------------------------------

func TestToolsCall_IncarnationDestroy_RBACForbidden(t *testing.T) {
	// RBAC пуст → deny. SelectByName РЕЗОЛВИТ scope (covens ∪ {name}) для
	// OR-Check (зеркало REST middleware), затем enforcer отказывает → forbidden.
	// teardown/audit при отказе НЕ стартуют.
	pool := &fakePool{incFn: incWithStatus(incarnation.StatusReady)}
	destroyer := &mcpDestroyer{}
	h, rec := newTestHandlerDestroy(t, pool, nil, destroyer, true)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.destroy",
		`{"name":"redis-prod","allow_destroy":false}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeForbidden {
		t.Errorf("data.code = %q, want forbidden", data.Code)
	}
	if destroyer.calls != 0 || len(rec.events) != 0 {
		t.Error("denied destroy must not start teardown / write audit")
	}
}

// --- validation: allow_destroy отсутствует ----------------------------

func TestToolsCall_IncarnationDestroy_MissingAllowDestroy(t *testing.T) {
	pool := &fakePool{incFn: incWithStatus(incarnation.StatusReady)}
	h, _ := newTestHandlerDestroy(t, pool, destroyerRBAC(), &mcpDestroyer{}, true)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.destroy",
		`{"name":"redis-prod"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Errorf("data.code = %q, want validation-failed", data.Code)
	}
}

// --- 500: destroy не сконфигурирован ----------------------------------

func TestToolsCall_IncarnationDestroy_NotConfigured(t *testing.T) {
	pool := &fakePool{incFn: incWithStatus(incarnation.StatusReady)}
	// ScenarioDestroyer не прокинут (newTestHandlerFull без destroyer).
	h, _ := newTestHandlerFull(t, pool, destroyerRBAC(), nil, &mcpResolver{ok: true}, &mcpLoader{hasDestroyScenario: true})

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.destroy",
		`{"name":"redis-prod","allow_destroy":false}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeInternalError {
		t.Errorf("data.code = %q, want internal-error", data.Code)
	}
}

// --- helpers ----------------------------------------------------------

func recHasEvent(rec *recordingAudit, et audit.EventType) bool {
	return recEvent(rec, et) != nil
}

func recEvent(rec *recordingAudit, et audit.EventType) *audit.Event {
	for _, ev := range rec.events {
		if ev.EventType == et {
			return ev
		}
	}
	return nil
}
