package api

// Guard tests for the BATCH-1 read-tier rollout: three READ catalogs (permissions /
// event-types / me-permissions) migrated from strict to huma full-typed (ADR-054
// §Pattern, the READ variant of pilot-1, no audit). They prove that huma routes on
// top of chi:
//
//   - wire: 200 + typed output (Content-Type application/json);
//   - GOLDEN byte-exact: huma-200-reply == legacy oapi-reply of the same handler
//     (omitempty/[]-vs-null/key set/$schema absence) — the main guard of the read-
//     tier rollout;
//   - no-audit: READ does not write audit (no middleware) — a run with a capture
//     writer yields 0 events;
//   - claims: me-permissions reads the AID from ctx (RequireJWT put it there),
//     nil claims → 500 problem+json (defensive parity with the domain Get);
//   - OpenAPI fragment: huma generates a 3.1 spec from the FULL-TYPED Go types.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
)

// catalogInjectClaims injects claims (replaces RequireJWT in prod).
func catalogInjectClaims(aid string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := apimiddleware.InjectClaimsForTest(req.Context(), &keeperjwt.Claims{Subject: aid})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
}

// remarshalSorted round-trips the reply through a map → canonical marshal (keys get
// sorted) — golden fixes the SET of keys/shape/$schema absence, not the order.
func remarshalSorted(t *testing.T, raw []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("reply не JSON: %v; raw=%s", err, raw)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	return string(out)
}

// === GET /v1/permissions ===

func humaPermissionsRouter(t *testing.T, permH *handlers.PermissionCatalogHandler) *chi.Mux {
	t.Helper()
	installHumaErrorOverride()
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		r.Use(catalogInjectClaims("archon-alice"))
		registerHumaPermissionsList(newHumaCadenceAPI(r), permH)
	})
	return r
}

// TestHumaPermissions_GoldenWire — GOLDEN byte-exact read-tier: huma-200-reply ==
// legacy oapi-reply of the same handler. The catalog is static → huma and legacy
// must produce identical bytes (after remarshaling through a map to normalize
// order). Drift (huma injects $schema / a field loses its []-shape) breaks equality.
func TestHumaPermissions_GoldenWire(t *testing.T) {
	permH := handlers.NewPermissionCatalogHandler(nil)
	r := humaPermissionsRouter(t, permH)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/permissions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	// Reference: native projection of the domain ListTyped() (handler-native T5d, no legacy generator).
	want := remarshalSorted(t, mustMarshal(t, newPermissionCatalogReply(permH.ListTyped())))
	got := remarshalSorted(t, rec.Body.Bytes())
	if got != want {
		t.Errorf("GOLDEN byte-exact дрейф permissions huma↔native:\n huma   = %s\n native = %s\n($schema / []-vs-null / onбор ключей разошлись — проверь permissionsListOutput и newHumaCadenceAPI)", got, want)
	}
}

// mustMarshal — json.Marshal with t.Fatal on error (reference for the huma↔native golden).
func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal эталоon: %v", err)
	}
	return b
}

func TestHumaPermissions_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	permH := handlers.NewPermissionCatalogHandler(nil)
	r := humaPermissionsRouter(t, permH)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/permissions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("READ-роут permissions записал audit (%d withбытий) — у нits нет audit-middleware", len(auditCap.Events()))
	}
}

// === GET /v1/event-types ===

func humaEventTypesRouter(t *testing.T, eventTypeH *handlers.EventTypeCatalogHandler) *chi.Mux {
	t.Helper()
	installHumaErrorOverride()
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		r.Use(catalogInjectClaims("archon-alice"))
		registerHumaEventTypesList(newHumaCadenceAPI(r), eventTypeH)
	})
	return r
}

// TestHumaEventTypes_GoldenWire — GOLDEN byte-exact: huma-200 == legacy oapi-reply
// (areas/point_events non-nil, area-glob `<name>.*`).
func TestHumaEventTypes_GoldenWire(t *testing.T) {
	eventTypeH := handlers.NewEventTypeCatalogHandler(nil)
	r := humaEventTypesRouter(t, eventTypeH)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/event-types", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	want := remarshalSorted(t, mustMarshal(t, newEventTypeCatalogReply(eventTypeH.ListTyped())))
	got := remarshalSorted(t, rec.Body.Bytes())
	if got != want {
		t.Errorf("GOLDEN byte-exact дрейф event-types huma↔native:\n huma   = %s\n native = %s", got, want)
	}
}

func TestHumaEventTypes_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	eventTypeH := handlers.NewEventTypeCatalogHandler(nil)
	r := humaEventTypesRouter(t, eventTypeH)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/event-types", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("READ-роут event-types записал audit (%d withбытий)", len(auditCap.Events()))
	}
}

