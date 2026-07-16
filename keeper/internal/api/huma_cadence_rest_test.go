package api

// Guard tests for cadence-rest (PATCH/DELETE/enable/disable) on huma, the FULL-TYPED
// WRITE-SELF-AUDIT form (batch-2f, ADR-054). These routes write audit INSIDE the handler
// (PatchTyped→emitWrite / DeleteTyped→emitDeleted / SetEnabledTyped→emitEnabledToggle),
// WITHOUT audit middleware — unlike the middleware-audit domains role/operator. The guards
// prove: wire 200/204, S6-SELF-AUDIT (the handler REALLY writes an event with a non-empty
// payload on 2xx — like the cadence pilot create), NoAudit on 404/403, golden-JSON
// byte-exact, RBAC-deny→403.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// TestHumaCadence_RestReachable_ChiCoexistence — a guard on the REACHABILITY of the
// cadence-rest huma routes (PATCH/DELETE + GET/{id}+/runs) on the REAL buildRouter with a
// non-nil cadenceH (and choirH along with it). Run through the real chi-mux (NOT the
// isolated test topology humaCadenceRestRouter): chi MUST dispatch a request on the /{id}
// node to the huma handler by method (PATCH/DELETE/GET), rather than return 405. Before the
// blocker fix, the sibling subrouter r.Route("/{id}") with GET / + GET /runs shadowed the
// whole /{id} node → chi gave it PATCH/DELETE → 405 (the huma op was never called). The test
// catches a topology regression: a foreign chi.Route on the bare param node /{id} next to a
// huma op on the same node.
//
// Checked via chi.Walk (a router-tree walk), NOT Match/ServeHTTP: enforcer=nil +
// verifier=nil (like TestHumaSoul_Exec_ChiCoexistence) — the request path would hit 401
// (auth chain without a verifier) BEFORE RBAC/huma, and chi.Routes.Match gives a FALSE-TRUE
// on the shadowed node (the /{id} node exists from the sibling subrouter, Match reports its
// pattern, but the node's actual handler answers only the subrouter's methods → in ServeHTTP
// that is 405). The reliable signal is METHOD REGISTRATION in the tree: chi.Walk enumerates
// exactly the registered method+pattern. If PATCH/DELETE /v1/cadences/{id} is not in the
// walk — the huma op is not mounted → 405 in prod. The test requires all four /{id} methods
// (GET/{id}, GET/{id}/runs, PATCH, DELETE) exactly once. Before the blocker fix PATCH/DELETE
// are absent from the walk (the sibling chi.Route swallowed the /{id} node) → the test is RED.
func TestHumaCadence_RestReachable_ChiCoexistence(t *testing.T) {
	cadenceH := handlers.NewCadenceHandler(foundCadenceStore(), nil, nil, nil, nil, nil, 0, nil)
	h := buildRouter(
		nil, // verifier
		nil, // healthH
		stubOperatorHandler(t),
		handlers.NewIncarnationHandler(nil, nil, nil, nil, nil, nil, nil, nil, nil),
		handlers.NewSoulHandler(nil, nil, nil, nil),
		stubRoleHandler(t), stubSynodHandler(t), stubSigilHandler(t), stubSigilKeyHandler(t),
		stubServiceHandler(t), nil, stubAugurHandler(t), stubOracleHandler(t),
		nil,                                     // pushH
		nil,                                     // pushProviderH
		nil,                                     // providerH
		nil,                                     // profileH
		nil,                                     // errandH
		nil,                                     // voyageH
		cadenceH,                                // cadenceH non-nil → cadence /{id} routes are mounted
		nil,                                     // auditH
		handlers.NewChoirHandler(nil, nil, nil), // choirH non-nil along with it
		nil,                                     // heraldH
		handlers.NewModuleCatalogHandler(nil, nil),
		handlers.NewModuleFormPrepHandler(nil, nil),
		handlers.NewPermissionCatalogHandler(nil),
		handlers.NewEventTypeCatalogHandler(nil),
		handlers.NewHeraldTypeCatalogHandler(nil),
		handlers.NewMyPermissionsHandler(nil, nil),
		nil,                                  // enforcer (nil: RBAC is not invoked — checked via the router tree)
		nil,                                  // auditWriter
		nil,                                  // metricsHTTP
		nil,                                  // tollDegraded
		nil,                                  // tempoLimiter
		nil,                                  // tempoMetrics
		nil,                                  // tempoVoyageCreateLimits
		nil,                                  // tempoVoyagePreviewLimits
		false,                                // webUIEnabled — /ui is out of scope for the cadence routing test
		nil,                                  // ldapAuth (LDAP not configured in the test)
		nil,                                  // oidcAuth (OIDC not configured in the test)
		nil,                                  // loginGuard (anti-bruteforce off in the test)
		apimiddleware.AuthLoginLimitConfig{}, // loginLimitCfg
		nil,                                  // soulStatsStaleFn (default 90s in the test)
		nil,                                  // clusterH (cluster view not mounted in the test)
		nil,                                  // runEventsDeps (ADR-068 §A3 — not tested here)
		nil,                                  // logger
	)
	routes, ok := h.(chi.Routes)
	if !ok {
		t.Fatalf("buildRouter вернул %T, не chi.Routes", h)
	}

	// chi.Walk: each /{id} method is registered exactly once. Missing PATCH/DELETE = the
	// blocker (sibling chi.Route shadowing); a duplicate = a mount collision. Teardown T1:
	// GET /v1/cadences (list) on the group root is a separate huma op; it does not shadow
	// the /{id} node and does not conflict with POST / (create). getListCount=1 + intact
	// /{id} nodes = list is mounted reachably WITHOUT a shadow.
	var patchCount, deleteCount, getIDCount, getRunsCount, getListCount int
	if err := chi.Walk(routes, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		switch {
		case method == http.MethodPatch && pattern == "/v1/cadences/{id}":
			patchCount++
		case method == http.MethodDelete && pattern == "/v1/cadences/{id}":
			deleteCount++
		case method == http.MethodGet && pattern == "/v1/cadences/{id}":
			getIDCount++
		case method == http.MethodGet && pattern == "/v1/cadences/{id}/runs":
			getRunsCount++
		case method == http.MethodGet && pattern == "/v1/cadences/":
			getListCount++
		}
		return nil
	}); err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	if patchCount != 1 {
		t.Errorf("PATCH /v1/cadences/{id} встретился %d раз, want 1 (0 = блокер: sibling chi.Route затеняет → 405)", patchCount)
	}
	if deleteCount != 1 {
		t.Errorf("DELETE /v1/cadences/{id} встретился %d раз, want 1 (0 = блокер: sibling chi.Route затеняет → 405)", deleteCount)
	}
	if getIDCount != 1 {
		t.Errorf("GET /v1/cadences/{id} встретился %d раз, want 1 (read-роут on huma)", getIDCount)
	}
	if getRunsCount != 1 {
		t.Errorf("GET /v1/cadences/{id}/runs встретился %d раз, want 1 (read-роут on huma)", getRunsCount)
	}
	if getListCount != 1 {
		t.Errorf("GET /v1/cadences/ встретился %d раз, want 1 (Teardown T1: list-роут on huma, WITHOUT shadow с /{id})", getListCount)
	}
}

