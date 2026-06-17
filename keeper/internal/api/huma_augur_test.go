package api

// Guard-тесты ТИРАЖ-БАТЧА-2b разворота AUGUR-домена (omens + rites) ЦЕЛИКОМ на huma
// full-typed (ADR-054 §Pattern, эталоны role/operator). omen create/delete + rite
// create/delete — WRITE+AUDIT (вариант B, huma-audit-middleware; события
// omen.created/omen.revoked/rite.created/rite.revoked); omen list/get + rite list —
// read (БЕЗ audit). Доказывают инварианты кластера поверх chi:
//
//   - wire/golden: omen create 201 OmenView; omen list 200 envelope; omen get 200;
//     omen delete 204 пустое; rite create 201 RiteView (allow byte-exact); rite list
//     200 items[]; rite delete 204 (byte-exact);
//   - unknown-field → 400; missing-required → 422; bad source_type enum → 422; bad
//     pagination → 400; missing omen-query → 422; RBAC-deny → 403;
//   - S6-GUARD на КАЖДЫЙ write-роут: полная huma-навеска пишет audit-event с НЕПУСТЫМ
//     payload + ПРАВИЛЬНЫМ event-type на 2xx и НЕ пишет на 4xx/403.

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
	"github.com/souls-guild/soul-stack/keeper/internal/augur"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// augurAt — фиксированный created_at, который все augur-success-пути отдают
// (детерминированный golden wire).
var augurAt = time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)

// hAugurPool — узкий мок [augur.ServicePool] для huma-теста (Exec/QueryRow/Query).
// Классифицирует SQL по подстроке и отдаёт детерминированный success-исход;
// error-классификацию валидируют handlers/augur_test.go.
type hAugurPool struct {
	omenDeleteRows int64
	riteDeleteRows int64
	omenGetMissing bool // GET/INSERT-rite-резолв omens WHERE name → ErrNoRows (404)
	omenListRows   [][]any
	riteListRows   [][]any
}

func (p *hAugurPool) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	switch {
	case strings.Contains(sql, "DELETE FROM omens"):
		return pgconn.NewCommandTag("DELETE " + hAugurItoa(p.omenDeleteRows)), nil
	case strings.Contains(sql, "DELETE FROM rites"):
		return pgconn.NewCommandTag("DELETE " + hAugurItoa(p.riteDeleteRows)), nil
	}
	return pgconn.CommandTag{}, &hAugurErr{"hAugurPool: unexpected Exec SQL: " + sql}
}

func (p *hAugurPool) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "INSERT INTO omens"):
		return hAugurRow{values: []any{augurAt}} // RETURNING created_at
	case strings.Contains(sql, "INSERT INTO rites"):
		return hAugurRow{values: []any{int64(42), augurAt}} // RETURNING id, created_at
	case strings.Contains(sql, "FROM omens") && strings.Contains(sql, "WHERE name"):
		if p.omenGetMissing {
			return hAugurRow{err: pgx.ErrNoRows}
		}
		// scanOmen: name, source_type, endpoint, auth_ref, created_by_aid, created_at.
		return hAugurRow{values: []any{"vault-prod", "vault", "https://vault:8200", "vault:secret/keeper/ar", nil, augurAt}}
	case strings.Contains(sql, "COUNT(*) FROM omens"):
		return hAugurRow{values: []any{len(p.omenListRows)}}
	}
	return hAugurRow{err: &hAugurErr{"hAugurPool: unexpected QueryRow SQL: " + sql}}
}

func (p *hAugurPool) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	switch {
	case strings.Contains(sql, "FROM omens") && strings.Contains(sql, "ORDER BY"):
		return &hAugurRows{rows: p.omenListRows}, nil
	case strings.Contains(sql, "FROM rites") && strings.Contains(sql, "WHERE omen"):
		return &hAugurRows{rows: p.riteListRows}, nil
	}
	return nil, &hAugurErr{"hAugurPool: unexpected Query SQL: " + sql}
}

type hAugurErr struct{ s string }

func (e *hAugurErr) Error() string { return e.s }

