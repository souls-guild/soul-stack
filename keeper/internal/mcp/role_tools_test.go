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

	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// --- rbac fake pool ---
//
// A narrow fake for [rbac.ServicePool]: runs exactly the SQL queries needed by
// the role tools (ListRoles read path + mutating tx). Business invariants of
// rbac.Service (builtin/self-lockout under FOR UPDATE) are covered by the rbac
// package's integration tests; here we test the TRANSPORT — that the tool
// calls Service correctly, maps errors, and encodes the output.

type roleFakePool struct {
	// views — backing for ListRoles (selectRoleViewsSQL + permissions + operators).
	views []rbac.RoleView

	// lockBuiltin — what SELECT builtin … FOR UPDATE returns (delete/update/grant
	// role lock). lockMissing=true → role not found (ErrRoleNotFound).
	lockBuiltin bool
	lockMissing bool

	// execErr — the error returned by the first mutating Exec (INSERT/DELETE).
	// Lets tests inject pg errors (23505/23503) to verify the sentinel→MCP mapping.
	execErr error

	// callerPerms — the caller's effective permissions (subset check
	// SELECT rp.permission … JOIN). nil → defaults to `["*"]` (caller=cluster-admin),
	// so transport tests pass the least-privilege gate. callerPermsExplicit
	// distinguishes "not set" (default `*`) from "set empty" (caller with no rights).
	callerPerms         []string
	callerPermsExplicit bool
}

func (p *roleFakePool) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if p.execErr != nil &&
		(strings.HasPrefix(strings.TrimSpace(sql), "INSERT") ||
			strings.HasPrefix(strings.TrimSpace(sql), "DELETE")) {
		return pgconn.CommandTag{}, p.execErr
	}
	// Successful no-op: one affected row (DELETE role/membership → not 0).
	return pgconn.NewCommandTag("OK 1"), nil
}

func (p *roleFakePool) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	switch {
	case strings.Contains(sql, "SELECT rp.permission"):
		// caller-perms (subset check): defaults to `["*"]` (caller=cluster-admin)
		// if the test hasn't set an explicit set. The `rp.permission` marker is
		// unique — checked BEFORE the rbac_role_operators cases (the caller-perms
		// query also joins rbac_role_operators).
		if p.callerPermsExplicit {
			return &roleRows{single: p.callerPerms}, nil
		}
		return &roleRows{single: []string{"*"}}, nil
	case strings.Contains(sql, "FROM rbac_roles WHERE name") && strings.Contains(sql, "FOR UPDATE"):
		// lockRole: SELECT builtin … FOR UPDATE.
		if p.lockMissing {
			return &roleRows{}, nil // empty → ErrRoleNotFound
		}
		return &roleRows{builtin: []bool{p.lockBuiltin}}, nil
	case strings.Contains(sql, "FROM rbac_role_operators") && strings.Contains(sql, "FOR UPDATE"):
		// lockRoleOperator / self-lockout probe: return a row (membership exists).
		return &roleRows{single: []string{"archon-keeper"}}, nil
	case strings.Contains(sql, "SELECT name, description, builtin, default_scope FROM rbac_roles"):
		return &roleViewRows{views: p.views}, nil
	case strings.Contains(sql, "FROM rbac_role_permissions WHERE role_name"):
		// rolePermissions(name) — without `*` (mutations don't trigger self-lockout).
		return &roleRows{}, nil
	case strings.Contains(sql, "FROM rbac_role_permissions"):
		// selectRolePermissionsSQL (list) — served from views.
		return &rolePermRows{views: p.views}, nil
	case strings.Contains(sql, "FROM rbac_role_operators"):
		// selectRoleOperatorsSQL (list) — served from views.
		return &roleOpRows{views: p.views}, nil
	}
	return &roleRows{}, nil
}

func (p *roleFakePool) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	return roleErrRow{}
}

func (p *roleFakePool) BeginTx(_ context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	return &roleFakeTx{pool: p}, nil
}

type roleFakeTx struct{ pool *roleFakePool }

