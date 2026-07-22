package api

// Guard tests for ROLLOUT-BATCH-2c, migrating the HERALD domain (heralds + tidings) ENTIRELY to
// huma full-typed (ADR-054 §Pattern, references role/operator/augur/push-provider).
// herald create/update/delete + tiding create/update/delete — WRITE+AUDIT (variant B,
// huma-audit middleware; events herald.created/.updated/.deleted and tiding.created/
// .updated/.deleted); herald/tiding list/get — read (no audit). They prove the cluster
// invariants over chi:
//
//   - wire/golden: herald create 201 Herald; herald list 200 envelope; herald get 200;
//     herald update 200 Herald; herald delete 204 empty; tiding create 201 Tiding;
//     tiding list 200; tiding delete 204 (byte-exact);
//   - unknown-field → 400; missing-required → 422; bad type enum → 422; bad pagination
//     → 400; bad include_ephemeral bool → 400; RBAC-deny → 403;
//   - S6-GUARD on EVERY write route: full huma wiring writes an audit event with a NON-EMPTY
//     payload + the CORRECT event-type on 2xx and does NOT write on 4xx/403.

import (
	"context"
	"encoding/json"
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
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/herald"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// heraldAt — the fixed created_at/updated_at that all herald success paths
// return (deterministic golden wire).
var heraldAt = time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)

// hHeraldPool — narrow mock of [herald.ExecQueryRower] for the huma test (Exec/QueryRow/Query).
// Classifies SQL by substring and returns a deterministic success outcome;
// the error classification is validated by the handlers/herald unit tests.
type hHeraldPool struct {
	heraldDeleteRows int64
	heraldUpdateRows int64
	tidingDeleteRows int64
	heraldGetMissing bool // SELECT FROM heralds WHERE name → ErrNoRows (404)
	tidingGetMissing bool // SELECT FROM tidings WHERE name → ErrNoRows (404)
	heraldListRows   [][]any
	tidingListRows   [][]any
}

func (p *hHeraldPool) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	switch {
	case strings.Contains(sql, "UPDATE heralds"), strings.Contains(sql, "UPDATE tidings"):
		// heraldUpdateRows is reused for both UPDATE routes (herald.update / tiding.update):
		// each test wires its OWN pool per case, so there is no collision.
		return pgconn.NewCommandTag("UPDATE " + hHeraldItoa(p.heraldUpdateRows)), nil
	case strings.Contains(sql, "DELETE FROM heralds"):
		return pgconn.NewCommandTag("DELETE " + hHeraldItoa(p.heraldDeleteRows)), nil
	case strings.Contains(sql, "DELETE FROM tidings"):
		return pgconn.NewCommandTag("DELETE " + hHeraldItoa(p.tidingDeleteRows)), nil
	}
	return pgconn.CommandTag{}, &hHeraldErr{"hHeraldPool: unexpected Exec SQL: " + sql}
}

func (p *hHeraldPool) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "INSERT INTO heralds"):
		return hHeraldRow{values: []any{heraldAt, heraldAt}} // RETURNING created_at, updated_at
	case strings.Contains(sql, "INSERT INTO tidings"):
		return hHeraldRow{values: []any{heraldAt, heraldAt}} // RETURNING created_at, updated_at
	case strings.Contains(sql, "FROM heralds") && strings.Contains(sql, "WHERE name"):
		if p.heraldGetMissing {
			return hHeraldRow{err: pgx.ErrNoRows}
		}
		return hHeraldRow{values: heraldScanRow()}
	case strings.Contains(sql, "FROM tidings") && strings.Contains(sql, "WHERE name"):
		if p.tidingGetMissing {
			return hHeraldRow{err: pgx.ErrNoRows}
		}
		return hHeraldRow{values: tidingScanRow()}
	case strings.Contains(sql, "COUNT(*) FROM heralds"):
		return hHeraldRow{values: []any{len(p.heraldListRows)}}
	case strings.Contains(sql, "COUNT(*) FROM tidings"):
		return hHeraldRow{values: []any{len(p.tidingListRows)}}
	}
	return hHeraldRow{err: &hHeraldErr{"hHeraldPool: unexpected QueryRow SQL: " + sql}}
}

