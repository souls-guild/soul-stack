package api

// Guard tests for the revoked perimeter (NIM-421 / NIM-356, ADR-014 Amendment
// 2026-05-27). SECURITY invariant:
//
//	A JWT whose Archon has been revoked is refused with 401
//	`operator-revoked-token` on EVERY authenticated route — not 403, not 200.
//
// The table is not hand-written: it is the chi tree itself, walked route by
// route. Hand-listing the paths is what let the defect survive — NIM-421 found
// three different answers across the surface (401 on the RequirePermission
// routes, 403 "lacks permission" on the RequireAction ones, and a plain 200 on
// the four catalog routes that carry no RBAC gate at all), and a hand-written
// list would have covered whichever group its author happened to think of. A
// walked tree also covers routes added after this test was written.
//
// The pair of tests is deliberate. Revoked→401 alone would be satisfied by a
// middleware that refuses everyone; active→never-revoked-401 alone would be
// satisfied by no middleware at all. Both must hold.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/api/health"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/shared/audit"
)

const (
	revokedGateAID = "archon-revoked-gate"
	activeGateAID  = "archon-active-gate"
)

// emptyRBAC — an enforcer over an empty snapshot: nobody is revoked, nobody
// holds anything. For routers whose test does not care about RBAC but which now
// carry [apimiddleware.RejectRevoked] and therefore need a non-nil provider.
func emptyRBAC(t *testing.T) RBACProvider {
	t.Helper()
	return revokedGateRBAC(t, nil)
}

// revokedGateRBAC — an enforcer whose revoked projection holds exactly the given
// AIDs, and whose single role `cluster-admin` grants `*` to both gate AIDs. The
// grant matters: without it an ACTIVE operator would be denied by the RBAC gates
// anyway, and the negative control could not distinguish "refused as revoked"
// from "refused for lack of rights" on the routes that have a gate.
func revokedGateRBAC(t *testing.T, revoked []string) RBACProvider {
	t.Helper()
	snap := &rbac.Snapshot{
		Roles:      map[string][]string{"cluster-admin": {"*"}},
		Membership: map[string][]string{revokedGateAID: {"cluster-admin"}, activeGateAID: {"cluster-admin"}},
		Revoked:    make(map[string]time.Time, len(revoked)),
	}
	for _, aid := range revoked {
		snap.Revoked[aid] = time.Unix(1700000000, 0).UTC()
	}
	enf, err := rbac.NewEnforcerFromSnapshot(snap)
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	return enf
}

// revokedGateToken issues a valid JWT for the AID (same key/issuer as
// [metaVerifier]) — valid by signature and exp, which is the whole point: the
// only thing wrong with it is that its Archon is gone.
func revokedGateToken(t *testing.T, aid string) string {
	t.Helper()
	iss, err := keeperjwt.NewIssuer([]byte(metaSigningKey), metaIssuer)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	tok, err := iss.Issue(aid, []string{"cluster-admin"}, time.Hour, false)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return tok
}

// revokedGateRouter — the REAL buildRouter with a real verifier and the given
// RBAC provider, wired with as many handlers non-nil as the constructors allow,
// so the walk below sees as much of the perimeter as possible. Handler
// dependencies are stubs: a request that gets past the revoked gate reaches one
// of them and panics — which this test reports as the failure it is.
func revokedGateRouter(t *testing.T, enforcer RBACProvider) http.Handler {
	t.Helper()
	return revokedGateRouterWith(t, enforcer, handlers.NewSoulHandler(nil, nil, nil, nil), nil)
}

