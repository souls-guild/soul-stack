// SHARED infrastructure for walking the Operator API chi router for route-coverage gates.
//
// Common primitives for the aggregator tests live here: the [route] type, path
// normalization, [collectRoutes] (chi.Walk over the assembled [buildRouter]) +
// [pathAllowlist] (opt-in domains whose handlers are passed nil when
// assembling the drift router). Their consumer is
// [TestFullSpec_CoversAllRoutes] (huma_full_spec_test.go), which cross-checks
// the real chi routes against the assembled huma spec ([buildFullOpenAPISpec])
// both ways: "route exists, not in the spec" (the aggregator forgot a domain)
// and "in the spec, no route" (a stray operation). This is the "every route is
// in the spec" guarantee — there's no separate handwritten source anymore, the
// served spec and the committed generata are both derived from the huma dump.
//
// The test is clean: the router is assembled via [buildRouter] with
// stub dependencies (zero-value embedded interfaces whose methods aren't
// called while walking the tree) — no Postgres/Redis/Vault, no `integration`
// build tag.
package api

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/augur"
	"github.com/souls-guild/soul-stack/keeper/internal/oracle"
	"github.com/souls-guild/soul-stack/keeper/internal/pluginhost"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
	"github.com/souls-guild/soul-stack/keeper/internal/sigil"
)

// route — a normalized route key for set comparison.
// method is upper case ("GET"/"POST"/…); path is `/v1/roles/{name}` with
// unified `{param}` placeholders.
type route struct {
	method string
	path   string
}

func (r route) String() string { return r.method + " " + r.path }

// normalizePath brings path to a canonical form for comparison. chi mounts
// `r.Route("/operators")` + `.Post("/")` as `/v1/operators/` (with a trailing
// slash), while the OpenAPI notation is `/v1/operators` (without). It's the
// same endpoint; we strip the trailing slash (except for root). `{param}`
// placeholders are already identical between chi and openapi (curly braces) —
// no separate normalization needed.
func normalizePath(p string) string {
	if len(p) > 1 {
		p = strings.TrimSuffix(p, "/")
	}
	return p
}

// wildcardSuffix — chi notation for a catch-all segment. Routes ending in it
// are fallback handlers (router.go: r.HandleFunc("/*")), not endpoints; by
// definition they have no place in the spec. chi registers such a wildcard
// under ALL HTTP methods, so we filter by path suffix rather than
// enumerating (method, path) pairs by name.
const wildcardSuffix = "/*"

