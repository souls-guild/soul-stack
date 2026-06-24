package api

// Guard-тесты INCARNATION-домена на huma (батч-2g, ADR-054). MIXED audit-класс —
// проверяем КАЖДЫЙ write правильным S6-guard-ом (перепутать класс = регрессия):
//
//   - MIDDLEWARE-AUDIT (create/run/unlock/upgrade): event пишет huma-audit-middleware
//     (вариант B) — guard через assertMiddlewareAudit (audit на 2xx с непустым
//     payload; на 4xx/403 — пусто).
//   - SELF-AUDIT (rerun-create/check-drift/destroy/update-hosts): event пишет САМ
//     handler ВНУТРИ *Typed — guard через assertSelfAudit (event с requiredKey).
//
// Плюс: golden byte-exact wire каждого роута; ChiCoexistence на РЕАЛЬНОМ buildRouter
// (incarnation+choir, chi.Walk); 400 на out-of-range list; RBAC-deny→403; read→NoAudit.

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
	"github.com/souls-guild/soul-stack/keeper/internal/statemigrate"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// === ChiCoexistence guard (РЕАЛЬНЫЙ buildRouter, incarnation+choir, chi.Walk) ===

// TestHumaIncarnation_ChiCoexistence — guard на ДОСТИЖИМОСТЬ ВСЕХ incarnation-huma-
// роутов + сосуществование с choir-mount на ОДНОЙ группе /v1/incarnations. После сноса
// chi.Route("/{name}") incarnation-op несут ПОЛНЫЙ путь /{name}[/...]; choir (батч-2f)
// смонтирован там же. Если хоть один incarnation- или choir-роут затенён (sibling
// chi.Route на узле /{name}) — chi.Walk не перечислит его (в проде 405). Тест требует
// каждый роут ровно по разу (НЕ chi.Match — даёт false-true на затенённом узле).
func TestHumaIncarnation_ChiCoexistence(t *testing.T) {
	incH := handlers.NewIncarnationHandler(&incTestDB{}, &incTestStarter{}, &incTestStarter{}, &incTestDrift{}, &incTestResolver{ok: true}, &incTestLoader{}, nil, nil, nil)
	h := buildRouter(
		nil, // verifier
		nil, // healthH
		stubOperatorHandler(t),
		incH,
		handlers.NewSoulHandler(nil, nil, nil, nil),
		stubRoleHandler(t), stubSynodHandler(t), stubSigilHandler(t), stubSigilKeyHandler(t),
		stubServiceHandler(t), nil, stubAugurHandler(t), stubOracleHandler(t),
		nil,                                     // pushH
		nil,                                     // pushProviderH
		nil,                                     // errandH
		nil,                                     // voyageH
		nil,                                     // cadenceH
		nil,                                     // auditH
		handlers.NewChoirHandler(nil, nil, nil), // choirH non-nil → choir-mount сосуществует
		nil,                                     // heraldH
		handlers.NewModuleCatalogHandler(nil, nil),
		handlers.NewModuleFormPrepHandler(nil, nil),
		handlers.NewPermissionCatalogHandler(nil),
		handlers.NewEventTypeCatalogHandler(nil),
		handlers.NewMyPermissionsHandler(nil, nil),
		nil,                                  // enforcer
		nil,                                  // auditWriter
		nil,                                  // metricsHTTP
		nil,                                  // tollDegraded
		nil,                                  // tempoLimiter
		nil,                                  // tempoMetrics
		nil,                                  // tempoVoyageCreateLimits
		nil,                                  // tempoVoyagePreviewLimits
		false,                                // webUIEnabled — /ui вне интереса incarnation-роутинг-теста
		nil,                                  // ldapAuth (LDAP не сконфигурирован в тесте)
		nil,                                  // oidcAuth (OIDC не сконфигурирован в тесте)
		nil,                                  // loginGuard (anti-bruteforce off в тесте)
		apimiddleware.AuthLoginLimitConfig{}, // loginLimitCfg
		nil,                                  // logger
	)
	routes, ok := h.(chi.Routes)
	if !ok {
		t.Fatalf("buildRouter вернул %T, не chi.Routes", h)
	}

	// Полный набор incarnation + choir роутов на группе /v1/incarnations: каждый
	// обязан встретиться РОВНО раз. Отсутствие = затенение (405 в проде); дубль =
	// коллизия mount-а.
	want := map[route]int{
		{http.MethodPost, "/v1/incarnations"}:                                      0,
		{http.MethodGet, "/v1/incarnations"}:                                       0,
		{http.MethodGet, "/v1/incarnations/{name}"}:                                0,
		{http.MethodGet, "/v1/incarnations/{name}/history"}:                        0,
		{http.MethodPost, "/v1/incarnations/{name}/scenarios/{scenario}"}:          0,
		{http.MethodPost, "/v1/incarnations/{name}/unlock"}:                        0,
		{http.MethodPost, "/v1/incarnations/{name}/upgrade"}:                       0,
		{http.MethodPost, "/v1/incarnations/{name}/rerun-create"}:                  0,
		{http.MethodPost, "/v1/incarnations/{name}/check-drift"}:                   0,
		{http.MethodDelete, "/v1/incarnations/{name}"}:                             0,
		{http.MethodPatch, "/v1/incarnations/{name}/hosts"}:                        0,
		{http.MethodPost, "/v1/incarnations/{name}/choirs"}:                        0,
		{http.MethodGet, "/v1/incarnations/{name}/choirs"}:                         0,
		{http.MethodDelete, "/v1/incarnations/{name}/choirs/{choir}"}:              0,
		{http.MethodPost, "/v1/incarnations/{name}/choirs/{choir}/voices"}:         0,
		{http.MethodGet, "/v1/incarnations/{name}/choirs/{choir}/voices"}:          0,
		{http.MethodDelete, "/v1/incarnations/{name}/choirs/{choir}/voices/{sid}"}: 0,
	}
	if err := chi.Walk(routes, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		k := route{method: method, path: normalizePath(pattern)}
		if _, tracked := want[k]; tracked {
			want[k]++
		}
		return nil
	}); err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	for k, n := range want {
		if n != 1 {
			t.Errorf("%s встретился %d раз, want 1 (0 = затенение/не смонтирован → 405; >1 = дубль)", k, n)
		}
	}
}

