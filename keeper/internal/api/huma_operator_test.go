package api

// Guard tests for ROLLOUT BATCH 2a turning the OPERATOR domain WHOLESALE onto huma full-typed
// (ADR-054 §Pattern, 5 references). ALL operator routes on huma: create/revoke/
// issue-token — WRITE+AUDIT (variant B, huma-audit-middleware); list — read with typed
// query (no audit); get — read with path (no audit). They prove the cluster invariants
// over chi:
//
//   - wire/golden: create 201 OperatorCreateReply; list 200 envelope items[]; get 200
//     Operator; revoke 204 empty; issue-token 200 IssueTokenReply (byte-exact);
//   - unknown-field → 400; missing-required → 422; bad auth_method enum → 422; bad
//     revoked bool → 400 (the query reference); RBAC-deny → 403;
//   - S6-GUARD on EVERY write route (create/revoke/issue-token): the full huma wiring
//     (RequirePermission + humaAuditMiddleware + huma handler) writes an audit event with a
//     NON-EMPTY payload + the CORRECT event-type on 2xx and does NOT write on 4xx/403.

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
	"github.com/souls-guild/soul-stack/shared/audit"
)

// opCreatedAt — a fixed created_at that SelectByAID returns in all operator success
// paths (a deterministic golden wire).
var opCreatedAt = time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)

// opPool — a narrow mock of [handlers.OperatorPool] for all operator success paths of the
// huma test. Covers 2xx (S6-guard: audit on success). Exec INSERT/UPDATE → OK;
// QueryRow COUNT → 1; QueryRow SELECT operator → an active archon-bob (NULL
// created_by_aid → bootstrap_initial=true); Query lock-admins → empty (target is not an
// admin → revoke does not lock out). Concrete scenarios are not varied — error
// classification is validated by handlers/operator_test.go.
type opPool struct{}

func (opPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag("OK 1"), nil
}
func (opPool) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "COUNT(*) FROM operators"):
		return opIntRow{n: 1}
	case strings.Contains(sql, "SELECT aid, display_name"):
		// active archon-bob, created_by_aid NULL (bootstrap_initial=true),
		// created_via='bootstrap' (ADR-058(d)), revoked_at NULL, metadata empty.
		return opStaticRow{values: []any{
			"archon-bob", "Bob", "jwt", opCreatedAt,
			nil, "bootstrap", nil, []byte("{}"),
		}}
	}
	return opStaticRow{err: pgx.ErrNoRows}
}
func (opPool) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	switch {
	case strings.Contains(sql, "FROM synod_operators"),
		strings.Contains(sql, "FROM rbac_role_operators"):
		return &opAdminRows{}, nil // empty → target is not cluster-admin → revoke ok
	case strings.Contains(sql, "FROM operators"):
		return &opListRows{}, nil // a single archon-bob row
	}
	return &opAdminRows{}, nil
}
func (opPool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return opTx{}, nil
}

// opTx proxies Exec/Query to opPool; Commit/Rollback no-op.
type opTx struct{ pgx.Tx }

func (opTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return opPool{}.Exec(ctx, sql, args...)
}
func (opTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return opPool{}.Query(ctx, sql, args...)
}
func (opTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return opPool{}.QueryRow(ctx, sql, args...)
}
func (opTx) Commit(context.Context) error   { return nil }
func (opTx) Rollback(context.Context) error { return nil }

type opIntRow struct{ n int }

func (r opIntRow) Scan(dest ...any) error { *dest[0].(*int) = r.n; return nil }

// opStaticRow — a SELECT operator row (8 columns: aid/display_name/auth_method/
// created_at/created_by_aid/created_via/revoked_at/metadata). nil values → NULL.
type opStaticRow struct {
	values []any
	err    error
}

func (r opStaticRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, d := range dest {
		v := r.values[i]
		switch dd := d.(type) {
		case *string:
			*dd = v.(string)
		case *time.Time:
			*dd = v.(time.Time)
		case **string:
			if v == nil {
				*dd = nil
			} else {
				s := v.(string)
				*dd = &s
			}
		case **time.Time:
			if v == nil {
				*dd = nil
			} else {
				tt := v.(time.Time)
				*dd = &tt
			}
		case *[]byte:
			if v == nil {
				*dd = nil
			} else {
				*dd = v.([]byte)
			}
		}
	}
	return nil
}

// opAdminRows — an empty lock-admins Query result (target is not an admin).
type opAdminRows struct{}