// pathAllowlist — spec paths that legitimately have no implementing route.
// Endpoint declarations whose handlers are wired only for a non-nil domain
// (push / errand / audit / push-provider): the drift-test builds the router
// with these domains=nil, so they're declared in the spec but absent from the
// router. An empty set would mean "any declaration without a route = drift";
// here we record the known opt-in endpoints with an explicit rationale. Once a
// handler is wired unconditionally, its entry must be removed — then the test
// starts checking it as an ordinary route.
var pathAllowlist = map[route]string{
	{method: http.MethodPost, path: "/v1/push/apply"}:     "keeper.push apply declared in the spec, route wired ONLY when non-nil pushH (drift-test builds router with pushH=nil)",
	{method: http.MethodGet, path: "/v1/push/{apply_id}"}: "keeper.push GET by apply_id, similarly, push/apply is wired only when pushH is non-nil",
	// errand.*-routes are wired ONLY when errandH is non-nil (ADR-033, slice E2);
	// the drift-test builds the router with errandH=nil, so they're declared in
	// the spec but absent from the router — a documented "opt-in" block (push pattern).
	{method: http.MethodPost, path: "/v1/souls/{sid}/exec"}:      "ADR-033 Errand: route wired ONLY when non-nil errandH (slice E2 production-wire-up)",
	{method: http.MethodGet, path: "/v1/errands"}:                "ADR-033 Errand list: route wired ONLY when non-nil errandH",
	{method: http.MethodGet, path: "/v1/errands/{errand_id}"}:    "ADR-033 Errand get: route wired ONLY when non-nil errandH",
	{method: http.MethodDelete, path: "/v1/errands/{errand_id}"}: "ADR-033 Errand cancel (slice E5): route wired ONLY when non-nil errandH",
	// audit.read — route wired ONLY when auditH is non-nil (UI iter 2, errandH/pushH
	// pattern). The drift-test builds the router with auditH=nil → declared in
	// the spec, but not in the router.
	{method: http.MethodGet, path: "/v1/audit"}: "UI iter 2 audit: route wired ONLY when non-nil auditH (production-wire-up via AuditReader)",
	// push-provider.* — routes are wired ONLY when pushProviderH is non-nil
	// (ADR-032 amendment 2026-05-26, S7-2); the drift-test builds the router
	// with pushProviderH=nil → declared in the spec, but not in the router.
	{method: http.MethodPost, path: "/v1/push-providers"}:          "S7-2 push-provider CRUD: route wired ONLY when non-nil pushProviderH",
	{method: http.MethodGet, path: "/v1/push-providers"}:           "S7-2 push-provider list: route wired ONLY when non-nil pushProviderH",
	{method: http.MethodGet, path: "/v1/push-providers/{name}"}:    "S7-2 push-provider get: route wired ONLY when non-nil pushProviderH",
	{method: http.MethodPut, path: "/v1/push-providers/{name}"}:    "S7-2 push-provider update: route wired ONLY when non-nil pushProviderH",
	{method: http.MethodDelete, path: "/v1/push-providers/{name}"}: "S7-2 push-provider delete: route wired ONLY when non-nil pushProviderH",

	// provider.* / profile.* — Cloud CRUD (ADR-017): routes are wired ONLY when
	// providerH/profileH is non-nil; the drift-test builds the router with nil →
	// declared in the spec, absent from the router (documented opt-in, push-provider pattern).
	{method: http.MethodPost, path: "/v1/providers"}:          "ADR-017 provider create: route wired ONLY when non-nil providerH",
	{method: http.MethodGet, path: "/v1/providers"}:           "ADR-017 provider list: route wired ONLY when non-nil providerH",
	{method: http.MethodGet, path: "/v1/providers/{name}"}:    "ADR-017 provider get: route wired ONLY when non-nil providerH",
	{method: http.MethodDelete, path: "/v1/providers/{name}"}: "ADR-017 provider delete: route wired ONLY when non-nil providerH",
	{method: http.MethodPost, path: "/v1/profiles"}:           "ADR-017 profile create: route wired ONLY when non-nil profileH",
	{method: http.MethodGet, path: "/v1/profiles"}:            "ADR-017 profile list: route wired ONLY when non-nil profileH",
	{method: http.MethodGet, path: "/v1/profiles/{name}"}:     "ADR-017 profile get: route wired ONLY when non-nil profileH",
	{method: http.MethodDelete, path: "/v1/profiles/{name}"}:  "ADR-017 profile delete: route wired ONLY when non-nil profileH",

	// push-runs list: route wired ONLY when pushH is non-nil (UI-4); the
	// drift-test builds the router with pushH=nil. The paired per-id detail
	// (`GET /v1/push/{apply_id}`) and `POST /v1/push/apply` are already in the
	// allowlist above.
	{method: http.MethodGet, path: "/v1/push-runs"}: "UI-4 Push-runs global list: route wired ONLY when non-nil pushH",

	// auth.ldap login: route wired ONLY when LDAPAuth is non-nil (ADR-058); the
	// drift-test builds the router with LDAPAuth=nil → declared in the spec
	// (prefix /auth), but absent from the router. OUTSIDE /v1 (public entry,
	// RequireJWT doesn't apply).
	{method: http.MethodPost, path: "/auth/ldap/login"}: "ADR-058 LDAP login: route wired ONLY when non-nil LDAPAuth (optional block auth.ldap in keeper.yml)",

	// auth.oidc endpoints (ADR-058 stage 2): wired ONLY when OIDCAuth is non-nil
	// (optional auth.oidc + Redis block); the drift-test builds the router with
	// OIDCAuth=nil → declared in the spec (prefix /auth), absent from the router.
	{method: http.MethodGet, path: "/auth/oidc/login"}:    "ADR-058 OIDC login: route wired ONLY when non-nil OIDCAuth (optional block auth.oidc + Redis)",
	{method: http.MethodGet, path: "/auth/oidc/callback"}: "ADR-058 OIDC callback: route wired ONLY when non-nil OIDCAuth (optional block auth.oidc + Redis)",

	// voyage.*-routes are wired ONLY when voyageH is non-nil (ADR-043 S5); the
	// drift-test builds the router with voyageH=nil, so they're declared in the
	// spec but absent from the router — a documented "opt-in" block (the
	// errandRunH/pushH pattern).
	{method: http.MethodPost, path: "/v1/voyages"}:             "ADR-043 Voyage create: route wired ONLY when non-nil voyageH",
	{method: http.MethodPost, path: "/v1/voyages/preview"}:     "ADR-043 amendment §4 Voyage preview: route wired ONLY when non-nil voyageH",
	{method: http.MethodGet, path: "/v1/voyages"}:              "ADR-043 Voyage list: route wired ONLY when non-nil voyageH",
	{method: http.MethodGet, path: "/v1/voyages/{id}"}:         "ADR-043 Voyage get: route wired ONLY when non-nil voyageH",
	{method: http.MethodGet, path: "/v1/voyages/{id}/targets"}: "ADR-043 Voyage targets drill: route wired ONLY when non-nil voyageH",
	{method: http.MethodDelete, path: "/v1/voyages/{id}"}:      "ADR-043 Voyage cancel: route wired ONLY when non-nil voyageH",

	// cadence.*-routes are wired ONLY when cadenceH is non-nil (ADR-046 S4); the
	// drift-test builds the router with cadenceH=nil, so they're declared in the
	// spec but absent from the router — a documented "opt-in" block (the voyageH
	// pattern).
	{method: http.MethodPost, path: "/v1/cadences"}:              "ADR-046 Cadence create: route wired ONLY when non-nil cadenceH",
	{method: http.MethodGet, path: "/v1/cadences"}:               "ADR-046 Cadence list: route wired ONLY when non-nil cadenceH",
	{method: http.MethodGet, path: "/v1/cadences/{id}"}:          "ADR-046 Cadence get: route wired ONLY when non-nil cadenceH",
	{method: http.MethodPatch, path: "/v1/cadences/{id}"}:        "ADR-046 Cadence update: route wired ONLY when non-nil cadenceH",
	{method: http.MethodDelete, path: "/v1/cadences/{id}"}:       "ADR-046 Cadence delete: route wired ONLY when non-nil cadenceH",
	{method: http.MethodPost, path: "/v1/cadences/{id}/enable"}:  "ADR-046 Cadence enable: route wired ONLY when non-nil cadenceH",
	{method: http.MethodPost, path: "/v1/cadences/{id}/disable"}: "ADR-046 Cadence disable: route wired ONLY when non-nil cadenceH",
	{method: http.MethodGet, path: "/v1/cadences/{id}/runs"}:     "ADR-046 Cadence runs drill: route wired ONLY when non-nil cadenceH",

	// choir.*-routes are wired ONLY when choirH is non-nil (ADR-044 S-T3); the
	// drift-test builds the router with choirH=nil, so they're declared in the
	// spec but absent from the router — a documented "opt-in" block (the
	// tideH/errandH/pushH pattern).
	{method: http.MethodPost, path: "/v1/incarnations/{name}/choirs"}:                        "ADR-044 Choir create: route wired ONLY when non-nil choirH",
	{method: http.MethodGet, path: "/v1/incarnations/{name}/choirs"}:                         "ADR-044 Choir list: route wired ONLY when non-nil choirH",
	{method: http.MethodDelete, path: "/v1/incarnations/{name}/choirs/{choir}"}:              "ADR-044 Choir delete: route wired ONLY when non-nil choirH",
	{method: http.MethodPost, path: "/v1/incarnations/{name}/choirs/{choir}/voices"}:         "ADR-044 Voice add: route wired ONLY when non-nil choirH",
	{method: http.MethodGet, path: "/v1/incarnations/{name}/choirs/{choir}/voices"}:          "ADR-044 Voice list: route wired ONLY when non-nil choirH",
	{method: http.MethodDelete, path: "/v1/incarnations/{name}/choirs/{choir}/voices/{sid}"}: "ADR-044 Voice remove: route wired ONLY when non-nil choirH",

	// herald.*/tiding.*-routes are wired ONLY when heraldH is non-nil (ADR-052 S4);
	// the drift-test builds the router with heraldH=nil, so they're declared in
	// the spec but absent from the router — a documented "opt-in" block (the
	// push-provider pattern).
	{method: http.MethodPost, path: "/v1/heralds"}:          "ADR-052 Herald create: route wired ONLY when non-nil heraldH",
	{method: http.MethodGet, path: "/v1/heralds"}:           "ADR-052 Herald list: route wired ONLY when non-nil heraldH",
	{method: http.MethodGet, path: "/v1/heralds/{name}"}:    "ADR-052 Herald get: route wired ONLY when non-nil heraldH",
	{method: http.MethodPut, path: "/v1/heralds/{name}"}:    "ADR-052 Herald update: route wired ONLY when non-nil heraldH",
	{method: http.MethodDelete, path: "/v1/heralds/{name}"}: "ADR-052 Herald delete: route wired ONLY when non-nil heraldH",
	{method: http.MethodPost, path: "/v1/tidings"}:          "ADR-052 Tiding create: route wired ONLY when non-nil heraldH",
	{method: http.MethodGet, path: "/v1/tidings"}:           "ADR-052 Tiding list: route wired ONLY when non-nil heraldH",
	{method: http.MethodGet, path: "/v1/tidings/{name}"}:    "ADR-052 Tiding get: route wired ONLY when non-nil heraldH",
	{method: http.MethodPut, path: "/v1/tidings/{name}"}:    "ADR-052 Tiding update: route wired ONLY when non-nil heraldH",
	{method: http.MethodDelete, path: "/v1/tidings/{name}"}: "ADR-052 Tiding delete: route wired ONLY when non-nil heraldH",

	// GET /v1/cluster — HA topology from the Conclave: route wired ONLY when
	// clusterH is non-nil (Redis wire-up); the drift-test builds the router with
	// clusterH=nil, so it's declared in the spec but absent from the router — a
	// documented "opt-in" block (the voyageH/cadenceH pattern).
	{method: http.MethodGet, path: "/v1/cluster"}: "ADR-006 Cluster overview: route wired ONLY when non-nil clusterH (production-wire-up with Redis-Conclave)",
}

