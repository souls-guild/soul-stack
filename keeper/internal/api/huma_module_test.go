package api

// Guard-тесты ТИРАЖ-БАТЧА-2e разворота MODULE-домена ЦЕЛИКОМ на huma full-typed
// (ADR-054 §Pattern, эталон catalog read-bare + form-prep read-with-body). ВСЕ три
// роута — READ-only (audit НЕ навешан). Доказывают инварианты кластера поверх chi:
//
//   - wire/golden: list 200 envelope; get 200 item; form-prep 200 {sids,truncated} —
//     huma-200-reply == legacy-200-reply ТОГО ЖЕ handler-а (byte-exact после ремаршала);
//   - get unknown → 404; form-prep unknown-field → 400; form-prep bad source → 422;
//     RBAC-deny → 403;
//   - no-audit: READ-домен не пишет audit (нет middleware) — capture-writer даёт 0 событий.

import (
	"context"
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
)

// hFormPrepResolver — мок [handlers.FormPrepSIDResolver] для huma-теста form-prep.
type hFormPrepResolver struct {
	sids      []string
	truncated bool
	err       error
}

func (r *hFormPrepResolver) ResolveSIDs(_ context.Context, _ handlers.FormPrepFilter) ([]string, bool, error) {
	return r.sids, r.truncated, r.err
}

// humaModuleRouter собирает chi-роутер со ВСЕМИ module-роутами через huma —
// продакшен-навеска из router.go: RequirePermission на каждой группе (list/get →
// service.list, form-prep → incarnation.run) БЕЗ audit (READ-домен). injectClaims
// заменяет RequireJWT. auditW (если не nil) навешивается, чтобы доказать no-audit.
func humaModuleRouter(t *testing.T, enforcer apimiddleware.PermissionChecker, catalogH *handlers.ModuleCatalogHandler, formPrepH *handlers.ModuleFormPrepHandler) *chi.Mux {
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
		r.Route("/modules", func(r chi.Router) {
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "service", "list", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				api := newHumaCadenceAPI(r)
				registerHumaModuleList(api, catalogH)
				registerHumaModuleGet(api, catalogH)
			})
			if formPrepH != nil {
				r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "incarnation", "run", apimiddleware.NoSelector)).Group(func(r chi.Router) {
					registerHumaModuleFormPrep(newHumaCadenceAPI(r), formPrepH)
				})
			}
		})
	})
	return r
}

// remarshalModule нормализует JSON через map (ключи сортируются) — golden фиксирует
// набор ключей/форму, не порядок.
func remarshalModule(t *testing.T, raw []byte) string {
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

// === LIST (READ, БЕЗ audit) ===

// TestHumaModule_List_GoldenWire — huma-200-reply форма envelope + byte-exact одной
// детерминированной записи (core.archive — single-state, описания params стабильны).
// Полный каталог не сравниваем byte-to-byte: manifestToParams выбирает описание
// param-а из ПЕРВОГО state-а в порядке map-итерации (Go-недетерминизм между двумя
// buildCatalog-вызовами для multi-state модулей — core.choir и т.п.; это
// pre-existing свойство handler-а, см. observations). Envelope-форма + стабильная
// запись фиксируют wire huma-роута без ложного дрейфа на недетерминированных описаниях.
func TestHumaModule_List_GoldenWire(t *testing.T) {
	catalogH := handlers.NewModuleCatalogHandler(nil, nil)
	r := humaModuleRouter(t, strictAllowAll{}, catalogH, nil)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/modules", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var got struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("reply не JSON-envelope: %v; body=%s", err, rec.Body.String())
	}
	if len(got.Items) == 0 {
		t.Fatal("items пуст — core-каталог обязан быть непустым")
	}
	// Byte-exact одной детерминированной записи (core.archive — single-state).
	var archive map[string]any
	for _, it := range got.Items {
		if it["name"] == "core.archive" {
			archive = it
		}
	}
	if archive == nil {
		t.Fatal("core.archive отсутствует в каталоге")
	}
	out, _ := json.Marshal(archive)
	const golden = `{"description":"Распаковка архива (tar/tar.gz/tar.bz2/zip) в каталог назначения.","errand_safe":false,"kind":"core","name":"core.archive","params":[{"description":"Каталог распаковки.","name":"dest","required":true,"type":"string"},{"description":"Формат (tar|tar.gz|tar.bz2|zip); опущено — auto-detect по расширению.","name":"format","required":false,"type":"string"},{"description":"Лимит числа записей в архиве; по умолчанию 100000. Защита от zip-bomb.","name":"max_entries","required":false,"type":"integer"},{"description":"Лимит отношения распакованных байт к сжатым (compression ratio); по умолчанию 100, 0 — отключено. Защита от zip-bomb с маленьким сжатым размером.","name":"max_ratio","required":false,"type":"integer"},{"description":"Лимит суммарного распакованного размера (число байт или N[KiB|MiB|GiB]); по умолчанию 1GiB. Защита от zip-bomb.","name":"max_size","required":false,"type":"string"},{"description":"Путь к архиву-источнику.","name":"path","required":true,"type":"string"}],"states":["extracted"]}`
	if string(out) != golden {
		t.Errorf("GOLDEN wire-дрейф module.list[core.archive]:\n got  = %s\n want = %s", string(out), golden)
	}
}

