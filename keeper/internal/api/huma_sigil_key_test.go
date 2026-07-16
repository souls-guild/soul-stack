package api

// Guard tests of ROLLOUT-BATCH-2a moving the SIGIL-KEY domain (/v1/sigil/keys) WHOLESALE onto
// huma full-typed (ADR-054 §Pattern, role references). introduce/set-primary/retire —
// WRITE+AUDIT (variant B, huma-audit-middleware; events sigil.key-introduced/
// sigil.key-primary-set/sigil.key-retired); list — read-bare (no audit). They prove:
//
//   - wire/golden: introduce 201 (stable fields; key_id/pubkey are ed25519-generated,
//     not byte-exact); list 200 items[]; set-primary/retire 204 empty;
//   - unknown-field → 400; bad key_id path → 422; RBAC-deny → 403;
//   - S6-GUARD on EVERY write route (introduce/set-primary/retire): the full huma
//     wiring writes an audit event with a NON-EMPTY payload + the CORRECT event type on 2xx and
//     does NOT write on 4xx/403.

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
	"github.com/souls-guild/soul-stack/keeper/internal/sigil"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// skTestKeyID — a valid key_id (64 hex) for the set-primary/retire path routes.
const skTestKeyID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

var skIntroducedAt = time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)

// skVault — mock [sigil.VaultWriter] (WriteKV no-op).
type skVault struct{}

func (skVault) WriteKV(context.Context, string, map[string]any) error { return nil }

// skPool — mock [sigil.KeyStorePool] for all sigil-key success paths of the huma test.
// introduce: BeginTx → insert QueryRow(id, introduced_at) → Commit. set-primary:
// selectKeyForUpdate → active non-primary → clear+set Exec → Commit. retire:
// lockActive (count>1) → selectKeyForUpdate active non-primary → retire Exec(rows=1)
// → Commit. list: listActiveKeys Query → one active non-primary key.
type skPool struct{}

func (skPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag("UPDATE 1"), nil
}
func (skPool) QueryRow(context.Context, string, ...any) pgx.Row {
	return skErrRow{err: pgx.ErrNoRows}
}
func (skPool) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	switch {
	case strings.Contains(sql, "FROM sigil_signing_keys") && strings.Contains(sql, "status = 'active'") && strings.Contains(sql, "ORDER BY"):
		return &skListRows{}, nil // listActiveKeys: one active non-primary key
	case strings.Contains(sql, "FOR UPDATE"):
		return &skLockRows{ids: []int64{1, 2}}, nil // countLockedActive>1 → retire ok
	}
	return &skLockRows{}, nil
}
func (skPool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return skTx{}, nil
}

// skTx — a pgx.Tx for the insert/select/exec paths of introduce/set-primary/retire.
type skTx struct{ pgx.Tx }

func (skTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag("UPDATE 1"), nil
}
func (skTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return skPool{}.Query(ctx, sql, args...)
}
func (skTx) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "INSERT INTO sigil_signing_keys"):
		return skInsertRow{id: 1, at: skIntroducedAt}
	case strings.Contains(sql, "WHERE key_id ="):
		// selectKeyForUpdate: active non-primary target key (10 columns).
		return skKeyRow{}
	}
	return skErrRow{err: pgx.ErrNoRows}
}
func (skTx) Commit(context.Context) error   { return nil }
func (skTx) Rollback(context.Context) error { return nil }

type skErrRow struct{ err error }

func (r skErrRow) Scan(...any) error { return r.err }

// skInsertRow — RETURNING (id, introduced_at) of the introduce insert.
type skInsertRow struct {
	id int64
	at time.Time
}

func (r skInsertRow) Scan(dest ...any) error {
	*(dest[0].(*int64)) = r.id
	*(dest[1].(*time.Time)) = r.at
	return nil
}

// skKeyRow — selectKeyByIDForUpdate (10 columns): active non-primary target key.
type skKeyRow struct{}

func (skKeyRow) Scan(dest ...any) error {
	*(dest[0].(*int64)) = 1                  // id
	*(dest[1].(*string)) = skTestKeyID       // key_id
	*(dest[2].(*string)) = "pem"             // pubkey_pem
	*(dest[3].(*string)) = "vault:ref"       // vault_ref
	*(dest[4].(*bool)) = false               // is_primary (NOT primary → retire/set-primary ok)
	*(dest[5].(*string)) = "active"          // status
	*(dest[6].(*time.Time)) = skIntroducedAt // introduced_at
	*(dest[7].(**string)) = nil              // introduced_by_aid
	*(dest[8].(**time.Time)) = nil           // retired_at
	*(dest[9].(**string)) = nil              // retired_by_aid
	return nil
}

// skLockRows — countLockedActive (id set FOR UPDATE).
type skLockRows struct {
	ids []int64
	idx int
}