func (t *roleFakeTx) Exec(ctx context.Context, sql string, a ...any) (pgconn.CommandTag, error) {
	return t.pool.Exec(ctx, sql, a...)
}
func (t *roleFakeTx) Query(ctx context.Context, sql string, a ...any) (pgx.Rows, error) {
	return t.pool.Query(ctx, sql, a...)
}
func (t *roleFakeTx) QueryRow(ctx context.Context, sql string, a ...any) pgx.Row {
	return t.pool.QueryRow(ctx, sql, a...)
}
func (t *roleFakeTx) Begin(ctx context.Context) (pgx.Tx, error) { return t, nil }
func (t *roleFakeTx) Commit(_ context.Context) error            { return nil }
func (t *roleFakeTx) Rollback(_ context.Context) error          { return nil }
func (t *roleFakeTx) BeginFunc(_ context.Context, fn func(pgx.Tx) error) error {
	return fn(t)
}
func (t *roleFakeTx) CopyFrom(_ context.Context, _ pgx.Identifier, _ []string, _ pgx.CopyFromSource) (int64, error) {
	panic("roleFakeTx.CopyFrom: unexpected")
}
func (t *roleFakeTx) SendBatch(_ context.Context, _ *pgx.Batch) pgx.BatchResults {
	panic("roleFakeTx.SendBatch: unexpected")
}
func (t *roleFakeTx) LargeObjects() pgx.LargeObjects { panic("roleFakeTx.LargeObjects: unexpected") }
func (t *roleFakeTx) Prepare(_ context.Context, _, _ string) (*pgconn.StatementDescription, error) {
	panic("roleFakeTx.Prepare: unexpected")
}
func (t *roleFakeTx) Conn() *pgx.Conn { return nil }

// --- rbac fake rows ---

type roleErrRow struct{}

func (roleErrRow) Scan(_ ...any) error { return pgx.ErrNoRows }

// roleRows — bool/string single-row result (lockRole builtin / membership).
type roleRows struct {
	builtin []bool
	single  []string
	idx     int
}

func (r *roleRows) Next() bool {
	n := len(r.builtin)
	if len(r.single) > n {
		n = len(r.single)
	}
	if r.idx >= n {
		return false
	}
	r.idx++
	return true
}
func (r *roleRows) Scan(dest ...any) error {
	if len(r.builtin) > 0 {
		*dest[0].(*bool) = r.builtin[r.idx-1]
		return nil
	}
	if len(r.single) > 0 {
		*dest[0].(*string) = r.single[r.idx-1]
	}
	return nil
}
func (r *roleRows) Err() error                                   { return nil }
func (r *roleRows) Close()                                       {}
func (r *roleRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *roleRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *roleRows) Values() ([]any, error)                       { return nil, nil }
func (r *roleRows) RawValues() [][]byte                          { return nil }
func (r *roleRows) Conn() *pgx.Conn                              { return nil }

// roleViewRows — SELECT name, description, builtin, default_scope (list).
type roleViewRows struct {
	views []rbac.RoleView
	idx   int
}

func (r *roleViewRows) Next() bool {
	if r.idx >= len(r.views) {
		return false
	}
	r.idx++
	return true
}
func (r *roleViewRows) Scan(dest ...any) error {
	v := r.views[r.idx-1]
	*dest[0].(*string) = v.Name
	*dest[1].(*string) = v.Description
	*dest[2].(*bool) = v.Builtin
	// default_scope nullable (ADR-047 S1): empty string → NULL (*string=nil).
	scopeDest := dest[3].(**string)
	if v.DefaultScope != "" {
		s := v.DefaultScope
		*scopeDest = &s
	} else {
		*scopeDest = nil
	}
	return nil
}
func (r *roleViewRows) Err() error                                   { return nil }
func (r *roleViewRows) Close()                                       {}
func (r *roleViewRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *roleViewRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *roleViewRows) Values() ([]any, error)                       { return nil, nil }
func (r *roleViewRows) RawValues() [][]byte                          { return nil }
func (r *roleViewRows) Conn() *pgx.Conn                              { return nil }

// rolePermRows / roleOpRows — (role_name, permission|aid) for assembling the list.
type rolePermRows struct {
	views []rbac.RoleView
	pairs [][2]string
	idx   int
	init  bool
}