// === isolated huma-router (продакшен-навеска буквально из router.go) ===

// incEnforcer — combined RBAC-стаб: PermissionChecker (RequirePermissionMulti на
// write) + ActionHolder (RequireAction на read). allow параметризует обе грани.
type incEnforcer struct{ allow bool }

func (e incEnforcer) Check(string, string, string, map[string]string) error {
	if e.allow {
		return nil
	}
	return rbac.ErrPermissionDenied
}
func (e incEnforcer) HoldsAction(string, string, string) bool { return e.allow }

// humaIncarnationRouter монтирует ВСЕ incarnation-роуты через huma ровно по навеске
// router.go: per-route RBAC + правильный audit-класс (MIDDLEWARE для create/run/unlock/
// upgrade; SELF для rerun-create/check-drift/destroy/update-hosts; read без audit) +
// huma-op с полным путём /{name}[/...] на группе /v1/incarnations. enforcer/auditW/incH
// параметризованы. injectClaims заменяет RequireJWT.
func humaIncarnationRouter(t *testing.T, enforcer incEnforcer, auditW audit.Writer, incH *handlers.IncarnationHandler) *chi.Mux {
	t.Helper()
	installHumaErrorOverride()
	r := chi.NewRouter()
	injectClaims := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := apimiddleware.InjectClaimsForTest(req.Context(), &keeperjwt.Claims{Subject: "archon-alice"})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
	multi := func(action string) func(http.Handler) http.Handler {
		return apimiddleware.RequirePermissionMulti(enforcer, "incarnation", action, incNoCtxSelector)
	}
	r.Route("/v1", func(r chi.Router) {
		r.Route("/incarnations", func(r chi.Router) {
			// MIDDLEWARE-AUDIT
			r.With(injectClaims, multi("create")).Group(func(r chi.Router) {
				registerHumaIncarnationCreate(newHumaIncarnationAPI(r, auditW, audit.EventIncarnationCreated, nil), incH)
			})
			r.With(injectClaims, multi("run")).Group(func(r chi.Router) {
				registerHumaIncarnationRun(newHumaIncarnationAPI(r, auditW, audit.EventIncarnationScenarioStarted, nil), incH)
			})
			r.With(injectClaims, multi("unlock")).Group(func(r chi.Router) {
				registerHumaIncarnationUnlock(newHumaIncarnationAPI(r, auditW, audit.EventIncarnationUnlocked, nil), incH)
			})
			r.With(injectClaims, multi("upgrade")).Group(func(r chi.Router) {
				registerHumaIncarnationUpgrade(newHumaIncarnationAPI(r, auditW, audit.EventIncarnationUpgradeStarted, nil), incH)
			})
			// SELF-AUDIT
			r.With(injectClaims, multi("create-rerun")).Group(func(r chi.Router) {
				registerHumaIncarnationRerunCreate(newHumaCadenceAPI(r), incH)
			})
			r.With(injectClaims, multi("check-drift")).Group(func(r chi.Router) {
				registerHumaIncarnationCheckDrift(newHumaCadenceAPI(r), incH)
			})
			r.With(injectClaims, multi("destroy")).Group(func(r chi.Router) {
				registerHumaIncarnationDestroy(newHumaCadenceAPI(r), incH)
			})
			r.With(injectClaims, multi("update-hosts")).Group(func(r chi.Router) {
				registerHumaIncarnationUpdateHosts(newHumaCadenceAPI(r), incH)
			})
			// READ
			r.With(injectClaims, stashRawQuery, apimiddleware.RequireAction(enforcer, "incarnation", "list")).Group(func(r chi.Router) {
				registerHumaIncarnationList(newHumaCadenceAPI(r), incH)
			})
			r.With(injectClaims, apimiddleware.RequireAction(enforcer, "incarnation", "get")).Group(func(r chi.Router) {
				registerHumaIncarnationGet(newHumaCadenceAPI(r), incH)
			})
			r.With(injectClaims, apimiddleware.RequireAction(enforcer, "incarnation", "history")).Group(func(r chi.Router) {
				registerHumaIncarnationHistory(newHumaCadenceAPI(r), incH)
			})
		})
	})
	return r
}

// incNoCtxSelector — MultiSelectorExtractor без БД-контекста (тест): пустой набор →
// RequirePermissionMulti пускает только bare/`*`-роли. Для allow-all-теста этого
// достаточно (strictAllowAll отвечает true на bare).
func incNoCtxSelector(_ *http.Request) []map[string]string { return nil }