func hAugurItoa(n int64) string {
	if n == 0 {
		return "0"
	}
	return "1"
}

// hAugurRow — staticRow для augur-колонок (string/time/int/int64/bool/[]byte +
// nullable-указатели).
type hAugurRow struct {
	values []any
	err    error
}

func (r hAugurRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, d := range dest {
		switch dd := d.(type) {
		case *string:
			*dd = r.values[i].(string)
		case *int:
			*dd = r.values[i].(int)
		case *int64:
			*dd = r.values[i].(int64)
		case *bool:
			*dd = r.values[i].(bool)
		case *time.Time:
			*dd = r.values[i].(time.Time)
		case *[]byte:
			*dd = r.values[i].([]byte)
		case **string:
			if r.values[i] == nil {
				*dd = nil
			} else {
				s := r.values[i].(string)
				*dd = &s
			}
		case **int:
			if r.values[i] == nil {
				*dd = nil
			} else {
				n := r.values[i].(int)
				*dd = &n
			}
		}
	}
	return nil
}

type hAugurRows struct {
	rows [][]any
	idx  int
}

func (r *hAugurRows) Next() bool { r.idx++; return r.idx <= len(r.rows) }
func (r *hAugurRows) Scan(dest ...any) error {
	return hAugurRow{values: r.rows[r.idx-1]}.Scan(dest...)
}
func (r *hAugurRows) Err() error                                   { return nil }
func (r *hAugurRows) Close()                                       {}
func (r *hAugurRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *hAugurRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *hAugurRows) Values() ([]any, error)                       { return nil, nil }
func (r *hAugurRows) RawValues() [][]byte                          { return nil }
func (r *hAugurRows) Conn() *pgx.Conn                              { return nil }

