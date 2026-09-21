package api

// HTTP-level guards of the settings surface (ADR-0073): the RBAC gate in front
// of every route, and the write-gate rejecting a candidate that would not
// survive the merge. Both assert the same thing from two sides — nothing
// reaches `keeper_settings` unless it is both permitted and valid.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// hSettingsPool records every statement that reached Postgres.
type hSettingsPool struct {
	writes  int
	deletes int
}

func (p *hSettingsPool) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "DELETE FROM keeper_settings") {
		p.deletes++
		return pgconn.NewCommandTag("DELETE 1"), nil
	}
	return pgconn.CommandTag{}, nil
}

func (p *hSettingsPool) QueryRow(context.Context, string, ...any) pgx.Row {
	p.writes++
	return hSettingsRow{}
}

func (p *hSettingsPool) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("hSettingsPool.Query unused")
}

type hSettingsRow struct{}

func (hSettingsRow) Scan(dest ...any) error {
	for _, d := range dest {
		if tp, ok := d.(*time.Time); ok {
			*tp = time.Now()
		}
	}
	return nil
}

// hSettingsOverlay — the SettingsStore surface with no overrides in place.
type hSettingsOverlay struct{}

func (hSettingsOverlay) Values() map[string]any         { return nil }
func (hSettingsOverlay) Overlay() []config.OverlayEntry { return nil }
func (hSettingsOverlay) Refresh(context.Context) error  { return nil }

// settingsStoreFixture loads the golden keeper.yml into a real config.Store, so
// ValidateOverlay runs the production pipeline rather than a stub.
func settingsStoreFixture(t *testing.T) *config.Store[config.KeeperConfig] {
	t.Helper()
	data, err := os.ReadFile(filepath.FromSlash("../../../examples/keeper/keeper.yml"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), "keeper.yml")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	store, diags, err := config.LoadKeeperStore(path, config.ValidateOptions{})
	if err != nil || diag.HasErrors(diags) {
		t.Fatalf("load fixture: err=%v diags=%v", err, diags)
	}
	return store
}

func humaSettingsRouter(t *testing.T, enforcer apimiddleware.PermissionChecker, auditW audit.Writer, pool *hSettingsPool) *chi.Mux {
	t.Helper()
	installHumaErrorOverride()
	svc, err := serviceregistry.NewService(serviceregistry.ServiceDeps{Pool: pool})
	if err != nil {
		t.Fatalf("serviceregistry.NewService: %v", err)
	}
	settingsH := handlers.NewSettingsHandler(settingsStoreFixture(t), hSettingsOverlay{}, svc, nil)

	r := chi.NewRouter()
	injectClaims := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := apimiddleware.InjectClaimsForTest(req.Context(), &keeperjwt.Claims{Subject: "archon-alice"})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
	// The settings operations declare paths relative to the group, exactly as
	// router.go mounts them.
	r.Route("/v1", func(r chi.Router) {
		r.Route("/settings", func(r chi.Router) {
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "setting", "read", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaSettingsList(newHumaCadenceAPI(r), settingsH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "setting", "update", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaSettingPut(newHumaSettingsAPI(r, auditW, audit.EventSettingUpdated, nil), settingsH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "setting", "delete", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaSettingDelete(newHumaSettingsAPI(r, auditW, audit.EventSettingDeleted, nil), settingsH)
			})
		})
	})
	return r
}

// Editing a cluster-wide tunable is its own privilege (ADR-0073(i)): without
// setting.update the mutation is refused and Postgres is never touched.
func TestHumaSettings_Put_RBACDeny_403(t *testing.T) {
	pool := &hSettingsPool{}
	auditCap := &auditCaptureWriter{}
	r := humaSettingsRouter(t, strictDenyAll{}, auditCap, pool)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/settings/cfg_toll_threshold",
		strings.NewReader(`{"value":"0.5"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if pool.writes != 0 {
		t.Errorf("keeper_settings written on an RBAC-denied update")
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit recorded on an RBAC-denied update (%d events)", len(auditCap.Events()))
	}
}

func TestHumaSettings_Delete_RBACDeny_403(t *testing.T) {
	pool := &hSettingsPool{}
	r := humaSettingsRouter(t, strictDenyAll{}, nil, pool)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/settings/cfg_toll_threshold", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if pool.deletes != 0 {
		t.Errorf("keeper_settings row deleted on an RBAC-denied delete")
	}
}

func TestHumaSettings_List_RBACDeny_403(t *testing.T) {
	r := humaSettingsRouter(t, strictDenyAll{}, nil, &hSettingsPool{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/settings", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// ★ The write-gate end to end: `poll_floor: 10m` is inside the field's own
// bounds but above the resolved `poll_ceiling`, so the merged config would not
// validate. It must be refused HERE — a row that every reader rejects would
// freeze the whole overlay on its last-good snapshot (ADR-0073(h/i)).
func TestHumaSettings_Put_CrossFieldViolationIsRejectedBeforeWrite(t *testing.T) {
	pool := &hSettingsPool{}
	auditCap := &auditCaptureWriter{}
	r := humaSettingsRouter(t, strictAllowAll{}, auditCap, pool)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/settings/cfg_cadence_scheduler_poll_floor",
		strings.NewReader(`{"value":"10m"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
	if pool.writes != 0 {
		t.Errorf("keeper_settings written despite the rejection")
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit recorded for a rejected update (%d events)", len(auditCap.Events()))
	}
}

// A value within the corridor goes through, so the gate above is not simply
// refusing everything.
func TestHumaSettings_Put_ValidValueIsAccepted(t *testing.T) {
	pool := &hSettingsPool{}
	r := humaSettingsRouter(t, strictAllowAll{}, nil, pool)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/settings/cfg_cadence_scheduler_poll_floor",
		strings.NewReader(`{"value":"45s"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if pool.writes != 1 {
		t.Errorf("writes = %d, want 1", pool.writes)
	}
}

// ADR-042: the UI renders the form from this catalog, so every entry has to
// carry its own type and bounds — including the kinds the pilot never had.
func TestHumaSettings_CatalogIsSelfDescribing(t *testing.T) {
	r := humaSettingsRouter(t, strictAllowAll{}, nil, &hSettingsPool{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/settings", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	for _, want := range []string{
		`"key":"cfg_reaper_interval"`, `"type":"duration"`,
		`"key":"cfg_reaper_dry_run"`, `"type":"bool"`,
		`"bounds":"true | false"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("catalog is missing %s; body=%s", want, body)
		}
	}
}
