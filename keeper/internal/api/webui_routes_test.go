package api

// Guard tests for the embed UI at the level of the REAL buildRouter (ADR-055, pilot):
//
//   - web_ui_enabled on → /ui/ is public (200, without JWT — parity /docs).
//   - web_ui_enabled: false → /ui/ is NOT mounted → 404 (catch-all NotFound).
//   - security REGRESSION: the embed /ui did NOT open the API perimeter — /v1/* without JWT
//     stays 401 (RequireJWT) for ANY toggle value.
//
// The isolated mount/SPA-fallback/redirect/real-asset mechanics are checked by
// the webui package unit tests (webui/mount_test.go); here — integration with the router
// and toggle semantics, unavailable on bare chi. The markers are content-stable:
// with the toggle off we check 404 on the genuinely existing /ui/index.html
// (proving "UI off", not "the file is missing").

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/api/health"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
)

// webUIRouter builds the REAL buildRouter with the given web_ui_enabled toggle.
// verifier is needed for the /v1 regression test (RequireJWT) and /openapi — for /ui
// no token is required. A copy of metaRouter with a controllable webUIEnabled (a separate
// helper, so as not to multiply parameters on the meta test).
func webUIRouter(t *testing.T, verifier *keeperjwt.Verifier, webUIEnabled bool) http.Handler {
	t.Helper()
	healthH := health.NewHandler(health.Deps{})
	return buildRouter(
		verifier,
		healthH,
		stubOperatorHandler(t),
		handlers.NewIncarnationHandler(nil, nil, nil, nil, nil, nil, nil, nil, nil),
		handlers.NewSoulHandler(nil, nil, nil, nil),
		handlers.TelemetrySpecStub(),
		stubRoleHandler(t), stubSynodHandler(t), stubSigilHandler(t), stubSigilKeyHandler(t),
		stubServiceHandler(t), nil, stubAugurHandler(t), stubOracleHandler(t),
		nil, // pushH
		nil, // pushProviderH
		nil, // providerH
		nil, // profileH
		nil, // errandH
		nil, // voyageH
		nil, // cadenceH
		nil, // auditH
		nil, // choirH
		nil, // heraldH
		handlers.NewModuleCatalogHandler(nil, nil),
		handlers.NewModuleFormPrepHandler(nil, nil),
		handlers.NewPermissionCatalogHandler(nil),
		handlers.NewEventTypeCatalogHandler(nil),
		handlers.NewHeraldTypeCatalogHandler(nil),
		handlers.NewMyPermissionsHandler(nil, nil),
		nil, // enforcer
		nil, // auditWriter
		nil, // metricsHTTP
		nil, // tollDegraded
		nil, // tempoLimiter
		nil, // tempoMetrics
		nil, // tempoVoyageCreateLimits
		nil, // tempoVoyagePreviewLimits
		webUIEnabled,
		nil,                                  // ldapAuth (LDAP не сконфигурирован в тесте)
		nil,                                  // oidcAuth (OIDC не сконфигурирован в тесте)
		nil,                                  // authToken (обмен /auth/token не тестируется здесь)
		AuthMethodsDeps{},                    // authMethods (/auth/methods монтируется, но не проверяется)
		nil,                                  // loginGuard (anti-bruteforce off в тесте)
		apimiddleware.AuthLoginLimitConfig{}, // loginLimitCfg
		nil,                                  // soulStatsStaleFn (default 90s in the test)
		nil,                                  // clusterH (cluster-view not mounted in the test)
		nil,                                  // runEventsDeps (ADR-068 §A3 — not tested here)
		nil,                                  // logger
	)
}

// TestWebUI_Enabled_Public200 — with the toggle on, /ui/ is public: 200 WITHOUT JWT
// (parity /docs). A regression (falling under RequireJWT) would give 401/a panic on
// a nil verifier.
func TestWebUI_Enabled_Public200(t *testing.T) {
	r := webUIRouter(t, nil, true)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/", http.NoBody)) // NO Authorization
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui/ with web_ui_enabled=on WITHOUT JWT = %d, want 200 (public static assets); body=%s",
			rec.Code, rec.Body.String())
	}
}

// TestWebUI_Disabled_NotMounted — with web_ui_enabled: false, /ui is NOT mounted:
// /ui/ falls into the catch-all NotFound → 404 (not 200).
func TestWebUI_Disabled_NotMounted(t *testing.T) {
	r := webUIRouter(t, nil, false)
	// /ui/index.html — a genuinely existing file of the embed tree: with the toggle
	// off it is not mounted, so it MUST give 404 (proving "UI off", not
	// "the file is missing").
	for _, path := range []string{"/ui", "/ui/", "/ui/index.html"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, http.NoBody))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s with web_ui_enabled=false = %d, want 404 (UI not mounted); body=%s",
				path, rec.Code, rec.Body.String())
		}
	}
}

// TestWebUI_DoesNotOpenAPIPerimeter — a security REGRESSION (ADR-055 §c): the public
// /ui static does not weaken the API boundary. /v1/* without JWT stays 401 for ANY
// toggle value (RequireJWT does not depend on the webui mount).
func TestWebUI_DoesNotOpenAPIPerimeter(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		r := webUIRouter(t, nil, enabled)
		rec := httptest.NewRecorder()
		// Any /v1 route without Authorization — RequireJWT answers 401 BEFORE RBAC
		// (enforcer=nil is not dereferenced). 403 is also acceptable per the spec, but
		// RequireJWT for an anonymous caller gives exactly 401.
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/souls", http.NoBody))
		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
			t.Errorf("GET /v1/souls without JWT (web_ui_enabled=%v) = %d, want 401/403 - embed /ui must NOT open the API perimeter; body=%s",
				enabled, rec.Code, rec.Body.String())
		}
	}
}
