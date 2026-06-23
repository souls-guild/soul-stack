package handlers

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
)

// T5d handler-native PILOT: operator (w,r)-оболочки сняты — HTTP обслуживает huma
// full-typed (huma_operator_test.go: golden-wire / unknown-field-400 / bind-enum-422 /
// bind-bool-400 / RBAC-403 / S6-audit на реальной huma-навеске). Эти unit-тесты
// проверяют то, что huma-integration НЕ покрывает: ДОМЕННУЮ классификацию ошибок
// *Typed-функций (sentinel→problem.Type) и atomic create+grant. Зовут *Typed
// напрямую, без httptest(w,r) — bind/decode-фазу (JSON-decode / enum-validate /
// bool-parse) держит huma на границе, не handler.

// claims конструирует keeperjwt.Claims для вызова *Typed напрямую.
func claims(subject string) *keeperjwt.Claims { return &keeperjwt.Claims{Subject: subject} }

// wantProblem проверяет, что err — доменный *problemError с ожидаемым problem.Type.
func wantProblem(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("ожидалась ошибка %q, получено nil", want)
	}
	d, ok := AsProblemDetails(err)
	if !ok {
		t.Fatalf("ошибка не *problemError: %v", err)
	}
	if d.Type != want {
		t.Errorf("problem.Type = %q, want %q", d.Type, want)
	}
}

// fakeIssuer — minimal JWTIssuer-mock: возвращает фиксированный токен.
type fakeIssuer struct {
	called bool
	err    error
}

func (f *fakeIssuer) Issue(aid string, roles []string, ttl time.Duration, _ bool) (string, error) {
	f.called = true
	if f.err != nil {
		return "", f.err
	}
	return "fake-jwt-" + aid, nil
}

// fakePool — узкий мок [handlers.OperatorPool]. Реализует Exec/QueryRow/Query
// для CRUD-операторов + BeginTx, возвращающую fakeTx-обёртку. Tx-методы
// (Commit/Rollback) — no-op; tx-обёртка просто проксирует Exec/QueryRow/Query
// на родительский fakePool. Race-условия мы тут не тестируем (это integration-
// тест в /internal/api/integration_test.go); revoke-handler в unit-тесте
// просто проверяет, что lockout-логика собралась и Revoke вызывается.
type fakePool struct {
	insertErr   error
	insertCalls int
	insertOp    *operator.Operator

	selectFn func(aid string) (*operator.Operator, error)
	revokeFn func(aid, reason string) error
	activeFn func() ([]string, error)
	listFn   func() ([]*operator.Operator, int, error)

	// roleGrants — лог membership-INSERT-ов на atomic create+grant пути
	// (POST /v1/operators с roles[]).
	roleGrants []string
	// grantErrFor — мапа role → ошибка INSERT-а membership-а (FK-violation
	// эмуляция). Тестам нужно проверить, что 422 на несуществующую роль идёт
	// корректно и tx откатывается.
	grantErrFor map[string]error

	beginErr error
}

func (f *fakePool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "INSERT INTO operators") {
		f.insertCalls++
		// args order = aid, display_name, auth_method, created_at, created_by_aid, revoked_at, metadata
		op := &operator.Operator{
			AID:         args[0].(string),
			DisplayName: args[1].(string),
		}
		if args[4] != nil {
			s := args[4].(string)
			op.CreatedByAID = &s
		}
		f.insertOp = op
		return pgconn.NewCommandTag("INSERT 0 1"), f.insertErr
	}
	if strings.Contains(sql, "INSERT INTO rbac_role_operators") {
		// args = role_name, aid, granted_by_aid
		role, _ := args[0].(string)
		if err, ok := f.grantErrFor[role]; ok {
			return pgconn.CommandTag{}, err
		}
		f.roleGrants = append(f.roleGrants, role)
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	}
	if strings.Contains(sql, "UPDATE operators") {
		if f.revokeFn != nil {
			// SQL с reason → args=[aid, reason]; без reason → args=[aid].
			aid := args[0].(string)
			reason := ""
			if len(args) > 1 {
				reason = args[1].(string)
			}
			err := f.revokeFn(aid, reason)
			if err != nil {
				return pgconn.NewCommandTag("UPDATE 0"), err
			}
			return pgconn.NewCommandTag("UPDATE 1"), nil
		}
		return pgconn.NewCommandTag("UPDATE 1"), nil
	}
	return pgconn.CommandTag{}, errInvalidSQL(sql)
}

