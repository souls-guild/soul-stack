package api

// Guard (NIM-1087): every route under /v1/incarnations/{…} whose gate runs the
// scope-aware Check hands that Check the incarnation named in the PATH.
//
// The selector behind those gates reads a path parameter by name, and the routes
// declare theirs in huma operation paths elsewhere. Nothing compiles the two
// against each other: when the selector read `name` and the routes declared `{id}`,
// it got "" on every request, every role scoped by `incarnation=` / `service=` /
// `coven=` was refused on every incarnation mutation, and every test stayed green —
// the selector's unit tests fed it the parameter it read, and the route-gate guard
// records which permission is asked, not the context it is asked with. Bare and `*`
// roles kept passing, so an admin saw nothing wrong.
//
// So this sends one request through each such route of the REAL router, with a
// database that answers the incarnation row, and reads the context the gate
// received. Then it checks the meaning end to end on one route: a role scoped to
// the incarnation's service passes the gate, one scoped to another service does not.

import (
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

const scopeGuardIncarnation = "redis-prod" // incRow answers service "redis"

// scopeRecordingRBAC records the context of every scope-aware Check.
type scopeRecordingRBAC struct {
	RBACProvider
	mu   sync.Mutex
	ctxs []map[string]string
}

func (r *scopeRecordingRBAC) Check(aid, resource, action string, ctx map[string]string) error {
	r.mu.Lock()
	r.ctxs = append(r.ctxs, maps.Clone(ctx))
	r.mu.Unlock()
	return r.RBACProvider.Check(aid, resource, action, ctx)
}

func (r *scopeRecordingRBAC) take() []map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.ctxs
	r.ctxs = nil
	return out
}

func scopeGuardRouter(t *testing.T, enforcer RBACProvider) http.Handler {
	t.Helper()
	verifier, err := keeperjwt.NewVerifier([]byte(metaSigningKey), metaIssuer, keeperjwt.NoRevocation)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	db := &incTestDB{selectByID: func(name string) pgx.Row { return incRow(name, "ready", "{}") }}
	incH := handlers.NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil)
	return revokedGateRouterIncarnation(t, verifier, enforcer, incH, handlers.NewSoulHandler(nil, nil, nil, nil), nil, false)
}

// scopeGuardPath puts the incarnation into the first path parameter, whatever it
// is called, and a literal into every other one.
func scopeGuardPath(pattern string) string {
	p := pattern
	if open := strings.Index(p, "{"); open >= 0 {
		if closing := strings.Index(p[open:], "}"); closing >= 0 {
			p = p[:open] + scopeGuardIncarnation + p[open+closing+1:]
		}
	}
	for {
		open := strings.Index(p, "{")
		if open < 0 {
			return p
		}
		closing := strings.Index(p[open:], "}")
		if closing < 0 {
			return p
		}
		p = p[:open] + "x" + p[open+closing+1:]
	}
}

func TestIncarnationScope_EveryScopedRouteSeesItsIncarnation(t *testing.T) {
	rec := &scopeRecordingRBAC{RBACProvider: zeroGrantRBAC(t).RBACProvider}
	h := scopeGuardRouter(t, rec)
	token := revokedGateToken(t, rbacGateAID)

	probed := map[route]struct{}{}
	for _, w := range walkGateRoutes(t, h) {
		// Any parameter name, not `{id}`: a route declaring another name is exactly
		// the mismatch this guard exists to catch, so filtering on `{id}` would skip it.
		if !strings.HasPrefix(w.r.path, "/v1/incarnations/{") {
			continue
		}
		rec.take()
		resp, panicked := serveRecovering(h, w.r.method, scopeGuardPath(w.r.path), token)
		ctxs := rec.take()
		// Selected by what ran, not by the route-gate table: a route new enough to
		// have no row there is still read. No Check means an existence gate (the
		// handler narrows) or no gate; nothing to read here.
		if len(ctxs) == 0 {
			continue
		}
		probed[w.r] = struct{}{}

		seen := false
		for _, c := range ctxs {
			if c["incarnation"] == scopeGuardIncarnation && c["service"] == "redis" {
				seen = true
				break
			}
		}
		if !seen {
			t.Errorf("%s: the scoped gate was asked with contexts %v, none naming incarnation=%s service=redis — the scope selector does not read the path parameter this route declares, so every scoped role is refused here",
				w.r, ctxs, scopeGuardIncarnation)
			continue
		}
		if panicked || resp.Code != http.StatusForbidden {
			t.Errorf("%s = %d (panicked=%v) for an Archon holding nothing, want 403", w.r, resp.Code, panicked)
		}
	}
	// The expected set comes from the route-gate table, which that guard holds to
	// the router: a route moved to an existence gate (reveal, NIM-1146) changes its
	// row there and leaves this set with it, where a fixed floor went red for it.
	expected := 0
	for r, g := range rbacRouteGates {
		if !strings.HasPrefix(r.path, "/v1/incarnations/{") || !slices.ContainsFunc(g.asks, func(a string) bool { return strings.HasPrefix(a, "check:") }) {
			continue
		}
		expected++
		if _, ok := probed[r]; !ok {
			t.Errorf("%s: the route-gate table says its gate is scope-aware (%v), but the request ran no scope-aware Check — the walk lost it, and this guard is covering less than it claims",
				r, g.asks)
		}
	}
	if expected == 0 {
		t.Fatal("no scope-aware incarnation route in rbacRouteGates — the selection above no longer matches the table")
	}
	t.Logf("guard: %d scoped incarnation routes handed the gate the path's incarnation", len(probed))
}

func TestIncarnationScope_ServiceScopedRolePassesOnlyItsOwnService(t *testing.T) {
	enf, err := rbac.NewEnforcerFromSnapshot(&rbac.Snapshot{
		Roles: map[string][]string{
			"label-redis":    {"incarnation.label-set on service=redis"},
			"label-postgres": {"incarnation.label-set on service=postgres"},
		},
		Membership: map[string][]string{
			"archon-scope-redis":    {"label-redis"},
			"archon-scope-postgres": {"label-postgres"},
		},
		Revoked: map[string]time.Time{},
	})
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	h := scopeGuardRouter(t, enf)
	path := "/v1/incarnations/" + scopeGuardIncarnation + "/label"

	resp, panicked := serveRecovering(h, http.MethodPut, path, revokedGateToken(t, "archon-scope-redis"))
	if !panicked && resp.Code == http.StatusForbidden {
		t.Errorf("PUT %s by a role holding incarnation.label-set on service=redis = 403 (%s), want the gate to let it through — the incarnation's service is redis",
			path, resp.Body.String())
	}

	resp, panicked = serveRecovering(h, http.MethodPut, path, revokedGateToken(t, "archon-scope-postgres"))
	if panicked || resp.Code != http.StatusForbidden {
		t.Errorf("PUT %s by a role holding incarnation.label-set on service=postgres = %d (panicked=%v), want 403 — the scope must still refuse another service",
			path, resp.Code, panicked)
	}
}
