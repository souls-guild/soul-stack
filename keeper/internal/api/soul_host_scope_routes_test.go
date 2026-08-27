package api

// Coven-narrowing guard for the per-host routes that share soulHostScope:
// POST /v1/souls/{sid}/issue-token and PUT /v1/souls/{sid}/ssh-target
// (NIM-588), plus POST /v1/souls/{sid}/exec (NIM-650).
//
// `soul.forget` has its own file (soul_forget_route_permission_test.go) because
// it also destroys rows. These share nothing with it but the selector, and
// the selector is exactly what a mount can be left out of: the routes are
// wired one by one in router.go, so a change that moves two of them and forgets
// the third leaves every guard on the moved routes green. Each mount is
// therefore asserted through the REAL buildRouter, on its own.
//
// Both directions per route. Narrowing that admitted everything satisfies the
// admit case alone; the pre-NIM-588 host-only context satisfies the refuse case
// alone. Only the pair says the coven is compared rather than fetched, ignored,
// or invented.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// soulHostScopeRoute — one per-host mutation, with a body the handler accepts.
// A body the handler rejects would answer 422 for BOTH covens and the refuse
// case would pass without the gate ever running.
type soulHostScopeRoute struct {
	name       string
	permission string
	method     string
	path       string
	body       string

	// admitStatus — what the route answers once the gate has let the call
	// through. Not always 200: the exec route reaches a handler whose
	// dispatcher is nil here and answers 500, which is precisely the proof
	// that the router gate admitted it. Asserting "not 403" instead would
	// also pass on a 404 from an unmounted route.
	admitStatus int
}

func soulHostScopeRoutes() []soulHostScopeRoute {
	return []soulHostScopeRoute{
		{
			name:        "issue-token",
			permission:  "soul.issue-token",
			method:      http.MethodPost,
			path:        "/v1/souls/" + forgetGateHost + "/issue-token",
			admitStatus: http.StatusOK,
		},
		{
			name:        "ssh-target-update",
			permission:  "soul.ssh-target-update",
			method:      http.MethodPut,
			path:        "/v1/souls/" + forgetGateHost + "/ssh-target",
			body:        `{"ssh_port":22,"ssh_user":"deploy","soul_path":"/opt/soul"}`,
			admitStatus: http.StatusOK,
		},
		{
			// NIM-650. `errand.run` used to be mounted with a host-only
			// selector, so `errand.run on coven=web` refused every host it
			// named. It now shares soulHostScope with the mutations above —
			// and shares this guard, because the whole failure mode was one
			// route being wired differently from its neighbours.
			name:        "errand-exec",
			permission:  "errand.run",
			method:      http.MethodPost,
			path:        "/v1/souls/" + forgetGateHost + "/exec",
			body:        `{"module":"core.cmd.shell"}`,
			admitStatus: http.StatusInternalServerError,
		},
	}
}

// soulHostScopeRouter — the real router over a host row carrying `covens`. The
// same row answers the gate's coven read and the handler's SelectBySID, so the
// test cannot end up asserting a grant against a coven the host does not have.
func soulHostScopeRouter(t *testing.T, perms []string, covens []string) http.Handler {
	t.Helper()
	installHumaErrorOverride()
	aid := soulForgetGateAID
	pool := &hSoulPool{existing: &soul.Soul{
		SID: forgetGateHost, Transport: soul.TransportAgent, Status: soul.StatusPending,
		Coven: covens, RegisteredAt: hSoulAt, CreatedByAID: &aid,
	}}
	soulH := handlers.NewSoulHandler(pool, hSoulScoper{unrestricted: true}, nil, nil)
	return revokedGateRouterWith(t, soulForgetGateRBAC(t, perms), soulH, &auditCaptureWriter{})
}

func soulHostScopeCall(t *testing.T, h http.Handler, rt soulHostScopeRoute) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if rt.body != "" {
		body = strings.NewReader(rt.body)
	}
	req := httptest.NewRequest(rt.method, rt.path, body)
	req.Header.Set("Authorization", "Bearer "+revokedGateToken(t, soulForgetGateAID))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestSoulHostRoutes_CovenGrantAdmitsHostInThatCoven — `<permission> on
// coven=web` reaches a host that IS in coven web, on every one of these mounts.
// Before NIM-588 each answered 403, naming a permission the operator held.
func TestSoulHostRoutes_CovenGrantAdmitsHostInThatCoven(t *testing.T) {
	for _, rt := range soulHostScopeRoutes() {
		t.Run(rt.name, func(t *testing.T) {
			h := soulHostScopeRouter(t, []string{rt.permission + " on coven=web"}, []string{"web"})

			rec := soulHostScopeCall(t, h, rt)
			if rec.Code != rt.admitStatus {
				t.Fatalf("%s %s = %d for `%s on coven=web` against a host IN coven web, want %d — "+
					"this mount is still resolving the host without its covens; body=%s",
					rt.method, rt.path, rec.Code, rt.permission, rt.admitStatus, rec.Body.String())
			}
		})
	}
}

// TestSoulHostRoutes_CovenGrantRefusesHostInAnotherCoven — the negative control
// per mount, and the reason the admit case above means anything.
func TestSoulHostRoutes_CovenGrantRefusesHostInAnotherCoven(t *testing.T) {
	for _, rt := range soulHostScopeRoutes() {
		t.Run(rt.name, func(t *testing.T) {
			h := soulHostScopeRouter(t, []string{rt.permission + " on coven=web"}, []string{"prod"})

			rec := soulHostScopeCall(t, h, rt)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s %s = %d for `%s on coven=web` against a host in coven prod, want 403 — "+
					"the coven grant is reaching hosts outside it; body=%s",
					rt.method, rt.path, rec.Code, rt.permission, rec.Body.String())
			}
		})
	}
}

// TestSoulHostRoutes_HostGrantStillWorks — the regression control. NIM-588
// replaced the selector these mounts used; a grant written against the host
// alone predates it and must be unaffected. Without this, a selector that
// dropped the host dimension and asserted only the coven would pass both tests
// above while silently breaking every `on host=` grant in the field.
func TestSoulHostRoutes_HostGrantStillWorks(t *testing.T) {
	for _, rt := range soulHostScopeRoutes() {
		t.Run(rt.name, func(t *testing.T) {
			h := soulHostScopeRouter(t, []string{rt.permission + " on host=" + forgetGateHost}, []string{"web"})

			rec := soulHostScopeCall(t, h, rt)
			if rec.Code != rt.admitStatus {
				t.Fatalf("%s %s = %d for `%s on host=%s`, want %d — a host-scoped grant stopped "+
					"working once the host also had a coven; body=%s",
					rt.method, rt.path, rec.Code, rt.permission, forgetGateHost, rt.admitStatus, rec.Body.String())
			}
		})
	}
}