func (f *fakePool) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "COUNT(*) FROM operators") {
		// List-COUNT — ставится перед SELECT operators в List(); тестам важно
		// только, что число согласовано с listFn (или 0 без listFn).
		if f.listFn != nil {
			_, total, err := f.listFn()
			if err != nil {
				return errRow{err: err}
			}
			return staticRow{values: []any{total}}
		}
		return staticRow{values: []any{0}}
	}
	if strings.Contains(sql, "SELECT aid, display_name") {
		// SelectByAID
		if f.selectFn != nil {
			op, err := f.selectFn(args[0].(string))
			if err != nil {
				return errRow{err: err}
			}
			var (
				createdByPtr any = nil
				revokedPtr   any = nil
			)
			if op.CreatedByAID != nil {
				createdByPtr = *op.CreatedByAID
			}
			if op.RevokedAt != nil {
				revokedPtr = *op.RevokedAt
			}
			createdVia := op.CreatedVia
			if createdVia == "" {
				createdVia = operator.CreatedViaUser
			}
			return staticRow{values: []any{
				op.AID, op.DisplayName, string(op.AuthMethod), op.CreatedAt,
				createdByPtr, createdVia, revokedPtr, []byte("{}"),
			}}
		}
		return errRow{err: pgx.ErrNoRows}
	}
	return errRow{err: errInvalidSQL(sql)}
}

func (f *fakePool) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	// Synod-ветка self-lockout-ядра (ADR-049(f), эпик Synod S2):
	// LockEffectiveClusterAdmins шлёт второй locking-запрос по synod_operators.
	// operator-handler-сценарии групповых админов не моделируют — пусто; их
	// покрывают rbac integration-guard-тесты. Проверяется ПЕРЕД прямой веткой
	// (маркер synod_operators однозначен).
	if strings.Contains(sql, "FROM synod_operators") {
		return &stringRows{}, nil
	}
	// Slice 3: lockout-probe идёт через rbac.LockEffectiveClusterAdmins —
	// SELECT ro.aid FROM rbac_role_operators JOIN … FOR UPDATE OF ro,rp,o.
	// activeFn возвращает уже-эффективный набор активных `*`-admin-ов из БД
	// (раньше admin-set брался из in-memory снимка и пересекался с active-AID-ами;
	// теперь весь admin-set приходит из БД-источника — пересечение лишнее).
	if strings.Contains(sql, "FROM rbac_role_operators") {
		var admins []string
		if f.activeFn != nil {
			a, err := f.activeFn()
			if err != nil {
				return nil, err
			}
			admins = a
		}
		return &stringRows{values: admins}, nil
	}
	if strings.Contains(sql, "FROM operators") {
		// List(operators) → ORDER BY created_at DESC. Тесты не валидируют WHERE-
		// предикат точно — это уровень operator.crud_test; здесь убеждаемся,
		// что handler корректно прокидывает rows из service-слоя.
		if f.listFn != nil {
			rows, _, err := f.listFn()
			if err != nil {
				return nil, err
			}
			return newOperatorRows(rows), nil
		}
		return newOperatorRows(nil), nil
	}
	return nil, errInvalidSQL(sql)
}

func errInvalidSQL(sql string) error { return &sqlErr{sql} }

type sqlErr struct{ sql string }

func (e *sqlErr) Error() string { return "fakePool: unexpected SQL: " + e.sql }

