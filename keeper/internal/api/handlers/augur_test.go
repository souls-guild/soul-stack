package handlers

// T5d-2c handler-native: augur (w,r) wrappers removed — HTTP is served by huma
// full-typed (huma_augur_test.go: golden-wire / unknown-field-400 / missing-required-422 /
// bad-source_type-enum-422 / bad-pagination-400 / missing-omen-422 / RBAC-403 /
// S6-audit on real huma wiring). These unit tests cover what the huma
// integration does NOT: DOMAIN classification of *Typed-function errors
// (sentinel→problem.Type) and byte-passthrough allow. They call *Typed directly, without
// httptest(w,r) — the bind/decode phase (JSON-decode / enum-validate / int-parse) is held
// by huma at the boundary, not the handler.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/augur"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// augurClaims builds a keeperjwt.Claims for calling *Typed directly.
func augurClaims(subject string) *keeperjwt.Claims { return &keeperjwt.Claims{Subject: subject} }

// augurFakePool — narrow mock of [augur.ServicePool] for AugurHandler unit tests.
// Classifies SQL by substring (omens / rites) and returns the test-provided
// outcome. Covers DOMAIN classification (sentinel→problem) + byte-passthrough;
// SQL-logic consistency is validated by augur/integration_test.go.
type augurFakePool struct {
	// omenInsertErr — error from the RETURNING scan of INSERT omens: pgErr 23505 → 409.
	omenInsertErr error
	// omenGetValues — outcome of SELECT … FROM omens WHERE name (resolve for GetOmen and
	// for InsertRite); nil → ErrNoRows (404). Also used by rite-insert.
	omenGetValues []any
	omenGetErr    error
	// omenListValues — rows of SELECT … FROM omens ORDER BY … (List).
	omenListValues [][]any
	omenCount      int
	// omenDeleteRows — RowsAffected of DELETE omens (0 → ErrNotFound).
	omenDeleteRows int64

	// riteInsertErr — error from the RETURNING scan of INSERT rites.
	riteInsertErr error
	// riteListValues — rows of SELECT … FROM rites WHERE omen (ListRites).
	riteListValues [][]any
	// riteDeleteRows — RowsAffected of DELETE rites (0 → ErrNotFound).
	riteDeleteRows int64
}

func (p *augurFakePool) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	switch {
	case contains(sql, "DELETE FROM omens"):
		return pgconn.NewCommandTag("DELETE " + itoa(p.omenDeleteRows)), nil
	case contains(sql, "DELETE FROM rites"):
		return pgconn.NewCommandTag("DELETE " + itoa(p.riteDeleteRows)), nil
	}
	return pgconn.CommandTag{}, errAugurUnexpected(sql)
}

func (p *augurFakePool) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	switch {
	case contains(sql, "INSERT INTO omens"):
		if p.omenInsertErr != nil {
			return errRow{err: p.omenInsertErr}
		}
		return augurRow{values: []any{time.Now()}} // RETURNING created_at
	case contains(sql, "INSERT INTO rites"):
		if p.riteInsertErr != nil {
			return errRow{err: p.riteInsertErr}
		}
		return augurRow{values: []any{int64(42), time.Now()}} // RETURNING id, created_at
	case contains(sql, "FROM omens") && contains(sql, "WHERE name"):
		if p.omenGetErr != nil {
			return errRow{err: p.omenGetErr}
		}
		if p.omenGetValues == nil {
			return errRow{err: pgx.ErrNoRows}
		}
		return augurRow{values: p.omenGetValues}
	case contains(sql, "SELECT COUNT(*) FROM omens"):
		return augurRow{values: []any{p.omenCount}}
	}
	return errRow{err: errAugurUnexpected(sql)}
}

func (p *augurFakePool) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	switch {
	case contains(sql, "FROM omens") && contains(sql, "ORDER BY"):
		return &augurRows{rows: p.omenListValues}, nil
	case contains(sql, "FROM rites") && contains(sql, "WHERE omen"):
		return &augurRows{rows: p.riteListValues}, nil
	}
	return nil, errAugurUnexpected(sql)
}

func errAugurUnexpected(sql string) error { return &svcErr{"augurFakePool: unexpected SQL: " + sql} }

