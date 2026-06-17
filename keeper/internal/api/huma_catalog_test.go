package api

// Guard-тесты БАТЧА-1 read-tier тиража: три READ-каталога (permissions /
// event-types / me-permissions) переведены со strict на huma full-typed (ADR-054
// §Pattern, READ-вариант pilot-1 БЕЗ audit). Доказывают, что huma-роуты поверх chi:
//
//   - wire: 200 + typed output (Content-Type application/json);
//   - GOLDEN byte-exact: huma-200-reply == legacy oapi-reply того же handler-а
//     (omitempty/[]-vs-null/набор ключей/$schema-отсутствие) — главный guard read-
//     tier тиража;
//   - no-audit: READ не пишет audit (нет middleware) — прогон с capture-writer даёт
//     0 событий;
//   - claims: me-permissions читает AID из ctx (RequireJWT положил), nil-claims →
//     500 problem+json (defensive parity доменного Get);
//   - OpenAPI-фрагмент: huma генерит 3.1-спеку из FULL-TYPED Go-типов.

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

// catalogInjectClaims навешивает claims (заменяет RequireJWT в проде).
func catalogInjectClaims(aid string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := apimiddleware.InjectClaimsForTest(req.Context(), &keeperjwt.Claims{Subject: aid})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
}

// remarshalSorted перекладывает reply через map → канонический marshal (ключи
// сортируются) — golden фиксирует НАБОР ключей/форму/$schema-отсутствие, не порядок.
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
// legacy oapi-reply того же handler-а. Каталог статичен → huma и legacy обязаны
// дать идентичные байты (после ремаршала через map для нормализации порядка).
// Дрейф (huma вмешает $schema / поле потеряет []-форму) ломает равенство.
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

	// Эталон: native-проекция доменного ListTyped() (handler-native T5d, без legacy-генерата).
	want := remarshalSorted(t, mustMarshal(t, newPermissionCatalogReply(permH.ListTyped())))
	got := remarshalSorted(t, rec.Body.Bytes())
	if got != want {
		t.Errorf("GOLDEN byte-exact дрейф permissions huma↔native:\n huma   = %s\n native = %s\n($schema / []-vs-null / набор ключей разошлись — проверь permissionsListOutput и newHumaCadenceAPI)", got, want)
	}
}

// mustMarshal — json.Marshal с t.Fatal на ошибке (эталон для huma↔native golden).
func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal эталона: %v", err)
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
		t.Errorf("READ-роут permissions записал audit (%d событий) — у него нет audit-middleware", len(auditCap.Events()))
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
		t.Errorf("READ-роут event-types записал audit (%d событий)", len(auditCap.Events()))
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
// для конкретного AID (permissions non-nil, pointer-optional, snake_case scope-ключи).
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

	// Эталон: native-проекция доменного GetTyped(aid) для того же AID.
	want := remarshalSorted(t, mustMarshal(t, newMyPermissionsReply(meH.GetTyped("archon-alice"))))
	got := remarshalSorted(t, rec.Body.Bytes())
	if got != want {
		t.Errorf("GOLDEN byte-exact дрейф me-permissions huma↔native:\n huma   = %s\n native = %s", got, want)
	}
}

// TestHumaMyPermissions_ScopeWire — SECURITY-relevant: state-измерение scope
// (ADR-047 S2c) долетает через huma-роут под snake_case-ключом `state` (регресс
// конвертера = молчаливая утеря scope-предиката).
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
		t.Fatalf("wire-JSON не содержит scope-ключ \"state\":\n%s", rec.Body.String())
	}
}

// TestHumaMyPermissions_NoClaims_500 — defensive parity доменного Get: без claims
// в ctx (auth-chain не собрана) → 500 problem+json.
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
		t.Errorf("READ-роут me-permissions записал audit (%d событий)", len(auditCap.Events()))
	}
}

// === OpenAPI-фрагмент: три READ-каталога из FULL-TYPED Go-типов ===

func TestHumaCatalog_OpenAPIFragment_3_1(t *testing.T) {
	frag, err := HumaCatalogSpecYAML()
	if err != nil {
		t.Fatalf("HumaCatalogSpecYAML: %v", err)
	}
	if !strings.Contains(frag, "openapi: 3.1.0") {
		t.Errorf("huma-фрагмент не несёт `openapi: 3.1.0`:\n%s", frag)
	}
	// Дамп строится на bare chi-роутере (без /v1-префикса), поэтому пути в
	// фрагменте — относительные к группе /v1 (/permissions, /event-types,
	// /me/permissions); /v1-префикс добавляет реальный mount (router.go).
	for _, want := range []string{
		"listPermissions", "listEventTypes", "listMyPermissions",
		"/permissions", "/event-types", "/me/permissions",
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("OpenAPI-фрагмент не содержит %q:\n%s", want, frag)
		}
	}
	// READ-каталоги без входа → НЕ должны нести requestBody/octet-stream.
	if strings.Contains(frag, "octet-stream") {
		t.Errorf("OpenAPI-фрагмент несёт application/octet-stream (READ-каталог не имеет тела запроса):\n%s", frag)
	}
}