// operatorRows — pgx.Rows-stub для list-операторов. Возвращает строки в
// порядке слайса (сортировку DB не симулируем — тест проверяет форму,
// не SQL).
type operatorRows struct {
	rows []*operator.Operator
	idx  int
}

func newOperatorRows(rs []*operator.Operator) *operatorRows {
	return &operatorRows{rows: rs}
}

func (r *operatorRows) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}

func (r *operatorRows) Scan(dest ...any) error {
	op := r.rows[r.idx-1]
	createdVia := op.CreatedVia
	if createdVia == "" {
		createdVia = operator.CreatedViaUser
	}
	*dest[0].(*string) = op.AID
	*dest[1].(*string) = op.DisplayName
	*dest[2].(*string) = string(op.AuthMethod)
	*dest[3].(*time.Time) = op.CreatedAt
	*dest[4].(**string) = op.CreatedByAID
	*dest[5].(*string) = createdVia
	*dest[6].(**time.Time) = op.RevokedAt
	*dest[7].(*[]byte) = []byte("{}")
	return nil
}

func (r *operatorRows) Err() error                                   { return nil }
func (r *operatorRows) Close()                                       {}
func (r *operatorRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *operatorRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *operatorRows) Values() ([]any, error)                       { return nil, nil }
func (r *operatorRows) RawValues() [][]byte                          { return nil }
func (r *operatorRows) Conn() *pgx.Conn                              { return nil }

// BeginTx возвращает fakeTx, делегирующую обратно в fakePool. Этого
// достаточно для unit-тестов revoke-handler-а: важно, что транзакционный
// path собран; consistency-тестируют integration-тесты (testcontainers PG).
func (f *fakePool) BeginTx(_ context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	if f.beginErr != nil {
		return nil, f.beginErr
	}
	return &fakeTx{pool: f}, nil
}

// fakeTx — обёртка над fakePool для unit-тестов: реализует pgx.Tx через
// pgx.Tx-stub из pgx (можно встроить заглушку всех методов через
// composition с no-op-структурой). Для нашего scope-а нужны только
// Exec/Query/QueryRow/Commit/Rollback; остальное — panic при обращении,
// поскольку не должно вызываться.
type fakeTx struct {
	pool *fakePool
}

func (t *fakeTx) Begin(ctx context.Context) (pgx.Tx, error) {
	return t.pool.BeginTx(ctx, pgx.TxOptions{})
}
func (t *fakeTx) BeginFunc(ctx context.Context, fn func(pgx.Tx) error) error { return fn(t) }
func (t *fakeTx) Commit(_ context.Context) error                             { return nil }
func (t *fakeTx) Rollback(_ context.Context) error                           { return nil }
func (t *fakeTx) CopyFrom(_ context.Context, _ pgx.Identifier, _ []string, _ pgx.CopyFromSource) (int64, error) {
	panic("fakeTx.CopyFrom: unexpected")
}
func (t *fakeTx) SendBatch(_ context.Context, _ *pgx.Batch) pgx.BatchResults {
	panic("fakeTx.SendBatch: unexpected")
}
func (t *fakeTx) LargeObjects() pgx.LargeObjects { panic("fakeTx.LargeObjects: unexpected") }
func (t *fakeTx) Prepare(_ context.Context, _, _ string) (*pgconn.StatementDescription, error) {
	panic("fakeTx.Prepare: unexpected")
}
func (t *fakeTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.pool.Exec(ctx, sql, args...)
}
func (t *fakeTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.pool.Query(ctx, sql, args...)
}
func (t *fakeTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.pool.QueryRow(ctx, sql, args...)
}
func (t *fakeTx) Conn() *pgx.Conn { return nil }

type errRow struct{ err error }

func (r errRow) Scan(_ ...any) error { return r.err }

type staticRow struct {
	values []any
	err    error
}

