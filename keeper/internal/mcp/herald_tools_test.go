package mcp

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/herald"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// heraldFakePool — narrow fake for [herald.ExecQueryRower] used by
// herald/tiding-tools tests. Covers TRANSPORT (RBAC / sentinel→MCP-code
// mapping / output / audit / invalidation); herald.Service business
// invariants are covered by herald/integration_test.go.
type heraldFakePool struct {
	insertErr error // INSERT error (nil → timestamps)
	getRow    []any // SELECT row by name; nil → ErrNoRows
	updateTag int64 // RowsAffected for UPDATE/DELETE
}

func (p *heraldFakePool) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "UPDATE") || strings.Contains(sql, "DELETE") {
		if p.updateTag > 0 {
			return pgconn.NewCommandTag("UPDATE 1"), nil
		}
		return pgconn.NewCommandTag("UPDATE 0"), nil
	}
	return pgconn.CommandTag{}, nil
}

func (p *heraldFakePool) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	if strings.Contains(sql, "INSERT") {
		if p.insertErr != nil {
			return hErrRow{p.insertErr}
		}
		now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
		return hValRow{[]any{now, now}}
	}
	if p.getRow == nil {
		return hErrRow{pgx.ErrNoRows}
	}
	return hValRow{p.getRow}
}

func (p *heraldFakePool) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return &hEmptyRows{}, nil
}

type hErrRow struct{ err error }

func (r hErrRow) Scan(...any) error { return r.err }

type hValRow struct{ vals []any }

func (r hValRow) Scan(dest ...any) error {
	for i := range dest {
		if i >= len(r.vals) {
			break
		}
		switch d := dest[i].(type) {
		case *time.Time:
			if v, ok := r.vals[i].(time.Time); ok {
				*d = v
			}
		case *string:
			if v, ok := r.vals[i].(string); ok {
				*d = v
			}
		case **string:
			if v, ok := r.vals[i].(*string); ok {
				*d = v
			}
		case *bool:
			if v, ok := r.vals[i].(bool); ok {
				*d = v
			}
		case *[]byte:
			if v, ok := r.vals[i].([]byte); ok {
				*d = v
			}
		case *[]string:
			if v, ok := r.vals[i].([]string); ok {
				*d = v
			}
		}
	}
	return nil
}

type hEmptyRows struct{}

func (*hEmptyRows) Next() bool                                   { return false }
func (*hEmptyRows) Scan(...any) error                            { return nil }
func (*hEmptyRows) Err() error                                   { return nil }
func (*hEmptyRows) Close()                                       {}
func (*hEmptyRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (*hEmptyRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (*hEmptyRows) Values() ([]any, error)                       { return nil, nil }
func (*hEmptyRows) RawValues() [][]byte                          { return nil }
func (*hEmptyRows) Conn() *pgx.Conn                              { return nil }

// fakeInvalidator — counts InvalidateRules calls (guards CRUD invalidation).
type fakeInvalidator struct{ calls int }

func (f *fakeInvalidator) InvalidateRules() { f.calls++ }

func newHeraldToolHandler(t *testing.T, rbacCfg *rbactest.Config, pool *heraldFakePool, inv herald.Invalidator) (*Handler, *recordingAudit) {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	enf, err := rbactest.NewEnforcer(rbacCfg)
	if err != nil {
		t.Fatalf("NewEnforcer: %v", err)
	}
	opSvc, err := operator.NewService(operator.ServiceDeps{
		Pool: &fakePool{}, Issuer: &fakeIssuer{}, RBAC: enf, TTLDefault: time.Hour, Logger: logger,
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
		svc, err := herald.NewService(herald.ServiceDeps{Pool: pool, Invalidator: inv, Logger: logger})
		if err != nil {
			t.Fatalf("herald.NewService: %v", err)
		}
		deps.HeraldSvc = svc
	}
	h, err := NewHandler(deps)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h, rec
}

// heraldAdminCfg — RBAC granting archon-alice all herald.*/tiding.*-permissions.
func heraldAdminCfg() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "herald-admin", Operators: []string{"archon-alice"}, Permissions: []string{
				"herald.create", "herald.read", "herald.list", "herald.update", "herald.delete",
				"tiding.create", "tiding.read", "tiding.list", "tiding.update", "tiding.delete",
			}},
		},
	}
}

// --- manifest ---

