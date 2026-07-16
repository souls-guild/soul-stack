package api

// Guard tests for the INCARNATION domain on huma (batch-2g, ADR-054). MIXED audit
// class — we check EVERY write with the right S6 guard (mixing up the class = a
// regression):
//
//   - MIDDLEWARE-AUDIT (create/run/unlock/upgrade): the event is written by the
//     huma-audit-middleware (variant B) — guarded via assertMiddlewareAudit (audit
//     on 2xx with a non-empty payload; empty on 4xx/403).
//   - SELF-AUDIT (rerun-last/check-drift/destroy/update-hosts): the event is
//     written by the handler ITSELF INSIDE *Typed — guarded via assertSelfAudit
//     (event with requiredKey).
//
// Plus: golden byte-exact wire for every route; ChiCoexistence on the REAL
// buildRouter (incarnation+choir, chi.Walk); 400 on out-of-range list;
// RBAC-deny→403; read→NoAudit.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
	"github.com/souls-guild/soul-stack/keeper/internal/statemigrate"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// === ChiCoexistence guard (REAL buildRouter, incarnation+choir, chi.Walk) ===

// TestHumaIncarnation_ChiCoexistence — guard on the REACHABILITY of ALL incarnation
// huma routes + coexistence with the choir mount on the SAME /v1/incarnations group.
// After chi.Route("/{name}") was removed, incarnation ops carry the FULL path
// /{name}[/...]; choir (batch-2f) is mounted at the same place. If even one
// incarnation or choir route is shadowed (a sibling chi.Route at the /{name} node) —
// chi.Walk won't list it (405 in prod). The test requires each route to be hit
// exactly once (NOT chi.Match — it gives a false-true on a shadowed node).
func TestHumaIncarnation_ChiCoexistence(t *testing.T) {
	incH := handlers.NewIncarnationHandler(&incTestDB{}, &incTestStarter{}, &incTestStarter{}, &incTestDrift{}, &incTestResolver{ok: true}, &incTestLoader{}, nil, nil, nil)
	h := buildRouter(
		nil, // verifier
		nil, // healthH
		stubOperatorHandler(t),
		incH,
		handlers.NewSoulHandler(nil, nil, nil, nil),
		stubRoleHandler(t), stubSynodHandler(t), stubSigilHandler(t), stubSigilKeyHandler(t),
		stubServiceHandler(t), nil, stubAugurHandler(t), stubOracleHandler(t),
		nil,                                     // pushH
		nil,                                     // pushProviderH
		nil,                                     // providerH
		nil,                                     // profileH
		nil,                                     // errandH
		nil,                                     // voyageH
		nil,                                     // cadenceH
		nil,                                     // auditH
		handlers.NewChoirHandler(nil, nil, nil), // choirH non-nil → choir-mount coexists
		nil,                                     // heraldH
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
		false,                                // webUIEnabled — /ui is out of scope for the incarnation routing test
		nil,                                  // ldapAuth (LDAP not configured in the test)
		nil,                                  // oidcAuth (OIDC not configured in the test)
		nil,                                  // loginGuard (anti-bruteforce off in the test)
		apimiddleware.AuthLoginLimitConfig{}, // loginLimitCfg
		nil,                                  // soulStatsStaleFn (defaults to 90s in the test)
		nil,                                  // clusterH (cluster-view not mounted in the test)
		nil,                                  // runEventsDeps (ADR-068 §A3 — not tested here)
		nil,                                  // logger
	)
	routes, ok := h.(chi.Routes)
	if !ok {
		t.Fatalf("buildRouter вернул %T, не chi.Routes", h)
	}

	// The full set of incarnation + choir routes on the /v1/incarnations group: each
	// one MUST be hit EXACTLY once. Missing = shadowing (405 in prod); duplicate =
	// a mount collision.
	want := map[route]int{
		{http.MethodPost, "/v1/incarnations"}:                                      0,
		{http.MethodGet, "/v1/incarnations"}:                                       0,
		{http.MethodGet, "/v1/incarnations/{name}"}:                                0,
		{http.MethodGet, "/v1/incarnations/{name}/upgrade-paths"}:                  0,
		{http.MethodGet, "/v1/incarnations/{name}/history"}:                        0,
		{http.MethodPost, "/v1/incarnations/{name}/scenarios/{scenario}"}:          0,
		{http.MethodPost, "/v1/incarnations/{name}/unlock"}:                        0,
		{http.MethodPost, "/v1/incarnations/{name}/upgrade"}:                       0,
		{http.MethodPost, "/v1/incarnations/{name}/rerun-last"}:                    0,
		{http.MethodPost, "/v1/incarnations/{name}/check-drift"}:                   0,
		{http.MethodDelete, "/v1/incarnations/{name}"}:                             0,
		{http.MethodPatch, "/v1/incarnations/{name}/hosts"}:                        0,
		{http.MethodPost, "/v1/incarnations/{name}/choirs"}:                        0,
		{http.MethodGet, "/v1/incarnations/{name}/choirs"}:                         0,
		{http.MethodDelete, "/v1/incarnations/{name}/choirs/{choir}"}:              0,
		{http.MethodPost, "/v1/incarnations/{name}/choirs/{choir}/voices"}:         0,
		{http.MethodGet, "/v1/incarnations/{name}/choirs/{choir}/voices"}:          0,
		{http.MethodDelete, "/v1/incarnations/{name}/choirs/{choir}/voices/{sid}"}: 0,
	}
	if err := chi.Walk(routes, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		k := route{method: method, path: normalizePath(pattern)}
		if _, tracked := want[k]; tracked {
			want[k]++
		}
		return nil
	}); err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	for k, n := range want {
		if n != 1 {
			t.Errorf("%s встретился %d раз, want 1 (0 = затенение/не смонтирован → 405; >1 = дубль)", k, n)
		}
	}
}

// === isolated huma-router (production wiring, verbatim from router.go) ===

// incEnforcer — a combined RBAC stub: PermissionChecker (RequirePermissionMulti on
// write) + ActionHolder (RequireAction on read). allow parameterizes both facets.
type incEnforcer struct{ allow bool }

func (e incEnforcer) Check(string, string, string, map[string]string) error {
	if e.allow {
		return nil
	}
	return rbac.ErrPermissionDenied
}
func (e incEnforcer) HoldsAction(string, string, string) bool { return e.allow }