func (r staticRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, d := range dest {
		switch d := d.(type) {
		case *string:
			*d = r.values[i].(string)
		case *time.Time:
			*d = r.values[i].(time.Time)
		case *int:
			*d = r.values[i].(int)
		case *int64:
			*d = r.values[i].(int64)
		case **string:
			if r.values[i] == nil {
				*d = nil
			} else {
				s := r.values[i].(string)
				*d = &s
			}
		case **time.Time:
			if r.values[i] == nil {
				*d = nil
			} else {
				t := r.values[i].(time.Time)
				*d = &t
			}
		case *[]byte:
			*d = r.values[i].([]byte)
		case *[]string:
			if r.values[i] == nil {
				*d = nil
			} else {
				*d = r.values[i].([]string)
			}
		}
	}
	return nil
}

type stringRows struct {
	values []string
	idx    int
}

func (r *stringRows) Next() bool {
	if r.idx >= len(r.values) {
		return false
	}
	r.idx++
	return true
}

func (r *stringRows) Scan(dest ...any) error {
	*dest[0].(*string) = r.values[r.idx-1]
	return nil
}

func (r *stringRows) Err() error                                   { return nil }
func (r *stringRows) Close()                                       {}
func (r *stringRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *stringRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *stringRows) Values() ([]any, error)                       { return nil, nil }
func (r *stringRows) RawValues() [][]byte                          { return nil }
func (r *stringRows) Conn() *pgx.Conn                              { return nil }

func newHandler(t *testing.T, pool *fakePool, rbacCfg *rbactest.Config) (*OperatorHandler, *fakeIssuer) {
	t.Helper()
	enf, err := rbactest.NewEnforcer(rbacCfg)
	if err != nil {
		t.Fatalf("NewEnforcer: %v", err)
	}
	iss := &fakeIssuer{}
	h := NewOperatorHandler(pool, iss, enf, time.Hour, nil)
	return h, iss
}

func TestOperatorCreateTyped_201(t *testing.T) {
	pool := &fakePool{
		selectFn: func(aid string) (*operator.Operator, error) {
			return &operator.Operator{
				AID:         aid,
				DisplayName: "Bob",
				AuthMethod:  operator.AuthMethodJWT,
				CreatedAt:   time.Now(),
			}, nil
		},
	}
	h, iss := newHandler(t, pool, &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "creator", Operators: []string{"archon-bob"}, Permissions: []string{"operator.create"}},
		},
	})
	reply, err := h.CreateTyped(context.Background(), claims("archon-alice"),
		OperatorCreateInput{AID: "archon-bob", DisplayName: "Bob"})
	if err != nil {
		t.Fatalf("CreateTyped: %v", err)
	}
	if reply.AID != "archon-bob" {
		t.Errorf("AID = %q", reply.AID)
	}
	if !iss.called {
		t.Errorf("issuer not called")
	}
	if pool.insertCalls != 1 {
		t.Errorf("insertCalls = %d", pool.insertCalls)
	}
	if pool.insertOp.CreatedByAID == nil || *pool.insertOp.CreatedByAID != "archon-alice" {
		t.Errorf("CreatedByAID = %v", pool.insertOp.CreatedByAID)
	}
}

func TestOperatorCreateTyped_InvalidAID_422(t *testing.T) {
	h, _ := newHandler(t, &fakePool{}, nil)
	_, err := h.CreateTyped(context.Background(), claims("archon-alice"), OperatorCreateInput{AID: "BOB"})
	wantProblem(t, err, problem.TypeValidationFailed)
}

func TestOperatorCreateTyped_MissingAID_422(t *testing.T) {
	h, _ := newHandler(t, &fakePool{}, nil)
	_, err := h.CreateTyped(context.Background(), claims("archon-alice"), OperatorCreateInput{})
	wantProblem(t, err, problem.TypeValidationFailed)
}

