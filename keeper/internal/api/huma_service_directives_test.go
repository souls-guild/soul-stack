package api

// Guard-тесты GET /v1/services/{name}/directives (NIM-76): доставка каталога
// директив redis.conf + ETag/Cache-Control immutable + version-сужение + 304.
// Полная huma-навеска (RequirePermission service.list + huma-операция),
// injectClaims заменяет RequireJWT.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
)

const hDirSHA1 = "a1b2c3d4e5f600112233445566778899aabbccdd"

// hDirSHARef — ref в immutable-форме (полный 40-hex commit SHA) для теста
// Cache-Control immutable. Отличается от snapshot-SHA1 (ETag): ref — то, что
// оператор запинил (?ref=), а SHA1 — content-hash разрешённого снапшота.
const hDirSHARef = "0123456789abcdef0123456789abcdef01234567"

// hDirLister — стаб DirectiveLister, отдающий фиксированный каталог + SHA1.
type hDirLister struct{ catalog *artifact.DirectiveCatalog }

func (l hDirLister) ListDirectives(context.Context, string, string, string) (*artifact.DirectiveCatalog, error) {
	return l.catalog, nil
}

// hDirErrLister — стаб, возвращающий ошибку git-loader-а (502-tier).
type hDirErrLister struct{}

func (hDirErrLister) ListDirectives(context.Context, string, string, string) (*artifact.DirectiveCatalog, error) {
	return nil, &hSvcErr{"git clone failed: connection refused"}
}

func hDirFullCatalog() *artifact.DirectiveCatalog {
	return &artifact.DirectiveCatalog{
		SHA1: hDirSHA1,
		Directives: map[string][]string{
			"6.2": {"appendonly", "maxmemory"},
			"7.4": {"appendonly", "io-threads", "maxmemory"},
			"8.2": {"appendonly", "io-threads", "maxmemory", "maxmemory-clients"},
		},
	}
}

// directivesTestRouter — минимальный роутер с /directives-роутом (parity
// humaServiceRouter, но только этот sub-read). lister=nil → 500 «not configured».
func directivesTestRouter(t *testing.T, lister handlers.ServiceDirectivesLister) *chi.Mux {
	t.Helper()
	installHumaErrorOverride()
	svc, err := serviceregistry.NewService(serviceregistry.ServiceDeps{Pool: &hSvcPool{getValues: svcGetRow()}})
	if err != nil {
		t.Fatalf("serviceregistry.NewService: %v", err)
	}
	serviceH := handlers.NewServiceHandler(svc, nil, nil, nil, nil, lister, nil)

	r := chi.NewRouter()
	injectClaims := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := apimiddleware.InjectClaimsForTest(req.Context(), &keeperjwt.Claims{Subject: "archon-alice"})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
	r.Route("/v1/services", func(r chi.Router) {
		r.With(injectClaims, apimiddleware.RequirePermission(strictAllowAll{}, "service", "list", apimiddleware.NoSelector)).Group(func(r chi.Router) {
			registerHumaServiceDirectives(newHumaCadenceAPI(r), serviceH)
		})
	})
	return r
}

type hDirBody struct {
	Service    string              `json:"service"`
	Ref        string              `json:"ref"`
	SHA1       string              `json:"sha1"`
	Directives map[string][]string `json:"directives"`
}

// TestDirectives_FullCatalog_ETag — guard #4 (тело+ETag): 200 + весь каталог; ETag
// == "<sha1>" (snapshot SHA1); тело несёт service/ref/sha1/directives. Дефолтный ref
// из реестра — "v1.0.0" (тег-имя → mutable) → Cache-Control no-cache (ревалидация).
func TestDirectives_FullCatalog_ETag(t *testing.T) {
	r := directivesTestRouter(t, hDirLister{catalog: hDirFullCatalog()})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/services/web/directives", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, want := rec.Header().Get("ETag"), `"`+hDirSHA1+`"`; got != want {
		t.Errorf("ETag = %q, want %q (snapshot SHA1)", got, want)
	}
	if got, want := rec.Header().Get("Cache-Control"), "no-cache"; got != want {
		t.Errorf("Cache-Control = %q, want %q (тег-ref v1.0.0 mutable)", got, want)
	}
	var body hDirBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal body: %v; raw=%s", err, rec.Body.String())
	}
	if body.Service != "web" || body.Ref != "v1.0.0" || body.SHA1 != hDirSHA1 {
		t.Errorf("body meta = %+v, want service=web ref=v1.0.0 sha1=%s", body, hDirSHA1)
	}
	if len(body.Directives) != 3 {
		t.Errorf("directives-серий = %d, want 3", len(body.Directives))
	}
	if !hDirContains(body.Directives["8.2"], "maxmemory") {
		t.Errorf("8.2 не содержит maxmemory: %v", body.Directives["8.2"])
	}
}

