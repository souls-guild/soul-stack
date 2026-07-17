package api

// VOYAGE guard tests on huma, FULL-TYPED WRITE-SELF-AUDIT form (batch-2f, ADR-054).
// create/cancel write audit INSIDE the handler (CreateTyped→emitCreated / CancelTyped→
// emitCancelled), without audit-middleware. preview — read-like dry-resolve without audit.
// list/get — read (no audit). Guards prove: wire 202/200, S6-SELF-AUDIT (handler
// ACTUALLY writes an event with a non-empty payload on 2xx), NoAudit on preview/422/403/404, 400 on
// list BadOffset/BadLimit (CheckPageBounds), golden-JSON byte-exact, RBAC-by-kind→403.
// command-kind covers create/cancel/preview (bare-check errand.run, scoper=nil →
// cluster-wide resolve; does not need a DB-scope incReader).

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
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/soulpurview"
	"github.com/souls-guild/soul-stack/shared/audit"
)

const voyageTestID = "01HZ0000000000000000000000"

// fakeVoyageStore — minimal mock of handlers.VoyageStore for the api-package huma guards.
// Serves INSERT INTO voyages (RETURNING created_at), INSERT INTO voyage_targets,
// selectByID (cancel), cancel-UPDATE, COUNT (list). Targets are inserted via tx.CopyFrom.
type fakeVoyageStore struct {
	insertCalls int
	selectByID  func(id string) pgx.Row
}

func (f *fakeVoyageStore) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	switch {
	case strings.Contains(sql, "INSERT INTO voyage_targets"):
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	case strings.Contains(sql, "UPDATE voyages") && strings.Contains(sql, "status      = 'cancelled'"):
		return pgconn.NewCommandTag("UPDATE 1"), nil
	}
	return pgconn.CommandTag{}, errStrictUnexpectedSQL
}

func (f *fakeVoyageStore) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "INSERT INTO voyages"):
		f.insertCalls++
		return strictScalarRow{vals: []any{time.Now().UTC()}}
	case strings.Contains(sql, "FROM voyages\nWHERE voyage_id = $1"):
		if f.selectByID != nil {
			return f.selectByID(args[0].(string))
		}
		return strictErrRow{err: pgx.ErrNoRows}
	case strings.Contains(sql, "SELECT COUNT(*) FROM voyages"):
		return strictScalarRow{vals: []any{0}}
	}
	return strictErrRow{err: errStrictUnexpectedSQL}
}

func (f *fakeVoyageStore) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	return &strictEmptyRows{}, nil
}
func (f *fakeVoyageStore) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 0, errStrictUnexpectedSQL
}
func (f *fakeVoyageStore) BeginTx(_ context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	return &fakeVoyageTx{store: f}, nil
}

type fakeVoyageTx struct{ store *fakeVoyageStore }

