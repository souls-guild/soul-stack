package api

// Guard-тесты embed-UI на уровне РЕАЛЬНОГО buildRouter (ADR-055, пилот):
//
//   - web_ui_enabled включён → /ui/ публичен (200, без JWT — parity /docs).
//   - web_ui_enabled: false → /ui/ НЕ смонтирован → 404 (catch-all NotFound).
//   - РЕГРЕСС безопасности: embed /ui НЕ открыл API-периметр — /v1/* без JWT
//     остаётся 401 (RequireJWT) при ЛЮБОМ значении тоггла.
//
// Изолированная механика mount/SPA-fallback/редирект/реальный-ассет проверена
// unit-тестами пакета webui (webui/mount_test.go); здесь — интеграция с router-ом
// и тоггл-семантика, недоступные на голом chi. Маркеры контентно-устойчивы:
// при выключенном тоггле проверяем 404 на реально существующем /ui/index.html
// (доказывает «UI off», а не «файла нет»).

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/api/health"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
)

// webUIRouter собирает РЕАЛЬНЫЙ buildRouter с переданным тогглом web_ui_enabled.
// verifier нужен для /v1-регресс-теста (RequireJWT) и /openapi — для проверки
// /ui токен не требуется. Копия metaRouter с управляемым webUIEnabled (отдельный
// helper, чтобы не плодить параметры у meta-теста).
func webUIRouter(t *testing.T, verifier *keeperjwt.Verifier, webUIEnabled bool) http.Handler {
	t.Helper()
	healthH := health.NewHandler(health.Deps{})
	return buildRouter(
		verifier,
		healthH,
		stubOperatorHandler(t),
		handlers.NewIncarnationHandler(nil, nil, nil, nil, nil, nil, nil, nil, nil),
		handlers.NewSoulHandler(nil, nil, nil, nil),
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
		nil,                                  // loginGuard (anti-bruteforce off в тесте)
		apimiddleware.AuthLoginLimitConfig{}, // loginLimitCfg
		nil,                                  // logger
	)
}

// TestWebUI_Enabled_Public200 — при включённом тоггле /ui/ публичен: 200 БЕЗ JWT
// (parity /docs). Регресс (попадание под RequireJWT) дал бы 401/панику на
// nil-verifier.
func TestWebUI_Enabled_Public200(t *testing.T) {
	r := webUIRouter(t, nil, true)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/", http.NoBody)) // НИКАКОГО Authorization
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui/ при web_ui_enabled=on БЕЗ JWT = %d, want 200 (публичная статика); body=%s",
			rec.Code, rec.Body.String())
	}
}

// TestWebUI_Disabled_NotMounted — при web_ui_enabled: false /ui НЕ смонтирован:
// /ui/ уходит в catch-all NotFound → 404 (не 200).
func TestWebUI_Disabled_NotMounted(t *testing.T) {
	r := webUIRouter(t, nil, false)
	// /ui/index.html — реально существующий файл embed-дерева: при выключенном
	// тоггле он не смонтирован, значит ДОЛЖЕН дать 404 (доказывает «UI off», а не
	// «файла нет»).
	for _, path := range []string{"/ui", "/ui/", "/ui/index.html"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, http.NoBody))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s при web_ui_enabled=false = %d, want 404 (UI не смонтирован); body=%s",
				path, rec.Code, rec.Body.String())
		}
	}
}

// TestWebUI_DoesNotOpenAPIPerimeter — РЕГРЕСС безопасности (ADR-055 §в): публичная
// статика /ui не ослабляет API-границу. /v1/* без JWT остаётся 401 при ЛЮБОМ
// значении тоггла (RequireJWT не зависит от webui-mount-а).
func TestWebUI_DoesNotOpenAPIPerimeter(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		r := webUIRouter(t, nil, enabled)
		rec := httptest.NewRecorder()
		// Любой /v1-роут без Authorization — RequireJWT отвечает 401 ДО RBAC
		// (enforcer=nil не разыменовывается). 403 тоже приемлем по ТЗ, но
		// RequireJWT для анонима даёт именно 401.
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/souls", http.NoBody))
		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
			t.Errorf("GET /v1/souls без JWT (web_ui_enabled=%v) = %d, want 401/403 — embed /ui НЕ должен открывать API-периметр; body=%s",
				enabled, rec.Code, rec.Body.String())
		}
	}
}
