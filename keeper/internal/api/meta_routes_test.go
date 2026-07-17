package api

// Guard tests for the meta zone (mechanism A, ADR-054 doc-viewer). SECURITY invariant:
//
//   - /healthz, /readyz — PUBLIC (liveness/readiness), 200 without a JWT.
//   - /docs — the PUBLIC viewer shell, 200 without a JWT (a JWT input field; the API
//     surface is NOT exposed — the spec arrives only after a fetch with a JWT).
//   - /openapi.yaml + /openapi.json — BEHIND a JWT: 401 without a token, 200 with a valid one.
//     /openapi.yaml used to be public (T1); mechanism A hid the full
//     API surface behind Bearer. The JSON variant is fetched by the /docs viewer (RapiDoc).
//
// The tests drive the routes through the REAL buildRouter. A regression (a public spec or
// auth on /healthz/readyz/docs) is caught by a changed response code.

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
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
)

const (
	metaSigningKey = "0123456789abcdef0123456789abcdef" // 32 bytes (HS256)
	metaIssuer     = "keeper.api.meta-test"
)

// metaVerifier builds a verifier on top of metaSigningKey/metaIssuer — for verifying
// the JWT on /openapi.yaml.
func metaVerifier(t *testing.T) *keeperjwt.Verifier {
	t.Helper()
	v, err := keeperjwt.NewVerifier([]byte(metaSigningKey), metaIssuer)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v
}

// metaValidToken issues a valid JWT (the same key/issuer as metaVerifier).
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

// metaRouter assembles the REAL buildRouter with a non-nil healthH (all Pingers nil →
// Readyz skips the checks → 200) and the given verifier (needed for the JWT gate on
// /openapi.yaml). verifier may be nil for tests where /openapi.yaml is never called
// with a token (without a token RequireJWT responds 401 BEFORE dereferencing verifier).
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
		nil,                                  // enforcer
		nil,                                  // auditWriter
		nil,                                  // metricsHTTP
		nil,                                  // tollDegraded
		nil,                                  // tempoLimiter
		nil,                                  // tempoMetrics
		nil,                                  // tempoVoyageCreateLimits
		nil,                                  // tempoVoyagePreviewLimits
		false,                                // webUIEnabled — meta tests don't check /ui (guard in webui_routes_test.go)
		nil,                                  // ldapAuth (LDAP not configured in the test)
		nil,                                  // oidcAuth (OIDC not configured in the test)
		nil,                                  // loginGuard (anti-bruteforce off in the test)
		apimiddleware.AuthLoginLimitConfig{}, // loginLimitCfg
		nil,                                  // soulStatsStaleFn (default 90s in the test)
		nil,                                  // clusterH (cluster-view not mounted in the test)
		nil,                                  // runEventsDeps (ADR-068 §A3 — not tested here)
		nil,                                  // logger
	)
}

// TestMetaRoutes_Public_200 — healthz/readyz/docs respond 200 WITHOUT a JWT (public).
// A regression (falling under RequireJWT/RBAC) would give 401/403/a panic on a nil verifier.
func TestMetaRoutes_Public_200(t *testing.T) {
	r := metaRouter(t, nil)
	for _, path := range []string{"/healthz", "/readyz", "/docs"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, http.NoBody) // NO Authorization header
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s WITHOUT JWT = %d, want 200 (public meta route); body=%s",
				path, rec.Code, rec.Body.String())
		}
	}
}

// TestMetaRoutes_OpenAPI_RequiresJWT — guard for mechanism A: /openapi.yaml is BEHIND a JWT.
//   - without Authorization → 401 (the spec is NOT exposed to anonymous callers);
//   - with a valid Bearer → 200, body — the runtime dump of the huma aggregator (3.1).
func TestMetaRoutes_OpenAPI_RequiresJWT(t *testing.T) {
	r := metaRouter(t, metaVerifier(t))

	// (1) Without a token — 401.
	recNoAuth := httptest.NewRecorder()
	reqNoAuth := httptest.NewRequest(http.MethodGet, "/openapi.yaml", http.NoBody)
	r.ServeHTTP(recNoAuth, reqNoAuth)
	if recNoAuth.Code != http.StatusUnauthorized {
		t.Fatalf("GET /openapi.yaml WITHOUT JWT = %d, want 401 (spec behind JWT, mechanism A); body=%s",
			recNoAuth.Code, recNoAuth.Body.String())
	}

	// (2) With a valid token — 200 + application/yaml + huma 3.1 dump.
	recAuth := httptest.NewRecorder()
	reqAuth := httptest.NewRequest(http.MethodGet, "/openapi.yaml", http.NoBody)
	reqAuth.Header.Set("Authorization", "Bearer "+metaValidToken(t))
	r.ServeHTTP(recAuth, reqAuth)

	if recAuth.Code != http.StatusOK {
		t.Fatalf("GET /openapi.yaml with a valid JWT = %d, want 200; body=%s", recAuth.Code, recAuth.Body.String())
	}
	if ct := recAuth.Header().Get("Content-Type"); ct != "application/yaml; charset=utf-8" {
		t.Errorf("Content-Type = %q, want application/yaml; charset=utf-8", ct)
	}
	if recAuth.Body.Len() == 0 {
		t.Fatal("body /openapi.yaml is empty - huma dump not served")
	}
	// huma sorts top-level keys alphabetically — `openapi:` need not come first;
	// we check the version as a substring (3.1 vs the former 3.0.3 hand-written spec).
	if !strings.Contains(recAuth.Body.String(), "openapi: 3.1") {
		t.Error("served spec does not contain `openapi: 3.1` - dump version is not 3.1")
	}
}

