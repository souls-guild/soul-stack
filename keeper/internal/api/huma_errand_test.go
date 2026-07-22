package api

// Guard tests for ROLLOUT-BATCH-2c turning the ERRAND domain (list + get + cancel) onto huma
// full-typed (ADR-054 §Pattern, references augur/audit-endpoint/role). list — read-with-
// typed-query, get — read-with-path (200 terminal / 202 running), cancel — WRITE+AUDIT
// (variant B, huma audit middleware; event errand.cancelled). They prove the cluster
// invariants on top of chi:
//
//   - wire/golden: list 200 envelope (status a bare enum string, started_at second-precision
//     UTC); get 200 ErrandResult terminal; get 202 ErrandAccepted running; cancel 204
//     empty (byte-exact);
//   - bad pagination → 400 (BadOffset/BadLimit is where we caught the blocker!); bad started_after
//     date-time → 400; bad status enum → 422; bad sid format → 422; not-found → 404;
//     terminal → 409; RBAC-deny → 403;
//   - S6-GUARD on cancel (the only write): the full huma wiring writes an audit event with
//     a NON-EMPTY payload + the CORRECT event-type errand.cancelled on 204 and does NOT write on
//     404/409/403. dispatcher self-audit is DISABLED (Audit=nil) — the middleware path is
//     exactly what is tested.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	"github.com/souls-guild/soul-stack/keeper/internal/errand"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// errandAt — fixed started_at/ttl_at that all errand success paths return
// (deterministic golden wire). Second precision (wire — RFC3339 seconds).
var errandAt = time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)

// hErrandPool — a narrow ExecQueryRower mock for errand.Store (List/Get).
// Classifies SQL by substring and returns a deterministic success outcome.
type hErrandPool struct {
	getStatus  string // status of the GET row (running → 202, otherwise 200); "" → ErrNotFound
	listRows   [][]any
	getMissing bool
}

func (p *hErrandPool) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, &hErrandErr{"hErrandPool: unexpected Exec"}
}

func (p *hErrandPool) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "FROM errands") && strings.Contains(sql, "errand_id = $1"):
		if p.getMissing {
			return hErrandRow{err: pgx.ErrNoRows}
		}
		return hErrandRow{values: errandScanRow("ERR-01", p.getStatus)}
	case strings.Contains(sql, "COUNT(*) FROM errands"):
		return hErrandRow{values: []any{len(p.listRows)}}
	}
	return hErrandRow{err: &hErrandErr{"hErrandPool: unexpected QueryRow: " + sql}}
}

func (p *hErrandPool) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "FROM errands") && strings.Contains(sql, "ORDER BY") {
		return &hErrandRows{rows: p.listRows}, nil
	}
	return nil, &hErrandErr{"hErrandPool: unexpected Query: " + sql}
}

// errandScanRow — scanRow columns: errand_id, sid, module, input(jsonb), status,
// exit_code, stdout, stderr, stdout_truncated, stderr_truncated, duration_ms,
// error_message, output(jsonb), started_by_aid, started_by_kid, started_at,
// finished_at, ttl_at. success terminal: exit_code=0, finished_at=errandAt.
func errandScanRow(id, status string) []any {
	var exit *int32
	var finished *time.Time
	if status != "running" {
		z := int32(0)
		exit = &z
		fin := errandAt
		finished = &fin
	}
	return []any{
		id, "host.test", "core.cmd.shell", []byte(nil), status,
		exit, "", "", false, false,
		(*int64)(nil), "", []byte(nil), "archon-alice", "kid-1",
		errandAt, finished, errandAt,
	}
}

type hErrandErr struct{ s string }

func (e *hErrandErr) Error() string { return e.s }

// hErrandRow — a staticRow for errand columns.
type hErrandRow struct {
	values []any
	err    error
}

func (r hErrandRow) Scan(dest ...any) error {
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
		case **int32:
			if r.values[i] == nil {
				*dd = nil
			} else {
				*dd = r.values[i].(*int32)
			}
		case **int64:
			if r.values[i] == nil {
				*dd = nil
			} else {
				*dd = r.values[i].(*int64)
			}
		case **time.Time:
			if r.values[i] == nil {
				*dd = nil
			} else {
				*dd = r.values[i].(*time.Time)
			}
		}
	}
	return nil
}

type hErrandRows struct {
	rows [][]any
	idx  int
}