// humaIncarnationRouter mounts ALL incarnation routes via huma exactly per the
// router.go wiring: per-route RBAC + the correct audit class (MIDDLEWARE for
// create/run/unlock/upgrade; SELF for rerun-last/check-drift/destroy/update-hosts;
// read without audit) + a huma op with the full path /{name}[/...] on the
// /v1/incarnations group. enforcer/auditW/incH are parameterized. injectClaims
// replaces RequireJWT.
func humaIncarnationRouter(t *testing.T, enforcer incEnforcer, auditW audit.Writer, incH *handlers.IncarnationHandler) *chi.Mux {
	t.Helper()
	installHumaErrorOverride()
	r := chi.NewRouter()
	injectClaims := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := apimiddleware.InjectClaimsForTest(req.Context(), &keeperjwt.Claims{Subject: "archon-alice"})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
	multi := func(action string) func(http.Handler) http.Handler {
		return apimiddleware.RequirePermissionMulti(enforcer, "incarnation", action, incNoCtxSelector)
	}
	r.Route("/v1", func(r chi.Router) {
		r.Route("/incarnations", func(r chi.Router) {
			// MIDDLEWARE-AUDIT
			r.With(injectClaims, multi("create")).Group(func(r chi.Router) {
				registerHumaIncarnationCreate(newHumaIncarnationAPI(r, auditW, audit.EventIncarnationCreated, nil), incH)
			})
			r.With(injectClaims, multi("run")).Group(func(r chi.Router) {
				registerHumaIncarnationRun(newHumaIncarnationAPI(r, auditW, audit.EventIncarnationScenarioStarted, nil), incH)
			})
			r.With(injectClaims, multi("unlock")).Group(func(r chi.Router) {
				registerHumaIncarnationUnlock(newHumaIncarnationAPI(r, auditW, audit.EventIncarnationUnlocked, nil), incH)
			})
			r.With(injectClaims, multi("upgrade")).Group(func(r chi.Router) {
				registerHumaIncarnationUpgrade(newHumaIncarnationAPI(r, auditW, audit.EventIncarnationUpgradeStarted, nil), incH)
			})
			// SELF-AUDIT
			r.With(injectClaims, multi("rerun-last")).Group(func(r chi.Router) {
				registerHumaIncarnationRerunLast(newHumaCadenceAPI(r), incH)
			})
			r.With(injectClaims, multi("check-drift")).Group(func(r chi.Router) {
				registerHumaIncarnationCheckDrift(newHumaCadenceAPI(r), incH)
			})
			r.With(injectClaims, multi("destroy")).Group(func(r chi.Router) {
				registerHumaIncarnationDestroy(newHumaCadenceAPI(r), incH)
			})
			r.With(injectClaims, multi("update-hosts")).Group(func(r chi.Router) {
				registerHumaIncarnationUpdateHosts(newHumaCadenceAPI(r), incH)
			})
			// secret reveal (NIM-74): POST self-audit + GET read, both under view-secrets.
			r.With(injectClaims, multi("view-secrets")).Group(func(r chi.Router) {
				registerHumaIncarnationRevealSecret(newHumaCadenceAPI(r), incH)
			})
			r.With(injectClaims, apimiddleware.RequireAction(enforcer, "incarnation", "view-secrets")).Group(func(r chi.Router) {
				registerHumaIncarnationRevealableSecrets(newHumaCadenceAPI(r), incH)
			})
			// READ
			r.With(injectClaims, stashRawQuery, apimiddleware.RequireAction(enforcer, "incarnation", "list")).Group(func(r chi.Router) {
				registerHumaIncarnationList(newHumaCadenceAPI(r), incH)
			})
			r.With(injectClaims, apimiddleware.RequireAction(enforcer, "incarnation", "get")).Group(func(r chi.Router) {
				registerHumaIncarnationGet(newHumaCadenceAPI(r), incH)
			})
			r.With(injectClaims, apimiddleware.RequireAction(enforcer, "incarnation", "history")).Group(func(r chi.Router) {
				registerHumaIncarnationHistory(newHumaCadenceAPI(r), incH)
			})
		})
	})
	return r
}

// incNoCtxSelector — a MultiSelectorExtractor without a DB context (test): an empty
// set → RequirePermissionMulti only lets through bare/`*` roles. That's enough for
// the allow-all test (strictAllowAll returns true on bare).
func incNoCtxSelector(_ *http.Request) []map[string]string { return nil }

func incScopeAllow() *handlers.IncarnationHandler {
	// scoper=incTestScoper{unrestricted:true} → get/list/history see everything.
	db := &incTestDB{
		selectByName:  func(name string) pgx.Row { return incRow(name, "ready", "{}") },
		soulsExisting: map[string]struct{}{"web1.example.com": {}},
	}
	return handlers.NewIncarnationHandler(db, &incTestStarter{}, &incTestStarter{}, &incTestDrift{}, &incTestResolver{ok: true}, &incTestLoader{}, nil, incTestScoper{unrestricted: true}, nil)
}

// === MIDDLEWARE-AUDIT: create ===

func TestHumaIncarnation_Create_WireAndMiddlewareAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	db := &incTestDB{insertRow: func() pgx.Row { return staticRow2(time.Now(), time.Now()) }}
	incH := handlers.NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil) // runner=nil → stub mode
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, auditCap, incH)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations", strings.NewReader(`{"name":"redis-prod","service":"redis"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	var reply struct {
		Incarnation string  `json:"incarnation"`
		ApplyID     *string `json:"apply_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if reply.Incarnation != "redis-prod" || reply.ApplyID == nil || *reply.ApplyID == "" {
		t.Errorf("reply = %+v, want incarnation=redis-prod + apply_id", reply)
	}
	assertMiddlewareAudit(t, auditCap, audit.EventIncarnationCreated, "name")
}

func TestHumaIncarnation_Create_UnknownField_400(t *testing.T) {
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, nil, incScopeAllow())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations", strings.NewReader(`{"name":"x","service":"redis","bogus":1}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (unknown field); body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaIncarnation_Create_RBACDeny_403_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaIncarnationRouter(t, incEnforcer{allow: false}, auditCap, incScopeAllow())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations", strings.NewReader(`{"name":"x","service":"redis"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан on 403 — RBAC-deny не toлжен toходить to handler-а")
	}
}