// collectRoutes gathers the actual `(method, path)` pairs from the chi tree
// via chi.Walk. buildRouter returns an http.Handler, whose concrete type is
// *chi.Mux implementing chi.Routes. chi path patterns are already in the
// `/v1/roles/{name}` form — matching the openapi notation, so no separate
// `{param}` normalization is needed (both use curly braces); we only
// upper-case the method.
func collectRoutes(t *testing.T) map[route]struct{} {
	t.Helper()
	h := buildRouter(
		nil, // verifier — middleware RequireJWT is built lazily, not dereferenced while walking
		nil, // healthH — r.Get(...) only stores a method-value handler
		stubOperatorHandler(t),
		handlers.NewIncarnationHandler(nil, nil, nil, nil, nil, nil, nil, nil, nil),
		handlers.NewSoulHandler(nil, nil, nil, nil),
		stubRoleHandler(t),
		stubSynodHandler(t),
		stubSigilHandler(t),
		stubSigilKeyHandler(t),
		stubServiceHandler(t),
		stubProvisioningPolicyHandler(t),
		stubAugurHandler(t),
		stubOracleHandler(t),
		nil, // pushH — push.*-routes are wired only when pushH is non-nil (router.go); currently in the allowlist
		nil, // pushProviderH — push-provider.*-routes are wired only when non-nil; in the allowlist
		nil, // providerH — provider.*-routes are wired only when non-nil; in the allowlist
		nil, // profileH — profile.*-routes are wired only when non-nil; in the allowlist
		nil, // errandH — errand.*-routes are wired only when errandH is non-nil; in the allowlist
		nil, // voyageH — voyage.*-routes are wired only when voyageH is non-nil (ADR-043 S5); in the allowlist
		nil, // cadenceH — cadence.*-routes are wired only when cadenceH is non-nil (ADR-046 S4); in the allowlist
		nil, // auditH — the audit route is wired only when auditH is non-nil; in the allowlist
		nil, // choirH — choir.*-routes are wired only when choirH is non-nil (ADR-044 S-T3); in the allowlist
		nil, // heraldH — herald.*/tiding.*-routes are wired only when heraldH is non-nil (ADR-052 S4); in the allowlist
		handlers.NewModuleCatalogHandler(nil, nil),  // moduleCatalogH — /v1/modules is always mounted (core catalog); plugins=nil → core only
		handlers.NewModuleFormPrepHandler(nil, nil), // moduleFormPrepH — non-nil → /v1/modules/{name}/form-prep is mounted (ADR-045 S3); the resolver isn't called while walking, even nil
		handlers.NewPermissionCatalogHandler(nil),   // permCatalogH — /v1/permissions is always mounted (static rbac catalog)
		handlers.NewEventTypeCatalogHandler(nil),    // eventTypeCatalogH — /v1/event-types is always mounted (static herald catalog)
		handlers.NewHeraldTypeCatalogHandler(nil),   // heraldTypeCatalogH — /v1/herald-types is always mounted (static herald-type catalog)
		handlers.NewMyPermissionsHandler(nil, nil),  // meH — /v1/me/permissions is always mounted (depends only on the RBAC snapshot); PermissionsOf isn't called while walking the tree
		nil,                                  // enforcer — RequirePermission is built lazily
		nil,                                  // auditWriter — Audit is built lazily
		nil,                                  // metricsHTTP — nil → the metrics middleware isn't wired (router.go)
		nil,                                  // tollDegradedReader — DegradedMiddleware skips when nil (router.go)
		nil,                                  // tempoLimiter — nil → RateLimit middleware passthrough (router.go)
		nil,                                  // tempoMetrics — nil → emit no-op (router.go)
		nil,                                  // tempoVoyageCreateLimits — nil is fine (RateLimit doesn't call the provider when the limiter is nil)
		nil,                                  // tempoVoyagePreviewLimits — nil is fine (RateLimit doesn't call the provider when the limiter is nil)
		false,                                // webUIEnabled — /ui is outside /v1, the drift-walker doesn't see it; keep it off for perimeter cleanliness
		nil,                                  // ldapAuth (LDAP isn't configured in the test)
		nil,                                  // oidcAuth (OIDC isn't configured in the test)
		nil,                                  // loginGuard (anti-bruteforce off in the test)
		apimiddleware.AuthLoginLimitConfig{}, // loginLimitCfg
		nil,                                  // soulStatsStaleFn (default 90s in the test)
		nil,                                  // clusterH (cluster-view isn't mounted in the test)
		&runEventsDeps{},                     // runEventsDeps — SSE run-events is mounted (ADR-068 §A3); deps' methods aren't called while walking the tree
		nil,                                  // logger — nil is fine (handlers get io.Discard internally)
	)

	routes, ok := h.(chi.Routes)
	if !ok {
		t.Fatalf("buildRouter returned %T, does not implement chi.Routes - chi.Walk traversal is impossible", h)
	}

	set := make(map[route]struct{})
	err := chi.Walk(routes, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		set[route{method: strings.ToUpper(method), path: normalizePath(pattern)}] = struct{}{}
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	if len(set) == 0 {
		t.Fatal("chi.Walk did not return a single route - is the router empty?")
	}
	return set
}

// --- stub dependencies for handler constructors ---
//
// buildRouter does NOT call these dependencies' methods while walking the
// tree — it only needs non-nil instances so the constructors don't panic and
// register the routes (in particular /v1/roles is registered only when
// roleH is non-nil). An embedded zero-value interface satisfies the type
// contract without hand-implementing every method; calling any of them
// would nil-panic — but the tree walk never reaches a call.

// stubOperatorHandler builds an OperatorHandler with a stub pool/issuer/rbac.
func stubOperatorHandler(t *testing.T) *handlers.OperatorHandler {
	t.Helper()
	return handlers.NewOperatorHandler(stubOperatorPool{}, stubIssuer{}, stubRBACSource{}, time.Hour, nil)
}

// stubRoleHandler builds a RoleHandler via rbac.Service with a stub pool.
func stubRoleHandler(t *testing.T) *handlers.RoleHandler {
	t.Helper()
	svc, err := rbac.NewService(rbac.ServiceDeps{Pool: stubRBACPool{}})
	if err != nil {
		t.Fatalf("rbac.NewService(stub): %v", err)
	}
	return handlers.NewRoleHandler(svc, nil)
}

// stubSynodHandler — a non-nil SynodHandler so synod.*-routes register for
// the drift check (the service's methods aren't called while walking the tree).
func stubSynodHandler(t *testing.T) *handlers.SynodHandler {
	t.Helper()
	svc, err := rbac.NewService(rbac.ServiceDeps{Pool: stubRBACPool{}})
	if err != nil {
		t.Fatalf("rbac.NewService(stub): %v", err)
	}
	return handlers.NewSynodHandler(svc, nil)
}

// stubSigilHandler builds a SigilHandler via sigil.Service with a stub
// Signer/Store/SlotReader. The dependencies' methods aren't called while
// walking the tree — it only needs a non-nil service so plugin.*-routes
// register.
func stubSigilHandler(t *testing.T) *handlers.SigilHandler {
	t.Helper()
	signer, err := sigil.NewSigner(ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)))
	if err != nil {
		t.Fatalf("sigil.NewSigner(stub): %v", err)
	}
	svc, err := sigil.NewService(sigil.ServiceDeps{
		Signer: signer,
		Store:  stubSigilStore{},
		Slots:  stubSlotReader{},
	})
	if err != nil {
		t.Fatalf("sigil.NewService(stub): %v", err)
	}
	return handlers.NewSigilHandler(svc, nil)
}