func (r *hErrandRows) Next() bool { r.idx++; return r.idx <= len(r.rows) }
func (r *hErrandRows) Scan(dest ...any) error {
	return hErrandRow{values: r.rows[r.idx-1]}.Scan(dest...)
}
func (r *hErrandRows) Err() error                                   { return nil }
func (r *hErrandRows) Close()                                       {}
func (r *hErrandRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *hErrandRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *hErrandRows) Values() ([]any, error)                       { return nil, nil }
func (r *hErrandRows) RawValues() [][]byte                          { return nil }
func (r *hErrandRows) Conn() *pgx.Conn                              { return nil }

// --- cancel-flow fakes (in-memory dispatcher, parity handlers/errand_test.go) ---

type hErrandCancelStore struct {
	mu   sync.Mutex
	rows map[string]errand.Row
}

func newHErrandCancelStore(statuses map[string]errand.Status) *hErrandCancelStore {
	s := &hErrandCancelStore{rows: map[string]errand.Row{}}
	now := errandAt
	for id, st := range statuses {
		s.rows[id] = errand.Row{
			ErrandID: id, SID: "host.test", Module: "core.cmd.shell", Status: st,
			StartedByAID: "archon-alice", StartedByKID: "kid-1",
			StartedAt: now, TTLAt: now.Add(time.Hour),
		}
	}
	return s
}

func (s *hErrandCancelStore) Insert(context.Context, errand.Row) error { return nil }
func (s *hErrandCancelStore) MarkTerminal(_ context.Context, id string, upd errand.TerminalUpdate) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	if !ok || r.Status != errand.StatusRunning {
		return false, nil
	}
	r.Status = upd.Status
	s.rows[id] = r
	return true, nil
}
func (s *hErrandCancelStore) SweepOrphanRunning(context.Context, string, time.Duration, string) ([]string, error) {
	return nil, nil
}
func (s *hErrandCancelStore) Get(_ context.Context, id string) (*errand.Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	if !ok {
		return nil, errand.ErrNotFound
	}
	cp := r
	return &cp, nil
}

type hErrandOutbound struct{}

func (hErrandOutbound) SendErrand(context.Context, string, *keeperv1.ErrandRequest) error { return nil }
func (hErrandOutbound) PublishErrand(context.Context, string, *keeperv1.ErrandRequest) error {
	return nil
}
func (hErrandOutbound) SendCancelErrand(context.Context, string, string) error    { return nil }
func (hErrandOutbound) PublishCancelErrand(context.Context, string, string) error { return nil }

type hErrandBus struct{}

func (b hErrandBus) Subscribe(ctx context.Context, applyID string) <-chan applybus.Event {
	return b.SubscribeWithBridge(ctx, applyID, true)
}

func (hErrandBus) SubscribeWithBridge(context.Context, string, bool) <-chan applybus.Event {
	return make(chan applybus.Event)
}

// buildHErrandDispatcher — in-memory Dispatcher. Audit=nil → the dispatcher does NOT write
// self-audit (the S6-guard checks the middleware path specifically, not a duplicate).
func buildHErrandDispatcher(t *testing.T, statuses map[string]errand.Status) *errand.Dispatcher {
	t.Helper()
	d, err := errand.NewDispatcher(errand.Deps{
		Store:    newHErrandCancelStore(statuses),
		Outbound: hErrandOutbound{},
		ApplyBus: hErrandBus{},
		Logger:   slog.New(slog.NewJSONHandler(io.Discard, nil)),
		KID:      "kid-1",
	})
	if err != nil {
		t.Fatalf("errand.NewDispatcher: %v", err)
	}
	return d
}