// revokedGateRouterWith is [revokedGateRouter] with the two dependencies a
// caller may need ALIVE rather than stubbed: the Soul handler and the audit
// writer. The route-permission guard (NIM-386) needs both — its positive
// control has to reach the handler and get a real answer, because a stub that
// panics on contact cannot tell "the gate let the request through" from "the
// route is not mounted at all", and those are the two outcomes the guard exists
// to separate.
func revokedGateRouterWith(t *testing.T, enforcer RBACProvider, soulH *handlers.SoulHandler, auditW audit.Writer) http.Handler {
	t.Helper()
	verifier, err := keeperjwt.NewVerifier([]byte(metaSigningKey), metaIssuer)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return buildRouter(
		verifier,
		health.NewHandler(health.Deps{}),
		stubOperatorHandler(t),
		handlers.NewIncarnationHandler(nil, nil, nil, nil, nil, nil, nil, nil),
		soulH,
		handlers.TelemetrySpecStub(),
		stubRoleHandler(t),
		stubSynodHandler(t),
		stubSigilHandler(t),
		stubSigilKeyHandler(t),
		stubServiceHandler(t),
		stubProvisioningPolicyHandler(t),
		stubSettingsHandler(t),
		stubAugurHandler(t),
		stubOracleHandler(t),
		// The opt-in domains are mounted here on purpose. They are opt-in in
		// *production* (a deployment without SSH providers never mounts push), but
		// leaving them nil here would mean the sweep below never walks them — and
		// the claim this file makes is about every authenticated route, not about
		// the two thirds a default deployment happens to enable. Feature-gated is
		// exactly where a gate is most likely to be missed.
		handlers.PushSpecStub(),
		handlers.PushProviderSpecStub(),
		handlers.ProviderSpecStub(),
		handlers.ProfileSpecStub(),
		handlers.ErrandSpecStub(),
		handlers.VoyageSpecStub(),
		handlers.CadenceSpecStub(),
		handlers.AuditSpecStub(),
		handlers.ChoirSpecStub(),
		handlers.HeraldSpecStub(),
		handlers.NewModuleCatalogHandler(nil, nil),
		handlers.NewModuleFormPrepHandler(nil, nil),
		handlers.NewPermissionCatalogHandler(nil),  // /v1/permissions — no RBAC gate; answered 200 before NIM-421
		handlers.NewEventTypeCatalogHandler(nil),   // /v1/event-types — ditto
		handlers.NewHeraldTypeCatalogHandler(nil),  // /v1/herald-types — ditto
		handlers.NewMyPermissionsHandler(nil, nil), // /v1/me/permissions — ditto
		enforcer,
		auditW,                               // auditWriter
		nil,                                  // metricsHTTP
		nil,                                  // tollDegraded
		nil,                                  // tempoLimiter
		nil,                                  // tempoMetrics
		nil,                                  // tempoVoyageCreateLimits
		nil,                                  // tempoVoyagePreviewLimits
		false,                                // webUIEnabled — /ui is static and public, outside the authenticated perimeter
		nil,                                  // ldapAuth — /auth/* is pre-authentication, outside this perimeter
		nil,                                  // oidcAuth — ditto
		nil,                                  // authToken — ditto
		AuthMethodsDeps{},                    // authMethods
		nil,                                  // loginGuard
		apimiddleware.AuthLoginLimitConfig{}, // loginLimitCfg
		nil,                                  // soulStatsStaleFn
		handlers.ClusterSpecStub(),           // opt-in domain — mounted, see above
		&runEventsDeps{},                     // runEventsDeps — SSE run-events (ADR-068 §A3)
		&consoleWSDeps{},                     // consoleWSDeps — the console WebSocket (NIM-143)
		handlers.ConsoleRecordingSpecStub(),  // consoleRecordingH — playback (NIM-148)
		nil,                                  // logger
	)
}

