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
// `RequirePermissionMulti(enforcer, "soul", "forget", soulHostScope)` wrapper
// in router.go — and nothing else in the tree refers to it. Delete that
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
	"errors"
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
	return soulForgetGateRBACScoped(t, perms, "")
}

// soulForgetGateRBACScoped — the same, with the role carrying a
// `default_scope` (ADR-047 S1). Empty defaultScope = the column is NULL, i.e.
// the key is absent from RoleScopes, which is what an unscoped role looks like
// coming out of [rbac.LoadSnapshot].
func soulForgetGateRBACScoped(t *testing.T, perms []string, defaultScope string) RBACProvider {
	t.Helper()
	scopes := map[string]string{}
	if defaultScope != "" {
		scopes["cluster-admin"] = defaultScope
	}
	enf, err := rbac.NewEnforcerFromSnapshot(&rbac.Snapshot{
		Roles:      map[string][]string{"cluster-admin": perms},
		RoleScopes: scopes,
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
	return forgetGateRouterScoped(t, perms, "")
}

func forgetGateRouterScoped(t *testing.T, perms []string, defaultScope string) (http.Handler, *fgPool, *fgTeardown, *auditCaptureWriter) {
	t.Helper()
	return forgetGateRouterWith(t, perms, defaultScope,
		&fgPool{status: "disconnected", seeds: 1, tokens: 0, members: 0, voices: 0})
}

// forgetGateRouterWith — the full form, where the caller supplies the host row.
// Every coven case below goes through it, because the gate's answer is a
// function of BOTH halves — the grant and the row it is judged against — and a
// helper that fixed the row would only ever be able to ask half the question.
func forgetGateRouterWith(t *testing.T, perms []string, defaultScope string, pool *fgPool) (http.Handler, *fgPool, *fgTeardown, *auditCaptureWriter) {
	t.Helper()
	installHumaErrorOverride()
	td := &fgTeardown{}
	auditCap := &auditCaptureWriter{}
	soulH := handlers.NewSoulHandlerWithTeardown(pool, hSoulScoper{unrestricted: true}, nil, td, nil)
	return revokedGateRouterWith(t, soulForgetGateRBACScoped(t, perms, defaultScope), soulH, auditCap), pool, td, auditCap
}

// forgetGateHost — the SID every case in this file forgets. Named because the
// coven cases below grant `on host=` against it, and a literal that drifted
// apart from the request path would turn those into vacuous denials.
const forgetGateHost = "host-1.example.com"

func forgetGateDelete(t *testing.T, h http.Handler, sid string) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	return serveRecovering(h, http.MethodDelete, "/v1/souls/"+sid, revokedGateToken(t, soulForgetGateAID))
}

// TestSoulForgetRoute_RefusesEveryPermissionButSoulForget — the known-bad case
// this file exists for. Removing the RequirePermission wrapper from the DELETE
// mount in router.go turns this 403 into a 200 with the host erased.
func TestSoulForgetRoute_RefusesEveryPermissionButSoulForget(t *testing.T) {
	h, pool, _, auditCap := forgetGateRouter(t, catalogExcept(t, "soul.forget"))

	rec, panicked := forgetGateDelete(t, h, forgetGateHost)
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

	rec, panicked := forgetGateDelete(t, h, forgetGateHost)
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

// --- coven narrowing (NIM-588) ---
//
// Until NIM-588 the selector put ONLY `host` into the RBAC context, and a
// dimension absent from the context fails closed. So `soul.forget on
// coven=<label>` was not "forget hosts in that coven" — it refused every call,
// including one aimed at a host that really was in the coven, with a 403 naming
// a permission the operator demonstrably held. The tests here used to assert
// that denial; they now assert the narrowing, and they are the reason the
// change cannot quietly regress.
//
// Two cases per direction, because a coven reaches this permission by two
// independent routes — the `on coven=` suffix on the permission string, and
// `default_scope` on the role (ADR-047 S1) — written by different people at
// different times. Both arrive at [rbac.Enforcer.Check] as the same
// `*ScopeExpr` (`effectiveScope`, permission.go), so one fix closes both; the
// pair exists so that a fix which closed only one goes red on the other rather
// than looking finished.
//
// The same shape applies to `soul.issue-token` and `soul.ssh-target-update`,
// the other two routes on [handlers.SoulSIDScopeSelector], and to all three on
// the MCP surface (ADR-004 makes OpenAPI and MCP equally primary — a permission
// that narrows over one and denies over the other is still broken for whoever
// uses the other). `soul.console` is NOT among them: it is mounted behind
// RequireAction with the scope applied inside the handler.

// TestSoulForgetRoute_CovenGrantAdmitsHostInThatCoven — the case NIM-588
// exists for. Stop resolving the host's covens in the selector and this 200
// becomes the old 403.
func TestSoulForgetRoute_CovenGrantAdmitsHostInThatCoven(t *testing.T) {
	h, pool, td, auditCap := forgetGateRouterWith(t, []string{"soul.forget on coven=web"}, "",
		&fgPool{status: "disconnected", coven: []string{"web"}, seeds: 1})

	rec, panicked := forgetGateDelete(t, h, forgetGateHost)
	if panicked {
		t.Fatal("DELETE /v1/souls/{sid} panicked for a `soul.forget on coven=web` holder")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE /v1/souls/{sid} = %d for `soul.forget on coven=web` against a host IN coven web, "+
			"want 200 — the grant is being read as a denial again; body=%s", rec.Code, rec.Body.String())
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
}

// TestSoulForgetRoute_CovenGrantRefusesHostInAnotherCoven — the other half, and
// the one that makes the case above mean something. Narrowing that admitted
// every host would satisfy the positive test just as well; this is what says
// the coven is being compared rather than merely fetched.
func TestSoulForgetRoute_CovenGrantRefusesHostInAnotherCoven(t *testing.T) {
	h, pool, _, auditCap := forgetGateRouterWith(t, []string{"soul.forget on coven=web"}, "",
		&fgPool{status: "disconnected", coven: []string{"prod"}, seeds: 1})

	rec, panicked := forgetGateDelete(t, h, forgetGateHost)
	if panicked {
		t.Fatal("DELETE /v1/souls/{sid} reached the handler and panicked — the permission gate did not fire")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("DELETE /v1/souls/{sid} = %d for `soul.forget on coven=web` against a host in coven prod, "+
			"want 403 — the coven grant is admitting hosts outside it; body=%s", rec.Code, rec.Body.String())
	}
	if pool.deleted() {
		t.Error("the host was DELETED despite the 403 — a host outside the granted coven was erased")
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("a refused forget wrote %d audit event(s)", len(auditCap.Events()))
	}
}

// TestSoulForgetRoute_CovenGrantAdmitsHostInAnyOfItsCovens — a host carries a
// LIST of covens (ADR-008), and the grant has to match any of them, not the
// first one Postgres happens to return. A selector that emitted a single
// context from `covens[0]` passes both tests above and fails this one.
func TestSoulForgetRoute_CovenGrantAdmitsHostInAnyOfItsCovens(t *testing.T) {
	h, pool, _, _ := forgetGateRouterWith(t, []string{"soul.forget on coven=web"}, "",
		&fgPool{status: "disconnected", coven: []string{"prod", "web"}, seeds: 1})

	rec, panicked := forgetGateDelete(t, h, forgetGateHost)
	if panicked {
		t.Fatal("DELETE /v1/souls/{sid} panicked for a `soul.forget on coven=web` holder")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE /v1/souls/{sid} = %d for `soul.forget on coven=web` against a host in [prod web], "+
			"want 200 — only the first coven of the list is reaching the gate; body=%s", rec.Code, rec.Body.String())
	}
	if !pool.deleted() {
		t.Error("200 without a DELETE FROM souls")
	}
}

// TestSoulForgetRoute_CovenRoleDefaultScopeAdmitsHostInThatCoven covers the
// SECOND way a coven reaches this permission, and the likelier one: the role
// carries `default_scope = coven=<label>` (ADR-047 S1) and the permission
// itself is bare. [rbac.NewEnforcerFromSnapshot] gives such a permission the
// role's scope (enforcer.go, `role.DefaultScope`), so it arrives at the gate
// coven-narrowed exactly like the suffix form.
func TestSoulForgetRoute_CovenRoleDefaultScopeAdmitsHostInThatCoven(t *testing.T) {
	h, pool, _, _ := forgetGateRouterWith(t, []string{"soul.forget"}, "coven=web",
		&fgPool{status: "disconnected", coven: []string{"web"}, seeds: 1})

	rec, panicked := forgetGateDelete(t, h, forgetGateHost)
	if panicked {
		t.Fatal("DELETE /v1/souls/{sid} panicked for a bare `soul.forget` in a role scoped coven=web")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE /v1/souls/{sid} = %d for a bare `soul.forget` in a role scoped `coven=web`, against a "+
			"host IN coven web, want 200 — the suffix form narrows but the role default_scope still denies; body=%s",
			rec.Code, rec.Body.String())
	}
	if !pool.deleted() {
		t.Error("200 without a DELETE FROM souls")
	}
}