// humaErrandRouter assembles a chi router with ALL errand routes via huma —
// the production wiring from router.go: RequirePermission(errand.<action>) + (cancel)
// huma audit middleware variant B + huma operation. dispatcher/store are injected
// separately (read via Store, cancel via Dispatcher). injectClaims replaces
// RequireJWT.
func humaErrandRouter(t *testing.T, enforcer apimiddleware.PermissionChecker, auditW audit.Writer, store *errand.Store, dispatcher *errand.Dispatcher) *chi.Mux {
	t.Helper()
	installHumaErrorOverride()
	errandH := handlers.NewErrandHandler(dispatcher, store, nil)

	r := chi.NewRouter()
	injectClaims := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := apimiddleware.InjectClaimsForTest(req.Context(), &keeperjwt.Claims{Subject: "archon-alice"})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
	r.Route("/v1", func(r chi.Router) {
		r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "errand", "list", apimiddleware.NoSelector)).Group(func(r chi.Router) {
			registerHumaErrandList(newHumaCadenceAPI(r), errandH)
		})
		r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "errand", "list", apimiddleware.NoSelector)).Group(func(r chi.Router) {
			registerHumaErrandGet(newHumaCadenceAPI(r), errandH)
		})
		r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "errand", "cancel", apimiddleware.NoSelector)).Group(func(r chi.Router) {
			registerHumaErrandCancel(newHumaErrandAPI(r, auditW, audit.EventTypeErrandCancelled, nil), errandH)
		})
	})
	return r
}

func hErrandStore(pool *hErrandPool) *errand.Store { return errand.NewStore(pool) }

// === ERRAND LIST (READ with typed query, no audit) ===