func (r *rolePermRows) ensure() {
	if r.init {
		return
	}
	for _, v := range r.views {
		for _, p := range v.Permissions {
			r.pairs = append(r.pairs, [2]string{v.Name, p})
		}
	}
	r.init = true
}
func (r *rolePermRows) Next() bool {
	r.ensure()
	if r.idx >= len(r.pairs) {
		return false
	}
	r.idx++
	return true
}
func (r *rolePermRows) Scan(dest ...any) error {
	*dest[0].(*string) = r.pairs[r.idx-1][0]
	*dest[1].(*string) = r.pairs[r.idx-1][1]
	return nil
}
func (r *rolePermRows) Err() error                                   { return nil }
func (r *rolePermRows) Close()                                       {}
func (r *rolePermRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *rolePermRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *rolePermRows) Values() ([]any, error)                       { return nil, nil }
func (r *rolePermRows) RawValues() [][]byte                          { return nil }
func (r *rolePermRows) Conn() *pgx.Conn                              { return nil }

type roleOpRows struct {
	views []rbac.RoleView
	pairs [][2]string
	idx   int
	init  bool
}

func (r *roleOpRows) ensure() {
	if r.init {
		return
	}
	for _, v := range r.views {
		for _, op := range v.Operators {
			r.pairs = append(r.pairs, [2]string{v.Name, op})
		}
	}
	r.init = true
}
func (r *roleOpRows) Next() bool {
	r.ensure()
	if r.idx >= len(r.pairs) {
		return false
	}
	r.idx++
	return true
}
func (r *roleOpRows) Scan(dest ...any) error {
	*dest[0].(*string) = r.pairs[r.idx-1][0]
	*dest[1].(*string) = r.pairs[r.idx-1][1]
	return nil
}
func (r *roleOpRows) Err() error                                   { return nil }
func (r *roleOpRows) Close()                                       {}
func (r *roleOpRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *roleOpRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *roleOpRows) Values() ([]any, error)                       { return nil, nil }
func (r *roleOpRows) RawValues() [][]byte                          { return nil }
func (r *roleOpRows) Conn() *pgx.Conn                              { return nil }

// --- harness ---

// newRoleHandler assembles a Handler with a real rbac.Service over
// roleFakePool. rolePool=nil → RBACRoles stays nil (for the nil-guard check).
func newRoleHandler(t *testing.T, rbacCfg *rbactest.Config, rolePool *roleFakePool) *Handler {
	t.Helper()
	h, _ := newRoleHandlerRec(t, rbacCfg, rolePool)
	return h
}

// newRoleHandlerRec — like newRoleHandler, but also returns recordingAudit so
// success tests can verify the audit-event emission (ADR-022).
func newRoleHandlerRec(t *testing.T, rbacCfg *rbactest.Config, rolePool *roleFakePool) (*Handler, *recordingAudit) {
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
	if rolePool != nil {
		svc, err := rbac.NewService(rbac.ServiceDeps{Pool: rolePool, Logger: logger})
		if err != nil {
			t.Fatalf("rbac.NewService: %v", err)
		}
		deps.RBACRoles = svc
	}

	h, err := NewHandler(deps)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h, rec
}

// roleAdminCfg — an RBAC config granting archon-alice all role.* permissions.
func roleAdminCfg() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "role-admin", Operators: []string{"archon-alice"}, Permissions: []string{
				"role.create", "role.delete", "role.list", "role.update",
				"role.grant-operator", "role.revoke-operator",
			}},
		},
	}
}

// --- tests: manifest / catalog ---

