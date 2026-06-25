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
// Узкий fake под [rbac.ServicePool]: гоняет именно те SQL-запросы, что нужны
// role-tools (ListRoles read-path + mutating-tx). Бизнес-инварианты rbac.Service
// (builtin/self-lockout под FOR UPDATE) покрыты integration-тестами пакета rbac;
// здесь проверяется ТРАНСПОРТ — что tool правильно зовёт Service, маппит ошибки
// и кодирует output.

type roleFakePool struct {
	// views — backing для ListRoles (selectRoleViewsSQL + permissions + operators).
	views []rbac.RoleView

	// lockBuiltin — что вернёт SELECT builtin … FOR UPDATE (delete/update/grant
	// lock роли). lockMissing=true → роль не найдена (ErrRoleNotFound).
	lockBuiltin bool
	lockMissing bool

	// execErr — ошибка, которую вернёт первый мутирующий Exec (INSERT/DELETE).
	// Позволяет инжектить pg-ошибки (23505/23503) для проверки sentinel→MCP.
	execErr error

	// callerPerms — эффективные permissions caller-а (subset-check
	// SELECT rp.permission … JOIN). nil → дефолт `["*"]` (caller=cluster-admin),
	// чтобы транспорт-тесты проходили least-privilege-гейт. callerPermsExplicit
	// отличает «не задано» (дефолт `*`) от «задано пустым» (caller без прав).
	callerPerms         []string
	callerPermsExplicit bool
}

func (p *roleFakePool) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if p.execErr != nil &&
		(strings.HasPrefix(strings.TrimSpace(sql), "INSERT") ||
			strings.HasPrefix(strings.TrimSpace(sql), "DELETE")) {
		return pgconn.CommandTag{}, p.execErr
	}
	// Успешный no-op: одна затронутая строка (DELETE role/membership → not 0).
	return pgconn.NewCommandTag("OK 1"), nil
}

func (p *roleFakePool) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	switch {
	case strings.Contains(sql, "SELECT rp.permission"):
		// caller-perms (subset-check): дефолт `["*"]` (caller=cluster-admin),
		// если тест не задал набор явно. Маркер `rp.permission` уникален —
		// проверяется ПЕРЕД rbac_role_operators-кейсами (caller-perms-запрос
		// тоже джойнит rbac_role_operators).
		if p.callerPermsExplicit {
			return &roleRows{single: p.callerPerms}, nil
		}
		return &roleRows{single: []string{"*"}}, nil
	case strings.Contains(sql, "FROM rbac_roles WHERE name") && strings.Contains(sql, "FOR UPDATE"):
		// lockRole: SELECT builtin … FOR UPDATE.
		if p.lockMissing {
			return &roleRows{}, nil // пустой → ErrRoleNotFound
		}
		return &roleRows{builtin: []bool{p.lockBuiltin}}, nil
	case strings.Contains(sql, "FROM rbac_role_operators") && strings.Contains(sql, "FOR UPDATE"):
		// lockRoleOperator / self-lockout probe: возвращаем строку (membership есть).
		return &roleRows{single: []string{"archon-keeper"}}, nil
	case strings.Contains(sql, "SELECT name, description, builtin, default_scope FROM rbac_roles"):
		return &roleViewRows{views: p.views}, nil
	case strings.Contains(sql, "FROM rbac_role_permissions WHERE role_name"):
		// rolePermissions(name) — без `*` (мутации не триггерят self-lockout).
		return &roleRows{}, nil
	case strings.Contains(sql, "FROM rbac_role_permissions"):
		// selectRolePermissionsSQL (list) — отдаём из views.
		return &rolePermRows{views: p.views}, nil
	case strings.Contains(sql, "FROM rbac_role_operators"):
		// selectRoleOperatorsSQL (list) — отдаём из views.
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

// roleRows — bool-/string-однострочный результат (lockRole builtin / membership).
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
	// default_scope nullable (ADR-047 S1): пустая строка → NULL (*string=nil).
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

// rolePermRows / roleOpRows — (role_name, permission|aid) для list-сборки.
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

// newRoleHandler собирает Handler с реальным rbac.Service над roleFakePool.
// rolePool=nil → RBACRoles остаётся nil (для проверки nil-guard).
func newRoleHandler(t *testing.T, rbacCfg *rbactest.Config, rolePool *roleFakePool) *Handler {
	t.Helper()
	h, _ := newRoleHandlerRec(t, rbacCfg, rolePool)
	return h
}

// newRoleHandlerRec — как newRoleHandler, но возвращает ещё и recordingAudit,
// чтобы success-тесты могли проверить эмиссию audit-event-а (ADR-022).
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

// roleAdminCfg — конфиг RBAC, дающий archon-alice все role.*-permissions.
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

// TestCatalog_TotalCount — каталог должен содержать ровно 83 tool (72 + 5
// keeper.herald.* + 5 keeper.tiding.* по ADR-052 S4 + keeper.soul.traits-assign
// по ADR-060).
func TestCatalog_TotalCount(t *testing.T) {
	if n := len(listAllTools()); n != 83 {
		t.Errorf("catalog size = %d, want 83", n)
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
	// archon-alice без role.*-permissions (пустой RBAC → deny all).
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
	// non-nil-слайс для роли без operators (JSON [] вместо null).
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
	// Битый permission ловится в Service ДО tx (ParsePermission) →
	// validation-failed, raw-префикс "rbac: " не течёт наружу.
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
	// lockRole ok (роль есть), INSERT membership → 23503 → ErrOperatorNotFound.
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

// TestRoleUpdate_Success — audit role.permissions-updated с новым набором.
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

// role.create с правом вне набора caller-а → forbidden. caller имеет role.*
// (проходит RBAC.Check на саму операцию), но в наборе нет `*` — создать роль
// с `*` нельзя (subset-check).
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

// role.update: добавление чужого права → forbidden. Старый набор роли пуст
// (rolePermissions(name) → пусто в fake), добавляется operator.create вне
// набора caller-а.
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

// requireSingleAudit проверяет, что записано ровно одно audit-событие с
// нужным EventType и source=mcp, и возвращает его для inspect-а payload-а.
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
// определены в handler_test.go (один пакет).
var _ = keeperjwt.Claims{}