// TestHumaIncarnation_RevealSecret_RBACDeny_403 — the reveal endpoint without the
// incarnation.view-secrets right is rejected with 403 by the middleware BEFORE the
// handler (NIM-74). No audit is written on 403 (RBAC-deny never reaches the
// self-audit in RevealSecretTyped).
func TestHumaIncarnation_RevealSecret_RBACDeny_403(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaIncarnationRouter(t, incEnforcer{allow: false}, auditCap, incScopeAllow())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations/redis-prod/secrets/reveal",
		strings.NewReader(`{"secret_id":"user_password","key":"alice"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан on 403 reveal — RBAC-deny не toлжен toходить to handler-а")
	}
}

// TestHumaIncarnation_RevealableSecrets_RBACDeny_403 — discovery without the
// view-secrets right → 403 via the existence-gate RequireAction.
func TestHumaIncarnation_RevealableSecrets_RBACDeny_403(t *testing.T) {
	r := humaIncarnationRouter(t, incEnforcer{allow: false}, nil, incScopeAllow())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/incarnations/redis-prod/secrets/revealable", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// === MIDDLEWARE-AUDIT: create — pre-flight assert gate (ADR-009/027 amend, form A) ===

// _ — a compile-time guard: the real *scenario.Runner MUST satisfy
// handlers.AssertPreflighter (the handler gets the pre-flighter via a type
// assertion on the runner, whose type set isn't statically checked when it's
// assigned to a ScenarioStarter). A drift between the Runner.PreflightAssert
// signature and the interface is caught here at build time, instead of a silent
// no-op pre-flight in production.
var _ handlers.AssertPreflighter = (*scenario.Runner)(nil)

// incCreateSnapshot writes a temp service snapshot with a single scenario
// `scenario/create/main.yml` (body is its contents), marked `create: true`, and
// returns the root. Needed by the multiple-create-scenarios mechanism (Phase 2):
// ResolveCreateScenarios scans art.LocalDir, so the create scenario must live on
// disk with the flag, otherwise the set is empty → a bare incarnation (the run
// won't start). body must itself carry `create: true` (the caller controls
// input/validate). t.TempDir cleans up automatically.
func incCreateSnapshot(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "scenario", "create")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.yml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write create/main.yml: %v", err)
	}
	return root
}

// incPreflightLoader — a disk-aware ServiceSnapshotLoader stub: Load returns
// LocalDir (for ResolveCreateScenarios), ReadFile reads the scenario from disk (for
// ValidateInput). localDir is the snapshot from incCreateSnapshot with create:true.
// pre-flight is the incPreflightStarter stub, the scenario itself is not read
// through this loader.
type incPreflightLoader struct{ localDir string }

func (l incPreflightLoader) Load(_ context.Context, ref artifact.ServiceRef) (*artifact.ServiceArtifact, error) {
	return &artifact.ServiceArtifact{Ref: ref, LocalDir: l.localDir}, nil
}
func (incPreflightLoader) LoadMigrationChain(_ *artifact.ServiceArtifact, _, _ int) (statemigrate.Chain, error) {
	return statemigrate.Chain{}, nil
}
func (incPreflightLoader) ListUpgrades(_ *artifact.ServiceArtifact) ([]artifact.Scenario, error) {
	return nil, nil
}
func (l incPreflightLoader) ReadFile(_ *artifact.ServiceArtifact, file string) ([]byte, error) {
	return os.ReadFile(filepath.Join(l.localDir, filepath.FromSlash(file)))
}

// incPreflightStarter — a ScenarioStarter + AssertPreflighter stub: pre-flight
// returns preflightErr (nil → passes), Start records that it was called in started.
type incPreflightStarter struct {
	preflightErr error
	started      *bool
}

func (s *incPreflightStarter) Start(_ context.Context, _ scenario.RunSpec) error {
	if s.started != nil {
		*s.started = true
	}
	return nil
}
func (s *incPreflightStarter) StartDestroy(_ context.Context, _ scenario.RunSpec) error { return nil }
func (s *incPreflightStarter) PreflightAssert(_ context.Context, _ scenario.RunSpec) error {
	return s.preflightErr
}

// TestHumaIncarnation_Create_PreflightAssertFail_422 — pre-flight assert false ON
// CREATE → 422 assert_failed, the incarnation is NOT created (insertRow is NOT
// called), Start is NOT run, no audit is written on 4xx. The KEY invariant of
// form A: rejection at the model stage BEFORE the commit, without the
// error_locked fail status.
func TestHumaIncarnation_Create_PreflightAssertFail_422(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	inserted := false
	started := false
	db := &incTestDB{insertRow: func() pgx.Row {
		inserted = true
		return staticRow2(time.Now(), time.Now())
	}}
	starter := &incPreflightStarter{
		preflightErr: scenario.ErrAssertFailed, // topology doesn't converge
		started:      &started,
	}
	loader := incPreflightLoader{localDir: incCreateSnapshot(t, incPreflightScenarioBody)}
	incH := handlers.NewIncarnationHandler(db, starter, nil, nil, &incTestResolver{ok: true}, loader, auditCap, nil, nil)
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, auditCap, incH)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations", strings.NewReader(`{"name":"redis-cluster","service":"redis","create_scenario":"create"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 assert_failed; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "assert-failed") {
		t.Errorf("body не несёт problem-type assert-failed: %s", rec.Body.String())
	}
	if inserted {
		t.Error("ИНВАРИАНТ НАРУШЕН: incarnation withздаon (insertRow вызван) on assert-fail — toлжbut быть NOT withздаbut")
	}
	if started {
		t.Error("ИНВАРИАНТ НАРУШЕН: scenario create запущен on assert-fail — Start не toлжен вызываться")
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан on 422 assert-fail — middleware не writes on 4xx")
	}
}

// incValidateLoader — a disk-aware ServiceSnapshotLoader stub with a scenario
// create/main.yml carrying a top-level validate: rule (the cross-field invariant
// "port is required if tls is disabled") + `create: true`. Load returns LocalDir
// (for ResolveCreateScenarios), ReadFile reads from disk (for ValidateInput).
// localDir is the snapshot from incCreateSnapshot.
type incValidateLoader struct{ localDir string }

func (l incValidateLoader) Load(_ context.Context, ref artifact.ServiceRef) (*artifact.ServiceArtifact, error) {
	return &artifact.ServiceArtifact{Ref: ref, LocalDir: l.localDir}, nil
}
func (incValidateLoader) LoadMigrationChain(_ *artifact.ServiceArtifact, _, _ int) (statemigrate.Chain, error) {
	return statemigrate.Chain{}, nil
}
func (incValidateLoader) ListUpgrades(_ *artifact.ServiceArtifact) ([]artifact.Scenario, error) {
	return nil, nil
}
func (l incValidateLoader) ReadFile(_ *artifact.ServiceArtifact, file string) ([]byte, error) {
	return os.ReadFile(filepath.Join(l.localDir, filepath.FromSlash(file)))
}

// incValidateScenarioBody — the contents of scenario/create/main.yml for incValidateLoader.
const incValidateScenarioBody = `name: create
create: true
input:
  tls: { type: boolean, default: false }
  port: { type: integer, default: 0 }
validate:
  - that: "input.tls || input.port > 0"
    message: "either enable tls or set a positive port"
tasks:
  - name: noop
    module: core.exec.run
    params: { cmd: "true" }
`

// incPreflightScenarioBody — a minimal create scenario with create:true (no
// input schema → ValidateInput passes) for incPreflightLoader.
const incPreflightScenarioBody = "name: create\ncreate: true\ntasks:\n  - name: noop\n    module: core.exec.run\n    params: { cmd: \"true\" }\n"

// TestHumaIncarnation_Create_ValidateRuleFail_422 — the top-level validate: rule
// fails on the request path → 422 validation-failed BEFORE the commit, the
// incarnation is NOT created, Start is NOT run, no audit is written on 4xx.
// validate: is symmetric with the pre-flight assert (form A), but input-only and
// via ValidateInput (DSL wave 2).
func TestHumaIncarnation_Create_ValidateRuleFail_422(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	inserted := false
	started := false
	db := &incTestDB{insertRow: func() pgx.Row {
		inserted = true
		return staticRow2(time.Now(), time.Now())
	}}
	starter := &incPreflightStarter{preflightErr: nil, started: &started}
	loader := incValidateLoader{localDir: incCreateSnapshot(t, incValidateScenarioBody)}
	incH := handlers.NewIncarnationHandler(db, starter, nil, nil, &incTestResolver{ok: true}, loader, auditCap, nil, nil)
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, auditCap, incH)

	rec := httptest.NewRecorder()
	// input WITHOUT port and WITHOUT tls → defaults (tls=false, port=0) → rule is false.
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations", strings.NewReader(`{"name":"redis-prod","service":"redis","create_scenario":"create"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 validation-failed; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "validation-failed") {
		t.Errorf("body не несёт problem-type validation-failed: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "either enable tls or set a positive port") {
		t.Errorf("body не несёт message правила: %s", rec.Body.String())
	}
	if inserted {
		t.Error("ИНВАРИАНТ НАРУШЕН: incarnation withздаon on validate-fail — toлжbut быть NOT withздаbut")
	}
	if started {
		t.Error("ИНВАРИАНТ НАРУШЕН: scenario create запущен on validate-fail — Start не toлжен вызываться")
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан on 422 validate-fail — middleware не writes on 4xx")
	}
}

