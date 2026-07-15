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

// T5d handler-native PILOT: operator (w,r) wrappers removed — HTTP is served by huma
// full-typed (huma_operator_test.go: golden-wire / unknown-field-400 / bind-enum-422 /
// bind-bool-400 / RBAC-403 / S6-audit on the real huma wiring). These unit tests
// cover what huma integration does NOT: the DOMAIN error classification of the
// *Typed functions (sentinel→problem.Type) and atomic create+grant. They call *Typed
// directly, without httptest(w,r) — the bind/decode phase (JSON-decode / enum-validate /
// bool-parse) is held by huma at the boundary, not the handler.

// claims builds keeperjwt.Claims to call *Typed directly.
func claims(subject string) *keeperjwt.Claims { return &keeperjwt.Claims{Subject: subject} }

// wantProblem checks that err is a domain *problemError with the expected problem.Type.
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

// fakeIssuer — minimal JWTIssuer mock: returns a fixed token.
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

// fakePool — narrow mock [handlers.OperatorPool]. Implements Exec/QueryRow/Query
// for operator CRUD + BeginTx returning a fakeTx wrapper. Tx methods
// (Commit/Rollback) are no-ops; the tx wrapper just proxies Exec/QueryRow/Query
// to the parent fakePool. We don't test race conditions here (that's an integration
// test in /internal/api/integration_test.go); the revoke handler unit test
// just checks that the lockout logic assembled and Revoke is called.
type fakePool struct {
	insertErr   error
	insertCalls int
	insertOp    *operator.Operator

	selectFn func(aid string) (*operator.Operator, error)
	revokeFn func(aid, reason string) error
	activeFn func() ([]string, error)
	listFn   func() ([]*operator.Operator, int, error)

	// roleGrants — log of membership INSERTs on the atomic create+grant path
	// (POST /v1/operators with roles[]).
	roleGrants []string
	// grantErrFor — map role → membership INSERT error (FK-violation
	// emulation). Tests need to check that 404 on a nonexistent role works
	// correctly and the tx rolls back.
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
			// SQL with reason → args=[aid, reason]; without reason → args=[aid].
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
		// List-COUNT — issued before SELECT operators in List(); tests care
		// only that the number is consistent with listFn (or 0 without listFn).
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
	// Synod branch of the self-lockout core (ADR-049(f), Synod epic S2):
	// LockEffectiveClusterAdmins sends a second locking query over synod_operators.
	// operator-handler scenarios don't model group admins — empty; they're
	// covered by rbac integration-guard tests. Checked BEFORE the direct branch
	// (the synod_operators marker is unambiguous).
	if strings.Contains(sql, "FROM synod_operators") {
		return &stringRows{}, nil
	}
	// Slice 3: the lockout probe goes through rbac.LockEffectiveClusterAdmins —
	// SELECT ro.aid FROM rbac_role_operators JOIN … FOR UPDATE OF ro,rp,o.
	// activeFn returns the already-effective set of active `*`-admins from the DB
	// (the whole admin set comes from the DB source, so no intersection with
	// active AIDs is needed).
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
		// List(operators) → ORDER BY created_at DESC. Tests don't validate the WHERE
		// predicate precisely — that's operator.crud_test's level; here we ensure
		// the handler correctly passes rows through from the service layer.
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

// operatorRows — pgx.Rows stub for listing operators. Returns rows in
// slice order (we don't simulate DB sorting — the test checks the shape,
// not the SQL).
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

// BeginTx returns a fakeTx delegating back to fakePool. That's
// enough for revoke-handler unit tests: what matters is that the transactional
// path assembled; consistency is tested by integration tests (testcontainers PG).
func (f *fakePool) BeginTx(_ context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	if f.beginErr != nil {
		return nil, f.beginErr
	}
	return &fakeTx{pool: f}, nil
}

// fakeTx — a fakePool wrapper for unit tests: implements pgx.Tx via a
// pgx.Tx stub (all methods can be stubbed via composition with a no-op
// struct). Our scope needs only Exec/Query/QueryRow/Commit/Rollback;
// the rest panic on call, since they must not be invoked.
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
		case *bool:
			*d = r.values[i].(bool)
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
// must start with alphanumeric, charset a-z0-9._@-, total length 2..128.
// The archon- prefix is no longer required; email-like / ldap-uid are valid.
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

// TestOperatorCreateTyped_DisplayName_Boundaries — display_name length:
// empty → ok (service substitutes AID), max 200 → ok, 201 → 422.
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
			return []string{"archon-alice"}, nil // alice — the only active
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
		// Lockout probe: DB admin set empty (activeFn nil) → target outside the set,
		// lockout impossible. Revoke returns ErrOperatorNotFound (AID not in the registry).
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

// TestOperatorListTyped_OutOfRange_400 — pagination bounds are enforced by ListTyped
// (sharedapi.CheckPageBounds): out-of-range → 400 (parity ParsePage), NOT 422.
// A huma typed-int carries no schema minimum/maximum, so the check lives in the domain.
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

// TestOperatorCreateTyped_WithRoles_201 — happy path: roles[] returned in reply,
// INSERT operator + grants ran in one tx.
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

// TestOperatorCreateTyped_WithRoles_UnknownRole_404 — FK-violation on role_name:
// 404 (role-not-found) + tx rolls back, roleGrants empty.
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

// TestOperatorCreateTyped_WithRoles_InvalidRoleName_422 — garbage role-name caught
// by pre-validation BEFORE tx (regex rejects it), 422 validation-failed.
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

// TestOperatorCreateTyped_WithoutRoles_201_BackwardCompat — requests without roles
// work as before: reply without GrantedRoles, INSERT without tx.
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
