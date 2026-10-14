package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
)

// lockedIncReader serves incarnation.SelectByID for the per-incarnation pass of a
// scenario Voyage: every name is ready unless statuses says otherwise, and its
// status_details carries details[name].
type lockedIncReader struct {
	statuses map[string]string
	details  map[string]string
}

func (r lockedIncReader) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	name, _ := args[0].(string)
	status := "ready"
	if s, ok := r.statuses[name]; ok {
		status = s
	}
	details := []byte(nil)
	if d, ok := r.details[name]; ok {
		details = []byte(d)
	}
	now := time.Now()
	return incStaticRow{values: []any{
		name, "redis", "v1", int(1),
		[]byte("{}"), status,
		details, any(nil),
		now, now, []string(nil),
		[]byte("{}"), "create", any(nil), any(nil),
	}}
}

func (lockedIncReader) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("lockedIncReader: unexpected Exec")
}

func (lockedIncReader) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("lockedIncReader: unexpected Query")
}

func humaVoyageRouterLocked(t *testing.T, store *fakeVoyageStore, names []string, inc lockedIncReader) *chi.Mux {
	return humaVoyageRouterLockedWith(t, store, names, inc, strictAllowAll{}, 0)
}

func humaVoyageRouterLockedWith(t *testing.T, store *fakeVoyageStore, names []string, inc lockedIncReader, enf apimiddleware.PermissionChecker, maxScope int) *chi.Mux {
	t.Helper()
	installHumaErrorOverride()
	voyageH := handlers.NewVoyageHandler(store, fakeVoyageScenResolver{incarnations: names}, fakeVoyageCmdResolver{},
		inc, enf, nil /*soulReader*/, nil /*scoper*/, nil /*gate*/, nil, nil, maxScope, 0, nil)
	r := chi.NewRouter()
	injectClaims := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := apimiddleware.InjectClaimsForTest(req.Context(), &keeperjwt.Claims{Subject: "archon-alice"})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
	r.Route("/v1/voyages", func(r chi.Router) {
		r.With(injectClaims).Group(func(r chi.Router) { registerHumaVoyageCreate(newHumaCadenceAPI(r), voyageH) })
		r.With(injectClaims).Group(func(r chi.Router) { registerHumaVoyagePreview(newHumaCadenceAPI(r), voyageH) })
	})
	return r
}

const lockedScenarioBody = `{"kind":"scenario","scenario_name":"deploy","target":{"incarnations":["demo","other"]}}`

// A scenario Voyage whose target holds an error_locked incarnation is refused
// before it exists — 409 incarnation-locked naming the run that locked it and the
// way out — instead of being accepted and failing that target 7 ms later with the
// reason only in the Keeper log (NIM-989). Preview answers the same, so a client
// that previews learns it before creating.
func TestHumaVoyage_Scenario_ErrorLockedTarget_409BeforeCreate(t *testing.T) {
	inc := lockedIncReader{
		statuses: map[string]string{"demo": "error_locked"},
		details:  map[string]string{"demo": `{"apply_id":"01HLOCKER","reason":"apply_failed"}`},
	}
	for _, path := range []string{"/v1/voyages", "/v1/voyages/preview"} {
		t.Run(path, func(t *testing.T) {
			store := &fakeVoyageStore{}
			r := humaVoyageRouterLocked(t, store, []string{"demo", "other"}, inc)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(lockedScenarioBody)))

			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
			}
			var p struct {
				Type   string `json:"type"`
				Detail string `json:"detail"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
				t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
			}
			if !strings.HasSuffix(p.Type, "/incarnation-locked") {
				t.Errorf("type = %q, want incarnation-locked", p.Type)
			}
			for _, want := range []string{"demo is error_locked by run 01HLOCKER", "/v1/incarnations/demo/unlock", "/v1/incarnations/demo/rerun-last"} {
				if !strings.Contains(p.Detail, want) {
					t.Errorf("detail %q does not contain %q", p.Detail, want)
				}
			}
			if strings.Contains(p.Detail, "other") {
				t.Errorf("detail %q names the ready incarnation too", p.Detail)
			}
			if store.insertCalls != 0 {
				t.Errorf("voyages INSERT calls = %d, want 0 — the refusal must come before the Voyage exists", store.insertCalls)
			}
		})
	}
}

// The control: with nothing locked the same request is accepted. Without it the
// 409 above could be any refusal the fake provokes.
func TestHumaVoyage_Scenario_NoLockedTarget_Accepted(t *testing.T) {
	store := &fakeVoyageStore{}
	r := humaVoyageRouterLocked(t, store, []string{"demo", "other"}, lockedIncReader{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/voyages", strings.NewReader(lockedScenarioBody)))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if store.insertCalls != 1 {
		t.Errorf("voyages INSERT calls = %d, want 1", store.insertCalls)
	}
}

// Only error_locked is refused at creation (the user's decision for NIM-989): an
// applying incarnation is still accepted and gets its reason on the target row.
func TestHumaVoyage_Scenario_ApplyingTarget_NotRefusedAtCreate(t *testing.T) {
	store := &fakeVoyageStore{}
	r := humaVoyageRouterLocked(t, store, []string{"demo"}, lockedIncReader{statuses: map[string]string{"demo": "applying"}})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/voyages",
		strings.NewReader(`{"kind":"scenario","scenario_name":"deploy","target":{"incarnations":["demo"]}}`)))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
}

// The huma route serves targets through toVoyageTargetEntry, not through the
// handler DTO's own JSON: a field the converter does not copy never reaches the
// wire, whatever the handler read.
func TestToVoyageTargetEntry_CarriesReason(t *testing.T) {
	code, msg := "incarnation_error_locked", "incarnation demo is error_locked by run 01HLOCKER"
	got := toVoyageTargetEntry(handlers.VoyageTargetEntryDTO{
		TargetKind: "incarnation", TargetID: "demo", Status: "failed",
		ErrorCode: &code, ErrorMessage: &msg,
	})
	if got.ErrorCode == nil || *got.ErrorCode != code || got.ErrorMessage == nil || *got.ErrorMessage != msg {
		t.Errorf("wire entry reason = (%v, %v), want (%q, %q)", got.ErrorCode, got.ErrorMessage, code, msg)
	}
}

// denyIncarnation passes the bare incarnation.run check and refuses it on one
// named incarnation — a role scoped away from it.
type denyIncarnation struct{ name string }

func (d denyIncarnation) Check(_, _, _ string, ctx map[string]string) error {
	if ctx != nil && ctx["incarnation"] == d.name {
		return errors.New("denied")
	}
	return nil
}

// The user's order for NIM-989: a target the operator may not run, or one over
// voyage.max_scope, is answered as such even when it also holds an error_locked
// incarnation — the 409 comes only after both.
func TestHumaVoyage_Scenario_ErrorLocked_ForbiddenAndOversizedWin(t *testing.T) {
	inc := lockedIncReader{statuses: map[string]string{"demo": "error_locked"}}
	cases := []struct {
		name     string
		enf      apimiddleware.PermissionChecker
		maxScope int
		want     int
	}{
		{"forbidden incarnation", denyIncarnation{name: "other"}, 0, http.StatusForbidden},
		{"scope over max_scope", strictAllowAll{}, 1, http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeVoyageStore{}
			r := humaVoyageRouterLockedWith(t, store, []string{"demo", "other"}, inc, tc.enf, tc.maxScope)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/voyages", strings.NewReader(lockedScenarioBody)))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}