func (t *fakeVoyageTx) Begin(context.Context) (pgx.Tx, error) { return t, nil }
func (t *fakeVoyageTx) Commit(context.Context) error          { return nil }
func (t *fakeVoyageTx) Rollback(context.Context) error        { return nil }
func (t *fakeVoyageTx) CopyFrom(_ context.Context, _ pgx.Identifier, _ []string, src pgx.CopyFromSource) (int64, error) {
	n := int64(0)
	for src.Next() {
		n++
	}
	return n, src.Err()
}
func (t *fakeVoyageTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults { panic("unexpected") }
func (t *fakeVoyageTx) LargeObjects() pgx.LargeObjects                         { panic("unexpected") }
func (t *fakeVoyageTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	panic("unexpected")
}
func (t *fakeVoyageTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.store.Exec(ctx, sql, args...)
}
func (t *fakeVoyageTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.store.Query(ctx, sql, args...)
}
func (t *fakeVoyageTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.store.QueryRow(ctx, sql, args...)
}
func (t *fakeVoyageTx) Conn() *pgx.Conn { return nil }

// fakeVoyageCmdResolver — command resolver: always returns a fixed SID set
// (scoper=nil → cluster-wide ResolveSIDs).
type fakeVoyageCmdResolver struct{ sids []string }

func (r fakeVoyageCmdResolver) ResolveSIDs(context.Context, handlers.VoyageCommandFilter) ([]string, error) {
	return r.sids, nil
}
func (r fakeVoyageCmdResolver) ResolveSIDsInScope(context.Context, handlers.VoyageCommandFilter, soulpurview.Scope) (handlers.ScopedSIDs, error) {
	return handlers.ScopedSIDs{SIDs: r.sids}, nil
}

// fakeVoyageScenResolver — scenario resolver. By default (zero) returns an empty set
// (command guards do not call it, but non-nil is needed for the CreateTyped config check). With
// incarnations[] filled it serves scenario-kind create (resolveScenarioScopeErr).
type fakeVoyageScenResolver struct{ incarnations []string }

func (r fakeVoyageScenResolver) ResolveIncarnations(context.Context, handlers.VoyageScenarioFilter) ([]string, error) {
	return r.incarnations, nil
}

// voyageCancelRow — pgx.Row for voyage.scanVoyage for cancel selectByID (exact order of
// 31 dest, parity with scanVoyage in keeper/internal/voyage/crud.go). Minimal command-kind
// pending run (cancellable: status=pending, not terminal/running). Fields by index:
// #0 voyage_id, #1 kind, #16 status — critical for CancelTyped (reads v.Kind/v.Status);
// the rest — zero/nil (a mismatch with scanVoyage would break the guard → recount).
type voyageCancelRow struct {
	id     string
	status string
	// kind — "command" (default, zero=empty → normalized) or "scenario".
	kind string
}

func (r voyageCancelRow) Scan(dest ...any) error {
	if len(dest) != 31 {
		return errStrict("voyageCancelRow: expected 31 dest, got scanVoyage desync")
	}
	kind := r.kind
	if kind == "" {
		kind = "command"
	}
	*dest[0].(*string) = r.id // voyage_id
	*dest[1].(*string) = kind // kind
	*dest[2].(**string) = nil // scenario_name
	*dest[3].(**string) = nil // module
	*dest[4].(*[]byte) = nil  // input
	*dest[5].(*json.RawMessage) = json.RawMessage(`["node-1.example.com"]`)
	*dest[6].(*[]byte) = nil       // target_origin
	*dest[7].(**int) = nil         // batch_size
	*dest[8].(**int) = nil         // concurrency
	*dest[9].(**string) = nil      // batch_mode
	*dest[10].(*bool) = false      // dry_run
	*dest[11].(**time.Time) = nil  // schedule_at
	*dest[12].(**float64) = nil    // interval_secs
	*dest[13].(**string) = nil     // on_failure
	*dest[14].(*int) = 1           // total_batches
	*dest[15].(*int) = 0           // current_batch_index
	*dest[16].(*string) = r.status // status
	*dest[17].(**string) = nil     // claimed_by_kid
	*dest[18].(**time.Time) = nil  // last_renewed_at
	*dest[19].(**time.Time) = nil  // claim_expires
	*dest[20].(*int) = 0           // attempt
	*dest[21].(*string) = "archon-alice"
	*dest[22].(*time.Time) = time.Now().UTC()
	*dest[23].(**time.Time) = nil // started_at
	*dest[24].(**time.Time) = nil // finished_at
	*dest[25].(*[]byte) = nil     // summary
	*dest[26].(**int) = nil       // batch_percent
	*dest[27].(**int) = nil       // fail_threshold
	*dest[28].(**float64) = nil   // inter_unit_secs
	*dest[29].(**bool) = nil      // require_alive
	*dest[30].(**string) = nil    // cadence_id
	return nil
}

// humaVoyageRouter mounts the voyage huma routes exactly per the router.go wiring. enforcer/
// auditW are parameterized; store/resolvers per case. scoper=nil (cluster-wide command).
func humaVoyageRouter(t *testing.T, enforcer apimiddleware.PermissionChecker, auditW audit.Writer, store *fakeVoyageStore, cmd handlers.VoyageCommandResolver) *chi.Mux {
	return humaVoyageRouterScen(t, enforcer, auditW, store, cmd, fakeVoyageScenResolver{})
}

// humaVoyageRouterScen — variant of humaVoyageRouter with a configurable scenario resolver
// (scenario-kind create/cancel: the resolver returns incarnations[], incReader=nil →
// per-incarnation scope-check skipped after bare-check incarnation.run).
func humaVoyageRouterScen(t *testing.T, enforcer apimiddleware.PermissionChecker, auditW audit.Writer, store *fakeVoyageStore, cmd handlers.VoyageCommandResolver, scen handlers.VoyageScenarioResolver) *chi.Mux {
	t.Helper()
	installHumaErrorOverride()
	voyageH := handlers.NewVoyageHandler(store, scen, cmd, nil, enforcer, nil, auditW, nil, 0, 0, nil)

	r := chi.NewRouter()
	injectClaims := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := apimiddleware.InjectClaimsForTest(req.Context(), &keeperjwt.Claims{Subject: "archon-alice"})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
	r.Route("/v1", func(r chi.Router) {
		r.Route("/voyages", func(r chi.Router) {
			r.With(injectClaims).Group(func(r chi.Router) { registerHumaVoyageCreate(newHumaCadenceAPI(r), voyageH) })
			r.With(injectClaims).Group(func(r chi.Router) { registerHumaVoyagePreview(newHumaCadenceAPI(r), voyageH) })
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "incarnation", "history", apimiddleware.NoSelector)).
				Group(func(r chi.Router) {
					api := newHumaCadenceAPI(r)
					registerHumaVoyageList(api, voyageH)
					registerHumaVoyageGet(api, voyageH)
					registerHumaVoyageTargets(api, voyageH)
				})
			r.With(injectClaims).Group(func(r chi.Router) { registerHumaVoyageCancel(newHumaCadenceAPI(r), voyageH) })
		})
	})
	return r
}