// TestSoulForgetRoute_CovenRoleDefaultScopeRefusesHostInAnotherCoven — the
// negative control for the role-scope form.
func TestSoulForgetRoute_CovenRoleDefaultScopeRefusesHostInAnotherCoven(t *testing.T) {
	h, pool, _, auditCap := forgetGateRouterWith(t, []string{"soul.forget"}, "coven=web",
		&fgPool{status: "disconnected", coven: []string{"prod"}, seeds: 1})

	rec, panicked := forgetGateDelete(t, h, forgetGateHost)
	if panicked {
		t.Fatal("DELETE /v1/souls/{sid} reached the handler and panicked — the permission gate did not fire")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("DELETE /v1/souls/{sid} = %d for a role scoped `coven=web` against a host in coven prod, "+
			"want 403; body=%s", rec.Code, rec.Body.String())
	}
	if pool.deleted() {
		t.Error("the host was DELETED despite the 403 — a host outside the role's coven was erased")
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("a refused forget wrote %d audit event(s)", len(auditCap.Events()))
	}
}

// TestSoulForgetRoute_CovenUnreadableKeepsHostDimension pins the fallback in
// [handlers.SoulSIDScopeSelector], which is the one place the fix could have
// made things WORSE than before it.
//
// The selector reads a row to learn the host's covens. When that read fails —
// Postgres down, host unknown — it keeps asserting `host=<sid>` and drops only
// the coven. Returning nothing instead would be the tidy-looking choice and it
// would break `soul.forget on host=web-1`, a grant that worked before NIM-588
// and has nothing to do with covens, every time the database hiccups.
//
// Both halves are asserted together: the host grant still admits (so the
// fallback is not a blanket denial) AND the coven grant still refuses (so the
// fallback is not a blanket admission that treats an unreadable row as "in
// every coven"). Either one alone is satisfied by a broken selector.
func TestSoulForgetRoute_CovenUnreadableKeepsHostDimension(t *testing.T) {
	dbDown := func() *fgPool {
		return &fgPool{status: "disconnected", covenErr: errors.New("pool closed"), seeds: 1}
	}

	t.Run("host grant survives", func(t *testing.T) {
		h, pool, _, _ := forgetGateRouterWith(t, []string{"soul.forget on host=" + forgetGateHost}, "", dbDown())

		rec, panicked := forgetGateDelete(t, h, forgetGateHost)
		if panicked {
			t.Fatal("DELETE /v1/souls/{sid} panicked for a `soul.forget on host=` holder")
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("DELETE /v1/souls/{sid} = %d for `soul.forget on host=%s` while the coven read fails, "+
				"want 200 — a database hiccup has started revoking host-scoped grants; body=%s",
				rec.Code, forgetGateHost, rec.Body.String())
		}
		if !pool.deleted() {
			t.Error("200 without a DELETE FROM souls")
		}
	})

	t.Run("coven grant fails closed", func(t *testing.T) {
		h, pool, _, _ := forgetGateRouterWith(t, []string{"soul.forget on coven=web"}, "", dbDown())

		rec, panicked := forgetGateDelete(t, h, forgetGateHost)
		if panicked {
			t.Fatal("DELETE /v1/souls/{sid} reached the handler and panicked — the permission gate did not fire")
		}
		if rec.Code != http.StatusForbidden {
			t.Fatalf("DELETE /v1/souls/{sid} = %d for `soul.forget on coven=web` while the coven read fails, "+
				"want 403 — an unreadable row is being treated as membership in every coven; body=%s",
				rec.Code, rec.Body.String())
		}
		if pool.deleted() {
			t.Error("a host was erased under a coven grant whose coven could not be read")
		}
	})
}