// TestHumaIncarnation_Create_ValidateRulePass_202 — the validate: rule passes
// (port>0) → create behaves as before: 202, the incarnation is created, Start runs.
func TestHumaIncarnation_Create_ValidateRulePass_202(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	inserted := false
	started := false
	db := &incTestDB{insertRow: func() pgx.Row {
		inserted = true
		return staticRow2(time.Now(), time.Now())
	}}
	starter := &incPreflightStarter{preflightErr: nil, started: &started}
	loader := incValidateLoader{localDir: incCreateSnapshot(t, incValidateScenarioBody)}
	incH := handlers.NewIncarnationHandler(db, starter, nil, nil, &incTestResolver{ok: true}, loader, auditCap, nil, nil)
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, auditCap, incH)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations", strings.NewReader(`{"name":"redis-prod","service":"redis","create_scenario":"create","input":{"port":6379}}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (validate passes); body=%s", rec.Code, rec.Body.String())
	}
	if !inserted {
		t.Error("incarnation не withздаon on validate-pass — happy-path сломан")
	}
	if !started {
		t.Error("Start не запущен on validate-pass — happy-path сломан")
	}
}

// TestHumaIncarnation_Create_PreflightAssertPass_202 — pre-flight passes
// (topology converges) → create behaves as before: 202 + apply_id, the
// incarnation is created, Start runs. Verifies pre-flight doesn't break the
// happy path.
func TestHumaIncarnation_Create_PreflightAssertPass_202(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	started := false
	db := &incTestDB{insertRow: func() pgx.Row { return staticRow2(time.Now(), time.Now()) }}
	starter := &incPreflightStarter{preflightErr: nil, started: &started}
	loader := incPreflightLoader{localDir: incCreateSnapshot(t, incPreflightScenarioBody)}
	incH := handlers.NewIncarnationHandler(db, starter, nil, nil, &incTestResolver{ok: true}, loader, auditCap, nil, nil)
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, auditCap, incH)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations", strings.NewReader(`{"name":"redis-prod","service":"redis","create_scenario":"create"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if !started {
		t.Error("Start не запущен on assert-pass — happy-path сломан")
	}
	assertMiddlewareAudit(t, auditCap, audit.EventIncarnationCreated, "name")
}

// === MIDDLEWARE-AUDIT: unlock ===

