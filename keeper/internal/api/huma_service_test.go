package api

// Guard-тесты ТИРАЖ-БАТЧА-2d разворота SERVICE-домена (реестр Service-ов) ЦЕЛИКОМ
// на huma full-typed (ADR-054 §Pattern, эталоны role/operator/augur/herald).
// register/update/deregister — WRITE+AUDIT (вариант B, huma-audit-middleware;
// события service.registered/.updated/.deregistered); list/get + refs/scenarios/
// state-schema/dependencies — read (БЕЗ audit). Доказывают инварианты кластера
// поверх chi:
//
//   - wire/golden: register 201 ServiceView; update 200 ServiceView; deregister 204
//     пустое; list 200 envelope; get 200; refs/scenarios/state-schema/dependencies
//     200 byte-exact;
//   - unknown-field → 400; missing-required → 422; RBAC-deny → 403; get-404;
//   - S6-GUARD на КАЖДЫЙ write-роут: полная huma-навеска пишет audit-event с НЕПУСТЫМ
//     payload + ПРАВИЛЬНЫМ event-type на 2xx и НЕ пишет на 4xx/403.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// svcAt — фиксированный created_at/updated_at для детерминированного golden wire.
var svcAt = time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)

// hSvcPool — узкий мок [serviceregistry.ServicePool] для huma-теста success-path.
// Классифицирует SQL по подстроке и отдаёт детерминированный исход; error-
// классификацию валидируют handlers/service_test.go + serviceregistry-integration.
type hSvcPool struct {
	deleteRows int64
	getMissing bool    // SELECT … WHERE name → ErrNoRows (404)
	getValues  []any   // строка SELECT … WHERE name (Get)
	listValues [][]any // строки SELECT … ORDER BY name (List)
}

func (p *hSvcPool) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "DELETE FROM service_registry") {
		return pgconn.NewCommandTag("DELETE " + hSvcItoa(p.deleteRows)), nil
	}
	return pgconn.CommandTag{}, &hSvcErr{"hSvcPool: unexpected Exec SQL: " + sql}
}

func (p *hSvcPool) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "INSERT INTO service_registry"):
		return hSvcRow{values: []any{svcAt, svcAt}} // RETURNING created_at, updated_at
	case strings.Contains(sql, "UPDATE service_registry"):
		return hSvcRow{values: []any{svcAt, svcAt}}
	case strings.Contains(sql, "FROM service_registry"):
		if p.getMissing || p.getValues == nil {
			return hSvcRow{err: pgx.ErrNoRows}
		}
		return hSvcRow{values: p.getValues}
	}
	return hSvcRow{err: &hSvcErr{"hSvcPool: unexpected QueryRow SQL: " + sql}}
}

func (p *hSvcPool) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "FROM service_registry") && strings.Contains(sql, "ORDER BY name") {
		return &hSvcRows{rows: p.listValues}, nil
	}
	return nil, &hSvcErr{"hSvcPool: unexpected Query SQL: " + sql}
}

type hSvcErr struct{ s string }

func (e *hSvcErr) Error() string { return e.s }

func hSvcItoa(n int64) string {
	if n == 0 {
		return "0"
	}
	return "1"
}

// hSvcRow — staticRow для service-колонок (string/time + nullable **string).
type hSvcRow struct {
	values []any
	err    error
}

func (r hSvcRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, d := range dest {
		switch dd := d.(type) {
		case *string:
			*dd = r.values[i].(string)
		case *time.Time:
			*dd = r.values[i].(time.Time)
		case **string:
			if r.values[i] == nil {
				*dd = nil
			} else {
				s := r.values[i].(string)
				*dd = &s
			}
		}
	}
	return nil
}

type hSvcRows struct {
	rows [][]any
	idx  int
}

func (r *hSvcRows) Next() bool { r.idx++; return r.idx <= len(r.rows) }
func (r *hSvcRows) Scan(dest ...any) error {
	return hSvcRow{values: r.rows[r.idx-1]}.Scan(dest...)
}
func (r *hSvcRows) Err() error                                   { return nil }
func (r *hSvcRows) Close()                                       {}
func (r *hSvcRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *hSvcRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *hSvcRows) Values() ([]any, error)                       { return nil, nil }
func (r *hSvcRows) RawValues() [][]byte                          { return nil }
func (r *hSvcRows) Conn() *pgx.Conn                              { return nil }