const voyageCmdBody = `{"kind":"command","module":"core.cmd.shell","target":{"sids":["node-1.example.com"]}}`

// --- Create (command) ---

func TestHumaVoyage_Create_WireAndAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	store := &fakeVoyageStore{}
	cmd := fakeVoyageCmdResolver{sids: []string{"node-1.example.com"}}
	r := humaVoyageRouter(t, strictAllowAll{}, auditCap, store, cmd)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/voyages", strings.NewReader(voyageCmdBody))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/v1/voyages/") {
		t.Errorf("Location = %q, want /v1/voyages/<id>", loc)
	}
	var reply struct {
		VoyageID  string `json:"voyage_id"`
		Kind      string `json:"kind"`
		ScopeSize int    `json:"scope_size"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if reply.VoyageID == "" || reply.Kind != "command" || reply.ScopeSize != 1 {
		t.Errorf("reply = %+v, want kind=command scope_size=1", reply)
	}
	// S6-SELF-AUDIT: command_run.invoked writes emitCreated INSIDE CreateTyped.
	assertSelfAudit(t, auditCap, audit.EventCommandRunInvoked, "voyage_id")
}

func TestHumaVoyage_Create_UnknownField_400(t *testing.T) {
	r := humaVoyageRouter(t, strictAllowAll{}, nil, &fakeVoyageStore{}, fakeVoyageCmdResolver{sids: []string{"x"}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/voyages", strings.NewReader(`{"kind":"command","module":"core.cmd.shell","target":{"sids":["node-1.example.com"]},"bogus":1}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (unknown field); body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaVoyage_Create_MissingTarget_422_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaVoyageRouter(t, strictAllowAll{}, auditCap, &fakeVoyageStore{}, fakeVoyageCmdResolver{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/voyages", strings.NewReader(`{"kind":"command","module":"core.cmd.shell"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (missing target); body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit recorded on 422 - the write path must not write")
	}
}

func TestHumaVoyage_Create_RBACDeny_403_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	// strictDenyAll → bare-check errand.run inside resolveCommandScopeErr denies → 403.
	r := humaVoyageRouter(t, strictDenyAll{}, auditCap, &fakeVoyageStore{}, fakeVoyageCmdResolver{sids: []string{"x"}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/voyages", strings.NewReader(voyageCmdBody))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (RBAC-by-kind deny); body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit recorded on 403")
	}
}

// --- Preview (read-like, NoAudit) ---

