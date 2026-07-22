package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/oracle"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// oracleFakePool — a narrow fake for [oracle.ServicePool] used by
// oracle-tools tests. Covers TRANSPORT (RBAC check / sentinel→MCP-code
// mapping / output / audit); business invariants of oracle.Service are
// covered by oracle/integration_test.go.
type oracleFakePool struct {
	vigilInsertErr  error
	vigilGetRow     []any
	vigilListRows   [][]any
	vigilCount      int
	vigilDeleteRows int64

	decreeInsertErr  error
	decreeGetRow     []any
	decreeListRows   [][]any
	decreeCount      int
	decreeDeleteRows int64
}

func (p *oracleFakePool) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	switch {
	case strings.Contains(sql, "DELETE FROM vigils"):
		return oracleDelTag(p.vigilDeleteRows), nil
	case strings.Contains(sql, "DELETE FROM decrees"):
		return oracleDelTag(p.decreeDeleteRows), nil
	}
	return pgconn.CommandTag{}, &oracleTestErr{"Exec: " + sql}
}

func (p *oracleFakePool) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "INSERT INTO vigils"):
		if p.vigilInsertErr != nil {
			return oracleErrRow{p.vigilInsertErr}
		}
		return oracleTRow{[]any{time.Now(), time.Now()}} // RETURNING created_at, updated_at
	case strings.Contains(sql, "INSERT INTO decrees"):
		if p.decreeInsertErr != nil {
			return oracleErrRow{p.decreeInsertErr}
		}
		return oracleTRow{[]any{"0s", time.Now(), time.Now()}} // RETURNING cooldown, created_at, updated_at
	case strings.Contains(sql, "FROM vigils") && strings.Contains(sql, "WHERE name"):
		if p.vigilGetRow == nil {
			return oracleErrRow{pgx.ErrNoRows}
		}
		return oracleTRow{p.vigilGetRow}
	case strings.Contains(sql, "FROM decrees") && strings.Contains(sql, "WHERE name"):
		if p.decreeGetRow == nil {
			return oracleErrRow{pgx.ErrNoRows}
		}
		return oracleTRow{p.decreeGetRow}
	case strings.Contains(sql, "SELECT COUNT(*) FROM vigils"):
		return oracleTRow{[]any{p.vigilCount}}
	case strings.Contains(sql, "SELECT COUNT(*) FROM decrees"):
		return oracleTRow{[]any{p.decreeCount}}
	}
	return oracleErrRow{&oracleTestErr{"QueryRow: " + sql}}
}

func (p *oracleFakePool) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	switch {
	case strings.Contains(sql, "FROM vigils") && strings.Contains(sql, "ORDER BY"):
		return &oracleTRows{rows: p.vigilListRows}, nil
	case strings.Contains(sql, "FROM decrees") && strings.Contains(sql, "ORDER BY"):
		return &oracleTRows{rows: p.decreeListRows}, nil
	}
	return nil, &oracleTestErr{"Query: " + sql}
}

func oracleDelTag(rows int64) pgconn.CommandTag {
	if rows > 0 {
		return pgconn.NewCommandTag("DELETE 1")
	}
	return pgconn.NewCommandTag("DELETE 0")
}

type oracleTestErr struct{ s string }

func (e *oracleTestErr) Error() string { return e.s }

type oracleErrRow struct{ err error }

func (r oracleErrRow) Scan(_ ...any) error { return r.err }

type oracleTRow struct{ values []any }

func (r oracleTRow) Scan(dest ...any) error { return scanOracleT(dest, r.values) }

type oracleTRows struct {
	rows [][]any
	idx  int
}

func (r *oracleTRows) Next() bool                                   { r.idx++; return r.idx <= len(r.rows) }
func (r *oracleTRows) Scan(dest ...any) error                       { return scanOracleT(dest, r.rows[r.idx-1]) }
func (r *oracleTRows) Err() error                                   { return nil }
func (r *oracleTRows) Close()                                       {}
func (r *oracleTRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *oracleTRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *oracleTRows) Values() ([]any, error)                       { return nil, nil }
func (r *oracleTRows) RawValues() [][]byte                          { return nil }
func (r *oracleTRows) Conn() *pgx.Conn                              { return nil }

func scanOracleT(dest, values []any) error {
	for i, d := range dest {
		switch d := d.(type) {
		case *string:
			*d = values[i].(string)
		case *int:
			*d = values[i].(int)
		case *bool:
			*d = values[i].(bool)
		case *time.Time:
			*d = values[i].(time.Time)
		case *[]string:
			if values[i] == nil {
				*d = nil
			} else {
				*d = values[i].([]string)
			}
		case *[]byte:
			if values[i] == nil {
				*d = nil
			} else {
				*d = values[i].([]byte)
			}
		case **string:
			if values[i] == nil {
				*d = nil
			} else {
				s := values[i].(string)
				*d = &s
			}
		}
	}
	return nil
}