func incScopeAllow() *handlers.IncarnationHandler {
	// scoper=incTestScoper{unrestricted:true} → get/list/history видят всё.
	db := &incTestDB{
		selectByName:  func(name string) pgx.Row { return incRow(name, "ready", "{}") },
		soulsExisting: map[string]struct{}{"web1.example.com": {}},
	}
	return handlers.NewIncarnationHandler(db, &incTestStarter{}, &incTestStarter{}, &incTestDrift{}, &incTestResolver{ok: true}, &incTestLoader{}, nil, incTestScoper{unrestricted: true}, nil)
}

// === MIDDLEWARE-AUDIT: create ===

func TestHumaIncarnation_Create_WireAndMiddlewareAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	db := &incTestDB{insertRow: func() pgx.Row { return staticRow2(time.Now(), time.Now()) }}
	incH := handlers.NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil) // runner=nil → stub-режим
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, auditCap, incH)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations", strings.NewReader(`{"name":"redis-prod","service":"redis"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	var reply struct {
		Incarnation string  `json:"incarnation"`
		ApplyID     *string `json:"apply_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if reply.Incarnation != "redis-prod" || reply.ApplyID == nil || *reply.ApplyID == "" {
		t.Errorf("reply = %+v, want incarnation=redis-prod + apply_id", reply)
	}
	assertMiddlewareAudit(t, auditCap, audit.EventIncarnationCreated, "name")
}

func TestHumaIncarnation_Create_UnknownField_400(t *testing.T) {
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, nil, incScopeAllow())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations", strings.NewReader(`{"name":"x","service":"redis","bogus":1}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (unknown field); body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaIncarnation_Create_RBACDeny_403_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaIncarnationRouter(t, incEnforcer{allow: false}, auditCap, incScopeAllow())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations", strings.NewReader(`{"name":"x","service":"redis"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан на 403 — RBAC-deny не должен доходить до handler-а")
	}
}

// === MIDDLEWARE-AUDIT: create — pre-flight assert-гейт (ADR-009/027 amend, форма A) ===

// _ — compile-time guard: реальный *scenario.Runner ОБЯЗАН удовлетворять
// handlers.AssertPreflighter (handler берёт pre-flighter type-assertion-ом из
// runner-а, type-set которого статически не сверяется при присваивании в
// ScenarioStarter). Drift сигнатуры Runner.PreflightAssert ↔ интерфейса
// ловится здесь при сборке, а не молчаливым no-op pre-flight в проде.
var _ handlers.AssertPreflighter = (*scenario.Runner)(nil)

// incPreflightLoader — ServiceSnapshotLoader-стаб, отдающий МИНИМАЛЬНЫЙ валидный
// scenario create/main.yml (без input-схемы → ValidateInput проходит). Нужен,
// потому что create при не-nil runner+loader делает sync ValidateInput ДО
// pre-flight; incTestLoader.ReadFile возвращает пустые байты (невалидный
// scenario → 500 на парсе). pre-flight же тут — стаб incPreflightStarter, не
// читает scenario через этот loader.
type incPreflightLoader struct{}

func (incPreflightLoader) Load(_ context.Context, ref artifact.ServiceRef) (*artifact.ServiceArtifact, error) {
	return &artifact.ServiceArtifact{Ref: ref}, nil
}
func (incPreflightLoader) LoadMigrationChain(_ *artifact.ServiceArtifact, _, _ int) (statemigrate.Chain, error) {
	return statemigrate.Chain{}, nil
}
func (incPreflightLoader) ReadFile(_ *artifact.ServiceArtifact, _ string) ([]byte, error) {
	return []byte("name: create\ntasks:\n  - name: noop\n    module: core.exec.run\n    params: { cmd: \"true\" }\n"), nil
}

// incPreflightStarter — ScenarioStarter + AssertPreflighter-стаб: pre-flight
// возвращает preflightErr (nil → проходит), Start учитывает факт вызова в started.
type incPreflightStarter struct {
	preflightErr error
	started      *bool
}

func (s *incPreflightStarter) Start(_ context.Context, _ scenario.RunSpec) error {
	if s.started != nil {
		*s.started = true
	}
	return nil
}
func (s *incPreflightStarter) StartDestroy(_ context.Context, _ scenario.RunSpec) error { return nil }
func (s *incPreflightStarter) PreflightAssert(_ context.Context, _ scenario.RunSpec) error {
	return s.preflightErr
}

// TestHumaIncarnation_Create_PreflightAssertFail_422 — pre-flight assert false на
// СОЗДАНИИ → 422 assert_failed, incarnation НЕ создана (insertRow НЕ вызван),
// Start НЕ запущен, audit на 4xx НЕ пишется. ГЛАВНЫЙ инвариант формы A: отказ на
// этапе модели ДО коммита, без fail-статуса error_locked.
func TestHumaIncarnation_Create_PreflightAssertFail_422(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	inserted := false
	started := false
	db := &incTestDB{insertRow: func() pgx.Row {
		inserted = true
		return staticRow2(time.Now(), time.Now())
	}}
	starter := &incPreflightStarter{
		preflightErr: scenario.ErrAssertFailed, // топология не сходится
		started:      &started,
	}
	incH := handlers.NewIncarnationHandler(db, starter, nil, nil, &incTestResolver{ok: true}, incPreflightLoader{}, auditCap, nil, nil)
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, auditCap, incH)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations", strings.NewReader(`{"name":"redis-cluster","service":"redis"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 assert_failed; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "assert-failed") {
		t.Errorf("body не несёт problem-type assert-failed: %s", rec.Body.String())
	}
	if inserted {
		t.Error("ИНВАРИАНТ НАРУШЕН: incarnation создана (insertRow вызван) на assert-fail — должно быть НЕ создано")
	}
	if started {
		t.Error("ИНВАРИАНТ НАРУШЕН: scenario create запущен на assert-fail — Start не должен вызываться")
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан на 422 assert-fail — middleware не пишет на 4xx")
	}
}