// TestOperatorCreateTyped_AID_Boundaries — pattern
// ^[a-z0-9][a-z0-9._@-]{1,127}$ (ADR-014 amendment 2026-05-29):
// старт с alphanumeric обязателен, charset a-z0-9._@-, общая длина 2..128.
// Префикс archon- больше НЕ обязателен; email-подобные / ldap-uid валидны.
func TestOperatorCreateTyped_AID_Boundaries(t *testing.T) {
	cases := []struct {
		name    string
		aid     string
		wantErr bool
	}{
		{"legacy archon- prefix", "archon-bob", false},
		{"plain name (no prefix)", "bob", false},
		{"arbitrary prefix", "admin-bob", false},
		{"email-like", "alice@corp.com", false},
		{"ldap-uid with underscore", "uid_4815", false},
		{"starts with digit", "0day", false},
		{"uppercase rejected", "archon-Bob", true},
		{"path traversal rejected", "archon/../evil", true},
		{"starts with dot rejected", ".hidden", true},
		{"starts with dash rejected", "-leading", true},
		{"single char too short", "a", true},
		{"empty", "", true},
		{"len 128 → ok", "a" + strings.Repeat("b", 127), false},
		{"len 129 → 422", "a" + strings.Repeat("b", 128), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool := &fakePool{
				selectFn: func(aid string) (*operator.Operator, error) {
					return &operator.Operator{AID: aid, DisplayName: aid, AuthMethod: operator.AuthMethodJWT, CreatedAt: time.Now()}, nil
				},
			}
			h, _ := newHandler(t, pool, nil)
			_, err := h.CreateTyped(context.Background(), claims("archon-alice"), OperatorCreateInput{AID: c.aid})
			if c.wantErr {
				wantProblem(t, err, problem.TypeValidationFailed)
			} else if err != nil {
				t.Errorf("aid=%q → unexpected err %v", c.aid, err)
			}
		})
	}
}

// TestOperatorCreateTyped_DisplayName_Boundaries — длина display_name:
// пустой → ok (service подставит AID), max 200 → ok, 201 → 422.
func TestOperatorCreateTyped_DisplayName_Boundaries(t *testing.T) {
	cases := []struct {
		name    string
		dn      string
		wantErr bool
	}{
		{"empty → ok", "", false},
		{"normal → ok", "Bob Ops", false},
		{"max 200 → ok", strings.Repeat("x", maxDisplayNameLen), false},
		{"max+1 201 → 422", strings.Repeat("x", maxDisplayNameLen+1), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool := &fakePool{
				selectFn: func(aid string) (*operator.Operator, error) {
					return &operator.Operator{AID: aid, DisplayName: c.dn, AuthMethod: operator.AuthMethodJWT, CreatedAt: time.Now()}, nil
				},
			}
			h, _ := newHandler(t, pool, nil)
			_, err := h.CreateTyped(context.Background(), claims("archon-alice"),
				OperatorCreateInput{AID: "archon-bob", DisplayName: c.dn})
			if c.wantErr {
				wantProblem(t, err, problem.TypeValidationFailed)
			} else if err != nil {
				t.Errorf("display_name len=%d → unexpected err %v", len(c.dn), err)
			}
		})
	}
}

func TestOperatorCreateTyped_DuplicateAID_409(t *testing.T) {
	pool := &fakePool{insertErr: operator.ErrOperatorAlreadyExists}
	h, _ := newHandler(t, pool, nil)
	_, err := h.CreateTyped(context.Background(), claims("archon-alice"), OperatorCreateInput{AID: "archon-bob"})
	wantProblem(t, err, problem.TypeOperatorExists)
}

func TestOperatorRevokeTyped_204(t *testing.T) {
	pool := &fakePool{
		activeFn: func() ([]string, error) {
			return []string{"archon-alice", "archon-bob"}, nil
		},
	}
	h, _ := newHandler(t, pool, &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "admin", Operators: []string{"archon-alice", "archon-bob"}, Permissions: []string{"*"}},
		},
	})
	reply, err := h.RevokeTyped(context.Background(), claims("archon-alice"), "archon-bob", "left team")
	if err != nil {
		t.Fatalf("RevokeTyped: %v", err)
	}
	if reply.AID != "archon-bob" || reply.Reason != "left team" {
		t.Errorf("reply = %+v", reply)
	}
}

