package api

// Guard tests of ROLLOUT BATCH 2b that unfolds the PUSH-PROVIDER domain ENTIRELY onto huma
// full-typed (ADR-054 §Pattern, role/operator references). create/update/delete — WRITE+AUDIT
// (variant B, huma audit middleware; events push-provider.created/.updated/.deleted);
// list/get — read (no audit). They prove the cluster invariants over chi:
//
//   - wire/golden: create 201 PushProvider; list 200 envelope; get 200; update 200
//     (replace params); delete 204 empty (byte-exact);
//   - unknown-field → 400; missing-required → 422; bad pagination → 400; RBAC-deny → 403;
//   - S6-GUARD on EVERY write route (create/update/delete): the full huma wiring writes an
//     audit event with a NON-EMPTY payload + the CORRECT event-type on 2xx and does NOT write on 4xx/403.

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
	"github.com/souls-guild/soul-stack/keeper/internal/pushprovider"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// ppAt — a fixed created/updated_at for a deterministic golden wire.
var ppAt = time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)

// hPushProviderPool — a mock [pushprovider.ExecQueryRower] for the huma test. A minimal PG
// imitation: name → entry in a map with the fixed time ppAt (deterministic golden). Error
// classification is validated by handlers/pushprovider_test.go.
type hPushProviderPool struct {
	entries map[string]*pushprovider.PushProvider
}

func newHPushProviderPool() *hPushProviderPool {
	return &hPushProviderPool{entries: make(map[string]*pushprovider.PushProvider)}
}

func (f *hPushProviderPool) seed(name string, params map[string]any) *hPushProviderPool {
	f.entries[name] = &pushprovider.PushProvider{
		Name:         name,
		Params:       params,
		CreatedAt:    ppAt,
		UpdatedAt:    ppAt,
		CreatedByAID: "archon-alice",
	}
	return f
}

func (f *hPushProviderPool) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	switch {
	case strings.Contains(sql, "UPDATE push_providers"):
		name := args[0].(string)
		p, ok := f.entries[name]
		if !ok {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		}
		var params map[string]any
		_ = json.Unmarshal(args[1].([]byte), &params)
		p.Params = params
		p.UpdatedAt = ppAt
		if args[2] != nil {
			s := args[2].(string)
			p.UpdatedByAID = &s
		}
		return pgconn.NewCommandTag("UPDATE 1"), nil
	case strings.Contains(sql, "DELETE FROM push_providers"):
		name := args[0].(string)
		if _, ok := f.entries[name]; !ok {
			return pgconn.NewCommandTag("DELETE 0"), nil
		}
		delete(f.entries, name)
		return pgconn.NewCommandTag("DELETE 1"), nil
	}
	return pgconn.NewCommandTag(""), nil
}

func (f *hPushProviderPool) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "INSERT INTO push_providers") {
		name := args[0].(string)
		if _, exists := f.entries[name]; exists {
			return hErrRowPP{err: &pgconn.PgError{Code: "23505", ConstraintName: "push_providers_pkey"}}
		}
		var params map[string]any
		_ = json.Unmarshal(args[1].([]byte), &params)
		f.entries[name] = &pushprovider.PushProvider{
			Name:         name,
			Params:       params,
			CreatedAt:    ppAt,
			UpdatedAt:    ppAt,
			CreatedByAID: args[2].(string),
		}
		return hScanRowPP{values: []any{ppAt, ppAt}} // RETURNING created_at, updated_at
	}
	if strings.Contains(sql, "SELECT") && strings.Contains(sql, "FROM push_providers") && strings.Contains(sql, "WHERE name = $1") {
		name := args[0].(string)
		p, ok := f.entries[name]
		if !ok {
			return hErrRowPP{err: pgx.ErrNoRows}
		}
		paramsBytes, _ := json.Marshal(p.Params)
		return hScanRowPP{values: []any{p.Name, paramsBytes, p.CreatedAt, p.UpdatedAt, p.CreatedByAID, p.UpdatedByAID}}
	}
	if strings.Contains(sql, "SELECT COUNT(*)") {
		return hCountRowPP{n: len(f.entries)}
	}
	return hErrRowPP{err: pgx.ErrNoRows}
}

func (f *hPushProviderPool) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	rows := make([][]any, 0, len(f.entries))
	for _, p := range f.entries {
		paramsBytes, _ := json.Marshal(p.Params)
		rows = append(rows, []any{p.Name, paramsBytes, p.CreatedAt, p.UpdatedAt, p.CreatedByAID, p.UpdatedByAID})
	}
	return &hRowsPP{rows: rows}, nil
}

type hErrRowPP struct{ err error }

