package api

// Guard tests of ROLLOUT BATCH 2d that unfolds the SYNOD domain (groups / membership / bundle)
// ENTIRELY onto huma full-typed (ADR-054 §Pattern, role/operator/augur/herald references).
// synod create/update/delete + add/remove-operator + grant/revoke-role — WRITE+AUDIT
// (variant B, huma audit middleware; events synod.created/.updated/.deleted/
// .operator-added/.operator-removed/.role-granted/.role-revoked); synod list — read
// (no audit). They prove the cluster invariants over chi:
//
//   - wire/golden: synod create 201 empty body; synod list 200 SynodView[] byte-for-
//     byte (Description always, Roles/Operators []-vs-null); write-204 empty;
//   - unknown-field → 400; missing-required → 422; bad path-AID → 422; RBAC-deny → 403;
//   - S6-GUARD on EVERY write route: the full huma wiring writes an audit event with a
//     NON-EMPTY payload + the CORRECT event-type on 2xx and does NOT write on 4xx/403.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// synodSuccessPool — a narrow mock [rbac.ServicePool] for the success path of all synod
// write routes in the huma test: lockSynod=found+non-builtin, synod-roles=empty (the group
// grants no `*` → the self-lockout probe is not invoked), lock membership/bundle=found, the
// caller subset-check is trivial (required is empty). Covers ONLY the 2xx path (S6-guard) —
// error classification is validated by the handler unit tests (handlers/synod_test.go) and
// rbac-integration. Tx proxies Exec/Query to the pool.
type synodSuccessPool struct {
	listRows [][]any // synod.list rows (LoadSynodViews), for GoldenWire
}

func (synodSuccessPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag("OK 1"), nil
}
func (synodSuccessPool) QueryRow(context.Context, string, ...any) pgx.Row {
	return synodErrRow{err: pgx.ErrNoRows}
}
func (p synodSuccessPool) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	switch {
	case strings.Contains(sql, "FOR UPDATE"):
		// lockSynod (builtin=false) / lockSynodRole / lockSynodOperator — the row
		// exists. lockSynod scans one bool (builtin); the other locks do not read the
		// row body (rows.Next() is enough). A bool row covers all cases.
		return &synodBoolRows{values: []bool{false}}, nil
	case strings.Contains(sql, "rp.permission = '*'"):
		return &synodEmptyRows{}, nil // the group grants no `*` → self-lockout probe skipped
	case strings.Contains(sql, "FROM synod_roles WHERE synod_name"):
		return &synodStrRows{}, nil // the group has no roles → subset-check is trivial
	case strings.Contains(sql, "FROM rbac_role_permissions"):
		return &synodStrRows{}, nil // the role has no permissions (grant/revoke-role: grants no `*`)
	case strings.Contains(sql, "default_scope FROM rbac_roles"):
		return &synodEmptyRows{}, nil // the role has no scope (grant-role: roleDefaultScope → nil)
	case strings.Contains(sql, "FROM synods ORDER BY name"):
		return &synodViewRows{rows: p.listRows}, nil
	case strings.Contains(sql, "FROM synod_roles"):
		return &synodPairRows{}, nil // synod-view roles (list)
	case strings.Contains(sql, "FROM synod_operators"):
		return &synodPairRows{}, nil // synod-view operators (list)
	}
	return nil, errStrictUnexpectedSQL
}
func (synodSuccessPool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return synodSuccessTx{}, nil
}

type synodErrRow struct{ err error }

func (r synodErrRow) Scan(...any) error { return r.err }

// synodSuccessTx — a pgx.Tx that proxies Exec/Query back to the success pool;
// Commit/Rollback are no-ops.
type synodSuccessTx struct{ pgx.Tx }

func (synodSuccessTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return synodSuccessPool{}.Exec(ctx, sql, args...)
}
func (synodSuccessTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return synodSuccessPool{}.Query(ctx, sql, args...)
}
func (synodSuccessTx) Commit(context.Context) error   { return nil }
func (synodSuccessTx) Rollback(context.Context) error { return nil }