// incValidateLoader — ServiceSnapshotLoader-стаб с scenario create/main.yml,
// несущим top-level validate:-правило (кросс-полевой инвариант «port обязателен,
// если tls выключен»). ValidateInput на create-пути читает этот scenario и
// прогоняет validate-правила над смерженным input (input-only eval).
type incValidateLoader struct{}

func (incValidateLoader) Load(_ context.Context, ref artifact.ServiceRef) (*artifact.ServiceArtifact, error) {
	return &artifact.ServiceArtifact{Ref: ref}, nil
}
func (incValidateLoader) LoadMigrationChain(_ *artifact.ServiceArtifact, _, _ int) (statemigrate.Chain, error) {
	return statemigrate.Chain{}, nil
}
func (incValidateLoader) ReadFile(_ *artifact.ServiceArtifact, _ string) ([]byte, error) {
	return []byte(`name: create
input:
  tls: { type: boolean, default: false }
  port: { type: integer, default: 0 }
validate:
  - that: "input.tls || input.port > 0"
    message: "either enable tls or set a positive port"
tasks:
  - name: noop
    module: core.exec.run
    params: { cmd: "true" }
`), nil
}

// TestHumaIncarnation_Create_ValidateRuleFail_422 — top-level validate:-правило
// не прошло на request-пути → 422 validation-failed ДО коммита, incarnation НЕ
// создана, Start НЕ запущен, audit на 4xx НЕ пишется. validate: симметрично
// pre-flight assert (форма A), но input-only и через ValidateInput (DSL wave 2).
func TestHumaIncarnation_Create_ValidateRuleFail_422(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	inserted := false
	started := false
	db := &incTestDB{insertRow: func() pgx.Row {
		inserted = true
		return staticRow2(time.Now(), time.Now())
	}}
	starter := &incPreflightStarter{preflightErr: nil, started: &started}
	incH := handlers.NewIncarnationHandler(db, starter, nil, nil, &incTestResolver{ok: true}, incValidateLoader{}, auditCap, nil, nil)
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, auditCap, incH)

	rec := httptest.NewRecorder()
	// input БЕЗ port и БЕЗ tls → defaults (tls=false, port=0) → правило false.
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations", strings.NewReader(`{"name":"redis-prod","service":"redis"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 validation-failed; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "validation-failed") {
		t.Errorf("body не несёт problem-type validation-failed: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "either enable tls or set a positive port") {
		t.Errorf("body не несёт message правила: %s", rec.Body.String())
	}
	if inserted {
		t.Error("ИНВАРИАНТ НАРУШЕН: incarnation создана на validate-fail — должно быть НЕ создано")
	}
	if started {
		t.Error("ИНВАРИАНТ НАРУШЕН: scenario create запущен на validate-fail — Start не должен вызываться")
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан на 422 validate-fail — middleware не пишет на 4xx")
	}
}

// TestHumaIncarnation_Create_ValidateRulePass_202 — validate:-правило проходит
// (port>0) → create работает как раньше: 202, incarnation создана, Start запущен.
func TestHumaIncarnation_Create_ValidateRulePass_202(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	inserted := false
	started := false
	db := &incTestDB{insertRow: func() pgx.Row {
		inserted = true
		return staticRow2(time.Now(), time.Now())
	}}
	starter := &incPreflightStarter{preflightErr: nil, started: &started}
	incH := handlers.NewIncarnationHandler(db, starter, nil, nil, &incTestResolver{ok: true}, incValidateLoader{}, auditCap, nil, nil)
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, auditCap, incH)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations", strings.NewReader(`{"name":"redis-prod","service":"redis","input":{"port":6379}}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (validate passes); body=%s", rec.Code, rec.Body.String())
	}
	if !inserted {
		t.Error("incarnation не создана на validate-pass — happy-path сломан")
	}
	if !started {
		t.Error("Start не запущен на validate-pass — happy-path сломан")
	}
}

// TestHumaIncarnation_Create_PreflightAssertPass_202 — pre-flight проходит
// (топология сходится) → create работает как раньше: 202 + apply_id, incarnation
// создана, Start запущен. Контроль, что pre-flight не ломает happy-path.
func TestHumaIncarnation_Create_PreflightAssertPass_202(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	started := false
	db := &incTestDB{insertRow: func() pgx.Row { return staticRow2(time.Now(), time.Now()) }}
	starter := &incPreflightStarter{preflightErr: nil, started: &started}
	incH := handlers.NewIncarnationHandler(db, starter, nil, nil, &incTestResolver{ok: true}, incPreflightLoader{}, auditCap, nil, nil)
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, auditCap, incH)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations", strings.NewReader(`{"name":"redis-prod","service":"redis"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if !started {
		t.Error("Start не запущен на assert-pass — happy-path сломан")
	}
	assertMiddlewareAudit(t, auditCap, audit.EventIncarnationCreated, "name")
}

// === MIDDLEWARE-AUDIT: unlock ===