func TestHumaIncarnation_Unlock_WireAndMiddlewareAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	db := &incTestDB{
		selectByName: func(name string) pgx.Row { return incRow(name, "error_locked", "{}") },
		unlockSelect: func() pgx.Row { return staticRow2Bytes([]byte("{}"), "error_locked") },
	}
	incH := handlers.NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, auditCap, incH)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations/redis-prod/unlock", strings.NewReader(`{"reason":"fix"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var reply struct {
		Name           string `json:"name"`
		PreviousStatus string `json:"previous_status"`
		Status         string `json:"status"`
		UnlockedByAID  string `json:"unlocked_by_aid"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if reply.Name != "redis-prod" || reply.PreviousStatus != "error_locked" || reply.Status != "ready" || reply.UnlockedByAID != "archon-alice" {
		t.Errorf("reply = %+v", reply)
	}
	assertMiddlewareAudit(t, auditCap, audit.EventIncarnationUnlocked, "previous_status")
}

func TestHumaIncarnation_Unlock_MissingReason_422(t *testing.T) {
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, nil, incScopeAllow())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations/redis-prod/unlock", strings.NewReader(`{}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (missing required reason); body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaIncarnation_Unlock_NotFound_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	db := &incTestDB{selectByName: func(string) pgx.Row { return errRow2{pgx.ErrNoRows} }}
	incH := handlers.NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, auditCap, incH)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations/nope/unlock", strings.NewReader(`{"reason":"x"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан on 404 unlock — write-путь не toлжен писать (middleware skip on 4xx)")
	}
}

// === MIDDLEWARE-AUDIT: upgrade ===

func TestHumaIncarnation_Upgrade_MiddlewareAuditClass(t *testing.T) {
	// The 500 path (loader=nil → endpoint not configured): proves that upgrade
	// does NOT write audit on NON-2xx (middleware-skip), and that the route is
	// mounted/reachable.
	auditCap := &auditCaptureWriter{}
	db := &incTestDB{selectByName: func(name string) pgx.Row { return incRow(name, "ready", "{}") }}
	incH := handlers.NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil) // loader=nil
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, auditCap, incH)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations/redis-prod/upgrade", strings.NewReader(`{"to_version":"v2"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (loader nil); body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан on 500 upgrade — middleware не toлжен писать on 5xx")
	}
}

func TestHumaIncarnation_Upgrade_MissingToVersion_422(t *testing.T) {
	db := &incTestDB{selectByName: func(name string) pgx.Row { return incRow(name, "ready", "{}") }}
	incH := handlers.NewIncarnationHandler(db, nil, nil, nil, &incTestResolver{ok: true}, &incTestLoader{}, nil, nil, nil)
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, nil, incH)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations/redis-prod/upgrade", strings.NewReader(`{}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (missing to_version); body=%s", rec.Code, rec.Body.String())
	}
}

// === MIDDLEWARE-AUDIT: run ===

func TestHumaIncarnation_Run_WireAndMiddlewareAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	db := &incTestDB{selectByName: func(name string) pgx.Row { return incRow(name, "ready", "{}") }}
	incH := handlers.NewIncarnationHandler(db, &incTestStarter{}, nil, nil, &incTestResolver{ok: true}, nil, nil, nil, nil)
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, auditCap, incH)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations/redis-prod/scenarios/converge", strings.NewReader(`{}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	var reply struct {
		ApplyID     string `json:"apply_id"`
		Incarnation string `json:"incarnation"`
		Scenario    string `json:"scenario"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if reply.ApplyID == "" || reply.Incarnation != "redis-prod" || reply.Scenario != "converge" {
		t.Errorf("reply = %+v", reply)
	}
	assertMiddlewareAudit(t, auditCap, audit.EventIncarnationScenarioStarted, "scenario")
}

// === SELF-AUDIT: rerun-last ===

func TestHumaIncarnation_RerunLast_SelfAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	db := &incTestDB{
		selectByName: func(name string) pgx.Row { return incRow(name, "error_locked", "{}") },
		unlockSelect: func() pgx.Row { return rerunSelectRow([]byte("{}"), "error_locked") },
	}
	incH := handlers.NewIncarnationHandler(db, &incTestStarter{}, nil, nil, &incTestResolver{ok: true}, nil, auditCap, nil, nil)
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, auditCap, incH)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations/redis-prod/rerun-last", strings.NewReader(`{"reason":"retry"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	var reply struct {
		ApplyID     string `json:"apply_id"`
		Incarnation string `json:"incarnation"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if reply.ApplyID == "" || reply.Incarnation != "redis-prod" {
		t.Errorf("reply = %+v", reply)
	}
	assertSelfAudit(t, auditCap, audit.EventIncarnationRerunLast, "previous_status")
}

func TestHumaIncarnation_RerunLast_NotErrorLocked_409_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	db := &incTestDB{
		selectByName: func(name string) pgx.Row { return incRow(name, "ready", "{}") },
		unlockSelect: func() pgx.Row { return rerunSelectRow([]byte("{}"), "ready") }, // not error_locked → ErrNotErrorLocked
	}
	incH := handlers.NewIncarnationHandler(db, &incTestStarter{}, nil, nil, &incTestResolver{ok: true}, nil, auditCap, nil, nil)
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, auditCap, incH)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations/redis-prod/rerun-last", strings.NewReader(`{"reason":"x"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан on 409 rerun — self-audit writesся только on успехе")
	}
}

// === SELF-AUDIT: check-drift ===

func TestHumaIncarnation_CheckDrift_WireAndSelfAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	db := &incTestDB{selectByName: func(name string) pgx.Row { return incRow(name, "ready", "{}") }}
	incH := handlers.NewIncarnationHandler(db, nil, nil, &incTestDrift{}, &incTestResolver{ok: true}, nil, auditCap, nil, nil)
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, auditCap, incH)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations/redis-prod/check-drift", strings.NewReader(`{}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertSelfAudit(t, auditCap, audit.EventIncarnationDriftChecked, "drift_summary")
}

// === SELF-AUDIT: destroy ===

