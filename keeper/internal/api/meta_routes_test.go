package api

// Guard-тесты meta-зоны (механизм A, ADR-054 doc-viewer). БЕЗОПАСНОСТЬ-инвариант:
//
//   - /healthz, /readyz — ПУБЛИЧНЫЕ (liveness/readiness), 200 без JWT.
//   - /docs — ПУБЛИЧНЫЙ shell вьювера, 200 без JWT (поле ввода JWT; API-
//     поверхность НЕ раскрыта — спека приходит лишь после fetch за JWT).
//   - /openapi.yaml + /openapi.json — ЗА JWT: 401 без токена, 200 с валидным.
//     Раньше /openapi.yaml был публичным (T1); механизм A спрятал полную
//     API-поверхность за Bearer. JSON-вариант фетчит вьювер /docs (RapiDoc).
//
// Тесты прогоняют роуты через РЕАЛЬНЫЙ buildRouter. Регресс (публичная спека или
// auth на /healthz/readyz/docs) ловится сменой кода ответа.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/api/health"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
)

const (
	metaSigningKey = "0123456789abcdef0123456789abcdef" // 32 байта (HS256)
	metaIssuer     = "keeper.api.meta-test"
)

// metaVerifier строит verifier поверх metaSigningKey/metaIssuer — для проверки
// JWT на /openapi.yaml.
func metaVerifier(t *testing.T) *keeperjwt.Verifier {
	t.Helper()
	v, err := keeperjwt.NewVerifier([]byte(metaSigningKey), metaIssuer)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v
}

// metaValidToken выпускает валидный JWT (тот же ключ/issuer, что у metaVerifier).
func metaValidToken(t *testing.T) string {
	t.Helper()
	iss, err := keeperjwt.NewIssuer([]byte(metaSigningKey), metaIssuer)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	tok, err := iss.Issue("archon-meta-test", []string{"cluster-admin"}, time.Hour, false)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return tok
}

// metaRouter собирает РЕАЛЬНЫЙ buildRouter с non-nil healthH (все Pinger-ы nil →
// Readyz пропускает проверки → 200) и переданным verifier-ом (нужен для JWT-гейта
// /openapi.yaml). verifier может быть nil для тестов, где /openapi.yaml не зовётся
// с токеном (без токена RequireJWT отвечает 401 ДО разыменования verifier).
func metaRouter(t *testing.T, verifier *keeperjwt.Verifier) http.Handler {
	t.Helper()
	healthH := health.NewHandler(health.Deps{})
	return buildRouter(
		verifier,
		healthH,
		stubOperatorHandler(t),
		handlers.NewIncarnationHandler(nil, nil, nil, nil, nil, nil, nil, nil, nil),
		handlers.NewSoulHandler(nil, nil, nil, nil),
		stubRoleHandler(t), stubSynodHandler(t), stubSigilHandler(t), stubSigilKeyHandler(t),
		stubServiceHandler(t), stubAugurHandler(t), stubOracleHandler(t),
		nil, // pushH
		nil, // pushProviderH
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
		nil, // logger
	)
}

// TestMetaRoutes_Public_200 — healthz/readyz/docs отвечают 200 БЕЗ JWT (публичны).
// Регресс (попадание под RequireJWT/RBAC) дал бы 401/403/панику на nil-verifier.
func TestMetaRoutes_Public_200(t *testing.T) {
	r := metaRouter(t, nil)
	for _, path := range []string{"/healthz", "/readyz", "/docs"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, http.NoBody) // НИКАКОГО Authorization-заголовка
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s БЕЗ JWT = %d, want 200 (публичный meta-роут); body=%s",
				path, rec.Code, rec.Body.String())
		}
	}
}

