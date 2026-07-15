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
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// svcRegFakePool — a narrow fake for [serviceregistry.ServicePool]: handles
// exactly the SQL service-tools need (Insert/Update RETURNING, Get/List
// SELECT, Delete Exec). Business invariants of serviceregistry.Service are
// covered by the serviceregistry package's integration tests; this checks
// TRANSPORT — that the tool calls Service correctly, maps errors, encodes
// output, and writes audit.
type svcRegFakePool struct {
	insertErr  error
	updateErr  error
	deleteRows int64
	getRow     []any
	getErr     error
	listRows   [][]any
}

func (p *svcRegFakePool) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "DELETE FROM service_registry") {
		if p.deleteRows > 0 {
			return pgconn.NewCommandTag("DELETE 1"), nil
		}
		return pgconn.NewCommandTag("DELETE 0"), nil
	}
	return pgconn.CommandTag{}, &svcRegErr{"svcRegFakePool.Exec: unexpected: " + sql}
}

func (p *svcRegFakePool) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "INSERT INTO service_registry"):
		if p.insertErr != nil {
			return svcRegErrRow{p.insertErr}
		}
		now := time.Now()
		return svcRegRow{[]any{now, now}}
	case strings.Contains(sql, "UPDATE service_registry"):
		if p.updateErr != nil {
			return svcRegErrRow{p.updateErr}
		}
		now := time.Now()
		return svcRegRow{[]any{now, now}}
	case strings.Contains(sql, "FROM service_registry"):
		if p.getErr != nil {
			return svcRegErrRow{p.getErr}
		}
		if p.getRow == nil {
			return svcRegErrRow{pgx.ErrNoRows}
		}
		return svcRegRow{p.getRow}
	}
	return svcRegErrRow{&svcRegErr{"svcRegFakePool.QueryRow: unexpected: " + sql}}
}

func (p *svcRegFakePool) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "FROM service_registry") && strings.Contains(sql, "ORDER BY name") {
		return &svcRegRows{rows: p.listRows}, nil
	}
	return nil, &svcRegErr{"svcRegFakePool.Query: unexpected: " + sql}
}

type svcRegErr struct{ s string }

func (e *svcRegErr) Error() string { return e.s }

type svcRegErrRow struct{ err error }

func (r svcRegErrRow) Scan(_ ...any) error { return r.err }

// svcRegRow — single-row pgx.Row for INSERT/UPDATE RETURNING and Get.
type svcRegRow struct{ values []any }

func (r svcRegRow) Scan(dest ...any) error { return scanSvcReg(dest, r.values) }

type svcRegRows struct {
	rows [][]any
	idx  int
}

func (r *svcRegRows) Next() bool                    { r.idx++; return r.idx <= len(r.rows) }
func (r *svcRegRows) Scan(dest ...any) error        { return scanSvcReg(dest, r.rows[r.idx-1]) }
func (r *svcRegRows) Err() error                    { return nil }
func (r *svcRegRows) Close()                        {}
func (r *svcRegRows) CommandTag() pgconn.CommandTag { return pgconn.CommandTag{} }
func (r *svcRegRows) FieldDescriptions() []pgconn.FieldDescription {
	return nil
}
func (r *svcRegRows) Values() ([]any, error) { return nil, nil }
func (r *svcRegRows) RawValues() [][]byte    { return nil }
func (r *svcRegRows) Conn() *pgx.Conn        { return nil }