func vigilTRow(name, interval, check string, coven []string) []any {
	now := time.Now()
	return []any{name, coven, nil, interval, check, []byte("{}"), true, now, now, nil}
}

func decreeTRow(name, onBeacon, incarnation, scenario string, coven []string) []any {
	now := time.Now()
	return []any{name, onBeacon, nil, coven, nil, incarnation, scenario, []byte("{}"), "0s", true, now, now, nil}
}

// --- harness ---

func newOracleToolHandler(t *testing.T, rbacCfg *rbactest.Config, pool *oracleFakePool) (*Handler, *recordingAudit) {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	enf, err := rbactest.NewEnforcer(rbacCfg)
	if err != nil {
		t.Fatalf("NewEnforcer: %v", err)
	}
	opSvc, err := operator.NewService(operator.ServiceDeps{
		Pool:       &fakePool{},
		Issuer:     &fakeIssuer{},
		RBAC:       enf,
		TTLDefault: time.Hour,
		Logger:     logger,
	})
	if err != nil {
		t.Fatalf("operator.NewService: %v", err)
	}

	rec := &recordingAudit{}
	deps := HandlerDeps{
		OperatorSvc:   opSvc,
		RBAC:          enf,
		AuditWriter:   rec,
		Logger:        logger,
		IncarnationDB: &fakePool{},
	}
	if pool != nil {
		where, err := oracle.NewWhereEvaluator()
		if err != nil {
			t.Fatalf("oracle.NewWhereEvaluator: %v", err)
		}
		svc, err := oracle.NewService(oracle.ServiceDeps{Pool: pool, Where: where, Logger: logger})
		if err != nil {
			t.Fatalf("oracle.NewService: %v", err)
		}
		deps.OracleSvc = svc
	}

	h, err := NewHandler(deps)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h, rec
}

// oracleAdminCfg — RBAC config granting archon-alice all vigil.*/decree.* permissions.
func oracleAdminCfg() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "oracle-admin", Operators: []string{"archon-alice"}, Permissions: []string{
				"vigil.create", "vigil.list", "vigil.delete",
				"decree.create", "decree.list", "decree.delete",
			}},
		},
	}
}

// --- tests: manifest ---

func TestOracleTools_InManifest(t *testing.T) {
	want := []string{
		"keeper.oracle.vigil.create", "keeper.oracle.vigil.list", "keeper.oracle.vigil.delete",
		"keeper.oracle.decree.create", "keeper.oracle.decree.list", "keeper.oracle.decree.delete",
	}
	for _, name := range want {
		e, ok := toolByName(name)
		if !ok {
			t.Errorf("%s missing from catalogManifest", name)
			continue
		}
		if e.status != toolStatusImplemented {
			t.Errorf("%s status = %d, want Implemented", name, e.status)
		}
	}
}

// --- tests: nil-guard ---

func TestOracleTools_NilGuard(t *testing.T) {
	h, _ := newOracleToolHandler(t, oracleAdminCfg(), nil) // OracleSvc == nil
	cases := []struct {
		tool string
		args string
	}{
		{"keeper.oracle.vigil.create", `{"name":"x","coven":["web"],"interval":"30s","check":"core.beacon.file_changed"}`},
		{"keeper.oracle.vigil.list", `{}`},
		{"keeper.oracle.vigil.delete", `{"name":"x"}`},
		{"keeper.oracle.decree.create", `{"name":"x","on_beacon":"b","coven":["db"],"incarnation_name":"prod-db","action_scenario":"restart_service"}`},
		{"keeper.oracle.decree.list", `{}`},
		{"keeper.oracle.decree.delete", `{"name":"x"}`},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			resp := callTool(t, h, "archon-alice", tc.tool, tc.args)
			if resp.Error == nil {
				t.Fatal("expected error response")
			}
			if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeInternalError {
				t.Errorf("code = %q, want internal-error", data.Code)
			}
		})
	}
}

// --- tests: RBAC enforcement ---

func TestOracleTools_RBACForbidden(t *testing.T) {
	// archon-alice has no vigil/decree permissions (empty RBAC → deny all).
	h, _ := newOracleToolHandler(t, nil, &oracleFakePool{})
	cases := []struct {
		tool string
		args string
	}{
		{"keeper.oracle.vigil.create", `{"name":"x","coven":["web"],"interval":"30s","check":"core.beacon.file_changed"}`},
		{"keeper.oracle.decree.create", `{"name":"x","on_beacon":"b","coven":["db"],"incarnation_name":"prod-db","action_scenario":"restart_service"}`},
		{"keeper.oracle.decree.delete", `{"name":"x"}`},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			resp := callTool(t, h, "archon-alice", tc.tool, tc.args)
			if resp.Error == nil {
				t.Fatal("expected forbidden error")
			}
			if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeForbidden {
				t.Errorf("code = %q, want forbidden", data.Code)
			}
		})
	}
}