func TestHumaErrand_List_GoldenWire(t *testing.T) {
	pool := &hErrandPool{listRows: [][]any{errandScanRow("ERR-01", "success")}}
	r := humaErrandRouter(t, strictAllowAll{}, nil, hErrandStore(pool), nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/errands", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply is not a JSON-object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"items":[{"errand_id":"ERR-01","exit_code":0,"finished_at":"2026-06-13T10:00:00Z","module":"core.cmd.shell","sid":"host.test","started_at":"2026-06-13T10:00:00Z","started_by_aid":"archon-alice","status":"success"}],"limit":50,"offset":0,"total":1}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-drift errand.list:\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaErrand_List_GoldenEmpty(t *testing.T) {
	r := humaErrandRouter(t, strictAllowAll{}, nil, hErrandStore(&hErrandPool{}), nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/errands", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	const golden = `{"items":[],"limit":50,"offset":0,"total":0}`
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	out, _ := json.Marshal(m)
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-drift errand.list (empty): got=%q want=%q", got, golden)
	}
}

func TestHumaErrand_List_BadOffset_400(t *testing.T) {
	r := humaErrandRouter(t, strictAllowAll{}, nil, hErrandStore(&hErrandPool{}), nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/errands?offset=-1", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (offset<0 -> CheckPageBounds 400, blocker!); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

func TestHumaErrand_List_BadLimit_400(t *testing.T) {
	r := humaErrandRouter(t, strictAllowAll{}, nil, hErrandStore(&hErrandPool{}), nil)
	for _, c := range []string{"/v1/errands?limit=0", "/v1/errands?limit=1001"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, c, nil)
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (out-of-range limit -> CheckPageBounds 400, blocker!); body=%s", c, rec.Code, rec.Body.String())
			continue
		}
		assertHumaProblem(t, rec, problem.TypeMalformedRequest)
	}
}

func TestHumaErrand_List_BadStartedAfter_400(t *testing.T) {
	r := humaErrandRouter(t, strictAllowAll{}, nil, hErrandStore(&hErrandPool{}), nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/errands?started_after=notadate", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (bad started_after date-time → huma-bind 400); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

func TestHumaErrand_List_BadStatus_422(t *testing.T) {
	r := humaErrandRouter(t, strictAllowAll{}, nil, hErrandStore(&hErrandPool{}), nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/errands?status=bogus", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (bad status enum); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

func TestHumaErrand_List_BadSID_422(t *testing.T) {
	r := humaErrandRouter(t, strictAllowAll{}, nil, hErrandStore(&hErrandPool{}), nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/errands?sid=BAD_SID", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (bad sid format); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

func TestHumaErrand_List_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaErrandRouter(t, strictAllowAll{}, auditCap, hErrandStore(&hErrandPool{}), nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/errands", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("READ route errand.list recorded audit (%d events)", len(auditCap.Events()))
	}
}

func TestHumaErrand_List_RBACDeny_403(t *testing.T) {
	r := humaErrandRouter(t, strictDenyAll{}, nil, hErrandStore(&hErrandPool{}), nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/errands", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// === ERRAND GET (READ with path, 200 terminal / 202 running) ===

func TestHumaErrand_Get_GoldenWire_Terminal(t *testing.T) {
	r := humaErrandRouter(t, strictAllowAll{}, nil, hErrandStore(&hErrandPool{getStatus: "success"}), nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/errands/ERR-01", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply is not a JSON-object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"errand_id":"ERR-01","exit_code":0,"finished_at":"2026-06-13T10:00:00Z","module":"core.cmd.shell","sid":"host.test","started_at":"2026-06-13T10:00:00Z","started_by_aid":"archon-alice","status":"success"}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-drift errand.get (terminal):\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaErrand_Get_Running_202(t *testing.T) {
	r := humaErrandRouter(t, strictAllowAll{}, nil, hErrandStore(&hErrandPool{getStatus: "running"}), nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/errands/ERR-01", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (running poll); body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply is not a JSON-object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"errand_id":"ERR-01","status":"running"}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-drift errand.get (running 202):\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaErrand_Get_NotFound_404(t *testing.T) {
	r := humaErrandRouter(t, strictAllowAll{}, nil, hErrandStore(&hErrandPool{getMissing: true}), nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/errands/GHOST", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeNotFound)
}

// === ERRAND CANCEL (WRITE+AUDIT errand.cancelled) ===

func TestHumaErrand_Cancel_204(t *testing.T) {
	d := buildHErrandDispatcher(t, map[string]errand.Status{"ERR-OK": errand.StatusRunning})
	r := humaErrandRouter(t, strictAllowAll{}, nil, nil, d)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/errands/ERR-OK", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "" {
		t.Errorf("204-body errand.cancel must be EMPTY, got %q", body)
	}
}

func TestHumaErrand_Cancel_NotFound_404(t *testing.T) {
	d := buildHErrandDispatcher(t, nil)
	r := humaErrandRouter(t, strictAllowAll{}, nil, nil, d)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/errands/GHOST", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeNotFound)
}

func TestHumaErrand_Cancel_Terminal_409(t *testing.T) {
	d := buildHErrandDispatcher(t, map[string]errand.Status{"ERR-DONE": errand.StatusSuccess})
	r := humaErrandRouter(t, strictAllowAll{}, nil, nil, d)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/errands/ERR-DONE", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (terminal); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeErrandNotCancellable)
}

func TestHumaErrand_Cancel_RBACDeny_403(t *testing.T) {
	d := buildHErrandDispatcher(t, map[string]errand.Status{"ERR-OK": errand.StatusRunning})
	r := humaErrandRouter(t, strictDenyAll{}, nil, nil, d)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/errands/ERR-OK", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaAudit_ErrandCancel_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	d := buildHErrandDispatcher(t, map[string]errand.Status{"ERR-OK": errand.StatusRunning})
	r := humaErrandRouter(t, strictAllowAll{}, auditCap, nil, d)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/errands/ERR-OK", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventTypeErrandCancelled, map[string]any{"errand_id": "ERR-OK"})
}

func TestHumaAudit_ErrandCancel_NoAudit_OnNotFound(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	d := buildHErrandDispatcher(t, nil)
	r := humaErrandRouter(t, strictAllowAll{}, auditCap, nil, d)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/errands/GHOST", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit recorded on 404 errand.cancel (%d events)", len(auditCap.Events()))
	}
}

func TestHumaAudit_ErrandCancel_NoAudit_OnTerminal(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	d := buildHErrandDispatcher(t, map[string]errand.Status{"ERR-DONE": errand.StatusSuccess})
	r := humaErrandRouter(t, strictAllowAll{}, auditCap, nil, d)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/errands/ERR-DONE", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit recorded on 409 errand.cancel (%d events)", len(auditCap.Events()))
	}
}

// === OpenAPI fragment: ALL errand operations from FULL-TYPED Go types ===

func TestHumaErrand_OpenAPIFragment_3_1(t *testing.T) {
	frag, err := HumaErrandSpecYAML()
	if err != nil {
		t.Fatalf("HumaErrandSpecYAML: %v", err)
	}
	if !strings.Contains(frag, "openapi: 3.1.0") {
		t.Errorf("huma fragment does not carry `openapi: 3.1.0`:\n%s", frag)
	}
	for _, want := range []string{"listErrands", "getErrand", "cancelErrand", "started_after"} {
		if !strings.Contains(frag, want) {
			t.Errorf("OpenAPI fragment does not contain %q:\n%s", want, frag)
		}
	}
	if strings.Contains(frag, "octet-stream") {
		t.Errorf("OpenAPI fragment carries application/octet-stream:\n%s", frag)
	}
}