// stubSigilKeyHandler builds a SigilKeyHandler via sigil.KeyService with a
// stub pool/vault. The dependencies' methods aren't called while walking the
// tree — it only needs a non-nil service so sigil/keys-routes register.
func stubSigilKeyHandler(t *testing.T) *handlers.SigilKeyHandler {
	t.Helper()
	svc, err := sigil.NewKeyService(sigil.KeyServiceDeps{
		Pool:  stubKeyStorePool{},
		Vault: stubVaultWriter{},
	})
	if err != nil {
		t.Fatalf("sigil.NewKeyService(stub): %v", err)
	}
	return handlers.NewSigilKeyHandler(svc, nil)
}

type stubKeyStorePool struct{ sigil.KeyStorePool }

type stubVaultWriter struct{}

func (stubVaultWriter) WriteKV(context.Context, string, map[string]any) error { return nil }

// stubServiceHandler builds a ServiceHandler via serviceregistry.Service with
// a stub pool. The pool's methods aren't called while walking the tree — it
// only needs a non-nil service so service.*-routes register.
func stubServiceHandler(t *testing.T) *handlers.ServiceHandler {
	t.Helper()
	svc, err := serviceregistry.NewService(serviceregistry.ServiceDeps{Pool: stubServicePool{}})
	if err != nil {
		t.Fatalf("serviceregistry.NewService(stub): %v", err)
	}
	return handlers.NewServiceHandler(svc, nil, nil, nil, nil, nil, nil)
}

