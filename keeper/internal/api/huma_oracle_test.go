package api

// Guard tests for ROLLOUT-BATCH-2b turning the ORACLE domain (vigils + decrees) ENTIRELY onto
// huma full-typed (ADR-054 §Pattern, references role/operator/augur). vigil create/delete
// + decree create/delete — WRITE+AUDIT (variant B, huma audit middleware; events
// vigil.created/vigil.deleted/decree.created/decree.deleted); vigil/decree list/get —
// read (no audit). They prove the cluster invariants on top of chi:
//
//   - wire/golden: vigil create 201 VigilView; vigil list 200 envelope; vigil get 200;
//     vigil delete 204 empty; decree symmetrically (params/action_input byte-exact JSONB);
//   - unknown-field → 400; missing-required → 422; bad pagination → 400; RBAC-deny → 403;
//   - S6-GUARD on EVERY write route: the full huma wiring writes an audit event with a NON-EMPTY
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
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/oracle"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// oracleAt — fixed created/updated_at for a deterministic golden wire.
var oracleAt = time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)

// hOraclePool — a narrow mock of [oracle.ServicePool] for the huma test. Classifies SQL by
// substring and returns a deterministic success outcome; error classification is validated by
// handlers/oracle_test.go.
type hOraclePool struct {
	vigilDeleteRows  int64
	decreeDeleteRows int64
	vigilGetMissing  bool
	decreeGetMissing bool
	vigilListRows    [][]any
	decreeListRows   [][]any
}

func (p *hOraclePool) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	switch {
	case strings.Contains(sql, "DELETE FROM vigils"):
		return pgconn.NewCommandTag("DELETE " + hOracleItoa(p.vigilDeleteRows)), nil
	case strings.Contains(sql, "DELETE FROM decrees"):
		return pgconn.NewCommandTag("DELETE " + hOracleItoa(p.decreeDeleteRows)), nil
	}
	return pgconn.CommandTag{}, &hOracleErr{"hOraclePool: unexpected Exec: " + sql}
}

func (p *hOraclePool) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "INSERT INTO vigils"):
		return hOracleRow{values: []any{oracleAt, oracleAt}} // RETURNING created_at, updated_at
	case strings.Contains(sql, "INSERT INTO decrees"):
		return hOracleRow{values: []any{"0s", oracleAt, oracleAt}} // RETURNING cooldown, created_at, updated_at
	case strings.Contains(sql, "FROM vigils") && strings.Contains(sql, "WHERE name"):
		if p.vigilGetMissing {
			return hOracleRow{err: pgx.ErrNoRows}
		}
		// scanVigil: name, coven, sid, interval_spec, check_addr, params, enabled, created_at, updated_at, created_by_aid.
		return hOracleRow{values: []any{"web-conf", []string{"web"}, nil, "30s", "core.beacon.file_changed", []byte(`{}`), true, oracleAt, oracleAt, nil}}
	case strings.Contains(sql, "FROM decrees") && strings.Contains(sql, "WHERE name"):
		if p.decreeGetMissing {
			return hOracleRow{err: pgx.ErrNoRows}
		}
		// scanDecree: name, on_beacon, where_cel, subject_coven, subject_sid, incarnation_name, action_scenario, action_input, cooldown, enabled, created_at, updated_at, created_by_aid.
		return hOracleRow{values: []any{"on-conf", "web-conf", nil, []string{"web"}, nil, "web", "reload", []byte(`{}`), "0s", true, oracleAt, oracleAt, nil}}
	case strings.Contains(sql, "COUNT(*) FROM vigils"):
		return hOracleRow{values: []any{len(p.vigilListRows)}}
	case strings.Contains(sql, "COUNT(*) FROM decrees"):
		return hOracleRow{values: []any{len(p.decreeListRows)}}
	}
	return hOracleRow{err: &hOracleErr{"hOraclePool: unexpected QueryRow: " + sql}}
}

func (p *hOraclePool) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	switch {
	case strings.Contains(sql, "FROM vigils") && strings.Contains(sql, "ORDER BY"):
		return &hOracleRows{rows: p.vigilListRows}, nil
	case strings.Contains(sql, "FROM decrees") && strings.Contains(sql, "ORDER BY"):
		return &hOracleRows{rows: p.decreeListRows}, nil
	}
	return nil, &hOracleErr{"hOraclePool: unexpected Query: " + sql}
}