// scanSvcReg scatters values into dest for time.Time / string / *string.
func scanSvcReg(dest, values []any) error {
	for i, d := range dest {
		switch d := d.(type) {
		case *time.Time:
			*d = values[i].(time.Time)
		case *string:
			*d = values[i].(string)
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

func svcRegEntryRow(name, git, ref string) []any {
	now := time.Now()
	return []any{name, git, ref, nil, nil, nil, now, now}
}

// --- harness ---

// newServiceToolHandler builds a Handler with a real serviceregistry.Service
// over svcRegFakePool. pool=nil → ServiceSvc stays nil (nil-guard).
func newServiceToolHandler(t *testing.T, rbacCfg *rbactest.Config, pool *svcRegFakePool) (*Handler, *recordingAudit) {
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
		svc, err := serviceregistry.NewService(serviceregistry.ServiceDeps{Pool: pool, Logger: logger})
		if err != nil {
			t.Fatalf("serviceregistry.NewService: %v", err)
		}
		deps.ServiceSvc = svc
	}

	h, err := NewHandler(deps)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h, rec
}

// serviceAdminCfg — RBAC config granting archon-alice all service.* permissions.
func serviceAdminCfg() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "service-admin", Operators: []string{"archon-alice"}, Permissions: []string{
				"service.register", "service.update", "service.list", "service.deregister",
			}},
		},
	}
}

// --- tests: manifest ---

