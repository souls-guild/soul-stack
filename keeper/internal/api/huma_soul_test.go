package api

// Guard tests for ROLLOUT-BATCH-2e turning the SOUL domain onto huma full-typed (ADR-054 §Pattern,
// references role/operator + audit-endpoint). create/coven-assign/issue-token/ssh-target —
// WRITE+AUDIT (variant B); list/get/soulprint/history — read (no audit). ErrandExec is NOT in
// this batch (strict). They prove the invariants on top of chi:
//
//   - wire/golden: create 201 SoulCreateReply (byte-exact); issue-token 200 (token+expires);
//     ssh-target 200 snapshot; coven-assign 200 (custom XOR);
//   - create unknown-field → 400; missing-required → 422; list bad-offset → 400; list/coven bad-
//     status-enum → 422; history bad-offset → 400; RBAC-deny → 403;
//   - S6-GUARD on EVERY write (create/coven-assign/issue-token/ssh-target): the full huma
//     wiring writes the CORRECT event-type with a NON-EMPTY payload on 2xx and does NOT write on 403/400/422;
//   - reads (get/list/soulprint/history) → NoAudit.
//
// Deep business logic (keyset/scope-eval/bulk-chunks/partial/transaction-rollback) is covered by
// handlers/soul_test.go via the SAME *Typed functions (the huma route calls them too); here — the huma layer.

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// hSoulAt — fixed registered_at/expires_at for a deterministic golden.
var hSoulAt = time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)

// hSoulPool — a compact mock of [handlers.SoulPool] for the huma test. Supports Create (Insert
// souls + Insert bootstrap_token + Commit), IssueToken (SelectBySID + Insert), UpdateSshTarget,
// List (COUNT + Query). Deep scenarios — handlers/soul_test.go.
type hSoulPool struct {
	existing  *soul.Soul // SelectBySID → row (issue-token/get); nil → ErrNoRows
	insertErr error
	listSouls []*soul.Soul
}

func (p *hSoulPool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return &hSoulTx{p: p}, nil
}

func (p *hSoulPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("hSoulPool.Exec: unexpected")
}

func (p *hSoulPool) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "INSERT INTO souls"):
		if p.insertErr != nil {
			return hSoulErrRow{err: p.insertErr}
		}
		return hSoulStaticRow{vals: []any{hSoulAt, hSoulAt}}
	case strings.Contains(sql, "INSERT INTO bootstrap_tokens"):
		return hSoulStaticRow{vals: []any{"token-uuid", hSoulAt}}
	case strings.Contains(sql, "FROM souls") && strings.Contains(sql, "WHERE sid = $1"):
		if p.existing == nil {
			return hSoulErrRow{err: pgx.ErrNoRows}
		}
		s := p.existing
		var createdByAID any
		if s.CreatedByAID != nil {
			createdByAID = *s.CreatedByAID
		}
		return hSoulStaticRow{vals: []any{
			s.SID, string(s.Transport), string(s.Status), s.Coven,
			s.RegisteredAt, nil, nil, createdByAID, nil, nil,
		}}
	case strings.Contains(sql, "COUNT(*) FROM souls"):
		return hSoulStaticRow{vals: []any{len(p.listSouls)}}
	case strings.Contains(sql, "UPDATE souls") && strings.Contains(sql, "ssh_target"):
		var sid string
		if len(args) > 0 {
			sid, _ = args[0].(string)
		}
		return hSoulStaticRow{vals: []any{sid}}
	}
	return hSoulErrRow{err: errors.New("hSoulPool.QueryRow: unexpected SQL: " + sql)}
}

func (p *hSoulPool) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	rows := make([][]any, 0, len(p.listSouls))
	for _, s := range p.listSouls {
		var createdByAID any
		if s.CreatedByAID != nil {
			createdByAID = *s.CreatedByAID
		}
		// scanSoul order: sid, transport, status, coven, registered_at, last_seen_at,
		// last_seen_by_kid, created_by_aid, requested_at, note.
		rows = append(rows, []any{
			s.SID, string(s.Transport), string(s.Status), s.Coven,
			s.RegisteredAt, nil, nil, createdByAID, nil, nil,
		})
	}
	return &hSoulRows{rows: rows}, nil
}

type hSoulTx struct{ p *hSoulPool }