func TestOperatorRevokeTyped_SelfLockout_409(t *testing.T) {
	pool := &fakePool{
		activeFn: func() ([]string, error) {
			return []string{"archon-alice"}, nil // alice — единственный active
		},
	}
	h, _ := newHandler(t, pool, &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "admin", Operators: []string{"archon-alice"}, Permissions: []string{"*"}},
		},
	})
	_, err := h.RevokeTyped(context.Background(), claims("archon-alice"), "archon-alice", "")
	wantProblem(t, err, problem.TypeWouldLockOutCluster)
}

func TestOperatorRevokeTyped_NotFound_404(t *testing.T) {
	pool := &fakePool{
		// Lockout-probe: БД-admin-set пуст (activeFn nil) → target вне набора,
		// lockout невозможен. Revoke возвращает ErrOperatorNotFound (AID нет в реестре).
		revokeFn: func(aid, reason string) error { return operator.ErrOperatorNotFound },
	}
	h, _ := newHandler(t, pool, nil)
	_, err := h.RevokeTyped(context.Background(), claims("archon-alice"), "archon-bob", "")
	wantProblem(t, err, problem.TypeNotFound)
}

func TestOperatorRevokeTyped_InvalidAID_422(t *testing.T) {
	h, _ := newHandler(t, &fakePool{}, nil)
	_, err := h.RevokeTyped(context.Background(), claims("archon-alice"), "BOB", "")
	wantProblem(t, err, problem.TypeValidationFailed)
}

func TestOperatorIssueTokenTyped_200(t *testing.T) {
	pool := &fakePool{
		selectFn: func(aid string) (*operator.Operator, error) {
			return &operator.Operator{
				AID:         aid,
				DisplayName: aid,
				AuthMethod:  operator.AuthMethodJWT,
				CreatedAt:   time.Now(),
			}, nil
		},
	}
	h, iss := newHandler(t, pool, &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "admin", Operators: []string{"archon-bob"}, Permissions: []string{"*"}},
		},
	})
	reply, err := h.IssueTokenTyped(context.Background(), claims("archon-alice"), "archon-bob")
	if err != nil {
		t.Fatalf("IssueTokenTyped: %v", err)
	}
	if reply.AID != "archon-bob" {
		t.Errorf("AID = %q", reply.AID)
	}
	if !iss.called {
		t.Errorf("issuer not called")
	}
}

func TestOperatorIssueTokenTyped_Revoked_409(t *testing.T) {
	now := time.Now()
	pool := &fakePool{
		selectFn: func(aid string) (*operator.Operator, error) {
			return &operator.Operator{
				AID:        aid,
				AuthMethod: operator.AuthMethodJWT,
				RevokedAt:  &now,
			}, nil
		},
	}
	h, _ := newHandler(t, pool, nil)
	_, err := h.IssueTokenTyped(context.Background(), claims("archon-alice"), "archon-bob")
	wantProblem(t, err, problem.TypeOperatorRevoked)
}

func TestOperatorIssueTokenTyped_NotFound_404(t *testing.T) {
	pool := &fakePool{
		selectFn: func(aid string) (*operator.Operator, error) {
			return nil, operator.ErrOperatorNotFound
		},
	}
	h, _ := newHandler(t, pool, nil)
	_, err := h.IssueTokenTyped(context.Background(), claims("archon-alice"), "archon-ghost")
	wantProblem(t, err, problem.TypeNotFound)
}

