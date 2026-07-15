package api

// Guard tests for CHOIR/VOICE on huma, FULL-TYPED WRITE-SELF-AUDIT form (batch-2f,
// ADR-054). These routes write audit INSIDE the handler (CreateTyped/DeleteTyped/
// AddVoiceTyped/RemoveVoiceTyped → writeAuditCtx), without audit middleware. The guards
// prove: wire 201/204/200, S6-SELF-AUDIT (the handler REALLY writes an event with a
// non-empty payload on 2xx), NoAudit on 403/422/404, golden-JSON byte-exact,
// RBAC-deny→403, multi-resource voices sub-resource. RBAC routes through
// RequirePermissionMulti(choir, ..., incScope) — but in the guards the enforcer is allow/deny-all,
// and the incScope selector is not invoked (the subject is the huma wiring, not RBAC scope).

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
	"github.com/souls-guild/soul-stack/shared/audit"
)

// fakeChoirDB — minimal mock of handlers.ChoirDB (choir.ExecQueryRower +
// choir.TxBeginner) for the api-package huma guards. Dispatches by SQL substring.
// Default is happy-path (create/add-voice success, the member-SID check passes).
type fakeChoirDB struct {
	insertChoirErr  error
	insertVoiceErr  error
	memberSIDs      []string // SIDs that are actual members of the incarnation (AddVoice membership check)
	choirLockRow    pgx.Row  // FOR UPDATE: nil → found (1); errRow → ErrChoirNotFound
	deleteChoirRows int
	deleteVoiceRows int
}

func (f *fakeChoirDB) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	switch {
	case strings.Contains(sql, "DELETE FROM incarnation_choirs"):
		return pgconn.NewCommandTag("DELETE " + choirItoa(f.deleteChoirRows)), nil
	case strings.Contains(sql, "DELETE FROM incarnation_choir_voices"):
		return pgconn.NewCommandTag("DELETE " + choirItoa(f.deleteVoiceRows)), nil
	}
	return pgconn.CommandTag{}, nil
}

func (f *fakeChoirDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "INSERT INTO incarnation_choirs"):
		if f.insertChoirErr != nil {
			return strictErrRow{err: f.insertChoirErr}
		}
		return choirTimeRow{}
	case strings.Contains(sql, "FOR UPDATE"):
		if f.choirLockRow != nil {
			return f.choirLockRow
		}
		return strictScalarRow{vals: []any{1}}
	case strings.Contains(sql, "INSERT INTO incarnation_choir_voices"):
		if f.insertVoiceErr != nil {
			return strictErrRow{err: f.insertVoiceErr}
		}
		return choirTimeRow{}
	}
	return strictErrRow{err: pgx.ErrNoRows}
}

func (f *fakeChoirDB) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	switch {
	case strings.Contains(sql, "FROM souls"):
		rows := make([][]any, 0, len(f.memberSIDs))
		for _, s := range f.memberSIDs {
			rows = append(rows, []any{s})
		}
		return &choirStrRows{rows: rows}, nil
	}
	return &strictEmptyRows{}, nil
}

func (f *fakeChoirDB) BeginTx(_ context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	return &fakeChoirTx{db: f}, nil
}

type fakeChoirTx struct{ db *fakeChoirDB }

func (t *fakeChoirTx) Begin(context.Context) (pgx.Tx, error) { return t, nil }
func (t *fakeChoirTx) Commit(context.Context) error          { return nil }
func (t *fakeChoirTx) Rollback(context.Context) error        { return nil }
func (t *fakeChoirTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	panic("unexpected")
}
func (t *fakeChoirTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults { panic("unexpected") }
func (t *fakeChoirTx) LargeObjects() pgx.LargeObjects                         { panic("unexpected") }
func (t *fakeChoirTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	panic("unexpected")
}
func (t *fakeChoirTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.db.Exec(ctx, sql, args...)
}
func (t *fakeChoirTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.db.Query(ctx, sql, args...)
}
func (t *fakeChoirTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.db.QueryRow(ctx, sql, args...)
}
func (t *fakeChoirTx) Conn() *pgx.Conn { return nil }

// choirTimeRow — Scan(&CreatedAt|&AddedAt) (a single time.Time).
type choirTimeRow struct{}

func (choirTimeRow) Scan(dest ...any) error {
	if len(dest) > 0 {
		if p, ok := dest[0].(*time.Time); ok {
			*p = time.Unix(0, 0).UTC()
		}
	}
	return nil
}

// choirStrRows — pgx.Rows over [][]any (string-only, for the membership check).
type choirStrRows struct {
	rows [][]any
	idx  int
}

func (r *choirStrRows) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}
func (r *choirStrRows) Scan(dest ...any) error {
	row := r.rows[r.idx-1]
	for i, d := range dest {
		if p, ok := d.(*string); ok {
			*p = row[i].(string)
		}
	}
	return nil
}
func (r *choirStrRows) Err() error                                   { return nil }
func (r *choirStrRows) Close()                                       {}
func (r *choirStrRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *choirStrRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *choirStrRows) Values() ([]any, error)                       { return nil, nil }
func (r *choirStrRows) RawValues() [][]byte                          { return nil }
func (r *choirStrRows) Conn() *pgx.Conn                              { return nil }