type stubServicePool struct{ serviceregistry.ServicePool }

// stubProvisioningPolicyHandler builds a ProvisioningPolicyHandler via
// serviceregistry.Service with a stub pool + stub reader. The methods aren't
// called while walking the tree — it only needs a non-nil handler so
// provisioning-policy-routes register (drift router↔full-spec). ADR-058 Part B.
func stubProvisioningPolicyHandler(t *testing.T) *handlers.ProvisioningPolicyHandler {
	t.Helper()
	svc, err := serviceregistry.NewService(serviceregistry.ServiceDeps{Pool: stubServicePool{}})
	if err != nil {
		t.Fatalf("serviceregistry.NewService(stub): %v", err)
	}
	return handlers.NewProvisioningPolicyHandler(stubProvisioningReader{}, svc, nil)
}

type stubProvisioningReader struct{}

func (stubProvisioningReader) ProvisioningPolicy() ([]string, bool) { return nil, false }

// stubAugurHandler builds an AugurHandler via augur.Service with a stub pool.
// The pool's methods aren't called while walking the tree — it only needs a
// non-nil service so augur.*-routes register.
func stubAugurHandler(t *testing.T) *handlers.AugurHandler {
	t.Helper()
	svc, err := augur.NewService(augur.ServiceDeps{Pool: stubAugurPool{}})
	if err != nil {
		t.Fatalf("augur.NewService(stub): %v", err)
	}
	return handlers.NewAugurHandler(svc, nil)
}