func (p *hHeraldPool) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	switch {
	case strings.Contains(sql, "FROM heralds") && strings.Contains(sql, "ORDER BY"):
		return &hHeraldRows{rows: p.heraldListRows}, nil
	case strings.Contains(sql, "FROM tidings") && strings.Contains(sql, "ORDER BY"):
		return &hHeraldRows{rows: p.tidingListRows}, nil
	}
	return nil, &hHeraldErr{"hHeraldPool: unexpected Query SQL: " + sql}
}

// heraldScanRow — scanHerald columns: name, type, config(jsonb-bytes), secret_ref,
// enabled, created_at, updated_at, created_by_aid.
func heraldScanRow() []any {
	return []any{"ops-webhook", "webhook", []byte(`{"url":"https://hook.test/notify"}`), nil, true, heraldAt, heraldAt, nil}
}

// tidingScanRow — scanTiding columns: name, herald, event_types, only_failures,
// only_changes, incarnation, cadence, task, ephemeral, voyage_id,
// created_from_cadence_id, annotations(jsonb-bytes), projection([]string), enabled,
// created_at, updated_at, created_by_aid.
func tidingScanRow() []any {
	return []any{
		"on-fail", "ops-webhook", []string{"scenario_run.*"}, false, false,
		nil, nil, nil, false, nil, nil, []byte(nil), []string{}, true,
		heraldAt, heraldAt, nil,
	}
}

type hHeraldErr struct{ s string }

func (e *hHeraldErr) Error() string { return e.s }

func hHeraldItoa(n int64) string {
	if n == 0 {
		return "0"
	}
	return "1"
}

// hHeraldRow — staticRow for herald/tiding columns (string/time/int/bool/[]byte/
// []string + nullable pointers).
type hHeraldRow struct {
	values []any
	err    error
}

func (r hHeraldRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, d := range dest {
		switch dd := d.(type) {
		case *string:
			*dd = r.values[i].(string)
		case *int:
			*dd = r.values[i].(int)
		case *bool:
			*dd = r.values[i].(bool)
		case *time.Time:
			*dd = r.values[i].(time.Time)
		case *[]byte:
			if r.values[i] == nil {
				*dd = nil
			} else {
				*dd = r.values[i].([]byte)
			}
		case *[]string:
			*dd = r.values[i].([]string)
		case **string:
			if r.values[i] == nil {
				*dd = nil
			} else {
				s := r.values[i].(string)
				*dd = &s
			}
		}
	}
	return nil
}

type hHeraldRows struct {
	rows [][]any
	idx  int
}

func (r *hHeraldRows) Next() bool { r.idx++; return r.idx <= len(r.rows) }
func (r *hHeraldRows) Scan(dest ...any) error {
	return hHeraldRow{values: r.rows[r.idx-1]}.Scan(dest...)
}
func (r *hHeraldRows) Err() error                                   { return nil }
func (r *hHeraldRows) Close()                                       {}
func (r *hHeraldRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *hHeraldRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *hHeraldRows) Values() ([]any, error)                       { return nil, nil }
func (r *hHeraldRows) RawValues() [][]byte                          { return nil }
func (r *hHeraldRows) Conn() *pgx.Conn                              { return nil }