func (r hErrRowPP) Scan(_ ...any) error { return r.err }

type hScanRowPP struct{ values []any }

func (r hScanRowPP) Scan(dest ...any) error {
	for i, d := range dest {
		switch dst := d.(type) {
		case *string:
			*dst = r.values[i].(string)
		case *time.Time:
			*dst = r.values[i].(time.Time)
		case **string:
			if r.values[i] == nil {
				*dst = nil
				continue
			}
			if p, ok := r.values[i].(*string); ok {
				*dst = p
				continue
			}
			s := r.values[i].(string)
			*dst = &s
		case *[]byte:
			*dst = r.values[i].([]byte)
		}
	}
	return nil
}

type hCountRowPP struct{ n int }

func (r hCountRowPP) Scan(dest ...any) error {
	if p, ok := dest[0].(*int); ok {
		*p = r.n
	}
	return nil
}

type hRowsPP struct {
	rows [][]any
	idx  int
}

func (r *hRowsPP) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}

func (r *hRowsPP) Scan(dest ...any) error {
	row := r.rows[r.idx-1]
	for i, d := range dest {
		switch dst := d.(type) {
		case *string:
			*dst = row[i].(string)
		case *time.Time:
			*dst = row[i].(time.Time)
		case **string:
			if row[i] == nil {
				*dst = nil
				continue
			}
			if p, ok := row[i].(*string); ok {
				*dst = p
				continue
			}
			s := row[i].(string)
			*dst = &s
		case *[]byte:
			*dst = row[i].([]byte)
		}
	}
	return nil
}

func (r *hRowsPP) Err() error                                   { return nil }
func (r *hRowsPP) Close()                                       {}
func (r *hRowsPP) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *hRowsPP) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *hRowsPP) Values() ([]any, error)                       { return nil, nil }
func (r *hRowsPP) RawValues() [][]byte                          { return nil }
func (r *hRowsPP) Conn() *pgx.Conn                              { return nil }