type stubAugurPool struct{ augur.ServicePool }

// stubOracleHandler builds an OracleHandler via oracle.Service with a stub
// pool + a real WhereEvaluator (compile-check for where-CEL; the constructor
// requires non-nil Where). The pool's methods aren't called while walking the
// tree — it only needs a non-nil service so vigil.*/decree.*-routes register.
func stubOracleHandler(t *testing.T) *handlers.OracleHandler {
	t.Helper()
	where, err := oracle.NewWhereEvaluator()
	if err != nil {
		t.Fatalf("oracle.NewWhereEvaluator(stub): %v", err)
	}
	svc, err := oracle.NewService(oracle.ServiceDeps{Pool: stubOraclePool{}, Where: where})
	if err != nil {
		t.Fatalf("oracle.NewService(stub): %v", err)
	}
	return handlers.NewOracleHandler(svc, nil)
}

type stubOraclePool struct{ oracle.ServicePool }

type stubSigilStore struct{}

func (stubSigilStore) Insert(context.Context, *sigil.Sigil) error { return nil }
func (stubSigilStore) Revoke(context.Context, string, string, string, string) error {
	return nil
}
func (stubSigilStore) ListActive(context.Context) ([]*sigil.Sigil, error) { return nil, nil }

type stubSlotReader struct{}

func (stubSlotReader) ReadSlot(string, string) (*pluginhost.SlotContents, error) {
	return nil, sigil.ErrPluginNotInCache
}

func (stubSlotReader) SlotCommitSHA(string, string) (string, error) {
	return "", pluginhost.ErrSlotNotFound
}

type stubOperatorPool struct{ handlers.OperatorPool }

type stubIssuer struct{}

func (stubIssuer) Issue(string, []string, time.Duration, bool) (string, error) {
	return "", fmt.Errorf("stub issuer: should not be called in the drift test")
}

type stubRBACSource struct{}

func (stubRBACSource) RolesOf(string) []string { return nil }

type stubRBACPool struct{ rbac.ServicePool }