// TestMetaRoutes_OpenAPI_RequiresJWT — guard механизма A: /openapi.yaml ЗА JWT.
//   - без Authorization → 401 (спека НЕ раскрывается анонимам);
//   - с валидным Bearer → 200, тело — runtime-дамп huma-агрегатора (3.1).
func TestMetaRoutes_OpenAPI_RequiresJWT(t *testing.T) {
	r := metaRouter(t, metaVerifier(t))

	// (1) Без токена — 401.
	recNoAuth := httptest.NewRecorder()
	reqNoAuth := httptest.NewRequest(http.MethodGet, "/openapi.yaml", http.NoBody)
	r.ServeHTTP(recNoAuth, reqNoAuth)
	if recNoAuth.Code != http.StatusUnauthorized {
		t.Fatalf("GET /openapi.yaml БЕЗ JWT = %d, want 401 (спека за JWT, механизм A); body=%s",
			recNoAuth.Code, recNoAuth.Body.String())
	}

	// (2) С валидным токеном — 200 + application/yaml + huma 3.1-дамп.
	recAuth := httptest.NewRecorder()
	reqAuth := httptest.NewRequest(http.MethodGet, "/openapi.yaml", http.NoBody)
	reqAuth.Header.Set("Authorization", "Bearer "+metaValidToken(t))
	r.ServeHTTP(recAuth, reqAuth)

	if recAuth.Code != http.StatusOK {
		t.Fatalf("GET /openapi.yaml с валидным JWT = %d, want 200; body=%s", recAuth.Code, recAuth.Body.String())
	}
	if ct := recAuth.Header().Get("Content-Type"); ct != "application/yaml; charset=utf-8" {
		t.Errorf("Content-Type = %q, want application/yaml; charset=utf-8", ct)
	}
	if recAuth.Body.Len() == 0 {
		t.Fatal("тело /openapi.yaml пусто — huma-дамп не отдан")
	}
	// huma сортирует top-level ключи алфавитно — `openapi:` не обязан быть первым;
	// проверяем версию как подстроку (3.1 vs прежняя 3.0.3-рукопись).
	if !strings.Contains(recAuth.Body.String(), "openapi: 3.1") {
		t.Error("served-спека не содержит `openapi: 3.1` — версия дампа не 3.1")
	}
}

// TestMetaRoutes_Docs_ShellAndAssets — /docs отдаёт HTML-shell вьювера (поле JWT +
// подключённый RapiDoc web-component), а /docs/assets/* — вшитую статику, ОБА без
// JWT. API-поверхность в shell не раскрыта (только поле ввода токена).
func TestMetaRoutes_Docs_ShellAndAssets(t *testing.T) {
	r := metaRouter(t, nil)

	// (1) Shell-страница.
	recShell := httptest.NewRecorder()
	r.ServeHTTP(recShell, httptest.NewRequest(http.MethodGet, "/docs", http.NoBody))
	if recShell.Code != http.StatusOK {
		t.Fatalf("GET /docs = %d, want 200", recShell.Code)
	}
	if ct := recShell.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type /docs = %q, want text/html…", ct)
	}
	body := recShell.Body.String()
	for _, marker := range []string{
		"Paste your Archon JWT",       // поле ввода токена
		"<rapi-doc",                   // RapiDoc web-component
		"/docs/assets/rapidoc-min.js", // подключение вшитого скрипта
		"loadSpec",                    // inline-render объектом (не spec-url)
		"allow-advanced-search",       // full-text поиск — причина перехода на RapiDoc
		"sessionStorage",              // XSS-гигиена (не localStorage)
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("/docs shell не содержит ожидаемый маркер %q", marker)
		}
	}
	if strings.Contains(body, "localStorage") {
		t.Error("/docs использует localStorage — XSS-гигиена требует sessionStorage")
	}

	// (2) Вшитый ассет — публичен, непуст, корректный JS-Content-Type.
	recAsset := httptest.NewRecorder()
	r.ServeHTTP(recAsset, httptest.NewRequest(http.MethodGet, "/docs/assets/rapidoc-min.js", http.NoBody))
	if recAsset.Code != http.StatusOK {
		t.Fatalf("GET /docs/assets/rapidoc-min.js = %d, want 200", recAsset.Code)
	}
	if recAsset.Body.Len() == 0 {
		t.Error("вшитый ассет rapidoc-min.js пуст — go:embed не наполнен?")
	}
}