func choirItoa(n int) string {
	if n == 0 {
		return "0"
	}
	return "1"
}

// humaChoirRouter mounts the choir/voice huma routes exactly per the router.go wiring
// (RequirePermissionMulti on each group + a huma op with the FULL path /{name}/choirs
// [/...] on the /v1/incarnations group). enforcer/auditW/db are parameterized. incScope
// (the RBAC selector) is not invoked by the guards — the enforcer is allow/deny-all.
func humaChoirRouter(t *testing.T, enforcer apimiddleware.PermissionChecker, auditW audit.Writer, db handlers.ChoirDB) *chi.Mux {
	t.Helper()
	installHumaErrorOverride()
	choirH := handlers.NewChoirHandler(db, auditW, nil)

	r := chi.NewRouter()
	injectClaims := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := apimiddleware.InjectClaimsForTest(req.Context(), &keeperjwt.Claims{Subject: "archon-alice"})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
	noMulti := func(_ *http.Request) []map[string]string { return nil }
	r.Route("/v1", func(r chi.Router) {
		r.Route("/incarnations", func(r chi.Router) {
			r.With(injectClaims, apimiddleware.RequirePermissionMulti(enforcer, "choir", "create", noMulti)).
				Group(func(r chi.Router) { registerHumaChoirCreate(newHumaCadenceAPI(r), choirH) })
			r.With(injectClaims, apimiddleware.RequirePermissionMulti(enforcer, "choir", "delete", noMulti)).
				Group(func(r chi.Router) { registerHumaChoirDelete(newHumaCadenceAPI(r), choirH) })
			r.With(injectClaims, apimiddleware.RequirePermissionMulti(enforcer, "choir", "add-voice", noMulti)).
				Group(func(r chi.Router) { registerHumaVoiceAdd(newHumaCadenceAPI(r), choirH) })
			r.With(injectClaims, apimiddleware.RequirePermissionMulti(enforcer, "choir", "remove-voice", noMulti)).
				Group(func(r chi.Router) { registerHumaVoiceRemove(newHumaCadenceAPI(r), choirH) })
			r.With(injectClaims, apimiddleware.RequirePermissionMulti(enforcer, "choir", "list", noMulti)).
				Group(func(r chi.Router) {
					api := newHumaCadenceAPI(r)
					registerHumaChoirList(api, choirH)
					registerHumaVoiceList(api, choirH)
				})
		})
	})
	return r
}

const (
	choirTestInc   = "redis-prod"
	choirTestName  = "primary"
	choirTestSID   = "node-1.example.com"
	choirCreateURL = "/v1/incarnations/redis-prod/choirs"
	choirDelURL    = "/v1/incarnations/redis-prod/choirs/primary"
	voiceAddURL    = "/v1/incarnations/redis-prod/choirs/primary/voices"
	voiceDelURL    = "/v1/incarnations/redis-prod/choirs/primary/voices/node-1.example.com"
)

// --- Create ---

