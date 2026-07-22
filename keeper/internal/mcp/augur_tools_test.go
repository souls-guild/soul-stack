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

	"github.com/souls-guild/soul-stack/keeper/internal/augur"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// augurFakePool — narrow fake for [augur.ServicePool] used by augur-tools
// tests. Covers TRANSPORT (RBAC check / sentinel→MCP-code mapping / output /
// audit); augur.Service business invariants are covered by
// augur/integration_test.go.
type augurFakePool struct {
	omenInsertErr  error
	omenGetRow     []any
	omenGetErr     error
	omenListRows   [][]any
	omenCount      int
	omenDeleteRows int64

	riteInsertErr  error
	riteListRows   [][]any
	riteDeleteRows int64
}

func (p *augurFakePool) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	switch {
	case strings.Contains(sql, "DELETE FROM omens"):
		return delTag(p.omenDeleteRows), nil
	case strings.Contains(sql, "DELETE FROM rites"):
		return delTag(p.riteDeleteRows), nil
	}
	return pgconn.CommandTag{}, &augurTestErr{"Exec: " + sql}
}

func (p *augurFakePool) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "INSERT INTO omens"):
		if p.omenInsertErr != nil {
			return augurErrRow{p.omenInsertErr}
		}
		return augurTRow{[]any{time.Now()}}
	case strings.Contains(sql, "INSERT INTO rites"):
		if p.riteInsertErr != nil {
			return augurErrRow{p.riteInsertErr}
		}
		return augurTRow{[]any{int64(7), time.Now()}}
	case strings.Contains(sql, "FROM omens") && strings.Contains(sql, "WHERE name"):
		if p.omenGetErr != nil {
			return augurErrRow{p.omenGetErr}
		}
		if p.omenGetRow == nil {
			return augurErrRow{pgx.ErrNoRows}
		}
		return augurTRow{p.omenGetRow}
	case strings.Contains(sql, "SELECT COUNT(*) FROM omens"):
		return augurTRow{[]any{p.omenCount}}
	}
	return augurErrRow{&augurTestErr{"QueryRow: " + sql}}
}

func (p *augurFakePool) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	switch {
	case strings.Contains(sql, "FROM omens") && strings.Contains(sql, "ORDER BY"):
		return &augurTRows{rows: p.omenListRows}, nil
	case strings.Contains(sql, "FROM rites") && strings.Contains(sql, "WHERE omen"):
		return &augurTRows{rows: p.riteListRows}, nil
	}
	return nil, &augurTestErr{"Query: " + sql}
}

func delTag(rows int64) pgconn.CommandTag {
	if rows > 0 {
		return pgconn.NewCommandTag("DELETE 1")
	}
	return pgconn.NewCommandTag("DELETE 0")
}

type augurTestErr struct{ s string }

func (e *augurTestErr) Error() string { return e.s }

type augurErrRow struct{ err error }

func (r augurErrRow) Scan(_ ...any) error { return r.err }

type augurTRow struct{ values []any }

func (r augurTRow) Scan(dest ...any) error { return scanAugurT(dest, r.values) }

type augurTRows struct {
	rows [][]any
	idx  int
}

func (r *augurTRows) Next() bool                                   { r.idx++; return r.idx <= len(r.rows) }
func (r *augurTRows) Scan(dest ...any) error                       { return scanAugurT(dest, r.rows[r.idx-1]) }
func (r *augurTRows) Err() error                                   { return nil }
func (r *augurTRows) Close()                                       {}
func (r *augurTRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *augurTRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *augurTRows) Values() ([]any, error)                       { return nil, nil }
func (r *augurTRows) RawValues() [][]byte                          { return nil }
func (r *augurTRows) Conn() *pgx.Conn                              { return nil }

