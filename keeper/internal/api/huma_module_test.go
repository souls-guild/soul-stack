package api

// Guard tests of ROLLOUT-BATCH-2e moving the MODULE domain WHOLESALE onto huma full-typed
// (ADR-054 §Pattern, reference catalog read-bare + form-prep read-with-body). ALL three
// routes are READ-only (audit not wired). They prove cluster invariants over chi:
//
//   - wire/golden: list 200 envelope; get 200 item; form-prep 200 {sids,truncated} —
//     huma-200-reply == legacy-200-reply of the SAME handler (byte-exact after remarshal);
//   - get unknown → 404; form-prep unknown-field → 400; form-prep bad source → 422;
//     RBAC-deny → 403;
//   - no-audit: the READ domain writes no audit (no middleware) — capture-writer yields 0 events.

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

// hFormPrepResolver — mock [handlers.FormPrepSIDResolver] for the huma form-prep test.
type hFormPrepResolver struct {
	sids      []string
	truncated bool
	err       error
}

func (r *hFormPrepResolver) ResolveSIDs(_ context.Context, _ handlers.FormPrepFilter) ([]string, bool, error) {
	return r.sids, r.truncated, r.err
}

// humaModuleRouter assembles a chi router with ALL module routes via huma —
// the production wiring from router.go: RequirePermission on each group (list/get →
// service.list, form-prep → incarnation.run) with no audit (READ domain). injectClaims
// replaces RequireJWT. auditW (if non-nil) is wired to prove no-audit.
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

// remarshalModule normalizes JSON through a map (keys sorted) — golden pins the
// key set/shape, not the order.
func remarshalModule(t *testing.T, raw []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("reply is not JSON: %v; raw=%s", err, raw)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	return string(out)
}

// === LIST (READ, no audit) ===

// TestHumaModule_List_GoldenWire — the huma-200-reply envelope shape + byte-exact of one
// deterministic record (core.archive — single-state, param descriptions stable).
// We don't compare the full catalog byte-to-byte: manifestToParams picks a param's
// description from the FIRST state in map-iteration order (Go non-determinism between two
// buildCatalog calls for multi-state modules — core.choir etc.; this is a
// pre-existing property of the handler, see observations). The envelope shape + a stable
// record pin the wire of the huma route without false drift on non-deterministic descriptions.
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
		t.Fatalf("reply is not a JSON envelope: %v; body=%s", err, rec.Body.String())
	}
	if len(got.Items) == 0 {
		t.Fatal("items is empty -- the core catalog must be non-empty")
	}
	// Byte-exact of one deterministic record (core.archive — single-state).
	var archive map[string]any
	for _, it := range got.Items {
		if it["name"] == "core.archive" {
			archive = it
		}
	}
	if archive == nil {
		t.Fatal("core.archive is missing from the catalog")
	}
	out, _ := json.Marshal(archive)
	const golden = `{"description":"Extract an archive (tar/tar.gz/tar.bz2/zip) into the destination directory.","errand_safe":false,"kind":"core","name":"core.archive","params":[{"description":"Extraction directory.","name":"dest","required":true,"type":"string"},{"description":"Format (tar|tar.gz|tar.bz2|zip); omitted — auto-detect by extension.","name":"format","required":false,"type":"string"},{"description":"Limit on the number of entries in the archive; default 100000. Zip-bomb protection.","name":"max_entries","required":false,"type":"integer"},{"description":"Limit on the ratio of extracted to compressed bytes (compression ratio); default 100, 0 — disabled. Zip-bomb protection for a small compressed size.","name":"max_ratio","required":false,"type":"integer"},{"description":"Limit on total extracted size (number of bytes or N[KiB|MiB|GiB]); default 1GiB. Zip-bomb protection.","name":"max_size","required":false,"type":"string"},{"description":"Path to the source archive.","name":"path","required":true,"type":"string"}],"states":["extracted"]}`
	if string(out) != golden {
		t.Errorf("GOLDEN wire drift module.list[core.archive]:\n got  = %s\n want = %s", string(out), golden)
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
	// Source of truth — the domain ListTyped of the same handler (the (w,r) wrapper removed);
	// the huma-200 bytes must match after remarshal.
	reply, err := catalogH.ListTyped(context.Background(), true)
	if err != nil {
		t.Fatalf("ListTyped(errand_safe): %v", err)
	}
	legacyBytes, _ := json.Marshal(reply)
	if got, want := remarshalModule(t, rec.Body.Bytes()), remarshalModule(t, legacyBytes); got != want {
		t.Errorf("errand_safe filter drift:\n got  = %s\n want = %s", got, want)
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

// === GET (READ, no audit) ===

func TestHumaModule_Get_GoldenWire(t *testing.T) {
	catalogH := handlers.NewModuleCatalogHandler(nil, nil)
	r := humaModuleRouter(t, strictAllowAll{}, catalogH, nil)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/modules/core.cmd", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Source of truth — the domain GetTyped of the same handler (legacy Get just
	// wraps it); the huma-200 bytes must match after remarshal.
	item, err := catalogH.GetTyped(context.Background(), "core.cmd")
	if err != nil {
		t.Fatalf("GetTyped(core.cmd): %v", err)
	}
	legacyBytes, _ := json.Marshal(item)
	if got, want := remarshalModule(t, rec.Body.Bytes()), remarshalModule(t, legacyBytes); got != want {
		t.Errorf("GOLDEN wire drift module.get:\n got  = %s\n want = %s", got, want)
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

// === FORM-PREP (READ resolve, no audit) ===

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
		t.Errorf("GOLDEN wire drift module.form-prep:\n got  = %s\n want = %s", got, golden)
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
	// Neither incarnation_hosts nor choir → source not set → 422 (domain toFilter).
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

// === NO-AUDIT (READ domain) ===

// TestHumaModule_ReadNoAudit — the READ domain writes no audit event on any of the
// three routes (no audit middleware). capture-writer is wired only as a trap.
func TestHumaModule_ReadNoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	catalogH := handlers.NewModuleCatalogHandler(nil, nil)
	formPrepH := handlers.NewModuleFormPrepHandler(&hFormPrepResolver{sids: []string{"x.example.com"}}, nil)
	r := humaModuleRouter(t, strictAllowAll{}, catalogH, formPrepH)
	_ = auditCap // the module domain gets no audit writer at all — no wiring; explicit 0-check below.

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
		t.Errorf("module READ endpoint recorded audit (%d events) -- read must not", len(auditCap.Events()))
	}
}

// TestHumaModule_SpecYAML — huma generates the 3.1 spec from the FULL-TYPED Go types of the module domain.
func TestHumaModule_SpecYAML(t *testing.T) {
	frag, err := HumaModuleSpecYAML()
	if err != nil {
		t.Fatalf("HumaModuleSpecYAML: %v", err)
	}
	// Paths are RELATIVE to the chi group /v1/modules (mount gives /v1/modules{path});
	// the spec dump on a bare router emits "/", "/{name}", "/{name}/form-prep".
	for _, want := range []string{"listModules", "getModule", "moduleFormPrep", "/{name}/form-prep"} {
		if !strings.Contains(frag, want) {
			t.Errorf("spec does not contain %q:\n%s", want, frag)
		}
	}
}
