package handlers

// T5d-2c handler-native: the oracle (w,r) wrappers are gone — HTTP is served by huma
// full-typed (huma_oracle_test.go: golden-wire / unknown-field-400 / missing-required-
// 422 / bad-pagination-400 / RBAC-403 / S6-audit on the real huma wiring). These unit
// tests cover what the huma integration does NOT: the DOMAIN classification of errors in
// the Typed functions (sentinel→problem.Type) + byte-passthrough params/action_input + audit
// payload. They call Typed directly, without httptest(w,r) — the bind/decode phase (JSON-decode
// / int-parse) is held by huma at the boundary, not the handler.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/oracle"
)

// oracleClaims constructs keeperjwt.Claims for calling Typed directly.
func oracleClaims(subject string) *keeperjwt.Claims { return &keeperjwt.Claims{Subject: subject} }

// oracleFakePool — a narrow mock of [oracle.ServicePool] for OracleHandler unit
// tests. Classifies SQL by substring (vigils / decrees) and returns the outcome
// set by the test. Covers the DOMAIN classification (sentinel→problem) +
// byte-passthrough; SQL consistency is in oracle/integration_test.go.
type oracleFakePool struct {
	vigilInsertErr  error
	vigilGetValues  []any
	vigilGetErr     error
	vigilListValues [][]any
	vigilCount      int
	vigilDeleteRows int64

	decreeInsertErr  error
	decreeGetValues  []any
	decreeGetErr     error
	decreeListValues [][]any
	decreeCount      int
	decreeDeleteRows int64
}

func (p *oracleFakePool) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	switch {
	case contains(sql, "DELETE FROM vigils"):
		return pgconn.NewCommandTag("DELETE " + itoa(p.vigilDeleteRows)), nil
	case contains(sql, "DELETE FROM decrees"):
		return pgconn.NewCommandTag("DELETE " + itoa(p.decreeDeleteRows)), nil
	}
	return pgconn.CommandTag{}, &svcErr{"oracleFakePool: unexpected Exec: " + sql}
}

func (p *oracleFakePool) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	switch {
	case contains(sql, "INSERT INTO vigils"):
		if p.vigilInsertErr != nil {
			return errRow{err: p.vigilInsertErr}
		}
		return oracleRow{values: []any{time.Now(), time.Now()}} // RETURNING created_at, updated_at
	case contains(sql, "INSERT INTO decrees"):
		if p.decreeInsertErr != nil {
			return errRow{err: p.decreeInsertErr}
		}
		return oracleRow{values: []any{"0s", time.Now(), time.Now()}} // RETURNING cooldown, created_at, updated_at
	case contains(sql, "FROM vigils") && contains(sql, "WHERE name"):
		if p.vigilGetErr != nil {
			return errRow{err: p.vigilGetErr}
		}
		if p.vigilGetValues == nil {
			return errRow{err: pgx.ErrNoRows}
		}
		return oracleRow{values: p.vigilGetValues}
	case contains(sql, "FROM decrees") && contains(sql, "WHERE name"):
		if p.decreeGetErr != nil {
			return errRow{err: p.decreeGetErr}
		}
		if p.decreeGetValues == nil {
			return errRow{err: pgx.ErrNoRows}
		}
		return oracleRow{values: p.decreeGetValues}
	case contains(sql, "SELECT COUNT(*) FROM vigils"):
		return oracleRow{values: []any{p.vigilCount}}
	case contains(sql, "SELECT COUNT(*) FROM decrees"):
		return oracleRow{values: []any{p.decreeCount}}
	}
	return errRow{err: &svcErr{"oracleFakePool: unexpected QueryRow: " + sql}}
}

func (p *oracleFakePool) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	switch {
	case contains(sql, "FROM vigils") && contains(sql, "ORDER BY"):
		return &oracleRows{rows: p.vigilListValues}, nil
	case contains(sql, "FROM decrees") && contains(sql, "ORDER BY"):
		return &oracleRows{rows: p.decreeListValues}, nil
	}
	return nil, &svcErr{"oracleFakePool: unexpected Query: " + sql}
}

