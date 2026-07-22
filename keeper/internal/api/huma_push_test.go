package api

// Guard tests for ROLLOUT-BATCH-2e of the PUSH domain flipping ENTIRELY onto huma
// full-typed (ADR-054 §Pattern, reference: operator issue-token + audit-endpoint).
// apply is WRITE+AUDIT (variant B, huma-audit-middleware; event push.applied;
// 202+body async); get/push-runs are read (WITHOUT audit). Prove cluster invariants
// on top of chi:
//
//   - apply unknown-field → 400; apply missing-required → 422; push-runs bad pagination → 400;
//     push-runs bad status-enum → 422; RBAC-deny → 403;
//   - S6-GUARD on apply (the only write): the full huma wiring does NOT write
//     push.applied on 403/400/422 (the security-critical half — over-write on failure).
//
// The apply happy-path (202 + S6 RecordsOnSuccess) and push-runs golden are NOT
// covered at unit level: *pushorch.PushRun holds a concrete *Store on PG, and the
// pushorch package itself documents (run_test.go §newTestPushRun) that end-to-end
// Apply over Store is deferred to integration_test.go (build-tag integration_pg) —
// testcontainers, not unit. So here (like handlers/push_test.go) svc=nil: this
// checks the validation/RBAC/no-audit branches that run BEFORE reaching the
// orchestrator. happy-path wire — toOapiPushApplyView/toOapiPushRunListEntry unit
// tests + pushorch/integration_test.go.

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

// hPushApplier — fake-orchestrator (narrow Apply surface): returns static
// apply_id without PG. Injected via handlers.NewPushHandlerWithApplier for
// happy-path 202 → S6 carrier→audit path checks (see
// TestHumaAudit_PushApply_RecordsOnSuccess). Prod still *pushorch.PushRun.
type hPushApplier struct{ applyID string }

func (a hPushApplier) Apply(context.Context, pushorch.ApplyRequest) (string, error) {
	return a.applyID, nil
}

// humaPushRouter builds chi router with ALL push routes via huma — production wrapper
// from router.go: RequirePermission on each group + (apply) huma-audit-middleware variant B.
// injectClaims replaces RequireJWT. pushH with nil-svc sufficient for validation/RBAC/no-audit-
// branches (they work BEFORE orchestrator call).
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
	// inventory missing (required) → huma 422 BEFORE handler (and BEFORE nil-svc check).
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

// === S6-AUDIT-GUARD (apply NO-write on rejection) ===

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
		t.Errorf("audit written on RBAC-deny push.apply (%d events)", len(auditCap.Events()))
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
		t.Errorf("audit written on 422 push.apply (%d events)", len(auditCap.Events()))
	}
}

func TestHumaAudit_PushApply_NoAudit_OnInternalError(t *testing.T) {
	// nil-svc → ApplyTyped returns 500 BEFORE any orchestrator call; on 5xx
	// middleware variant B (hctx.Status()>=300) audit does NOT write.
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
		t.Errorf("audit written on 500 push.apply (%d events)", len(auditCap.Events()))
	}
}

// TestHumaAudit_PushApply_RecordsOnSuccess — S6-critical happy-path: real
// POST /v1/push/apply through prod-mirror huma.API + RequirePermission → 202 →
// audit MUST contain push.applied with NON-EMPTY payload (apply_id). This is exactly
// carrier→middleware path, which S6-regression broke (full-typed huma writes
// response itself, bypassing StatusRecorder). Mock setup (hPushApplier) decouples PG
// orchestrator dependency. Mutation check: remove SetHumaAuditPayload in
// registerHumaPushApply → payload empty → assertAuditWritten paints at len==0.
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

// === PUSH-RUNS (READ-with-typed-query, NO audit) — validation BEFORE nil-svc check ===

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

// TestHumaPush_RunsList_ValidStatusEnum_PassesBind — each valid status of domain
// passes huma-enum-bind (not rejected with 422) — enum set synchronized with pushorch.PushRunStatus.
// Then nil-svc → 500 (but enum phase passed, which we check: NOT 422).
func TestHumaPush_RunsList_ValidStatusEnum_PassesBind(t *testing.T) {
	r := humaPushRouter(t, strictAllowAll{}, nil, nilSvcPushHandler())
	for _, st := range []string{"pending", "running", "success", "partial_failed", "failed", "cancelled"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/push-runs?status="+st, nil))
		if rec.Code == http.StatusUnprocessableEntity {
			t.Errorf("valid status %q rejected with 422 (enum set out of sync with domain); body=%s", st, rec.Body.String())
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

// TestHumaPush_GetRunsRead_NoAudit — read routes (get/push-runs) do not carry audit-middleware:
// run with capture-writer (attached only to apply group) gives 0 events on read.
func TestHumaPush_GetRunsRead_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaPushRouter(t, strictAllowAll{}, auditCap, nilSvcPushHandler())
	for _, path := range []string{"/v1/push-runs", "/v1/push/01HABCDEFGHJKMNPQRSTVWXYZ0"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		// nil-svc → 500, but this is read branch: audit-middleware NOT attached to these groups.
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("read push get/push-runs wrote audit (%d events)", len(auditCap.Events()))
	}
}

func TestHumaPush_SpecYAML(t *testing.T) {
	frag, err := HumaPushSpecYAML()
	if err != nil {
		t.Fatalf("HumaPushSpecYAML: %v", err)
	}
	for _, want := range []string{"pushApply", "pushGet", "listPushRuns", "/apply", "/push-runs"} {
		if !strings.Contains(frag, want) {
			t.Errorf("spec does not contain %q:\n%s", want, frag)
		}
	}
}