// --- tests: validation ---

func TestOracleTools_Validation(t *testing.T) {
	h, _ := newOracleToolHandler(t, oracleAdminCfg(), &oracleFakePool{})
	cases := []struct {
		name string
		tool string
		args string
		want string
	}{
		{"vigil-no-name", "keeper.oracle.vigil.create", `{"coven":["web"],"interval":"30s","check":"core.beacon.file_changed"}`, mcpCodeValidationFailed},
		{"vigil-bad-interval", "keeper.oracle.vigil.create", `{"name":"x","coven":["web"],"interval":"nope","check":"core.beacon.file_changed"}`, mcpCodeValidationFailed},
		{"vigil-unknown-check", "keeper.oracle.vigil.create", `{"name":"x","coven":["web"],"interval":"30s","check":"core.beacon.bogus"}`, mcpCodeValidationFailed},
		{"vigil-subject-xor", "keeper.oracle.vigil.create", `{"name":"x","coven":["web"],"sid":"h1","interval":"30s","check":"core.beacon.file_changed"}`, mcpCodeValidationFailed},
		{"vigil-unknown-field", "keeper.oracle.vigil.create", `{"name":"x","coven":["web"],"interval":"30s","check":"core.beacon.file_changed","z":1}`, mcpCodeMalformedRequest},
		{"decree-no-name", "keeper.oracle.decree.create", `{"on_beacon":"b","coven":["db"],"incarnation_name":"prod-db","action_scenario":"restart_service"}`, mcpCodeValidationFailed},
		{"decree-bad-scenario", "keeper.oracle.decree.create", `{"name":"x","on_beacon":"b","coven":["db"],"incarnation_name":"prod-db","action_scenario":"Bad-Scenario"}`, mcpCodeValidationFailed},
		{"decree-bad-where", "keeper.oracle.decree.create", `{"name":"x","on_beacon":"b","coven":["db"],"incarnation_name":"prod-db","action_scenario":"restart_service","where":"event.data.x =="}`, mcpCodeValidationFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := callTool(t, h, "archon-alice", tc.tool, tc.args)
			if resp.Error == nil {
				t.Fatal("expected error")
			}
			if data := mustToolErrorData(t, resp.Error.Data); data.Code != tc.want {
				t.Errorf("code = %q, want %q", data.Code, tc.want)
			}
		})
	}
}

// --- tests: vigil create success + audit ---