type hOracleErr struct{ s string }

func (e *hOracleErr) Error() string { return e.s }

func hOracleItoa(n int64) string {
	if n == 0 {
		return "0"
	}
	return "1"
}

// hOracleRow — a staticRow for oracle columns (string/int/bool/time/[]string/[]byte +
// nullable pointers).
type hOracleRow struct {
	values []any
	err    error
}

func (r hOracleRow) Scan(dest ...any) error {
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
		case *[]string:
			if r.values[i] == nil {
				*dd = nil
			} else {
				*dd = r.values[i].([]string)
			}
		case *[]byte:
			if r.values[i] == nil {
				*dd = nil
			} else {
				*dd = r.values[i].([]byte)
			}
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

type hOracleRows struct {
	rows [][]any
	idx  int
}

func (r *hOracleRows) Next() bool { r.idx++; return r.idx <= len(r.rows) }
func (r *hOracleRows) Scan(dest ...any) error {
	return hOracleRow{values: r.rows[r.idx-1]}.Scan(dest...)
}
func (r *hOracleRows) Err() error                                   { return nil }
func (r *hOracleRows) Close()                                       {}
func (r *hOracleRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *hOracleRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *hOracleRows) Values() ([]any, error)                       { return nil, nil }
func (r *hOracleRows) RawValues() [][]byte                          { return nil }
func (r *hOracleRows) Conn() *pgx.Conn                              { return nil }

// humaOracleRouter assembles a chi router with ALL oracle routes via huma —
// the production wiring from router.go: RequirePermission(vigil/decree.<action>) on each
// group + (for write) huma audit middleware variant B + huma operation (full path).
// injectClaims replaces RequireJWT.
func humaOracleRouter(t *testing.T, enforcer apimiddleware.PermissionChecker, auditW audit.Writer, pool *hOraclePool) *chi.Mux {
	t.Helper()
	installHumaErrorOverride()
	where, err := oracle.NewWhereEvaluator()
	if err != nil {
		t.Fatalf("oracle.NewWhereEvaluator: %v", err)
	}
	svc, err := oracle.NewService(oracle.ServiceDeps{Pool: pool, Where: where})
	if err != nil {
		t.Fatalf("oracle.NewService: %v", err)
	}
	oracleH := handlers.NewOracleHandler(svc, nil)

	r := chi.NewRouter()
	injectClaims := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := apimiddleware.InjectClaimsForTest(req.Context(), &keeperjwt.Claims{Subject: "archon-alice"})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
	r.Route("/v1", func(r chi.Router) {
		r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "vigil", "create", apimiddleware.NoSelector)).Group(func(r chi.Router) {
			registerHumaVigilCreate(newHumaOracleAPI(r, auditW, audit.EventVigilCreated, nil), oracleH)
		})
		r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "vigil", "list", apimiddleware.NoSelector)).Group(func(r chi.Router) {
			registerHumaVigilList(newHumaCadenceAPI(r), oracleH)
		})
		r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "vigil", "list", apimiddleware.NoSelector)).Group(func(r chi.Router) {
			registerHumaVigilGet(newHumaCadenceAPI(r), oracleH)
		})
		r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "vigil", "delete", apimiddleware.NoSelector)).Group(func(r chi.Router) {
			registerHumaVigilDelete(newHumaOracleAPI(r, auditW, audit.EventVigilDeleted, nil), oracleH)
		})
		r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "decree", "create", apimiddleware.NoSelector)).Group(func(r chi.Router) {
			registerHumaDecreeCreate(newHumaOracleAPI(r, auditW, audit.EventDecreeCreated, nil), oracleH)
		})
		r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "decree", "list", apimiddleware.NoSelector)).Group(func(r chi.Router) {
			registerHumaDecreeList(newHumaCadenceAPI(r), oracleH)
		})
		r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "decree", "list", apimiddleware.NoSelector)).Group(func(r chi.Router) {
			registerHumaDecreeGet(newHumaCadenceAPI(r), oracleH)
		})
		r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "decree", "delete", apimiddleware.NoSelector)).Group(func(r chi.Router) {
			registerHumaDecreeDelete(newHumaOracleAPI(r, auditW, audit.EventDecreeDeleted, nil), oracleH)
		})
	})
	return r
}