// TestMetaRoutes_Docs_ShellAndAssets — /docs serves the HTML viewer shell (a JWT field +
// the embedded RapiDoc web component), and /docs/assets/* — the embedded static assets, BOTH without
// a JWT. The API surface is not exposed in the shell (only the token input field).
func TestMetaRoutes_Docs_ShellAndAssets(t *testing.T) {
	r := metaRouter(t, nil)

	// (1) Shell page.
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
		"Paste your Archon JWT",       // token input field
		"<rapi-doc",                   // RapiDoc web-component
		"/docs/assets/rapidoc-min.js", // reference to the embedded script
		"loadSpec",                    // inline render with an object (not spec-url)
		"allow-advanced-search",       // full-text search — the reason for switching to RapiDoc
		"sessionStorage",              // XSS hygiene (not localStorage)
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("/docs shell does not contain the expected marker %q", marker)
		}
	}
	if strings.Contains(body, "localStorage") {
		t.Error("/docs uses localStorage - XSS hygiene requires sessionStorage")
	}

	// (2) The embedded asset — public, non-empty, correct JS Content-Type.
	recAsset := httptest.NewRecorder()
	r.ServeHTTP(recAsset, httptest.NewRequest(http.MethodGet, "/docs/assets/rapidoc-min.js", http.NoBody))
	if recAsset.Code != http.StatusOK {
		t.Fatalf("GET /docs/assets/rapidoc-min.js = %d, want 200", recAsset.Code)
	}
	if recAsset.Body.Len() == 0 {
		t.Error("embedded asset rapidoc-min.js is empty - go:embed not filled in?")
	}
}

// TestMetaRoutes_OpenAPIJSON_RequiresJWT — guard for mechanism A for the JSON variant
// of the spec (fetched by the /docs viewer, RapiDoc loadSpec):
//   - without Authorization → 401 (the spec is NOT exposed to anonymous callers);
//   - with a valid Bearer → 200, body — the JSON dump of the same huma aggregator (3.1).
func TestMetaRoutes_OpenAPIJSON_RequiresJWT(t *testing.T) {
	r := metaRouter(t, metaVerifier(t))

	// (1) Without a token — 401.
	recNoAuth := httptest.NewRecorder()
	r.ServeHTTP(recNoAuth, httptest.NewRequest(http.MethodGet, "/openapi.json", http.NoBody))
	if recNoAuth.Code != http.StatusUnauthorized {
		t.Fatalf("GET /openapi.json WITHOUT JWT = %d, want 401 (spec behind JWT, mechanism A); body=%s",
			recNoAuth.Code, recNoAuth.Body.String())
	}

	// (2) With a valid token — 200 + application/json + huma 3.1 dump.
	recAuth := httptest.NewRecorder()
	reqAuth := httptest.NewRequest(http.MethodGet, "/openapi.json", http.NoBody)
	reqAuth.Header.Set("Authorization", "Bearer "+metaValidToken(t))
	r.ServeHTTP(recAuth, reqAuth)

	if recAuth.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json with a valid JWT = %d, want 200; body=%s", recAuth.Code, recAuth.Body.String())
	}
	if ct := recAuth.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, want application/json; charset=utf-8", ct)
	}
	if recAuth.Body.Len() == 0 {
		t.Fatal("body /openapi.json is empty - huma dump not served")
	}
	// JSON form 3.1: the version is quoted as the value of the "openapi" key.
	if !strings.Contains(recAuth.Body.String(), `"openapi":"3.1`) {
		t.Error("served JSON spec does not contain `\"openapi\":\"3.1` - dump version is not 3.1")
	}
}