func TestHumaIncarnation_Destroy_MissingAllowDestroy_400(t *testing.T) {
	// allow_destroy required boolean query — missing → 400 (huma required-param).
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, nil, incScopeAllow())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/incarnations/redis-prod", http.NoBody)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (missing allow_destroy); body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaIncarnation_Destroy_BadAllowDestroy_400(t *testing.T) {
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, nil, incScopeAllow())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/incarnations/redis-prod?allow_destroy=maybe", http.NoBody)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (non-boolean allow_destroy); body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaIncarnation_Destroy_ForceWireAndSelfAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	db := &incTestDB{
		selectByName: func(name string) pgx.Row { return incRow(name, "ready", "{}") },
		unlockSelect: func() pgx.Row { return staticRow2Bytes([]byte("{}"), "ready") }, // Destroy FOR UPDATE select (state, status)
	}
	incH := handlers.NewIncarnationHandler(db, nil, &incTestStarter{}, nil, &incTestResolver{ok: true}, &incTestLoader{}, auditCap, nil, nil)
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, auditCap, incH)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/incarnations/redis-prod?allow_destroy=true", http.NoBody)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	var reply struct {
		ApplyID string `json:"apply_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if reply.ApplyID == "" {
		t.Errorf("reply.apply_id пуст")
	}
	// destroy_started + destroy_completed are written by the service layer INSIDE
	// Destroy/DeleteAfterTeardown (SELF-AUDIT) — at least destroy_started must be there.
	if !hasAuditEvent(auditCap, audit.EventIncarnationDestroyStarted) {
		t.Errorf("incarnation.destroy_started NOT записан (SELF-AUDIT сломан); events=%v", auditEventTypes(auditCap))
	}
}

// === SELF-AUDIT: update-hosts ===

func TestHumaIncarnation_UpdateHosts_WireAndSelfAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	db := &incTestDB{
		selectByName:  func(name string) pgx.Row { return incRow(name, "ready", "{}") },
		soulsExisting: map[string]struct{}{"web1.example.com": {}},
	}
	incH := handlers.NewIncarnationHandler(db, nil, nil, nil, nil, nil, auditCap, nil, nil)
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, auditCap, incH)
	rec := httptest.NewRecorder()
	body := `{"mode":"replace","hosts":[{"sid":"web1.example.com","role":"master"}]}`
	req := httptest.NewRequest(http.MethodPatch, "/v1/incarnations/redis-prod/hosts", strings.NewReader(body))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var reply struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if reply.Name != "redis-prod" {
		t.Errorf("reply.name = %q, want redis-prod", reply.Name)
	}
	assertSelfAudit(t, auditCap, audit.EventIncarnationHostsUpdated, "mode")
}

func TestHumaIncarnation_UpdateHosts_BadMode_422(t *testing.T) {
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, nil, incScopeAllow())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/incarnations/redis-prod/hosts", strings.NewReader(`{"mode":"bogus","hosts":[]}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (enum mode); body=%s", rec.Code, rec.Body.String())
	}
}

// === READ: list / get / history (NoAudit + 400 list) ===

func TestHumaIncarnation_List_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, auditCap, incScopeAllow())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/incarnations", http.NoBody)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("read GET /v1/incarnations записал audit (%d) — read не writes", len(auditCap.Events()))
	}
}

func TestHumaIncarnation_List_BadLimit_400(t *testing.T) {
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, nil, incScopeAllow())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/incarnations?limit=99999", http.NoBody)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (CheckPageBounds out-of-range); body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaIncarnation_List_BadLimitType_400(t *testing.T) {
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, nil, incScopeAllow())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/incarnations?limit=lots", http.NoBody)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (bad int limit); body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaIncarnation_Get_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, auditCap, incScopeAllow())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/incarnations/redis-prod", http.NoBody)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("read GET /{name} записал audit — read не writes")
	}
}

// TestHumaIncarnation_Get_BareIncarnation_OmitsCreatedScenario — GUARD for Phase 2
// (handler-level, the real NULL projection of scanIncarnation through the huma
// reply): GET on a bare incarnation (created_scenario IS NULL) → 200, no panic,
// the created_scenario key is OMITTED from the JSON body (omitempty on an empty
// value). Regression = NULL breaks scanIncarnation (panic on **string) OR
// created_scenario materializes as "" on the wire (instead of being omitted).
func TestHumaIncarnation_Get_BareIncarnation_OmitsCreatedScenario(t *testing.T) {
	db := &incTestDB{
		selectByName: func(name string) pgx.Row { return incRowBare(name, "ready", "{}") },
	}
	incH := handlers.NewIncarnationHandler(db, &incTestStarter{}, &incTestStarter{}, &incTestDrift{}, &incTestResolver{ok: true}, &incTestLoader{}, nil, incTestScoper{unrestricted: true}, nil)
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, nil, incH)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/incarnations/redis-bare", http.NoBody)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if body["name"] != "redis-bare" {
		t.Errorf("name = %v, want redis-bare", body["name"])
	}
	if v, ok := body["created_scenario"]; ok {
		t.Errorf("bare-инкарonция: created_scenario присутствует в wire (=%v), want опущеbut (omitempty)", v)
	}
}

func TestHumaIncarnation_History_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, auditCap, incScopeAllow())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/incarnations/redis-prod/history", http.NoBody)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("read GET /{name}/history записал audit — read не writes")
	}
}

func TestHumaIncarnation_History_BadLimit_400(t *testing.T) {
	r := humaIncarnationRouter(t, incEnforcer{allow: true}, nil, incScopeAllow())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/incarnations/redis-prod/history?limit=lots", http.NoBody)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (bad limit); body=%s", rec.Code, rec.Body.String())
	}
}

// === spec-dump ===

func TestHumaIncarnation_SpecYAML(t *testing.T) {
	frag, err := HumaIncarnationSpecYAML()
	if err != nil {
		t.Fatalf("HumaIncarnationSpecYAML: %v", err)
	}
	for _, want := range []string{
		"createIncarnation", "listIncarnations", "getIncarnation", "getIncarnationHistory",
		"runIncarnationScenario", "unlockIncarnation", "upgradeIncarnation",
		"rerunLastIncarnation", "checkIncarnationDrift", "destroyIncarnation", "updateIncarnationHosts",
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("спека не withдержит op %q", want)
		}
	}
}

// === assert helpers ===

