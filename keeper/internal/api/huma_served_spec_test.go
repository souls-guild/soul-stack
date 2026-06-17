// Гейт served-механизма GET /openapi.yaml. Доказывает, что servedOpenAPIHandler
// отдаёт runtime-дамп huma-агрегатора (3.1) — а не embed committed-рукопись 3.0.3.
//
// ВНИМАНИЕ: тест зовёт servedOpenAPIHandler НАПРЯМУЮ (мимо router-а), поэтому
// JWT-гейт здесь не участвует — он навешан middleware-ом RequireJWT на router-
// уровне (механизм A, ADR-054). Auth-гейт /openapi.yaml проверяет
// meta_routes_test (TestMetaRoutes_OpenAPI_RequiresJWT: 401 без / 200 с JWT).
package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// TestServedOpenAPI_HandlerHuma31 — гейт самого servedOpenAPIHandler (мимо
// router-а, без auth-цепочки):
//   - 200 (handler сам auth не проверяет — JWT-гейт на router-уровне);
//   - Content-Type application/yaml;
//   - тело — OpenAPI 3.1.0 (huma-генерат, не 3.0.3-рукопись);
//   - тело несёт контрактные схемы (IncarnationCreateRequest / SoulListReply /
//     SoulprintFacts) — доказательство, что served-дамп = полная агрегат-спека.
func TestServedOpenAPI_HandlerHuma31(t *testing.T) {
	rec := httptest.NewRecorder()
	// Зовём handler напрямую — JWT-гейт на router-уровне (механизм A), не здесь.
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

	// huma сортирует top-level YAML-ключи (openapi не обязан быть первым) —
	// проверяем поле через парс, не префиксом строки.
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

// TestServedOpenAPI_CacheStable — кеш-механизм: повторные вызовы отдают идентичный
// буфер (sync.Once собирает один раз; дамп неизменен за процесс).
func TestServedOpenAPI_CacheStable(t *testing.T) {
	first, err := openAPISpecBytes()
	if err != nil {
		t.Fatalf("openAPISpecBytes: %v", err)
	}
	second, err := openAPISpecBytes()
	if err != nil {
		t.Fatalf("openAPISpecBytes (2): %v", err)
	}
	// Тот же backing-массив (кеш, не пересборка).
	if len(first) == 0 || &first[0] != &second[0] {
		t.Error("повторный вызов вернул иной буфер — кеш не сработал (пересборка на каждый запрос?)")
	}
}