func TestServiceTools_InManifest(t *testing.T) {
	want := []string{
		"keeper.service.register", "keeper.service.update",
		"keeper.service.list", "keeper.service.deregister",
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

func TestServiceTools_NilGuard(t *testing.T) {
	h, _ := newServiceToolHandler(t, serviceAdminCfg(), nil) // ServiceSvc == nil
	cases := []struct {
		tool string
		args string
	}{
		{"keeper.service.register", `{"name":"web","git":"g","ref":"v1"}`},
		{"keeper.service.update", `{"name":"web","git":"g","ref":"v1"}`},
		{"keeper.service.list", `{}`},
		{"keeper.service.deregister", `{"name":"web"}`},
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

func TestServiceTools_RBACForbidden(t *testing.T) {
	// archon-alice has no service.* permissions (empty RBAC → deny all).
	h, _ := newServiceToolHandler(t, nil, &svcRegFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.service.register",
		`{"name":"web","git":"g","ref":"v1"}`)
	if resp.Error == nil {
		t.Fatal("expected forbidden error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeForbidden {
		t.Errorf("code = %q, want forbidden", data.Code)
	}
}

// --- tests: validation ---

func TestServiceTools_Validation(t *testing.T) {
	h, _ := newServiceToolHandler(t, serviceAdminCfg(), &svcRegFakePool{})
	cases := []struct {
		name string
		tool string
		args string
		want string
	}{
		{"register-no-name", "keeper.service.register", `{"git":"g","ref":"v1"}`, mcpCodeValidationFailed},
		{"register-empty-git", "keeper.service.register", `{"name":"web","git":"","ref":"v1"}`, mcpCodeValidationFailed},
		{"register-bad-refresh", "keeper.service.register", `{"name":"web","git":"g","ref":"v1","refresh":"x"}`, mcpCodeValidationFailed},
		{"update-no-name", "keeper.service.update", `{"git":"g","ref":"v1"}`, mcpCodeValidationFailed},
		{"deregister-no-name", "keeper.service.deregister", `{}`, mcpCodeValidationFailed},
		{"register-unknown-field", "keeper.service.register", `{"name":"web","git":"g","ref":"v1","x":1}`, mcpCodeMalformedRequest},
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

// --- tests: register success + audit-payload ---

func TestServiceRegister_Success(t *testing.T) {
	h, rec := newServiceToolHandler(t, serviceAdminCfg(), &svcRegFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.service.register",
		`{"name":"web","git":"https://git/web.git","ref":"v1.0.0"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}

	var res toolsCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var out serviceView
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
	if out.Name != "web" || out.Git != "https://git/web.git" || out.Ref != "v1.0.0" {
		t.Errorf("output = %+v", out)
	}

	// audit event service.registered with payload {name, git, ref, created_by_aid}.
	if len(rec.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(rec.events))
	}
	ev := rec.events[0]
	if ev.EventType != audit.EventServiceRegistered {
		t.Errorf("event_type = %q, want service.registered", ev.EventType)
	}
	if ev.Source != audit.SourceMCP {
		t.Errorf("source = %q, want mcp", ev.Source)
	}
	assertPayload(t, ev.Payload, "name", "web")
	assertPayload(t, ev.Payload, "git", "https://git/web.git")
	assertPayload(t, ev.Payload, "ref", "v1.0.0")
	assertPayload(t, ev.Payload, "created_by_aid", "archon-alice")
}

func TestServiceRegister_Duplicate409(t *testing.T) {
	h, _ := newServiceToolHandler(t, serviceAdminCfg(), &svcRegFakePool{
		insertErr: &pgconn.PgError{Code: "23505", ConstraintName: "service_registry_pkey"},
	})
	resp := callTool(t, h, "archon-alice", "keeper.service.register",
		`{"name":"web","git":"g","ref":"v1"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeServiceExists {
		t.Errorf("code = %q, want service-already-exists", data.Code)
	}
}

func TestServiceRegister_FKViolation404(t *testing.T) {
	h, _ := newServiceToolHandler(t, serviceAdminCfg(), &svcRegFakePool{
		insertErr: &pgconn.PgError{Code: "23503", ConstraintName: "service_registry_created_by_aid_fkey"},
	})
	resp := callTool(t, h, "archon-alice", "keeper.service.register",
		`{"name":"web","git":"g","ref":"v1"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeNotFound {
		t.Errorf("code = %q, want not-found", data.Code)
	}
}

// --- tests: update / list / deregister ---

func TestServiceUpdate_Success(t *testing.T) {
	h, rec := newServiceToolHandler(t, serviceAdminCfg(), &svcRegFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.service.update",
		`{"name":"web","git":"https://git/web.git","ref":"v2.0.0"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if len(rec.events) != 1 || rec.events[0].EventType != audit.EventServiceUpdated {
		t.Fatalf("expected service.updated audit, got %+v", rec.events)
	}
	assertPayload(t, rec.events[0].Payload, "ref", "v2.0.0")
}

func TestServiceUpdate_NotFound404(t *testing.T) {
	h, _ := newServiceToolHandler(t, serviceAdminCfg(), &svcRegFakePool{updateErr: pgx.ErrNoRows})
	resp := callTool(t, h, "archon-alice", "keeper.service.update",
		`{"name":"ghost","git":"g","ref":"v1"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeNotFound {
		t.Errorf("code = %q, want not-found", data.Code)
	}
}

func TestServiceList_Success(t *testing.T) {
	h, rec := newServiceToolHandler(t, serviceAdminCfg(), &svcRegFakePool{
		listRows: [][]any{svcRegEntryRow("api", "g1", "v1"), svcRegEntryRow("web", "g2", "v2")},
	})
	resp := callTool(t, h, "archon-alice", "keeper.service.list", `{}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var res toolsCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var out serviceListOutput
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
	if len(out.Services) != 2 {
		t.Fatalf("services = %d, want 2", len(out.Services))
	}
	// read-only: list is not audited.
	if len(rec.events) != 0 {
		t.Errorf("list emitted %d audit events, want 0", len(rec.events))
	}
}

func TestServiceDeregister_Success(t *testing.T) {
	h, rec := newServiceToolHandler(t, serviceAdminCfg(), &svcRegFakePool{deleteRows: 1})
	resp := callTool(t, h, "archon-alice", "keeper.service.deregister", `{"name":"web"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if len(rec.events) != 1 || rec.events[0].EventType != audit.EventServiceDeregistered {
		t.Fatalf("expected service.deregistered audit, got %+v", rec.events)
	}
	assertPayload(t, rec.events[0].Payload, "name", "web")
}

func TestServiceDeregister_NotFound404(t *testing.T) {
	h, _ := newServiceToolHandler(t, serviceAdminCfg(), &svcRegFakePool{deleteRows: 0})
	resp := callTool(t, h, "archon-alice", "keeper.service.deregister", `{"name":"ghost"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeNotFound {
		t.Errorf("code = %q, want not-found", data.Code)
	}
}

// assertPayload checks that payload[key] == want (string).
func assertPayload(t *testing.T, payload map[string]any, key, want string) {
	t.Helper()
	got, ok := payload[key]
	if !ok {
		t.Errorf("payload missing key %q", key)
		return
	}
	if s, _ := got.(string); s != want {
		t.Errorf("payload[%q] = %v, want %q", key, got, want)
	}
}