func TestOperatorIssueTokenTyped_InvalidAID_422(t *testing.T) {
	h, _ := newHandler(t, &fakePool{}, nil)
	_, err := h.IssueTokenTyped(context.Background(), claims("archon-alice"), "BOB")
	wantProblem(t, err, problem.TypeValidationFailed)
}

func TestOperatorListTyped_200(t *testing.T) {
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	created := "archon-alice"
	pool := &fakePool{
		listFn: func() ([]*operator.Operator, int, error) {
			return []*operator.Operator{
				{AID: "archon-alice", DisplayName: "Alice", AuthMethod: operator.AuthMethodJWT, CreatedAt: now, CreatedVia: operator.CreatedViaBootstrap},
				{AID: "archon-bob", DisplayName: "Bob", AuthMethod: operator.AuthMethodJWT, CreatedAt: now, CreatedByAID: &created, CreatedVia: operator.CreatedViaUser},
			}, 2, nil
		},
	}
	h, _ := newHandler(t, pool, nil)
	page, err := h.ListTyped(context.Background(), operator.ListFilter{}, 0, 50)
	if err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	if page.Total != 2 || len(page.Items) != 2 {
		t.Fatalf("items=%d total=%d", len(page.Items), page.Total)
	}
	if !page.Items[0].BootstrapInitial {
		t.Errorf("alice should be bootstrap (created_via='bootstrap')")
	}
	if page.Items[1].BootstrapInitial {
		t.Errorf("bob should NOT be bootstrap (created_via='user')")
	}
}

// TestOperatorListTyped_OutOfRange_400 — границы пагинации enforce-ит ListTyped
// (sharedapi.CheckPageBounds): out-of-range → 400 (parity ParsePage), а НЕ 422.
// huma typed-int НЕ несёт schema-minimum/maximum, поэтому проверка живёт в домене.
func TestOperatorListTyped_OutOfRange_400(t *testing.T) {
	h, _ := newHandler(t, &fakePool{}, nil)
	for _, c := range []struct {
		offset, limit int
	}{
		{-1, 50},
		{0, 0},
		{0, 1001},
	} {
		_, err := h.ListTyped(context.Background(), operator.ListFilter{}, c.offset, c.limit)
		wantProblem(t, err, problem.TypeMalformedRequest)
	}
}

func TestOperatorGetTyped_200(t *testing.T) {
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	pool := &fakePool{
		selectFn: func(aid string) (*operator.Operator, error) {
			return &operator.Operator{
				AID: aid, DisplayName: "Bob", AuthMethod: operator.AuthMethodJWT, CreatedAt: now,
			}, nil
		},
	}
	h, _ := newHandler(t, pool, nil)
	view, err := h.GetTyped(context.Background(), "archon-bob")
	if err != nil {
		t.Fatalf("GetTyped: %v", err)
	}
	if view.AID != "archon-bob" {
		t.Errorf("AID = %q", view.AID)
	}
}

func TestOperatorGetTyped_NotFound_404(t *testing.T) {
	pool := &fakePool{
		selectFn: func(aid string) (*operator.Operator, error) {
			return nil, operator.ErrOperatorNotFound
		},
	}
	h, _ := newHandler(t, pool, nil)
	_, err := h.GetTyped(context.Background(), "archon-ghost")
	wantProblem(t, err, problem.TypeNotFound)
}

func TestOperatorGetTyped_InvalidAID_422(t *testing.T) {
	h, _ := newHandler(t, &fakePool{}, nil)
	_, err := h.GetTyped(context.Background(), "BOB")
	wantProblem(t, err, problem.TypeValidationFailed)
}

// --- Create with roles[] (atomic create+grant, UX-fix) ---------------

