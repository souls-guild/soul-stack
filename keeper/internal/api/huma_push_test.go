package api

// Guard-тесты ТИРАЖ-БАТЧА-2e разворота PUSH-домена ЦЕЛИКОМ на huma full-typed (ADR-054
// §Pattern, эталоны operator issue-token + audit-endpoint). apply — WRITE+AUDIT (вариант B,
// huma-audit-middleware; событие push.applied; 202+body async); get/push-runs — read (БЕЗ
// audit). Доказывают инварианты кластера поверх chi:
//
//   - apply unknown-field → 400; apply missing-required → 422; push-runs bad pagination → 400;
//     push-runs bad status-enum → 422; RBAC-deny → 403;
//   - S6-GUARD на apply (единственный write): полная huma-навеска НЕ пишет push.applied на
//     403/400/422 (security-критичная половина — over-write на отказе).
//
// Apply happy-path (202 + S6 RecordsOnSuccess) и push-runs golden НЕ покрыты unit-level:
// *pushorch.PushRun держит конкретный *Store на PG, и сам pushorch-пакет документирует
// (run_test.go §newTestPushRun), что end-to-end Apply поверх Store отложен в
// integration_test.go (build-tag integration_pg) — testcontainers, не unit. Поэтому здесь
// (как и handlers/push_test.go) svc=nil: проверяются validation/RBAC/no-audit-ветки, которые
// отрабатывают ДО обращения к orchestrator-у. happy-path wire — toOapiPushApplyView/
// toOapiPushRunListEntry unit-тесты + pushorch/integration_test.go.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/pushorch"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// hPushApplier — fake-orchestrator (узкая Apply-поверхность): отдаёт статичный
// apply_id без PG. Внедряется через handlers.NewPushHandlerWithApplier для
// happy-path 202 → проверки S6 carrier→audit-пути (см.
// TestHumaAudit_PushApply_RecordsOnSuccess). Прод по-прежнему *pushorch.PushRun.
type hPushApplier struct{ applyID string }

func (a hPushApplier) Apply(context.Context, pushorch.ApplyRequest) (string, error) {
	return a.applyID, nil
}

// humaPushRouter собирает chi-роутер со ВСЕМИ push-роутами через huma — продакшен-навеска
// из router.go: RequirePermission на каждой группе + (apply) huma-audit-middleware вариант B.
// injectClaims заменяет RequireJWT. pushH с nil-svc достаточно для validation/RBAC/no-audit-
// веток (они отрабатывают ДО orchestrator-вызова).
func humaPushRouter(t *testing.T, enforcer apimiddleware.PermissionChecker, auditW audit.Writer, pushH *handlers.PushHandler) *chi.Mux {
	t.Helper()
	installHumaErrorOverride()
	r := chi.NewRouter()
	injectClaims := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := apimiddleware.InjectClaimsForTest(req.Context(), &keeperjwt.Claims{Subject: "archon-alice"})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
	r.Route("/v1", func(r chi.Router) {
		r.Route("/push", func(r chi.Router) {
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "push", "apply", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaPushApply(newHumaPushAPI(r, auditW, audit.EventPushApplied, nil), pushH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "push", "read", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaPushGet(newHumaCadenceAPI(r), pushH)
			})
		})
		r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "incarnation", "history", apimiddleware.NoSelector)).Group(func(r chi.Router) {
			registerHumaPushRunsList(newHumaCadenceAPI(r), pushH)
		})
	})
	return r
}

func nilSvcPushHandler() *handlers.PushHandler { return handlers.NewPushHandler(nil, nil) }

// === APPLY (WRITE+AUDIT push.applied) ===