func TestHumaIncarnation_Unlock_WireAndMiddlewareAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	db := &incTestDB{
		selectByName: func(name string) pgx.Row { return incRow(name, "error_locked", "{}") },
		unlockSelect: func() pgx.Row { return staticRow2Bytes([]byte("{}"), "error_locked") },
	}
	incH := handlers.NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, auditCap, incH)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations/redis-prod/unlock", strings.NewReader(`{"reason":"fix"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var reply struct {
		Name           string `json:"name"`
		PreviousStatus string `json:"previous_status"`
		Status         string `json:"status"`
		UnlockedByAID  string `json:"unlocked_by_aid"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if reply.Name != "redis-prod" || reply.PreviousStatus != "error_locked" || reply.Status != "ready" || reply.UnlockedByAID != "archon-alice" {
		t.Errorf("reply = %+v", reply)
	}
	assertMiddlewareAudit(t, auditCap, audit.EventIncarnationUnlocked, "previous_status")
}

func TestHumaIncarnation_Unlock_MissingReason_422(t *testing.T) {
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, nil, incScopeAllow())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations/redis-prod/unlock", strings.NewReader(`{}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (missing required reason); body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaIncarnation_Unlock_NotFound_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	db := &incTestDB{selectByName: func(string) pgx.Row { return errRow2{pgx.ErrNoRows} }}
	incH := handlers.NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, auditCap, incH)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations/nope/unlock", strings.NewReader(`{"reason":"x"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан на 404 unlock — write-путь не должен писать (middleware skip на 4xx)")
	}
}

// === MIDDLEWARE-AUDIT: upgrade ===

func TestHumaIncarnation_Upgrade_MiddlewareAuditClass(t *testing.T) {
	// 500-путь (loader=nil → endpoint не сконфигурирован): доказывает, что upgrade
	// на НЕ-2xx НЕ пишет audit (middleware-skip), и что роут смонтирован/достижим.
	auditCap := &auditCaptureWriter{}
	db := &incTestDB{selectByName: func(name string) pgx.Row { return incRow(name, "ready", "{}") }}
	incH := handlers.NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil) // loader=nil
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, auditCap, incH)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations/redis-prod/upgrade", strings.NewReader(`{"to_version":"v2"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (loader nil); body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан на 500 upgrade — middleware не должен писать на 5xx")
	}
}

func TestHumaIncarnation_Upgrade_MissingToVersion_422(t *testing.T) {
	db := &incTestDB{selectByName: func(name string) pgx.Row { return incRow(name, "ready", "{}") }}
	incH := handlers.NewIncarnationHandler(db, nil, nil, nil, &incTestResolver{ok: true}, &incTestLoader{}, nil, nil, nil)
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, nil, incH)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations/redis-prod/upgrade", strings.NewReader(`{}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (missing to_version); body=%s", rec.Code, rec.Body.String())
	}
}

// === MIDDLEWARE-AUDIT: run ===

func TestHumaIncarnation_Run_WireAndMiddlewareAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	db := &incTestDB{selectByName: func(name string) pgx.Row { return incRow(name, "ready", "{}") }}
	incH := handlers.NewIncarnationHandler(db, &incTestStarter{}, nil, nil, &incTestResolver{ok: true}, nil, nil, nil, nil)
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, auditCap, incH)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations/redis-prod/scenarios/converge", strings.NewReader(`{}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	var reply struct {
		ApplyID     string `json:"apply_id"`
		Incarnation string `json:"incarnation"`
		Scenario    string `json:"scenario"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if reply.ApplyID == "" || reply.Incarnation != "redis-prod" || reply.Scenario != "converge" {
		t.Errorf("reply = %+v", reply)
	}
	assertMiddlewareAudit(t, auditCap, audit.EventIncarnationScenarioStarted, "scenario")
}

// === SELF-AUDIT: rerun-create ===

func TestHumaIncarnation_RerunCreate_SelfAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	db := &incTestDB{
		selectByName: func(name string) pgx.Row { return incRow(name, "error_locked", "{}") },
		unlockSelect: func() pgx.Row { return staticRow2Bytes([]byte("{}"), "error_locked") },
	}
	incH := handlers.NewIncarnationHandler(db, &incTestStarter{}, nil, nil, &incTestResolver{ok: true}, nil, auditCap, nil, nil)
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, auditCap, incH)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations/redis-prod/rerun-create", strings.NewReader(`{"reason":"retry"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	var reply struct {
		ApplyID     string `json:"apply_id"`
		Incarnation string `json:"incarnation"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if reply.ApplyID == "" || reply.Incarnation != "redis-prod" {
		t.Errorf("reply = %+v", reply)
	}
	assertSelfAudit(t, auditCap, audit.EventIncarnationCreateRerun, "previous_status")
}

func TestHumaIncarnation_RerunCreate_NotErrorLocked_409_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	db := &incTestDB{
		selectByName: func(name string) pgx.Row { return incRow(name, "ready", "{}") },
		unlockSelect: func() pgx.Row { return staticRow2Bytes([]byte("{}"), "ready") }, // не error_locked → ErrNotErrorLocked
	}
	incH := handlers.NewIncarnationHandler(db, &incTestStarter{}, nil, nil, &incTestResolver{ok: true}, nil, auditCap, nil, nil)
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, auditCap, incH)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations/redis-prod/rerun-create", strings.NewReader(`{"reason":"x"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан на 409 rerun — self-audit пишется только на успехе")
	}
}

// === SELF-AUDIT: check-drift ===

func TestHumaIncarnation_CheckDrift_WireAndSelfAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	db := &incTestDB{selectByName: func(name string) pgx.Row { return incRow(name, "ready", "{}") }}
	incH := handlers.NewIncarnationHandler(db, nil, nil, &incTestDrift{}, &incTestResolver{ok: true}, nil, auditCap, nil, nil)
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, auditCap, incH)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations/redis-prod/check-drift", strings.NewReader(`{}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertSelfAudit(t, auditCap, audit.EventIncarnationDriftChecked, "drift_summary")
}