func (r *opAdminRows) Next() bool                                   { return false }
func (r *opAdminRows) Scan(...any) error                            { return nil }
func (r *opAdminRows) Err() error                                   { return nil }
func (r *opAdminRows) Close()                                       {}
func (r *opAdminRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *opAdminRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *opAdminRows) Values() ([]any, error)                       { return nil, nil }
func (r *opAdminRows) RawValues() [][]byte                          { return nil }
func (r *opAdminRows) Conn() *pgx.Conn                              { return nil }

// opListRows — a single List operators row (archon-bob, active, NULL created_by_aid).
type opListRows struct{ done bool }

func (r *opListRows) Next() bool {
	if r.done {
		return false
	}
	r.done = true
	return true
}
func (r *opListRows) Scan(dest ...any) error {
	return opStaticRow{values: []any{
		"archon-bob", "Bob", "jwt", opCreatedAt,
		nil, "bootstrap", nil, []byte("{}"),
	}}.Scan(dest...)
}
func (r *opListRows) Err() error                                   { return nil }
func (r *opListRows) Close()                                       {}
func (r *opListRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *opListRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *opListRows) Values() ([]any, error)                       { return nil, nil }
func (r *opListRows) RawValues() [][]byte                          { return nil }
func (r *opListRows) Conn() *pgx.Conn                              { return nil }

// opIssuer — a JWTIssuer mock: a fixed token (a deterministic golden jwt).
type opIssuer struct{}

func (opIssuer) Issue(aid string, _ []string, _ time.Duration, _ bool) (string, error) {
	return "jwt-" + aid, nil
}

// opRBAC — RBACSource-mock (RolesOf).
type opRBAC struct{}

func (opRBAC) RolesOf(string) []string { return nil }

// humaOperatorRouter assembles a chi router with ALL operator routes via huma — the
// production wiring from router.go: RequirePermission(operator.<action>) on each group +
// (for write) huma-audit-middleware variant B + a huma operation.
// injectClaims replaces RequireJWT.
func humaOperatorRouter(t *testing.T, enforcer apimiddleware.PermissionChecker, auditW audit.Writer) *chi.Mux {
	return humaOperatorRouterWithPool(t, enforcer, auditW, opPool{})
}

// opCapturePool — opPool + intercepts the SQL/args of the list SELECT (for the ?q= bind guard).
type opCapturePool struct {
	opPool
	listSQL  string
	listArgs []any
}

func (p *opCapturePool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "SELECT aid, display_name") && strings.Contains(sql, "FROM operators") {
		p.listSQL, p.listArgs = sql, args
	}
	return p.opPool.Query(ctx, sql, args...)
}