// === GET /v1/me/permissions ===

func humaMyPermissionsRouter(t *testing.T, meH *handlers.MyPermissionsHandler, aid string, withClaims bool) *chi.Mux {
	t.Helper()
	installHumaErrorOverride()
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		if withClaims {
			r.Use(catalogInjectClaims(aid))
		}
		registerHumaMyPermissionsList(newHumaCadenceAPI(r), meH)
	})
	return r
}

// TestHumaMyPermissions_GoldenWire — GOLDEN byte-exact: huma-200 == legacy oapi-reply
// for a specific AID (permissions non-nil, pointer-optional, snake_case scope keys).
func TestHumaMyPermissions_GoldenWire(t *testing.T) {
	e := rbactest.MustEnforcer(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "ops", Operators: []string{"archon-alice"}, Permissions: []string{"incarnation.run", "soul.list"}},
	}})
	meH := handlers.NewMyPermissionsHandler(e, nil)
	r := humaMyPermissionsRouter(t, meH, "archon-alice", true)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/me/permissions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Reference: native projection of the domain GetTyped(aid) for the same AID.
	want := remarshalSorted(t, mustMarshal(t, newMyPermissionsReply(meH.GetTyped("archon-alice"))))
	got := remarshalSorted(t, rec.Body.Bytes())
	if got != want {
		t.Errorf("GOLDEN byte-exact дрейф me-permissions huma↔native:\n huma   = %s\n native = %s", got, want)
	}
}

// TestHumaMyPermissions_ScopeWire — SECURITY-relevant: the state dimension of scope
// (ADR-047 S2c) makes it through the huma route under the snake_case key `state`
// (a converter regression = a silent loss of the scope predicate).
func TestHumaMyPermissions_ScopeWire(t *testing.T) {
	e := rbactest.MustEnforcer(t, &rbactest.Config{Roles: []rbactest.Role{
		{
			Name:        "state-scoped",
			Operators:   []string{"archon-state"},
			Permissions: []string{`incarnation.run on state='state.redis_version == "8.0"'`},
		},
	}})
	meH := handlers.NewMyPermissionsHandler(e, nil)
	r := humaMyPermissionsRouter(t, meH, "archon-state", true)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/me/permissions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"state"`) {
		t.Fatalf("wire-JSON не withдержит scope-ключ \"state\":\n%s", rec.Body.String())
	}
}

// TestHumaMyPermissions_NoClaims_500 — defensive parity with the domain Get: without
// claims in ctx (the auth chain is not assembled) → 500 problem+json.
func TestHumaMyPermissions_NoClaims_500(t *testing.T) {
	e := rbactest.MustEnforcer(t, &rbactest.Config{})
	meH := handlers.NewMyPermissionsHandler(e, nil)
	r := humaMyPermissionsRouter(t, meH, "", false)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/me/permissions", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("без claims status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeInternalError)
}

func TestHumaMyPermissions_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	e := rbactest.MustEnforcer(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "ops", Operators: []string{"archon-alice"}, Permissions: []string{"soul.list"}},
	}})
	meH := handlers.NewMyPermissionsHandler(e, nil)
	r := humaMyPermissionsRouter(t, meH, "archon-alice", true)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/me/permissions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("READ-роут me-permissions записал audit (%d withбытий)", len(auditCap.Events()))
	}
}

// === OpenAPI fragment: three READ catalogs from FULL-TYPED Go types ===

func TestHumaCatalog_OpenAPIFragment_3_1(t *testing.T) {
	frag, err := HumaCatalogSpecYAML()
	if err != nil {
		t.Fatalf("HumaCatalogSpecYAML: %v", err)
	}
	if !strings.Contains(frag, "openapi: 3.1.0") {
		t.Errorf("huma-фрагмент не несёт `openapi: 3.1.0`:\n%s", frag)
	}
	// The dump is built on a bare chi router (without the /v1 prefix), so the paths
	// in the fragment are relative to the /v1 group (/permissions, /event-types,
	// /me/permissions); the real mount (router.go) adds the /v1 prefix.
	for _, want := range []string{
		"listPermissions", "listEventTypes", "listMyPermissions",
		"/permissions", "/event-types", "/me/permissions",
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("OpenAPI-фрагмент не withдержит %q:\n%s", want, frag)
		}
	}
	// READ catalogs have no input → must NOT carry requestBody/octet-stream.
	if strings.Contains(frag, "octet-stream") {
		t.Errorf("OpenAPI-фрагмент несёт application/octet-stream (READ-каталог не имеет тела запроса):\n%s", frag)
	}
}