func TestHumaVoyage_Preview_WireAndNoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	cmd := fakeVoyageCmdResolver{sids: []string{"node-1.example.com", "node-2.example.com"}}
	r := humaVoyageRouter(t, strictAllowAll{}, auditCap, &fakeVoyageStore{}, cmd)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/voyages/preview", strings.NewReader(voyageCmdBody))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var reply struct {
		Kind      string `json:"kind"`
		ScopeSize int    `json:"scope_size"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &reply)
	if reply.Kind != "command" || reply.ScopeSize != 2 {
		t.Errorf("preview reply = %+v, want kind=command scope_size=2", reply)
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("preview recorded audit (%d events) - dry-resolve must not write", len(auditCap.Events()))
	}
}

// --- List (read, CheckPageBounds→400) ---

func TestHumaVoyage_List_Read_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaVoyageRouter(t, strictAllowAll{}, auditCap, &fakeVoyageStore{}, fakeVoyageCmdResolver{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/voyages", http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var reply struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if reply.Items == nil {
		t.Error("items must be [] (non-nil)")
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("READ list recorded audit")
	}
}

func TestHumaVoyage_List_BadOffset_400(t *testing.T) {
	r := humaVoyageRouter(t, strictAllowAll{}, nil, &fakeVoyageStore{}, fakeVoyageCmdResolver{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/voyages?offset=-5", http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (BadOffset CheckPageBounds); body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaVoyage_List_BadLimit_400(t *testing.T) {
	r := humaVoyageRouter(t, strictAllowAll{}, nil, &fakeVoyageStore{}, fakeVoyageCmdResolver{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/voyages?limit=9999", http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (BadLimit CheckPageBounds); body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaVoyage_List_BadStatusEnum_422(t *testing.T) {
	r := humaVoyageRouter(t, strictAllowAll{}, nil, &fakeVoyageStore{}, fakeVoyageCmdResolver{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/voyages?status=bogus", http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (bad status enum); body=%s", rec.Code, rec.Body.String())
	}
}

// --- Get (read, 404 NoAudit) ---

func TestHumaVoyage_Get_NotFound_404(t *testing.T) {
	store := &fakeVoyageStore{} // selectByID=nil → 404
	r := humaVoyageRouter(t, strictAllowAll{}, nil, store, fakeVoyageCmdResolver{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/voyages/"+voyageTestID, http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaVoyage_Get_BadID_422(t *testing.T) {
	r := humaVoyageRouter(t, strictAllowAll{}, nil, &fakeVoyageStore{}, fakeVoyageCmdResolver{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/voyages/not-a-ulid", http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (bad id); body=%s", rec.Code, rec.Body.String())
	}
}

// --- Cancel (self-audit) ---

func TestHumaVoyage_Cancel_WireAndAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	store := &fakeVoyageStore{selectByID: func(id string) pgx.Row {
		return voyageCancelRow{id: id, status: "pending"}
	}}
	r := humaVoyageRouter(t, strictAllowAll{}, auditCap, store, fakeVoyageCmdResolver{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/voyages/"+voyageTestID, http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	var reply struct {
		VoyageID string `json:"voyage_id"`
		Status   string `json:"status"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &reply)
	if reply.VoyageID != voyageTestID || reply.Status != "cancelled" {
		t.Errorf("cancel reply = %+v, want voyage_id=%s status=cancelled", reply, voyageTestID)
	}
	// S6-SELF-AUDIT: command_run.cancelled writes emitCancelled INSIDE CancelTyped.
	assertSelfAudit(t, auditCap, audit.EventCommandRunCancelled, "voyage_id")
}

func TestHumaVoyage_Cancel_NotFound_404_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	store := &fakeVoyageStore{} // selectByID=nil → 404
	r := humaVoyageRouter(t, strictAllowAll{}, auditCap, store, fakeVoyageCmdResolver{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/voyages/"+voyageTestID, http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit recorded on 404 cancel")
	}
}

func TestHumaVoyage_Cancel_BadID_422(t *testing.T) {
	r := humaVoyageRouter(t, strictAllowAll{}, nil, &fakeVoyageStore{}, fakeVoyageCmdResolver{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/voyages/not-a-ulid", http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (bad id); body=%s", rec.Code, rec.Body.String())
	}
}

// --- scenario-kind (create + cancel) — symmetry with command-kind ---

const voyageScenBody = `{"kind":"scenario","scenario_name":"deploy","target":{"incarnations":["web-prod"]}}`