// TestDocsShell_SetApiKey_CleanJWT — contract guard for mechanism A: setApiKey
// receives a CLEAN jwt without a Bearer prefix. RapiDoc itself adds 'Bearer ' for
// the http/bearer scheme (bearerAuth), so a double prefix ('Bearer Bearer …')
// would break "Try It". Goes RED if someone returns a manual Bearer prefix INTO
// setApiKey.
//
// IMPORTANT: we check specifically the setApiKey call, NOT the global absence of
// 'Bearer ' + jwt in docsPage — a Bearer prefix is LEGITIMATE in the fetch header
// of '/openapi.json' (our JS adds it there, RapiDoc has nothing to do with that fetch).
// A global ban would false-positive on that correct fetch string.
func TestDocsShell_SetApiKey_CleanJWT(t *testing.T) {
	if !strings.Contains(docsPage, "setApiKey('bearerAuth', jwt)") {
		t.Error("docsPage does not call setApiKey('bearerAuth', jwt) with a CLEAN jwt - Try It will not get the token")
	}
	// RapiDoc itself prefixes 'Bearer ' for http/bearer; a manual prefix in
	// setApiKey = a double Bearer.
	if strings.Contains(docsPage, "setApiKey('bearerAuth', 'Bearer") {
		t.Error("docsPage passes an already Bearer-prefixed literal to setApiKey - double prefix breaks Try It")
	}
	if strings.Contains(docsPage, "setApiKey('bearerAuth', 'Bearer ' + jwt") {
		t.Error("docsPage concatenates 'Bearer ' + jwt in setApiKey - RapiDoc will add its own prefix, resulting in double Bearer")
	}
}

// TestDocsShell_LoadSpec_Object — contract guard for mechanism A: loadSpec receives
// an OBJECT (resp.json()), not a string. RapiDoc.loadSpec(string) treats the argument
// as a spec URL and fetches it WITHOUT our Bearer → 401. We pass the parsed JSON.
// Goes RED if someone returns loadSpec(string)/resp.text().
func TestDocsShell_LoadSpec_Object(t *testing.T) {
	if !strings.Contains(docsPage, "loadSpec(specObj)") {
		t.Error("docsPage does not call loadSpec(specObj) - RapiDoc.loadSpec(string) treats it as a spec URL (fetch without Bearer -> 401)")
	}
	if !strings.Contains(docsPage, "resp.json()") {
		t.Error("docsPage does not parse resp.json() - loadSpec needs an OBJECT, not the raw spec text")
	}
	if strings.Contains(docsPage, "resp.text()") {
		t.Error("docsPage reads resp.text() - loadSpec(string)=spec-URL -> fetch without Bearer -> 401")
	}
}

// TestServedSpec_YAML_JSON_SameSourceOfTruth — guard for a single source of truth:
// the YAML and JSON served specs are both built from ONE buildFullOpenAPISpec, so
// after normalization (YAML→map, JSON→map) the structures must be identical.
// If the formats diverge (a different aggregator/a manual edit to one of them) —
// reflect.DeepEqual goes red.
func TestServedSpec_YAML_JSON_SameSourceOfTruth(t *testing.T) {
	yamlBytes, err := openAPISpecBytes()
	if err != nil {
		t.Fatalf("openAPISpecBytes: %v", err)
	}
	jsonBytes, err := openAPISpecJSONBytes()
	if err != nil {
		t.Fatalf("openAPISpecJSONBytes: %v", err)
	}

	// We parse both into the same Go form (map[string]any) so the comparison does not
	// depend on textual differences between YAML vs JSON (indentation, quoting, order) —
	// only on STRUCTURE. yaml.v3 into any gives a map[string]interface{} just like
	// encoding/json, so DeepEqual is comparable. We normalize numbers (JSON gives
	// float64, YAML — int) before comparing.
	var fromYAML map[string]any
	if err := yaml.Unmarshal(yamlBytes, &fromYAML); err != nil {
		t.Fatalf("parsing YAML spec: %v", err)
	}
	var fromJSON map[string]any
	if err := json.Unmarshal(jsonBytes, &fromJSON); err != nil {
		t.Fatalf("parsing JSON spec: %v", err)
	}

	normalizeNumbers(fromYAML)
	normalizeNumbers(fromJSON)

	if !reflect.DeepEqual(fromYAML, fromJSON) {
		t.Error("YAML and JSON served specs diverge structurally - single source-of-truth violated (buildFullOpenAPISpec)")
	}
}

// normalizeNumbers converts all numeric values of the tree to float64. yaml.v3
// parses integers as int, encoding/json — as float64; without normalizing,
// DeepEqual would false-positive on numbers that are equal in value (for example,
// on minimum/maximum in the pagination schemas).
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
