package api

// Guard-тесты ТИРАЖ-БАТЧА-2e разворота SOUL-домена на huma full-typed (ADR-054 §Pattern,
// эталоны role/operator + audit-endpoint). create/coven-assign/issue-token/ssh-target —
// WRITE+AUDIT (вариант B); list/get/soulprint/history — read (БЕЗ audit). ErrandExec НЕ в
// этом батче (strict). Доказывают инварианты поверх chi:
//
//   - wire/golden: create 201 SoulCreateReply (byte-exact); issue-token 200 (token+expires);
//     ssh-target 200 snapshot; coven-assign 200 (custom XOR);
//   - create unknown-field → 400; missing-required → 422; list bad-offset → 400; list/coven bad-
//     status-enum → 422; history bad-offset → 400; RBAC-deny → 403;
//   - S6-GUARD на КАЖДЫЙ write (create/coven-assign/issue-token/ssh-target): полная huma-
//     навеска пишет ВЕРНЫЙ event-type с НЕПУСТЫМ payload на 2xx и НЕ пишет на 403/400/422;
//   - reads (get/list/soulprint/history) → NoAudit.
//
// Глубокая бизнес-логика (keyset/scope-eval/bulk-chunks/partial/transaction-rollback) покрыта
// handlers/soul_test.go через ТЕ ЖЕ *Typed-функции (huma-роут зовёт их же); здесь — huma-слой.

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

// hSoulAt — фиксированное registered_at/expires_at для детерминированного golden.
var hSoulAt = time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)