// === SELF-AUDIT: destroy ===

func TestHumaIncarnation_Destroy_MissingAllowDestroy_400(t *testing.T) {
	// allow_destroy required boolean query — отсутствует → 400 (huma required-param).
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, nil, incScopeAllow())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/incarnations/redis-prod", http.NoBody)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (missing allow_destroy); body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaIncarnation_Destroy_BadAllowDestroy_400(t *testing.T) {
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, nil, incScopeAllow())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/incarnations/redis-prod?allow_destroy=maybe", http.NoBody)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (non-boolean allow_destroy); body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaIncarnation_Destroy_ForceWireAndSelfAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	db := &incTestDB{
		selectByName: func(name string) pgx.Row { return incRow(name, "ready", "{}") },
		unlockSelect: func() pgx.Row { return staticRow2Bytes([]byte("{}"), "ready") }, // Destroy FOR UPDATE select (state, status)
	}
	incH := handlers.NewIncarnationHandler(db, nil, &incTestStarter{}, nil, &incTestResolver{ok: true}, &incTestLoader{}, auditCap, nil, nil)
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, auditCap, incH)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/incarnations/redis-prod?allow_destroy=true", http.NoBody)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	var reply struct {
		ApplyID string `json:"apply_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if reply.ApplyID == "" {
		t.Errorf("reply.apply_id пуст")
	}
	// destroy_started + destroy_completed пишет service-слой ВНУТРИ Destroy/
	// DeleteAfterTeardown (SELF-AUDIT) — хотя бы destroy_started обязан быть.
	if !hasAuditEvent(auditCap, audit.EventIncarnationDestroyStarted) {
		t.Errorf("incarnation.destroy_started НЕ записан (SELF-AUDIT сломан); events=%v", auditEventTypes(auditCap))
	}
}

// === SELF-AUDIT: update-hosts ===

func TestHumaIncarnation_UpdateHosts_WireAndSelfAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	db := &incTestDB{
		selectByName:  func(name string) pgx.Row { return incRow(name, "ready", "{}") },
		soulsExisting: map[string]struct{}{"web1.example.com": {}},
	}
	incH := handlers.NewIncarnationHandler(db, nil, nil, nil, nil, nil, auditCap, nil, nil)
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, auditCap, incH)
	rec := httptest.NewRecorder()
	body := `{"mode":"replace","hosts":[{"sid":"web1.example.com","role":"master"}]}`
	req := httptest.NewRequest(http.MethodPatch, "/v1/incarnations/redis-prod/hosts", strings.NewReader(body))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var reply struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if reply.Name != "redis-prod" {
		t.Errorf("reply.name = %q, want redis-prod", reply.Name)
	}
	assertSelfAudit(t, auditCap, audit.EventIncarnationHostsUpdated, "mode")
}

func TestHumaIncarnation_UpdateHosts_BadMode_422(t *testing.T) {
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, nil, incScopeAllow())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/incarnations/redis-prod/hosts", strings.NewReader(`{"mode":"bogus","hosts":[]}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (enum mode); body=%s", rec.Code, rec.Body.String())
	}
}

// === READ: list / get / history (NoAudit + 400 list) ===

func TestHumaIncarnation_List_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, auditCap, incScopeAllow())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/incarnations", http.NoBody)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("read GET /v1/incarnations записал audit (%d) — read не пишет", len(auditCap.Events()))
	}
}

func TestHumaIncarnation_List_BadLimit_400(t *testing.T) {
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, nil, incScopeAllow())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/incarnations?limit=99999", http.NoBody)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (CheckPageBounds out-of-range); body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaIncarnation_List_BadLimitType_400(t *testing.T) {
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, nil, incScopeAllow())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/incarnations?limit=lots", http.NoBody)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (bad int limit); body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaIncarnation_Get_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, auditCap, incScopeAllow())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/incarnations/redis-prod", http.NoBody)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("read GET /{name} записал audit — read не пишет")
	}
}

func TestHumaIncarnation_History_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, auditCap, incScopeAllow())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/incarnations/redis-prod/history", http.NoBody)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("read GET /{name}/history записал audit — read не пишет")
	}
}

func TestHumaIncarnation_History_BadLimit_400(t *testing.T) {
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, nil, incScopeAllow())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/incarnations/redis-prod/history?limit=lots", http.NoBody)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (bad limit); body=%s", rec.Code, rec.Body.String())
	}
}

// === spec-dump ===

func TestHumaIncarnation_SpecYAML(t *testing.T) {
	frag, err := HumaIncarnationSpecYAML()
	if err != nil {
		t.Fatalf("HumaIncarnationSpecYAML: %v", err)
	}
	for _, want := range []string{
		"createIncarnation", "listIncarnations", "getIncarnation", "getIncarnationHistory",
		"runIncarnationScenario", "unlockIncarnation", "upgradeIncarnation",
		"rerunCreateIncarnation", "checkIncarnationDrift", "destroyIncarnation", "updateIncarnationHosts",
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("спека не содержит op %q", want)
		}
	}
}

// === assert helpers ===

