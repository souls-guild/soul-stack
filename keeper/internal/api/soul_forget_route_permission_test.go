package api

// Route-permission guard for DELETE /v1/souls/{sid} (NIM-386).
//
// SECURITY invariant:
//
//	The route is reachable ONLY by an operator holding `soul.forget`.
//	No other permission in the catalog opens it, and holding every other
//	permission in the catalog does not.
//
// Why a test of its own. The permission lives in exactly one place — the
// `RequirePermission(enforcer, "soul", "forget", handlers.SoulSIDSelector)`
// wrapper in router.go — and nothing else in the tree refers to it. Delete that
// wrapper and every other test in this package still passes: the handler tests
// mount their own chain (`forgetRouter`), so they keep asserting a gate that
// production no longer has, and the revoked sweep only ever asks about 401. The
// route would then erase hosts for anyone with a valid JWT, and the suite would
// be green about it.
//
// The negative control is deliberately "every permission EXCEPT this one"
// rather than "no permissions". A bare-empty operator is refused by ANY gate,
// including the wrong one — such a test passes just as well if the route were
// wrapped in `soul.list`. Granting the whole catalog minus one name is what
// makes the refusal attributable to `soul.forget` and to nothing else.
//
// Both controls run through the REAL buildRouter, not a hand-mounted chain: the
// claim is about how the route is mounted in production, and a chain written in
// the test file is a copy of the answer rather than a check of it.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/shared/audit"
)

const soulForgetGateAID = "archon-forget-gate"

// soulForgetGateRBAC — an enforcer granting the named AID exactly `perms`.
func soulForgetGateRBAC(t *testing.T, perms []string) RBACProvider {
	t.Helper()
	enf, err := rbac.NewEnforcerFromSnapshot(&rbac.Snapshot{
		Roles:      map[string][]string{"cluster-admin": perms},
		Membership: map[string][]string{soulForgetGateAID: {"cluster-admin"}},
		Revoked:    map[string]time.Time{},
	})
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	return enf
}

// catalogExcept — every permission name in the RBAC catalog but the given one.
// Reading the catalog rather than listing names by hand keeps the control
// honest as the catalog grows: a permission added later is granted here
// automatically, so a route that starts accepting it fails this test.
func catalogExcept(t *testing.T, omit string) []string {
	t.Helper()
	if _, ok := rbac.AllowedPermissions[omit]; !ok {
		t.Fatalf("%q is not in rbac.AllowedPermissions — the control would omit nothing", omit)
	}
	out := make([]string, 0, len(rbac.AllowedPermissions))
	for name := range rbac.AllowedPermissions {
		if name == omit {
			continue
		}
		out = append(out, name)
	}
	return out
}

// forgetGateRouter — the real router with a Soul handler that can actually
// forget, so a request that reaches the handler produces a 200 and a recorded
// DELETE instead of a panic. The panic would be evidence too, but a much worse
// kind: it cannot be told apart from a handler that failed for its own reasons.
func forgetGateRouter(t *testing.T, perms []string) (http.Handler, *fgPool, *fgTeardown, *auditCaptureWriter) {
	t.Helper()
	installHumaErrorOverride()
	pool := &fgPool{status: "disconnected", seeds: 1, tokens: 0, members: 0, voices: 0}
	td := &fgTeardown{}
	auditCap := &auditCaptureWriter{}
	soulH := handlers.NewSoulHandlerWithTeardown(pool, hSoulScoper{unrestricted: true}, nil, td, nil)
	return revokedGateRouterWith(t, soulForgetGateRBAC(t, perms), soulH, auditCap), pool, td, auditCap
}

func forgetGateDelete(t *testing.T, h http.Handler, sid string) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	return serveRecovering(h, http.MethodDelete, "/v1/souls/"+sid, revokedGateToken(t, soulForgetGateAID))
}

// TestSoulForgetRoute_RefusesEveryPermissionButSoulForget — the known-bad case
// this file exists for. Removing the RequirePermission wrapper from the DELETE
// mount in router.go turns this 403 into a 200 with the host erased.
func TestSoulForgetRoute_RefusesEveryPermissionButSoulForget(t *testing.T) {
	h, pool, _, auditCap := forgetGateRouter(t, catalogExcept(t, "soul.forget"))

	rec, panicked := forgetGateDelete(t, h, "host-1.example.com")
	if panicked {
		t.Fatal("DELETE /v1/souls/{sid} reached the handler and panicked — the permission gate did not fire")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("DELETE /v1/souls/{sid} = %d for an operator holding every permission EXCEPT soul.forget, want 403; body=%s",
			rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), problem.TypeForbidden) {
		t.Errorf("403 without the %s problem type; body=%s", problem.TypeForbidden, rec.Body.String())
	}
	// The refusal must name the right permission. A 403 produced by some other
	// gate would satisfy the status check while the route stayed ungoverned by
	// `soul.forget` — and the operator reading it would go looking for the
	// wrong grant.
	if !strings.Contains(rec.Body.String(), "soul.forget") {
		t.Errorf("403 does not name `soul.forget` — the route is gated by something else; body=%s", rec.Body.String())
	}

	// The status code is the weaker half. What matters is that nothing was
	// destroyed: a gate that answers 403 after the delete has already run is
	// not a gate.
	if pool.deleted() {
		t.Error("the host was DELETED despite the 403 — the refusal came after the erase, not before it")
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("a refused forget wrote %d audit event(s) — `soul.forgotten` must mean a host is gone",
			len(auditCap.Events()))
	}
}

// TestSoulForgetRoute_AdmitsSoulForgetAlone — the positive control. Without it
// the test above would pass on a route that is broken, unmounted, or gated by
// something nobody can ever hold.
func TestSoulForgetRoute_AdmitsSoulForgetAlone(t *testing.T) {
	h, pool, td, auditCap := forgetGateRouter(t, []string{"soul.forget"})

	rec, panicked := forgetGateDelete(t, h, "host-1.example.com")
	if panicked {
		t.Fatal("DELETE /v1/souls/{sid} panicked for an operator holding soul.forget")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE /v1/souls/{sid} = %d for an operator holding soul.forget alone, want 200; body=%s",
			rec.Code, rec.Body.String())
	}
	if !pool.deleted() {
		t.Error("200 without a DELETE FROM souls — the route answered success without forgetting anything")
	}
	if !td.closed {
		t.Error("the host was forgotten without the teardown running — the row went, the resources stayed")
	}
	if len(auditCap.Events()) != 1 {
		t.Fatalf("a successful forget wrote %d audit events, want exactly 1", len(auditCap.Events()))
	}
	if got := auditCap.Events()[0].EventType; got != audit.EventSoulForgotten {
		t.Errorf("audit event type = %q, want %q", got, audit.EventSoulForgotten)
	}
}