// oracleRow — a staticRow for oracle columns: adds *[]string (coven) and
// json.RawMessage (params/action_input via *[]byte) to the augur set.
type oracleRow struct {
	values []any
	err    error
}

func (r oracleRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, d := range dest {
		switch d := d.(type) {
		case *string:
			*d = r.values[i].(string)
		case *int:
			*d = r.values[i].(int)
		case *bool:
			*d = r.values[i].(bool)
		case *time.Time:
			*d = r.values[i].(time.Time)
		case *[]string:
			if r.values[i] == nil {
				*d = nil
			} else {
				*d = r.values[i].([]string)
			}
		case *[]byte:
			if r.values[i] == nil {
				*d = nil
			} else {
				*d = r.values[i].([]byte)
			}
		case **string:
			if r.values[i] == nil {
				*d = nil
			} else {
				s := r.values[i].(string)
				*d = &s
			}
		}
	}
	return nil
}

type oracleRows struct {
	rows [][]any
	idx  int
}

func (r *oracleRows) Next() bool { r.idx++; return r.idx <= len(r.rows) }
func (r *oracleRows) Scan(dest ...any) error {
	return oracleRow{values: r.rows[r.idx-1]}.Scan(dest...)
}
func (r *oracleRows) Err() error                                   { return nil }
func (r *oracleRows) Close()                                       {}
func (r *oracleRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *oracleRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *oracleRows) Values() ([]any, error)                       { return nil, nil }
func (r *oracleRows) RawValues() [][]byte                          { return nil }
func (r *oracleRows) Conn() *pgx.Conn                              { return nil }

func newOracleHandler(t *testing.T, pool *oracleFakePool) *OracleHandler {
	t.Helper()
	where, err := oracle.NewWhereEvaluator()
	if err != nil {
		t.Fatalf("oracle.NewWhereEvaluator: %v", err)
	}
	svc, err := oracle.NewService(oracle.ServiceDeps{Pool: pool, Where: where})
	if err != nil {
		t.Fatalf("oracle.NewService: %v", err)
	}
	return NewOracleHandler(svc, nil)
}