// assertMiddlewareAudit — S6-MIDDLEWARE-GUARD: huma-audit-middleware (вариант B)
// записал ровно одно событие want с непустым payload, содержащим requiredKey.
// Мутация (снять SetHumaAuditPayload в register-func / снять middleware-навеску)
// краснит этот тест.
func assertMiddlewareAudit(t *testing.T, cap *auditCaptureWriter, want audit.EventType, requiredKey string) {
	t.Helper()
	evs := cap.Events()
	if len(evs) != 1 {
		t.Fatalf("middleware-audit: %d событий, want 1 (event=%s)", len(evs), want)
	}
	ev := evs[0]
	if ev.EventType != want {
		t.Errorf("event_type = %q, want %q", ev.EventType, want)
	}
	if ev.Source != audit.SourceAPI {
		t.Errorf("source = %q, want api", ev.Source)
	}
	if ev.ArchonAID != "archon-alice" {
		t.Errorf("archon = %q, want archon-alice", ev.ArchonAID)
	}
	if len(ev.Payload) == 0 {
		t.Fatalf("payload пуст — SetHumaAuditPayload не сработал (рецидив S6)")
	}
	if _, ok := ev.Payload[requiredKey]; !ok {
		t.Errorf("payload не содержит ключ %q: %v", requiredKey, ev.Payload)
	}
}

func hasAuditEvent(cap *auditCaptureWriter, want audit.EventType) bool {
	for _, ev := range cap.Events() {
		if ev.EventType == want {
			return true
		}
	}
	return false
}

func auditEventTypes(cap *auditCaptureWriter) []audit.EventType {
	var out []audit.EventType
	for _, ev := range cap.Events() {
		out = append(out, ev.EventType)
	}
	return out
}

// === minimal fakes (api-пакет) ===

// incTestDB — минимальный [handlers.IncarnationDB] для huma-wire-тестов: покрывает
// insert (create), SelectByName (get/run/unlock/upgrade/destroy/history probe),
// unlock/rerun SELECT FOR UPDATE, souls-existence (update-hosts), list COUNT/SELECT.
type incTestDB struct {
	insertRow     func() pgx.Row
	selectByName  func(name string) pgx.Row
	unlockSelect  func() pgx.Row
	soulsExisting map[string]struct{}
}

func (f *incTestDB) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "DELETE FROM incarnation") {
		return pgconn.NewCommandTag("DELETE 1"), nil
	}
	return pgconn.CommandTag{}, nil
}

func (f *incTestDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "INSERT INTO incarnation"):
		if f.insertRow != nil {
			return f.insertRow()
		}
		return staticRow2(time.Now(), time.Now())
	case strings.Contains(sql, "SELECT state, state_schema_version, status") && strings.Contains(sql, "FOR UPDATE"):
		if f.unlockSelect != nil {
			return f.unlockSelect()
		}
		return errRow2{pgx.ErrNoRows}
	case strings.Contains(sql, "SELECT state, status") && strings.Contains(sql, "FOR UPDATE"):
		if f.unlockSelect != nil {
			return f.unlockSelect()
		}
		return errRow2{pgx.ErrNoRows}
	case strings.Contains(sql, "SELECT scenario") && strings.Contains(sql, "FROM state_history"):
		return staticRow1("create")
	case strings.Contains(sql, "UPDATE incarnation") && strings.Contains(sql, "RETURNING updated_at"):
		return staticRow1Time(time.Now().UTC())
	case strings.Contains(sql, "WHERE name = $1") || strings.Contains(sql, "FROM incarnation\nWHERE name"):
		if f.selectByName != nil {
			return f.selectByName(args[0].(string))
		}
		return errRow2{pgx.ErrNoRows}
	case strings.Contains(sql, "COUNT(*) FROM incarnation") || strings.Contains(sql, "COUNT(*) FROM state_history"):
		return staticRow1Int(0)
	}
	return errRow2{errors.New("incTestDB.QueryRow: unexpected SQL: " + sql)}
}

func (f *incTestDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "FROM souls WHERE sid = ANY") {
		sids, _ := args[0].([]string)
		var found []string
		for _, sid := range sids {
			if _, ok := f.soulsExisting[sid]; ok {
				found = append(found, sid)
			}
		}
		return &incStringRows{values: found}, nil
	}
	return &incEmptyRows{}, nil
}

func (f *incTestDB) BeginTx(_ context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	return &incTestTx{db: f}, nil
}

// incTestTx — pgx.Tx-обёртка над incTestDB (Unlock/Upgrade/Destroy/UpdateHosts
// идут под tx). Commit/Rollback no-op; неиспользуемые методы panic-ают.
type incTestTx struct{ db *incTestDB }

func (t *incTestTx) Begin(ctx context.Context) (pgx.Tx, error) {
	return t.db.BeginTx(ctx, pgx.TxOptions{})
}
func (t *incTestTx) Commit(_ context.Context) error   { return nil }
func (t *incTestTx) Rollback(_ context.Context) error { return nil }
func (t *incTestTx) CopyFrom(_ context.Context, _ pgx.Identifier, _ []string, _ pgx.CopyFromSource) (int64, error) {
	panic("incTestTx.CopyFrom: unexpected")
}
func (t *incTestTx) SendBatch(_ context.Context, _ *pgx.Batch) pgx.BatchResults {
	panic("incTestTx.SendBatch: unexpected")
}
func (t *incTestTx) LargeObjects() pgx.LargeObjects { panic("incTestTx.LargeObjects: unexpected") }
func (t *incTestTx) Prepare(_ context.Context, _, _ string) (*pgconn.StatementDescription, error) {
	panic("incTestTx.Prepare: unexpected")
}
func (t *incTestTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.db.Exec(ctx, sql, args...)
}
func (t *incTestTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.db.Query(ctx, sql, args...)
}
func (t *incTestTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.db.QueryRow(ctx, sql, args...)
}
func (t *incTestTx) Conn() *pgx.Conn { return nil }