// cadenceRestTestID — a valid ULID (26 Crockford-base32) for the path-{id} guards.
const cadenceRestTestID = "01HZ0000000000000000000000"

// cadenceRestStoredRow — a pgx.Row for cadence.scanCadence (27 dest, parity with the
// handler test's cadenceFullRow): the minimal stored command-kind row (cron +
// service-target), enough for a PATCH/Get round-trip. service-target + incReader=nil →
// per-target scope is skipped after the bare-check (parity newCadenceHandler).
type cadenceRestStoredRow struct{ id string }

func (r cadenceRestStoredRow) Scan(dest ...any) error {
	now := time.Now().UTC()
	*dest[0].(*string) = r.id
	*dest[1].(*string) = "hourly"
	*dest[2].(*bool) = true
	*dest[3].(*string) = "cron"
	*dest[4].(**int) = nil // interval_seconds
	cron := "0 * * * *"
	*dest[5].(**string) = &cron // cron_expr
	*dest[6].(*string) = "queue"
	*dest[7].(*string) = "command"
	*dest[8].(**string) = nil // scenario_name
	mod := "core.cmd.shell"
	*dest[9].(**string) = &mod // module
	*dest[10].(*json.RawMessage) = json.RawMessage(`{"coven":["prod"]}`)
	*dest[11].(*[]byte) = []byte(`{}`)
	*dest[12].(**string) = nil // batch_mode
	*dest[13].(**int) = nil    // batch_size
	*dest[14].(**int) = nil    // batch_percent
	*dest[15].(**int) = nil    // concurrency
	*dest[16].(**int) = nil    // fail_threshold
	*dest[17].(**int) = nil    // fail_threshold_percent
	*dest[18].(**float64) = nil
	*dest[19].(**float64) = nil
	*dest[20].(**bool) = nil   // require_alive
	*dest[21].(**string) = nil // on_failure
	*dest[22].(**time.Time) = nil
	*dest[23].(**time.Time) = nil
	*dest[24].(*string) = "archon-alice"
	*dest[25].(*time.Time) = now
	*dest[26].(*time.Time) = now
	return nil
}