func scanAugurT(dest, values []any) error {
	for i, d := range dest {
		switch d := d.(type) {
		case *string:
			*d = values[i].(string)
		case *int:
			*d = values[i].(int)
		case *int64:
			*d = values[i].(int64)
		case *bool:
			*d = values[i].(bool)
		case *time.Time:
			*d = values[i].(time.Time)
		case *[]byte:
			*d = values[i].([]byte)
		case **string:
			if values[i] == nil {
				*d = nil
			} else {
				s := values[i].(string)
				*d = &s
			}
		case **int:
			if values[i] == nil {
				*d = nil
			} else {
				n := values[i].(int)
				*d = &n
			}
		}
	}
	return nil
}

func omenTRow(name, src, endpoint, authRef string) []any {
	return []any{name, src, endpoint, authRef, nil, time.Now()}
}

func riteTRow(id int64, omen string, coven *string, allow []byte) []any {
	var cov any
	if coven != nil {
		cov = *coven
	}
	return []any{id, omen, cov, nil, allow, false, nil, nil, nil, time.Now()}
}

// --- harness ---

func newAugurToolHandler(t *testing.T, rbacCfg *rbactest.Config, pool *augurFakePool) (*Handler, *recordingAudit) {
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
		svc, err := augur.NewService(augur.ServiceDeps{Pool: pool, Logger: logger})
		if err != nil {
			t.Fatalf("augur.NewService: %v", err)
		}
		deps.AugurSvc = svc
	}

	h, err := NewHandler(deps)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h, rec
}

// augurAdminCfg — RBAC granting archon-alice all omen.*/rite.*-permissions.
func augurAdminCfg() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "augur-admin", Operators: []string{"archon-alice"}, Permissions: []string{
				"omen.create", "omen.list", "omen.delete",
				"rite.create", "rite.list", "rite.delete",
			}},
		},
	}
}

// --- tests: manifest ---