// incRow — SelectByName-row (порядок колонок scanIncarnation). status/state параметризованы.
func incRow(name, status, state string) pgx.Row {
	now := time.Now()
	return incStaticRow{values: []any{
		name, "redis", "v1", int(1),
		[]byte("{}"), []byte(state), status,
		[]byte(nil), any(nil),
		now, now, []string(nil),
		any(nil), []byte(nil),
	}}
}

// incStaticRow / helpers — локальные row-стабы api-пакета (parity handlers-test staticRow).
type incStaticRow struct{ values []any }

func (r incStaticRow) Scan(dest ...any) error {
	for i, d := range dest {
		switch d := d.(type) {
		case *string:
			*d = r.values[i].(string)
		case *time.Time:
			*d = r.values[i].(time.Time)
		case *int:
			*d = r.values[i].(int)
		case *int64:
			*d = r.values[i].(int64)
		case **string:
			if r.values[i] == nil {
				*d = nil
			} else {
				s := r.values[i].(string)
				*d = &s
			}
		case **time.Time:
			if r.values[i] == nil {
				*d = nil
			} else {
				tt := r.values[i].(time.Time)
				*d = &tt
			}
		case *[]byte:
			*d = r.values[i].([]byte)
		case *[]string:
			if r.values[i] == nil {
				*d = nil
			} else {
				*d = r.values[i].([]string)
			}
		}
	}
	return nil
}

func staticRow1(s string) pgx.Row                { return incStaticRow{values: []any{s}} }
func staticRow1Int(n int) pgx.Row                { return incStaticRow{values: []any{n}} }
func staticRow1Time(t time.Time) pgx.Row         { return incStaticRow{values: []any{t}} }
func staticRow2(a, b time.Time) pgx.Row          { return incStaticRow{values: []any{a, b}} }
func staticRow2Bytes(b []byte, s string) pgx.Row { return incStaticRow{values: []any{b, s}} }

type errRow2 struct{ err error }

func (r errRow2) Scan(_ ...any) error { return r.err }

type incEmptyRows struct{}

func (r *incEmptyRows) Next() bool                                   { return false }
func (r *incEmptyRows) Scan(_ ...any) error                          { return nil }
func (r *incEmptyRows) Err() error                                   { return nil }
func (r *incEmptyRows) Close()                                       {}
func (r *incEmptyRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *incEmptyRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *incEmptyRows) Values() ([]any, error)                       { return nil, nil }
func (r *incEmptyRows) RawValues() [][]byte                          { return nil }
func (r *incEmptyRows) Conn() *pgx.Conn                              { return nil }

type incStringRows struct {
	values []string
	idx    int
}

func (r *incStringRows) Next() bool {
	if r.idx >= len(r.values) {
		return false
	}
	r.idx++
	return true
}
func (r *incStringRows) Scan(dest ...any) error {
	*dest[0].(*string) = r.values[r.idx-1]
	return nil
}
func (r *incStringRows) Err() error                                   { return nil }
func (r *incStringRows) Close()                                       {}
func (r *incStringRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *incStringRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *incStringRows) Values() ([]any, error)                       { return nil, nil }
func (r *incStringRows) RawValues() [][]byte                          { return nil }
func (r *incStringRows) Conn() *pgx.Conn                              { return nil }

// incTestStarter — [handlers.ScenarioStarter] + [handlers.DestroyStarter]-стаб (no-op).
type incTestStarter struct{}

func (incTestStarter) Start(_ context.Context, _ scenario.RunSpec) error        { return nil }
func (incTestStarter) StartDestroy(_ context.Context, _ scenario.RunSpec) error { return nil }

// incTestResolver — [handlers.ServiceResolver]-стаб. ok→резолвит фиктивный ref.
type incTestResolver struct{ ok bool }

func (r incTestResolver) Resolve(service string) (artifact.ServiceRef, bool) {
	return artifact.ServiceRef{Name: service, Ref: "v1"}, r.ok
}

// incTestLoader — [handlers.ServiceSnapshotLoader]-стаб. PrepareDestroy/HasDestroyScenario
// идут через него; Load возвращает art с nil-Manifest (autoCreate/autoDestroy default true).
type incTestLoader struct{}

func (incTestLoader) Load(_ context.Context, ref artifact.ServiceRef) (*artifact.ServiceArtifact, error) {
	return &artifact.ServiceArtifact{Ref: ref}, nil
}
func (incTestLoader) LoadMigrationChain(_ *artifact.ServiceArtifact, _, _ int) (statemigrate.Chain, error) {
	return statemigrate.Chain{}, nil
}
func (incTestLoader) ReadFile(_ *artifact.ServiceArtifact, _ string) ([]byte, error) {
	return nil, nil
}

// incTestDrift — [handlers.DriftChecker]-стаб: CheckDrift отдаёт пустой clean-report.
type incTestDrift struct{}

func (incTestDrift) CheckDrift(_ context.Context, _ scenario.CheckDriftSpec) (*scenario.DriftReport, error) {
	return &scenario.DriftReport{}, nil
}
func (incTestDrift) MarkDriftStatus(_ context.Context, _ string, _ bool) error { return nil }

// incTestScoper — [handlers.PurviewResolver]-стаб: unrestricted → get/list/history видят всё.
type incTestScoper struct{ unrestricted bool }

func (s incTestScoper) ResolvePurview(_, _, _ string) rbac.Purview {
	return rbac.Purview{Unrestricted: s.unrestricted}
}