// humaCadenceRestRouter mounts the cadence-rest huma routes (PATCH/DELETE/enable/disable)
// exactly per the router.go wiring: RequirePermission on each group + a huma operation with
// the FULL path /{id}[/...] on the /v1/cadences group. store/enforcer/auditW are
// parameterized; selectByID is tuned per case (found/not-found).
func humaCadenceRestRouter(t *testing.T, enforcer apimiddleware.PermissionChecker, auditW audit.Writer, store *strictFakeCadenceStore) *chi.Mux {
	t.Helper()
	installHumaErrorOverride()
	cadenceH := handlers.NewCadenceHandler(store, nil, nil, enforcer, auditW, nil, 0, nil)

	r := chi.NewRouter()
	injectClaims := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := apimiddleware.InjectClaimsForTest(req.Context(), &keeperjwt.Claims{Subject: "archon-alice"})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
	r.Route("/v1", func(r chi.Router) {
		r.Route("/cadences", func(r chi.Router) {
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "cadence", "list", apimiddleware.NoSelector)).
				Group(func(r chi.Router) { registerHumaCadenceGet(newHumaCadenceAPI(r), cadenceH) })
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "incarnation", "history", apimiddleware.NoSelector)).
				Group(func(r chi.Router) { registerHumaCadenceRuns(newHumaCadenceAPI(r), cadenceH) })
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "cadence", "update", apimiddleware.NoSelector)).
				Group(func(r chi.Router) { registerHumaCadencePatch(newHumaCadenceAPI(r), cadenceH) })
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "cadence", "delete", apimiddleware.NoSelector)).
				Group(func(r chi.Router) { registerHumaCadenceDelete(newHumaCadenceAPI(r), cadenceH) })
			r.With(injectClaims, apimiddleware.RequireAnyPermission(enforcer, "cadence", []string{"enable", "update"}, apimiddleware.NoSelector)).
				Group(func(r chi.Router) { registerHumaCadenceEnable(newHumaCadenceAPI(r), cadenceH) })
			r.With(injectClaims, apimiddleware.RequireAnyPermission(enforcer, "cadence", []string{"disable", "update"}, apimiddleware.NoSelector)).
				Group(func(r chi.Router) { registerHumaCadenceDisable(newHumaCadenceAPI(r), cadenceH) })
		})
	})
	return r
}

// foundCadenceStore — a store that returns a stored row on Get (PATCH/enable/delete OK).
func foundCadenceStore() *strictFakeCadenceStore {
	return &strictFakeCadenceStore{selectByID: func(id string) pgx.Row { return cadenceRestStoredRow{id: id} }}
}

// --- PATCH ---

