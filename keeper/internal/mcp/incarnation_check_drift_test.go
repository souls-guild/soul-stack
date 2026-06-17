package mcp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// mcpDrift — мок [handlers.DriftChecker]: фиксирует spec + число вызовов и
// возвращает заранее заготовленный DriftReport / ошибку.
type mcpDrift struct {
	gotSpec  scenario.CheckDriftSpec
	calls    int
	report   *scenario.DriftReport
	err      error
	marked   bool
	markName string
	markHas  bool
}

func (f *mcpDrift) CheckDrift(_ context.Context, spec scenario.CheckDriftSpec) (*scenario.DriftReport, error) {
	f.calls++
	f.gotSpec = spec
	return f.report, f.err
}

func (f *mcpDrift) MarkDriftStatus(_ context.Context, name string, hasDrift bool) error {
	f.marked = true
	f.markName = name
	f.markHas = hasDrift
	return nil
}

func driftRBAC() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "drifter", Operators: []string{"archon-alice"}, Permissions: []string{"incarnation.check-drift"}},
		},
	}
}

// newTestHandlerDrift собирает Handler с drift-стеком (drift + registry).
func newTestHandlerDrift(t *testing.T, pool *fakePool, rbacCfg *rbactest.Config, drift handlers.DriftChecker) (*Handler, *recordingAudit) {
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
		OperatorSvc:     svc,
		RBAC:            enf,
		AuditWriter:     rec,
		Logger:          slog.New(slog.NewJSONHandler(io.Discard, nil)),
		IncarnationDB:   pool,
		ScenarioDrift:   drift,
		ServiceRegistry: &mcpResolver{ok: true},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h, rec
}

func sampleDriftReport() *scenario.DriftReport {
	return &scenario.DriftReport{
		CheckedAt:       time.Now().UTC(),
		IncarnationName: "redis-prod",
		ScenarioRef:     scenario.ConvergeScenarioName,
		Hosts: []scenario.DriftHostReport{
			{
				SID: "host-a.example.com", Status: scenario.DriftStatusDrifted,
				Tasks: []scenario.DriftTaskResult{
					{Idx: 0, Module: "core.file.present", Changed: true},
				},
			},
		},
		Summary: scenario.DriftSummary{HostsDrifted: 1},
	}
}

// TestToolsCall_IncarnationCheckDrift_Success — happy-path: возвращается
// DriftReport, MarkDriftStatus вызван с hasDrift=true (есть drifted-хост),
// audit пишется с correlation_id=apply_id.
func TestToolsCall_IncarnationCheckDrift_Success(t *testing.T) {
	pool := &fakePool{incFn: incWithStatus(incarnation.StatusReady)}
	drift := &mcpDrift{report: sampleDriftReport()}
	h, rec := newTestHandlerDrift(t, pool, driftRBAC(), drift)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.check-drift",
		`{"name":"redis-prod"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if drift.calls != 1 {
		t.Errorf("CheckDrift calls = %d, want 1", drift.calls)
	}
	if !drift.marked || drift.markName != "redis-prod" || !drift.markHas {
		t.Errorf("MarkDriftStatus state = (%v,%q,%v), want (true,redis-prod,true)",
			drift.marked, drift.markName, drift.markHas)
	}
	// Audit.
	found := false
	for _, ev := range rec.events {
		if ev.EventType == audit.EventIncarnationDriftChecked {
			found = true
			if ev.CorrelationID == "" {
				t.Error("audit: correlation_id (apply_id) пуст")
			}
			if ev.Source != audit.SourceMCP {
				t.Errorf("audit source = %s, want mcp", ev.Source)
			}
		}
	}
	if !found {
		t.Error("audit: incarnation.drift_checked не записан")
	}
}

// TestToolsCall_IncarnationCheckDrift_ConvergeMissing — Runner вернул
// ErrConvergeMissing → 422 validation-failed, не internal-error.
func TestToolsCall_IncarnationCheckDrift_ConvergeMissing(t *testing.T) {
	pool := &fakePool{incFn: incWithStatus(incarnation.StatusReady)}
	drift := &mcpDrift{err: scenario.ErrConvergeMissing}
	h, _ := newTestHandlerDrift(t, pool, driftRBAC(), drift)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.check-drift",
		`{"name":"redis-prod"}`)
	if resp.Error == nil {
		t.Fatal("expected error, got success")
	}
	data := mustToolErrorData(t, resp.Error.Data)
	if data.Code != mcpCodeValidationFailed {
		t.Errorf("data.code = %q, want validation-failed", data.Code)
	}
}

// TestToolsCall_IncarnationCheckDrift_InputMissing — drift-input не резолвится
// → validation-failed.
func TestToolsCall_IncarnationCheckDrift_InputMissing(t *testing.T) {
	pool := &fakePool{incFn: incWithStatus(incarnation.StatusReady)}
	drift := &mcpDrift{err: errors.New("scenario: required: foo")}
	// Возвращаем ErrDriftInputMissing напрямую через wrap.
	drift.err = scenario.ErrDriftInputMissing
	h, _ := newTestHandlerDrift(t, pool, driftRBAC(), drift)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.check-drift",
		`{"name":"redis-prod"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	data := mustToolErrorData(t, resp.Error.Data)
	if data.Code != mcpCodeValidationFailed {
		t.Errorf("data.code = %q, want validation-failed", data.Code)
	}
}

// TestToolsCall_IncarnationCheckDrift_ScopeDeniesProd — оператор со scope
// `incarnation.check-drift on coven=dev` НЕ может проверить prod-incarnation
// (covens=[prod]) — паритет destroy/run scope-теста.
func TestToolsCall_IncarnationCheckDrift_ScopeDeniesProd(t *testing.T) {
	prod := incWithCovens([]string{"prod"})
	drift := &mcpDrift{}
	h, _ := newTestHandlerDrift(t, &fakePool{incFn: prod},
		scopedRBAC("incarnation.check-drift on coven=dev"), drift)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.check-drift",
		`{"name":"redis-prod"}`)
	expectForbidden(t, resp, "check-drift")
	if drift.calls != 0 {
		t.Error("denied check-drift must not run CheckDrift")
	}
}

// TestToolsCall_IncarnationCheckDrift_ScopeMatchingCovenPasses — scope coven=prod
// матчит prod-incarnation (covens=[prod]).
func TestToolsCall_IncarnationCheckDrift_ScopeMatchingCovenPasses(t *testing.T) {
	prod := incWithCovens([]string{"prod"})
	drift := &mcpDrift{report: sampleDriftReport()}
	h, _ := newTestHandlerDrift(t, &fakePool{incFn: prod},
		scopedRBAC("incarnation.check-drift on coven=prod"), drift)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.check-drift",
		`{"name":"redis-prod"}`)
	expectNotForbidden(t, resp, "check-drift")
}

// TestToolsCall_IncarnationCheckDrift_NotConfigured — runner=nil → internal-error.
func TestToolsCall_IncarnationCheckDrift_NotConfigured(t *testing.T) {
	pool := &fakePool{incFn: incWithStatus(incarnation.StatusReady)}
	h, _ := newTestHandlerDrift(t, pool, driftRBAC(), nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.check-drift",
		`{"name":"redis-prod"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	data := mustToolErrorData(t, resp.Error.Data)
	if data.Code != mcpCodeInternalError {
		t.Errorf("data.code = %q, want internal-error", data.Code)
	}
}