// humaPushProviderRouter assembles a chi router with ALL push-provider routes via huma — the
// production wiring from router.go: RequirePermission(push-provider.<action>) on each group +
// (for write) huma audit middleware variant B + the huma operation. injectClaims replaces
// RequireJWT.
func humaPushProviderRouter(t *testing.T, enforcer apimiddleware.PermissionChecker, auditW audit.Writer, pool *hPushProviderPool) *chi.Mux {
	t.Helper()
	installHumaErrorOverride()
	svc, err := pushprovider.NewService(pushprovider.ServiceDeps{Pool: pool})
	if err != nil {
		t.Fatalf("pushprovider.NewService: %v", err)
	}
	pushProviderH := handlers.NewPushProviderHandler(svc, nil)

	r := chi.NewRouter()
	injectClaims := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := apimiddleware.InjectClaimsForTest(req.Context(), &keeperjwt.Claims{Subject: "archon-alice"})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
	r.Route("/v1", func(r chi.Router) {
		r.Route("/push-providers", func(r chi.Router) {
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "push-provider", "create", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaPushProviderCreate(newHumaPushProviderAPI(r, auditW, audit.EventPushProviderCreated, nil), pushProviderH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "push-provider", "list", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaPushProviderList(newHumaCadenceAPI(r), pushProviderH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "push-provider", "read", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaPushProviderGet(newHumaCadenceAPI(r), pushProviderH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "push-provider", "update", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaPushProviderUpdate(newHumaPushProviderAPI(r, auditW, audit.EventPushProviderUpdated, nil), pushProviderH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "push-provider", "delete", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaPushProviderDelete(newHumaPushProviderAPI(r, auditW, audit.EventPushProviderDeleted, nil), pushProviderH)
			})
		})
	})
	return r
}

// === CREATE (WRITE+AUDIT push-provider.created) ===

func TestHumaPushProvider_Create_GoldenWire(t *testing.T) {
	r := humaPushProviderRouter(t, strictAllowAll{}, nil, newHPushProviderPool())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/push-providers",
		strings.NewReader(`{"name":"vault-bastion","params":{"vault_addr":"https://vault.example.com"}}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply is not a JSON object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"created_at":"2026-06-13T10:00:00Z","created_by_aid":"archon-alice","name":"vault-bastion","params":{"vault_addr":"https://vault.example.com"},"updated_at":"2026-06-13T10:00:00Z"}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire drift push-provider.create:\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaPushProvider_Create_UnknownField_400(t *testing.T) {
	r := humaPushProviderRouter(t, strictAllowAll{}, nil, newHPushProviderPool())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/push-providers",
		strings.NewReader(`{"name":"x","params":{},"bogus":1}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

func TestHumaPushProvider_Create_MissingName_422(t *testing.T) {
	r := humaPushProviderRouter(t, strictAllowAll{}, nil, newHPushProviderPool())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/push-providers", strings.NewReader(`{"params":{}}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (missing required name); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

func TestHumaPushProvider_Create_RBACDeny_403(t *testing.T) {
	r := humaPushProviderRouter(t, strictDenyAll{}, nil, newHPushProviderPool())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/push-providers",
		strings.NewReader(`{"name":"vault-bastion","params":{}}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaAudit_PushProviderCreate_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaPushProviderRouter(t, strictAllowAll{}, auditCap, newHPushProviderPool())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/push-providers",
		strings.NewReader(`{"name":"vault-bastion","params":{"vault_addr":"https://vault.example.com"}}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventPushProviderCreated, map[string]any{"name": "vault-bastion"})
}

func TestHumaAudit_PushProviderCreate_NoAudit_OnRBACDeny(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaPushProviderRouter(t, strictDenyAll{}, auditCap, newHPushProviderPool())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/push-providers",
		strings.NewReader(`{"name":"vault-bastion","params":{}}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit written on RBAC-deny push-provider.create (%d events)", len(auditCap.Events()))
	}
}

func TestHumaAudit_PushProviderCreate_NoAudit_OnValidationFail(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaPushProviderRouter(t, strictAllowAll{}, auditCap, newHPushProviderPool())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/push-providers", strings.NewReader(`{"params":{}}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit written on 422 push-provider.create (%d events)", len(auditCap.Events()))
	}
}

// === LIST (READ with typed query, no audit) ===

func TestHumaPushProvider_List_GoldenWire(t *testing.T) {
	pool := newHPushProviderPool().seed("vault-bastion", map[string]any{"vault_addr": "https://vault.example.com"})
	r := humaPushProviderRouter(t, strictAllowAll{}, nil, pool)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/push-providers", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply is not a JSON object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"items":[{"created_at":"2026-06-13T10:00:00Z","created_by_aid":"archon-alice","name":"vault-bastion","params":{"vault_addr":"https://vault.example.com"},"updated_at":"2026-06-13T10:00:00Z"}],"limit":50,"offset":0,"total":1}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire drift push-provider.list:\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaPushProvider_List_GoldenEmpty(t *testing.T) {
	r := humaPushProviderRouter(t, strictAllowAll{}, nil, newHPushProviderPool())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/push-providers", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	const golden = `{"items":[],"limit":50,"offset":0,"total":0}`
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	out, _ := json.Marshal(m)
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire drift push-provider.list (empty): got=%q want=%q", got, golden)
	}
}

func TestHumaPushProvider_List_BadOffset_400(t *testing.T) {
	r := humaPushProviderRouter(t, strictAllowAll{}, nil, newHPushProviderPool())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/push-providers?offset=-1", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (offset<0 → CheckPageBounds); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

func TestHumaPushProvider_List_BadLimit_400(t *testing.T) {
	r := humaPushProviderRouter(t, strictAllowAll{}, nil, newHPushProviderPool())
	for _, c := range []string{"/v1/push-providers?limit=0", "/v1/push-providers?limit=1001"} {
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

func TestHumaPushProvider_List_BadInt_400(t *testing.T) {
	r := humaPushProviderRouter(t, strictAllowAll{}, nil, newHPushProviderPool())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/push-providers?offset=notanint", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (bad int → parseInto); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

func TestHumaPushProvider_List_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaPushProviderRouter(t, strictAllowAll{}, auditCap, newHPushProviderPool())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/push-providers", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("READ route push-provider.list wrote audit (%d events)", len(auditCap.Events()))
	}
}

func TestHumaPushProvider_List_RBACDeny_403(t *testing.T) {
	r := humaPushProviderRouter(t, strictDenyAll{}, nil, newHPushProviderPool())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/push-providers", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// === GET (READ with path, no audit) ===

func TestHumaPushProvider_Get_GoldenWire(t *testing.T) {
	pool := newHPushProviderPool().seed("vault-bastion", map[string]any{"vault_addr": "https://vault.example.com"})
	r := humaPushProviderRouter(t, strictAllowAll{}, nil, pool)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/push-providers/vault-bastion", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply is not a JSON object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"created_at":"2026-06-13T10:00:00Z","created_by_aid":"archon-alice","name":"vault-bastion","params":{"vault_addr":"https://vault.example.com"},"updated_at":"2026-06-13T10:00:00Z"}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire drift push-provider.get:\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaPushProvider_Get_NotFound_404(t *testing.T) {
	r := humaPushProviderRouter(t, strictAllowAll{}, nil, newHPushProviderPool())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/push-providers/ghost", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeNotFound)
}

func TestHumaPushProvider_Get_BadName_422(t *testing.T) {
	r := humaPushProviderRouter(t, strictAllowAll{}, nil, newHPushProviderPool())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/push-providers/BAD_NAME", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (bad path-name); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

// === UPDATE (WRITE+AUDIT push-provider.updated, PUT replace) ===

func TestHumaPushProvider_Update_GoldenWire(t *testing.T) {
	pool := newHPushProviderPool().seed("vault-bastion", map[string]any{"old": "x"})
	r := humaPushProviderRouter(t, strictAllowAll{}, nil, pool)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/push-providers/vault-bastion",
		strings.NewReader(`{"params":{"vault_addr":"https://new.example.com"}}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply is not a JSON object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"created_at":"2026-06-13T10:00:00Z","created_by_aid":"archon-alice","name":"vault-bastion","params":{"vault_addr":"https://new.example.com"},"updated_at":"2026-06-13T10:00:00Z","updated_by_aid":"archon-alice"}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire drift push-provider.update:\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaPushProvider_Update_UnknownField_400(t *testing.T) {
	pool := newHPushProviderPool().seed("vault-bastion", map[string]any{"old": "x"})
	r := humaPushProviderRouter(t, strictAllowAll{}, nil, pool)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/push-providers/vault-bastion",
		strings.NewReader(`{"params":{},"bogus":1}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

func TestHumaPushProvider_Update_NotFound_404(t *testing.T) {
	r := humaPushProviderRouter(t, strictAllowAll{}, nil, newHPushProviderPool())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/push-providers/ghost",
		strings.NewReader(`{"params":{}}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeNotFound)
}

func TestHumaAudit_PushProviderUpdate_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	pool := newHPushProviderPool().seed("vault-bastion", map[string]any{"old": "x"})
	r := humaPushProviderRouter(t, strictAllowAll{}, auditCap, pool)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/push-providers/vault-bastion",
		strings.NewReader(`{"params":{"vault_addr":"https://new.example.com"}}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventPushProviderUpdated, map[string]any{"name": "vault-bastion"})
}

func TestHumaAudit_PushProviderUpdate_NoAudit_OnNotFound(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaPushProviderRouter(t, strictAllowAll{}, auditCap, newHPushProviderPool())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/push-providers/ghost",
		strings.NewReader(`{"params":{}}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit written on 404 push-provider.update (%d events)", len(auditCap.Events()))
	}
}

// === DELETE (WRITE+AUDIT push-provider.deleted) ===

func TestHumaPushProvider_Delete_204(t *testing.T) {
	pool := newHPushProviderPool().seed("vault-bastion", map[string]any{"old": "x"})
	r := humaPushProviderRouter(t, strictAllowAll{}, nil, pool)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/push-providers/vault-bastion", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "" {
		t.Errorf("204-body push-provider.delete must be EMPTY, got %q", body)
	}
}

func TestHumaAudit_PushProviderDelete_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	pool := newHPushProviderPool().seed("vault-bastion", map[string]any{"old": "x"})
	r := humaPushProviderRouter(t, strictAllowAll{}, auditCap, pool)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/push-providers/vault-bastion", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventPushProviderDeleted, map[string]any{"name": "vault-bastion"})
}

func TestHumaAudit_PushProviderDelete_NoAudit_OnBadName(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaPushProviderRouter(t, strictAllowAll{}, auditCap, newHPushProviderPool())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/push-providers/BAD_NAME", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (bad path-name); body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit written on bad-name push-provider.delete (%d events)", len(auditCap.Events()))
	}
}

// === OpenAPI fragment: ALL push-provider operations from the FULL-TYPED Go types ===

func TestHumaPushProvider_OpenAPIFragment_3_1(t *testing.T) {
	frag, err := HumaPushProviderSpecYAML()
	if err != nil {
		t.Fatalf("HumaPushProviderSpecYAML: %v", err)
	}
	if !strings.Contains(frag, "openapi: 3.1.0") {
		t.Errorf("huma fragment does not carry `openapi: 3.1.0`:\n%s", frag)
	}
	for _, want := range []string{
		"createPushProvider", "listPushProviders", "getPushProvider",
		"updatePushProvider", "deletePushProvider", "params",
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("OpenAPI fragment does not contain %q:\n%s", want, frag)
		}
	}
	if strings.Contains(frag, "octet-stream") {
		t.Errorf("OpenAPI fragment carries application/octet-stream:\n%s", frag)
	}
}