// TestHumaCadenceRest_Patch_WireOK — PATCH 200 + cadenceDTO (read-modify-write via
// PatchTyped).
func TestHumaCadenceRest_Patch_WireOK(t *testing.T) {
	r := humaCadenceRestRouter(t, strictAllowAll{}, nil, foundCadenceStore())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/cadences/"+cadenceRestTestID, strings.NewReader(`{"name":"renamed"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var dto struct {
		CadenceID string `json:"cadence_id"`
		Name      string `json:"name"`
		Kind      string `json:"kind"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if dto.CadenceID != cadenceRestTestID || dto.Name != "renamed" || dto.Kind != "command" {
		t.Errorf("dto = %+v, want cadence_id=%s name=renamed kind=command", dto, cadenceRestTestID)
	}
}

// TestHumaCadenceRest_Patch_SelfAuditRecorded — S6-SELF-AUDIT-GUARD: PATCH via huma writes
// cadence.updated with a non-empty payload INSIDE PatchTyped (handler, NOT middleware).
func TestHumaCadenceRest_Patch_SelfAuditRecorded(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaCadenceRestRouter(t, strictAllowAll{}, auditCap, foundCadenceStore())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/cadences/"+cadenceRestTestID, strings.NewReader(`{"name":"renamed"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertSelfAudit(t, auditCap, audit.EventCadenceUpdated, "cadence_id")
}

// TestHumaCadenceRest_Patch_NotFound_NoAudit — 404 (no cadence) does NOT write audit
// (PatchTyped never reaches emitWrite).
func TestHumaCadenceRest_Patch_NotFound_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaCadenceRestRouter(t, strictAllowAll{}, auditCap, &strictFakeCadenceStore{}) // selectByID=nil → 404
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/cadences/"+cadenceRestTestID, strings.NewReader(`{"name":"x"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан on 404 (%d withбытий) — write-путь не toлжен писать", len(auditCap.Events()))
	}
}

// TestHumaCadenceRest_Patch_RBACDeny_403 — RequirePermission(cadence.update) rejects
// before the huma handler → 403, no audit.
func TestHumaCadenceRest_Patch_RBACDeny_403(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaCadenceRestRouter(t, strictDenyAll{}, auditCap, foundCadenceStore())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/cadences/"+cadenceRestTestID, strings.NewReader(`{"name":"x"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан on 403 — RBAC-deny не toлжен toходить to handler-а")
	}
}

// TestHumaCadenceRest_Patch_UnknownField_400 — additionalProperties:false (huma) →
// unknown field → 400.
func TestHumaCadenceRest_Patch_UnknownField_400(t *testing.T) {
	r := humaCadenceRestRouter(t, strictAllowAll{}, nil, foundCadenceStore())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/cadences/"+cadenceRestTestID, strings.NewReader(`{"bogus":1}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (unknown field); body=%s", rec.Code, rec.Body.String())
	}
}

// --- DELETE ---

// TestHumaCadenceRest_Delete_WireOK_204 — DELETE 204 No Content.
func TestHumaCadenceRest_Delete_WireOK_204(t *testing.T) {
	r := humaCadenceRestRouter(t, strictAllowAll{}, nil, foundCadenceStore())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/cadences/"+cadenceRestTestID, http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("204 с непустым bodyм: %s", rec.Body.String())
	}
}

// TestHumaCadenceRest_Delete_SelfAuditRecorded — S6-SELF-AUDIT-GUARD: DELETE writes
// cadence.deleted INSIDE DeleteTyped.
func TestHumaCadenceRest_Delete_SelfAuditRecorded(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaCadenceRestRouter(t, strictAllowAll{}, auditCap, foundCadenceStore())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/cadences/"+cadenceRestTestID, http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	assertSelfAudit(t, auditCap, audit.EventCadenceDeleted, "cadence_id")
}

// TestHumaCadenceRest_Delete_NotFound_NoAudit — DELETE 0 rows → 404, no audit.
func TestHumaCadenceRest_Delete_NotFound_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	store := &strictFakeCadenceStore{deleteNoRow: true}
	r := humaCadenceRestRouter(t, strictAllowAll{}, auditCap, store)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/cadences/"+cadenceRestTestID, http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан on 404 delete — write-путь не toлжен писать")
	}
}

// --- enable/disable ---

