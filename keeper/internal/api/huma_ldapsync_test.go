package api

// HTTP-level guards of /v1/ldap-sync (ADR-058 amendment NIM-1053): the
// setting.* gate in front of both routes, the interval bounds refused before
// anything is written, and the audit event of an accepted change.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/ldapsync"
	"github.com/souls-guild/soul-stack/shared/audit"
)

type fakeLDAPSyncStore struct {
	settings ldapsync.Settings
	set      bool
	loadErr  error
	status   ldapsync.Status
	saves    []ldapsync.Settings
	savedBy  string
}

func (f *fakeLDAPSyncStore) LoadSettings(context.Context) (ldapsync.Settings, bool, error) {
	if f.loadErr != nil {
		return ldapsync.Settings{}, true, f.loadErr
	}
	if !f.set {
		return ldapsync.DefaultSettings(), false, nil
	}
	return f.settings, true, nil
}

func (f *fakeLDAPSyncStore) SaveSettings(_ context.Context, s ldapsync.Settings, by string) error {
	f.saves = append(f.saves, s)
	f.savedBy = by
	f.settings, f.set = s, true
	return nil
}

func (f *fakeLDAPSyncStore) Status(context.Context) (ldapsync.Status, error) { return f.status, nil }

func humaLDAPSyncRouter(t *testing.T, enforcer apimiddleware.PermissionChecker, auditW audit.Writer, store *fakeLDAPSyncStore) *chi.Mux {
	t.Helper()
	installHumaErrorOverride()
	h := handlers.NewLDAPSyncHandler(store, true, nil)
	injectClaims := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := apimiddleware.InjectClaimsForTest(req.Context(), &keeperjwt.Claims{Subject: "archon-alice"})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
	r := chi.NewRouter()
	r.Route("/v1/ldap-sync", func(r chi.Router) {
		r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "setting", "read", apimiddleware.NoSelector)).Group(func(r chi.Router) {
			registerHumaLDAPSyncGet(newHumaCadenceAPI(r), h)
		})
		r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "setting", "update", apimiddleware.NoSelector)).Group(func(r chi.Router) {
			registerHumaLDAPSyncPut(newHumaAuditAPI(r, auditW, audit.EventLDAPSyncSettingsChanged, nil), h)
		})
	})
	return r
}

func TestHumaLDAPSync_Put_RBACDeny_403(t *testing.T) {
	store := &fakeLDAPSyncStore{}
	auditCap := &auditCaptureWriter{}
	r := humaLDAPSyncRouter(t, strictDenyAll{}, auditCap, store)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/ldap-sync", strings.NewReader(`{"interval":"1h"}`)))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if len(store.saves) != 0 || len(auditCap.Events()) != 0 {
		t.Errorf("denied update reached the store (%d saves) or the audit (%d events)", len(store.saves), len(auditCap.Events()))
	}
}