// augurRow — staticRow for augur columns (string/time/int/int64/bool/[]byte +
// nullable pointers). Separate from the shared staticRow, which does not cover
// int64/bool/**int.
type augurRow struct {
	values []any
	err    error
}

func (r augurRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, d := range dest {
		switch d := d.(type) {
		case *string:
			*d = r.values[i].(string)
		case *int:
			*d = r.values[i].(int)
		case *int64:
			*d = r.values[i].(int64)
		case *bool:
			*d = r.values[i].(bool)
		case *time.Time:
			*d = r.values[i].(time.Time)
		case *[]byte:
			*d = r.values[i].([]byte)
		case **string:
			if r.values[i] == nil {
				*d = nil
			} else {
				s := r.values[i].(string)
				*d = &s
			}
		case **int:
			if r.values[i] == nil {
				*d = nil
			} else {
				n := r.values[i].(int)
				*d = &n
			}
		}
	}
	return nil
}

type augurRows struct {
	rows [][]any
	idx  int
}

func (r *augurRows) Next() bool { r.idx++; return r.idx <= len(r.rows) }
func (r *augurRows) Scan(dest ...any) error {
	return augurRow{values: r.rows[r.idx-1]}.Scan(dest...)
}
func (r *augurRows) Err() error                                   { return nil }
func (r *augurRows) Close()                                       {}
func (r *augurRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *augurRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *augurRows) Values() ([]any, error)                       { return nil, nil }
func (r *augurRows) RawValues() [][]byte                          { return nil }
func (r *augurRows) Conn() *pgx.Conn                              { return nil }

func newAugurHandler(t *testing.T, pool *augurFakePool) *AugurHandler {
	t.Helper()
	svc, err := augur.NewService(augur.ServiceDeps{Pool: pool})
	if err != nil {
		t.Fatalf("augur.NewService: %v", err)
	}
	return NewAugurHandler(svc, nil)
}

// wantAugurProblem checks that err is a domain *problemError with the expected problem.Type.
func wantAugurProblem(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %q, got nil", want)
	}
	d, ok := AsProblemDetails(err)
	if !ok {
		t.Fatalf("error is not *problemError: %v", err)
	}
	if d.Type != want {
		t.Errorf("problem.Type = %q, want %q", d.Type, want)
	}
}

// omenRow — an omens row (scanOmen: name, source_type, endpoint, auth_ref,
// created_by_aid, created_at).
func omenRow(name, src, endpoint, authRef string) []any {
	return []any{name, src, endpoint, authRef, nil, time.Now()}
}

// riteRow — a rites row (scanRite: id, omen, coven, sid, allow, delegate,
// token_ttl, token_num_uses, created_by_aid, created_at).
func riteRow(id int64, omen string, coven *string, allow []byte) []any {
	return []any{id, omen, anyStr(coven), nil, allow, false, nil, nil, nil, time.Now()}
}