// assertMiddlewareAudit — S6-MIDDLEWARE-GUARD: the huma-audit-middleware (variant B)
// wrote exactly one want event with a non-empty payload containing requiredKey.
// A mutation (removing SetHumaAuditPayload in the register func / removing the
// middleware wiring) turns this test red.
func assertMiddlewareAudit(t *testing.T, cap *auditCaptureWriter, want audit.EventType, requiredKey string) {
	t.Helper()
	evs := cap.Events()
	if len(evs) != 1 {
		t.Fatalf("middleware-audit: %d withбытий, want 1 (event=%s)", len(evs), want)
	}
	ev := evs[0]
	if ev.EventType != want {
		t.Errorf("event_type = %q, want %q", ev.EventType, want)
	}
	if ev.Source != audit.SourceAPI {
		t.Errorf("source = %q, want api", ev.Source)
	}
	if ev.ArchonAID != "archon-alice" {
		t.Errorf("archon = %q, want archon-alice", ev.ArchonAID)
	}
	if len(ev.Payload) == 0 {
		t.Fatalf("payload empty — SetHumaAuditPayload не сработал (рецидив S6)")
	}
	if _, ok := ev.Payload[requiredKey]; !ok {
		t.Errorf("payload не withдержит ключ %q: %v", requiredKey, ev.Payload)
	}
}

func hasAuditEvent(cap *auditCaptureWriter, want audit.EventType) bool {
	for _, ev := range cap.Events() {
		if ev.EventType == want {
			return true
		}
	}
	return false
}

func auditEventTypes(cap *auditCaptureWriter) []audit.EventType {
	var out []audit.EventType
	for _, ev := range cap.Events() {
		out = append(out, ev.EventType)
	}
	return out
}

// === minimal fakes (api package) ===

// incTestDB — a minimal [handlers.IncarnationDB] for huma wire tests: covers
// insert (create), SelectByName (get/run/unlock/upgrade/destroy/history probe),
// unlock/rerun SELECT FOR UPDATE, souls-existence (update-hosts), list COUNT/SELECT.
type incTestDB struct {
	insertRow     func() pgx.Row
	selectByName  func(name string) pgx.Row
	unlockSelect  func() pgx.Row
	soulsExisting map[string]struct{}
}

func (f *incTestDB) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "DELETE FROM incarnation") {
		return pgconn.NewCommandTag("DELETE 1"), nil
	}
	return pgconn.CommandTag{}, nil
}

func (f *incTestDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "INSERT INTO incarnation"):
		if f.insertRow != nil {
			return f.insertRow()
		}
		return staticRow2(time.Now(), time.Now())
	case strings.Contains(sql, "SELECT state, state_schema_version, status") && strings.Contains(sql, "FOR UPDATE"):
		if f.unlockSelect != nil {
			return f.unlockSelect()
		}
		return errRow2{pgx.ErrNoRows}
	case strings.Contains(sql, "SELECT state, status") && strings.Contains(sql, "FOR UPDATE"):
		if f.unlockSelect != nil {
			return f.unlockSelect()
		}
		return errRow2{pgx.ErrNoRows}
	case strings.Contains(sql, "SELECT scenario") && strings.Contains(sql, "FROM state_history"):
		return incStaticRow{values: []any{"create", "01HFAILEDRUN00000000000000"}}
	case strings.Contains(sql, "FROM apply_runs") && strings.Contains(sql, "recipe IS NOT NULL"):
		return errRow2{pgx.ErrNoRows}
	case strings.Contains(sql, "UPDATE incarnation") && strings.Contains(sql, "RETURNING updated_at"):
		return staticRow1Time(time.Now().UTC())
	case strings.Contains(sql, "WHERE name = $1") || strings.Contains(sql, "FROM incarnation\nWHERE name"):
		if f.selectByName != nil {
			return f.selectByName(args[0].(string))
		}
		return errRow2{pgx.ErrNoRows}
	case strings.Contains(sql, "COUNT(*) FROM incarnation") || strings.Contains(sql, "COUNT(*) FROM state_history"):
		return staticRow1Int(0)
	}
	return errRow2{errors.New("incTestDB.QueryRow: unexpected SQL: " + sql)}
}

func (f *incTestDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "FROM souls WHERE sid = ANY") {
		sids, _ := args[0].([]string)
		var found []string
		for _, sid := range sids {
			if _, ok := f.soulsExisting[sid]; ok {
				found = append(found, sid)
			}
		}
		return &incStringRows{values: found}, nil
	}
	return &incEmptyRows{}, nil
}

func (f *incTestDB) BeginTx(_ context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	return &incTestTx{db: f}, nil
}

// incTestTx — a pgx.Tx wrapper around incTestDB (Unlock/Upgrade/Destroy/UpdateHosts
// run under a tx). Commit/Rollback are no-ops; unused methods panic.
type incTestTx struct{ db *incTestDB }

func (t *incTestTx) Begin(ctx context.Context) (pgx.Tx, error) {
	return t.db.BeginTx(ctx, pgx.TxOptions{})
}
func (t *incTestTx) Commit(_ context.Context) error   { return nil }
func (t *incTestTx) Rollback(_ context.Context) error { return nil }
func (t *incTestTx) CopyFrom(_ context.Context, _ pgx.Identifier, _ []string, _ pgx.CopyFromSource) (int64, error) {
	panic("incTestTx.CopyFrom: unexpected")
}
func (t *incTestTx) SendBatch(_ context.Context, _ *pgx.Batch) pgx.BatchResults {
	panic("incTestTx.SendBatch: unexpected")
}
func (t *incTestTx) LargeObjects() pgx.LargeObjects { panic("incTestTx.LargeObjects: unexpected") }
func (t *incTestTx) Prepare(_ context.Context, _, _ string) (*pgconn.StatementDescription, error) {
	panic("incTestTx.Prepare: unexpected")
}
func (t *incTestTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.db.Exec(ctx, sql, args...)
}
func (t *incTestTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.db.Query(ctx, sql, args...)
}
func (t *incTestTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.db.QueryRow(ctx, sql, args...)
}
func (t *incTestTx) Conn() *pgx.Conn { return nil }

// incRow — a SelectByName row (column order matches scanIncarnation). status/state are parameterized.
func incRow(name, status, state string) pgx.Row {
	now := time.Now()
	return incStaticRow{values: []any{
		name, "redis", "v1", int(1),
		[]byte("{}"), []byte(state), status,
		[]byte(nil), any(nil),
		now, now, []string(nil),
		[]byte("{}"), // traits
		any(nil), []byte(nil),
		"create", // created_scenario (migration 089, NOT NULL DEFAULT)
		any(nil), // applying_apply_id (ADR-068 §A1)
	}}
}