// === VIGIL CREATE (WRITE+AUDIT vigil.created) ===

func TestHumaVigil_Create_GoldenWire(t *testing.T) {
	r := humaOracleRouter(t, strictAllowAll{}, nil, &hOraclePool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/vigils",
		strings.NewReader(`{"name":"web-conf","coven":["web"],"interval":"30s","check":"core.beacon.file_changed"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply не JSON-object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"check":"core.beacon.file_changed","coven":["web"],"created_at":"2026-06-13T10:00:00Z","created_by_aid":"archon-alice","enabled":true,"interval":"30s","name":"web-conf","params":{},"updated_at":"2026-06-13T10:00:00Z"}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-дрейф vigil.create:\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaVigil_Create_UnknownField_400(t *testing.T) {
	r := humaOracleRouter(t, strictAllowAll{}, nil, &hOraclePool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/vigils",
		strings.NewReader(`{"name":"x","coven":["web"],"interval":"30s","check":"core.beacon.file_changed","bogus":1}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

func TestHumaVigil_Create_MissingCheck_422(t *testing.T) {
	r := humaOracleRouter(t, strictAllowAll{}, nil, &hOraclePool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/vigils",
		strings.NewReader(`{"name":"x","coven":["web"],"interval":"30s"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (missing required check); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

func TestHumaVigil_Create_RBACDeny_403(t *testing.T) {
	r := humaOracleRouter(t, strictDenyAll{}, nil, &hOraclePool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/vigils",
		strings.NewReader(`{"name":"web-conf","coven":["web"],"interval":"30s","check":"core.beacon.file_changed"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaAudit_VigilCreate_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaOracleRouter(t, strictAllowAll{}, auditCap, &hOraclePool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/vigils",
		strings.NewReader(`{"name":"web-conf","coven":["web"],"interval":"30s","check":"core.beacon.file_changed"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventVigilCreated, map[string]any{
		"name": "web-conf", "check": "core.beacon.file_changed",
		"interval": "30s", "subject": "coven=web", "created_by_aid": "archon-alice",
	})
}

func TestHumaAudit_VigilCreate_NoAudit_OnRBACDeny(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaOracleRouter(t, strictDenyAll{}, auditCap, &hOraclePool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/vigils",
		strings.NewReader(`{"name":"web-conf","coven":["web"],"interval":"30s","check":"core.beacon.file_changed"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан on RBAC-deny vigil.create (%d withбытий)", len(auditCap.Events()))
	}
}

func TestHumaAudit_VigilCreate_NoAudit_OnValidationFail(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaOracleRouter(t, strictAllowAll{}, auditCap, &hOraclePool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/vigils",
		strings.NewReader(`{"name":"x","coven":["web"],"interval":"30s"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан on 422 vigil.create (%d withбытий)", len(auditCap.Events()))
	}
}

// === VIGIL LIST (READ with typed query, no audit) ===

func TestHumaVigil_List_GoldenWire(t *testing.T) {
	pool := &hOraclePool{vigilListRows: [][]any{
		{"web-conf", []string{"web"}, nil, "30s", "core.beacon.file_changed", []byte(`{}`), true, oracleAt, oracleAt, nil},
	}}
	r := humaOracleRouter(t, strictAllowAll{}, nil, pool)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/vigils", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply не JSON-object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"items":[{"check":"core.beacon.file_changed","coven":["web"],"created_at":"2026-06-13T10:00:00Z","enabled":true,"interval":"30s","name":"web-conf","params":{},"updated_at":"2026-06-13T10:00:00Z"}],"limit":50,"offset":0,"total":1}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-дрейф vigil.list:\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaVigil_List_BadOffset_400(t *testing.T) {
	r := humaOracleRouter(t, strictAllowAll{}, nil, &hOraclePool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/vigils?offset=-1", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (offset<0 → CheckPageBounds); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

func TestHumaVigil_List_BadLimit_400(t *testing.T) {
	r := humaOracleRouter(t, strictAllowAll{}, nil, &hOraclePool{})
	for _, c := range []string{"/v1/vigils?limit=0", "/v1/vigils?limit=1001"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, c, nil)
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (out-of-range limit → CheckPageBounds); body=%s", c, rec.Code, rec.Body.String())
			continue
		}
		assertHumaProblem(t, rec, problem.TypeMalformedRequest)
	}
}

func TestHumaVigil_List_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaOracleRouter(t, strictAllowAll{}, auditCap, &hOraclePool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/vigils", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("READ-роут vigil.list записал audit (%d withбытий)", len(auditCap.Events()))
	}
}

func TestHumaVigil_List_RBACDeny_403(t *testing.T) {
	r := humaOracleRouter(t, strictDenyAll{}, nil, &hOraclePool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/vigils", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// === VIGIL GET (READ with path, no audit) ===

func TestHumaVigil_Get_GoldenWire(t *testing.T) {
	r := humaOracleRouter(t, strictAllowAll{}, nil, &hOraclePool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/vigils/web-conf", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply не JSON-object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"check":"core.beacon.file_changed","coven":["web"],"created_at":"2026-06-13T10:00:00Z","enabled":true,"interval":"30s","name":"web-conf","params":{},"updated_at":"2026-06-13T10:00:00Z"}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-дрейф vigil.get:\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaVigil_Get_NotFound_404(t *testing.T) {
	r := humaOracleRouter(t, strictAllowAll{}, nil, &hOraclePool{vigilGetMissing: true})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/vigils/ghost", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeNotFound)
}

func TestHumaVigil_Get_BadName_422(t *testing.T) {
	r := humaOracleRouter(t, strictAllowAll{}, nil, &hOraclePool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/vigils/BAD_NAME", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (bad path-name); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

// === VIGIL DELETE (WRITE+AUDIT vigil.deleted) ===

func TestHumaVigil_Delete_204(t *testing.T) {
	r := humaOracleRouter(t, strictAllowAll{}, nil, &hOraclePool{vigilDeleteRows: 1})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/vigils/web-conf", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "" {
		t.Errorf("204-body vigil.delete toлжbut быть ПУСТЫМ, got %q", body)
	}
}

func TestHumaAudit_VigilDelete_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaOracleRouter(t, strictAllowAll{}, auditCap, &hOraclePool{vigilDeleteRows: 1})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/vigils/web-conf", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventVigilDeleted, map[string]any{"name": "web-conf"})
}

func TestHumaAudit_VigilDelete_NoAudit_OnNotFound(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaOracleRouter(t, strictAllowAll{}, auditCap, &hOraclePool{vigilDeleteRows: 0})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/vigils/ghost", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан on 404 vigil.delete (%d withбытий)", len(auditCap.Events()))
	}
}

// === DECREE CREATE (WRITE+AUDIT decree.created) ===

func TestHumaDecree_Create_GoldenWire(t *testing.T) {
	r := humaOracleRouter(t, strictAllowAll{}, nil, &hOraclePool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/decrees",
		strings.NewReader(`{"name":"on-conf","on_beacon":"web-conf","coven":["web"],"incarnation_name":"web","action_scenario":"reload"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply не JSON-object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"action_input":{},"action_scenario":"reload","cooldown":"0s","coven":["web"],"created_at":"2026-06-13T10:00:00Z","created_by_aid":"archon-alice","enabled":true,"incarnation_name":"web","name":"on-conf","on_beacon":"web-conf","updated_at":"2026-06-13T10:00:00Z"}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-дрейф decree.create:\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaDecree_Create_UnknownField_400(t *testing.T) {
	r := humaOracleRouter(t, strictAllowAll{}, nil, &hOraclePool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/decrees",
		strings.NewReader(`{"name":"on-conf","on_beacon":"web-conf","coven":["web"],"incarnation_name":"web","action_scenario":"reload","bogus":1}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

func TestHumaDecree_Create_MissingOnBeacon_422(t *testing.T) {
	r := humaOracleRouter(t, strictAllowAll{}, nil, &hOraclePool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/decrees",
		strings.NewReader(`{"name":"on-conf","coven":["web"],"incarnation_name":"web","action_scenario":"reload"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (missing required on_beacon); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

func TestHumaDecree_Create_RBACDeny_403(t *testing.T) {
	r := humaOracleRouter(t, strictDenyAll{}, nil, &hOraclePool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/decrees",
		strings.NewReader(`{"name":"on-conf","on_beacon":"web-conf","coven":["web"],"incarnation_name":"web","action_scenario":"reload"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaAudit_DecreeCreate_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaOracleRouter(t, strictAllowAll{}, auditCap, &hOraclePool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/decrees",
		strings.NewReader(`{"name":"on-conf","on_beacon":"web-conf","coven":["web"],"incarnation_name":"web","action_scenario":"reload"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventDecreeCreated, map[string]any{
		"name": "on-conf", "on_beacon": "web-conf", "incarnation": "web",
		"action_scenario": "reload", "subject": "coven=web", "created_by_aid": "archon-alice",
	})
}

// === DECREE LIST (READ with typed query, no audit) ===

func TestHumaDecree_List_GoldenWire(t *testing.T) {
	pool := &hOraclePool{decreeListRows: [][]any{
		{"on-conf", "web-conf", nil, []string{"web"}, nil, "web", "reload", []byte(`{}`), "0s", true, oracleAt, oracleAt, nil},
	}}
	r := humaOracleRouter(t, strictAllowAll{}, nil, pool)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/decrees", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply не JSON-object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"items":[{"action_input":{},"action_scenario":"reload","cooldown":"0s","coven":["web"],"created_at":"2026-06-13T10:00:00Z","enabled":true,"incarnation_name":"web","name":"on-conf","on_beacon":"web-conf","updated_at":"2026-06-13T10:00:00Z"}],"limit":50,"offset":0,"total":1}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-дрейф decree.list:\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaDecree_List_BadLimit_400(t *testing.T) {
	r := humaOracleRouter(t, strictAllowAll{}, nil, &hOraclePool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/decrees?limit=0", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

func TestHumaDecree_List_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaOracleRouter(t, strictAllowAll{}, auditCap, &hOraclePool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/decrees", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("READ-роут decree.list записал audit (%d withбытий)", len(auditCap.Events()))
	}
}

// === DECREE GET (READ with path, no audit) ===

func TestHumaDecree_Get_NotFound_404(t *testing.T) {
	r := humaOracleRouter(t, strictAllowAll{}, nil, &hOraclePool{decreeGetMissing: true})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/decrees/ghost", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeNotFound)
}

// === DECREE DELETE (WRITE+AUDIT decree.deleted) ===

func TestHumaDecree_Delete_204(t *testing.T) {
	r := humaOracleRouter(t, strictAllowAll{}, nil, &hOraclePool{decreeDeleteRows: 1})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/decrees/on-conf", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "" {
		t.Errorf("204-body decree.delete toлжbut быть ПУСТЫМ, got %q", body)
	}
}

func TestHumaAudit_DecreeDelete_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaOracleRouter(t, strictAllowAll{}, auditCap, &hOraclePool{decreeDeleteRows: 1})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/decrees/on-conf", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventDecreeDeleted, map[string]any{"name": "on-conf"})
}

func TestHumaAudit_DecreeDelete_NoAudit_OnBadName(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaOracleRouter(t, strictAllowAll{}, auditCap, &hOraclePool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/decrees/BAD_NAME", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (bad path-name); body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан on bad-name decree.delete (%d withбытий)", len(auditCap.Events()))
	}
}

// === OpenAPI fragment: ALL oracle operations from FULL-TYPED Go types ===

func TestHumaOracle_OpenAPIFragment_3_1(t *testing.T) {
	frag, err := HumaOracleSpecYAML()
	if err != nil {
		t.Fatalf("HumaOracleSpecYAML: %v", err)
	}
	if !strings.Contains(frag, "openapi: 3.1.0") {
		t.Errorf("huma-фрагмент не несёт `openapi: 3.1.0`:\n%s", frag)
	}
	for _, want := range []string{
		"createVigil", "listVigils", "getVigil", "deleteVigil",
		"createDecree", "listDecrees", "getDecree", "deleteDecree", "on_beacon",
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("OpenAPI-фрагмент не withдержит %q:\n%s", want, frag)
		}
	}
	if strings.Contains(frag, "octet-stream") {
		t.Errorf("OpenAPI-фрагмент несёт application/octet-stream:\n%s", frag)
	}
}