func anyStr(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// --- Omen CreateOmenTyped: domain classification ---

func TestAugurHandler_CreateOmenTyped_201(t *testing.T) {
	h := newAugurHandler(t, &augurFakePool{})
	reply, err := h.CreateOmenTyped(context.Background(), augurClaims("archon-alice"), OmenCreateInput{
		Name: "vault-prod", SourceType: "vault", Endpoint: "https://vault:8200", AuthRef: "vault:secret/keeper/ar",
	})
	if err != nil {
		t.Fatalf("CreateOmenTyped: %v", err)
	}
	if reply.View.Name != "vault-prod" || reply.View.SourceType != "vault" {
		t.Errorf("view = %+v", reply.View)
	}
	if reply.CallerAID != "archon-alice" {
		t.Errorf("CallerAID = %q", reply.CallerAID)
	}
	// audit-payload without secrets: auth_ref is logged (a vault-ref is not a secret), allow is absent.
	p := reply.AuditPayload()
	if p["auth_ref"] != "vault:secret/keeper/ar" || p["created_by_aid"] != "archon-alice" {
		t.Errorf("audit payload = %v", p)
	}
}

func TestAugurHandler_CreateOmenTyped_BadSourceType_422(t *testing.T) {
	h := newAugurHandler(t, &augurFakePool{})
	_, err := h.CreateOmenTyped(context.Background(), augurClaims("archon-alice"), OmenCreateInput{
		Name: "x", SourceType: "redis", Endpoint: "e", AuthRef: "vault:s/p",
	})
	wantAugurProblem(t, err, problem.TypeValidationFailed)
}

func TestAugurHandler_CreateOmenTyped_BadAuthRef_422(t *testing.T) {
	h := newAugurHandler(t, &augurFakePool{})
	_, err := h.CreateOmenTyped(context.Background(), augurClaims("archon-alice"), OmenCreateInput{
		Name: "x", SourceType: "vault", Endpoint: "e", AuthRef: "plain-secret",
	})
	wantAugurProblem(t, err, problem.TypeValidationFailed)
}

func TestAugurHandler_CreateOmenTyped_Duplicate_409(t *testing.T) {
	h := newAugurHandler(t, &augurFakePool{
		omenInsertErr: &pgconn.PgError{Code: "23505", ConstraintName: "omens_pkey"},
	})
	_, err := h.CreateOmenTyped(context.Background(), augurClaims("archon-alice"), OmenCreateInput{
		Name: "vault-prod", SourceType: "vault", Endpoint: "e", AuthRef: "vault:s/p",
	})
	wantAugurProblem(t, err, problem.TypeOmenExists)
}

// --- Omen List / Get / Delete: domain classification ---

func TestAugurHandler_ListOmensTyped_200(t *testing.T) {
	h := newAugurHandler(t, &augurFakePool{
		omenCount: 2,
		omenListValues: [][]any{
			omenRow("vault-prod", "vault", "e1", "vault:s/p1"),
			omenRow("prom-main", "prometheus", "e2", "vault:s/p2"),
		},
	})
	page, err := h.ListOmensTyped(context.Background(), 0, 50)
	if err != nil {
		t.Fatalf("ListOmensTyped: %v", err)
	}
	if page.Total != 2 || len(page.Items) != 2 || page.Items[0].Name != "vault-prod" {
		t.Errorf("page = %+v", page)
	}
}

func TestAugurHandler_ListOmensTyped_Empty_NonNil(t *testing.T) {
	h := newAugurHandler(t, &augurFakePool{omenCount: 0})
	page, err := h.ListOmensTyped(context.Background(), 0, 50)
	if err != nil {
		t.Fatalf("ListOmensTyped: %v", err)
	}
	if page.Items == nil {
		t.Errorf("Items must be a non-nil empty slice")
	}
}

func TestAugurHandler_ListOmensTyped_OutOfRange_400(t *testing.T) {
	h := newAugurHandler(t, &augurFakePool{})
	_, err := h.ListOmensTyped(context.Background(), -1, 50)
	wantAugurProblem(t, err, problem.TypeMalformedRequest)
}

func TestAugurHandler_GetOmenTyped_200(t *testing.T) {
	h := newAugurHandler(t, &augurFakePool{omenGetValues: omenRow("vault-prod", "vault", "e", "vault:s/p")})
	view, err := h.GetOmenTyped(context.Background(), "vault-prod")
	if err != nil {
		t.Fatalf("GetOmenTyped: %v", err)
	}
	if view.Name != "vault-prod" {
		t.Errorf("view = %+v", view)
	}
}

func TestAugurHandler_GetOmenTyped_NotFound_404(t *testing.T) {
	h := newAugurHandler(t, &augurFakePool{omenGetValues: nil})
	_, err := h.GetOmenTyped(context.Background(), "ghost")
	wantAugurProblem(t, err, problem.TypeNotFound)
}

func TestAugurHandler_GetOmenTyped_BadName_422(t *testing.T) {
	h := newAugurHandler(t, &augurFakePool{})
	_, err := h.GetOmenTyped(context.Background(), "BAD..NAME")
	wantAugurProblem(t, err, problem.TypeValidationFailed)
}

func TestAugurHandler_DeleteOmenTyped_204(t *testing.T) {
	h := newAugurHandler(t, &augurFakePool{omenDeleteRows: 1})
	reply, err := h.DeleteOmenTyped(context.Background(), "vault-prod")
	if err != nil {
		t.Fatalf("DeleteOmenTyped: %v", err)
	}
	if reply.AuditPayload()["name"] != "vault-prod" {
		t.Errorf("audit payload = %v", reply.AuditPayload())
	}
}

func TestAugurHandler_DeleteOmenTyped_NotFound_404(t *testing.T) {
	h := newAugurHandler(t, &augurFakePool{omenDeleteRows: 0})
	_, err := h.DeleteOmenTyped(context.Background(), "ghost")
	wantAugurProblem(t, err, problem.TypeNotFound)
}

// --- Rite CreateRiteTyped: domain classification + byte-passthrough allow ---

func TestAugurHandler_CreateRiteTyped_201(t *testing.T) {
	h := newAugurHandler(t, &augurFakePool{
		omenGetValues: omenRow("vault-prod", "vault", "e", "vault:s/p"),
	})
	cov := "web"
	reply, err := h.CreateRiteTyped(context.Background(), augurClaims("archon-alice"), RiteCreateInput{
		Omen: "vault-prod", Coven: &cov, Allow: json.RawMessage(`{"paths":["secret/app"]}`),
	})
	if err != nil {
		t.Fatalf("CreateRiteTyped: %v", err)
	}
	if reply.View.Omen != "vault-prod" || reply.View.ID != 42 || reply.Subject != "coven=web" {
		t.Errorf("reply = %+v", reply)
	}
	// audit-payload: allow is NOT included (augur.md §8).
	if _, ok := reply.AuditPayload()["allow"]; ok {
		t.Errorf("allow-list must NOT appear in the audit-payload")
	}
}

// TestAugurHandler_CreateRiteTyped_AllowByteExact — guards byte-passthrough JSONB
// (ADR-051 category D). allow with a NON-lexicographic key order
// (`policies` BEFORE `paths`) must come back in RiteView.Allow BYTE-FOR-BYTE, without
// reordering. Catches a regression to a map-converter return: unmarshal→map→marshal
// would sort the keys. vault-Omen + shape {paths?, policies?} (strict schema) →
// ValidateAllow passes.
func TestAugurHandler_CreateRiteTyped_AllowByteExact(t *testing.T) {
	h := newAugurHandler(t, &augurFakePool{
		omenGetValues: omenRow("vault-prod", "vault", "e", "vault:s/p"),
	})
	// policies BEFORE paths — deliberately the reverse of lexicographic order.
	const allow = `{"policies":["app-ro"],"paths":["secret/app","secret/db"]}`
	cov := "web"
	reply, err := h.CreateRiteTyped(context.Background(), augurClaims("archon-alice"), RiteCreateInput{
		Omen: "vault-prod", Coven: &cov, Allow: json.RawMessage(allow),
	})
	if err != nil {
		t.Fatalf("CreateRiteTyped: %v", err)
	}
	// allow in RiteView is raw bytes as-is, key order untouched.
	if string(reply.View.Allow) != allow {
		t.Fatalf("allow must be preserved byte-for-byte (key order as-is); got = %s", reply.View.Allow)
	}
}

func TestAugurHandler_CreateRiteTyped_SubjectXOR_422(t *testing.T) {
	// coven and sid together — an XOR violation (checked before the Omen resolve).
	h := newAugurHandler(t, &augurFakePool{})
	cov, sid := "web", "h1.example"
	_, err := h.CreateRiteTyped(context.Background(), augurClaims("archon-alice"), RiteCreateInput{
		Omen: "vault-prod", Coven: &cov, SID: &sid, Allow: json.RawMessage(`{"paths":["x"]}`),
	})
	wantAugurProblem(t, err, problem.TypeValidationFailed)
}

func TestAugurHandler_CreateRiteTyped_OmenNotFound_404(t *testing.T) {
	h := newAugurHandler(t, &augurFakePool{omenGetValues: nil}) // Omen resolve → ErrNoRows
	cov := "web"
	_, err := h.CreateRiteTyped(context.Background(), augurClaims("archon-alice"), RiteCreateInput{
		Omen: "ghost", Coven: &cov, Allow: json.RawMessage(`{"paths":["x"]}`),
	})
	wantAugurProblem(t, err, problem.TypeNotFound)
}

func TestAugurHandler_CreateRiteTyped_BadAllowShape_422(t *testing.T) {
	// vault-Omen, allow with the prometheus shape {queries} → ValidateAllow rejects.
	h := newAugurHandler(t, &augurFakePool{
		omenGetValues: omenRow("vault-prod", "vault", "e", "vault:s/p"),
	})
	cov := "web"
	_, err := h.CreateRiteTyped(context.Background(), augurClaims("archon-alice"), RiteCreateInput{
		Omen: "vault-prod", Coven: &cov, Allow: json.RawMessage(`{"queries":["up"]}`),
	})
	wantAugurProblem(t, err, problem.TypeValidationFailed)
}

func TestAugurHandler_CreateRiteTyped_TokenWithoutDelegate_422(t *testing.T) {
	h := newAugurHandler(t, &augurFakePool{
		omenGetValues: omenRow("vault-prod", "vault", "e", "vault:s/p"),
	})
	cov, ttl := "web", "5m"
	_, err := h.CreateRiteTyped(context.Background(), augurClaims("archon-alice"), RiteCreateInput{
		Omen: "vault-prod", Coven: &cov, Allow: json.RawMessage(`{"paths":["x"]}`), TokenTTL: &ttl,
	})
	wantAugurProblem(t, err, problem.TypeValidationFailed)
}

// --- Rite List / Delete: domain classification ---

func TestAugurHandler_ListRitesTyped_200(t *testing.T) {
	cov := "web"
	h := newAugurHandler(t, &augurFakePool{
		riteListValues: [][]any{riteRow(1, "vault-prod", &cov, []byte(`{"paths":["x"]}`))},
	})
	res, err := h.ListRitesTyped(context.Background(), "vault-prod")
	if err != nil {
		t.Fatalf("ListRitesTyped: %v", err)
	}
	if len(res.Items) != 1 || res.Items[0].Omen != "vault-prod" {
		t.Errorf("res = %+v", res)
	}
}

func TestAugurHandler_ListRitesTyped_NoOmen_422(t *testing.T) {
	h := newAugurHandler(t, &augurFakePool{})
	_, err := h.ListRitesTyped(context.Background(), "")
	wantAugurProblem(t, err, problem.TypeValidationFailed)
}

func TestAugurHandler_DeleteRiteTyped_204(t *testing.T) {
	h := newAugurHandler(t, &augurFakePool{riteDeleteRows: 1})
	reply, err := h.DeleteRiteTyped(context.Background(), "42")
	if err != nil {
		t.Fatalf("DeleteRiteTyped: %v", err)
	}
	if reply.AuditPayload()["id"] != int64(42) {
		t.Errorf("audit payload = %v", reply.AuditPayload())
	}
}

func TestAugurHandler_DeleteRiteTyped_NotFound_404(t *testing.T) {
	h := newAugurHandler(t, &augurFakePool{riteDeleteRows: 0})
	_, err := h.DeleteRiteTyped(context.Background(), "99")
	wantAugurProblem(t, err, problem.TypeNotFound)
}

func TestAugurHandler_DeleteRiteTyped_BadID_422(t *testing.T) {
	h := newAugurHandler(t, &augurFakePool{})
	_, err := h.DeleteRiteTyped(context.Background(), "notanint")
	wantAugurProblem(t, err, problem.TypeValidationFailed)
}

// --- shared audit-middleware test-helpers (also used by oracle_test.go) ---

// captureAuditWriter — fake audit.Writer that captures the written Event.
type captureAuditWriter struct{ ev *audit.Event }

func (w *captureAuditWriter) Write(_ context.Context, ev *audit.Event) error {
	w.ev = ev
	return nil
}

// runWithAudit runs a (w,r) handler through the Audit middleware (the same path as
// router.go) and returns the captured payload. This reads the payload through
// the real production contract (SetAuditPayload → middleware), without internal
// accessors. Used by domains whose (w,r) routes are not yet handler-native.
func runWithAudit(t *testing.T, eventType audit.EventType, handler http.HandlerFunc, req *http.Request, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	w := &captureAuditWriter{}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	mw := middleware.Audit(w, eventType, nil, logger)
	mw(handler).ServeHTTP(rec, req)
	if rec.Code >= 300 {
		t.Fatalf("handler failed: Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	if w.ev == nil {
		t.Fatal("audit event not written")
	}
	return w.ev.Payload
}