// TestHumaCadenceRest_Enable_WireAndAudit — enable 200 + {cadence_id,enabled:true} +
// S6-SELF-AUDIT cadence.updated.
func TestHumaCadenceRest_Enable_WireAndAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaCadenceRestRouter(t, strictAllowAll{}, auditCap, foundCadenceStore())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/cadences/"+cadenceRestTestID+"/enable", http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var reply struct {
		CadenceID string `json:"cadence_id"`
		Enabled   bool   `json:"enabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if reply.CadenceID != cadenceRestTestID || !reply.Enabled {
		t.Errorf("reply = %+v, want cadence_id=%s enabled=true", reply, cadenceRestTestID)
	}
	assertSelfAudit(t, auditCap, audit.EventCadenceUpdated, "cadence_id")
}

// TestHumaCadenceRest_Disable_WireAndAudit — disable 200 + enabled:false + audit.
func TestHumaCadenceRest_Disable_WireAndAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaCadenceRestRouter(t, strictAllowAll{}, auditCap, foundCadenceStore())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/cadences/"+cadenceRestTestID+"/disable", http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var reply struct {
		Enabled bool `json:"enabled"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &reply)
	if reply.Enabled {
		t.Errorf("disable enabled = true, want false")
	}
	assertSelfAudit(t, auditCap, audit.EventCadenceUpdated, "cadence_id")
}

// TestHumaCadenceRest_Enable_NotFound_NoAudit — enable of a nonexistent one → 404, no audit.
func TestHumaCadenceRest_Enable_NotFound_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	store := &strictFakeCadenceStore{setEnabledNoRow: true}
	r := humaCadenceRestRouter(t, strictAllowAll{}, auditCap, store)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/cadences/"+cadenceRestTestID+"/enable", http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан on 404 enable")
	}
}

// TestHumaCadenceRest_Enable_GoldenWire — golden-JSON byte-exact 200-reply enable.
func TestHumaCadenceRest_Enable_GoldenWire(t *testing.T) {
	r := humaCadenceRestRouter(t, strictAllowAll{}, nil, foundCadenceStore())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/cadences/"+cadenceRestTestID+"/enable", http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := normalizeJSONKeys(t, rec.Body.Bytes())
	const golden = `{"cadence_id":"01HZ0000000000000000000000","enabled":true}`
	if got != golden {
		t.Errorf("GOLDEN wire-дрейф enable-reply:\n got  = %s\n want = %s\n(onбор ключей/$schema fromменился — проверь cadenceToggleOutput/newHumaCadenceAPI)", got, golden)
	}
}

// --- GET /{id} (read) ---