// humaOperatorRouterWithPool — humaOperatorRouter with an injected OperatorPool (for guards
// that need to intercept the SQL/args of the domain query).
func humaOperatorRouterWithPool(t *testing.T, enforcer apimiddleware.PermissionChecker, auditW audit.Writer, pool handlers.OperatorPool) *chi.Mux {
	t.Helper()
	installHumaErrorOverride()
	opH := handlers.NewOperatorHandler(pool, opIssuer{}, opRBAC{}, time.Hour, nil)

	r := chi.NewRouter()
	injectClaims := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := apimiddleware.InjectClaimsForTest(req.Context(), &keeperjwt.Claims{Subject: "archon-alice"})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
	r.Route("/v1", func(r chi.Router) {
		r.Route("/operators", func(r chi.Router) {
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "operator", "create", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaOperatorCreate(newHumaOperatorAPI(r, auditW, audit.EventOperatorCreated, nil), opH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "operator", "list", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaOperatorList(newHumaCadenceAPI(r), opH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "operator", "list", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaOperatorGet(newHumaCadenceAPI(r), opH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "operator", "revoke", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaOperatorRevoke(newHumaOperatorAPI(r, auditW, audit.EventOperatorRevoked, nil), opH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "operator", "issue-token", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaOperatorIssueToken(newHumaOperatorAPI(r, auditW, audit.EventOperatorTokenIssued, nil), opH)
			})
		})
	})
	return r
}

// === CREATE (WRITE+AUDIT operator.created) ===

func TestHumaOperator_Create_GoldenWire(t *testing.T) {
	r := humaOperatorRouter(t, strictAllowAll{}, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/operators", strings.NewReader(`{"aid":"archon-bob","display_name":"Bob"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply is not a JSON object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"aid":"archon-bob","created_at":"2026-06-13T10:00:00Z","created_by_aid":"archon-alice","display_name":"Bob","jwt":"jwt-archon-bob"}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-drift operator.create:\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaOperator_Create_UnknownField_400(t *testing.T) {
	r := humaOperatorRouter(t, strictAllowAll{}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/operators", strings.NewReader(`{"aid":"archon-bob","bogus":1}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

func TestHumaOperator_Create_MissingAID_422(t *testing.T) {
	r := humaOperatorRouter(t, strictAllowAll{}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/operators", strings.NewReader(`{"display_name":"Bob"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

func TestHumaOperator_Create_RBACDeny_403(t *testing.T) {
	r := humaOperatorRouter(t, strictDenyAll{}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/operators", strings.NewReader(`{"aid":"archon-bob"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaAudit_OperatorCreate_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaOperatorRouter(t, strictAllowAll{}, auditCap)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/operators", strings.NewReader(`{"aid":"archon-bob","display_name":"Bob"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventOperatorCreated, map[string]any{
		"aid": "archon-bob", "created_by_aid": "archon-alice", "auth_method": "jwt",
	})
}

func TestHumaAudit_OperatorCreate_NoAudit_OnRBACDeny(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaOperatorRouter(t, strictDenyAll{}, auditCap)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/operators", strings.NewReader(`{"aid":"archon-bob"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit recorded on RBAC-deny operator.create (%d events)", len(auditCap.Events()))
	}
}

// === LIST (READ, query-tier, no audit) ===

func TestHumaOperator_List_GoldenWire(t *testing.T) {
	r := humaOperatorRouter(t, strictAllowAll{}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/operators", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply is not a JSON object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"items":[{"aid":"archon-bob","auth_method":"jwt","bootstrap_initial":true,"created_at":"2026-06-13T10:00:00Z","created_via":"bootstrap","display_name":"Bob"}],"limit":50,"offset":0,"total":1}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-drift operator.list:\n got  = %s\n want = %s", got, golden)
	}
}

// TestHumaOperator_List_Q_BindsToFilter — guard: ?q=<val> binds into
// operatorListInput.Q and reaches SQL as an ILIKE predicate over display_name/aid with
// an escaped (%→\%) argument.
func TestHumaOperator_List_Q_BindsToFilter(t *testing.T) {
	pool := &opCapturePool{}
	r := humaOperatorRouterWithPool(t, strictAllowAll{}, nil, pool)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/operators?q=a%25b", nil) // q="a%b"
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(pool.listSQL, "display_name ILIKE") || !strings.Contains(pool.listSQL, "aid ILIKE") {
		t.Fatalf("list SQL without ILIKE predicate q: %q", pool.listSQL)
	}
	const wantArg = `%a\%b%`
	found := false
	for _, a := range pool.listArgs {
		if a == wantArg {
			found = true
		}
	}
	if !found {
		t.Errorf("list args %v do not contain the escaped q argument %q", pool.listArgs, wantArg)
	}
}

func TestHumaOperator_List_BadAuthMethod_422(t *testing.T) {
	r := humaOperatorRouter(t, strictAllowAll{}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/operators?auth_method=bogus", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (bad auth_method enum); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

func TestHumaOperator_List_BadRevoked_400(t *testing.T) {
	r := humaOperatorRouter(t, strictAllowAll{}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/operators?revoked=notabool", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (bad revoked bool); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

// TestHumaOperator_List_BadOffset_400 — the CONTRACT bounds invariant (decision A, parity
// audit OutOfRangePagination): offset<0 → 400 (sharedapi.CheckPageBounds in ListTyped),
// and NOT 200 with a broken envelope and NOT huma-422 (a huma typed-int does NOT carry a schema-minimum).
// Must match the legacy ParsePage (offset<0 → 400), otherwise a wire-change.
func TestHumaOperator_List_BadOffset_400(t *testing.T) {
	r := humaOperatorRouter(t, strictAllowAll{}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/operators?offset=-1", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (offset<0 → CheckPageBounds 400, NOT 200/huma-422, parity ParsePage); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

// TestHumaOperator_List_BadLimit_400 — out-of-range limit (0 and 1001) → 400
// (sharedapi.CheckPageBounds: limit∈[1,1000]), parity audit OutOfRangePagination and
// the legacy ParsePage. The huma path used to silently return 200 with broken pagination
// (bypassing CheckPageBounds) — this guard catches the regression.
func TestHumaOperator_List_BadLimit_400(t *testing.T) {
	r := humaOperatorRouter(t, strictAllowAll{}, nil)
	for _, c := range []string{
		"/v1/operators?limit=0",
		"/v1/operators?limit=1001",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, c, nil)
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (out-of-range limit → CheckPageBounds 400, parity ParsePage, NOT huma-422); body=%s", c, rec.Code, rec.Body.String())
			continue
		}
		assertHumaProblem(t, rec, problem.TypeMalformedRequest)
	}
}

func TestHumaOperator_List_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaOperatorRouter(t, strictAllowAll{}, auditCap)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/operators", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("READ route operator.list recorded audit (%d events)", len(auditCap.Events()))
	}
}

func TestHumaOperator_List_RBACDeny_403(t *testing.T) {
	r := humaOperatorRouter(t, strictDenyAll{}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/operators", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// === GET (READ, path, no audit) ===

func TestHumaOperator_Get_GoldenWire(t *testing.T) {
	r := humaOperatorRouter(t, strictAllowAll{}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/operators/archon-bob", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply is not a JSON object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"aid":"archon-bob","auth_method":"jwt","bootstrap_initial":true,"created_at":"2026-06-13T10:00:00Z","created_via":"bootstrap","display_name":"Bob"}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-drift operator.get:\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaOperator_Get_BadAID_422(t *testing.T) {
	r := humaOperatorRouter(t, strictAllowAll{}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/operators/INVALID", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (bad path-AID); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

func TestHumaOperator_Get_RBACDeny_403(t *testing.T) {
	r := humaOperatorRouter(t, strictDenyAll{}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/operators/archon-bob", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// === REVOKE (WRITE+AUDIT operator.revoked) ===

func TestHumaOperator_Revoke_204(t *testing.T) {
	r := humaOperatorRouter(t, strictAllowAll{}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/operators/archon-bob/revoke", strings.NewReader(`{}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "" {
		t.Errorf("204-body operator.revoke must be EMPTY, got %q", body)
	}
}

func TestHumaAudit_OperatorRevoke_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaOperatorRouter(t, strictAllowAll{}, auditCap)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/operators/archon-bob/revoke", strings.NewReader(`{"reason":"offboarding"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventOperatorRevoked, map[string]any{
		"aid": "archon-bob", "reason": "offboarding",
	})
}

func TestHumaAudit_OperatorRevoke_NoAudit_OnBadAID(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaOperatorRouter(t, strictAllowAll{}, auditCap)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/operators/INVALID/revoke", strings.NewReader(`{}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (bad path-AID); body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit recorded on bad-AID revoke (%d events)", len(auditCap.Events()))
	}
}

// === ISSUE-TOKEN (WRITE+AUDIT operator.token-issued, 200 WITH BODY) ===

func TestHumaOperator_IssueToken_GoldenWire(t *testing.T) {
	r := humaOperatorRouter(t, strictAllowAll{}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/operators/archon-bob/issue-token", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply is not a JSON object: %v; body=%s", err, rec.Body.String())
	}
	// expires_at — now+TTL (unstable), we guard the set of the remaining fields.
	if m["aid"] != "archon-bob" {
		t.Errorf("aid = %v, want archon-bob", m["aid"])
	}
	if m["jwt"] != "jwt-archon-bob" {
		t.Errorf("jwt = %v, want jwt-archon-bob", m["jwt"])
	}
	if _, ok := m["expires_at"]; !ok {
		t.Errorf("issue-token reply without expires_at: %v", m)
	}
}

func TestHumaAudit_OperatorIssueToken_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaOperatorRouter(t, strictAllowAll{}, auditCap)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/operators/archon-bob/issue-token", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventOperatorTokenIssued, map[string]any{"aid": "archon-bob"})
}

func TestHumaAudit_OperatorIssueToken_NoAudit_OnBadAID(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaOperatorRouter(t, strictAllowAll{}, auditCap)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/operators/INVALID/issue-token", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (bad path-AID); body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit recorded on bad-AID issue-token (%d events)", len(auditCap.Events()))
	}
}

// === OpenAPI fragment: ALL operator operations from FULL-TYPED Go types ===

func TestHumaOperator_OpenAPIFragment_3_1(t *testing.T) {
	frag, err := HumaOperatorSpecYAML()
	if err != nil {
		t.Fatalf("HumaOperatorSpecYAML: %v", err)
	}
	if !strings.Contains(frag, "openapi: 3.1.0") {
		t.Errorf("huma fragment does not carry `openapi: 3.1.0`:\n%s", frag)
	}
	for _, want := range []string{
		"createOperator", "listOperators", "getOperator", "revokeOperator",
		"issueOperatorToken", "auth_method", "revoked",
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("OpenAPI fragment does not contain %q:\n%s", want, frag)
		}
	}
	// list-query auth_method multi-value is NOT needed (single string), but there
	// must be no explode artifacts of the RawBody bridge.
	if strings.Contains(frag, "octet-stream") {
		t.Errorf("OpenAPI fragment carries application/octet-stream:\n%s", frag)
	}
}