// TestHumaVoyage_Create_Scenario_WireAndAudit — scenario-kind create 202 + scope_size
// (resolver returns 1 incarnation) + S6-SELF-AUDIT scenario_run.started INSIDE
// createScenarioTyped (emitCreated). Symmetry with command-kind (EventCommandRunInvoked):
// both kind branches on the migrated huma layer are inspected.
func TestHumaVoyage_Create_Scenario_WireAndAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	store := &fakeVoyageStore{}
	scen := fakeVoyageScenResolver{incarnations: []string{"web-prod"}}
	r := humaVoyageRouterScen(t, strictAllowAll{}, auditCap, store, fakeVoyageCmdResolver{}, scen)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/voyages", strings.NewReader(voyageScenBody))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	var reply struct {
		VoyageID  string `json:"voyage_id"`
		Kind      string `json:"kind"`
		ScopeSize int    `json:"scope_size"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if reply.VoyageID == "" || reply.Kind != "scenario" || reply.ScopeSize != 1 {
		t.Errorf("reply = %+v, want kind=scenario scope_size=1", reply)
	}
	assertSelfAudit(t, auditCap, audit.EventScenarioRunStarted, "voyage_id")
}

// TestHumaVoyage_Cancel_Scenario_WireAndAudit — scenario-kind cancel 202 + S6-SELF-AUDIT
// scenario_run.cancelled INSIDE CancelTyped (emitCancelled). Symmetry with command-kind
// (EventCommandRunCancelled): both cancel kind branches on the huma layer are inspected.
func TestHumaVoyage_Cancel_Scenario_WireAndAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	store := &fakeVoyageStore{selectByID: func(id string) pgx.Row {
		return voyageCancelRow{id: id, status: "pending", kind: "scenario"}
	}}
	r := humaVoyageRouter(t, strictAllowAll{}, auditCap, store, fakeVoyageCmdResolver{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/voyages/"+voyageTestID, http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	var reply struct {
		VoyageID string `json:"voyage_id"`
		Status   string `json:"status"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &reply)
	if reply.VoyageID != voyageTestID || reply.Status != "cancelled" {
		t.Errorf("cancel reply = %+v, want voyage_id=%s status=cancelled", reply, voyageTestID)
	}
	assertSelfAudit(t, auditCap, audit.EventScenarioRunCancelled, "voyage_id")
}

// --- GOLDEN byte-exact (202 create + 202 cancel) ---

// TestHumaVoyage_Create_GoldenWire — GOLDEN-JSON byte-exact 202-reply create (command-
// kind). voyage_id is non-deterministic (ULID) → normalized to a placeholder; the other keys/
// set/absence of $schema are fixed. voyage is the only domain with MCP-via-httptest,
// where the 202-body byte-exact matters (wire mismatch is caught here).
func TestHumaVoyage_Create_GoldenWire(t *testing.T) {
	store := &fakeVoyageStore{}
	cmd := fakeVoyageCmdResolver{sids: []string{"node-1.example.com"}}
	r := humaVoyageRouter(t, strictAllowAll{}, nil, store, cmd)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/voyages", strings.NewReader(voyageCmdBody))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	got := normalizeVoyageID(t, rec.Body.Bytes())
	const golden = `{"kind":"command","location":"/v1/voyages/ULID","scope_size":1,"status":"pending","voyage_id":"ULID"}`
	if got != golden {
		t.Errorf("GOLDEN wire-drift create-202:\n got  = %s\n want = %s\n(key set/$schema/location changed - check voyageCreateOutput/VoyageCreateReply)", got, golden)
	}
}

// TestHumaVoyage_Cancel_GoldenWire — GOLDEN-JSON byte-exact 202-reply cancel. voyage_id
// is fixed (path-{id} → deterministic). Pins the key set (voyage_id/status) and
// the absence of $schema/extra fields.
func TestHumaVoyage_Cancel_GoldenWire(t *testing.T) {
	store := &fakeVoyageStore{selectByID: func(id string) pgx.Row {
		return voyageCancelRow{id: id, status: "pending"}
	}}
	r := humaVoyageRouter(t, strictAllowAll{}, nil, store, fakeVoyageCmdResolver{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/voyages/"+voyageTestID, http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	got := normalizeJSONKeys(t, rec.Body.Bytes())
	const golden = `{"status":"cancelled","voyage_id":"01HZ0000000000000000000000"}`
	if got != golden {
		t.Errorf("GOLDEN wire-drift cancel-202:\n got  = %s\n want = %s\n(key set/$schema changed - check voyageCancelOutput/VoyageCancelReply)", got, golden)
	}
}

// normalizeVoyageID replaces the non-deterministic voyage_id (ULID) and the location tail with
// the placeholder "ULID" and re-pours through a map → sorted-marshal (golden byte-exact;
// the placeholder has no special chars — json.Marshal does not HTML-escape).
func normalizeVoyageID(t *testing.T, raw []byte) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("reply is not a JSON-object: %v; raw=%s", err, raw)
	}
	if _, ok := m["voyage_id"]; ok {
		m["voyage_id"] = "ULID"
	}
	if _, ok := m["location"]; ok {
		m["location"] = "/v1/voyages/ULID"
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	return string(out)
}
