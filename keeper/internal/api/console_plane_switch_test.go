package api

// Route-level guards for the console plane switch (NIM-292, `console.enabled`).
//
// The contract these pin is a distinction, not a status code: with the plane
// switched off `/v1/console` must be INDISTINGUISHABLE from a path that was
// never routed. 403 would say "this cluster has a console plane and you may not
// use it" — a fact an operator of a console-free cluster should not be able to
// read off the API — so the difference between the two answers is the whole
// control, and a refactor that swapped them would look correct in every other
// test in this package.
//
// The second contract is what the switch must NOT reach: recordings outlive the
// plane that produced them. Turning consoles off is a decision about new
// sessions; it is not a way to make yesterday's root shells unreadable to an
// auditor.

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/api/health"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/console"
	"github.com/souls-guild/soul-stack/keeper/internal/console/consoletest"
	"github.com/souls-guild/soul-stack/keeper/internal/consolepg"
)

// consolePlaneRouter assembles the real router with BOTH console surfaces wired
// — the WebSocket and the playback routes — so a test can watch one go away
// while the other stays.
func consolePlaneRouter(t *testing.T, enforcer RBACProvider, planeEnabled func() bool) http.Handler {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(nopWriter{}, nil))

	recorder, err := console.NewRecorder(consoletest.NewStore(),
		console.StaticRecorderConfig(console.RecorderConfig{}), logger)
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	hub, err := console.NewHub(console.HubDeps{
		Dispatcher: &fakeSoul{},
		Recorder:   recorder,
		Logger:     logger,
	})
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}

	playback := handlers.NewConsoleRecordingHandler(
		stubRecordingReader{rec: consolepg.Recording{
			RecordingID: "01J0000000000000000000CAST",
			SessionID:   "01J0000000000000000000SESS",
			Kind:        "interactive",
			SID:         "web-01.example.com",
			ArchonAID:   "archon-victim",
		}},
		unrestrictedScoper{}, nil, nil)

	return buildRouter(
		metaVerifier(t),
		health.NewHandler(health.Deps{}),
		stubOperatorHandler(t),
		handlers.NewIncarnationHandler(nil, nil, nil, nil, nil, nil, nil, nil, nil),
		handlers.NewSoulHandler(nil, nil, nil, nil),
		handlers.TelemetrySpecStub(),
		stubRoleHandler(t), stubSynodHandler(t), stubSigilHandler(t), stubSigilKeyHandler(t),
		stubServiceHandler(t), nil, nil, stubAugurHandler(t), stubOracleHandler(t),
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		handlers.NewModuleCatalogHandler(nil, nil),
		handlers.NewModuleFormPrepHandler(nil, nil),
		handlers.NewPermissionCatalogHandler(nil),
		handlers.NewEventTypeCatalogHandler(nil),
		handlers.NewHeraldTypeCatalogHandler(nil),
		handlers.NewMyPermissionsHandler(nil, nil),
		enforcer,
		nil, nil, nil, nil, nil, nil, nil,
		false,
		nil, nil, nil,
		AuthMethodsDeps{},
		nil,
		apimiddleware.AuthLoginLimitConfig{},
		nil, nil, nil,
		&consoleWSDeps{
			Hub:          hub,
			Enforcer:     enforcer,
			Logger:       logger,
			PlaneEnabled: planeEnabled,
		},
		playback,
		nil,
	)
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// The router needs a full RBACProvider; the socket tests' allow/deny stubs are
// only the checker half, so they are widened over the same stubRBAC the playback
// tests use.
type planeAllowRBAC struct{ stubRBAC }

func (planeAllowRBAC) Check(string, string, string, map[string]string) error { return nil }
func (planeAllowRBAC) HoldsAction(string, string, string) bool               { return true }

type planeDenyRBAC struct{ stubRBAC }

func (planeDenyRBAC) Check(string, string, string, map[string]string) error {
	return errConsoleForbidden
}
func (planeDenyRBAC) HoldsAction(string, string, string) bool { return false }

func consolePlaneGET(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+metaValidToken(t))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func planeOff() bool { return false }
func planeOn() bool  { return true }