func (r *skLockRows) Next() bool { r.idx++; return r.idx <= len(r.ids) }
func (r *skLockRows) Scan(dest ...any) error {
	*(dest[0].(*int64)) = r.ids[r.idx-1]
	return nil
}
func (r *skLockRows) Err() error                                   { return nil }
func (r *skLockRows) Close()                                       {}
func (r *skLockRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *skLockRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *skLockRows) Values() ([]any, error)                       { return nil, nil }
func (r *skLockRows) RawValues() [][]byte                          { return nil }
func (r *skLockRows) Conn() *pgx.Conn                              { return nil }

// skListRows — listActiveKeys: one active non-primary key (10 columns).
type skListRows struct{ done bool }

func (r *skListRows) Next() bool {
	if r.done {
		return false
	}
	r.done = true
	return true
}
func (r *skListRows) Scan(dest ...any) error                       { return skKeyRow{}.Scan(dest...) }
func (r *skListRows) Err() error                                   { return nil }
func (r *skListRows) Close()                                       {}
func (r *skListRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *skListRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *skListRows) Values() ([]any, error)                       { return nil, nil }
func (r *skListRows) RawValues() [][]byte                          { return nil }
func (r *skListRows) Conn() *pgx.Conn                              { return nil }

// humaSigilKeyRouter assembles a chi router with ALL sigil-key routes via huma —
// the production wiring from router.go: RequirePermission(sigil.key-*) on each group +
// (for write) huma-audit-middleware variant B + the huma operation. injectClaims replaces
// RequireJWT.
func humaSigilKeyRouter(t *testing.T, enforcer apimiddleware.PermissionChecker, auditW audit.Writer) *chi.Mux {
	t.Helper()
	installHumaErrorOverride()
	svc, err := sigil.NewKeyService(sigil.KeyServiceDeps{Pool: skPool{}, Vault: skVault{}})
	if err != nil {
		t.Fatalf("sigil.NewKeyService: %v", err)
	}
	sigilKeyH := handlers.NewSigilKeyHandler(svc, nil)

	r := chi.NewRouter()
	injectClaims := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := apimiddleware.InjectClaimsForTest(req.Context(), &keeperjwt.Claims{Subject: "archon-alice"})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
	r.Route("/v1", func(r chi.Router) {
		r.Route("/sigil/keys", func(r chi.Router) {
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "sigil", "key-introduce", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaSigilKeyIntroduce(newHumaSigilKeyAPI(r, auditW, audit.EventSigilKeyIntroduced, nil), sigilKeyH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "sigil", "key-list", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaSigilKeyList(newHumaCadenceAPI(r), sigilKeyH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "sigil", "key-set-primary", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaSigilKeySetPrimary(newHumaSigilKeyAPI(r, auditW, audit.EventSigilKeyPrimarySet, nil), sigilKeyH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "sigil", "key-retire", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaSigilKeyRetire(newHumaSigilKeyAPI(r, auditW, audit.EventSigilKeyRetired, nil), sigilKeyH)
			})
		})
	})
	return r
}

// === INTRODUCE (WRITE+AUDIT sigil.key-introduced, 201 + body) ===

func TestHumaSigilKey_Introduce_201(t *testing.T) {
	r := humaSigilKeyRouter(t, strictAllowAll{}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sigil/keys", strings.NewReader(`{}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply не JSON-object: %v; body=%s", err, rec.Body.String())
	}
	// key_id/pubkey are ed25519-generated (not byte-exact); we guard the field set + types.
	for _, k := range []string{"key_id", "pubkey_pem", "is_primary", "status", "introduced_at"} {
		if _, ok := m[k]; !ok {
			t.Errorf("introduce 201-body без поля %q: %v", k, m)
		}
	}
	if m["is_primary"] != true {
		// makePrimary default — the first key becomes primary (Introduce semantics).
		t.Logf("is_primary=%v (makePrimary=false; зависит от onличия active-primary)", m["is_primary"])
	}
	// pubkey must NOT carry the private key.
	if pem, _ := m["pubkey_pem"].(string); strings.Contains(pem, "PRIVATE") {
		t.Errorf("SECURITY: introduce-ответ несёт PRIVATE-материал: %q", pem)
	}
}