func TestHumaPush_Apply_UnknownField_400(t *testing.T) {
	r := humaPushRouter(t, strictAllowAll{}, nil, nilSvcPushHandler())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/push/apply",
		strings.NewReader(`{"inventory":["x.example.com"],"destiny":"d@v1","bogus":1}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

func TestHumaPush_Apply_MissingRequired_422(t *testing.T) {
	r := humaPushRouter(t, strictAllowAll{}, nil, nilSvcPushHandler())
	rec := httptest.NewRecorder()
	// inventory отсутствует (required) → huma 422 ДО handler-а (и ДО nil-svc-чека).
	req := httptest.NewRequest(http.MethodPost, "/v1/push/apply", strings.NewReader(`{"destiny":"d@v1"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

func TestHumaPush_Apply_RBACDeny_403(t *testing.T) {
	r := humaPushRouter(t, strictDenyAll{}, nil, nilSvcPushHandler())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/push/apply",
		strings.NewReader(`{"inventory":["x.example.com"],"destiny":"d@v1"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// === S6-AUDIT-GUARD (apply NO-write на отказе) ===

func TestHumaAudit_PushApply_NoAudit_OnRBACDeny(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaPushRouter(t, strictDenyAll{}, auditCap, nilSvcPushHandler())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/push/apply",
		strings.NewReader(`{"inventory":["x.example.com"],"destiny":"d@v1"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан на RBAC-deny push.apply (%d событий)", len(auditCap.Events()))
	}
}

func TestHumaAudit_PushApply_NoAudit_OnValidationFail(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaPushRouter(t, strictAllowAll{}, auditCap, nilSvcPushHandler())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/push/apply", strings.NewReader(`{"destiny":"d@v1"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан на 422 push.apply (%d событий)", len(auditCap.Events()))
	}
}

func TestHumaAudit_PushApply_NoAudit_OnInternalError(t *testing.T) {
	// nil-svc → ApplyTyped возвращает 500 ДО любого orchestrator-вызова; на 5xx
	// middleware вариант B (hctx.Status()>=300) audit НЕ пишет.
	auditCap := &auditCaptureWriter{}
	r := humaPushRouter(t, strictAllowAll{}, auditCap, nilSvcPushHandler())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/push/apply",
		strings.NewReader(`{"inventory":["x.example.com"],"destiny":"d@v1"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (nil-svc); body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан на 500 push.apply (%d событий)", len(auditCap.Events()))
	}
}

// TestHumaAudit_PushApply_RecordsOnSuccess — S6-критичный happy-path: реальный
// POST /v1/push/apply через прод-зеркальную huma.API + RequirePermission → 202 →
// audit ДОЛЖЕН содержать push.applied с НЕПУСТЫМ payload (apply_id). Это ровно
// carrier→middleware-путь, который ломала S6-регрессия (full-typed huma пишет
// ответ сам, минуя StatusRecorder). Мок-сем (hPushApplier) развязывает PG-
// зависимость orchestrator-а. Мутационная проверка: убрать SetHumaAuditPayload в
// registerHumaPushApply → payload пуст → assertAuditWritten красит на len==0.
func TestHumaAudit_PushApply_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	pushH := handlers.NewPushHandlerWithApplier(hPushApplier{applyID: "01HPUSHAPPLY000000000000000"}, nil)
	r := humaPushRouter(t, strictAllowAll{}, auditCap, pushH)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/push/apply",
		strings.NewReader(`{"inventory":["x.example.com"],"destiny":"d@v1.0.0"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventPushApplied, map[string]any{"apply_id": "01HPUSHAPPLY000000000000000"})
}

// === PUSH-RUNS (READ-with-typed-query, БЕЗ audit) — валидация ДО nil-svc-чека ===

func TestHumaPush_RunsList_BadOffset_400(t *testing.T) {
	r := humaPushRouter(t, strictAllowAll{}, nil, nilSvcPushHandler())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/push-runs?offset=-1", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

func TestHumaPush_RunsList_BadLimit_400(t *testing.T) {
	r := humaPushRouter(t, strictAllowAll{}, nil, nilSvcPushHandler())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/push-runs?limit=5000", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

func TestHumaPush_RunsList_BadStatusEnum_422(t *testing.T) {
	r := humaPushRouter(t, strictAllowAll{}, nil, nilSvcPushHandler())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/push-runs?status=weird", nil))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

// TestHumaPush_RunsList_ValidStatusEnum_PassesBind — каждый валидный статус домена
// проходит huma-enum-bind (не отбивается 422) — enum-набор синхронен pushorch.PushRunStatus.
// Дальше nil-svc → 500 (но enum-фаза пройдена, что и проверяем: НЕ 422).
func TestHumaPush_RunsList_ValidStatusEnum_PassesBind(t *testing.T) {
	r := humaPushRouter(t, strictAllowAll{}, nil, nilSvcPushHandler())
	for _, st := range []string{"pending", "running", "success", "partial_failed", "failed", "cancelled"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/push-runs?status="+st, nil))
		if rec.Code == http.StatusUnprocessableEntity {
			t.Errorf("валидный статус %q отбит 422 (enum-набор рассинхронен с доменом); body=%s", st, rec.Body.String())
		}
	}
}

func TestHumaPush_RunsList_RBACDeny_403(t *testing.T) {
	r := humaPushRouter(t, strictDenyAll{}, nil, nilSvcPushHandler())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/push-runs", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHumaPush_GetRunsRead_NoAudit — read-роуты (get/push-runs) не несут audit-middleware:
// прогон с capture-writer (навешан лишь на apply-группу) даёт 0 событий на read.
func TestHumaPush_GetRunsRead_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaPushRouter(t, strictAllowAll{}, auditCap, nilSvcPushHandler())
	for _, path := range []string{"/v1/push-runs", "/v1/push/01HABCDEFGHJKMNPQRSTVWXYZ0"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		// nil-svc → 500, но это read-ветка: audit-middleware на этих группах НЕ навешан.
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("read push get/push-runs записали audit (%d событий)", len(auditCap.Events()))
	}
}

func TestHumaPush_SpecYAML(t *testing.T) {
	frag, err := HumaPushSpecYAML()
	if err != nil {
		t.Fatalf("HumaPushSpecYAML: %v", err)
	}
	for _, want := range []string{"pushApply", "pushGet", "listPushRuns", "/apply", "/push-runs"} {
		if !strings.Contains(frag, want) {
			t.Errorf("спека не содержит %q:\n%s", want, frag)
		}
	}
}
