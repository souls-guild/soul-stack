package api

// Route-level guards for console recording playback (NIM-148).
//
// The handler's own scope logic is covered in package handlers; what can only be
// checked here is the shape of the ROUTE gate, and it has one way to be wrong
// that looks right: gating a route whose URL names no host with a scope-aware
// permission check. An absent scope dimension fails closed (ADR-047 §g G1), so
// that variant denies exactly the `host=`-scoped roles the feature exists to
// serve — the bug NIM-144 hit on the WebSocket, repeated here because the
// listing has the same shape.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/api/health"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/consolepg"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

// stubRecordingReader serves one recording on one host.
type stubRecordingReader struct{ rec consolepg.Recording }

func (s stubRecordingReader) List(context.Context, consolepg.ListFilter, int, int) ([]consolepg.Recording, int, error) {
	return []consolepg.Recording{s.rec}, 1, nil
}

func (s stubRecordingReader) Get(_ context.Context, id string) (consolepg.Recording, error) {
	if id != s.rec.RecordingID {
		return consolepg.Recording{}, consolepg.ErrNotFound
	}
	return s.rec, nil
}

func (s stubRecordingReader) WriteCast(_ context.Context, _ consolepg.Recording, write func([]byte) error) error {
	return write([]byte("{\"version\":2}\n[0.1,\"o\",\"hi\"]\n"))
}

// unrestrictedScoper answers every purview query with "no restriction" — the
// route gate, not the scope, is what these tests are about.
type unrestrictedScoper struct{}

func (unrestrictedScoper) ResolvePurview(string, string, string) rbac.Purview {
	return rbac.Purview{Unrestricted: true}
}

// consoleRecordingRouter assembles the real router with only the playback
// domain wired, under the given enforcer.
func consoleRecordingRouter(t *testing.T, enforcer RBACProvider) http.Handler {
	t.Helper()
	h := handlers.NewConsoleRecordingHandler(
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
		handlers.NewIncarnationHandler(nil, nil, nil, nil, nil, nil, nil, nil),
		handlers.NewSoulHandler(nil, nil, nil, nil),
		handlers.TelemetrySpecStub(),
		stubRoleHandler(t), stubSynodHandler(t), stubSigilHandler(t), stubSigilKeyHandler(t),
		stubServiceHandler(t), nil, nil, stubAugurHandler(t), stubOracleHandler(t),
		nil, nil, nil, nil, nil, nil, nil, nil,
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
		nil, // consoleWSDeps — the WebSocket is not part of these tests
		h,
		nil,
	)
}

func consoleRecordingGET(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+metaValidToken(t))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestConsoleRecordingRoutes_GateIsExistenceOnly — the load-bearing guard. A
// role holding `soul.console on host=<sid>` has HoldsAction true but fails a
// Check with no host in the context. It must still reach the listing: the URL
// names no host, so the route can only ask "may this Archon reach consoles at
// all", and the per-host boundary is applied to the ROWS.
//
// If this goes red with 403, someone swapped RequireAction for a scope-aware
// RequirePermission and every host-scoped operator just lost playback.
func TestConsoleRecordingRoutes_GateIsExistenceOnly(t *testing.T) {
	h := consoleRecordingRouter(t, scopedConsoleRBAC{})

	for _, path := range []string{
		"/v1/console/recordings",
		"/v1/console/recordings/01J0000000000000000000CAST",
		"/v1/console/recordings/01J0000000000000000000CAST/cast",
	} {
		rec := consoleRecordingGET(t, h, path)
		if rec.Code == http.StatusForbidden {
			t.Fatalf("GET %s = 403 for a host-scoped role — the route gate is scope-aware and fails closed on the missing host dimension", path)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, rec.Code)
		}
	}
}

// TestConsoleRecordingRoutes_WithoutTheRightAre403 — the other half: an operator
// holding no `soul.console` at all is refused at the route, before any store is
// touched.
func TestConsoleRecordingRoutes_WithoutTheRightAre403(t *testing.T) {
	h := consoleRecordingRouter(t, noConsoleRBAC{})

	for _, path := range []string{
		"/v1/console/recordings",
		"/v1/console/recordings/01J0000000000000000000CAST",
		"/v1/console/recordings/01J0000000000000000000CAST/cast",
	} {
		if rec := consoleRecordingGET(t, h, path); rec.Code != http.StatusForbidden {
			t.Fatalf("GET %s = %d without soul.console, want 403", path, rec.Code)
		}
	}
}

// TestConsoleRecordingCast_ServedAsAnAsciicast — the body is handed over with
// the media type players recognize, and as an attachment: a cast holds whatever
// an operator typed, and an inline disposition would let a browser interpret it
// in the API's own origin.
func TestConsoleRecordingCast_ServedAsAnAsciicast(t *testing.T) {
	h := consoleRecordingRouter(t, scopedConsoleRBAC{})
	rec := consoleRecordingGET(t, h, "/v1/console/recordings/01J0000000000000000000CAST/cast")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != castContentType {
		t.Fatalf("Content-Type = %q, want %q", got, castContentType)
	}
	if got := rec.Header().Get("Content-Disposition"); got == "" || got[:10] != "attachment" {
		t.Fatalf("Content-Disposition = %q, want an attachment", got)
	}
	if body := rec.Body.String(); body != "{\"version\":2}\n[0.1,\"o\",\"hi\"]\n" {
		t.Fatalf("body = %q, want the stored cast verbatim", body)
	}
}

// TestConsoleRecordingCast_UnknownIDIs404 — and the refusal carries problem+json
// like every other route, not the stream's content type.
func TestConsoleRecordingCast_UnknownIDIs404(t *testing.T) {
	h := consoleRecordingRouter(t, scopedConsoleRBAC{})
	rec := consoleRecordingGET(t, h, "/v1/console/recordings/01J000000000000000000MISS/cast")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got == castContentType {
		t.Fatal("a refusal was served as an asciicast")
	}
}

// scopedConsoleRBAC models `soul.console on host=<sid>`: it holds the action, but
// a Check with no host in the context is denied.
type scopedConsoleRBAC struct{ stubRBAC }

func (scopedConsoleRBAC) HoldsAction(string, string, string) bool { return true }

func (scopedConsoleRBAC) Check(_, _, _ string, ctx map[string]string) error {
	if _, ok := ctx["host"]; ok {
		return nil
	}
	return errConsoleForbidden
}

// noConsoleRBAC holds nothing.
type noConsoleRBAC struct{ stubRBAC }

func (noConsoleRBAC) HoldsAction(string, string, string) bool { return false }

func (noConsoleRBAC) Check(string, string, string, map[string]string) error {
	return errConsoleForbidden
}

var errConsoleForbidden = &consoleForbiddenError{}

type consoleForbiddenError struct{}

func (*consoleForbiddenError) Error() string { return "forbidden" }

// stubRBAC fills the rest of RBACProvider; these tests only exercise the gate.
type stubRBAC struct{}

func (stubRBAC) IsRevoked(string) bool { return false }

func (stubRBAC) ResolvePurview(string, string, string) rbac.Purview {
	return rbac.Purview{Unrestricted: true}
}

func (stubRBAC) CovenScope(string, string, string) ([]string, bool) { return nil, true }

func (stubRBAC) PermissionsOf(string) []rbac.EffectivePermission { return nil }

func (stubRBAC) RolesOf(string) []string { return nil }