// The load-bearing guard: off means 404, and it means 404 for an operator who
// DOES hold `soul.console`. Anything else and the switch has become a permission
// check rather than a statement about the cluster.
func TestConsolePlaneSwitch_OffGivesNotFoundNotForbidden(t *testing.T) {
	h := consolePlaneRouter(t, planeAllowRBAC{}, planeOff)

	rec := consolePlaneGET(t, h, "/v1/console")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /v1/console with the plane off = %d, want 404 — a switched-off plane must answer as an unrouted path, not as a permission failure", rec.Code)
	}

	// The same problem document an unrouted path produces. `instance` echoes the
	// requested path and so differs by construction — every other field must
	// not, because any of them differing is a probe for whether this cluster has
	// consoles.
	absent := consolePlaneGET(t, h, "/v1/no-such-endpoint-at-all")
	var off, none map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &off); err != nil {
		t.Fatalf("disabled-plane body is not a problem document: %v", err)
	}
	if err := json.Unmarshal(absent.Body.Bytes(), &none); err != nil {
		t.Fatalf("unrouted-path body is not a problem document: %v", err)
	}
	delete(off, "instance")
	delete(none, "instance")
	if !reflect.DeepEqual(off, none) {
		t.Fatalf("disabled-plane problem %v differs from an unrouted path's %v beyond `instance`", off, none)
	}
}

// The counterpart that gives the 404 its meaning: with the plane ON, a caller
// without the right gets 403. If this ever returns 404 too, the guard above
// stops proving anything.
func TestConsolePlaneSwitch_OnStillDistinguishesForbidden(t *testing.T) {
	h := consolePlaneRouter(t, planeDenyRBAC{}, planeOn)

	rec := consolePlaneGET(t, h, "/v1/console")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /v1/console with the plane on and the right withheld = %d, want 403", rec.Code)
	}
}

// With the plane on and the right held, the request reaches the socket layer —
// it fails the WebSocket upgrade because this is a plain GET, and that is the
// point: 400-ish, not 404, means the route is there.
func TestConsolePlaneSwitch_OnMountsTheRoute(t *testing.T) {
	h := consolePlaneRouter(t, planeAllowRBAC{}, planeOn)

	rec := consolePlaneGET(t, h, "/v1/console")
	if rec.Code == http.StatusNotFound || rec.Code == http.StatusForbidden {
		t.Fatalf("GET /v1/console with the plane on = %d, want the upgrade failure of a mounted route", rec.Code)
	}
}

// Recordings are not part of the switch. An auditor reading what a root shell
// did last week must not depend on whether consoles are open today.
func TestConsolePlaneSwitch_OffKeepsRecordingsReadable(t *testing.T) {
	h := consolePlaneRouter(t, planeAllowRBAC{}, planeOff)

	for _, path := range []string{
		"/v1/console/recordings",
		"/v1/console/recordings/01J0000000000000000000CAST",
		"/v1/console/recordings/01J0000000000000000000CAST/cast",
	} {
		rec := consolePlaneGET(t, h, path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s with the plane off = %d, want 200 — recordings outlive the plane that produced them", path, rec.Code)
		}
	}
}

// The switch is resolved per request, not captured when the router was built.
// Without this the UI would accept the edit and change nothing until a restart —
// the silent failure ADR-0073(j.5) exists to prevent.
func TestConsolePlaneSwitch_AppliesWithoutRebuildingTheRouter(t *testing.T) {
	var on atomic.Bool
	on.Store(true)
	h := consolePlaneRouter(t, planeAllowRBAC{}, on.Load)

	if rec := consolePlaneGET(t, h, "/v1/console"); rec.Code == http.StatusNotFound {
		t.Fatal("route missing while the plane is on")
	}
	on.Store(false)
	if rec := consolePlaneGET(t, h, "/v1/console"); rec.Code != http.StatusNotFound {
		t.Fatalf("after switching the plane off = %d, want 404 without a restart", rec.Code)
	}
	on.Store(true)
	if rec := consolePlaneGET(t, h, "/v1/console"); rec.Code == http.StatusNotFound {
		t.Fatal("the plane did not come back on")
	}
}