// hSoulPool — компактный мок [handlers.SoulPool] для huma-теста. Поддерживает Create (Insert
// souls + Insert bootstrap_token + Commit), IssueToken (SelectBySID + Insert), UpdateSshTarget,
// List (COUNT + Query). Глубокие сценарии — handlers/soul_test.go.
type hSoulPool struct {
	existing  *soul.Soul // SelectBySID → строка (issue-token/get); nil → ErrNoRows
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

// assignScan присваивает значение в pgx-dest по типу (узкий набор колонок souls).
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

// hSoulScoper — мок [handlers.PurviewResolver]: unrestricted → весь флот видим (offset-fast-
// path, без keyset). Для huma-теста достаточно (scope-eval покрыт handlers/soul_test.go).
type hSoulScoper struct{ unrestricted bool }

func (s hSoulScoper) ResolvePurview(string, string, string) rbac.Purview {
	return rbac.Purview{Unrestricted: s.unrestricted}
}

func newHSoulHandler(pool *hSoulPool) *handlers.SoulHandler {
	return handlers.NewSoulHandler(pool, hSoulScoper{unrestricted: true}, nil, nil)
}

// hSoulEnforcer реализует и [apimiddleware.PermissionChecker] (write-роуты, RequirePermission),
// и [apimiddleware.ActionHolder] (read-роуты, RequireAction existence-gate). allow=false →
// 403 на обоих путях.
type hSoulEnforcer struct{ allow bool }

func (e hSoulEnforcer) Check(string, string, string, map[string]string) error {
	if e.allow {
		return nil
	}
	return rbac.ErrPermissionDenied
}

func (e hSoulEnforcer) HoldsAction(string, string, string) bool { return e.allow }

// humaSoulRouter собирает chi-роутер со ВСЕМИ soul-роутами через huma (кроме exec) —
// продакшен-навеска из router.go. injectClaims заменяет RequireJWT.
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
	// transport=ssh → без bootstrap_token; date-time секундный.
	const golden = `{"covens":["web"],"created_by_aid":"archon-alice","registered_at":"2026-06-13T12:00:00Z","sid":"host-1.example.com","status":"pending","transport":"ssh"}`
	if got := remarshalSorted(t, rec.Body.Bytes()); got != golden {
		t.Errorf("GOLDEN wire-дрейф soul.create:\n got  = %s\n want = %s", got, golden)
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
		t.Errorf("audit записан на RBAC-deny soul.create (%d событий)", len(auditCap.Events()))
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
		t.Errorf("audit записан на 422 soul.create (%d событий)", len(auditCap.Events()))
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
		t.Fatalf("reply не JSON: %v; body=%s", err, rec.Body.String())
	}
	if m["sid"] != "host-1.example.com" || m["bootstrap_token"] == "" || m["expires_at"] == nil {
		t.Errorf("issue-token 200-тело: %v (ожидали sid+bootstrap_token+expires_at)", m)
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
		t.Errorf("audit записан на RBAC-deny soul.issue-token (%d событий)", len(auditCap.Events()))
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
		t.Errorf("GOLDEN wire-дрейф soul.ssh-target:\n got  = %s\n want = %s", got, remarshalSorted(t, []byte(golden)))
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

// === COVEN-ASSIGN (WRITE+AUDIT soul.coven-changed) — dry_run (без bulk-chunk-плана) ===

func TestHumaSoul_CovenAssign_DryRunGolden(t *testing.T) {
	r := humaSoulRouter(t, hSoulEnforcer{allow: true}, nil, newHSoulHandler(&hSoulPool{}))
	rec := httptest.NewRecorder()
	// dry_run + unrestricted scope → CountBulkMatched (COUNT) без UPDATE. append-mode.
	req := httptest.NewRequest(http.MethodPost, "/v1/souls/coven",
		strings.NewReader(`{"mode":"append","label":"prod","dry_run":true,"selector":{"all":true}}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply не JSON: %v; body=%s", err, rec.Body.String())
	}
	// custom MarshalJSON XOR: append → label (не labels), dry_run:true.
	if m["mode"] != "append" || m["label"] != "prod" || m["dry_run"] != true {
		t.Errorf("coven-assign dry_run 200-тело XOR-форма неверна: %v", m)
	}
	if _, hasLabels := m["labels"]; hasLabels {
		t.Errorf("append-mode не должен нести labels: %v", m)
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

// === LIST (READ-with-typed-query, БЕЗ audit) ===

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
	// offset>0 + ВАЛИДНЫЙ cursor → 422 (две пагинации одновременно). Битый cursor дал бы 400
	// (decode-фейл ДО conflict-чека), поэтому кодируем настоящий keyset-курсор.
	enc := sharedapi.EncodeKeysetCursor(sharedapi.KeysetCursor{RegisteredAt: hSoulAt, SID: "host-1.example.com"})
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/souls?offset=5&cursor="+enc, nil))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

// TestHumaSoul_List_BadCursor_400 — битый keyset-курсор (DecodeKeysetCursor-фейл)
// → 400 TypeMalformedRequest. Покрывает ветку soulParsePage (decode-фейл ДО
// conflict-чека); симметрично TestHumaSoul_List_OffsetCursorConflict_422, но без
// offset — чистый decode-фейл. "not-base64!" не декодируется как keyset-курсор.
func TestHumaSoul_List_BadCursor_400(t *testing.T) {
	r := humaSoulRouter(t, hSoulEnforcer{allow: true}, nil, newHSoulHandler(&hSoulPool{}))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/souls?cursor=not-base64!", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (битый cursor → decode-фейл); body=%s", rec.Code, rec.Body.String())
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
		t.Errorf("read soul.list записал audit (%d событий)", len(auditCap.Events()))
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

// hExecStore — in-memory StoreAPI для exec-happy-path (Insert/MarkTerminal/Get).
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

// hExecBus — bus, доставляющий заранее заданный ResultEvent на Subscribe (sync 200).
// nil-event → канал не публикует (escalation-путь к 202 при малом ServerCap).
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

// buildHExecDispatcher — in-memory Dispatcher для exec. deliver != nil → sync-результат
// доставляется немедленно (200); deliver == nil + serverCap малый → async (202). Audit=nil
// → dispatcher НЕ пишет self-audit (S6-guard проверяет именно middleware-путь).
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

// humaExecRouter — chi-роутер с huma-exec-роутом (прод-зеркало router.go): RequirePermission
// (errand.run, ErrandSIDSelector) + huma-audit-middleware errand.invoked. injectClaims
// заменяет RequireJWT.
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
		t.Fatalf("status = %d, want 200 (sync терминал); body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Errorf("Location-header на 200 sync должен ОТСУТСТВОВАТЬ, got %q", loc)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply не JSON-object: %v; body=%s", err, rec.Body.String())
	}
	// errand_id (свежий ULID) и duration_ms (реальный time.Since elapsed) недетерминированы —
	// проверяем их наличие, обнуляем для byte-exact-сравнения остального wire (поля/enum/
	// started_at-truncation). Тот же контракт у легаси strict-пути.
	if m["errand_id"] == "" || m["errand_id"] == nil {
		t.Errorf("sync 200-тело должно нести errand_id, got %v", m)
	}
	if _, ok := m["duration_ms"]; !ok {
		t.Errorf("sync 200-тело должно нести duration_ms, got %v", m)
	}
	delete(m, "errand_id")
	delete(m, "duration_ms")
	out, _ := json.Marshal(m)
	// started_at = hSoulAt (Clock) .UTC().Truncate(Second); статус голая enum-строка.
	const golden = `{"exit_code":0,"module":"core.cmd.shell","sid":"host-1.example.com","started_at":"2026-06-13T12:00:00Z","started_by_aid":"archon-alice","status":"success","stdout":"ok"}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-дрейф soul.exec (sync 200):\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaSoul_Exec_GoldenWire_Async202(t *testing.T) {
	// deliver=nil + ServerCap=1ms + timeout=2s → sync-окно истекает БЕЗ результата → 202.
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
		t.Errorf("Location-header на 202 должен быть /v1/errands/<id>, got %q", loc)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply не JSON-object: %v; body=%s", err, rec.Body.String())
	}
	if m["status"] != "running" || m["errand_id"] == "" || m["errand_id"] == nil {
		t.Errorf("202-тело должно нести errand_id + status:running, got %v", m)
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

// === EXEC S6-GUARD (errand.invoked на ОБА 2xx; НЕ пишет на 403/422) ===

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
		t.Errorf("audit записан на RBAC-deny soul.exec (%d событий)", len(auditCap.Events()))
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
		t.Errorf("audit записан на 422 soul.exec (%d событий)", len(auditCap.Events()))
	}
}

func TestHumaAudit_ErrandExec_NoAudit_OnSoulNotConnected_404(t *testing.T) {
	// Outbound.SendErrand вернёт ErrSoulNotConnected → dispatcher отдаёт terminal-fail
	// с err → ExecTyped мапит 404; middleware не пишет (4xx).
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
		t.Errorf("audit записан на 404 soul.exec (%d событий)", len(auditCap.Events()))
	}
}

// errandNotConnectedDispatcher — Outbound, отдающий ErrSoulNotConnected (Soul не
// подключён) → Dispatch возвращает err → ExecTyped мапит 404.
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

// TestHumaSoul_Exec_ChiCoexistence — guard на сосуществование huma-exec
// (/v1/souls/{sid}/exec) со ВСЕМИ остальными soul-detail-huma-роутами
// (/v1/souls/{sid}[/issue-token|/ssh-target|/soulprint|/history]) на одной
// /souls-chi-группе. Собирает прод-router через buildRouter с НЕ-nil errandH
// (drift-test ставит nil → exec не монтируется) и обходит chi.Walk: exec ДОЛЖЕН
// быть ровно один, без дубль-mount-а / коллизии префикса. Сборка router-а с
// коллизией chi-pattern-ов паникнула бы здесь.
func TestHumaSoul_Exec_ChiCoexistence(t *testing.T) {
	h := buildRouter(
		nil, // verifier
		nil, // healthH
		stubOperatorHandler(t),
		handlers.NewIncarnationHandler(nil, nil, nil, nil, nil, nil, nil, nil, nil),
		handlers.NewSoulHandler(nil, nil, nil, nil),
		stubRoleHandler(t), stubSynodHandler(t), stubSigilHandler(t), stubSigilKeyHandler(t),
		stubServiceHandler(t), nil, stubAugurHandler(t), stubOracleHandler(t),
		nil,                                      // pushH
		nil,                                      // pushProviderH
		handlers.NewErrandHandler(nil, nil, nil), // errandH non-nil → exec монтируется на huma
		nil,                                      // voyageH
		nil,                                      // cadenceH
		nil,                                      // auditH
		nil,                                      // choirH
		nil,                                      // heraldH
		handlers.NewModuleCatalogHandler(nil, nil),
		handlers.NewModuleFormPrepHandler(nil, nil),
		handlers.NewPermissionCatalogHandler(nil),
		handlers.NewEventTypeCatalogHandler(nil),
		handlers.NewMyPermissionsHandler(nil, nil),
		nil,   // enforcer
		nil,   // auditWriter
		nil,   // metricsHTTP
		nil,   // tollDegraded
		nil,   // tempoLimiter
		nil,   // tempoMetrics
		nil,   // tempoVoyageCreateLimits
		nil,   // tempoVoyagePreviewLimits
		false, // webUIEnabled — /ui вне интереса soul-роутинг-теста
		nil,   // ldapAuth (LDAP не сконфигурирован в тесте)
		nil,   // oidcAuth (OIDC не сконфигурирован в тесте)
		nil,   // logger
	)
	routes, ok := h.(chi.Routes)
	if !ok {
		t.Fatalf("buildRouter вернул %T, не chi.Routes", h)
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
		t.Errorf("POST /v1/souls/{sid}/exec встретился %d раз, want 1 (дубль/коллизия mount-а)", execCount)
	}
	if detailCount != 1 {
		t.Errorf("GET /v1/souls/{sid}/soulprint встретился %d раз, want 1 (exec сломал soul-detail-группу)", detailCount)
	}
}

func TestHumaSoul_SpecYAML(t *testing.T) {
	frag, err := HumaSoulSpecYAML()
	if err != nil {
		t.Fatalf("HumaSoulSpecYAML: %v", err)
	}
	for _, want := range []string{"createSoul", "assignSoulCoven", "issueSoulToken", "updateSoulSSHTarget", "ErrandExec", "listSouls", "getSoul", "getSoulprint", "getSoulHistory"} {
		if !strings.Contains(frag, want) {
			t.Errorf("спека не содержит op %q:\n%s", want, frag)
		}
	}
}