// TestMetaRoutes_OpenAPIJSON_RequiresJWT — guard механизма A для JSON-варианта
// спеки (фетчит вьювер /docs, RapiDoc loadSpec):
//   - без Authorization → 401 (спека НЕ раскрывается анонимам);
//   - с валидным Bearer → 200, тело — JSON-дамп того же huma-агрегатора (3.1).
func TestMetaRoutes_OpenAPIJSON_RequiresJWT(t *testing.T) {
	r := metaRouter(t, metaVerifier(t))

	// (1) Без токена — 401.
	recNoAuth := httptest.NewRecorder()
	r.ServeHTTP(recNoAuth, httptest.NewRequest(http.MethodGet, "/openapi.json", http.NoBody))
	if recNoAuth.Code != http.StatusUnauthorized {
		t.Fatalf("GET /openapi.json БЕЗ JWT = %d, want 401 (спека за JWT, механизм A); body=%s",
			recNoAuth.Code, recNoAuth.Body.String())
	}

	// (2) С валидным токеном — 200 + application/json + huma 3.1-дамп.
	recAuth := httptest.NewRecorder()
	reqAuth := httptest.NewRequest(http.MethodGet, "/openapi.json", http.NoBody)
	reqAuth.Header.Set("Authorization", "Bearer "+metaValidToken(t))
	r.ServeHTTP(recAuth, reqAuth)

	if recAuth.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json с валидным JWT = %d, want 200; body=%s", recAuth.Code, recAuth.Body.String())
	}
	if ct := recAuth.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, want application/json; charset=utf-8", ct)
	}
	if recAuth.Body.Len() == 0 {
		t.Fatal("тело /openapi.json пусто — huma-дамп не отдан")
	}
	// JSON-форма 3.1: версия в кавычках как значение ключа "openapi".
	if !strings.Contains(recAuth.Body.String(), `"openapi":"3.1`) {
		t.Error("served-JSON-спека не содержит `\"openapi\":\"3.1` — версия дампа не 3.1")
	}
}

// TestDocsShell_SetApiKey_CleanJWT — контракт-guard механизма A: setApiKey
// получает ЧИСТЫЙ jwt без Bearer-префикса. RapiDoc сам добавляет 'Bearer ' для
// http/bearer-схемы (bearerAuth), поэтому двойной префикс ('Bearer Bearer …')
// сломал бы "Try It". КРАСНЕЕТ, если кто-то вернёт ручной Bearer-префикс В
// setApiKey.
//
// ВАЖНО: проверяем именно вызов setApiKey, а НЕ глобальное отсутствие
// 'Bearer ' + jwt в docsPage — Bearer-префикс ЛЕГИТИМЕН в fetch-заголовке
// '/openapi.json' (там его добавляет наш JS, RapiDoc к fetch непричастен).
// Глобальный запрет ложно бил бы по корректной строке fetch.
func TestDocsShell_SetApiKey_CleanJWT(t *testing.T) {
	if !strings.Contains(docsPage, "setApiKey('bearerAuth', jwt)") {
		t.Error("docsPage не зовёт setApiKey('bearerAuth', jwt) с ЧИСТЫМ jwt — Try It не получит токен")
	}
	// RapiDoc сам префиксует 'Bearer ' для http/bearer; ручной префикс в
	// setApiKey = двойной Bearer.
	if strings.Contains(docsPage, "setApiKey('bearerAuth', 'Bearer") {
		t.Error("docsPage передаёт в setApiKey уже Bearer-префиксованный литерал — двойной префикс ломает Try It")
	}
	if strings.Contains(docsPage, "setApiKey('bearerAuth', 'Bearer ' + jwt") {
		t.Error("docsPage склеивает 'Bearer ' + jwt в setApiKey — RapiDoc добавит свой префикс, выйдет двойной Bearer")
	}
}