// humaAugurRouter собирает chi-роутер со ВСЕМИ augur-роутами через huma —
// продакшен-навеска из router.go: RequirePermission(omen/rite.<action>) на каждой
// группе + (для write) huma-audit-middleware вариант B + huma-операция. injectClaims
// заменяет RequireJWT.
func humaAugurRouter(t *testing.T, enforcer apimiddleware.PermissionChecker, auditW audit.Writer, pool *hAugurPool) *chi.Mux {
	t.Helper()
	installHumaErrorOverride()
	svc, err := augur.NewService(augur.ServiceDeps{Pool: pool})
	if err != nil {
		t.Fatalf("augur.NewService: %v", err)
	}
	augurH := handlers.NewAugurHandler(svc, nil)

	r := chi.NewRouter()
	injectClaims := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := apimiddleware.InjectClaimsForTest(req.Context(), &keeperjwt.Claims{Subject: "archon-alice"})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
	r.Route("/v1", func(r chi.Router) {
		r.Route("/augur", func(r chi.Router) {
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "omen", "create", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaOmenCreate(newHumaAugurAPI(r, auditW, audit.EventOmenCreated, nil), augurH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "omen", "list", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaOmenList(newHumaCadenceAPI(r), augurH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "omen", "list", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaOmenGet(newHumaCadenceAPI(r), augurH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "omen", "delete", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaOmenDelete(newHumaAugurAPI(r, auditW, audit.EventOmenRevoked, nil), augurH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "rite", "create", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaRiteCreate(newHumaAugurAPI(r, auditW, audit.EventRiteCreated, nil), augurH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "rite", "list", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaRiteList(newHumaCadenceAPI(r), augurH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "rite", "delete", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaRiteDelete(newHumaAugurAPI(r, auditW, audit.EventRiteRevoked, nil), augurH)
			})
		})
	})
	return r
}

// === OMEN CREATE (WRITE+AUDIT omen.created) ===

func TestHumaOmen_Create_GoldenWire(t *testing.T) {
	r := humaAugurRouter(t, strictAllowAll{}, nil, &hAugurPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/augur/omens",
		strings.NewReader(`{"name":"vault-prod","source_type":"vault","endpoint":"https://vault:8200","auth_ref":"vault:secret/keeper/ar"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply не JSON-object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"auth_ref":"vault:secret/keeper/ar","created_at":"2026-06-13T10:00:00Z","created_by_aid":"archon-alice","endpoint":"https://vault:8200","name":"vault-prod","source_type":"vault"}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-дрейф omen.create:\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaOmen_Create_UnknownField_400(t *testing.T) {
	r := humaAugurRouter(t, strictAllowAll{}, nil, &hAugurPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/augur/omens",
		strings.NewReader(`{"name":"x","source_type":"vault","endpoint":"e","auth_ref":"vault:s/p","bogus":1}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

func TestHumaOmen_Create_MissingName_422(t *testing.T) {
	r := humaAugurRouter(t, strictAllowAll{}, nil, &hAugurPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/augur/omens",
		strings.NewReader(`{"source_type":"vault","endpoint":"e","auth_ref":"vault:s/p"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

func TestHumaOmen_Create_BadSourceType_422(t *testing.T) {
	r := humaAugurRouter(t, strictAllowAll{}, nil, &hAugurPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/augur/omens",
		strings.NewReader(`{"name":"x","source_type":"redis","endpoint":"e","auth_ref":"vault:s/p"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (bad source_type enum); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

func TestHumaOmen_Create_RBACDeny_403(t *testing.T) {
	r := humaAugurRouter(t, strictDenyAll{}, nil, &hAugurPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/augur/omens",
		strings.NewReader(`{"name":"vault-prod","source_type":"vault","endpoint":"e","auth_ref":"vault:s/p"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaAudit_OmenCreate_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaAugurRouter(t, strictAllowAll{}, auditCap, &hAugurPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/augur/omens",
		strings.NewReader(`{"name":"vault-prod","source_type":"vault","endpoint":"https://vault:8200","auth_ref":"vault:secret/keeper/ar"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventOmenCreated, map[string]any{
		"name": "vault-prod", "source_type": "vault",
		"endpoint": "https://vault:8200", "auth_ref": "vault:secret/keeper/ar",
		"created_by_aid": "archon-alice",
	})
}

func TestHumaAudit_OmenCreate_NoAudit_OnRBACDeny(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaAugurRouter(t, strictDenyAll{}, auditCap, &hAugurPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/augur/omens",
		strings.NewReader(`{"name":"vault-prod","source_type":"vault","endpoint":"e","auth_ref":"vault:s/p"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан на RBAC-deny omen.create (%d событий)", len(auditCap.Events()))
	}
}

func TestHumaAudit_OmenCreate_NoAudit_OnValidationFail(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaAugurRouter(t, strictAllowAll{}, auditCap, &hAugurPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/augur/omens",
		strings.NewReader(`{"source_type":"vault","endpoint":"e","auth_ref":"vault:s/p"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан на 422 omen.create (%d событий)", len(auditCap.Events()))
	}
}

// === OMEN LIST (READ-with-typed-query, БЕЗ audit) ===

func TestHumaOmen_List_GoldenWire(t *testing.T) {
	pool := &hAugurPool{omenListRows: [][]any{
		{"vault-prod", "vault", "https://vault:8200", "vault:secret/keeper/ar", nil, augurAt},
	}}
	r := humaAugurRouter(t, strictAllowAll{}, nil, pool)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/augur/omens", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply не JSON-object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"items":[{"auth_ref":"vault:secret/keeper/ar","created_at":"2026-06-13T10:00:00Z","endpoint":"https://vault:8200","name":"vault-prod","source_type":"vault"}],"limit":50,"offset":0,"total":1}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-дрейф omen.list:\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaOmen_List_GoldenEmpty(t *testing.T) {
	r := humaAugurRouter(t, strictAllowAll{}, nil, &hAugurPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/augur/omens", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	const golden = `{"items":[],"limit":50,"offset":0,"total":0}`
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	out, _ := json.Marshal(m)
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-дрейф omen.list (empty): got=%q want=%q", got, golden)
	}
}

func TestHumaOmen_List_BadOffset_400(t *testing.T) {
	r := humaAugurRouter(t, strictAllowAll{}, nil, &hAugurPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/augur/omens?offset=-1", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (offset<0 → CheckPageBounds 400, parity ParsePage); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

func TestHumaOmen_List_BadLimit_400(t *testing.T) {
	r := humaAugurRouter(t, strictAllowAll{}, nil, &hAugurPool{})
	for _, c := range []string{"/v1/augur/omens?limit=0", "/v1/augur/omens?limit=1001"} {
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

func TestHumaOmen_List_BadInt_400(t *testing.T) {
	r := humaAugurRouter(t, strictAllowAll{}, nil, &hAugurPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/augur/omens?limit=notanint", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (bad int → parseInto); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

func TestHumaOmen_List_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaAugurRouter(t, strictAllowAll{}, auditCap, &hAugurPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/augur/omens", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("READ-роут omen.list записал audit (%d событий)", len(auditCap.Events()))
	}
}

func TestHumaOmen_List_RBACDeny_403(t *testing.T) {
	r := humaAugurRouter(t, strictDenyAll{}, nil, &hAugurPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/augur/omens", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// === OMEN GET (READ-with-path, БЕЗ audit) ===

func TestHumaOmen_Get_GoldenWire(t *testing.T) {
	r := humaAugurRouter(t, strictAllowAll{}, nil, &hAugurPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/augur/omens/vault-prod", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply не JSON-object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"auth_ref":"vault:secret/keeper/ar","created_at":"2026-06-13T10:00:00Z","endpoint":"https://vault:8200","name":"vault-prod","source_type":"vault"}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-дрейф omen.get:\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaOmen_Get_NotFound_404(t *testing.T) {
	r := humaAugurRouter(t, strictAllowAll{}, nil, &hAugurPool{omenGetMissing: true})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/augur/omens/ghost", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeNotFound)
}

func TestHumaOmen_Get_BadName_422(t *testing.T) {
	r := humaAugurRouter(t, strictAllowAll{}, nil, &hAugurPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/augur/omens/BAD_NAME", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (bad path-name); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

// === OMEN DELETE (WRITE+AUDIT omen.revoked) ===

func TestHumaOmen_Delete_204(t *testing.T) {
	r := humaAugurRouter(t, strictAllowAll{}, nil, &hAugurPool{omenDeleteRows: 1})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/augur/omens/vault-prod", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "" {
		t.Errorf("204-тело omen.delete должно быть ПУСТЫМ, got %q", body)
	}
}

func TestHumaAudit_OmenDelete_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaAugurRouter(t, strictAllowAll{}, auditCap, &hAugurPool{omenDeleteRows: 1})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/augur/omens/vault-prod", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventOmenRevoked, map[string]any{"name": "vault-prod"})
}

func TestHumaAudit_OmenDelete_NoAudit_OnNotFound(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaAugurRouter(t, strictAllowAll{}, auditCap, &hAugurPool{omenDeleteRows: 0})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/augur/omens/ghost", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан на 404 omen.delete (%d событий)", len(auditCap.Events()))
	}
}

// === RITE CREATE (WRITE+AUDIT rite.created) ===

func TestHumaRite_Create_GoldenWire(t *testing.T) {
	r := humaAugurRouter(t, strictAllowAll{}, nil, &hAugurPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/augur/rites",
		strings.NewReader(`{"omen":"vault-prod","coven":"prod","allow":{"paths":["secret/data/app"]}}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply не JSON-object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"allow":{"paths":["secret/data/app"]},"coven":"prod","created_at":"2026-06-13T10:00:00Z","created_by_aid":"archon-alice","delegate":false,"id":42,"omen":"vault-prod"}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-дрейф rite.create:\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaRite_Create_UnknownField_400(t *testing.T) {
	r := humaAugurRouter(t, strictAllowAll{}, nil, &hAugurPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/augur/rites",
		strings.NewReader(`{"omen":"vault-prod","coven":"prod","allow":{"a":1},"bogus":1}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

func TestHumaRite_Create_MissingOmen_422(t *testing.T) {
	r := humaAugurRouter(t, strictAllowAll{}, nil, &hAugurPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/augur/rites",
		strings.NewReader(`{"coven":"prod","allow":{"a":1}}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (missing required omen); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

func TestHumaRite_Create_RBACDeny_403(t *testing.T) {
	r := humaAugurRouter(t, strictDenyAll{}, nil, &hAugurPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/augur/rites",
		strings.NewReader(`{"omen":"vault-prod","coven":"prod","allow":{"a":1}}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaAudit_RiteCreate_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaAugurRouter(t, strictAllowAll{}, auditCap, &hAugurPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/augur/rites",
		strings.NewReader(`{"omen":"vault-prod","coven":"prod","allow":{"paths":["secret/data/app"]}}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventRiteCreated, map[string]any{
		"id": int64(42), "omen": "vault-prod", "subject": "coven=prod",
		"delegate": false, "created_by_aid": "archon-alice",
	})
}

// === RITE LIST (READ-with-typed-query, обязательный omen=, БЕЗ audit) ===

func TestHumaRite_List_GoldenWire(t *testing.T) {
	pool := &hAugurPool{riteListRows: [][]any{
		// scanRite: id, omen, coven, sid, allow, delegate, token_ttl, token_num_uses, created_by_aid, created_at.
		{int64(42), "vault-prod", "prod", nil, []byte(`{"paths":["secret/data/app"]}`), false, nil, nil, nil, augurAt},
	}}
	r := humaAugurRouter(t, strictAllowAll{}, nil, pool)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/augur/rites?omen=vault-prod", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply не JSON-object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"items":[{"allow":{"paths":["secret/data/app"]},"coven":"prod","created_at":"2026-06-13T10:00:00Z","delegate":false,"id":42,"omen":"vault-prod"}]}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-дрейф rite.list:\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaRite_List_MissingOmen_422(t *testing.T) {
	r := humaAugurRouter(t, strictAllowAll{}, nil, &hAugurPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/augur/rites", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (omen-query обязателен); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

func TestHumaRite_List_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaAugurRouter(t, strictAllowAll{}, auditCap, &hAugurPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/augur/rites?omen=vault-prod", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("READ-роут rite.list записал audit (%d событий)", len(auditCap.Events()))
	}
}

// === RITE DELETE (WRITE+AUDIT rite.revoked) ===

func TestHumaRite_Delete_204(t *testing.T) {
	r := humaAugurRouter(t, strictAllowAll{}, nil, &hAugurPool{riteDeleteRows: 1})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/augur/rites/42", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "" {
		t.Errorf("204-тело rite.delete должно быть ПУСТЫМ, got %q", body)
	}
}

func TestHumaAudit_RiteDelete_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaAugurRouter(t, strictAllowAll{}, auditCap, &hAugurPool{riteDeleteRows: 1})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/augur/rites/42", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventRiteRevoked, map[string]any{"id": int64(42)})
}

func TestHumaAudit_RiteDelete_NoAudit_OnBadID(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaAugurRouter(t, strictAllowAll{}, auditCap, &hAugurPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/augur/rites/notanint", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (id не число); body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан на bad-id rite.delete (%d событий)", len(auditCap.Events()))
	}
}

// === OpenAPI-фрагмент: ВСЕ augur-операции из FULL-TYPED Go-типов ===

func TestHumaAugur_OpenAPIFragment_3_1(t *testing.T) {
	frag, err := HumaAugurSpecYAML()
	if err != nil {
		t.Fatalf("HumaAugurSpecYAML: %v", err)
	}
	if !strings.Contains(frag, "openapi: 3.1.0") {
		t.Errorf("huma-фрагмент не несёт `openapi: 3.1.0`:\n%s", frag)
	}
	for _, want := range []string{
		"createOmen", "listOmens", "getOmen", "deleteOmen",
		"createRite", "listRites", "deleteRite", "source_type",
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("OpenAPI-фрагмент не содержит %q:\n%s", want, frag)
		}
	}
	if strings.Contains(frag, "octet-stream") {
		t.Errorf("OpenAPI-фрагмент несёт application/octet-stream:\n%s", frag)
	}
}