func TestAugurTools_InManifest(t *testing.T) {
	want := []string{
		"keeper.augur.omen.create", "keeper.augur.omen.list", "keeper.augur.omen.delete",
		"keeper.augur.rite.create", "keeper.augur.rite.list", "keeper.augur.rite.delete",
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

func TestAugurTools_NilGuard(t *testing.T) {
	h, _ := newAugurToolHandler(t, augurAdminCfg(), nil) // AugurSvc == nil
	cases := []struct {
		tool string
		args string
	}{
		{"keeper.augur.omen.create", `{"name":"x","source_type":"vault","endpoint":"e","auth_ref":"vault:s/p"}`},
		{"keeper.augur.omen.list", `{}`},
		{"keeper.augur.omen.delete", `{"name":"x"}`},
		{"keeper.augur.rite.create", `{"omen":"x","allow":{"paths":["x"]}}`},
		{"keeper.augur.rite.list", `{"omen":"x"}`},
		{"keeper.augur.rite.delete", `{"id":1}`},
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

func TestAugurTools_RBACForbidden(t *testing.T) {
	// archon-alice without omen/rite-permissions (empty RBAC → deny all).
	h, _ := newAugurToolHandler(t, nil, &augurFakePool{})
	cases := []struct {
		tool string
		args string
	}{
		{"keeper.augur.omen.create", `{"name":"x","source_type":"vault","endpoint":"e","auth_ref":"vault:s/p"}`},
		{"keeper.augur.rite.create", `{"omen":"x","allow":{"paths":["x"]}}`},
		{"keeper.augur.rite.delete", `{"id":1}`},
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

func TestAugurTools_Validation(t *testing.T) {
	h, _ := newAugurToolHandler(t, augurAdminCfg(), &augurFakePool{
		omenGetRow: omenTRow("vault-prod", "vault", "e", "vault:s/p"),
	})
	cases := []struct {
		name string
		tool string
		args string
		want string
	}{
		{"omen-no-name", "keeper.augur.omen.create", `{"source_type":"vault","endpoint":"e","auth_ref":"vault:s/p"}`, mcpCodeValidationFailed},
		{"omen-bad-source", "keeper.augur.omen.create", `{"name":"x","source_type":"redis","endpoint":"e","auth_ref":"vault:s/p"}`, mcpCodeValidationFailed},
		{"omen-bad-authref", "keeper.augur.omen.create", `{"name":"x","source_type":"vault","endpoint":"e","auth_ref":"plain"}`, mcpCodeValidationFailed},
		{"omen-unknown-field", "keeper.augur.omen.create", `{"name":"x","source_type":"vault","endpoint":"e","auth_ref":"vault:s/p","z":1}`, mcpCodeMalformedRequest},
		{"rite-no-omen", "keeper.augur.rite.create", `{"allow":{"paths":["x"]}}`, mcpCodeValidationFailed},
		{"rite-bad-allow-shape", "keeper.augur.rite.create", `{"omen":"vault-prod","coven":"web","allow":{"queries":["up"]}}`, mcpCodeValidationFailed},
		{"rite-list-no-omen", "keeper.augur.rite.list", `{}`, mcpCodeValidationFailed},
		{"rite-delete-bad-id", "keeper.augur.rite.delete", `{"id":0}`, mcpCodeValidationFailed},
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

// --- tests: omen create success + audit ---

func TestAugurOmenCreate_Success(t *testing.T) {
	h, rec := newAugurToolHandler(t, augurAdminCfg(), &augurFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.augur.omen.create",
		`{"name":"vault-prod","source_type":"vault","endpoint":"https://vault:8200","auth_ref":"vault:secret/keeper/ar"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}

	var res toolsCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var out omenView
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
	if out.Name != "vault-prod" || out.SourceType != "vault" {
		t.Errorf("output = %+v", out)
	}

	if len(rec.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(rec.events))
	}
	ev := rec.events[0]
	if ev.EventType != audit.EventOmenCreated {
		t.Errorf("event_type = %q, want omen.created", ev.EventType)
	}
	if ev.Source != audit.SourceMCP {
		t.Errorf("source = %q, want mcp", ev.Source)
	}
	assertPayload(t, ev.Payload, "name", "vault-prod")
	assertPayload(t, ev.Payload, "source_type", "vault")
	assertPayload(t, ev.Payload, "auth_ref", "vault:secret/keeper/ar")
	assertPayload(t, ev.Payload, "created_by_aid", "archon-alice")
}

func TestAugurOmenCreate_Duplicate409(t *testing.T) {
	h, _ := newAugurToolHandler(t, augurAdminCfg(), &augurFakePool{
		omenInsertErr: &pgconn.PgError{Code: "23505", ConstraintName: "omens_pkey"},
	})
	resp := callTool(t, h, "archon-alice", "keeper.augur.omen.create",
		`{"name":"vault-prod","source_type":"vault","endpoint":"e","auth_ref":"vault:s/p"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeOmenExists {
		t.Errorf("code = %q, want omen-already-exists", data.Code)
	}
}

// --- tests: omen list / delete ---

func TestAugurOmenList_Success(t *testing.T) {
	h, rec := newAugurToolHandler(t, augurAdminCfg(), &augurFakePool{
		omenCount:    2,
		omenListRows: [][]any{omenTRow("vault-prod", "vault", "e1", "vault:s/p1"), omenTRow("prom", "prometheus", "e2", "vault:s/p2")},
	})
	resp := callTool(t, h, "archon-alice", "keeper.augur.omen.list", `{}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var res toolsCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var out omenListOutput
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
	if len(out.Omens) != 2 || out.Total != 2 {
		t.Fatalf("out = %+v", out)
	}
	if len(rec.events) != 0 {
		t.Errorf("list emitted %d audit events, want 0", len(rec.events))
	}
}

func TestAugurOmenDelete_Success(t *testing.T) {
	h, rec := newAugurToolHandler(t, augurAdminCfg(), &augurFakePool{omenDeleteRows: 1})
	resp := callTool(t, h, "archon-alice", "keeper.augur.omen.delete", `{"name":"vault-prod"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if len(rec.events) != 1 || rec.events[0].EventType != audit.EventOmenRevoked {
		t.Fatalf("expected omen.revoked audit, got %+v", rec.events)
	}
	assertPayload(t, rec.events[0].Payload, "name", "vault-prod")
}

func TestAugurOmenDelete_NotFound404(t *testing.T) {
	h, _ := newAugurToolHandler(t, augurAdminCfg(), &augurFakePool{omenDeleteRows: 0})
	resp := callTool(t, h, "archon-alice", "keeper.augur.omen.delete", `{"name":"ghost"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeNotFound {
		t.Errorf("code = %q, want not-found", data.Code)
	}
}

// --- tests: rite create / list / delete ---

func TestAugurRiteCreate_Success(t *testing.T) {
	h, rec := newAugurToolHandler(t, augurAdminCfg(), &augurFakePool{
		omenGetRow: omenTRow("vault-prod", "vault", "e", "vault:s/p"),
	})
	resp := callTool(t, h, "archon-alice", "keeper.augur.rite.create",
		`{"omen":"vault-prod","coven":"web","allow":{"paths":["secret/app"]}}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var res toolsCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var out riteView
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
	if out.ID != 7 || out.Omen != "vault-prod" {
		t.Errorf("output = %+v", out)
	}

	if len(rec.events) != 1 || rec.events[0].EventType != audit.EventRiteCreated {
		t.Fatalf("expected rite.created audit, got %+v", rec.events)
	}
	assertPayload(t, rec.events[0].Payload, "omen", "vault-prod")
	assertPayload(t, rec.events[0].Payload, "subject", "coven=web")
	// allow-list is NOT in the payload (augur.md §8).
	if _, ok := rec.events[0].Payload["allow"]; ok {
		t.Errorf("allow-list leaked into the audit payload: %v", rec.events[0].Payload)
	}
}

func TestAugurRiteCreate_OmenNotFound404(t *testing.T) {
	h, _ := newAugurToolHandler(t, augurAdminCfg(), &augurFakePool{omenGetRow: nil})
	resp := callTool(t, h, "archon-alice", "keeper.augur.rite.create",
		`{"omen":"ghost","coven":"web","allow":{"paths":["x"]}}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeNotFound {
		t.Errorf("code = %q, want not-found", data.Code)
	}
}

func TestAugurRiteList_Success(t *testing.T) {
	cov := "web"
	h, rec := newAugurToolHandler(t, augurAdminCfg(), &augurFakePool{
		riteListRows: [][]any{riteTRow(1, "vault-prod", &cov, []byte(`{"paths":["x"]}`))},
	})
	resp := callTool(t, h, "archon-alice", "keeper.augur.rite.list", `{"omen":"vault-prod"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var res toolsCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var out riteListOutput
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
	if len(out.Rites) != 1 || out.Rites[0].Omen != "vault-prod" {
		t.Fatalf("out = %+v", out)
	}
	if len(rec.events) != 0 {
		t.Errorf("list emitted %d audit events, want 0", len(rec.events))
	}
}

func TestAugurRiteDelete_Success(t *testing.T) {
	h, rec := newAugurToolHandler(t, augurAdminCfg(), &augurFakePool{riteDeleteRows: 1})
	resp := callTool(t, h, "archon-alice", "keeper.augur.rite.delete", `{"id":7}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if len(rec.events) != 1 || rec.events[0].EventType != audit.EventRiteRevoked {
		t.Fatalf("expected rite.revoked audit, got %+v", rec.events)
	}
}

func TestAugurRiteDelete_NotFound404(t *testing.T) {
	h, _ := newAugurToolHandler(t, augurAdminCfg(), &augurFakePool{riteDeleteRows: 0})
	resp := callTool(t, h, "archon-alice", "keeper.augur.rite.delete", `{"id":99}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeNotFound {
		t.Errorf("code = %q, want not-found", data.Code)
	}
}