func TestHumaSigilKey_Introduce_UnknownField_400(t *testing.T) {
	r := humaSigilKeyRouter(t, strictAllowAll{}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sigil/keys", strings.NewReader(`{"bogus":1}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

func TestHumaSigilKey_Introduce_RBACDeny_403(t *testing.T) {
	r := humaSigilKeyRouter(t, strictDenyAll{}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sigil/keys", strings.NewReader(`{}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaAudit_SigilKeyIntroduce_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaSigilKeyRouter(t, strictAllowAll{}, auditCap)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sigil/keys", strings.NewReader(`{}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	evs := auditCap.Events()
	if len(evs) == 0 {
		t.Fatalf("audit NOT записан on успешbutм introduce (S6-рецидив)")
	}
	if evs[0].EventType != audit.EventSigilKeyIntroduced {
		t.Errorf("event_type = %q, want %q", evs[0].EventType, audit.EventSigilKeyIntroduced)
	}
	if evs[0].ArchonAID != "archon-alice" {
		t.Errorf("archon_aid = %q, want archon-alice", evs[0].ArchonAID)
	}
	if evs[0].Payload["introduced_by_aid"] != "archon-alice" {
		t.Errorf("payload introduced_by_aid = %v, want archon-alice (payload=%+v)", evs[0].Payload["introduced_by_aid"], evs[0].Payload)
	}
	if _, ok := evs[0].Payload["key_id"]; !ok {
		t.Errorf("payload без key_id: %+v", evs[0].Payload)
	}
}

func TestHumaAudit_SigilKeyIntroduce_NoAudit_OnRBACDeny(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaSigilKeyRouter(t, strictDenyAll{}, auditCap)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sigil/keys", strings.NewReader(`{}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан on RBAC-deny introduce (%d withбытий)", len(auditCap.Events()))
	}
}

// === LIST (READ-bare, no audit) ===

func TestHumaSigilKey_List_GoldenWire(t *testing.T) {
	r := humaSigilKeyRouter(t, strictAllowAll{}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/sigil/keys", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply не JSON-object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	golden := `{"items":[{"introduced_at":"2026-06-13T10:00:00Z","is_primary":false,"key_id":"` + skTestKeyID + `","status":"active"}]}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-дрейф sigil-key.list:\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaSigilKey_List_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaSigilKeyRouter(t, strictAllowAll{}, auditCap)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/sigil/keys", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("READ-роут sigil-key.list записал audit (%d withбытий)", len(auditCap.Events()))
	}
}

func TestHumaSigilKey_List_RBACDeny_403(t *testing.T) {
	r := humaSigilKeyRouter(t, strictDenyAll{}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/sigil/keys", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// === SET-PRIMARY (WRITE+AUDIT sigil.key-primary-set, 204) ===

func TestHumaSigilKey_SetPrimary_204(t *testing.T) {
	r := humaSigilKeyRouter(t, strictAllowAll{}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sigil/keys/"+skTestKeyID+"/primary", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "" {
		t.Errorf("204-body set-primary toлжbut быть ПУСТЫМ, got %q", body)
	}
}

func TestHumaAudit_SigilKeySetPrimary_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaSigilKeyRouter(t, strictAllowAll{}, auditCap)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sigil/keys/"+skTestKeyID+"/primary", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventSigilKeyPrimarySet, map[string]any{
		"key_id": skTestKeyID, "set_by_aid": "archon-alice",
	})
}

func TestHumaAudit_SigilKeySetPrimary_NoAudit_OnBadKeyID(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaSigilKeyRouter(t, strictAllowAll{}, auditCap)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sigil/keys/NOTHEX/primary", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (битый key_id); body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан on bad-key_id set-primary (%d withбытий)", len(auditCap.Events()))
	}
}

// === RETIRE (WRITE+AUDIT sigil.key-retired, 204) ===

func TestHumaSigilKey_Retire_204(t *testing.T) {
	r := humaSigilKeyRouter(t, strictAllowAll{}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/sigil/keys/"+skTestKeyID, nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "" {
		t.Errorf("204-body retire toлжbut быть ПУСТЫМ, got %q", body)
	}
}

func TestHumaAudit_SigilKeyRetire_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaSigilKeyRouter(t, strictAllowAll{}, auditCap)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/sigil/keys/"+skTestKeyID, nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventSigilKeyRetired, map[string]any{
		"key_id": skTestKeyID, "retired_by_aid": "archon-alice",
	})
}

func TestHumaAudit_SigilKeyRetire_NoAudit_OnBadKeyID(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaSigilKeyRouter(t, strictAllowAll{}, auditCap)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/sigil/keys/NOTHEX", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (битый key_id); body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан on bad-key_id retire (%d withбытий)", len(auditCap.Events()))
	}
}

// === OpenAPI fragment: ALL sigil-key operations from FULL-TYPED Go types ===

func TestHumaSigilKey_OpenAPIFragment_3_1(t *testing.T) {
	frag, err := HumaSigilKeySpecYAML()
	if err != nil {
		t.Fatalf("HumaSigilKeySpecYAML: %v", err)
	}
	if !strings.Contains(frag, "openapi: 3.1.0") {
		t.Errorf("huma-фрагмент не несёт `openapi: 3.1.0`:\n%s", frag)
	}
	for _, want := range []string{
		"introduceSigilKey", "listSigilKeys", "setPrimarySigilKey", "retireSigilKey",
		"make_primary", "pubkey_pem",
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("OpenAPI-фрагмент не withдержит %q:\n%s", want, frag)
		}
	}
	if strings.Contains(frag, "octet-stream") {
		t.Errorf("OpenAPI-фрагмент несёт application/octet-stream:\n%s", frag)
	}
}