// hSvcRefsLister/hSvcScenarioLister/etc — lister-стабы для sub-read-роутов
// (refs/scenarios/state-schema/dependencies). nil-вариант (lister не передан)
// проверяет 500 «not configured»; здесь — success-исход.
type hSvcRefsLister struct{ refs []artifact.GitRef }

func (l hSvcRefsLister) ListRefs(context.Context, string, string) ([]artifact.GitRef, error) {
	return l.refs, nil
}

type hSvcScenarioLister struct{ scenarios []artifact.Scenario }

func (l hSvcScenarioLister) ListScenarios(context.Context, string, string, string) ([]artifact.Scenario, error) {
	return l.scenarios, nil
}

type hSvcStateSchemaLister struct{ info *artifact.StateSchemaInfo }

func (l hSvcStateSchemaLister) ListStateSchema(context.Context, string, string, string) (*artifact.StateSchemaInfo, error) {
	return l.info, nil
}

type hSvcDepsLister struct{ deps *artifact.ServiceDependencies }

func (l hSvcDepsLister) ListDependencies(context.Context, string, string, string) (*artifact.ServiceDependencies, error) {
	return l.deps, nil
}

// hSvcRefCapture* — lister-стабы, ФИКСИРУЮЩИЕ переданный ref (указатель пишется
// при вызове). Доказывают, что huma реально биндит ?ref=<git-ref> и override
// доезжает от query-параметра до lister-а (без них ветка `if ref==""` покрыта
// лишь дефолтом из реестра). gotRef — *string, чтобы тест считал значение после
// ServeHTTP (lister передаётся в роутер по значению).
type hSvcRefCaptureScenario struct {
	gotRef    *string
	scenarios []artifact.Scenario
}

func (l hSvcRefCaptureScenario) ListScenarios(_ context.Context, _, _, ref string) ([]artifact.Scenario, error) {
	*l.gotRef = ref
	return l.scenarios, nil
}

type hSvcRefCaptureStateSchema struct {
	gotRef *string
	info   *artifact.StateSchemaInfo
}

func (l hSvcRefCaptureStateSchema) ListStateSchema(_ context.Context, _, _, ref string) (*artifact.StateSchemaInfo, error) {
	*l.gotRef = ref
	return l.info, nil
}

type hSvcRefCaptureDeps struct {
	gotRef *string
	deps   *artifact.ServiceDependencies
}

func (l hSvcRefCaptureDeps) ListDependencies(_ context.Context, _, _, ref string) (*artifact.ServiceDependencies, error) {
	*l.gotRef = ref
	return l.deps, nil
}

// hSvcErrScenario / hSvcErrDeps — lister-стабы, возвращающие ошибку git-источника
// (ls-remote/clone недоступен). Доказывают 502-tier: handler маппит ошибку
// lister-а в TypeBadGateway (а не 500). Симметричны с hSvcScenarioLister/
// hSvcDepsLister, но err≠nil.
type hSvcErrScenario struct{}

func (hSvcErrScenario) ListScenarios(context.Context, string, string, string) ([]artifact.Scenario, error) {
	return nil, &hSvcErr{"git clone failed: connection refused"}
}

type hSvcErrDeps struct{}

func (hSvcErrDeps) ListDependencies(context.Context, string, string, string) (*artifact.ServiceDependencies, error) {
	return nil, &hSvcErr{"git ls-remote failed: connection refused"}
}