func TestHumaChoir_Create_WireAndAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaChoirRouter(t, strictAllowAll{}, auditCap, &fakeChoirDB{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, choirCreateURL, strings.NewReader(`{"choir_name":"primary"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var dto struct {
		ChoirName       string `json:"choir_name"`
		IncarnationName string `json:"incarnation_name"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if dto.ChoirName != "primary" || dto.IncarnationName != choirTestInc {
		t.Errorf("dto = %+v, want choir=primary inc=%s", dto, choirTestInc)
	}
	assertSelfAudit(t, auditCap, audit.EventChoirCreated, "choir_name")
}

func TestHumaChoir_Create_UnknownField_400(t *testing.T) {
	r := humaChoirRouter(t, strictAllowAll{}, nil, &fakeChoirDB{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, choirCreateURL, strings.NewReader(`{"choir_name":"primary","bogus":1}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (unknown field); body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaChoir_Create_BadChoirName_422_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaChoirRouter(t, strictAllowAll{}, auditCap, &fakeChoirDB{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, choirCreateURL, strings.NewReader(`{"choir_name":"BAD NAME"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (bad choir_name); body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан на 422 — write-путь не должен писать")
	}
}

func TestHumaChoir_Create_RBACDeny_403_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaChoirRouter(t, strictDenyAll{}, auditCap, &fakeChoirDB{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, choirCreateURL, strings.NewReader(`{"choir_name":"primary"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан на 403")
	}
}

func TestHumaChoir_Create_GoldenWire(t *testing.T) {
	r := humaChoirRouter(t, strictAllowAll{}, nil, &fakeChoirDB{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, choirCreateURL, strings.NewReader(`{"choir_name":"primary"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	got := normalizeJSONKeys(t, rec.Body.Bytes())
	// nullable choir fields (created_by_aid/description/min_size/max_size) — null (no
	// omitempty, parity legacy Choir). created_at is normalized (epoch-0 fix).
	const golden = `{"choir_name":"primary","created_at":"1970-01-01T00:00:00Z","created_by_aid":"archon-alice","description":null,"incarnation_name":"redis-prod","max_size":null,"min_size":null}`
	if got != golden {
		t.Errorf("GOLDEN wire-дрейф choir create-reply:\n got  = %s\n want = %s\n(набор ключей/null/$schema изменился — проверь choirCreateOutput/toChoirDTO)", got, golden)
	}
}

// --- Delete ---

func TestHumaChoir_Delete_WireAndAudit_204(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaChoirRouter(t, strictAllowAll{}, auditCap, &fakeChoirDB{deleteChoirRows: 1})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, choirDelURL, http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	assertSelfAudit(t, auditCap, audit.EventChoirDeleted, "choir_name")
}

func TestHumaChoir_Delete_NotFound_404_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaChoirRouter(t, strictAllowAll{}, auditCap, &fakeChoirDB{deleteChoirRows: 0})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, choirDelURL, http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан на 404 delete")
	}
}

// --- AddVoice (multi-resource sub-resource) ---

func TestHumaVoice_Add_WireAndAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	db := &fakeChoirDB{memberSIDs: []string{choirTestSID}}
	r := humaChoirRouter(t, strictAllowAll{}, auditCap, db)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, voiceAddURL, strings.NewReader(`{"sid":"node-1.example.com"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var dto struct {
		SID       string `json:"sid"`
		ChoirName string `json:"choir_name"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &dto)
	if dto.SID != choirTestSID || dto.ChoirName != choirTestName {
		t.Errorf("dto = %+v, want sid=%s choir=%s", dto, choirTestSID, choirTestName)
	}
	assertSelfAudit(t, auditCap, audit.EventChoirVoiceAdded, "sid")
}

func TestHumaVoice_Add_NotMember_422_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	db := &fakeChoirDB{memberSIDs: nil} // SID is not a member of the incarnation → ErrNotMembers → 422
	r := humaChoirRouter(t, strictAllowAll{}, auditCap, db)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, voiceAddURL, strings.NewReader(`{"sid":"node-1.example.com"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (SID не член); body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан на 422 add-voice")
	}
}

// --- RemoveVoice ---

func TestHumaVoice_Remove_WireAndAudit_204(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaChoirRouter(t, strictAllowAll{}, auditCap, &fakeChoirDB{deleteVoiceRows: 1})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, voiceDelURL, http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	assertSelfAudit(t, auditCap, audit.EventChoirVoiceRemoved, "sid")
}

func TestHumaVoice_Remove_NotFound_404_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaChoirRouter(t, strictAllowAll{}, auditCap, &fakeChoirDB{deleteVoiceRows: 0})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, voiceDelURL, http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан на 404 remove-voice")
	}
}

// --- Reads (NoAudit) ---

func TestHumaChoir_List_Read_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaChoirRouter(t, strictAllowAll{}, auditCap, &fakeChoirDB{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, choirCreateURL, http.NoBody)
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
		t.Error("items должен быть [] (non-nil), а не null")
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("READ list записал audit (%d событий) — read не должен писать", len(auditCap.Events()))
	}
}

func TestHumaVoice_List_Read_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaChoirRouter(t, strictAllowAll{}, auditCap, &fakeChoirDB{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, voiceAddURL, http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("READ list-voices записал audit")
	}
}