// TestOperatorCreateTyped_WithRoles_201 — happy path: roles[] возвращены в reply,
// INSERT operator + grant-ы прошли в одной tx.
func TestOperatorCreateTyped_WithRoles_201(t *testing.T) {
	pool := &fakePool{
		selectFn: func(aid string) (*operator.Operator, error) {
			return &operator.Operator{
				AID: aid, DisplayName: "Bob", AuthMethod: operator.AuthMethodJWT,
				CreatedAt: time.Now(),
			}, nil
		},
	}
	h, _ := newHandler(t, pool, nil)
	reply, err := h.CreateTyped(context.Background(), claims("archon-alice"), OperatorCreateInput{
		AID: "archon-bob", DisplayName: "Bob", Roles: []string{"cluster-readonly", "incarnation-operator"},
	})
	if err != nil {
		t.Fatalf("CreateTyped: %v", err)
	}
	if len(reply.GrantedRoles) != 2 ||
		reply.GrantedRoles[0] != "cluster-readonly" || reply.GrantedRoles[1] != "incarnation-operator" {
		t.Errorf("GrantedRoles = %v, want [cluster-readonly, incarnation-operator]", reply.GrantedRoles)
	}
	if len(pool.roleGrants) != 2 {
		t.Errorf("pool.roleGrants = %v, want 2 grant-а", pool.roleGrants)
	}
}

// TestOperatorCreateTyped_WithRoles_UnknownRole_404 — FK-violation на role_name:
// 404 (role-not-found) + tx откатывается, roleGrants пуст.
func TestOperatorCreateTyped_WithRoles_UnknownRole_404(t *testing.T) {
	pool := &fakePool{
		grantErrFor: map[string]error{
			"ghost-role": &pgconn.PgError{
				Code:           "23503",
				ConstraintName: "rbac_role_operators_role_name_fkey",
			},
		},
	}
	h, _ := newHandler(t, pool, nil)
	_, err := h.CreateTyped(context.Background(), claims("archon-alice"), OperatorCreateInput{
		AID: "archon-bob", DisplayName: "Bob", Roles: []string{"ghost-role"},
	})
	wantProblem(t, err, problem.TypeRoleNotFound)
	if len(pool.roleGrants) != 0 {
		t.Errorf("pool.roleGrants = %v, want 0 при rollback", pool.roleGrants)
	}
}

// TestOperatorCreateTyped_WithRoles_InvalidRoleName_422 — мусорный role-name ловится
// pre-валидацией ДО tx (regex не пропускает), 422 validation-failed.
func TestOperatorCreateTyped_WithRoles_InvalidRoleName_422(t *testing.T) {
	pool := &fakePool{}
	h, _ := newHandler(t, pool, nil)
	_, err := h.CreateTyped(context.Background(), claims("archon-alice"), OperatorCreateInput{
		AID: "archon-bob", DisplayName: "Bob", Roles: []string{"Bad Role!"},
	})
	wantProblem(t, err, problem.TypeValidationFailed)
	if pool.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0 — pre-валидация до tx", pool.insertCalls)
	}
}

// TestOperatorCreateTyped_WithoutRoles_201_BackwardCompat — запросы без roles
// работают как раньше: reply без GrantedRoles, INSERT без tx.
func TestOperatorCreateTyped_WithoutRoles_201_BackwardCompat(t *testing.T) {
	pool := &fakePool{
		selectFn: func(aid string) (*operator.Operator, error) {
			return &operator.Operator{
				AID: aid, DisplayName: "Bob", AuthMethod: operator.AuthMethodJWT,
				CreatedAt: time.Now(),
			}, nil
		},
	}
	h, _ := newHandler(t, pool, nil)
	reply, err := h.CreateTyped(context.Background(), claims("archon-alice"),
		OperatorCreateInput{AID: "archon-bob", DisplayName: "Bob"})
	if err != nil {
		t.Fatalf("CreateTyped: %v", err)
	}
	if len(reply.GrantedRoles) != 0 {
		t.Errorf("GrantedRoles = %v, want пусто без roles[]", reply.GrantedRoles)
	}
	if len(pool.roleGrants) != 0 {
		t.Errorf("roleGrants = %v, want 0 без roles[]", pool.roleGrants)
	}
}