func TestOracleVigilCreate_Success(t *testing.T) {
	h, rec := newOracleToolHandler(t, oracleAdminCfg(), &oracleFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.oracle.vigil.create",
		`{"name":"web-conf","coven":["web"],"interval":"30s","check":"core.beacon.file_changed"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}

	var res toolsCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var out vigilView
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
	if out.Name != "web-conf" || out.Check != "core.beacon.file_changed" {
		t.Errorf("output = %+v", out)
	}

	if len(rec.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(rec.events))
	}
	ev := rec.events[0]
	if ev.EventType != audit.EventVigilCreated {
		t.Errorf("event_type = %q, want vigil.created", ev.EventType)
	}
	if ev.Source != audit.SourceMCP {
		t.Errorf("source = %q, want mcp", ev.Source)
	}
	assertPayload(t, ev.Payload, "name", "web-conf")
	assertPayload(t, ev.Payload, "subject", "coven=web")
	assertPayload(t, ev.Payload, "created_by_aid", "archon-alice")
	if _, ok := ev.Payload["params"]; ok {
		t.Errorf("params leaked into audit-payload: %v", ev.Payload)
	}
}

func TestOracleVigilCreate_Duplicate409(t *testing.T) {
	h, _ := newOracleToolHandler(t, oracleAdminCfg(), &oracleFakePool{
		vigilInsertErr: &pgconn.PgError{Code: "23505", ConstraintName: "vigils_pkey"},
	})
	resp := callTool(t, h, "archon-alice", "keeper.oracle.vigil.create",
		`{"name":"web-conf","coven":["web"],"interval":"30s","check":"core.beacon.file_changed"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeVigilExists {
		t.Errorf("code = %q, want vigil-already-exists", data.Code)
	}
}

// --- tests: vigil list / delete ---

func TestOracleVigilList_Success(t *testing.T) {
	h, rec := newOracleToolHandler(t, oracleAdminCfg(), &oracleFakePool{
		vigilCount: 2,
		vigilListRows: [][]any{
			vigilTRow("web-conf", "30s", "core.beacon.file_changed", []string{"web"}),
			vigilTRow("db-svc", "1m", "core.beacon.service_down", []string{"db"}),
		},
	})
	resp := callTool(t, h, "archon-alice", "keeper.oracle.vigil.list", `{}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var res toolsCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var out vigilListOutput
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
	if len(out.Vigils) != 2 || out.Total != 2 {
		t.Fatalf("out = %+v", out)
	}
	if len(rec.events) != 0 {
		t.Errorf("list emitted %d audit events, want 0", len(rec.events))
	}
}

func TestOracleVigilList_LimitTooLarge(t *testing.T) {
	h, _ := newOracleToolHandler(t, oracleAdminCfg(), &oracleFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.oracle.vigil.list", `{"limit":5000}`)
	if resp.Error == nil {
		t.Fatal("expected validation error on limit > 1000")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Errorf("code = %q, want validation-failed", data.Code)
	}
}

func TestOracleVigilDelete_Success(t *testing.T) {
	h, rec := newOracleToolHandler(t, oracleAdminCfg(), &oracleFakePool{vigilDeleteRows: 1})
	resp := callTool(t, h, "archon-alice", "keeper.oracle.vigil.delete", `{"name":"web-conf"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if len(rec.events) != 1 || rec.events[0].EventType != audit.EventVigilDeleted {
		t.Fatalf("expected vigil.deleted audit, got %+v", rec.events)
	}
	assertPayload(t, rec.events[0].Payload, "name", "web-conf")
}

func TestOracleVigilDelete_NotFound404(t *testing.T) {
	h, _ := newOracleToolHandler(t, oracleAdminCfg(), &oracleFakePool{vigilDeleteRows: 0})
	resp := callTool(t, h, "archon-alice", "keeper.oracle.vigil.delete", `{"name":"ghost"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeNotFound {
		t.Errorf("code = %q, want not-found", data.Code)
	}
}

// --- tests: decree create / list / delete ---

func TestOracleDecreeCreate_Success(t *testing.T) {
	h, rec := newOracleToolHandler(t, oracleAdminCfg(), &oracleFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.oracle.decree.create",
		`{"name":"restart-on-down","on_beacon":"db-svc","coven":["db"],"incarnation_name":"prod-db","action_scenario":"restart_service","where":"event.data.severity == \"critical\""}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var res toolsCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var out decreeView
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
	if out.Name != "restart-on-down" || out.ActionScenario != "restart_service" {
		t.Errorf("output = %+v", out)
	}

	if len(rec.events) != 1 || rec.events[0].EventType != audit.EventDecreeCreated {
		t.Fatalf("expected decree.created audit, got %+v", rec.events)
	}
	assertPayload(t, rec.events[0].Payload, "on_beacon", "db-svc")
	assertPayload(t, rec.events[0].Payload, "subject", "coven=db")
	if _, ok := rec.events[0].Payload["where"]; ok {
		t.Errorf("where-CEL leaked into audit-payload: %v", rec.events[0].Payload)
	}
	if _, ok := rec.events[0].Payload["action_input"]; ok {
		t.Errorf("action_input leaked into audit-payload: %v", rec.events[0].Payload)
	}
}

func TestOracleDecreeList_Success(t *testing.T) {
	h, rec := newOracleToolHandler(t, oracleAdminCfg(), &oracleFakePool{
		decreeCount: 1,
		decreeListRows: [][]any{
			decreeTRow("restart-on-down", "db-svc", "prod-db", "restart_service", []string{"db"}),
		},
	})
	resp := callTool(t, h, "archon-alice", "keeper.oracle.decree.list", `{}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var res toolsCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var out decreeListOutput
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
	if len(out.Decrees) != 1 || out.Total != 1 {
		t.Fatalf("out = %+v", out)
	}
	if len(rec.events) != 0 {
		t.Errorf("list emitted %d audit events, want 0", len(rec.events))
	}
}

func TestOracleDecreeDelete_Success(t *testing.T) {
	h, rec := newOracleToolHandler(t, oracleAdminCfg(), &oracleFakePool{decreeDeleteRows: 1})
	resp := callTool(t, h, "archon-alice", "keeper.oracle.decree.delete", `{"name":"restart-on-down"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if len(rec.events) != 1 || rec.events[0].EventType != audit.EventDecreeDeleted {
		t.Fatalf("expected decree.deleted audit, got %+v", rec.events)
	}
}

func TestOracleDecreeDelete_NotFound404(t *testing.T) {
	h, _ := newOracleToolHandler(t, oracleAdminCfg(), &oracleFakePool{decreeDeleteRows: 0})
	resp := callTool(t, h, "archon-alice", "keeper.oracle.decree.delete", `{"name":"ghost"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeNotFound {
		t.Errorf("code = %q, want not-found", data.Code)
	}
}