func TestHumaLDAPSync_Get_RBACDeny_403(t *testing.T) {
	r := humaLDAPSyncRouter(t, strictDenyAll{}, nil, &fakeLDAPSyncStore{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/ldap-sync", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// The interval is how long a role lost in the directory keeps working, so its
// bounds are refused here, before Postgres, not discovered by the leader.
func TestHumaLDAPSync_Put_OutOfBoundsIsRejectedBeforeWrite(t *testing.T) {
	for _, body := range []string{`{"interval":"30s"}`, `{"interval":"25h"}`, `{"interval":"often"}`, `{"interval":""}`} {
		t.Run(body, func(t *testing.T) {
			store := &fakeLDAPSyncStore{}
			auditCap := &auditCaptureWriter{}
			r := humaLDAPSyncRouter(t, strictAllowAll{}, auditCap, store)

			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/ldap-sync", strings.NewReader(body)))

			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
			}
			assertHumaProblem(t, rec, problem.TypeValidationFailed)
			if len(store.saves) != 0 || len(auditCap.Events()) != 0 {
				t.Errorf("rejected update reached the store (%d saves) or the audit (%d events)", len(store.saves), len(auditCap.Events()))
			}
		})
	}
}

func TestHumaLDAPSync_Put_AcceptedIsSavedAndAudited(t *testing.T) {
	store := &fakeLDAPSyncStore{settings: ldapsync.Settings{Interval: 15 * time.Minute}, set: true}
	auditCap := &auditCaptureWriter{}
	r := humaLDAPSyncRouter(t, strictAllowAll{}, auditCap, store)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/ldap-sync", strings.NewReader(`{"interval":"1h"}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(store.saves) != 1 || store.saves[0].Interval != time.Hour || store.savedBy != "archon-alice" {
		t.Fatalf("saves = %+v by %q, want one 1h save by archon-alice", store.saves, store.savedBy)
	}
	var got LDAPSyncReply
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Settings.Interval != "1h" || !got.SettingsSet {
		t.Errorf("body = %+v, want settings.interval=1h settings_set=true", got)
	}

	evs := auditCap.Events()
	if len(evs) != 1 || evs[0].EventType != audit.EventLDAPSyncSettingsChanged || evs[0].ArchonAID != "archon-alice" {
		t.Fatalf("audit = %+v, want one ldap_sync.settings_changed by archon-alice", evs)
	}
	p, _ := json.Marshal(evs[0].Payload)
	if !strings.Contains(string(p), `"settings":{"interval":"1h"}`) || !strings.Contains(string(p), `"previous":{"interval":"15m"}`) {
		t.Errorf("audit payload = %s, want the new settings and the previous ones", p)
	}
}

// The RBAC probe walks the router the gate sweeps build; a route missing from
// it is skipped there without a word. Pin that /v1/ldap-sync is in it and that
// the gate it runs is the one the table declares.
func TestLDAPSync_MountedInTheGateSweepRouter(t *testing.T) {
	rec := zeroGrantRBAC(t)
	h := revokedGateRouterWebUI(t, rec)
	token := revokedGateToken(t, rbacGateAID)
	for method, want := range map[string]rbacAsk{http.MethodGet: "check:setting.read", http.MethodPut: "check:setting.update"} {
		rec.take()
		resp, _ := serveRecovering(h, method, "/v1/ldap-sync", token)
		if got := rec.take(); len(got) != 1 || got[0] != want {
			t.Errorf("%s /v1/ldap-sync asked %v, want [%s] (status %d)", method, got, want, resp.Code)
		}
	}
}

// A stored value this version cannot read: the status still answers, and the
// settings say plainly that they are the fallback.
func TestHumaLDAPSync_Get_UnreadableStoredValue(t *testing.T) {
	r := humaLDAPSyncRouter(t, strictAllowAll{}, nil, &fakeLDAPSyncStore{loadErr: ldapsync.ErrInvalidSettings})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/ldap-sync", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got LDAPSyncReply
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Settings.Interval != "15m" || got.SettingsError == "" {
		t.Errorf("reply = %+v, want the 15m default with settings_error set", got)
	}
}

func TestHumaLDAPSync_Get_ReportsDefaultsAndStatus(t *testing.T) {
	last := time.Date(2026, 10, 24, 21, 15, 0, 0, time.UTC)
	store := &fakeLDAPSyncStore{status: ldapsync.Status{
		LastRunAt:     &last,
		LastSuccessAt: &last,
		Operators:     map[ldapsync.Outcome]int{ldapsync.OutcomeSynced: 41, ldapsync.OutcomeNotFound: 1},
		Pending:       2,
	}}
	r := humaLDAPSyncRouter(t, strictAllowAll{}, nil, store)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/ldap-sync", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got LDAPSyncReply
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Settings.Interval != "15m" || got.SettingsSet {
		t.Errorf("settings = %+v set=%v, want the 15m default, unset", got.Settings, got.SettingsSet)
	}
	ops := got.Status.Operators
	if !got.Status.Enabled || ops.Synced != 41 || ops.NotFound != 1 || ops.Pending != 2 || got.Status.LastRunAt == nil {
		t.Errorf("status = %+v, want enabled, 41 synced, 1 not_found, 2 pending, a last run", got.Status)
	}
}
