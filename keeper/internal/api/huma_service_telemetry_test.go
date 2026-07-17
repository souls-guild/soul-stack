package api

// Guard tests for GET /v1/services/{name}/telemetry (NIM-87): delivery of the default
// (per-service, without essence) host-vitals telemetry config + known_collectors for the UI
// + ETag/Cache-Control immutable + 304. Full huma wiring (RequirePermission
// service.list + huma operation), injectClaims replaces RequireJWT.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/config"
)

const hTelSHA1 = "b2c3d4e5f6a700112233445566778899aabbccde"

// hTelSHARef — a ref in immutable form (full 40-hex commit SHA) for the
// Cache-Control immutable test (parity with hDirSHARef).
const hTelSHARef = "89abcdef0123456789abcdef0123456789abcdef"

// hTelLister — a ServiceTelemetryLister stub returning a fixed catalog.
type hTelLister struct {
	catalog *serviceregistry.TelemetryCatalog
}

func (l hTelLister) ListServiceTelemetry(context.Context, string, string, string) (*serviceregistry.TelemetryCatalog, error) {
	return l.catalog, nil
}

// hTelErrLister — a stub returning a git-loader error (502-tier).
type hTelErrLister struct{}

func (hTelErrLister) ListServiceTelemetry(context.Context, string, string, string) (*serviceregistry.TelemetryCatalog, error) {
	return nil, &hSvcErr{"git clone failed: connection refused"}
}

func hTelCatalog(collectors []string) *serviceregistry.TelemetryCatalog {
	return &serviceregistry.TelemetryCatalog{
		SHA1:      hTelSHA1,
		Telemetry: &keeperv1.TelemetryConfig{Enabled: true, IntervalSec: 30, Collectors: collectors},
	}
}

// telemetryTestRouter — a minimal router with the /telemetry route (parity with
// directivesTestRouter). lister=nil → 500 "not configured".
func telemetryTestRouter(t *testing.T, lister handlers.ServiceTelemetryLister) *chi.Mux {
	t.Helper()
	installHumaErrorOverride()
	svc, err := serviceregistry.NewService(serviceregistry.ServiceDeps{Pool: &hSvcPool{getValues: svcGetRow()}})
	if err != nil {
		t.Fatalf("serviceregistry.NewService: %v", err)
	}
	serviceH := handlers.NewServiceHandler(svc, nil, nil, nil, nil, nil, lister, nil)

	r := chi.NewRouter()
	injectClaims := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := apimiddleware.InjectClaimsForTest(req.Context(), &keeperjwt.Claims{Subject: "archon-alice"})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
	r.Route("/v1/services", func(r chi.Router) {
		r.With(injectClaims, apimiddleware.RequirePermission(strictAllowAll{}, "service", "list", apimiddleware.NoSelector)).Group(func(r chi.Router) {
			registerHumaServiceTelemetry(newHumaCadenceAPI(r), serviceH)
		})
	})
	return r
}

type hTelBody struct {
	Service         string   `json:"service"`
	Ref             string   `json:"ref"`
	SHA1            string   `json:"sha1"`
	Enabled         bool     `json:"enabled"`
	IntervalSec     int32    `json:"interval_sec"`
	Collectors      []string `json:"collectors"`
	KnownCollectors []string `json:"known_collectors"`
}

// TestServiceTelemetry_FullConfig_ETag — 200 + config + known_collectors; ETag ==
// "<sha1>"; tag-ref v1.0.0 mutable → Cache-Control no-cache.
func TestServiceTelemetry_FullConfig_ETag(t *testing.T) {
	r := telemetryTestRouter(t, hTelLister{catalog: hTelCatalog([]string{"cpu", "mem"})})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/services/web/telemetry", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, want := rec.Header().Get("ETag"), `"`+hTelSHA1+`"`; got != want {
		t.Errorf("ETag = %q, want %q (snapshot SHA1)", got, want)
	}
	if got, want := rec.Header().Get("Cache-Control"), "no-cache"; got != want {
		t.Errorf("Cache-Control = %q, want %q (tag-ref v1.0.0 mutable)", got, want)
	}
	var body hTelBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal body: %v; raw=%s", err, rec.Body.String())
	}
	if body.Service != "web" || body.Ref != "v1.0.0" || body.SHA1 != hTelSHA1 {
		t.Errorf("body meta = %+v, want service=web ref=v1.0.0 sha1=%s", body, hTelSHA1)
	}
	if !body.Enabled || body.IntervalSec != 30 {
		t.Errorf("body config = %+v, want enabled=true interval_sec=30", body)
	}
	if len(body.Collectors) != 2 {
		t.Errorf("collectors = %v, want [cpu mem]", body.Collectors)
	}
	if len(body.KnownCollectors) != len(config.KnownCollectors) {
		t.Errorf("known_collectors = %v, want %v (full set)", body.KnownCollectors, config.KnownCollectors)
	}
}

// TestServiceTelemetry_CacheControl_ImmutableForSHARef — pinned commit-SHA ref → immutable+year.
func TestServiceTelemetry_CacheControl_ImmutableForSHARef(t *testing.T) {
	r := telemetryTestRouter(t, hTelLister{catalog: hTelCatalog([]string{"cpu"})})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/services/web/telemetry?ref="+hTelSHARef, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, want := rec.Header().Get("Cache-Control"), "public, max-age=31536000, immutable"; got != want {
		t.Errorf("Cache-Control = %q, want %q (pinned SHA-ref immutable)", got, want)
	}
}

// TestServiceTelemetry_EmptyCollectors_200 — collectors empty → `[]` (not null) + 200.
func TestServiceTelemetry_EmptyCollectors_200(t *testing.T) {
	r := telemetryTestRouter(t, hTelLister{catalog: hTelCatalog(nil)})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/services/web/telemetry", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(raw["collectors"]) != "[]" {
		t.Errorf("collectors = %s, want [] (not null)", raw["collectors"])
	}
}

// TestServiceTelemetry_IfNoneMatch_304 — conditional GET: If-None-Match matched the ETag
// → 304 without a body.
func TestServiceTelemetry_IfNoneMatch_304(t *testing.T) {
	r := telemetryTestRouter(t, hTelLister{catalog: hTelCatalog([]string{"cpu", "mem"})})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/services/web/telemetry", nil)
	req.Header.Set("If-None-Match", `"`+hTelSHA1+`"`)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("304 body not empty: %q", rec.Body.String())
	}
	if got := rec.Header().Get("ETag"); got != `"`+hTelSHA1+`"` {
		t.Errorf("ETag on 304 = %q, want snapshot SHA1", got)
	}
}

// TestServiceTelemetry_NilLister_500 — lister not configured → 500 "not configured".
func TestServiceTelemetry_NilLister_500(t *testing.T) {
	r := telemetryTestRouter(t, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/services/web/telemetry", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// TestServiceTelemetry_LoaderError_502 — a git-loader error → 502 Bad Gateway.
func TestServiceTelemetry_LoaderError_502(t *testing.T) {
	r := telemetryTestRouter(t, hTelErrLister{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/services/web/telemetry", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
}