func TestHeraldTools_InManifest(t *testing.T) {
	want := []string{
		"keeper.herald.create", "keeper.herald.update", "keeper.herald.delete",
		"keeper.herald.list", "keeper.herald.read",
		"keeper.tiding.create", "keeper.tiding.update", "keeper.tiding.delete",
		"keeper.tiding.list", "keeper.tiding.read",
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

// --- nil-guard ---

func TestHeraldTools_NilGuard(t *testing.T) {
	h, _ := newHeraldToolHandler(t, heraldAdminCfg(), nil, nil) // HeraldSvc == nil
	cases := []struct{ tool, args string }{
		{"keeper.herald.create", `{"name":"x","type":"webhook","config":{"url":"https://x/y"}}`},
		{"keeper.herald.list", `{}`},
		{"keeper.herald.read", `{"name":"x"}`},
		{"keeper.tiding.create", `{"name":"t","herald":"x","event_types":["scenario_run.*"]}`},
		{"keeper.tiding.delete", `{"name":"t"}`},
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

// --- RBAC enforcement ---

func TestHeraldTools_RBACForbidden(t *testing.T) {
	// archon-alice without herald/tiding-permissions (empty RBAC → deny all).
	h, _ := newHeraldToolHandler(t, nil, &heraldFakePool{}, nil)
	cases := []struct{ tool, args string }{
		{"keeper.herald.create", `{"name":"x","type":"webhook","config":{"url":"https://x/y"}}`},
		{"keeper.tiding.create", `{"name":"t","herald":"x","event_types":["scenario_run.*"]}`},
		{"keeper.tiding.delete", `{"name":"t"}`},
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

// --- success + audit + invalidation ---

func TestHeraldCreate_Success_AuditAndInvalidate(t *testing.T) {
	inv := &fakeInvalidator{}
	h, rec := newHeraldToolHandler(t, heraldAdminCfg(), &heraldFakePool{}, inv)
	resp := callTool(t, h, "archon-alice", "keeper.herald.create",
		`{"name":"slack-ops","type":"webhook","config":{"url":"https://hooks.example.com/x"}}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if inv.calls != 1 {
		t.Errorf("InvalidateRules calls = %d, want 1 (CRUD-мутация инвалидирует снимок)", inv.calls)
	}
	if len(rec.events) != 1 || rec.events[0].EventType != audit.EventHeraldCreated {
		t.Errorf("audit events = %v, want one herald.created", rec.events)
	}
}

func TestHeraldCreate_Duplicate409(t *testing.T) {
	pool := &heraldFakePool{insertErr: &pgconn.PgError{Code: "23505", ConstraintName: "heralds_pkey"}}
	h, _ := newHeraldToolHandler(t, heraldAdminCfg(), pool, &fakeInvalidator{})
	resp := callTool(t, h, "archon-alice", "keeper.herald.create",
		`{"name":"dup","type":"webhook","config":{"url":"https://x/y"}}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeHeraldExists {
		t.Errorf("code = %q, want herald-already-exists", data.Code)
	}
}

func TestHeraldRead_NotFound404(t *testing.T) {
	h, _ := newHeraldToolHandler(t, heraldAdminCfg(), &heraldFakePool{}, &fakeInvalidator{})
	resp := callTool(t, h, "archon-alice", "keeper.herald.read", `{"name":"ghost"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeNotFound {
		t.Errorf("code = %q, want not-found", data.Code)
	}
}

func TestHeraldCreate_BadConfig_Validation422(t *testing.T) {
	h, _ := newHeraldToolHandler(t, heraldAdminCfg(), &heraldFakePool{}, &fakeInvalidator{})
	// http:// + private IP without opt-out → SSRF-guard → validation-failed.
	resp := callTool(t, h, "archon-alice", "keeper.herald.create",
		`{"name":"insecure","type":"webhook","config":{"url":"http://10.0.0.1/h"}}`)
	if resp.Error == nil {
		t.Fatal("expected validation error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Errorf("code = %q, want validation-failed", data.Code)
	}
}

// --- Tiding parity ---

func TestTidingCreate_MissingHerald404(t *testing.T) {
	pool := &heraldFakePool{insertErr: &pgconn.PgError{Code: "23503", ConstraintName: "tidings_herald_fk"}}
	h, _ := newHeraldToolHandler(t, heraldAdminCfg(), pool, &fakeInvalidator{})
	resp := callTool(t, h, "archon-alice", "keeper.tiding.create",
		`{"name":"t1","herald":"ghost","event_types":["scenario_run.*"]}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeNotFound {
		t.Errorf("code = %q, want not-found (FK на missing herald)", data.Code)
	}
}

func TestTidingCreate_BadEventTypes422(t *testing.T) {
	h, _ := newHeraldToolHandler(t, heraldAdminCfg(), &heraldFakePool{}, &fakeInvalidator{})
	resp := callTool(t, h, "archon-alice", "keeper.tiding.create",
		`{"name":"t1","herald":"ch","event_types":["operator.created"]}`)
	if resp.Error == nil {
		t.Fatal("expected validation error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Errorf("code = %q, want validation-failed", data.Code)
	}
}

func TestTidingDelete_Success_Invalidate(t *testing.T) {
	inv := &fakeInvalidator{}
	pool := &heraldFakePool{updateTag: 1}
	h, rec := newHeraldToolHandler(t, heraldAdminCfg(), pool, inv)
	resp := callTool(t, h, "archon-alice", "keeper.tiding.delete", `{"name":"t1"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if inv.calls != 1 {
		t.Errorf("InvalidateRules calls = %d, want 1", inv.calls)
	}
	if len(rec.events) != 1 || rec.events[0].EventType != audit.EventTidingDeleted {
		t.Errorf("audit events = %v, want one tiding.deleted", rec.events)
	}
}

// --- unknown-field → 400 (strictUnmarshal) ---

func TestHeraldCreate_UnknownField400(t *testing.T) {
	h, _ := newHeraldToolHandler(t, heraldAdminCfg(), &heraldFakePool{}, &fakeInvalidator{})
	resp := callTool(t, h, "archon-alice", "keeper.herald.create",
		`{"name":"x","type":"webhook","config":{"url":"https://x/y"},"bogus":true}`)
	if resp.Error == nil {
		t.Fatal("expected malformed error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeMalformedRequest {
		t.Errorf("code = %q, want malformed-request", data.Code)
	}
}