func TestRoleTools_InManifest(t *testing.T) {
	want := map[string]struct{}{
		"keeper.role.create":          {},
		"keeper.role.delete":          {},
		"keeper.role.list":            {},
		"keeper.role.update":          {},
		"keeper.role.grant-operator":  {},
		"keeper.role.revoke-operator": {},
	}
	for name := range want {
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

// TestCatalog_TotalCount — the catalog must contain exactly 90 tools (72 + 5
// keeper.herald.* + 5 keeper.tiding.* per ADR-052 S4 + keeper.soul.traits-assign
// per ADR-060 + 6 Cloud-CRUD: provider/profile read/list/delete per ADR-017
// — provider.create/profile.create used to be stubs, now implemented +
// keeper.incarnation.traits-set per ADR-060 amend R1, relocated per-soul →
// per-incarnation).
func TestCatalog_TotalCount(t *testing.T) {
	if n := len(listAllTools()); n != 90 {
		t.Errorf("catalog size = %d, want 90", n)
	}
}

// --- tests: nil-guard ---

func TestRoleTools_NilGuard(t *testing.T) {
	h := newRoleHandler(t, roleAdminCfg(), nil) // RBACRoles == nil
	cases := []struct {
		tool string
		args string
	}{
		{"keeper.role.create", `{"name":"ops","permissions":["incarnation.get"]}`},
		{"keeper.role.delete", `{"name":"ops"}`},
		{"keeper.role.list", `{}`},
		{"keeper.role.update", `{"name":"ops","permissions":[]}`},
		{"keeper.role.grant-operator", `{"role":"ops","aid":"archon-bob"}`},
		{"keeper.role.revoke-operator", `{"role":"ops","aid":"archon-bob"}`},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			resp := callTool(t, h, "archon-alice", tc.tool, tc.args)
			if resp.Error == nil {
				t.Fatal("expected error response")
			}
			data := mustToolErrorData(t, resp.Error.Data)
			if data.Code != mcpCodeInternalError {
				t.Errorf("code = %q, want internal-error", data.Code)
			}
		})
	}
}

// --- tests: RBAC ---