// incRowBare — like incRow, but created_scenario = NULL (a bare incarnation,
// migration 090: created without a bootstrap scenario). scanIncarnation reads the
// 16th column into **string → nil.
func incRowBare(name, status, state string) pgx.Row {
	now := time.Now()
	return incStaticRow{values: []any{
		name, "redis", "v1", int(1),
		[]byte("{}"), []byte(state), status,
		[]byte(nil), any(nil),
		now, now, []string(nil),
		[]byte("{}"), // traits
		any(nil), []byte(nil),
		any(nil), // created_scenario = NULL (bare, migration 090)
		any(nil), // applying_apply_id (ADR-068 §A1, bare → NULL)
	}}
}

// incStaticRow / helpers — local row stubs for the api package (parity with the handlers-test staticRow).
type incStaticRow struct{ values []any }

func (r incStaticRow) Scan(dest ...any) error {
	for i, d := range dest {
		switch d := d.(type) {
		case *string:
			*d = r.values[i].(string)
		case *time.Time:
			*d = r.values[i].(time.Time)
		case *int:
			*d = r.values[i].(int)
		case *int64:
			*d = r.values[i].(int64)
		case **string:
			if r.values[i] == nil {
				*d = nil
			} else {
				s := r.values[i].(string)
				*d = &s
			}
		case **time.Time:
			if r.values[i] == nil {
				*d = nil
			} else {
				tt := r.values[i].(time.Time)
				*d = &tt
			}
		case *[]byte:
			*d = r.values[i].([]byte)
		case *[]string:
			if r.values[i] == nil {
				*d = nil
			} else {
				*d = r.values[i].([]string)
			}
		}
	}
	return nil
}

func staticRow1(s string) pgx.Row                { return incStaticRow{values: []any{s}} }
func staticRow1Int(n int) pgx.Row                { return incStaticRow{values: []any{n}} }
func staticRow1Time(t time.Time) pgx.Row         { return incStaticRow{values: []any{t}} }
func staticRow2(a, b time.Time) pgx.Row          { return incStaticRow{values: []any{a, b}} }
func staticRow2Bytes(b []byte, s string) pgx.Row { return incStaticRow{values: []any{b, s}} }

// rerunSelectRow — the FOR UPDATE select for UnlockForRerun (state, status,
// created_scenario, spec; migration 089 + B1). Distinct from staticRow2Bytes:
// the rerun path scans FOUR columns (the 4th is spec, to pass spec.input through),
// plain Unlock/Destroy scan two.
func rerunSelectRow(state []byte, status string) pgx.Row {
	return incStaticRow{values: []any{state, status, "create", []byte("{}")}}
}

type errRow2 struct{ err error }

func (r errRow2) Scan(_ ...any) error { return r.err }

type incEmptyRows struct{}

func (r *incEmptyRows) Next() bool                                   { return false }
func (r *incEmptyRows) Scan(_ ...any) error                          { return nil }
func (r *incEmptyRows) Err() error                                   { return nil }
func (r *incEmptyRows) Close()                                       {}
func (r *incEmptyRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *incEmptyRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *incEmptyRows) Values() ([]any, error)                       { return nil, nil }
func (r *incEmptyRows) RawValues() [][]byte                          { return nil }
func (r *incEmptyRows) Conn() *pgx.Conn                              { return nil }

type incStringRows struct {
	values []string
	idx    int
}

func (r *incStringRows) Next() bool {
	if r.idx >= len(r.values) {
		return false
	}
	r.idx++
	return true
}
func (r *incStringRows) Scan(dest ...any) error {
	*dest[0].(*string) = r.values[r.idx-1]
	return nil
}
func (r *incStringRows) Err() error                                   { return nil }
func (r *incStringRows) Close()                                       {}
func (r *incStringRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *incStringRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *incStringRows) Values() ([]any, error)                       { return nil, nil }
func (r *incStringRows) RawValues() [][]byte                          { return nil }
func (r *incStringRows) Conn() *pgx.Conn                              { return nil }

// incTestStarter — a [handlers.ScenarioStarter] + [handlers.DestroyStarter] stub (no-op).
type incTestStarter struct{}

func (incTestStarter) Start(_ context.Context, _ scenario.RunSpec) error        { return nil }
func (incTestStarter) StartDestroy(_ context.Context, _ scenario.RunSpec) error { return nil }

// incTestResolver — a [handlers.ServiceResolver] stub. ok→resolves a fake ref.
type incTestResolver struct{ ok bool }

func (r incTestResolver) Resolve(service string) (artifact.ServiceRef, bool) {
	return artifact.ServiceRef{Name: service, Ref: "v1"}, r.ok
}

// incTestLoader — a [handlers.ServiceSnapshotLoader] stub. PrepareDestroy/HasDestroyScenario
// go through it; Load returns art with a nil Manifest (autoCreate/autoDestroy default true).
type incTestLoader struct{}

func (incTestLoader) Load(_ context.Context, ref artifact.ServiceRef) (*artifact.ServiceArtifact, error) {
	return &artifact.ServiceArtifact{Ref: ref}, nil
}
func (incTestLoader) LoadMigrationChain(_ *artifact.ServiceArtifact, _, _ int) (statemigrate.Chain, error) {
	return statemigrate.Chain{}, nil
}
func (incTestLoader) ListUpgrades(_ *artifact.ServiceArtifact) ([]artifact.Scenario, error) {
	return nil, nil
}
func (incTestLoader) ReadFile(_ *artifact.ServiceArtifact, _ string) ([]byte, error) {
	return nil, nil
}

// incTestDrift — a [handlers.DriftChecker] stub: CheckDrift returns an empty clean report.
type incTestDrift struct{}

func (incTestDrift) CheckDrift(_ context.Context, _ scenario.CheckDriftSpec) (*scenario.DriftReport, error) {
	return &scenario.DriftReport{}, nil
}
func (incTestDrift) MarkDriftStatus(_ context.Context, _ string, _ bool) error { return nil }

// incTestScoper — a [handlers.PurviewResolver] stub: unrestricted → get/list/history see everything.
type incTestScoper struct{ unrestricted bool }

func (s incTestScoper) ResolvePurview(_, _, _ string) rbac.Purview {
	return rbac.Purview{Unrestricted: s.unrestricted}
}