// TestDirectives_CacheControl_ImmutableForSHARef — guard #4 (immutable-ветка):
// pinned commit-SHA ref (?ref=<40hex>) → Cache-Control immutable+год (содержимое
// неизменно), ETag присутствует.
func TestDirectives_CacheControl_ImmutableForSHARef(t *testing.T) {
	r := directivesTestRouter(t, hDirLister{catalog: hDirFullCatalog()})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/services/web/directives?ref="+hDirSHARef, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, want := rec.Header().Get("Cache-Control"), "public, max-age=31536000, immutable"; got != want {
		t.Errorf("Cache-Control = %q, want %q (pinned SHA-ref immutable)", got, want)
	}
	if got := rec.Header().Get("ETag"); got != `"`+hDirSHA1+`"` {
		t.Errorf("ETag = %q, want snapshot SHA1", got)
	}
}

// TestDirectives_CacheControl_RevalidateForBranchRef — mutable ветка-ref (?ref=main)
// → Cache-Control no-cache (invalidateDirectives не застревает за годовым кешем).
func TestDirectives_CacheControl_RevalidateForBranchRef(t *testing.T) {
	r := directivesTestRouter(t, hDirLister{catalog: hDirFullCatalog()})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/services/web/directives?ref=main", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, want := rec.Header().Get("Cache-Control"), "no-cache"; got != want {
		t.Errorf("Cache-Control = %q, want %q (ветка-ref mutable)", got, want)
	}
}

// TestDirectives_VersionNarrows — ?version=8.2.2 сужает тело до серии 8.2 (handler
// фильтрует полный каталог lister-а).
func TestDirectives_VersionNarrows(t *testing.T) {
	r := directivesTestRouter(t, hDirLister{catalog: hDirFullCatalog()})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/services/web/directives?version=8.2.2", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body hDirBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Directives) != 1 || body.Directives["8.2"] == nil {
		t.Fatalf("version=8.2.2 → серии %v, want ровно {8.2}", hDirKeys(body.Directives))
	}
	// ETag остаётся snapshot SHA1 (per-URL ресурс, тело меняется, ETag = версия снапшота).
	if got := rec.Header().Get("ETag"); got != `"`+hDirSHA1+`"` {
		t.Errorf("ETag = %q, want snapshot SHA1", got)
	}
}

// TestDirectives_EmptyCatalog_200 — guard #3 (endpoint-половина): сервис без каталога
// → directives:{} + HTTP 200 (НЕ 404).
func TestDirectives_EmptyCatalog_200(t *testing.T) {
	r := directivesTestRouter(t, hDirLister{catalog: &artifact.DirectiveCatalog{SHA1: hDirSHA1, Directives: map[string][]string{}}})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/services/web/directives", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (не 404); body=%s", rec.Code, rec.Body.String())
	}
	// directives обязан присутствовать как {} (не null) для мягкой деградации фронта.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(raw["directives"]) != "{}" {
		t.Errorf("directives = %s, want {}", raw["directives"])
	}
}

// TestDirectives_IfNoneMatch_304 — conditional GET: If-None-Match совпал с ETag →
// 304 без тела, ETag present.
func TestDirectives_IfNoneMatch_304(t *testing.T) {
	r := directivesTestRouter(t, hDirLister{catalog: hDirFullCatalog()})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/services/web/directives", nil)
	req.Header.Set("If-None-Match", `"`+hDirSHA1+`"`)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("304 тело непустое: %q", rec.Body.String())
	}
	if got := rec.Header().Get("ETag"); got != `"`+hDirSHA1+`"` {
		t.Errorf("ETag на 304 = %q, want snapshot SHA1", got)
	}
}

// TestDirectives_IfNoneMatch_Stale200 — If-None-Match с ЧУЖИМ ETag → полный 200.
func TestDirectives_IfNoneMatch_Stale200(t *testing.T) {
	r := directivesTestRouter(t, hDirLister{catalog: hDirFullCatalog()})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/services/web/directives", nil)
	req.Header.Set("If-None-Match", `"stale-sha1"`)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (ETag не совпал)", rec.Code)
	}
}

// TestDirectives_NilLister_500 — lister не сконфигурирован → 500 «not configured».
func TestDirectives_NilLister_500(t *testing.T) {
	r := directivesTestRouter(t, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/services/web/directives", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// TestDirectives_LoaderError_502 — ошибка git-loader-а → 502 Bad Gateway.
func TestDirectives_LoaderError_502(t *testing.T) {
	r := directivesTestRouter(t, hDirErrLister{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/services/web/directives", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
}

func hDirContains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func hDirKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