func TestRoleTools_RBACForbidden(t *testing.T) {
	// archon-alice without role.* permissions (empty RBAC → deny all).
	h := newRoleHandler(t, nil, &roleFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.role.create",
		`{"name":"ops","permissions":["incarnation.get"]}`)
	if resp.Error == nil {
		t.Fatal("expected forbidden error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeForbidden {
		t.Errorf("code = %q, want forbidden", data.Code)
	}
}

// --- tests: validation ---

func TestRoleTools_Validation(t *testing.T) {
	h := newRoleHandler(t, roleAdminCfg(), &roleFakePool{})
	cases := []struct {
		name string
		tool string
		args string
		want string
	}{
		{"create-no-name", "keeper.role.create", `{"permissions":[]}`, mcpCodeValidationFailed},
		{"delete-no-name", "keeper.role.delete", `{}`, mcpCodeValidationFailed},
		{"update-no-name", "keeper.role.update", `{"permissions":[]}`, mcpCodeValidationFailed},
		{"grant-no-role", "keeper.role.grant-operator", `{"aid":"archon-bob"}`, mcpCodeValidationFailed},
		{"grant-no-aid", "keeper.role.grant-operator", `{"role":"ops"}`, mcpCodeValidationFailed},
		{"grant-bad-aid", "keeper.role.grant-operator", `{"role":"ops","aid":".bob"}`, mcpCodeValidationFailed},
		{"revoke-bad-aid", "keeper.role.revoke-operator", `{"role":"ops","aid":"BOB"}`, mcpCodeValidationFailed},
		{"create-unknown-field", "keeper.role.create", `{"name":"ops","x":1}`, mcpCodeMalformedRequest},
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

// --- tests: list happy-path ---

func TestRoleList_Success(t *testing.T) {
	pool := &roleFakePool{views: []rbac.RoleView{
		{Name: "cluster-admin", Description: "root", Builtin: true,
			Permissions: []string{"*"}, Operators: []string{"archon-alice"}},
		{Name: "ops", Description: "", Builtin: false,
			Permissions: []string{"incarnation.get", "incarnation.list"}, Operators: nil},
	}}
	h := newRoleHandler(t, roleAdminCfg(), pool)
	resp := callTool(t, h, "archon-alice", "keeper.role.list", `{}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var res toolsCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var out roleListOutput
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
	if len(out.Roles) != 2 {
		t.Fatalf("roles = %d, want 2", len(out.Roles))
	}
	// non-nil slice for a role without operators (JSON [] instead of null).
	for _, r := range out.Roles {
		if r.Name == "ops" {
			if r.Operators == nil {
				t.Errorf("ops.operators is nil, want []")
			}
			if len(r.Permissions) != 2 {
				t.Errorf("ops.permissions = %v", r.Permissions)
			}
		}
	}
}

// --- tests: mutating success + sentinel mapping ---

func TestRoleCreate_Success(t *testing.T) {
	h, rec := newRoleHandlerRec(t, roleAdminCfg(), &roleFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.role.create",
		`{"name":"ops","description":"team","permissions":["incarnation.get"]}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	ev := requireSingleAudit(t, rec, "role.created")
	if ev.Payload["name"] != "ops" {
		t.Errorf("payload.name = %v, want ops", ev.Payload["name"])
	}
	if ev.Payload["created_by_aid"] != "archon-alice" {
		t.Errorf("payload.created_by_aid = %v, want archon-alice", ev.Payload["created_by_aid"])
	}
	if _, ok := ev.Payload["permissions"]; !ok {
		t.Errorf("payload missing 'permissions'")
	}
}

func TestRoleCreate_AlreadyExists(t *testing.T) {
	// INSERT rbac_roles → 23505 → ErrRoleAlreadyExists → role-already-exists.
	pool := &roleFakePool{execErr: &pgconn.PgError{Code: "23505", ConstraintName: "rbac_roles_pkey"}}
	h := newRoleHandler(t, roleAdminCfg(), pool)
	resp := callTool(t, h, "archon-alice", "keeper.role.create",
		`{"name":"ops","permissions":["incarnation.get"]}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeRoleExists {
		t.Errorf("code = %q, want role-already-exists", data.Code)
	}
}

func TestRoleCreate_BadPermission(t *testing.T) {
	// A malformed permission is caught in Service BEFORE the tx (ParsePermission)
	// → validation-failed; the raw "rbac: " prefix doesn't leak out.
	h := newRoleHandler(t, roleAdminCfg(), &roleFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.role.create",
		`{"name":"ops","permissions":["keeper.incarnation.get"]}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Errorf("code = %q, want validation-failed", data.Code)
	}
}

func TestRoleDelete_Builtin(t *testing.T) {
	pool := &roleFakePool{lockBuiltin: true}
	h := newRoleHandler(t, roleAdminCfg(), pool)
	resp := callTool(t, h, "archon-alice", "keeper.role.delete", `{"name":"cluster-admin"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeRoleBuiltin {
		t.Errorf("code = %q, want role-builtin", data.Code)
	}
}

func TestRoleDelete_NotFound(t *testing.T) {
	pool := &roleFakePool{lockMissing: true}
	h := newRoleHandler(t, roleAdminCfg(), pool)
	resp := callTool(t, h, "archon-alice", "keeper.role.delete", `{"name":"ghost"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeNotFound {
		t.Errorf("code = %q, want not-found", data.Code)
	}
}

func TestRoleDelete_Success(t *testing.T) {
	h, rec := newRoleHandlerRec(t, roleAdminCfg(), &roleFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.role.delete", `{"name":"ops"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	ev := requireSingleAudit(t, rec, "role.deleted")
	if ev.Payload["name"] != "ops" {
		t.Errorf("payload.name = %v, want ops", ev.Payload["name"])
	}
}

func TestRoleGrantOperator_OperatorNotFound(t *testing.T) {
	// lockRole ok (role exists), INSERT membership → 23503 → ErrOperatorNotFound.
	pool := &roleFakePool{execErr: &pgconn.PgError{Code: "23503", ConstraintName: "rbac_role_operators_aid_fk"}}
	h := newRoleHandler(t, roleAdminCfg(), pool)
	resp := callTool(t, h, "archon-alice", "keeper.role.grant-operator",
		`{"role":"ops","aid":"archon-ghost"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeNotFound {
		t.Errorf("code = %q, want not-found", data.Code)
	}
}

func TestRoleGrantOperator_Success(t *testing.T) {
	h, rec := newRoleHandlerRec(t, roleAdminCfg(), &roleFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.role.grant-operator",
		`{"role":"ops","aid":"archon-bob"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	ev := requireSingleAudit(t, rec, "role.operator-granted")
	if ev.Payload["name"] != "ops" || ev.Payload["aid"] != "archon-bob" {
		t.Errorf("payload = %+v, want name=ops aid=archon-bob", ev.Payload)
	}
	if ev.Payload["granted_by_aid"] != "archon-alice" {
		t.Errorf("payload.granted_by_aid = %v, want archon-alice", ev.Payload["granted_by_aid"])
	}
}

func TestRoleRevokeOperator_Success(t *testing.T) {
	h, rec := newRoleHandlerRec(t, roleAdminCfg(), &roleFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.role.revoke-operator",
		`{"role":"ops","aid":"archon-bob"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	ev := requireSingleAudit(t, rec, "role.operator-revoked")
	if ev.Payload["name"] != "ops" || ev.Payload["aid"] != "archon-bob" {
		t.Errorf("payload = %+v, want name=ops aid=archon-bob", ev.Payload)
	}
}

// TestRoleUpdate_Success — audit role.permissions-updated with the new set.
func TestRoleUpdate_Success(t *testing.T) {
	h, rec := newRoleHandlerRec(t, roleAdminCfg(), &roleFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.role.update",
		`{"name":"ops","permissions":["incarnation.get","soul.list"]}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	ev := requireSingleAudit(t, rec, "role.permissions-updated")
	if ev.Payload["name"] != "ops" {
		t.Errorf("payload.name = %v, want ops", ev.Payload["name"])
	}
	if _, ok := ev.Payload["permissions"]; !ok {
		t.Errorf("payload missing 'permissions'")
	}
}

// --- least-privilege subset-check (ErrPermissionNotHeld → forbidden) ---

// role.create with a permission outside the caller's set → forbidden. The
// caller has role.* (passes RBAC.Check on the operation itself), but `*`
// isn't in the set — creating a role with `*` isn't allowed (subset check).
func TestRoleCreate_PermissionNotHeld_Forbidden(t *testing.T) {
	pool := &roleFakePool{callerPermsExplicit: true, callerPerms: []string{"role.create"}}
	h := newRoleHandler(t, roleAdminCfg(), pool)
	resp := callTool(t, h, "archon-alice", "keeper.role.create",
		`{"name":"escalation","permissions":["*"]}`)
	if resp.Error == nil {
		t.Fatal("expected forbidden error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeForbidden {
		t.Errorf("code = %q, want forbidden", data.Code)
	}
}

// role.update: adding a permission the caller doesn't hold → forbidden. The
// role's old set is empty (rolePermissions(name) → empty in the fake), and
// operator.create outside the caller's set is being added.
func TestRoleUpdate_PermissionNotHeld_Forbidden(t *testing.T) {
	pool := &roleFakePool{callerPermsExplicit: true, callerPerms: []string{"role.update"}}
	h := newRoleHandler(t, roleAdminCfg(), pool)
	resp := callTool(t, h, "archon-alice", "keeper.role.update",
		`{"name":"ops","permissions":["operator.create"]}`)
	if resp.Error == nil {
		t.Fatal("expected forbidden error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeForbidden {
		t.Errorf("code = %q, want forbidden", data.Code)
	}
}

// requireSingleAudit verifies that exactly one audit event with the expected
// EventType and source=mcp was recorded, and returns it for inspecting the
// payload.
func requireSingleAudit(t *testing.T, rec *recordingAudit, wantType string) *audit.Event {
	t.Helper()
	if len(rec.events) != 1 {
		t.Fatalf("audit events = %d, want 1: %+v", len(rec.events), rec.events)
	}
	ev := rec.events[0]
	if string(ev.EventType) != wantType {
		t.Errorf("EventType = %q, want %q", ev.EventType, wantType)
	}
	if ev.Source != audit.SourceMCP {
		t.Errorf("Source = %q, want %q", ev.Source, audit.SourceMCP)
	}
	if ev.ArchonAID != "archon-alice" {
		t.Errorf("ArchonAID = %q, want archon-alice", ev.ArchonAID)
	}
	return ev
}

// claims-helper / callTool / mustToolErrorData / fakeIssuer / recordingAudit
// are defined in handler_test.go (same package).
var _ = keeperjwt.Claims{}