// wantOracleProblem checks that err is a domain *problemError with the expected problem.Type.
func wantOracleProblem(t *testing.T, err error, want string) {
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

func covenPtr(v ...string) *[]string { s := append([]string{}, v...); return &s }

// vigilRow — a vigils row (collectVigils: name, coven, sid, interval_spec,
// check_addr, params, enabled, created_at, updated_at, created_by_aid).
func vigilRow(name, interval, check string, coven []string) []any {
	now := time.Now()
	return []any{name, coven, nil, interval, check, []byte("{}"), true, now, now, nil}
}

// decreeRow — a decrees row (collectDecrees: name, on_beacon, where_cel,
// subject_coven, subject_sid, incarnation_name, action_scenario, action_input,
// cooldown, enabled, created_at, updated_at, created_by_aid).
func decreeRow(name, onBeacon, incarnation, scenario string, coven []string) []any {
	now := time.Now()
	return []any{name, onBeacon, nil, coven, nil, incarnation, scenario, []byte("{}"), "0s", true, now, now, nil}
}

// --- Vigil CreateVigilTyped: domain classification ---

func TestOracleHandler_CreateVigilTyped_201(t *testing.T) {
	h := newOracleHandler(t, &oracleFakePool{})
	reply, err := h.CreateVigilTyped(context.Background(), oracleClaims("archon-alice"), VigilCreateInput{
		Name: "web-conf", Coven: covenPtr("web"), Interval: "30s", Check: "core.beacon.file_changed",
	})
	if err != nil {
		t.Fatalf("CreateVigilTyped: %v", err)
	}
	if reply.View.Name != "web-conf" || reply.View.Check != "core.beacon.file_changed" {
		t.Errorf("view = %+v", reply.View)
	}
	if reply.CallerAID != "archon-alice" {
		t.Errorf("CallerAID = %q", reply.CallerAID)
	}
}

func TestOracleHandler_CreateVigilTyped_BadInterval_422(t *testing.T) {
	h := newOracleHandler(t, &oracleFakePool{})
	_, err := h.CreateVigilTyped(context.Background(), oracleClaims("archon-alice"), VigilCreateInput{
		Name: "x", Coven: covenPtr("web"), Interval: "notaduration", Check: "core.beacon.file_changed",
	})
	wantOracleProblem(t, err, problem.TypeValidationFailed)
}

func TestOracleHandler_CreateVigilTyped_UnknownCheck_422(t *testing.T) {
	h := newOracleHandler(t, &oracleFakePool{})
	_, err := h.CreateVigilTyped(context.Background(), oracleClaims("archon-alice"), VigilCreateInput{
		Name: "x", Coven: covenPtr("web"), Interval: "30s", Check: "core.beacon.bogus",
	})
	wantOracleProblem(t, err, problem.TypeValidationFailed)
}

func TestOracleHandler_CreateVigilTyped_SubjectXOR_422(t *testing.T) {
	h := newOracleHandler(t, &oracleFakePool{})
	sid := "h1.example"
	_, err := h.CreateVigilTyped(context.Background(), oracleClaims("archon-alice"), VigilCreateInput{
		Name: "x", Coven: covenPtr("web"), SID: &sid, Interval: "30s", Check: "core.beacon.file_changed",
	})
	wantOracleProblem(t, err, problem.TypeValidationFailed)
}

func TestOracleHandler_CreateVigilTyped_Duplicate_409(t *testing.T) {
	h := newOracleHandler(t, &oracleFakePool{
		vigilInsertErr: &pgconn.PgError{Code: "23505", ConstraintName: "vigils_pkey"},
	})
	_, err := h.CreateVigilTyped(context.Background(), oracleClaims("archon-alice"), VigilCreateInput{
		Name: "web-conf", Coven: covenPtr("web"), Interval: "30s", Check: "core.beacon.file_changed",
	})
	wantOracleProblem(t, err, problem.TypeVigilExists)
}

// TestOracleHandler_CreateVigilTyped_ParamsByteExact — guards byte-passthrough JSONB
// (ADR-051 category D). params with a NON-lexicographic key order (`zzz` BEFORE
// `a`/`mmm`) must come back in VigilView.Params BYTE-FOR-BYTE, without reordering.
// Catches a regression to a map converter: unmarshal→map→marshal would sort the keys.
// INSERT...RETURNING returns only created_at/updated_at → params in the reply = the raw
// bytes of the request body.
func TestOracleHandler_CreateVigilTyped_ParamsByteExact(t *testing.T) {
	h := newOracleHandler(t, &oracleFakePool{})
	const params = `{"zzz":1,"a":2,"mmm":3}`
	raw := json.RawMessage(params)
	reply, err := h.CreateVigilTyped(context.Background(), oracleClaims("archon-alice"), VigilCreateInput{
		Name: "web-conf", Coven: covenPtr("web"), Interval: "30s", Check: "core.beacon.file_changed", Params: &raw,
	})
	if err != nil {
		t.Fatalf("CreateVigilTyped: %v", err)
	}
	if string(reply.View.Params) != params {
		t.Fatalf("params should be preserved byte-for-byte (key order as-is); got = %s", reply.View.Params)
	}
}

// --- Vigil List / Get / Delete: domain classification ---

func TestOracleHandler_ListVigilsTyped_200(t *testing.T) {
	h := newOracleHandler(t, &oracleFakePool{
		vigilCount: 2,
		vigilListValues: [][]any{
			vigilRow("web-conf", "30s", "core.beacon.file_changed", []string{"web"}),
			vigilRow("db-svc", "1m", "core.beacon.service_down", []string{"db"}),
		},
	})
	page, err := h.ListVigilsTyped(context.Background(), 0, 50)
	if err != nil {
		t.Fatalf("ListVigilsTyped: %v", err)
	}
	if page.Total != 2 || len(page.Items) != 2 || page.Items[0].Name != "web-conf" {
		t.Errorf("page = %+v", page)
	}
}

func TestOracleHandler_ListVigilsTyped_Empty_NonNil(t *testing.T) {
	h := newOracleHandler(t, &oracleFakePool{vigilCount: 0})
	page, err := h.ListVigilsTyped(context.Background(), 0, 50)
	if err != nil {
		t.Fatalf("ListVigilsTyped: %v", err)
	}
	if page.Items == nil {
		t.Errorf("Items should be a non-nil empty slice")
	}
}

func TestOracleHandler_ListVigilsTyped_OutOfRange_400(t *testing.T) {
	h := newOracleHandler(t, &oracleFakePool{})
	_, err := h.ListVigilsTyped(context.Background(), -1, 50)
	wantOracleProblem(t, err, problem.TypeMalformedRequest)
}

func TestOracleHandler_GetVigilTyped_200(t *testing.T) {
	h := newOracleHandler(t, &oracleFakePool{
		vigilGetValues: vigilRow("web-conf", "30s", "core.beacon.file_changed", []string{"web"}),
	})
	view, err := h.GetVigilTyped(context.Background(), "web-conf")
	if err != nil {
		t.Fatalf("GetVigilTyped: %v", err)
	}
	if view.Name != "web-conf" {
		t.Errorf("view = %+v", view)
	}
}

func TestOracleHandler_GetVigilTyped_NotFound_404(t *testing.T) {
	h := newOracleHandler(t, &oracleFakePool{vigilGetValues: nil})
	_, err := h.GetVigilTyped(context.Background(), "ghost")
	wantOracleProblem(t, err, problem.TypeNotFound)
}

func TestOracleHandler_GetVigilTyped_BadName_422(t *testing.T) {
	h := newOracleHandler(t, &oracleFakePool{})
	_, err := h.GetVigilTyped(context.Background(), "BAD..NAME")
	wantOracleProblem(t, err, problem.TypeValidationFailed)
}

func TestOracleHandler_DeleteVigilTyped_204(t *testing.T) {
	h := newOracleHandler(t, &oracleFakePool{vigilDeleteRows: 1})
	reply, err := h.DeleteVigilTyped(context.Background(), "web-conf")
	if err != nil {
		t.Fatalf("DeleteVigilTyped: %v", err)
	}
	if reply.AuditPayload()["name"] != "web-conf" {
		t.Errorf("audit payload = %v", reply.AuditPayload())
	}
}

func TestOracleHandler_DeleteVigilTyped_NotFound_404(t *testing.T) {
	h := newOracleHandler(t, &oracleFakePool{vigilDeleteRows: 0})
	_, err := h.DeleteVigilTyped(context.Background(), "ghost")
	wantOracleProblem(t, err, problem.TypeNotFound)
}

// --- Decree CreateDecreeTyped: domain classification ---

func TestOracleHandler_CreateDecreeTyped_201(t *testing.T) {
	h := newOracleHandler(t, &oracleFakePool{})
	reply, err := h.CreateDecreeTyped(context.Background(), oracleClaims("archon-alice"), DecreeCreateInput{
		Name: "restart-on-down", OnBeacon: "db-svc", Coven: covenPtr("db"), IncarnationName: "prod-db", ActionScenario: "restart_service",
	})
	if err != nil {
		t.Fatalf("CreateDecreeTyped: %v", err)
	}
	if reply.View.Name != "restart-on-down" || reply.View.ActionScenario != "restart_service" {
		t.Errorf("view = %+v", reply.View)
	}
}

func TestOracleHandler_CreateDecreeTyped_BadIncarnation_422(t *testing.T) {
	h := newOracleHandler(t, &oracleFakePool{})
	_, err := h.CreateDecreeTyped(context.Background(), oracleClaims("archon-alice"), DecreeCreateInput{
		Name: "x", OnBeacon: "db-svc", Coven: covenPtr("db"), IncarnationName: "BAD..NAME", ActionScenario: "restart_service",
	})
	wantOracleProblem(t, err, problem.TypeValidationFailed)
}

func TestOracleHandler_CreateDecreeTyped_BadScenario_422(t *testing.T) {
	h := newOracleHandler(t, &oracleFakePool{})
	_, err := h.CreateDecreeTyped(context.Background(), oracleClaims("archon-alice"), DecreeCreateInput{
		Name: "x", OnBeacon: "db-svc", Coven: covenPtr("db"), IncarnationName: "prod-db", ActionScenario: "Bad-Scenario",
	})
	wantOracleProblem(t, err, problem.TypeValidationFailed)
}

func TestOracleHandler_CreateDecreeTyped_BadWhereCEL_422(t *testing.T) {
	h := newOracleHandler(t, &oracleFakePool{})
	where := "event.data.x =="
	_, err := h.CreateDecreeTyped(context.Background(), oracleClaims("archon-alice"), DecreeCreateInput{
		Name: "x", OnBeacon: "db-svc", Coven: covenPtr("db"), IncarnationName: "prod-db", ActionScenario: "restart_service", Where: &where,
	})
	wantOracleProblem(t, err, problem.TypeValidationFailed)
}

func TestOracleHandler_CreateDecreeTyped_ValidWhereCEL_201(t *testing.T) {
	h := newOracleHandler(t, &oracleFakePool{})
	where := `event.data.severity == "critical"`
	_, err := h.CreateDecreeTyped(context.Background(), oracleClaims("archon-alice"), DecreeCreateInput{
		Name: "crit", OnBeacon: "db-svc", Coven: covenPtr("db"), IncarnationName: "prod-db", ActionScenario: "restart_service", Where: &where,
	})
	if err != nil {
		t.Fatalf("CreateDecreeTyped: %v", err)
	}
}

func TestOracleHandler_CreateDecreeTyped_SubjectXOR_422(t *testing.T) {
	h := newOracleHandler(t, &oracleFakePool{})
	_, err := h.CreateDecreeTyped(context.Background(), oracleClaims("archon-alice"), DecreeCreateInput{
		Name: "x", OnBeacon: "db-svc", IncarnationName: "prod-db", ActionScenario: "restart_service",
	})
	wantOracleProblem(t, err, problem.TypeValidationFailed)
}

func TestOracleHandler_CreateDecreeTyped_Duplicate_409(t *testing.T) {
	h := newOracleHandler(t, &oracleFakePool{
		decreeInsertErr: &pgconn.PgError{Code: "23505", ConstraintName: "decrees_pkey"},
	})
	_, err := h.CreateDecreeTyped(context.Background(), oracleClaims("archon-alice"), DecreeCreateInput{
		Name: "restart-on-down", OnBeacon: "db-svc", Coven: covenPtr("db"), IncarnationName: "prod-db", ActionScenario: "restart_service",
	})
	wantOracleProblem(t, err, problem.TypeDecreeExists)
}

// TestOracleHandler_CreateDecreeTyped_ActionInputByteExact — guards byte-passthrough
// JSONB (ADR-051 category D). action_input with a NON-lexicographic key order must come
// back in DecreeView.ActionInput BYTE-FOR-BYTE. INSERT...RETURNING returns only
// cooldown/created_at/updated_at → action_input in the reply = the raw bytes of the request body.
func TestOracleHandler_CreateDecreeTyped_ActionInputByteExact(t *testing.T) {
	h := newOracleHandler(t, &oracleFakePool{})
	const actionInput = `{"zzz":1,"a":2,"mmm":3}`
	raw := json.RawMessage(actionInput)
	reply, err := h.CreateDecreeTyped(context.Background(), oracleClaims("archon-alice"), DecreeCreateInput{
		Name: "restart-on-down", OnBeacon: "db-svc", Coven: covenPtr("db"), IncarnationName: "prod-db", ActionScenario: "restart_service", ActionInput: &raw,
	})
	if err != nil {
		t.Fatalf("CreateDecreeTyped: %v", err)
	}
	if string(reply.View.ActionInput) != actionInput {
		t.Fatalf("action_input should be preserved byte-for-byte (key order as-is); got = %s", reply.View.ActionInput)
	}
}

// --- Decree List / Get / Delete ---

func TestOracleHandler_ListDecreesTyped_200(t *testing.T) {
	h := newOracleHandler(t, &oracleFakePool{
		decreeCount: 1,
		decreeListValues: [][]any{
			decreeRow("restart-on-down", "db-svc", "prod-db", "restart_service", []string{"db"}),
		},
	})
	page, err := h.ListDecreesTyped(context.Background(), 0, 50)
	if err != nil {
		t.Fatalf("ListDecreesTyped: %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Name != "restart-on-down" {
		t.Errorf("page = %+v", page)
	}
}

func TestOracleHandler_GetDecreeTyped_NotFound_404(t *testing.T) {
	h := newOracleHandler(t, &oracleFakePool{decreeGetValues: nil})
	_, err := h.GetDecreeTyped(context.Background(), "ghost")
	wantOracleProblem(t, err, problem.TypeNotFound)
}

func TestOracleHandler_DeleteDecreeTyped_204(t *testing.T) {
	h := newOracleHandler(t, &oracleFakePool{decreeDeleteRows: 1})
	reply, err := h.DeleteDecreeTyped(context.Background(), "restart-on-down")
	if err != nil {
		t.Fatalf("DeleteDecreeTyped: %v", err)
	}
	if reply.AuditPayload()["name"] != "restart-on-down" {
		t.Errorf("audit payload = %v", reply.AuditPayload())
	}
}

func TestOracleHandler_DeleteDecreeTyped_NotFound_404(t *testing.T) {
	h := newOracleHandler(t, &oracleFakePool{decreeDeleteRows: 0})
	_, err := h.DeleteDecreeTyped(context.Background(), "ghost")
	wantOracleProblem(t, err, problem.TypeNotFound)
}

// --- audit payload without secrets ---

// TestOracleHandler_CreateVigilTyped_AuditPayload — the vigil.created payload carries
// name/check/interval/subject/created_by_aid; params is absent from the payload.
func TestOracleHandler_CreateVigilTyped_AuditPayload(t *testing.T) {
	h := newOracleHandler(t, &oracleFakePool{})
	reply, err := h.CreateVigilTyped(context.Background(), oracleClaims("archon-alice"), VigilCreateInput{
		Name: "web-conf", Coven: covenPtr("web"), Interval: "30s", Check: "core.beacon.file_changed",
	})
	if err != nil {
		t.Fatalf("CreateVigilTyped: %v", err)
	}
	p := reply.AuditPayload()
	if p["name"] != "web-conf" || p["check"] != "core.beacon.file_changed" {
		t.Errorf("payload = %v", p)
	}
	if p["subject"] != "coven=web" {
		t.Errorf("subject = %v", p["subject"])
	}
	if _, ok := p["params"]; ok {
		t.Errorf("params must NOT end up in the audit-payload: %v", p)
	}
	if p["created_by_aid"] != "archon-alice" {
		t.Errorf("created_by_aid = %v", p["created_by_aid"])
	}
}

// TestOracleHandler_CreateDecreeTyped_AuditPayload — the decree.created payload carries
// name/on_beacon/incarnation/action_scenario/subject; where-CEL and action_input are
// absent from the payload (action_input may carry a vault-ref in transit).
func TestOracleHandler_CreateDecreeTyped_AuditPayload(t *testing.T) {
	h := newOracleHandler(t, &oracleFakePool{})
	where := `event.data.severity == "critical"`
	reply, err := h.CreateDecreeTyped(context.Background(), oracleClaims("archon-alice"), DecreeCreateInput{
		Name: "restart-on-down", OnBeacon: "db-svc", Coven: covenPtr("db"), IncarnationName: "prod-db", ActionScenario: "restart_service", Where: &where,
	})
	if err != nil {
		t.Fatalf("CreateDecreeTyped: %v", err)
	}
	p := reply.AuditPayload()
	if p["name"] != "restart-on-down" || p["on_beacon"] != "db-svc" {
		t.Errorf("payload = %v", p)
	}
	if p["subject"] != "coven=db" {
		t.Errorf("subject = %v", p["subject"])
	}
	if _, ok := p["where"]; ok {
		t.Errorf("where-CEL must NOT end up in the audit-payload: %v", p)
	}
	if _, ok := p["action_input"]; ok {
		t.Errorf("action_input must NOT end up in the audit-payload: %v", p)
	}
}