// authenticatedRoutes — every (method, concrete path) pair behind a JWT: the
// whole /v1 subtree plus the two spec routes, which sit outside /v1 but are
// authenticated all the same. Path params are filled with a literal; the value
// is irrelevant because no request under test is meant to reach a handler.
func authenticatedRoutes(t *testing.T, h http.Handler) []struct{ method, path string } {
	t.Helper()
	routes, ok := h.(chi.Routes)
	if !ok {
		t.Fatalf("buildRouter returned %T, does not implement chi.Routes", h)
	}
	var out []struct{ method, path string }
	seen := make(map[string]struct{})
	err := chi.Walk(routes, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(pattern, "/v1/") && pattern != "/openapi.yaml" && pattern != "/openapi.json" {
			return nil
		}
		// The /v1/* catch-all is the 404 fallback, registered by chi under every
		// method. It is authenticated too — it sits under the same chain — so it
		// stays in, collapsed to one concrete path.
		path := strings.ReplaceAll(normalizePath(pattern), "/*", "/no-such-endpoint")
		for {
			open := strings.Index(path, "{")
			if open < 0 {
				break
			}
			closing := strings.Index(path[open:], "}")
			if closing < 0 {
				break
			}
			path = path[:open] + "x" + path[open+closing+1:]
		}
		key := strings.ToUpper(method) + " " + path
		if _, dup := seen[key]; dup {
			return nil
		}
		seen[key] = struct{}{}
		out = append(out, struct{ method, path string }{strings.ToUpper(method), path})
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	// 169 routes walk today. The floor is set to catch a *domain* silently
	// falling out of the sweep, not to pin the exact count: passing one handler
	// as nil unmounts its whole subtree, and chi.Walk then reports success over
	// whatever is left. That is not hypothetical — this sweep shipped with the
	// eleven opt-in handlers nil, walking 117 routes while claiming to cover
	// every authenticated one, and a floor of 50 was comfortably clear of it.
	// 150 is below today's count with room for ordinary churn and above the
	// number any single domain-drop leaves behind.
	if len(out) < 150 {
		t.Fatalf("walked only %d authenticated routes (expected ~169) — a handler is nil and its subtree never mounted, so the sweep silently skips it", len(out))
	}
	return out
}

// serveRecovering runs one request and reports whether the handler panicked.
// A panic means the request got past the middleware into a stub-backed handler,
// which for the revoked sweep IS the regression under test — reporting it beats
// letting it abort the whole test binary.
func serveRecovering(h http.Handler, method, path, token string) (rec *httptest.ResponseRecorder, panicked bool) {
	rec = httptest.NewRecorder()
	defer func() {
		if r := recover(); r != nil {
			panicked = true
		}
	}()
	req := httptest.NewRequest(method, path, io.NopCloser(strings.NewReader("{}")))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	return rec, false
}

// TestRevokedOperator_401OnEveryAuthenticatedRoute — the positive half: a
// revoked Archon is refused with 401 `operator-revoked-token` on every route
// behind a JWT.
func TestRevokedOperator_401OnEveryAuthenticatedRoute(t *testing.T) {
	h := revokedGateRouter(t, revokedGateRBAC(t, []string{revokedGateAID}))
	token := revokedGateToken(t, revokedGateAID)

	for _, rt := range authenticatedRoutes(t, h) {
		rec, panicked := serveRecovering(h, rt.method, rt.path, token)
		if panicked {
			t.Errorf("%s %s: reached the handler — the revoked gate did not fire", rt.method, rt.path)
			continue
		}
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401 for a revoked Archon; body=%s",
				rt.method, rt.path, rec.Code, rec.Body.String())
			continue
		}
		if !strings.Contains(rec.Body.String(), problem.TypeOperatorRevokedToken) {
			t.Errorf("%s %s: 401 without the %s problem type — a client cannot tell revoked from unauthenticated; body=%s",
				rt.method, rt.path, problem.TypeOperatorRevokedToken, rec.Body.String())
		}
	}
}

// TestActiveOperator_NeverRefusedAsRevoked — the negative half: an Archon who is
// NOT revoked never gets the revoked refusal. Without this, a gate that refuses
// unconditionally would pass the sweep above.
//
// Deliberately not asserting a specific success code: these handlers run on stub
// dependencies, so past the gate a route may 200, 400, 500 or panic, and none of
// that is this test's business. The single claim is that the reason is never
// "revoked".
func TestActiveOperator_NeverRefusedAsRevoked(t *testing.T) {
	h := revokedGateRouter(t, revokedGateRBAC(t, []string{revokedGateAID}))
	token := revokedGateToken(t, activeGateAID)

	for _, rt := range authenticatedRoutes(t, h) {
		rec, panicked := serveRecovering(h, rt.method, rt.path, token)
		if panicked {
			continue // got past the gate — exactly what an active Archon should do
		}
		if strings.Contains(rec.Body.String(), problem.TypeOperatorRevokedToken) {
			t.Errorf("%s %s = %d refused an ACTIVE Archon as revoked; body=%s",
				rt.method, rt.path, rec.Code, rec.Body.String())
		}
	}
}

// TestRevokedGate_CoversTheRoutesThatHadNoGate — a named check for the four
// routes NIM-421 found answering 200 indefinitely. The sweep above already
// covers them, but only implicitly: if the walk ever stops reaching them, the
// sweep goes quiet while these fail loudly, and the ticket's finding stays
// pinned to the specific paths it was found on.
func TestRevokedGate_CoversTheRoutesThatHadNoGate(t *testing.T) {
	h := revokedGateRouter(t, revokedGateRBAC(t, []string{revokedGateAID}))
	token := revokedGateToken(t, revokedGateAID)

	for _, path := range []string{
		"/v1/me/permissions",
		"/v1/permissions",
		"/v1/event-types",
		"/v1/herald-types",
	} {
		rec, panicked := serveRecovering(h, http.MethodGet, path, token)
		if panicked {
			t.Errorf("GET %s: reached the handler — the revoked gate did not fire", path)
			continue
		}
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s = %d, want 401 (this route carries NO RBAC gate — before NIM-421 it served a revoked Archon indefinitely); body=%s",
				path, rec.Code, rec.Body.String())
		}
	}
}