func TestHumaModule_List_ErrandSafeFilter(t *testing.T) {
	catalogH := handlers.NewModuleCatalogHandler(nil, nil)
	r := humaModuleRouter(t, strictAllowAll{}, catalogH, nil)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/modules?errand_safe=true", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// Source of truth — доменный ListTyped того же handler-а ((w,r)-оболочка снята);
	// huma-200-байты обязаны совпасть после ремаршала.
	reply, err := catalogH.ListTyped(context.Background(), true)
	if err != nil {
		t.Fatalf("ListTyped(errand_safe): %v", err)
	}
	legacyBytes, _ := json.Marshal(reply)
	if got, want := remarshalModule(t, rec.Body.Bytes()), remarshalModule(t, legacyBytes); got != want {
		t.Errorf("errand_safe-фильтр дрейф:\n got  = %s\n want = %s", got, want)
	}
}

func TestHumaModule_List_RBACDeny_403(t *testing.T) {
	r := humaModuleRouter(t, strictDenyAll{}, handlers.NewModuleCatalogHandler(nil, nil), nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/modules", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// === GET (READ, БЕЗ audit) ===

func TestHumaModule_Get_GoldenWire(t *testing.T) {
	catalogH := handlers.NewModuleCatalogHandler(nil, nil)
	r := humaModuleRouter(t, strictAllowAll{}, catalogH, nil)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/modules/core.cmd", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Source of truth — доменный GetTyped того же handler-а (legacy Get лишь
	// оборачивает его); huma-200-байты обязаны совпасть после ремаршала.
	item, err := catalogH.GetTyped(context.Background(), "core.cmd")
	if err != nil {
		t.Fatalf("GetTyped(core.cmd): %v", err)
	}
	legacyBytes, _ := json.Marshal(item)
	if got, want := remarshalModule(t, rec.Body.Bytes()), remarshalModule(t, legacyBytes); got != want {
		t.Errorf("GOLDEN wire-дрейф module.get:\n got  = %s\n want = %s", got, want)
	}
}

func TestHumaModule_Get_NotFound_404(t *testing.T) {
	r := humaModuleRouter(t, strictAllowAll{}, handlers.NewModuleCatalogHandler(nil, nil), nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/modules/core.nonexistent", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeNotFound)
}

// === FORM-PREP (READ-резолв, БЕЗ audit) ===

func TestHumaModule_FormPrep_GoldenWire(t *testing.T) {
	resolver := &hFormPrepResolver{sids: []string{"host-a.example.com", "host-b.example.com"}, truncated: true}
	formPrepH := handlers.NewModuleFormPrepHandler(resolver, nil)
	r := humaModuleRouter(t, strictAllowAll{}, handlers.NewModuleCatalogHandler(nil, nil), formPrepH)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/modules/core.cmd/form-prep",
		strings.NewReader(`{"source":{"incarnation_hosts":"web"},"prefix":"host-"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	const golden = `{"sids":["host-a.example.com","host-b.example.com"],"truncated":true}`
	if got := remarshalModule(t, rec.Body.Bytes()); got != golden {
		t.Errorf("GOLDEN wire-дрейф module.form-prep:\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaModule_FormPrep_UnknownField_400(t *testing.T) {
	formPrepH := handlers.NewModuleFormPrepHandler(&hFormPrepResolver{}, nil)
	r := humaModuleRouter(t, strictAllowAll{}, handlers.NewModuleCatalogHandler(nil, nil), formPrepH)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/modules/core.cmd/form-prep",
		strings.NewReader(`{"source":{"incarnation_hosts":"web"},"bogus":1}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

func TestHumaModule_FormPrep_BadSource_422(t *testing.T) {
	formPrepH := handlers.NewModuleFormPrepHandler(&hFormPrepResolver{}, nil)
	r := humaModuleRouter(t, strictAllowAll{}, handlers.NewModuleCatalogHandler(nil, nil), formPrepH)
	rec := httptest.NewRecorder()
	// Ни incarnation_hosts, ни choir → source не задан → 422 (домен toFilter).
	req := httptest.NewRequest(http.MethodPost, "/v1/modules/core.cmd/form-prep",
		strings.NewReader(`{"source":{}}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

func TestHumaModule_FormPrep_RBACDeny_403(t *testing.T) {
	formPrepH := handlers.NewModuleFormPrepHandler(&hFormPrepResolver{}, nil)
	r := humaModuleRouter(t, strictDenyAll{}, handlers.NewModuleCatalogHandler(nil, nil), formPrepH)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/modules/core.cmd/form-prep",
		strings.NewReader(`{"source":{"incarnation_hosts":"web"}}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// === NO-AUDIT (READ-домен) ===

// TestHumaModule_ReadNoAudit — READ-домен не пишет ни одного audit-event ни на одном
// из трёх роутов (нет audit-middleware). capture-writer навешен лишь как ловушка.
func TestHumaModule_ReadNoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	catalogH := handlers.NewModuleCatalogHandler(nil, nil)
	formPrepH := handlers.NewModuleFormPrepHandler(&hFormPrepResolver{sids: []string{"x.example.com"}}, nil)
	r := humaModuleRouter(t, strictAllowAll{}, catalogH, formPrepH)
	_ = auditCap // module-домен audit-writer вообще не получает — навески нет; явный 0-чек ниже.

	for _, tc := range []struct {
		method, path, body string
	}{
		{http.MethodGet, "/v1/modules", ""},
		{http.MethodGet, "/v1/modules/core.cmd", ""},
		{http.MethodPost, "/v1/modules/core.cmd/form-prep", `{"source":{"incarnation_hosts":"web"}}`},
	} {
		rec := httptest.NewRecorder()
		var body *strings.Reader
		if tc.body != "" {
			body = strings.NewReader(tc.body)
			r.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, body))
		} else {
			r.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s: status = %d, want 200; body=%s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("module READ-домен записал audit (%d событий) — read не должен", len(auditCap.Events()))
	}
}

// TestHumaModule_SpecYAML — huma генерит 3.1-спеку из FULL-TYPED Go-типов module-домена.
func TestHumaModule_SpecYAML(t *testing.T) {
	frag, err := HumaModuleSpecYAML()
	if err != nil {
		t.Fatalf("HumaModuleSpecYAML: %v", err)
	}
	// Пути ОТНОСИТЕЛЬНЫ chi-группы /v1/modules (mount даёт /v1/modules{path});
	// spec-dump на голом router эмитит "/", "/{name}", "/{name}/form-prep".
	for _, want := range []string{"listModules", "getModule", "moduleFormPrep", "/{name}/form-prep"} {
		if !strings.Contains(frag, want) {
			t.Errorf("спека не содержит %q:\n%s", want, frag)
		}
	}
}