// TestHumaCadenceRest_Get_WireOK — GET /{id} 200 + cadenceDTO (read-tier huma, no audit).
// Proves the read route is reachable on huma (part of the blocker fix).
func TestHumaCadenceRest_Get_WireOK(t *testing.T) {
	r := humaCadenceRestRouter(t, strictAllowAll{}, nil, foundCadenceStore())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/cadences/"+cadenceRestTestID, http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var dto struct {
		CadenceID string `json:"cadence_id"`
		Kind      string `json:"kind"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if dto.CadenceID != cadenceRestTestID || dto.Kind != "command" {
		t.Errorf("dto = %+v, want cadence_id=%s kind=command", dto, cadenceRestTestID)
	}
}

// TestHumaCadenceRest_Get_NotFound_404 — GET /{id} of a nonexistent one → 404 (read route).
func TestHumaCadenceRest_Get_NotFound_404(t *testing.T) {
	r := humaCadenceRestRouter(t, strictAllowAll{}, nil, &strictFakeCadenceStore{}) // selectByID=nil → 404
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/cadences/"+cadenceRestTestID, http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHumaCadenceRest_Get_GoldenWire — golden-JSON byte-exact GET /{id} 200 reply.
// Keys/omitempty/absence of $schema are pinned. Non-deterministic fields
// (created_at/updated_at) are normalized; cadence_id is fixed (path-{id}). The reference
// matches legacy strict GetCadence (the same toCadenceDTO shape).
func TestHumaCadenceRest_Get_GoldenWire(t *testing.T) {
	r := humaCadenceRestRouter(t, strictAllowAll{}, nil, foundCadenceStore())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/cadences/"+cadenceRestTestID, http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := normalizeCadenceTimestamps(t, rec.Body.Bytes())
	const golden = `{"cadence_id":"01HZ0000000000000000000000","created_at":"TS","created_by_aid":"archon-alice","cron_expr":"0 * * * *","enabled":true,"kind":"command","module":"core.cmd.shell","name":"hourly","overlap_policy":"queue","schedule_kind":"cron","target":{"coven":["prod"]},"updated_at":"TS"}`
	if got != golden {
		t.Errorf("GOLDEN wire-дрейф GET {id}:\n got  = %s\n want = %s\n(onбор ключей/$schema fromменился — проверь cadenceGetOutput/toCadenceDTO)", got, golden)
	}
}

// TestHumaCadenceRest_Runs_GoldenWire — golden-JSON byte-exact GET /{id}/runs 200 (empty
// set: the store returns 0 runs). Pins the envelope shape items/offset/limit/total
// (sharedapi.PagedResponse) == legacy strict ListCadenceRuns.
func TestHumaCadenceRest_Runs_GoldenWire(t *testing.T) {
	r := humaCadenceRestRouter(t, strictAllowAll{}, nil, foundCadenceStore())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/cadences/"+cadenceRestTestID+"/runs", http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := normalizeJSONKeys(t, rec.Body.Bytes())
	const golden = `{"items":[],"limit":50,"offset":0,"total":0}`
	if got != golden {
		t.Errorf("GOLDEN wire-дрейф GET {id}/runs:\n got  = %s\n want = %s\n(envelope-form fromменилась — проверь cadenceRunsOutput/CadenceRunsReply)", got, golden)
	}
}

// TestHumaCadenceRest_Runs_BadLimit_400 — out-of-range limit → 400 (CheckPageBounds in
// RunsTyped, parity legacy ParsePage; NOT huma min/max).
func TestHumaCadenceRest_Runs_BadLimit_400(t *testing.T) {
	r := humaCadenceRestRouter(t, strictAllowAll{}, nil, foundCadenceStore())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/cadences/"+cadenceRestTestID+"/runs?limit=99999", http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (out-of-range limit); body=%s", rec.Code, rec.Body.String())
	}
}

// --- GET / (list) — Teardown T1 ---

// humaCadenceListRouter mounts GET /v1/cadences (list) exactly per the router.go wiring:
// RequirePermission(cadence.list) on the group + a huma operation with path "/" on the
// /v1/cadences group. store/enforcer are parameterized.
func humaCadenceListRouter(t *testing.T, enforcer apimiddleware.PermissionChecker, store *strictFakeCadenceStore) *chi.Mux {
	t.Helper()
	installHumaErrorOverride()
	cadenceH := handlers.NewCadenceHandler(store, nil, nil, enforcer, nil, nil, 0, nil)

	r := chi.NewRouter()
	injectClaims := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := apimiddleware.InjectClaimsForTest(req.Context(), &keeperjwt.Claims{Subject: "archon-alice"})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
	r.Route("/v1", func(r chi.Router) {
		r.Route("/cadences", func(r chi.Router) {
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "cadence", "list", apimiddleware.NoSelector)).
				Group(func(r chi.Router) { registerHumaCadenceList(newHumaCadenceAPI(r), cadenceH) })
		})
	})
	return r
}

// TestHumaCadenceRest_List_GoldenWire — golden-JSON byte-exact GET /v1/cadences 200 (empty
// set: the store returns 0 rows/COUNT=0). Pins the envelope shape items/offset/limit/total
// (sharedapi.PagedResponse) == legacy strict ListCadences: items non-nil [], no $schema,
// default offset=0/limit=50.
func TestHumaCadenceRest_List_GoldenWire(t *testing.T) {
	r := humaCadenceListRouter(t, strictAllowAll{}, &strictFakeCadenceStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/cadences", http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := normalizeJSONKeys(t, rec.Body.Bytes())
	const golden = `{"items":[],"limit":50,"offset":0,"total":0}`
	if got != golden {
		t.Errorf("GOLDEN wire-дрейф GET /v1/cadences (list):\n got  = %s\n want = %s\n(envelope-form fromменилась — проверь cadenceListOutput/CadenceListReply)", got, golden)
	}
}

// TestHumaCadenceRest_List_BadLimit_400 — out-of-range limit → 400 (CheckPageBounds in
// ListTyped, parity legacy ParsePage; NOT huma min/max).
func TestHumaCadenceRest_List_BadLimit_400(t *testing.T) {
	r := humaCadenceListRouter(t, strictAllowAll{}, &strictFakeCadenceStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/cadences?limit=99999", http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (out-of-range limit); body=%s", rec.Code, rec.Body.String())
	}
}

// TestHumaCadenceRest_List_BadEnabledEnum_422 — enabled outside {true,false} → 422
// (huma schema-validate enum mismatch; parity legacy "query 'enabled' must be …").
func TestHumaCadenceRest_List_BadEnabledEnum_422(t *testing.T) {
	r := humaCadenceListRouter(t, strictAllowAll{}, &strictFakeCadenceStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/cadences?enabled=maybe", http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (bad enabled enum); body=%s", rec.Code, rec.Body.String())
	}
}

// TestHumaCadenceRest_List_BadKindEnum_422 — kind outside {scenario,command} → 422
// (huma schema-validate enum mismatch; parity legacy ValidKind → 422).
func TestHumaCadenceRest_List_BadKindEnum_422(t *testing.T) {
	r := humaCadenceListRouter(t, strictAllowAll{}, &strictFakeCadenceStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/cadences?kind=bogus", http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (bad kind enum); body=%s", rec.Code, rec.Body.String())
	}
}

// TestHumaCadenceRest_List_RBACDeny_403 — RequirePermission(cadence.list) rejects before
// the huma handler → 403.
func TestHumaCadenceRest_List_RBACDeny_403(t *testing.T) {
	r := humaCadenceListRouter(t, strictDenyAll{}, &strictFakeCadenceStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/cadences", http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// normalizeCadenceTimestamps replaces the non-deterministic created_at/updated_at with the
// placeholder "TS" and re-pours through a map → sorted-marshal (golden byte-exact with a
// fixed key set; created_at/updated_at = "TS" in the reference). The placeholder has no
// special characters — json.Marshal does not HTML-escape it.
func normalizeCadenceTimestamps(t *testing.T, raw []byte) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("reply не JSON-object: %v; raw=%s", err, raw)
	}
	for _, k := range []string{"created_at", "updated_at"} {
		if _, ok := m[k]; ok {
			m[k] = "TS"
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	return string(out)
}

// assertSelfAudit — the shared S6-SELF-AUDIT-GUARD: the capture writer received EXACTLY an
// event of the required type with a non-empty payload containing requiredKey, from
// archon-alice. Proves the huma route carried the self-audit (emit INSIDE the handler)
// through to the writer on 2xx.
func assertSelfAudit(t *testing.T, cap *auditCaptureWriter, want audit.EventType, requiredKey string) {
	t.Helper()
	evs := cap.Events()
	if len(evs) == 0 {
		t.Fatalf("audit NOT записан on успешbutм 2xx — huma сломал self-audit write-путь (%s)", want)
	}
	ev := evs[0]
	if ev.EventType != want {
		t.Errorf("event_type = %q, want %q", ev.EventType, want)
	}
	if ev.ArchonAID != "archon-alice" {
		t.Errorf("archon_aid = %q, want archon-alice", ev.ArchonAID)
	}
	if len(ev.Payload) == 0 {
		t.Error("audit payload empty — self-audit потерял toменный payload")
	}
	if ev.Payload[requiredKey] == nil {
		t.Errorf("audit payload без %q: %+v", requiredKey, ev.Payload)
	}
}

// normalizeJSONKeys re-pours a JSON object through a map → a canonical marshal (keys sorted
// deterministically), preserving key presence/absence and any extra fields (e.g. $schema —
// it will surface in the diff).
func normalizeJSONKeys(t *testing.T, raw []byte) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("reply не JSON-object: %v; raw=%s", err, raw)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	return string(out)
}