// TestDocsShell_LoadSpec_Object — контракт-guard механизма A: loadSpec получает
// ОБЪЕКТ (resp.json()), а не строку. RapiDoc.loadSpec(строка) трактует аргумент
// как spec-URL и фетчит его БЕЗ нашего Bearer → 401. Подаём разобранный JSON.
// КРАСНЕЕТ, если кто-то вернёт loadSpec(строка)/resp.text().
func TestDocsShell_LoadSpec_Object(t *testing.T) {
	if !strings.Contains(docsPage, "loadSpec(specObj)") {
		t.Error("docsPage не зовёт loadSpec(specObj) — RapiDoc.loadSpec(строка) трактует её как spec-URL (фетч без Bearer → 401)")
	}
	if !strings.Contains(docsPage, "resp.json()") {
		t.Error("docsPage не парсит resp.json() — loadSpec нужен ОБЪЕКТ, не сырой текст спеки")
	}
	if strings.Contains(docsPage, "resp.text()") {
		t.Error("docsPage читает resp.text() — loadSpec(строка)=spec-URL → фетч без Bearer → 401")
	}
}

// TestServedSpec_YAML_JSON_SameSourceOfTruth — guard единого source-of-truth:
// YAML и JSON served-спеки собираются из ОДНОГО buildFullOpenAPISpec, поэтому
// после нормализации (YAML→map, JSON→map) структуры обязаны быть идентичны.
// Если форматы разойдутся (разный агрегатор/ручная правка одного из них) —
// reflect.DeepEqual покраснеет.
func TestServedSpec_YAML_JSON_SameSourceOfTruth(t *testing.T) {
	yamlBytes, err := openAPISpecBytes()
	if err != nil {
		t.Fatalf("openAPISpecBytes: %v", err)
	}
	jsonBytes, err := openAPISpecJSONBytes()
	if err != nil {
		t.Fatalf("openAPISpecJSONBytes: %v", err)
	}

	// Оба парсим в одну и ту же Go-форму (map[string]any), чтобы сравнение не
	// зависело от текстовых различий YAML vs JSON (отступы, кавычки, порядок) —
	// только от СТРУКТУРЫ. yaml.v3 в any даёт map[string]interface{} как и
	// encoding/json, поэтому DeepEqual сопоставим. Числа выравниваем (JSON даёт
	// float64, YAML — int) перед сравнением.
	var fromYAML map[string]any
	if err := yaml.Unmarshal(yamlBytes, &fromYAML); err != nil {
		t.Fatalf("разбор YAML-спеки: %v", err)
	}
	var fromJSON map[string]any
	if err := json.Unmarshal(jsonBytes, &fromJSON); err != nil {
		t.Fatalf("разбор JSON-спеки: %v", err)
	}

	normalizeNumbers(fromYAML)
	normalizeNumbers(fromJSON)

	if !reflect.DeepEqual(fromYAML, fromJSON) {
		t.Error("YAML- и JSON-served-спеки структурно расходятся — нарушен единый source-of-truth (buildFullOpenAPISpec)")
	}
}

// normalizeNumbers приводит все числовые значения дерева к float64. yaml.v3
// разбирает целые как int, encoding/json — как float64; без выравнивания
// DeepEqual ложно краснел бы на идентичных по значению числах (например, на
// minimum/maximum в схемах пагинации).
func normalizeNumbers(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			switch n := val.(type) {
			case int:
				t[k] = float64(n)
			case int64:
				t[k] = float64(n)
			case uint64:
				t[k] = float64(n)
			default:
				normalizeNumbers(val)
			}
		}
	case []any:
		for i, val := range t {
			switch n := val.(type) {
			case int:
				t[i] = float64(n)
			case int64:
				t[i] = float64(n)
			case uint64:
				t[i] = float64(n)
			default:
				normalizeNumbers(val)
			}
		}
	}
}