// --- minimal pgx.Rows wrappers (api package) ---

type synodBoolRows struct {
	values []bool
	idx    int
}

func (r *synodBoolRows) Next() bool                                   { r.idx++; return r.idx <= len(r.values) }
func (r *synodBoolRows) Scan(dest ...any) error                       { *dest[0].(*bool) = r.values[r.idx-1]; return nil }
func (r *synodBoolRows) Err() error                                   { return nil }
func (r *synodBoolRows) Close()                                       {}
func (r *synodBoolRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *synodBoolRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *synodBoolRows) Values() ([]any, error)                       { return nil, nil }
func (r *synodBoolRows) RawValues() [][]byte                          { return nil }
func (r *synodBoolRows) Conn() *pgx.Conn                              { return nil }

type synodEmptyRows struct{}

func (synodEmptyRows) Next() bool                                   { return false }
func (synodEmptyRows) Scan(...any) error                            { return nil }
func (synodEmptyRows) Err() error                                   { return nil }
func (synodEmptyRows) Close()                                       {}
func (synodEmptyRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (synodEmptyRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (synodEmptyRows) Values() ([]any, error)                       { return nil, nil }
func (synodEmptyRows) RawValues() [][]byte                          { return nil }
func (synodEmptyRows) Conn() *pgx.Conn                              { return nil }

type synodStrRows struct {
	values []string
	idx    int
}

func (r *synodStrRows) Next() bool                                   { r.idx++; return r.idx <= len(r.values) }
func (r *synodStrRows) Scan(dest ...any) error                       { *dest[0].(*string) = r.values[r.idx-1]; return nil }
func (r *synodStrRows) Err() error                                   { return nil }
func (r *synodStrRows) Close()                                       {}
func (r *synodStrRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *synodStrRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *synodStrRows) Values() ([]any, error)                       { return nil, nil }
func (r *synodStrRows) RawValues() [][]byte                          { return nil }
func (r *synodStrRows) Conn() *pgx.Conn                              { return nil }

// synodViewRows — synod.list rows (LoadSynodViews loadSynodViewRows):
// name, description, builtin.
type synodViewRows struct {
	rows [][]any
	idx  int
}

func (r *synodViewRows) Next() bool { r.idx++; return r.idx <= len(r.rows) }
func (r *synodViewRows) Scan(dest ...any) error {
	row := r.rows[r.idx-1]
	*dest[0].(*string) = row[0].(string)
	*dest[1].(*string) = row[1].(string)
	*dest[2].(*bool) = row[2].(bool)
	return nil
}
func (r *synodViewRows) Err() error                                   { return nil }
func (r *synodViewRows) Close()                                       {}
func (r *synodViewRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *synodViewRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *synodViewRows) Values() ([]any, error)                       { return nil, nil }
func (r *synodViewRows) RawValues() [][]byte                          { return nil }
func (r *synodViewRows) Conn() *pgx.Conn                              { return nil }

// synodPairRows — empty (synod, role|aid) rows for synod-view roles/operators.
type synodPairRows struct{}

func (synodPairRows) Next() bool                                   { return false }
func (synodPairRows) Scan(...any) error                            { return nil }
func (synodPairRows) Err() error                                   { return nil }
func (synodPairRows) Close()                                       {}
func (synodPairRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (synodPairRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (synodPairRows) Values() ([]any, error)                       { return nil, nil }
func (synodPairRows) RawValues() [][]byte                          { return nil }
func (synodPairRows) Conn() *pgx.Conn                              { return nil }

// humaSynodRouter assembles a chi router with ALL synod routes via huma — the production
// wiring literally from router.go: RequirePermission(synod.<action>) on each group + (for
// write) huma audit middleware variant B + the huma operation. injectClaims replaces
// RequireJWT.
func humaSynodRouter(t *testing.T, enforcer apimiddleware.PermissionChecker, auditW audit.Writer, pool rbac.ServicePool) *chi.Mux {
	t.Helper()
	installHumaErrorOverride()
	svc, err := rbac.NewService(rbac.ServiceDeps{Pool: pool})
	if err != nil {
		t.Fatalf("rbac.NewService: %v", err)
	}
	synodH := handlers.NewSynodHandler(svc, nil)

	r := chi.NewRouter()
	injectClaims := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := apimiddleware.InjectClaimsForTest(req.Context(), &keeperjwt.Claims{Subject: "archon-alice"})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
	r.Route("/v1", func(r chi.Router) {
		r.Route("/synods", func(r chi.Router) {
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "synod", "create", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaSynodCreate(newHumaSynodAPI(r, auditW, audit.EventSynodCreated, nil), synodH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "synod", "list", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaSynodList(newHumaCadenceAPI(r), synodH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "synod", "update", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaSynodUpdate(newHumaSynodAPI(r, auditW, audit.EventSynodUpdated, nil), synodH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "synod", "delete", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaSynodDelete(newHumaSynodAPI(r, auditW, audit.EventSynodDeleted, nil), synodH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "synod", "add-operator", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaSynodAddOperator(newHumaSynodAPI(r, auditW, audit.EventSynodOperatorAdded, nil), synodH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "synod", "remove-operator", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaSynodRemoveOperator(newHumaSynodAPI(r, auditW, audit.EventSynodOperatorRemoved, nil), synodH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "synod", "grant-role", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaSynodGrantRole(newHumaSynodAPI(r, auditW, audit.EventSynodRoleGranted, nil), synodH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "synod", "revoke-role", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaSynodRevokeRole(newHumaSynodAPI(r, auditW, audit.EventSynodRoleRevoked, nil), synodH)
			})
		})
	})
	return r
}

// === CREATE (WRITE+AUDIT synod.created) ===

func TestHumaSynod_Create_GoldenEmptyBody(t *testing.T) {
	r := humaSynodRouter(t, strictAllowAll{}, nil, synodSuccessPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/synods", strings.NewReader(`{"name":"team-ops"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	const golden = "" // legacy POST /v1/synods 201 — no body
	if got := rec.Body.String(); got != golden {
		t.Errorf("GOLDEN wire drift synod.create 201-body: got=%q want=%q", got, golden)
	}
}

func TestHumaSynod_Create_UnknownField_400(t *testing.T) {
	r := humaSynodRouter(t, strictAllowAll{}, nil, synodSuccessPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/synods", strings.NewReader(`{"name":"team-ops","bogus":1}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

func TestHumaSynod_Create_MissingName_422(t *testing.T) {
	r := humaSynodRouter(t, strictAllowAll{}, nil, synodSuccessPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/synods", strings.NewReader(`{"description":"x"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

func TestHumaSynod_Create_RBACDeny_403(t *testing.T) {
	r := humaSynodRouter(t, strictDenyAll{}, nil, synodSuccessPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/synods", strings.NewReader(`{"name":"team-ops"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaAudit_SynodCreate_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaSynodRouter(t, strictAllowAll{}, auditCap, synodSuccessPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/synods", strings.NewReader(`{"name":"team-ops"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventSynodCreated, map[string]any{
		"name": "team-ops", "created_by_aid": "archon-alice",
	})
}

func TestHumaAudit_SynodCreate_NoAudit_OnRBACDeny(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaSynodRouter(t, strictDenyAll{}, auditCap, synodSuccessPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/synods", strings.NewReader(`{"name":"team-ops"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit recorded on RBAC-deny synod.create (%d events)", len(auditCap.Events()))
	}
}

// === LIST (READ, no audit) ===

func TestHumaSynod_List_GoldenWire(t *testing.T) {
	pool := synodSuccessPool{listRows: [][]any{
		{"team-ops", "ops team", false},
	}}
	r := humaSynodRouter(t, strictAllowAll{}, nil, pool)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/synods", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply is not a JSON object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"items":[{"builtin":false,"description":"ops team","name":"team-ops","operators":[],"roles":[]}]}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire drift synod.list:\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaSynod_List_GoldenEmpty(t *testing.T) {
	r := humaSynodRouter(t, strictAllowAll{}, nil, synodSuccessPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/synods", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	const golden = `{"items":[]}`
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	out, _ := json.Marshal(m)
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire drift synod.list (empty): got=%q want=%q", got, golden)
	}
}

func TestHumaSynod_List_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaSynodRouter(t, strictAllowAll{}, auditCap, synodSuccessPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/synods", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("READ route synod.list recorded audit (%d events)", len(auditCap.Events()))
	}
}

func TestHumaSynod_List_RBACDeny_403(t *testing.T) {
	r := humaSynodRouter(t, strictDenyAll{}, nil, synodSuccessPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/synods", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// === UPDATE (WRITE+AUDIT synod.updated) ===

func TestHumaSynod_Update_204(t *testing.T) {
	r := humaSynodRouter(t, strictAllowAll{}, nil, synodSuccessPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/synods/team-ops", strings.NewReader(`{"description":"new desc"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "" {
		t.Errorf("204-body synod.update must be EMPTY, got %q", body)
	}
}

func TestHumaAudit_SynodUpdate_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaSynodRouter(t, strictAllowAll{}, auditCap, synodSuccessPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/synods/team-ops", strings.NewReader(`{"description":"new desc"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventSynodUpdated, map[string]any{
		"name": "team-ops", "description": "new desc",
	})
}

func TestHumaAudit_SynodUpdate_NoAudit_OnMissingDescription(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaSynodRouter(t, strictAllowAll{}, auditCap, synodSuccessPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/synods/team-ops", strings.NewReader(`{}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (missing required description); body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit recorded on 422 synod.update (%d events)", len(auditCap.Events()))
	}
}

// === DELETE (WRITE+AUDIT synod.deleted) ===

func TestHumaSynod_Delete_204(t *testing.T) {
	r := humaSynodRouter(t, strictAllowAll{}, nil, synodSuccessPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/synods/team-ops", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "" {
		t.Errorf("204-body synod.delete must be EMPTY, got %q", body)
	}
}

func TestHumaAudit_SynodDelete_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaSynodRouter(t, strictAllowAll{}, auditCap, synodSuccessPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/synods/team-ops", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventSynodDeleted, map[string]any{"name": "team-ops"})
}

func TestHumaAudit_SynodDelete_NoAudit_OnRBACDeny(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaSynodRouter(t, strictDenyAll{}, auditCap, synodSuccessPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/synods/team-ops", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit recorded on RBAC-deny synod.delete (%d events)", len(auditCap.Events()))
	}
}

// === ADD OPERATOR (WRITE+AUDIT synod.operator-added) ===

func TestHumaSynod_AddOperator_204(t *testing.T) {
	r := humaSynodRouter(t, strictAllowAll{}, nil, synodSuccessPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/synods/team-ops/operators", strings.NewReader(`{"aid":"archon-bob"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaAudit_SynodAddOperator_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaSynodRouter(t, strictAllowAll{}, auditCap, synodSuccessPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/synods/team-ops/operators", strings.NewReader(`{"aid":"archon-bob"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventSynodOperatorAdded, map[string]any{
		"name": "team-ops", "aid": "archon-bob", "added_by_aid": "archon-alice",
	})
}

func TestHumaAudit_SynodAddOperator_NoAudit_OnInvalidAID(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaSynodRouter(t, strictAllowAll{}, auditCap, synodSuccessPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/synods/team-ops/operators", strings.NewReader(`{"aid":"bad aid!"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (malformed AID); body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit recorded on invalid-AID add-operator (%d events)", len(auditCap.Events()))
	}
}

// === REMOVE OPERATOR (WRITE+AUDIT synod.operator-removed) ===

func TestHumaSynod_RemoveOperator_204(t *testing.T) {
	r := humaSynodRouter(t, strictAllowAll{}, nil, synodSuccessPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/synods/team-ops/operators/archon-bob", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaAudit_SynodRemoveOperator_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaSynodRouter(t, strictAllowAll{}, auditCap, synodSuccessPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/synods/team-ops/operators/archon-bob", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventSynodOperatorRemoved, map[string]any{
		"name": "team-ops", "aid": "archon-bob",
	})
}

func TestHumaAudit_SynodRemoveOperator_NoAudit_OnInvalidAID(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaSynodRouter(t, strictAllowAll{}, auditCap, synodSuccessPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/synods/team-ops/operators/INVALID", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (malformed path-AID); body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit recorded on invalid-AID remove-operator (%d events)", len(auditCap.Events()))
	}
}

// === GRANT ROLE (WRITE+AUDIT synod.role-granted) ===

func TestHumaSynod_GrantRole_204(t *testing.T) {
	r := humaSynodRouter(t, strictAllowAll{}, nil, synodSuccessPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/synods/team-ops/roles", strings.NewReader(`{"role":"ops"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaAudit_SynodGrantRole_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaSynodRouter(t, strictAllowAll{}, auditCap, synodSuccessPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/synods/team-ops/roles", strings.NewReader(`{"role":"ops"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventSynodRoleGranted, map[string]any{
		"name": "team-ops", "role": "ops", "granted_by_aid": "archon-alice",
	})
}

func TestHumaAudit_SynodGrantRole_NoAudit_OnMissingRole(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaSynodRouter(t, strictAllowAll{}, auditCap, synodSuccessPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/synods/team-ops/roles", strings.NewReader(`{}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (missing required role); body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit recorded on 422 grant-role (%d events)", len(auditCap.Events()))
	}
}

// === REVOKE ROLE (WRITE+AUDIT synod.role-revoked) ===

func TestHumaSynod_RevokeRole_204(t *testing.T) {
	r := humaSynodRouter(t, strictAllowAll{}, nil, synodSuccessPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/synods/team-ops/roles/ops", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaAudit_SynodRevokeRole_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaSynodRouter(t, strictAllowAll{}, auditCap, synodSuccessPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/synods/team-ops/roles/ops", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventSynodRoleRevoked, map[string]any{
		"name": "team-ops", "role": "ops",
	})
}

func TestHumaAudit_SynodRevokeRole_NoAudit_OnRBACDeny(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaSynodRouter(t, strictDenyAll{}, auditCap, synodSuccessPool{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/synods/team-ops/roles/ops", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit recorded on RBAC-deny revoke-role (%d events)", len(auditCap.Events()))
	}
}

// === OpenAPI fragment: ALL synod operations from the FULL-TYPED Go types ===

func TestHumaSynod_OpenAPIFragment_3_1(t *testing.T) {
	frag, err := HumaSynodSpecYAML()
	if err != nil {
		t.Fatalf("HumaSynodSpecYAML: %v", err)
	}
	if !strings.Contains(frag, "openapi: 3.1.0") {
		t.Errorf("huma fragment does not contain `openapi: 3.1.0`:\n%s", frag)
	}
	for _, want := range []string{
		"createSynod", "listSynods", "updateSynod", "deleteSynod",
		"addSynodOperator", "removeSynodOperator", "grantSynodRole", "revokeSynodRole",
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("OpenAPI fragment does not contain %q:\n%s", want, frag)
		}
	}
	if strings.Contains(frag, "octet-stream") {
		t.Errorf("OpenAPI fragment contains application/octet-stream:\n%s", frag)
	}
}