// humaHeraldRouter assembles a chi router with ALL herald/tiding routes via huma —
// the production wiring from router.go: RequirePermission(herald/tiding.<action>) on each
// group + (for write) huma-audit middleware variant B + the huma operation. injectClaims
// replaces RequireJWT.
func humaHeraldRouter(t *testing.T, enforcer apimiddleware.PermissionChecker, auditW audit.Writer, pool *hHeraldPool) *chi.Mux {
	t.Helper()
	installHumaErrorOverride()
	svc, err := herald.NewService(herald.ServiceDeps{Pool: pool})
	if err != nil {
		t.Fatalf("herald.NewService: %v", err)
	}
	heraldH := handlers.NewHeraldHandler(svc, nil)

	r := chi.NewRouter()
	injectClaims := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := apimiddleware.InjectClaimsForTest(req.Context(), &keeperjwt.Claims{Subject: "archon-alice"})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
	r.Route("/v1", func(r chi.Router) {
		r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "herald", "create", apimiddleware.NoSelector)).Group(func(r chi.Router) {
			registerHumaHeraldCreate(newHumaHeraldAPI(r, auditW, audit.EventHeraldCreated, nil), heraldH)
		})
		r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "herald", "list", apimiddleware.NoSelector)).Group(func(r chi.Router) {
			registerHumaHeraldList(newHumaCadenceAPI(r), heraldH)
		})
		r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "herald", "read", apimiddleware.NoSelector)).Group(func(r chi.Router) {
			registerHumaHeraldGet(newHumaCadenceAPI(r), heraldH)
		})
		r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "herald", "update", apimiddleware.NoSelector)).Group(func(r chi.Router) {
			registerHumaHeraldUpdate(newHumaHeraldAPI(r, auditW, audit.EventHeraldUpdated, nil), heraldH)
		})
		r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "herald", "delete", apimiddleware.NoSelector)).Group(func(r chi.Router) {
			registerHumaHeraldDelete(newHumaHeraldAPI(r, auditW, audit.EventHeraldDeleted, nil), heraldH)
		})
		r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "tiding", "create", apimiddleware.NoSelector)).Group(func(r chi.Router) {
			registerHumaTidingCreate(newHumaHeraldAPI(r, auditW, audit.EventTidingCreated, nil), heraldH)
		})
		r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "tiding", "list", apimiddleware.NoSelector)).Group(func(r chi.Router) {
			registerHumaTidingList(newHumaCadenceAPI(r), heraldH)
		})
		r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "tiding", "read", apimiddleware.NoSelector)).Group(func(r chi.Router) {
			registerHumaTidingGet(newHumaCadenceAPI(r), heraldH)
		})
		r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "tiding", "update", apimiddleware.NoSelector)).Group(func(r chi.Router) {
			registerHumaTidingUpdate(newHumaHeraldAPI(r, auditW, audit.EventTidingUpdated, nil), heraldH)
		})
		r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "tiding", "delete", apimiddleware.NoSelector)).Group(func(r chi.Router) {
			registerHumaTidingDelete(newHumaHeraldAPI(r, auditW, audit.EventTidingDeleted, nil), heraldH)
		})
	})
	return r
}

const heraldCreateJSON = `{"name":"ops-webhook","type":"webhook","config":{"url":"https://hook.test/notify"}}`

// === HERALD CREATE (WRITE+AUDIT herald.created) ===