// humaServiceRouter собирает chi-роутер со ВСЕМИ service-роутами через huma —
// продакшен-навеска буквально из router.go: RequirePermission(service.<action>) на
// каждой группе + (для write) huma-audit-middleware варианта B + huma-операция.
// injectClaims заменяет RequireJWT. lister-ы переданы — sub-read success-path.
func humaServiceRouter(t *testing.T, enforcer apimiddleware.PermissionChecker, auditW audit.Writer, pool *hSvcPool,
	refs handlers.ServiceRefsLister, scenarios handlers.ServiceScenarioLister,
	stateSchema handlers.ServiceStateSchemaLister, deps handlers.ServiceDependenciesLister) *chi.Mux {
	t.Helper()
	installHumaErrorOverride()
	svc, err := serviceregistry.NewService(serviceregistry.ServiceDeps{Pool: pool})
	if err != nil {
		t.Fatalf("serviceregistry.NewService: %v", err)
	}
	serviceH := handlers.NewServiceHandler(svc, refs, scenarios, stateSchema, deps, nil)

	r := chi.NewRouter()
	injectClaims := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := apimiddleware.InjectClaimsForTest(req.Context(), &keeperjwt.Claims{Subject: "archon-alice"})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
	r.Route("/v1", func(r chi.Router) {
		r.Route("/services", func(r chi.Router) {
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "service", "register", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaServiceRegister(newHumaServiceAPI(r, auditW, audit.EventServiceRegistered, nil), serviceH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "service", "list", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaServiceList(newHumaCadenceAPI(r), serviceH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "service", "list", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaServiceGet(newHumaCadenceAPI(r), serviceH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "service", "update", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaServiceUpdate(newHumaServiceAPI(r, auditW, audit.EventServiceUpdated, nil), serviceH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "service", "deregister", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaServiceDeregister(newHumaServiceAPI(r, auditW, audit.EventServiceDeregistered, nil), serviceH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "service", "list", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaServiceRefs(newHumaCadenceAPI(r), serviceH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "service", "list", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaServiceScenarios(newHumaCadenceAPI(r), serviceH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "service", "list", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaServiceStateSchema(newHumaCadenceAPI(r), serviceH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "service", "list", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaServiceDependencies(newHumaCadenceAPI(r), serviceH)
			})
		})
	})
	return r
}

// svcGetRow — строка Get/List: name, git, ref, refresh(null), created_by(null),
// updated_by(null), created_at, updated_at.
func svcGetRow() []any {
	return []any{"web", "https://git/web.git", "v1.0.0", nil, nil, nil, svcAt, svcAt}
}

// === REGISTER (WRITE+AUDIT service.registered) ===

func TestHumaService_Register_GoldenWire(t *testing.T) {
	r := humaServiceRouter(t, strictAllowAll{}, nil, &hSvcPool{}, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/services",
		strings.NewReader(`{"name":"web","git":"https://git/web.git","ref":"v1.0.0"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply не JSON-object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"created_at":"2026-06-13T10:00:00Z","created_by_aid":"archon-alice","git":"https://git/web.git","name":"web","ref":"v1.0.0","updated_at":"2026-06-13T10:00:00Z","updated_by_aid":"archon-alice"}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-дрейф service.register:\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaService_Register_UnknownField_400(t *testing.T) {
	r := humaServiceRouter(t, strictAllowAll{}, nil, &hSvcPool{}, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/services",
		strings.NewReader(`{"name":"web","git":"g","ref":"v1","bogus":1}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

func TestHumaService_Register_MissingName_422(t *testing.T) {
	r := humaServiceRouter(t, strictAllowAll{}, nil, &hSvcPool{}, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/services",
		strings.NewReader(`{"git":"g","ref":"v1"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

func TestHumaService_Register_RBACDeny_403(t *testing.T) {
	r := humaServiceRouter(t, strictDenyAll{}, nil, &hSvcPool{}, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/services",
		strings.NewReader(`{"name":"web","git":"https://git/web.git","ref":"v1.0.0"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaAudit_ServiceRegister_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaServiceRouter(t, strictAllowAll{}, auditCap, &hSvcPool{}, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/services",
		strings.NewReader(`{"name":"web","git":"https://git/web.git","ref":"v1.0.0"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventServiceRegistered, map[string]any{
		"name": "web", "git": "https://git/web.git", "ref": "v1.0.0", "created_by_aid": "archon-alice",
	})
}

func TestHumaAudit_ServiceRegister_NoAudit_OnRBACDeny(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaServiceRouter(t, strictDenyAll{}, auditCap, &hSvcPool{}, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/services",
		strings.NewReader(`{"name":"web","git":"https://git/web.git","ref":"v1.0.0"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан на RBAC-deny service.register (%d событий)", len(auditCap.Events()))
	}
}

// === LIST (READ, БЕЗ audit) ===

func TestHumaService_List_GoldenWire(t *testing.T) {
	pool := &hSvcPool{listValues: [][]any{svcGetRow()}}
	r := humaServiceRouter(t, strictAllowAll{}, nil, pool, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/services", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply не JSON-object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"items":[{"created_at":"2026-06-13T10:00:00Z","git":"https://git/web.git","name":"web","ref":"v1.0.0","updated_at":"2026-06-13T10:00:00Z"}]}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-дрейф service.list:\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaService_List_GoldenEmpty(t *testing.T) {
	r := humaServiceRouter(t, strictAllowAll{}, nil, &hSvcPool{}, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/services", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	const golden = `{"items":[]}`
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	out, _ := json.Marshal(m)
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-дрейф service.list (empty): got=%q want=%q", got, golden)
	}
}

func TestHumaService_List_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaServiceRouter(t, strictAllowAll{}, auditCap, &hSvcPool{}, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/services", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("READ-роут service.list записал audit (%d событий)", len(auditCap.Events()))
	}
}

func TestHumaService_List_RBACDeny_403(t *testing.T) {
	r := humaServiceRouter(t, strictDenyAll{}, nil, &hSvcPool{}, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/services", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// === GET (READ-with-path, БЕЗ audit) ===

func TestHumaService_Get_GoldenWire(t *testing.T) {
	pool := &hSvcPool{getValues: svcGetRow()}
	r := humaServiceRouter(t, strictAllowAll{}, nil, pool, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/services/web", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply не JSON-object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"created_at":"2026-06-13T10:00:00Z","git":"https://git/web.git","name":"web","ref":"v1.0.0","updated_at":"2026-06-13T10:00:00Z"}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-дрейф service.get:\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaService_Get_NotFound_404(t *testing.T) {
	r := humaServiceRouter(t, strictAllowAll{}, nil, &hSvcPool{getMissing: true}, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/services/ghost", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeNotFound)
}

// === UPDATE (WRITE+AUDIT service.updated) ===

func TestHumaService_Update_GoldenWire(t *testing.T) {
	r := humaServiceRouter(t, strictAllowAll{}, nil, &hSvcPool{}, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/services/web",
		strings.NewReader(`{"git":"https://git/web.git","ref":"v2.0.0"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply не JSON-object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"created_at":"2026-06-13T10:00:00Z","git":"https://git/web.git","name":"web","ref":"v2.0.0","updated_at":"2026-06-13T10:00:00Z","updated_by_aid":"archon-alice"}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-дрейф service.update:\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaAudit_ServiceUpdate_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaServiceRouter(t, strictAllowAll{}, auditCap, &hSvcPool{}, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/services/web",
		strings.NewReader(`{"git":"https://git/web.git","ref":"v2.0.0"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventServiceUpdated, map[string]any{
		"name": "web", "git": "https://git/web.git", "ref": "v2.0.0",
	})
}

func TestHumaAudit_ServiceUpdate_NoAudit_OnMissingRef(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaServiceRouter(t, strictAllowAll{}, auditCap, &hSvcPool{}, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/services/web",
		strings.NewReader(`{"git":"https://git/web.git"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (missing required ref); body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан на 422 service.update (%d событий)", len(auditCap.Events()))
	}
}

// === DEREGISTER (WRITE+AUDIT service.deregistered) ===

func TestHumaService_Deregister_204(t *testing.T) {
	r := humaServiceRouter(t, strictAllowAll{}, nil, &hSvcPool{deleteRows: 1}, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/services/web", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "" {
		t.Errorf("204-тело service.deregister должно быть ПУСТЫМ, got %q", body)
	}
}

func TestHumaAudit_ServiceDeregister_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaServiceRouter(t, strictAllowAll{}, auditCap, &hSvcPool{deleteRows: 1}, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/services/web", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventServiceDeregistered, map[string]any{"name": "web"})
}

func TestHumaAudit_ServiceDeregister_NoAudit_OnNotFound(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaServiceRouter(t, strictAllowAll{}, auditCap, &hSvcPool{deleteRows: 0}, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/services/ghost", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан на 404 service.deregister (%d событий)", len(auditCap.Events()))
	}
}

// === SUB-READS (refs/scenarios/state-schema/dependencies — read-with-path, БЕЗ audit) ===

func TestHumaService_Refs_GoldenWire(t *testing.T) {
	pool := &hSvcPool{getValues: svcGetRow()}
	refs := hSvcRefsLister{refs: []artifact.GitRef{{Name: "v1.0.0", Type: "tag", Commit: "abc"}}}
	r := humaServiceRouter(t, strictAllowAll{}, nil, pool, refs, nil, nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/services/web/refs", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply не JSON-object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"refs":[{"commit":"abc","name":"v1.0.0","type":"tag"}],"service":"web"}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-дрейф service.refs:\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaService_Refs_NotConfigured_500(t *testing.T) {
	// lister=nil → 500 «not configured» (handler-контракт, до wire-up).
	r := humaServiceRouter(t, strictAllowAll{}, nil, &hSvcPool{getValues: svcGetRow()}, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/services/web/refs", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (refs lister not configured); body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaService_Scenarios_GoldenWire(t *testing.T) {
	pool := &hSvcPool{getValues: svcGetRow()}
	scenarios := hSvcScenarioLister{scenarios: []artifact.Scenario{{Name: "deploy"}}}
	r := humaServiceRouter(t, strictAllowAll{}, nil, pool, nil, scenarios, nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/services/web/scenarios", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// scenarios несут kind/runnable, размечаемые scenario-пакетом — golden фиксирует
	// присутствие service/ref/scenarios + имя scenario (kind зависит от канона имени).
	var got struct {
		Service   string `json:"service"`
		Ref       string `json:"ref"`
		Scenarios []struct {
			Name string `json:"name"`
		} `json:"scenarios"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("reply не JSON: %v; body=%s", err, rec.Body.String())
	}
	if got.Service != "web" || got.Ref != "v1.0.0" || len(got.Scenarios) != 1 || got.Scenarios[0].Name != "deploy" {
		t.Errorf("scenarios wire-дрейф: %+v", got)
	}
}

func TestHumaService_StateSchema_GoldenWire(t *testing.T) {
	pool := &hSvcPool{getValues: svcGetRow()}
	ss := hSvcStateSchemaLister{info: &artifact.StateSchemaInfo{Version: 3}}
	r := humaServiceRouter(t, strictAllowAll{}, nil, pool, nil, nil, ss, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/services/web/state-schema", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply не JSON-object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"migrations":[],"ref":"v1.0.0","service":"web","state_schema_version":3}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-дрейф service.state-schema:\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaService_Dependencies_GoldenWire(t *testing.T) {
	pool := &hSvcPool{getValues: svcGetRow()}
	deps := hSvcDepsLister{deps: &artifact.ServiceDependencies{}}
	r := humaServiceRouter(t, strictAllowAll{}, nil, pool, nil, nil, nil, deps)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/services/web/dependencies", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply не JSON-object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"destiny":[],"modules":[],"ref":"v1.0.0","service":"web"}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-дрейф service.dependencies:\n got  = %s\n want = %s", got, golden)
	}
}

// --- ?ref= query-override доезжает до lister→reply.Ref (пункт 1) ---
//
// Без этих тестов ветка `if ref == "" { ref = entry.Ref }` покрыта только
// дефолтом из реестра (svcGetRow → "v1.0.0"); что huma РЕАЛЬНО биндит ?ref= и
// override доходит до lister-а — не проверено (регресс query-binding пройдёт
// молча). Тесты ассертят И что lister получил override-ref, И что reply.Ref
// отражает его (а не дефолт реестра).

func TestHumaService_Scenarios_RefOverride_ReachesLister(t *testing.T) {
	var gotRef string
	pool := &hSvcPool{getValues: svcGetRow()} // entry.Ref = "v1.0.0" (дефолт)
	scenarios := hSvcRefCaptureScenario{gotRef: &gotRef, scenarios: []artifact.Scenario{{Name: "deploy"}}}
	r := humaServiceRouter(t, strictAllowAll{}, nil, pool, nil, scenarios, nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/services/web/scenarios?ref=v2.0.0", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if gotRef != "v2.0.0" {
		t.Errorf("override-ref НЕ доехал до lister: gotRef=%q, want \"v2.0.0\" (huma не забиндил ?ref= или override не применился)", gotRef)
	}
	var got struct {
		Ref string `json:"ref"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("reply не JSON: %v; body=%s", err, rec.Body.String())
	}
	if got.Ref != "v2.0.0" {
		t.Errorf("reply.Ref=%q, want \"v2.0.0\" (override должен отражаться в ответе, не дефолт реестра)", got.Ref)
	}
}

func TestHumaService_StateSchema_RefOverride_ReachesLister(t *testing.T) {
	var gotRef string
	pool := &hSvcPool{getValues: svcGetRow()}
	ss := hSvcRefCaptureStateSchema{gotRef: &gotRef, info: &artifact.StateSchemaInfo{Version: 3}}
	r := humaServiceRouter(t, strictAllowAll{}, nil, pool, nil, nil, ss, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/services/web/state-schema?ref=v2.0.0", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if gotRef != "v2.0.0" {
		t.Errorf("override-ref НЕ доехал до lister: gotRef=%q, want \"v2.0.0\"", gotRef)
	}
	var got struct {
		Ref string `json:"ref"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("reply не JSON: %v; body=%s", err, rec.Body.String())
	}
	if got.Ref != "v2.0.0" {
		t.Errorf("reply.Ref=%q, want \"v2.0.0\"", got.Ref)
	}
}

func TestHumaService_Dependencies_RefOverride_ReachesLister(t *testing.T) {
	var gotRef string
	pool := &hSvcPool{getValues: svcGetRow()}
	deps := hSvcRefCaptureDeps{gotRef: &gotRef, deps: &artifact.ServiceDependencies{}}
	r := humaServiceRouter(t, strictAllowAll{}, nil, pool, nil, nil, nil, deps)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/services/web/dependencies?ref=v2.0.0", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if gotRef != "v2.0.0" {
		t.Errorf("override-ref НЕ доехал до lister: gotRef=%q, want \"v2.0.0\"", gotRef)
	}
	var got struct {
		Ref string `json:"ref"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("reply не JSON: %v; body=%s", err, rec.Body.String())
	}
	if got.Ref != "v2.0.0" {
		t.Errorf("reply.Ref=%q, want \"v2.0.0\"", got.Ref)
	}
}

// Контроль: без ?ref= lister получает дефолтный ref из реестра (а override-тест
// выше доказывает, что НЕ дефолт при наличии query). Замыкает обе ветки `if ref==""`.
func TestHumaService_Scenarios_NoRef_UsesRegistryDefault(t *testing.T) {
	var gotRef string
	pool := &hSvcPool{getValues: svcGetRow()} // entry.Ref = "v1.0.0"
	scenarios := hSvcRefCaptureScenario{gotRef: &gotRef, scenarios: []artifact.Scenario{{Name: "deploy"}}}
	r := humaServiceRouter(t, strictAllowAll{}, nil, pool, nil, scenarios, nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/services/web/scenarios", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if gotRef != "v1.0.0" {
		t.Errorf("без ?ref= lister должен получить дефолт реестра: gotRef=%q, want \"v1.0.0\"", gotRef)
	}
}

// --- 502 BadGateway при ошибке git-loader-а зафиксирован golden (пункт 2) ---
//
// 502-tier достижим (handlers/service.go), объявлен в Errors huma-операций, но что
// huma РЕАЛЬНО отдаёт 502 (а не 500) при ошибке lister-а — не было проверено.
// Тесты с lister-ом, возвращающим ошибку git-источника, ассертят rec.Code==502 +
// problem.TypeBadGateway.

func TestHumaService_Scenarios_LoaderError_502(t *testing.T) {
	pool := &hSvcPool{getValues: svcGetRow()}
	r := humaServiceRouter(t, strictAllowAll{}, nil, pool, nil, hSvcErrScenario{}, nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/services/web/scenarios", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (loader git-источник недоступен); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeBadGateway)
}

func TestHumaService_Dependencies_LoaderError_502(t *testing.T) {
	pool := &hSvcPool{getValues: svcGetRow()}
	r := humaServiceRouter(t, strictAllowAll{}, nil, pool, nil, nil, nil, hSvcErrDeps{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/services/web/dependencies", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (loader git-источник недоступен); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeBadGateway)
}

func TestHumaService_SubRead_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	pool := &hSvcPool{getValues: svcGetRow()}
	refs := hSvcRefsLister{refs: []artifact.GitRef{{Name: "v1.0.0", Type: "tag", Commit: "abc"}}}
	r := humaServiceRouter(t, strictAllowAll{}, auditCap, pool, refs, nil, nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/services/web/refs", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("READ-роут service.refs записал audit (%d событий)", len(auditCap.Events()))
	}
}

// === OpenAPI-фрагмент: ВСЕ service-операции из FULL-TYPED Go-типов ===

func TestHumaService_OpenAPIFragment_3_1(t *testing.T) {
	frag, err := HumaServiceSpecYAML()
	if err != nil {
		t.Fatalf("HumaServiceSpecYAML: %v", err)
	}
	if !strings.Contains(frag, "openapi: 3.1.0") {
		t.Errorf("huma-фрагмент не несёт `openapi: 3.1.0`:\n%s", frag)
	}
	for _, want := range []string{
		"registerService", "listServices", "getService", "updateService", "deregisterService",
		"listServiceRefs", "listServiceScenarios", "listServiceStateSchema", "listServiceDependencies",
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("OpenAPI-фрагмент не содержит %q:\n%s", want, frag)
		}
	}
	if strings.Contains(frag, "octet-stream") {
		t.Errorf("OpenAPI-фрагмент несёт application/octet-stream:\n%s", frag)
	}
}