func (t *hSoulTx) Begin(context.Context) (pgx.Tx, error) { return t, nil }
func (t *hSoulTx) Commit(context.Context) error          { return nil }
func (t *hSoulTx) Rollback(context.Context) error        { return nil }
func (t *hSoulTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 0, nil
}
func (t *hSoulTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults { return nil }
func (t *hSoulTx) LargeObjects() pgx.LargeObjects                         { panic("unexpected") }
func (t *hSoulTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	return nil, nil
}
func (t *hSoulTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag("OK"), nil
}
func (t *hSoulTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.p.Query(ctx, sql, args...)
}
func (t *hSoulTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.p.QueryRow(ctx, sql, args...)
}
func (t *hSoulTx) Conn() *pgx.Conn { return nil }

type hSoulErrRow struct{ err error }

func (r hSoulErrRow) Scan(...any) error { return r.err }

type hSoulStaticRow struct{ vals []any }

func (r hSoulStaticRow) Scan(dest ...any) error {
	for i, d := range dest {
		if i >= len(r.vals) {
			break
		}
		hSoulAssignScan(d, r.vals[i])
	}
	return nil
}

type hSoulRows struct {
	rows [][]any
	idx  int
}

func (r *hSoulRows) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}
func (r *hSoulRows) Scan(dest ...any) error {
	row := r.rows[r.idx-1]
	for i, d := range dest {
		if i >= len(row) {
			break
		}
		hSoulAssignScan(d, row[i])
	}
	return nil
}
func (r *hSoulRows) Err() error                                   { return nil }
func (r *hSoulRows) Close()                                       {}
func (r *hSoulRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *hSoulRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *hSoulRows) Values() ([]any, error)                       { return nil, nil }
func (r *hSoulRows) RawValues() [][]byte                          { return nil }
func (r *hSoulRows) Conn() *pgx.Conn                              { return nil }

// assignScan assigns a value to a pgx dest by type (a narrow set of souls columns).
func hSoulAssignScan(d, v any) {
	switch dst := d.(type) {
	case *string:
		if v != nil {
			*dst = v.(string)
		}
	case *int:
		if v != nil {
			*dst = v.(int)
		}
	case *time.Time:
		if v != nil {
			*dst = v.(time.Time)
		}
	case *[]string:
		if v != nil {
			*dst = v.([]string)
		}
	case **string:
		if v == nil {
			*dst = nil
			return
		}
		s := v.(string)
		*dst = &s
	case **time.Time:
		if v == nil {
			*dst = nil
			return
		}
		t := v.(time.Time)
		*dst = &t
	}
}

// hSoulScoper — a mock of [handlers.PurviewResolver]: unrestricted → all souls visible (offset-fast-
// path, no keyset). Sufficient for the huma test (scope-eval is covered by handlers/soul_test.go).
type hSoulScoper struct{ unrestricted bool }

func (s hSoulScoper) ResolvePurview(string, string, string) rbac.Purview {
	return rbac.Purview{Unrestricted: s.unrestricted}
}

func newHSoulHandler(pool *hSoulPool) *handlers.SoulHandler {
	return handlers.NewSoulHandler(pool, hSoulScoper{unrestricted: true}, nil, nil)
}

// hSoulEnforcer implements both [apimiddleware.PermissionChecker] (write routes, RequirePermission)
// and [apimiddleware.ActionHolder] (read routes, RequireAction existence-gate). allow=false →
// 403 on both paths.
type hSoulEnforcer struct{ allow bool }

func (e hSoulEnforcer) Check(string, string, string, map[string]string) error {
	if e.allow {
		return nil
	}
	return rbac.ErrPermissionDenied
}

func (e hSoulEnforcer) HoldsAction(string, string, string) bool { return e.allow }

