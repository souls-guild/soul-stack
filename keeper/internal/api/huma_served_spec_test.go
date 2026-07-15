// Gate for the served mechanism of GET /openapi.yaml. Proves that servedOpenAPIHandler
// serves the runtime dump of the huma aggregator (3.1) — not the embedded committed hand-written 3.0.3.
//
// NOTE: the test calls servedOpenAPIHandler DIRECTLY (bypassing the router), so
// the JWT gate is not involved here — it is attached by the RequireJWT middleware at the router
// level (mechanism A, ADR-054). The /openapi.yaml auth gate is checked by
// meta_routes_test (TestMetaRoutes_OpenAPI_RequiresJWT: 401 without / 200 with JWT).
package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// TestServedOpenAPI_HandlerHuma31 — a gate for servedOpenAPIHandler itself (bypassing
// the router, without the auth chain):
//   - 200 (the handler does not check auth itself — the JWT gate is at the router level);
//   - Content-Type application/yaml;
//   - the body — OpenAPI 3.1.0 (a huma generate, not the 3.0.3 hand-written);
//   - the body carries the contract schemas (IncarnationCreateRequest / SoulListReply /
//     SoulprintFacts) — proof that the served dump = the full aggregate spec.
func TestServedOpenAPI_HandlerHuma31(t *testing.T) {
	rec := httptest.NewRecorder()
	// Call the handler directly — the JWT gate is at the router level (mechanism A), not here.
	req := httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil)
	servedOpenAPIHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (handler сам auth не проверяет)", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != contentTypeOpenAPI {
		t.Errorf("Content-Type = %q, want %q", ct, contentTypeOpenAPI)
	}
	if cl := rec.Header().Get("Content-Length"); cl == "" {
		t.Error("Content-Length не выставлен")
	}

	body, _ := io.ReadAll(rec.Body)
	s := string(body)

	// huma sorts top-level YAML keys (openapi need not be first) —
	// check the field via parsing, not by a line prefix.
	var doc map[string]any
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("served-спека не парсится как YAML: %v", err)
	}
	if v, _ := doc["openapi"].(string); v != "3.1.0" {
		t.Errorf("openapi = %q, want 3.1.0 (huma-генерат, не 3.0.3-рукопись)", v)
	}

	for _, schema := range []string{"IncarnationCreateRequest", "SoulListReply", "SoulprintFacts"} {
		if !strings.Contains(s, schema) {
			t.Errorf("served-спека не содержит контрактную схему %q — агрегат-дамп неполон?", schema)
		}
	}
}

// TestServedOpenAPI_CacheStable — the cache mechanism: repeated calls return an identical
// buffer (sync.Once builds once; the dump is unchanged for the process lifetime).
func TestServedOpenAPI_CacheStable(t *testing.T) {
	first, err := openAPISpecBytes()
	if err != nil {
		t.Fatalf("openAPISpecBytes: %v", err)
	}
	second, err := openAPISpecBytes()
	if err != nil {
		t.Fatalf("openAPISpecBytes (2): %v", err)
	}
	// The same backing array (cache, not a rebuild).
	if len(first) == 0 || &first[0] != &second[0] {
		t.Error("повторный вызов вернул иной буфер — кеш не сработал (пересборка на каждый запрос?)")
	}
}