func TestHumaHerald_Create_GoldenWire(t *testing.T) {
	r := humaHeraldRouter(t, strictAllowAll{}, nil, &hHeraldPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/heralds", strings.NewReader(heraldCreateJSON))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply is not a JSON-object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"config":{"url":"https://hook.test/notify"},"created_at":"2026-06-13T10:00:00Z","created_by_aid":"archon-alice","enabled":true,"name":"ops-webhook","type":"webhook","updated_at":"2026-06-13T10:00:00Z"}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-drift herald.create:\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaHerald_Create_UnknownField_400(t *testing.T) {
	r := humaHeraldRouter(t, strictAllowAll{}, nil, &hHeraldPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/heralds",
		strings.NewReader(`{"name":"x","type":"webhook","config":{"url":"https://h.test"},"bogus":1}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

func TestHumaHerald_Create_MissingName_422(t *testing.T) {
	r := humaHeraldRouter(t, strictAllowAll{}, nil, &hHeraldPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/heralds",
		strings.NewReader(`{"type":"webhook","config":{"url":"https://h.test"}}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

func TestHumaHerald_Create_BadType_422(t *testing.T) {
	r := humaHeraldRouter(t, strictAllowAll{}, nil, &hHeraldPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/heralds",
		strings.NewReader(`{"name":"x","type":"slack","config":{"url":"https://h.test"}}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (bad type enum); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

func TestHumaHerald_Create_RBACDeny_403(t *testing.T) {
	r := humaHeraldRouter(t, strictDenyAll{}, nil, &hHeraldPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/heralds", strings.NewReader(heraldCreateJSON))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaAudit_HeraldCreate_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaHeraldRouter(t, strictAllowAll{}, auditCap, &hHeraldPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/heralds", strings.NewReader(heraldCreateJSON))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventHeraldCreated, map[string]any{
		"name": "ops-webhook", "type": "webhook", "enabled": true,
		"url": "https://hook.test/notify",
	})
}

func TestHumaAudit_HeraldCreate_NoAudit_OnRBACDeny(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaHeraldRouter(t, strictDenyAll{}, auditCap, &hHeraldPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/heralds", strings.NewReader(heraldCreateJSON))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit recorded on RBAC-deny herald.create (%d events)", len(auditCap.Events()))
	}
}

func TestHumaAudit_HeraldCreate_NoAudit_OnValidationFail(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaHeraldRouter(t, strictAllowAll{}, auditCap, &hHeraldPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/heralds",
		strings.NewReader(`{"type":"webhook","config":{"url":"https://h.test"}}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit recorded on 422 herald.create (%d events)", len(auditCap.Events()))
	}
}

// === HERALD LIST (READ with typed query, no audit) ===

func TestHumaHerald_List_GoldenWire(t *testing.T) {
	pool := &hHeraldPool{heraldListRows: [][]any{heraldScanRow()}}
	r := humaHeraldRouter(t, strictAllowAll{}, nil, pool)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/heralds", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply is not a JSON-object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"items":[{"config":{"url":"https://hook.test/notify"},"created_at":"2026-06-13T10:00:00Z","enabled":true,"name":"ops-webhook","type":"webhook","updated_at":"2026-06-13T10:00:00Z"}],"limit":50,"offset":0,"total":1}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-drift herald.list:\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaHerald_List_GoldenEmpty(t *testing.T) {
	r := humaHeraldRouter(t, strictAllowAll{}, nil, &hHeraldPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/heralds", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	const golden = `{"items":[],"limit":50,"offset":0,"total":0}`
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	out, _ := json.Marshal(m)
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-drift herald.list (empty): got=%q want=%q", got, golden)
	}
}

func TestHumaHerald_List_BadOffset_400(t *testing.T) {
	r := humaHeraldRouter(t, strictAllowAll{}, nil, &hHeraldPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/heralds?offset=-1", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (offset<0 → CheckPageBounds 400); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

func TestHumaHerald_List_BadLimit_400(t *testing.T) {
	r := humaHeraldRouter(t, strictAllowAll{}, nil, &hHeraldPool{})
	for _, c := range []string{"/v1/heralds?limit=0", "/v1/heralds?limit=1001"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, c, nil)
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (out-of-range limit → CheckPageBounds 400); body=%s", c, rec.Code, rec.Body.String())
			continue
		}
		assertHumaProblem(t, rec, problem.TypeMalformedRequest)
	}
}

func TestHumaHerald_List_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaHeraldRouter(t, strictAllowAll{}, auditCap, &hHeraldPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/heralds", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("READ-route herald.list recorded audit (%d events)", len(auditCap.Events()))
	}
}

// === HERALD GET (READ with path, no audit) ===

func TestHumaHerald_Get_GoldenWire(t *testing.T) {
	r := humaHeraldRouter(t, strictAllowAll{}, nil, &hHeraldPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/heralds/ops-webhook", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply is not a JSON-object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"config":{"url":"https://hook.test/notify"},"created_at":"2026-06-13T10:00:00Z","enabled":true,"name":"ops-webhook","type":"webhook","updated_at":"2026-06-13T10:00:00Z"}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-drift herald.get:\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaHerald_Get_NotFound_404(t *testing.T) {
	r := humaHeraldRouter(t, strictAllowAll{}, nil, &hHeraldPool{heraldGetMissing: true})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/heralds/ghost", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeNotFound)
}

func TestHumaHerald_Get_BadName_422(t *testing.T) {
	r := humaHeraldRouter(t, strictAllowAll{}, nil, &hHeraldPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/heralds/BAD_NAME", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (bad path-name); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

// === HERALD UPDATE (WRITE+AUDIT herald.updated) ===

func TestHumaHerald_Update_GoldenWire(t *testing.T) {
	r := humaHeraldRouter(t, strictAllowAll{}, nil, &hHeraldPool{heraldUpdateRows: 1})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/heralds/ops-webhook",
		strings.NewReader(`{"type":"webhook","config":{"url":"https://hook.test/notify"}}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply is not a JSON-object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"config":{"url":"https://hook.test/notify"},"created_at":"2026-06-13T10:00:00Z","enabled":true,"name":"ops-webhook","type":"webhook","updated_at":"2026-06-13T10:00:00Z"}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-drift herald.update:\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaHerald_Update_NotFound_404(t *testing.T) {
	r := humaHeraldRouter(t, strictAllowAll{}, nil, &hHeraldPool{heraldUpdateRows: 0})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/heralds/ghost",
		strings.NewReader(`{"type":"webhook","config":{"url":"https://hook.test/notify"}}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeNotFound)
}

func TestHumaAudit_HeraldUpdate_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaHeraldRouter(t, strictAllowAll{}, auditCap, &hHeraldPool{heraldUpdateRows: 1})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/heralds/ops-webhook",
		strings.NewReader(`{"type":"webhook","config":{"url":"https://hook.test/notify"}}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventHeraldUpdated, map[string]any{
		"name": "ops-webhook", "type": "webhook", "enabled": true,
	})
}

func TestHumaAudit_HeraldUpdate_NoAudit_OnNotFound(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaHeraldRouter(t, strictAllowAll{}, auditCap, &hHeraldPool{heraldUpdateRows: 0})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/heralds/ghost",
		strings.NewReader(`{"type":"webhook","config":{"url":"https://hook.test/notify"}}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit recorded on 404 herald.update (%d events)", len(auditCap.Events()))
	}
}

// === HERALD DELETE (WRITE+AUDIT herald.deleted) ===

func TestHumaHerald_Delete_204(t *testing.T) {
	r := humaHeraldRouter(t, strictAllowAll{}, nil, &hHeraldPool{heraldDeleteRows: 1})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/heralds/ops-webhook", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "" {
		t.Errorf("204-body herald.delete must be EMPTY, got %q", body)
	}
}

func TestHumaAudit_HeraldDelete_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaHeraldRouter(t, strictAllowAll{}, auditCap, &hHeraldPool{heraldDeleteRows: 1})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/heralds/ops-webhook", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventHeraldDeleted, map[string]any{"name": "ops-webhook"})
}

func TestHumaAudit_HeraldDelete_NoAudit_OnNotFound(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaHeraldRouter(t, strictAllowAll{}, auditCap, &hHeraldPool{heraldDeleteRows: 0})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/heralds/ghost", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit recorded on 404 herald.delete (%d events)", len(auditCap.Events()))
	}
}

// === TIDING CREATE (WRITE+AUDIT tiding.created) ===

const tidingCreateJSON = `{"name":"on-fail","herald":"ops-webhook","event_types":["scenario_run.*"]}`

func TestHumaTiding_Create_GoldenWire(t *testing.T) {
	r := humaHeraldRouter(t, strictAllowAll{}, nil, &hHeraldPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/tidings", strings.NewReader(tidingCreateJSON))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply is not a JSON-object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"created_at":"2026-06-13T10:00:00Z","created_by_aid":"archon-alice","enabled":true,"ephemeral":false,"event_types":["scenario_run.*"],"herald":"ops-webhook","name":"on-fail","only_changes":false,"only_failures":false,"updated_at":"2026-06-13T10:00:00Z"}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-drift tiding.create:\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaTiding_Create_UnknownField_400(t *testing.T) {
	r := humaHeraldRouter(t, strictAllowAll{}, nil, &hHeraldPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/tidings",
		strings.NewReader(`{"name":"x","herald":"h","event_types":["scenario_run.*"],"bogus":1}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

func TestHumaTiding_Create_MissingHerald_422(t *testing.T) {
	r := humaHeraldRouter(t, strictAllowAll{}, nil, &hHeraldPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/tidings",
		strings.NewReader(`{"name":"x","event_types":["scenario_run.*"]}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (missing required herald); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

func TestHumaTiding_Create_RBACDeny_403(t *testing.T) {
	r := humaHeraldRouter(t, strictDenyAll{}, nil, &hHeraldPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/tidings", strings.NewReader(tidingCreateJSON))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaAudit_TidingCreate_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaHeraldRouter(t, strictAllowAll{}, auditCap, &hHeraldPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/tidings", strings.NewReader(tidingCreateJSON))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventTidingCreated, map[string]any{
		"name": "on-fail", "herald": "ops-webhook",
		"only_failures": false, "only_changes": false, "enabled": true,
	})
}

// === TIDING LIST (READ with typed query, no audit) ===

func TestHumaTiding_List_GoldenWire(t *testing.T) {
	pool := &hHeraldPool{tidingListRows: [][]any{tidingScanRow()}}
	r := humaHeraldRouter(t, strictAllowAll{}, nil, pool)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/tidings", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply is not a JSON-object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"items":[{"created_at":"2026-06-13T10:00:00Z","enabled":true,"ephemeral":false,"event_types":["scenario_run.*"],"herald":"ops-webhook","name":"on-fail","only_changes":false,"only_failures":false,"updated_at":"2026-06-13T10:00:00Z"}],"limit":50,"offset":0,"total":1}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-drift tiding.list:\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaTiding_List_BadIncludeEphemeral_400(t *testing.T) {
	r := humaHeraldRouter(t, strictAllowAll{}, nil, &hHeraldPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/tidings?include_ephemeral=notabool", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (bad include_ephemeral bool → huma-bind 400); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

func TestHumaTiding_List_BadOffset_400(t *testing.T) {
	r := humaHeraldRouter(t, strictAllowAll{}, nil, &hHeraldPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/tidings?offset=-1", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

func TestHumaTiding_List_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaHeraldRouter(t, strictAllowAll{}, auditCap, &hHeraldPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/tidings", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("READ-route tiding.list recorded audit (%d events)", len(auditCap.Events()))
	}
}

// === TIDING GET (READ with path, no audit) ===

func TestHumaTiding_Get_NotFound_404(t *testing.T) {
	r := humaHeraldRouter(t, strictAllowAll{}, nil, &hHeraldPool{tidingGetMissing: true})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/tidings/ghost", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeNotFound)
}

// === TIDING UPDATE (WRITE+AUDIT tiding.updated) ===

func TestHumaAudit_TidingUpdate_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	// UPDATE tidings → rows affected >0 (via the UpdateTiding tag), then re-read SELECT.
	r := humaHeraldRouter(t, strictAllowAll{}, auditCap, &hHeraldPool{heraldUpdateRows: 1})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/tidings/on-fail",
		strings.NewReader(`{"herald":"ops-webhook","event_types":["scenario_run.*"]}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventTidingUpdated, map[string]any{
		"name": "on-fail", "herald": "ops-webhook",
	})
}

// === TIDING DELETE (WRITE+AUDIT tiding.deleted) ===

func TestHumaTiding_Delete_204(t *testing.T) {
	r := humaHeraldRouter(t, strictAllowAll{}, nil, &hHeraldPool{tidingDeleteRows: 1})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/tidings/on-fail", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "" {
		t.Errorf("204-body tiding.delete must be EMPTY, got %q", body)
	}
}

func TestHumaAudit_TidingDelete_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaHeraldRouter(t, strictAllowAll{}, auditCap, &hHeraldPool{tidingDeleteRows: 1})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/tidings/on-fail", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventTidingDeleted, map[string]any{"name": "on-fail"})
}

func TestHumaAudit_TidingDelete_NoAudit_OnNotFound(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaHeraldRouter(t, strictAllowAll{}, auditCap, &hHeraldPool{tidingDeleteRows: 0})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/tidings/ghost", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit recorded on 404 tiding.delete (%d events)", len(auditCap.Events()))
	}
}

// === OpenAPI fragment: ALL herald/tiding operations from FULL-TYPED Go types ===

func TestHumaHerald_OpenAPIFragment_3_1(t *testing.T) {
	frag, err := HumaHeraldSpecYAML()
	if err != nil {
		t.Fatalf("HumaHeraldSpecYAML: %v", err)
	}
	if !strings.Contains(frag, "openapi: 3.1.0") {
		t.Errorf("huma-fragment does not carry `openapi: 3.1.0`:\n%s", frag)
	}
	for _, want := range []string{
		"createHerald", "listHeralds", "getHerald", "updateHerald", "deleteHerald",
		"createTiding", "listTidings", "getTiding", "updateTiding", "deleteTiding",
		"event_types",
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("OpenAPI-fragment does not contain %q:\n%s", want, frag)
		}
	}
	if strings.Contains(frag, "octet-stream") {
		t.Errorf("OpenAPI-fragment carries application/octet-stream:\n%s", frag)
	}
}