// humaSoulRouter assembles a chi router with ALL soul routes via huma (except exec) —
// the production wiring from router.go. injectClaims replaces RequireJWT.
func humaSoulRouter(t *testing.T, enforcer hSoulEnforcer, auditW audit.Writer, soulH *handlers.SoulHandler) *chi.Mux {
	t.Helper()
	installHumaErrorOverride()
	r := chi.NewRouter()
	injectClaims := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := apimiddleware.InjectClaimsForTest(req.Context(), &keeperjwt.Claims{Subject: "archon-alice"})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
	r.Route("/v1", func(r chi.Router) {
		r.Route("/souls", func(r chi.Router) {
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "soul", "create", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaSoulCreate(newHumaSoulAPI(r, auditW, audit.EventSoulCreated, nil), soulH)
			})
			r.With(injectClaims, apimiddleware.RequireAction(enforcer, "soul", "list")).Group(func(r chi.Router) {
				registerHumaSoulList(newHumaCadenceAPI(r), soulH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "soul", "coven-assign", handlers.SoulCovenLabelSelector)).Group(func(r chi.Router) {
				registerHumaSoulCovenAssign(newHumaSoulAPI(r, auditW, audit.EventSoulCovenChanged, nil), soulH)
			})
			r.With(injectClaims, apimiddleware.RequireAction(enforcer, "soul", "list")).Group(func(r chi.Router) {
				api := newHumaCadenceAPI(r)
				registerHumaSoulGet(api, soulH)
				registerHumaSoulSoulprint(api, soulH)
				registerHumaSoulHistory(api, soulH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "soul", "issue-token", handlers.SoulSIDSelector)).Group(func(r chi.Router) {
				registerHumaSoulIssueToken(newHumaSoulAPI(r, auditW, audit.EventSoulTokenIssued, nil), soulH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "soul", "ssh-target-update", handlers.SoulSIDSelector)).Group(func(r chi.Router) {
				registerHumaSoulSshTarget(newHumaSoulAPI(r, auditW, audit.EventSoulSshTargetUpdated, nil), soulH)
			})
		})
	})
	return r
}

func agentSoul() *soul.Soul {
	aid := "archon-alice"
	return &soul.Soul{
		SID: "host-1.example.com", Transport: soul.TransportAgent, Status: soul.StatusPending,
		Coven: []string{"web"}, RegisteredAt: hSoulAt, CreatedByAID: &aid,
	}
}

// === CREATE (WRITE+AUDIT soul.created) ===

func TestHumaSoul_Create_GoldenWire(t *testing.T) {
	r := humaSoulRouter(t, hSoulEnforcer{allow: true}, nil, newHSoulHandler(&hSoulPool{}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/souls",
		strings.NewReader(`{"sid":"host-1.example.com","transport":"ssh","covens":["web"]}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	// transport=ssh → no bootstrap_token; date-time seconds.
	const golden = `{"covens":["web"],"created_by_aid":"archon-alice","registered_at":"2026-06-13T12:00:00Z","sid":"host-1.example.com","status":"pending","transport":"ssh"}`
	if got := remarshalSorted(t, rec.Body.Bytes()); got != golden {
		t.Errorf("GOLDEN wire-drift soul.create:\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaSoul_Create_UnknownField_400(t *testing.T) {
	r := humaSoulRouter(t, hSoulEnforcer{allow: true}, nil, newHSoulHandler(&hSoulPool{}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/souls",
		strings.NewReader(`{"sid":"h.example.com","transport":"ssh","bogus":1}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

func TestHumaSoul_Create_MissingRequired_422(t *testing.T) {
	r := humaSoulRouter(t, hSoulEnforcer{allow: true}, nil, newHSoulHandler(&hSoulPool{}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/souls", strings.NewReader(`{"transport":"ssh"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

func TestHumaSoul_Create_RBACDeny_403(t *testing.T) {
	r := humaSoulRouter(t, hSoulEnforcer{allow: false}, nil, newHSoulHandler(&hSoulPool{}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/souls", strings.NewReader(`{"sid":"h.example.com","transport":"ssh"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaAudit_SoulCreate_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaSoulRouter(t, hSoulEnforcer{allow: true}, auditCap, newHSoulHandler(&hSoulPool{}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/souls",
		strings.NewReader(`{"sid":"host-1.example.com","transport":"ssh","covens":["web"]}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventSoulCreated, map[string]any{"sid": "host-1.example.com"})
}

func TestHumaAudit_SoulCreate_NoAudit_OnRBACDeny(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaSoulRouter(t, hSoulEnforcer{allow: false}, auditCap, newHSoulHandler(&hSoulPool{}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/souls", strings.NewReader(`{"sid":"h.example.com","transport":"ssh"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit recorded on RBAC-deny soul.create (%d events)", len(auditCap.Events()))
	}
}

func TestHumaAudit_SoulCreate_NoAudit_OnValidationFail(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaSoulRouter(t, hSoulEnforcer{allow: true}, auditCap, newHSoulHandler(&hSoulPool{}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/souls", strings.NewReader(`{"transport":"ssh"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit recorded on 422 soul.create (%d events)", len(auditCap.Events()))
	}
}

// === ISSUE-TOKEN (WRITE+AUDIT soul.token-issued, 200+body) ===

func TestHumaSoul_IssueToken_GoldenWire(t *testing.T) {
	r := humaSoulRouter(t, hSoulEnforcer{allow: true}, nil, newHSoulHandler(&hSoulPool{existing: agentSoul()}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/souls/host-1.example.com/issue-token", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply not JSON: %v; body=%s", err, rec.Body.String())
	}
	if m["sid"] != "host-1.example.com" || m["bootstrap_token"] == "" || m["expires_at"] == nil {
		t.Errorf("issue-token 200-body: %v (expected sid+bootstrap_token+expires_at)", m)
	}
}

func TestHumaAudit_SoulIssueToken_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaSoulRouter(t, hSoulEnforcer{allow: true}, auditCap, newHSoulHandler(&hSoulPool{existing: agentSoul()}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/souls/host-1.example.com/issue-token", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventSoulTokenIssued, map[string]any{"sid": "host-1.example.com"})
}

func TestHumaAudit_SoulIssueToken_NoAudit_OnRBACDeny(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaSoulRouter(t, hSoulEnforcer{allow: false}, auditCap, newHSoulHandler(&hSoulPool{existing: agentSoul()}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/souls/host-1.example.com/issue-token", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit recorded on RBAC-deny soul.issue-token (%d events)", len(auditCap.Events()))
	}
}

// === SSH-TARGET (WRITE+AUDIT soul.ssh-target.updated, 200+body) ===

func TestHumaSoul_SshTarget_GoldenWire(t *testing.T) {
	r := humaSoulRouter(t, hSoulEnforcer{allow: true}, nil, newHSoulHandler(&hSoulPool{existing: agentSoul()}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/souls/host-1.example.com/ssh-target",
		strings.NewReader(`{"ssh_port":22,"ssh_user":"deploy","soul_path":"/opt/soul"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	const golden = `{"sid":"host-1.example.com","ssh_target":{"soul_path":"/opt/soul","ssh_port":22,"ssh_user":"deploy"}}`
	if got := remarshalSorted(t, rec.Body.Bytes()); got != remarshalSorted(t, []byte(golden)) {
		t.Errorf("GOLDEN wire-drift soul.ssh-target:\n got  = %s\n want = %s", got, remarshalSorted(t, []byte(golden)))
	}
}

func TestHumaSoul_SshTarget_BadPort_422(t *testing.T) {
	r := humaSoulRouter(t, hSoulEnforcer{allow: true}, nil, newHSoulHandler(&hSoulPool{existing: agentSoul()}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/souls/host-1.example.com/ssh-target",
		strings.NewReader(`{"ssh_port":99999,"ssh_user":"deploy","soul_path":"/opt/soul"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

func TestHumaAudit_SoulSshTarget_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaSoulRouter(t, hSoulEnforcer{allow: true}, auditCap, newHSoulHandler(&hSoulPool{existing: agentSoul()}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/souls/host-1.example.com/ssh-target",
		strings.NewReader(`{"ssh_port":22,"ssh_user":"deploy","soul_path":"/opt/soul"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventSoulSshTargetUpdated, map[string]any{"sid": "host-1.example.com"})
}

// === COVEN-ASSIGN (WRITE+AUDIT soul.coven-changed) — dry_run (no bulk-chunk plan) ===

func TestHumaSoul_CovenAssign_DryRunGolden(t *testing.T) {
	r := humaSoulRouter(t, hSoulEnforcer{allow: true}, nil, newHSoulHandler(&hSoulPool{}))
	rec := httptest.NewRecorder()
	// dry_run + unrestricted scope → CountBulkMatched (COUNT) without UPDATE. append-mode.
	req := httptest.NewRequest(http.MethodPost, "/v1/souls/coven",
		strings.NewReader(`{"mode":"append","label":"prod","dry_run":true,"selector":{"all":true}}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply not JSON: %v; body=%s", err, rec.Body.String())
	}
	// custom MarshalJSON XOR: append → label (not labels), dry_run:true.
	if m["mode"] != "append" || m["label"] != "prod" || m["dry_run"] != true {
		t.Errorf("coven-assign dry_run 200-body XOR-form incorrect: %v", m)
	}
	if _, hasLabels := m["labels"]; hasLabels {
		t.Errorf("append-mode must not carry labels: %v", m)
	}
}

func TestHumaSoul_CovenAssign_BadMode_422(t *testing.T) {
	r := humaSoulRouter(t, hSoulEnforcer{allow: true}, nil, newHSoulHandler(&hSoulPool{}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/souls/coven",
		strings.NewReader(`{"mode":"weird","label":"x","selector":{"all":true}}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

func TestHumaAudit_SoulCovenAssign_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaSoulRouter(t, hSoulEnforcer{allow: true}, auditCap, newHSoulHandler(&hSoulPool{}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/souls/coven",
		strings.NewReader(`{"mode":"append","label":"prod","dry_run":true,"selector":{"all":true}}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventSoulCovenChanged, map[string]any{"mode": "append"})
}

// === LIST (READ with typed query, no audit) ===

func TestHumaSoul_List_BadOffset_400(t *testing.T) {
	r := humaSoulRouter(t, hSoulEnforcer{allow: true}, nil, newHSoulHandler(&hSoulPool{}))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/souls?offset=-1", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

func TestHumaSoul_List_BadLimit_400(t *testing.T) {
	r := humaSoulRouter(t, hSoulEnforcer{allow: true}, nil, newHSoulHandler(&hSoulPool{}))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/souls?limit=99999", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

func TestHumaSoul_List_BadStatusEnum_422(t *testing.T) {
	r := humaSoulRouter(t, hSoulEnforcer{allow: true}, nil, newHSoulHandler(&hSoulPool{}))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/souls?status=weird", nil))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

func TestHumaSoul_List_OffsetCursorConflict_422(t *testing.T) {
	r := humaSoulRouter(t, hSoulEnforcer{allow: true}, nil, newHSoulHandler(&hSoulPool{}))
	rec := httptest.NewRecorder()
	// offset>0 + a VALID cursor → 422 (two paginations at once). A broken cursor would give 400
	// (decode-fail BEFORE the conflict check), so we encode a real keyset cursor.
	enc := sharedapi.EncodeKeysetCursor(sharedapi.KeysetCursor{RegisteredAt: hSoulAt, SID: "host-1.example.com"})
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/souls?offset=5&cursor="+enc, nil))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

// TestHumaSoul_List_BadCursor_400 — a broken keyset cursor (DecodeKeysetCursor fail)
// → 400 TypeMalformedRequest. Covers the soulParsePage branch (decode-fail BEFORE the
// conflict check); symmetric to TestHumaSoul_List_OffsetCursorConflict_422, but without
// offset — a pure decode-fail. "not-base64!" does not decode as a keyset cursor.
func TestHumaSoul_List_BadCursor_400(t *testing.T) {
	r := humaSoulRouter(t, hSoulEnforcer{allow: true}, nil, newHSoulHandler(&hSoulPool{}))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/souls?cursor=not-base64!", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (broken cursor → decode-fail); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

func TestHumaSoul_List_RBACDeny_403(t *testing.T) {
	r := humaSoulRouter(t, hSoulEnforcer{allow: false}, nil, newHSoulHandler(&hSoulPool{}))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/souls", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaSoul_List_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaSoulRouter(t, hSoulEnforcer{allow: true}, auditCap, newHSoulHandler(&hSoulPool{listSouls: []*soul.Soul{agentSoul()}}))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/souls", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("read soul.list recorded audit (%d events)", len(auditCap.Events()))
	}
}

// === HISTORY (READ-with-typed-query) — bad-offset 400 ===

func TestHumaSoul_History_BadOffset_400(t *testing.T) {
	r := humaSoulRouter(t, hSoulEnforcer{allow: true}, nil, newHSoulHandler(&hSoulPool{existing: agentSoul()}))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/souls/host-1.example.com/history?offset=-1", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

// === EXEC (POST /v1/souls/{sid}/exec — WRITE+AUDIT errand.invoked, 200 sync / 202 async) ===

// hExecStore — in-memory StoreAPI for the exec happy-path (Insert/MarkTerminal/Get).
type hExecStore struct {
	mu   sync.Mutex
	rows map[string]errand.Row
}

func newHExecStore() *hExecStore { return &hExecStore{rows: map[string]errand.Row{}} }

func (s *hExecStore) Insert(_ context.Context, row errand.Row) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[row.ErrandID] = row
	return nil
}
func (s *hExecStore) MarkTerminal(_ context.Context, id string, upd errand.TerminalUpdate) (bool, error) {
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
func (s *hExecStore) SweepOrphanRunning(context.Context, string, time.Duration, string) ([]string, error) {
	return nil, nil
}
func (s *hExecStore) Get(_ context.Context, id string) (*errand.Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	if !ok {
		return nil, errand.ErrNotFound
	}
	cp := r
	return &cp, nil
}

// hExecBus — a bus that delivers a pre-set ResultEvent on Subscribe (sync 200).
// nil event → the channel does not publish (escalation path to 202 with a small ServerCap).
type hExecBus struct {
	deliver *errand.ResultEvent
}

func (b hExecBus) Subscribe(ctx context.Context, applyID string) <-chan applybus.Event {
	return b.SubscribeWithBridge(ctx, applyID, true)
}

func (b hExecBus) SubscribeWithBridge(_ context.Context, applyID string, _ bool) <-chan applybus.Event {
	ch := make(chan applybus.Event, 1)
	if b.deliver != nil {
		ev := *b.deliver
		ch <- applybus.Event{ApplyID: applyID, Kind: errand.KindCompleted, Payload: ev}
	}
	return ch
}

// buildHExecDispatcher — in-memory Dispatcher for exec. deliver != nil → the sync result
// is delivered immediately (200); deliver == nil + small serverCap → async (202). Audit=nil
// → the dispatcher does NOT write self-audit (the S6-guard checks the middleware path).
func buildHExecDispatcher(t *testing.T, deliver *errand.ResultEvent, serverCap time.Duration) *errand.Dispatcher {
	t.Helper()
	d, err := errand.NewDispatcher(errand.Deps{
		Store:     newHExecStore(),
		Outbound:  hExecOutbound{},
		ApplyBus:  hExecBus{deliver: deliver},
		Logger:    slog.New(slog.NewJSONHandler(io.Discard, nil)),
		KID:       "kid-1",
		ServerCap: serverCap,
		Clock:     func() time.Time { return hSoulAt },
	})
	if err != nil {
		t.Fatalf("errand.NewDispatcher: %v", err)
	}
	return d
}

type hExecOutbound struct{}

func (hExecOutbound) SendErrand(context.Context, string, *keeperv1.ErrandRequest) error { return nil }
func (hExecOutbound) SendCancelErrand(context.Context, string, string) error            { return nil }

// humaExecRouter — a chi router with the huma exec route (prod mirror of router.go): RequirePermission
// (errand.run, ErrandSIDSelector) + huma audit middleware errand.invoked. injectClaims
// replaces RequireJWT.
func humaExecRouter(t *testing.T, enforcer hSoulEnforcer, auditW audit.Writer, d *errand.Dispatcher) *chi.Mux {
	t.Helper()
	installHumaErrorOverride()
	errandH := handlers.NewErrandHandler(d, nil, nil)
	r := chi.NewRouter()
	injectClaims := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := apimiddleware.InjectClaimsForTest(req.Context(), &keeperjwt.Claims{Subject: "archon-alice"})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
	r.Route("/v1", func(r chi.Router) {
		r.Route("/souls", func(r chi.Router) {
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "errand", "run", handlers.ErrandSIDSelector)).Group(func(r chi.Router) {
				registerHumaSoulExec(newHumaSoulAPI(r, auditW, audit.EventTypeErrandInvoked, nil), errandH)
			})
		})
	})
	return r
}

func execSyncResult() *errand.ResultEvent {
	exit := int32(0)
	return &errand.ResultEvent{Status: errand.StatusSuccess, ExitCode: &exit, Stdout: "ok"}
}

func TestHumaSoul_Exec_GoldenWire_Sync200(t *testing.T) {
	d := buildHExecDispatcher(t, execSyncResult(), time.Second)
	r := humaExecRouter(t, hSoulEnforcer{allow: true}, nil, d)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/souls/host-1.example.com/exec",
		strings.NewReader(`{"module":"core.cmd.shell","timeout_seconds":5}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (sync terminal); body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Errorf("Location-header on 200 sync must be ABSENT, got %q", loc)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply not JSON-object: %v; body=%s", err, rec.Body.String())
	}
	// errand_id (a fresh ULID) and duration_ms (real time.Since elapsed) are non-deterministic —
	// we check their presence and zero them for a byte-exact comparison of the rest of the wire (fields/enum/
	// started_at-truncation). The same contract as the legacy strict path.
	if m["errand_id"] == "" || m["errand_id"] == nil {
		t.Errorf("sync 200-body must carry errand_id, got %v", m)
	}
	if _, ok := m["duration_ms"]; !ok {
		t.Errorf("sync 200-body must carry duration_ms, got %v", m)
	}
	delete(m, "errand_id")
	delete(m, "duration_ms")
	out, _ := json.Marshal(m)
	// started_at = hSoulAt (Clock) .UTC().Truncate(Second); status a bare enum string.
	const golden = `{"exit_code":0,"module":"core.cmd.shell","sid":"host-1.example.com","started_at":"2026-06-13T12:00:00Z","started_by_aid":"archon-alice","status":"success","stdout":"ok"}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-drift soul.exec (sync 200):\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaSoul_Exec_GoldenWire_Async202(t *testing.T) {
	// deliver=nil + ServerCap=1ms + timeout=2s → the sync window expires WITHOUT a result → 202.
	d := buildHExecDispatcher(t, nil, time.Millisecond)
	r := humaExecRouter(t, hSoulEnforcer{allow: true}, nil, d)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/souls/host-1.example.com/exec",
		strings.NewReader(`{"module":"core.cmd.shell","timeout_seconds":2}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (async escalation); body=%s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/v1/errands/") {
		t.Errorf("Location-header on 202 must be /v1/errands/<id>, got %q", loc)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply not JSON-object: %v; body=%s", err, rec.Body.String())
	}
	if m["status"] != "running" || m["errand_id"] == "" || m["errand_id"] == nil {
		t.Errorf("202-body must carry errand_id + status:running, got %v", m)
	}
}

func TestHumaSoul_Exec_UnknownField_400(t *testing.T) {
	d := buildHExecDispatcher(t, execSyncResult(), time.Second)
	r := humaExecRouter(t, hSoulEnforcer{allow: true}, nil, d)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/souls/host-1.example.com/exec",
		strings.NewReader(`{"module":"core.cmd.shell","bogus":1}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

func TestHumaSoul_Exec_MissingModule_422(t *testing.T) {
	d := buildHExecDispatcher(t, execSyncResult(), time.Second)
	r := humaExecRouter(t, hSoulEnforcer{allow: true}, nil, d)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/souls/host-1.example.com/exec",
		strings.NewReader(`{"timeout_seconds":5}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (module required); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

func TestHumaSoul_Exec_BadSID_422(t *testing.T) {
	d := buildHExecDispatcher(t, execSyncResult(), time.Second)
	r := humaExecRouter(t, hSoulEnforcer{allow: true}, nil, d)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/souls/BAD_SID/exec",
		strings.NewReader(`{"module":"core.cmd.shell"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (bad sid format); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

func TestHumaSoul_Exec_RBACDeny_403(t *testing.T) {
	d := buildHExecDispatcher(t, execSyncResult(), time.Second)
	r := humaExecRouter(t, hSoulEnforcer{allow: false}, nil, d)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/souls/host-1.example.com/exec",
		strings.NewReader(`{"module":"core.cmd.shell"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// === EXEC S6-GUARD (errand.invoked on BOTH 2xx; does NOT write on 403/422) ===

func TestHumaAudit_ErrandExec_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	d := buildHExecDispatcher(t, execSyncResult(), time.Second)
	r := humaExecRouter(t, hSoulEnforcer{allow: true}, auditCap, d)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/souls/host-1.example.com/exec",
		strings.NewReader(`{"module":"core.cmd.shell","timeout_seconds":5}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventTypeErrandInvoked, map[string]any{
		"sid": "host-1.example.com", "module": "core.cmd.shell",
	})
}

func TestHumaAudit_ErrandExec_RecordsOnAsync202(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	d := buildHExecDispatcher(t, nil, time.Millisecond)
	r := humaExecRouter(t, hSoulEnforcer{allow: true}, auditCap, d)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/souls/host-1.example.com/exec",
		strings.NewReader(`{"module":"core.cmd.shell","timeout_seconds":2}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventTypeErrandInvoked, map[string]any{
		"sid": "host-1.example.com", "module": "core.cmd.shell",
	})
}

func TestHumaAudit_ErrandExec_NoAudit_OnRBACDeny(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	d := buildHExecDispatcher(t, execSyncResult(), time.Second)
	r := humaExecRouter(t, hSoulEnforcer{allow: false}, auditCap, d)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/souls/host-1.example.com/exec",
		strings.NewReader(`{"module":"core.cmd.shell"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit recorded on RBAC-deny soul.exec (%d events)", len(auditCap.Events()))
	}
}

func TestHumaAudit_ErrandExec_NoAudit_OnValidationFail(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	d := buildHExecDispatcher(t, execSyncResult(), time.Second)
	r := humaExecRouter(t, hSoulEnforcer{allow: true}, auditCap, d)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/souls/host-1.example.com/exec",
		strings.NewReader(`{"timeout_seconds":5}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit recorded on 422 soul.exec (%d events)", len(auditCap.Events()))
	}
}

func TestHumaAudit_ErrandExec_NoAudit_OnSoulNotConnected_404(t *testing.T) {
	// Outbound.SendErrand returns ErrSoulNotConnected → the dispatcher returns a terminal-fail
	// with err → ExecTyped maps to 404; the middleware does not write (4xx).
	auditCap := &auditCaptureWriter{}
	r := humaExecRouter(t, hSoulEnforcer{allow: true}, auditCap, errandNotConnectedDispatcher(t))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/souls/host-1.example.com/exec",
		strings.NewReader(`{"module":"core.cmd.shell"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (soul not connected); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeNotFound)
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit recorded on 404 soul.exec (%d events)", len(auditCap.Events()))
	}
}

// errandNotConnectedDispatcher — an Outbound returning ErrSoulNotConnected (Soul not
// connected) → Dispatch returns err → ExecTyped maps to 404.
func errandNotConnectedDispatcher(t *testing.T) *errand.Dispatcher {
	t.Helper()
	d, err := errand.NewDispatcher(errand.Deps{
		Store:     newHExecStore(),
		Outbound:  hExecOutboundOffline{},
		ApplyBus:  hExecBus{},
		Logger:    slog.New(slog.NewJSONHandler(io.Discard, nil)),
		KID:       "kid-1",
		ServerCap: time.Second,
		Clock:     func() time.Time { return hSoulAt },
	})
	if err != nil {
		t.Fatalf("errand.NewDispatcher: %v", err)
	}
	return d
}

type hExecOutboundOffline struct{}

func (hExecOutboundOffline) SendErrand(context.Context, string, *keeperv1.ErrandRequest) error {
	return errand.ErrSoulNotConnected
}
func (hExecOutboundOffline) SendCancelErrand(context.Context, string, string) error { return nil }

// TestHumaSoul_Exec_ChiCoexistence — guard for the coexistence of huma-exec
// (/v1/souls/{sid}/exec) with ALL the other soul-detail huma routes
// (/v1/souls/{sid}[/issue-token|/ssh-target|/soulprint|/history]) on one
// /souls chi group. Builds the prod router via buildRouter with a NON-nil errandH
// (the drift-test sets nil → exec is not mounted) and walks chi.Walk: exec MUST
// appear exactly once, without a duplicate mount / prefix collision. Building the router
// with a chi-pattern collision would panic here.
func TestHumaSoul_Exec_ChiCoexistence(t *testing.T) {
	h := buildRouter(
		nil, // verifier
		nil, // healthH
		stubOperatorHandler(t),
		handlers.NewIncarnationHandler(nil, nil, nil, nil, nil, nil, nil, nil, nil),
		handlers.NewSoulHandler(nil, nil, nil, nil),
		handlers.TelemetrySpecStub(),
		stubRoleHandler(t), stubSynodHandler(t), stubSigilHandler(t), stubSigilKeyHandler(t),
		stubServiceHandler(t), nil, stubAugurHandler(t), stubOracleHandler(t),
		nil,                                      // pushH
		nil,                                      // pushProviderH
		nil,                                      // providerH
		nil,                                      // profileH
		handlers.NewErrandHandler(nil, nil, nil), // errandH non-nil → exec is mounted on huma
		nil,                                      // voyageH
		nil,                                      // cadenceH
		nil,                                      // auditH
		nil,                                      // choirH
		nil,                                      // heraldH
		handlers.NewModuleCatalogHandler(nil, nil),
		handlers.NewModuleFormPrepHandler(nil, nil),
		handlers.NewPermissionCatalogHandler(nil),
		handlers.NewEventTypeCatalogHandler(nil),
		handlers.NewHeraldTypeCatalogHandler(nil),
		handlers.NewMyPermissionsHandler(nil, nil),
		nil,                                  // enforcer
		nil,                                  // auditWriter
		nil,                                  // metricsHTTP
		nil,                                  // tollDegraded
		nil,                                  // tempoLimiter
		nil,                                  // tempoMetrics
		nil,                                  // tempoVoyageCreateLimits
		nil,                                  // tempoVoyagePreviewLimits
		false,                                // webUIEnabled — /ui is out of scope for the soul routing test
		nil,                                  // ldapAuth (LDAP not configured in the test)
		nil,                                  // oidcAuth (OIDC not configured in the test)
		nil,                                  // loginGuard (anti-bruteforce off in the test)
		apimiddleware.AuthLoginLimitConfig{}, // loginLimitCfg
		nil,                                  // soulStatsStaleFn (default 90s in the test)
		nil,                                  // clusterH (cluster-view not mounted in the test)
		nil,                                  // runEventsDeps (ADR-068 §A3 — not tested here)
		nil,                                  // logger
	)
	routes, ok := h.(chi.Routes)
	if !ok {
		t.Fatalf("buildRouter returned %T, not chi.Routes", h)
	}
	var execCount, detailCount int
	if err := chi.Walk(routes, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == http.MethodPost && pattern == "/v1/souls/{sid}/exec" {
			execCount++
		}
		if method == http.MethodGet && pattern == "/v1/souls/{sid}/soulprint" {
			detailCount++
		}
		return nil
	}); err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	if execCount != 1 {
		t.Errorf("POST /v1/souls/{sid}/exec occurred %d times, want 1 (duplicate/mount collision)", execCount)
	}
	if detailCount != 1 {
		t.Errorf("GET /v1/souls/{sid}/soulprint occurred %d times, want 1 (exec broke the soul-detail group)", detailCount)
	}
}

func TestHumaSoul_SpecYAML(t *testing.T) {
	frag, err := HumaSoulSpecYAML()
	if err != nil {
		t.Fatalf("HumaSoulSpecYAML: %v", err)
	}
	for _, want := range []string{"createSoul", "assignSoulCoven", "issueSoulToken", "updateSoulSSHTarget", "ErrandExec", "listSouls", "getSoul", "getSoulprint", "getSoulHistory"} {
		if !strings.Contains(frag, want) {
			t.Errorf("spec does not contain op %q:\n%s", want, frag)
		}
	}
}
