package operator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

// discardLogger — non-nil slog.Logger to /dev/null. Needed to cover
// logger branches in Create (s.logger != nil), not cluttering test output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- test harness for Service -----------------------------------------
//
// Service needs ServicePool (ExecQueryRower + BeginTx), JWTIssuer and
// RBACSource. crud_test.go already provides staticRow / errRow / fakeRows / assign;
// here we add only what's missing: SQL-routing pool with
// BeginTx, fake-tx, fake-issuer and fake-rbac.

// fakeIssuer — JWTIssuer mock. Returns fixed token or error.
type fakeIssuer struct {
	calls    int
	lastAID  string
	lastRole []string
	err      error
}

func (f *fakeIssuer) Issue(aid string, roles []string, _ time.Duration, _ bool) (string, error) {
	f.calls++
	f.lastAID = aid
	f.lastRole = roles
	if f.err != nil {
		return "", f.err
	}
	return "fake-jwt-" + aid, nil
}

// fakeRBAC — RBACSource mock. roles returned by RolesOf (same for all
// AIDs sufficient in test). Lockout-probe (Slice 3) doesn't take admin-set from RBAC —
// it's read from DB (svcPool.effectiveAdmins), so no ClusterAdmins field.
type fakeRBAC struct {
	roles []string
}

func (f *fakeRBAC) RolesOf(_ string) []string { return f.roles }

// svcTx — pgx.Tx stub for Revoke. Delegates Exec/Query/QueryRow back to
// svcPool (common SQL routing), counts Commit/Rollback. Methods outside
// Revoke scope — panic (shouldn't be called).
type svcTx struct {
	pool      *svcPool
	committed bool
	rolled    bool
	commitErr error
}

func (t *svcTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.pool.Exec(ctx, sql, args...)
}
func (t *svcTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.pool.QueryRow(ctx, sql, args...)
}
func (t *svcTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.pool.Query(ctx, sql, args...)
}
func (t *svcTx) Commit(_ context.Context) error {
	if t.commitErr != nil {
		return t.commitErr
	}
	t.committed = true
	return nil
}
func (t *svcTx) Rollback(_ context.Context) error { t.rolled = true; return nil }

func (t *svcTx) Begin(context.Context) (pgx.Tx, error) { panic("svcTx.Begin: unused") }
func (t *svcTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	panic("svcTx.CopyFrom: unused")
}
func (t *svcTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults {
	panic("svcTx.SendBatch: unused")
}
func (t *svcTx) LargeObjects() pgx.LargeObjects { panic("svcTx.LargeObjects: unused") }
func (t *svcTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	panic("svcTx.Prepare: unused")
}
func (t *svcTx) Conn() *pgx.Conn { return nil }

// svcPool — ServicePool mock with SQL routing. Each field controls
// behavior of corresponding CRUD call so Service tests create only
// needed scenario.
type svcPool struct {
	insertCalls int
	insertErr   error

	// selectFn — response for SelectByAID; nil → ErrNoRows (not found).
	selectFn func(aid string) (*Operator, error)

	// revokeTag — RowsAffected for UPDATE operators; default "UPDATE 1".
	revokeTag pgconn.CommandTag
	revokeErr error

	// effectiveAdmins — active cluster-admins that
	// rbac.LockEffectiveClusterAdmins will return (FOR UPDATE query from DB source,
	// Slice 3). Previously lockout-probe took admin-set from ClusterAdmins() snapshot
	// and intersected with active AIDs; now full admin-set comes from DB.
	effectiveAdmins []string
	queryErr        error

	// roleGrants — log of successfully inserted membership rows (role, aid)
	// for atomic create+grant path. Tests validate all requested
	// roles went through tx (or none on rollback).
	roleGrants []roleGrantArgs
	// grantErrFor — if role key matches — INSERT membership will return
	// this error (FK-violation emulation for nonexistent role/aid).
	grantErrFor map[string]error

	beginErr  error
	commitErr error
	tx        *svcTx
}

// roleGrantArgs — log entry for inserted membership row. Used by
// atomic create+grant tests.
type roleGrantArgs struct {
	role, aid, by string
}

func (p *svcPool) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	switch {
	case strings.Contains(sql, "INSERT INTO operators"):
		p.insertCalls++
		if p.insertErr != nil {
			return pgconn.CommandTag{}, p.insertErr
		}
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	case strings.Contains(sql, "INSERT INTO rbac_role_operators"):
		// args order = role_name, aid, granted_by_aid
		role, _ := args[0].(string)
		aid, _ := args[1].(string)
		var by string
		if len(args) > 2 && args[2] != nil {
			by, _ = args[2].(string)
		}
		if err, ok := p.grantErrFor[role]; ok {
			return pgconn.CommandTag{}, err
		}
		p.roleGrants = append(p.roleGrants, roleGrantArgs{role: role, aid: aid, by: by})
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	case strings.Contains(sql, "UPDATE operators"):
		if p.revokeErr != nil {
			return pgconn.CommandTag{}, p.revokeErr
		}
		if p.revokeTag.String() == "" {
			return pgconn.NewCommandTag("UPDATE 1"), nil
		}
		return p.revokeTag, nil
	}
	return pgconn.CommandTag{}, errors.New("svcPool.Exec: unexpected SQL: " + sql)
}

func (p *svcPool) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "SELECT aid, display_name") {
		if p.selectFn == nil {
			return errRow{err: pgx.ErrNoRows}
		}
		op, err := p.selectFn(args[0].(string))
		if err != nil {
			return errRow{err: err}
		}
		var createdBy, revoked any
		if op.CreatedByAID != nil {
			createdBy = *op.CreatedByAID
		}
		if op.RevokedAt != nil {
			revoked = *op.RevokedAt
		}
		createdVia := op.CreatedVia
		if createdVia == "" {
			createdVia = CreatedViaUser
		}
		return staticRow{values: []any{
			op.AID, op.DisplayName, string(op.AuthMethod), op.CreatedAt,
			createdBy, createdVia, revoked, []byte("{}"),
		}}
	}
	return errRow{err: errors.New("svcPool.QueryRow: unexpected SQL: " + sql)}
}

func (p *svcPool) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	// Synod epic S2 (ADR-049(f)): rbac.LockEffectiveClusterAdmins now sends TWO
	// locking queries — direct branch (FROM rbac_role_operators) and Synod branch
	// (FROM synod_operators). Synod branch this mock returns empty: operator
	// unit scenarios set admin-set via effectiveAdmins (direct path) and
	// don't model group admins (covered by integration-guard tests
	// rbac.synod_security_integration_test.go). Branch order checked in
	// rbac package; here only correct table routing matters.
	switch {
	case strings.Contains(sql, "FROM synod_operators"):
		if p.queryErr != nil {
			return nil, p.queryErr
		}
		return &fakeRows{values: nil}, nil
	case strings.Contains(sql, "FROM rbac_role_operators"):
		if p.queryErr != nil {
			return nil, p.queryErr
		}
		return &fakeRows{values: p.effectiveAdmins}, nil
	}
	return nil, errors.New("svcPool.Query: unexpected SQL: " + sql)
}

func (p *svcPool) BeginTx(_ context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	if p.beginErr != nil {
		return nil, p.beginErr
	}
	p.tx = &svcTx{pool: p, commitErr: p.commitErr}
	return p.tx, nil
}

// compile-time check: svcPool satisfies ServicePool.
var _ ServicePool = (*svcPool)(nil)

// newService assembles Service with test deps. TTL fixed, logger nil
// (Service tolerates nil logger — see NewService).
func newService(t *testing.T, pool ServicePool, iss JWTIssuer, rb RBACSource) *Service {
	t.Helper()
	s, err := NewService(ServiceDeps{
		Pool:       pool,
		Issuer:     iss,
		RBAC:       rb,
		TTLDefault: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return s
}

// newServiceLogged — Service with non-nil logger (to cover logger branches).
func newServiceLogged(t *testing.T, pool ServicePool, iss JWTIssuer, rb RBACSource) *Service {
	t.Helper()
	s, err := NewService(ServiceDeps{
		Pool:       pool,
		Issuer:     iss,
		RBAC:       rb,
		TTLDefault: time.Hour,
		Logger:     discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return s
}

// --- NewService ------------------------------------------------------

func TestNewService_RejectsNilDeps(t *testing.T) {
	base := ServiceDeps{
		Pool:       &svcPool{},
		Issuer:     &fakeIssuer{},
		RBAC:       &fakeRBAC{},
		TTLDefault: time.Hour,
	}
	cases := []struct {
		name   string
		mutate func(*ServiceDeps)
		want   string
	}{
		{"nil-pool", func(d *ServiceDeps) { d.Pool = nil }, "Pool is nil"},
		{"nil-issuer", func(d *ServiceDeps) { d.Issuer = nil }, "Issuer is nil"},
		{"nil-rbac", func(d *ServiceDeps) { d.RBAC = nil }, "RBAC is nil"},
		{"zero-ttl", func(d *ServiceDeps) { d.TTLDefault = 0 }, "TTLDefault must be positive"},
		{"negative-ttl", func(d *ServiceDeps) { d.TTLDefault = -time.Second }, "TTLDefault must be positive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := base
			tc.mutate(&d)
			s, err := NewService(d)
			if err == nil {
				t.Fatalf("NewService(%s): err = nil, want non-nil", tc.name)
			}
			if s != nil {
				t.Errorf("NewService(%s): service = %v, want nil on error", tc.name, s)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want substring %q", err, tc.want)
			}
		})
	}
}

func TestNewService_HappyPath(t *testing.T) {
	s, err := NewService(ServiceDeps{
		Pool:       &svcPool{},
		Issuer:     &fakeIssuer{},
		RBAC:       &fakeRBAC{},
		TTLDefault: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if s == nil {
		t.Fatal("NewService: service = nil")
	}
}

// --- Create ----------------------------------------------------------

func TestCreate_HappyPath(t *testing.T) {
	createdAt := time.Date(2026, 5, 22, 10, 0, 0, 0, time.UTC)
	parent := "archon-alice"
	pool := &svcPool{
		selectFn: func(aid string) (*Operator, error) {
			return &Operator{
				AID:          aid,
				DisplayName:  "Bob",
				AuthMethod:   AuthMethodJWT,
				CreatedAt:    createdAt,
				CreatedByAID: &parent,
			}, nil
		},
	}
	iss := &fakeIssuer{}
	s := newService(t, pool, iss, &fakeRBAC{roles: []string{"operator"}})

	res, err := s.Create(context.Background(), CreateInput{
		AID:         "archon-bob",
		DisplayName: "Bob",
		CallerAID:   "archon-alice",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if pool.insertCalls != 1 {
		t.Errorf("insertCalls = %d, want 1", pool.insertCalls)
	}
	if iss.calls != 1 {
		t.Errorf("issuer.calls = %d, want 1", iss.calls)
	}
	if iss.lastAID != "archon-bob" {
		t.Errorf("issued for AID %q, want archon-bob", iss.lastAID)
	}
	if res.JWT != "fake-jwt-archon-bob" {
		t.Errorf("JWT = %q", res.JWT)
	}
	if res.AID != "archon-bob" {
		t.Errorf("AID = %q", res.AID)
	}
	if res.CreatedByAID != "archon-alice" {
		t.Errorf("CreatedByAID = %q, want archon-alice", res.CreatedByAID)
	}
	// created_at should come from DB (SelectByAID), not local "now".
	if !res.CreatedAt.Equal(createdAt) {
		t.Errorf("CreatedAt = %v, want %v (из БД)", res.CreatedAt, createdAt)
	}
	if res.ExpiresAt.Before(time.Now()) {
		t.Errorf("ExpiresAt = %v, должен быть в будущем", res.ExpiresAt)
	}
}

func TestCreate_DefaultDisplayNameFromAID(t *testing.T) {
	pool := &svcPool{
		selectFn: func(aid string) (*Operator, error) {
			// SelectByAID will return what's in DB (display_name = AID).
			return &Operator{AID: aid, DisplayName: aid, AuthMethod: AuthMethodJWT}, nil
		},
	}
	s := newService(t, pool, &fakeIssuer{}, &fakeRBAC{})

	res, err := s.Create(context.Background(), CreateInput{
		AID:       "archon-bob",
		CallerAID: "archon-alice",
		// DisplayName empty → default = AID.
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.DisplayName != "archon-bob" {
		t.Errorf("DisplayName = %q, want archon-bob (default = AID)", res.DisplayName)
	}
}

func TestCreate_RejectsInvalidAID(t *testing.T) {
	pool := &svcPool{}
	s := newService(t, pool, &fakeIssuer{}, &fakeRBAC{})
	_, err := s.Create(context.Background(), CreateInput{AID: ".bob", CallerAID: "archon-alice"})
	if err == nil {
		t.Fatal("Create with invalid AID returned nil err")
	}
	if !strings.Contains(err.Error(), "invalid AID") {
		t.Errorf("err = %q, want substring invalid AID", err)
	}
	if pool.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0 — no round-trip on invalid AID", pool.insertCalls)
	}
}

func TestCreate_RejectsEmptyCallerAID(t *testing.T) {
	pool := &svcPool{}
	s := newService(t, pool, &fakeIssuer{}, &fakeRBAC{})
	_, err := s.Create(context.Background(), CreateInput{AID: "archon-bob", CallerAID: ""})
	if err == nil {
		t.Fatal("Create with empty CallerAID returned nil err")
	}
	if !strings.Contains(err.Error(), "CallerAID is empty") {
		t.Errorf("err = %q", err)
	}
	if pool.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0 — no insert without CallerAID", pool.insertCalls)
	}
}

func TestCreate_AlreadyExistsPropagated(t *testing.T) {
	pool := &svcPool{
		insertErr: &pgconn.PgError{
			Code:           pgErrCodeUniqueViolation,
			ConstraintName: "operators_pkey",
		},
	}
	iss := &fakeIssuer{}
	s := newService(t, pool, iss, &fakeRBAC{})

	_, err := s.Create(context.Background(), CreateInput{AID: "archon-bob", CallerAID: "archon-alice"})
	if !errors.Is(err, ErrOperatorAlreadyExists) {
		t.Fatalf("err = %v, want errors.Is ErrOperatorAlreadyExists", err)
	}
	if iss.calls != 0 {
		t.Errorf("issuer.calls = %d, want 0 — JWT not issued on Insert refusal", iss.calls)
	}
}

// TestCreate_IssueFailsAfterInsert captures error recovery: Insert committed,
// but Issue failed. Caller gets wrapped error; operator remains in DB
// (orphaned), JWT not issued — manual reconciliation (documented in Create).
func TestCreate_IssueFailsAfterInsert(t *testing.T) {
	pool := &svcPool{
		selectFn: func(aid string) (*Operator, error) {
			return &Operator{AID: aid, DisplayName: aid, AuthMethod: AuthMethodJWT}, nil
		},
	}
	iss := &fakeIssuer{err: errors.New("vault signing key unavailable")}
	s := newService(t, pool, iss, &fakeRBAC{})

	res, err := s.Create(context.Background(), CreateInput{AID: "archon-bob", CallerAID: "archon-alice"})
	if err == nil {
		t.Fatal("Create with failing issuer returned nil err")
	}
	if res != nil {
		t.Errorf("res = %v, want nil при провале Issue", res)
	}
	if !strings.Contains(err.Error(), "issue JWT failed") {
		t.Errorf("err = %q, want substring issue JWT failed", err)
	}
	// Insert already happened — operator row remains in DB (orphaned).
	if pool.insertCalls != 1 {
		t.Errorf("insertCalls = %d, want 1 — Insert committed до провала Issue", pool.insertCalls)
	}
	// Caller should be able to unwrap original cause.
	if !strings.Contains(err.Error(), "vault signing key unavailable") {
		t.Errorf("err = %q, want wrap of original Issue cause", err)
	}
}

// TestCreate_FallsBackOnPostInsertSelectFailure — SelectByAID after Insert
// failed (not ErrNoRows, but transport error). Create does NOT fail: Insert
// + JWT succeeded, created_at falls back to local "now".
func TestCreate_FallsBackOnPostInsertSelectFailure(t *testing.T) {
	before := time.Now().UTC()
	pool := &svcPool{
		selectFn: func(_ string) (*Operator, error) {
			return nil, errors.New("connection reset by peer")
		},
	}
	iss := &fakeIssuer{}
	s := newService(t, pool, iss, &fakeRBAC{})

	res, err := s.Create(context.Background(), CreateInput{
		AID:         "archon-bob",
		DisplayName: "Bob",
		CallerAID:   "archon-alice",
	})
	if err != nil {
		t.Fatalf("Create: expected success with fallback, got %v", err)
	}
	if res.JWT != "fake-jwt-archon-bob" {
		t.Errorf("JWT = %q", res.JWT)
	}
	// CreatedAt — local fallback, not from DB.
	if res.CreatedAt.Before(before) {
		t.Errorf("CreatedAt = %v, want >= %v (local fallback)", res.CreatedAt, before)
	}
	// DisplayName taken from local op (Insert argument), not from DB.
	if res.DisplayName != "Bob" {
		t.Errorf("DisplayName = %q, want Bob (local fallback)", res.DisplayName)
	}
}

// TestCreate_IssueFailsAfterInsert_LogsError — same as
// TestCreate_IssueFailsAfterInsert, but with non-nil logger: covers
// logger.Error branch (s.logger != nil) when Issue fails after Insert.
func TestCreate_IssueFailsAfterInsert_LogsError(t *testing.T) {
	pool := &svcPool{
		selectFn: func(aid string) (*Operator, error) {
			return &Operator{AID: aid, DisplayName: aid, AuthMethod: AuthMethodJWT}, nil
		},
	}
	iss := &fakeIssuer{err: errors.New("vault unreachable")}
	s := newServiceLogged(t, pool, iss, &fakeRBAC{})

	_, err := s.Create(context.Background(), CreateInput{AID: "archon-bob", CallerAID: "archon-alice"})
	if err == nil {
		t.Fatal("Create with failing issuer returned nil err")
	}
	if pool.insertCalls != 1 {
		t.Errorf("insertCalls = %d, want 1", pool.insertCalls)
	}
}

// TestCreate_PostInsertSelectFails_LogsWarn — non-nil logger on failure
// post-insert SelectByAID: covers logger.Warn branch and fallback to
// local created_at.
func TestCreate_PostInsertSelectFails_LogsWarn(t *testing.T) {
	pool := &svcPool{
		selectFn: func(_ string) (*Operator, error) {
			return nil, errors.New("read timeout")
		},
	}
	s := newServiceLogged(t, pool, &fakeIssuer{}, &fakeRBAC{})

	res, err := s.Create(context.Background(), CreateInput{
		AID:         "archon-bob",
		DisplayName: "Bob",
		CallerAID:   "archon-alice",
	})
	if err != nil {
		t.Fatalf("Create: expected success with fallback, got %v", err)
	}
	if res.DisplayName != "Bob" {
		t.Errorf("DisplayName = %q, want Bob (fallback)", res.DisplayName)
	}
}

// --- IssueToken ------------------------------------------------------

func TestIssueToken_HappyPath(t *testing.T) {
	pool := &svcPool{
		selectFn: func(aid string) (*Operator, error) {
			return &Operator{AID: aid, DisplayName: aid, AuthMethod: AuthMethodJWT}, nil
		},
	}
	iss := &fakeIssuer{}
	s := newService(t, pool, iss, &fakeRBAC{roles: []string{"operator"}})

	res, err := s.IssueToken(context.Background(), IssueTokenInput{AID: "archon-bob", CallerAID: "archon-alice"})
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	if res.AID != "archon-bob" {
		t.Errorf("AID = %q", res.AID)
	}
	if res.JWT != "fake-jwt-archon-bob" {
		t.Errorf("JWT = %q", res.JWT)
	}
	if res.ExpiresAt.Before(time.Now()) {
		t.Errorf("ExpiresAt = %v, должен быть в будущем", res.ExpiresAt)
	}
	if iss.lastRole == nil || iss.lastRole[0] != "operator" {
		t.Errorf("issued roles = %v, want [operator] из RBAC", iss.lastRole)
	}
}

func TestIssueToken_RejectsInvalidAID(t *testing.T) {
	pool := &svcPool{}
	s := newService(t, pool, &fakeIssuer{}, &fakeRBAC{})
	_, err := s.IssueToken(context.Background(), IssueTokenInput{AID: ".bob", CallerAID: "archon-alice"})
	if err == nil {
		t.Fatal("IssueToken with invalid AID returned nil err")
	}
	if !strings.Contains(err.Error(), "invalid AID") {
		t.Errorf("err = %q", err)
	}
}

func TestIssueToken_NotFound(t *testing.T) {
	pool := &svcPool{} // selectFn nil → ErrNoRows → ErrOperatorNotFound
	iss := &fakeIssuer{}
	s := newService(t, pool, iss, &fakeRBAC{})

	_, err := s.IssueToken(context.Background(), IssueTokenInput{AID: "archon-ghost", CallerAID: "archon-alice"})
	if !errors.Is(err, ErrOperatorNotFound) {
		t.Fatalf("err = %v, want ErrOperatorNotFound", err)
	}
	if iss.calls != 0 {
		t.Errorf("issuer.calls = %d, want 0 для несуществующего AID", iss.calls)
	}
}

func TestIssueToken_Revoked(t *testing.T) {
	now := time.Now().UTC()
	pool := &svcPool{
		selectFn: func(aid string) (*Operator, error) {
			return &Operator{AID: aid, DisplayName: aid, AuthMethod: AuthMethodJWT, RevokedAt: &now}, nil
		},
	}
	iss := &fakeIssuer{}
	s := newService(t, pool, iss, &fakeRBAC{})

	_, err := s.IssueToken(context.Background(), IssueTokenInput{AID: "archon-bob", CallerAID: "archon-alice"})
	if !errors.Is(err, ErrOperatorAlreadyRevoked) {
		t.Fatalf("err = %v, want ErrOperatorAlreadyRevoked", err)
	}
	if iss.calls != 0 {
		t.Errorf("issuer.calls = %d, want 0 для ревокнутого оператора", iss.calls)
	}
}

func TestIssueToken_IssueFails(t *testing.T) {
	pool := &svcPool{
		selectFn: func(aid string) (*Operator, error) {
			return &Operator{AID: aid, DisplayName: aid, AuthMethod: AuthMethodJWT}, nil
		},
	}
	iss := &fakeIssuer{err: errors.New("signing key rotation in progress")}
	s := newService(t, pool, iss, &fakeRBAC{})

	_, err := s.IssueToken(context.Background(), IssueTokenInput{AID: "archon-bob", CallerAID: "archon-alice"})
	if err == nil {
		t.Fatal("IssueToken with failing issuer returned nil err")
	}
	if !strings.Contains(err.Error(), "issue JWT failed") {
		t.Errorf("err = %q, want substring issue JWT failed", err)
	}
}

// --- Revoke (unit, via fakeTx) -------------------------------------

func TestRevoke_HappyPath_Service(t *testing.T) {
	pool := &svcPool{
		// target (archon-bob) not in admin-set → exclusion does nothing
		// changes, lockout impossible.
		effectiveAdmins: []string{"archon-alice"},
	}
	s := newService(t, pool, &fakeIssuer{}, &fakeRBAC{})

	err := s.Revoke(context.Background(), RevokeInput{AID: "archon-bob", Reason: "left team", CallerAID: "archon-alice"})
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if pool.tx == nil || !pool.tx.committed {
		t.Errorf("tx not committed: tx=%v", pool.tx)
	}
}

func TestRevoke_RejectsInvalidAID_Service(t *testing.T) {
	pool := &svcPool{}
	s := newService(t, pool, &fakeIssuer{}, &fakeRBAC{})
	err := s.Revoke(context.Background(), RevokeInput{AID: ".bob", CallerAID: "archon-alice"})
	if err == nil {
		t.Fatal("Revoke with invalid AID returned nil err")
	}
	if !strings.Contains(err.Error(), "invalid AID") {
		t.Errorf("err = %q", err)
	}
	if pool.tx != nil {
		t.Errorf("tx opened on invalid AID, want no round-trip")
	}
}

// TestRevoke_WouldLockOutCluster — target — only active
// cluster-admin. Service returns ErrWouldLockOutCluster, UPDATE doesn't go,
// tx rolls back.
func TestRevoke_WouldLockOutCluster(t *testing.T) {
	pool := &svcPool{
		effectiveAdmins: []string{"archon-alice"}, // only target is active
	}
	s := newService(t, pool, &fakeIssuer{}, &fakeRBAC{})

	err := s.Revoke(context.Background(), RevokeInput{AID: "archon-alice", CallerAID: "archon-alice"})
	if !errors.Is(err, ErrWouldLockOutCluster) {
		t.Fatalf("err = %v, want ErrWouldLockOutCluster", err)
	}
	if pool.tx != nil && pool.tx.committed {
		t.Errorf("tx committed on lockout — want rollback")
	}
}

// TestRevoke_AdminButNotLast — target in admin-set, but active admins
// more than one → revoke succeeds (lockout invariant not violated).
func TestRevoke_AdminButNotLast(t *testing.T) {
	pool := &svcPool{
		effectiveAdmins: []string{"archon-alice", "archon-bob"},
	}
	s := newService(t, pool, &fakeIssuer{}, &fakeRBAC{})

	err := s.Revoke(context.Background(), RevokeInput{AID: "archon-alice", CallerAID: "archon-bob"})
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if pool.tx == nil || !pool.tx.committed {
		t.Errorf("tx not committed")
	}
}

func TestRevoke_NotFound_Service(t *testing.T) {
	pool := &svcPool{
		effectiveAdmins: nil,
		// UPDATE 0 rows + SelectByAID → ErrNoRows → ErrOperatorNotFound.
		revokeTag: pgconn.NewCommandTag("UPDATE 0"),
		// selectFn nil → QueryRow will return ErrNoRows.
	}
	s := newService(t, pool, &fakeIssuer{}, &fakeRBAC{})

	err := s.Revoke(context.Background(), RevokeInput{AID: "archon-ghost", CallerAID: "archon-alice"})
	if !errors.Is(err, ErrOperatorNotFound) {
		t.Fatalf("err = %v, want ErrOperatorNotFound", err)
	}
}

func TestRevoke_AlreadyRevoked_Service(t *testing.T) {
	now := time.Now().UTC()
	pool := &svcPool{
		revokeTag: pgconn.NewCommandTag("UPDATE 0"),
		selectFn: func(aid string) (*Operator, error) {
			return &Operator{AID: aid, DisplayName: aid, AuthMethod: AuthMethodJWT, RevokedAt: &now}, nil
		},
	}
	s := newService(t, pool, &fakeIssuer{}, &fakeRBAC{})

	err := s.Revoke(context.Background(), RevokeInput{AID: "archon-bob", CallerAID: "archon-alice"})
	if !errors.Is(err, ErrOperatorAlreadyRevoked) {
		t.Fatalf("err = %v, want ErrOperatorAlreadyRevoked", err)
	}
}

func TestRevoke_BeginTxFails(t *testing.T) {
	pool := &svcPool{beginErr: errors.New("pool exhausted")}
	s := newService(t, pool, &fakeIssuer{}, &fakeRBAC{})

	err := s.Revoke(context.Background(), RevokeInput{AID: "archon-bob", CallerAID: "archon-alice"})
	if err == nil {
		t.Fatal("Revoke with BeginTx-failure returned nil err")
	}
	if !strings.Contains(err.Error(), "begin tx") {
		t.Errorf("err = %q, want substring begin tx", err)
	}
}

func TestRevoke_LockQueryFails(t *testing.T) {
	pool := &svcPool{
		queryErr: errors.New("FOR UPDATE deadlock detected"),
	}
	s := newService(t, pool, &fakeIssuer{}, &fakeRBAC{})

	err := s.Revoke(context.Background(), RevokeInput{AID: "archon-bob", CallerAID: "archon-alice"})
	if err == nil {
		t.Fatal("Revoke with lock-query-failure returned nil err")
	}
	if pool.tx != nil && pool.tx.committed {
		t.Errorf("tx committed on lock-query failure")
	}
}

// TestRevoke_EmptyAdminSet — RBAC tables contain no effective
// `*`-admin (Slice 3: lockout-probe ALWAYS hits DB, branches "admin-set empty,
// skip Query" no more). Query returned empty set → target not in
// admin-set → lockout impossible, revoke succeeds.
func TestRevoke_EmptyAdminSet(t *testing.T) {
	pool := &svcPool{
		effectiveAdmins: nil,
	}
	s := newService(t, pool, &fakeIssuer{}, &fakeRBAC{})

	err := s.Revoke(context.Background(), RevokeInput{AID: "archon-bob", CallerAID: "archon-alice"})
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if pool.tx == nil || !pool.tx.committed {
		t.Errorf("tx not committed")
	}
}

// TestRevoke_CommitFails — UPDATE succeeded, but COMMIT failed. Service wraps
// in "commit tx" error (caller maps to 500).
func TestRevoke_CommitFails(t *testing.T) {
	pool := &svcPool{
		effectiveAdmins: []string{"archon-alice"},
		commitErr:       errors.New("connection lost during commit"),
	}
	s := newService(t, pool, &fakeIssuer{}, &fakeRBAC{})

	err := s.Revoke(context.Background(), RevokeInput{AID: "archon-bob", CallerAID: "archon-alice"})
	if err == nil {
		t.Fatal("Revoke with commit-failure returned nil err")
	}
	if !strings.Contains(err.Error(), "commit tx") {
		t.Errorf("err = %q, want substring commit tx", err)
	}
}

// --- Create with roles (atomic create+grant) -------------------------

// TestCreate_WithRoles_GrantsAtomically — happy path UX-fix: Create accepts
// roles[], INSERT operator + GrantOperators for all roles go in one tx,
// commit succeeds → returned list of granted roles.
func TestCreate_WithRoles_GrantsAtomically(t *testing.T) {
	createdAt := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)
	parent := "archon-alice"
	pool := &svcPool{
		selectFn: func(aid string) (*Operator, error) {
			return &Operator{
				AID: aid, DisplayName: "Bob", AuthMethod: AuthMethodJWT,
				CreatedAt: createdAt, CreatedByAID: &parent,
			}, nil
		},
	}
	s := newService(t, pool, &fakeIssuer{}, &fakeRBAC{roles: []string{"cluster-readonly"}})

	res, err := s.Create(context.Background(), CreateInput{
		AID: "archon-bob", DisplayName: "Bob", CallerAID: "archon-alice",
		Roles: []string{"cluster-readonly", "incarnation-operator"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if pool.insertCalls != 1 {
		t.Errorf("insertCalls = %d, want 1", pool.insertCalls)
	}
	if len(pool.roleGrants) != 2 {
		t.Fatalf("roleGrants = %d, want 2", len(pool.roleGrants))
	}
	if pool.roleGrants[0].role != "cluster-readonly" || pool.roleGrants[0].aid != "archon-bob" {
		t.Errorf("grant[0] = %+v", pool.roleGrants[0])
	}
	if pool.roleGrants[0].by != "archon-alice" {
		t.Errorf("granted_by[0] = %q, want archon-alice", pool.roleGrants[0].by)
	}
	if pool.roleGrants[1].role != "incarnation-operator" {
		t.Errorf("grant[1] = %+v", pool.roleGrants[1])
	}
	if pool.tx == nil || !pool.tx.committed {
		t.Errorf("tx not committed: tx=%v", pool.tx)
	}
	if len(res.GrantedRoles) != 2 || res.GrantedRoles[0] != "cluster-readonly" {
		t.Errorf("GrantedRoles = %v, want [cluster-readonly, incarnation-operator]", res.GrantedRoles)
	}
}

// TestCreate_WithRoles_UnknownRole_Rollback — FK-violation on role_name →
// rbac.ErrRoleNotFound passed to caller, tx rolls back, operator
// NOT created (insertCalls=1 in mock — Insert succeeded, but Commit didn't),
// roleGrants empty for failed role.
func TestCreate_WithRoles_UnknownRole_Rollback(t *testing.T) {
	pool := &svcPool{
		grantErrFor: map[string]error{
			"ghost-role": &pgconn.PgError{
				Code:           pgErrCodeForeignKeyViolation,
				ConstraintName: "rbac_role_operators_role_name_fkey",
			},
		},
	}
	s := newService(t, pool, &fakeIssuer{}, &fakeRBAC{})

	_, err := s.Create(context.Background(), CreateInput{
		AID: "archon-bob", CallerAID: "archon-alice",
		Roles: []string{"ghost-role"},
	})
	if !errors.Is(err, rbac.ErrRoleNotFound) {
		t.Fatalf("err = %v, want errors.Is(err, rbac.ErrRoleNotFound)", err)
	}
	if pool.tx == nil {
		t.Fatal("tx не открыта")
	}
	if pool.tx.committed {
		t.Errorf("tx закоммичена при FK-violation на role — want rollback")
	}
	if !pool.tx.rolled {
		t.Errorf("tx не откачена явно")
	}
	if len(pool.roleGrants) != 0 {
		t.Errorf("roleGrants = %d, want 0 при rollback", len(pool.roleGrants))
	}
}

// TestCreate_WithRoles_InvalidRoleName_Pre — pre-validation of role name before tx:
// garbage name caught by regex, tx not opened.
func TestCreate_WithRoles_InvalidRoleName_Pre(t *testing.T) {
	pool := &svcPool{}
	s := newService(t, pool, &fakeIssuer{}, &fakeRBAC{})

	_, err := s.Create(context.Background(), CreateInput{
		AID: "archon-bob", CallerAID: "archon-alice",
		Roles: []string{"Bad Role!"},
	})
	if err == nil {
		t.Fatal("err = nil, want invalid role name")
	}
	if !strings.Contains(err.Error(), "invalid role name") {
		t.Errorf("err = %q, want substring invalid role name", err)
	}
	if pool.tx != nil {
		t.Errorf("tx открыта на битом имени — want нет round-trip")
	}
	if pool.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0 — pre-валидация ДО Insert", pool.insertCalls)
	}
}

// TestCreate_WithRoles_PublishesInvalidate — after successful atomic create+grant
// service calls Invalidator (cluster-wide RBAC invalidation — parity with
// rbac.Service.GrantOperator). Without roles publish does NOT go (no membership
// changes).
func TestCreate_WithRoles_PublishesInvalidate(t *testing.T) {
	pool := &svcPool{
		selectFn: func(aid string) (*Operator, error) {
			return &Operator{AID: aid, DisplayName: aid, AuthMethod: AuthMethodJWT}, nil
		},
	}
	s := newService(t, pool, &fakeIssuer{}, &fakeRBAC{})
	inv := &countingInvalidator{}
	s.SetInvalidator(inv)

	_, err := s.Create(context.Background(), CreateInput{
		AID: "archon-bob", CallerAID: "archon-alice",
		Roles: []string{"some-role"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := inv.calls.Load(); got != 1 {
		t.Fatalf("Invalidate calls = %d, want 1 после atomic create+grant", got)
	}
}

// TestCreate_WithoutRoles_NoInvalidate — flip side: without roles in
// request publish not called (no membership changes, save traffic).
func TestCreate_WithoutRoles_NoInvalidate(t *testing.T) {
	pool := &svcPool{
		selectFn: func(aid string) (*Operator, error) {
			return &Operator{AID: aid, DisplayName: aid, AuthMethod: AuthMethodJWT}, nil
		},
	}
	s := newService(t, pool, &fakeIssuer{}, &fakeRBAC{})
	inv := &countingInvalidator{}
	s.SetInvalidator(inv)

	if _, err := s.Create(context.Background(), CreateInput{
		AID: "archon-bob", CallerAID: "archon-alice",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := inv.calls.Load(); got != 0 {
		t.Errorf("Invalidate calls = %d, want 0 без ролей в запросе", got)
	}
}

// TestCreate_WithRoles_DoesNotPublishOnRollback — FK-violation → rollback →
// Invalidate NOT called (membership not committed, no point noise).
func TestCreate_WithRoles_DoesNotPublishOnRollback(t *testing.T) {
	pool := &svcPool{
		grantErrFor: map[string]error{
			"ghost-role": &pgconn.PgError{
				Code:           pgErrCodeForeignKeyViolation,
				ConstraintName: "rbac_role_operators_role_name_fkey",
			},
		},
	}
	s := newService(t, pool, &fakeIssuer{}, &fakeRBAC{})
	inv := &countingInvalidator{}
	s.SetInvalidator(inv)

	_, err := s.Create(context.Background(), CreateInput{
		AID: "archon-bob", CallerAID: "archon-alice",
		Roles: []string{"ghost-role"},
	})
	if err == nil {
		t.Fatal("err = nil, want FK-violation")
	}
	if got := inv.calls.Load(); got != 0 {
		t.Errorf("Invalidate calls = %d, want 0 при rollback", got)
	}
}

// TestCreate_WithRoles_BeginTxFails — BeginTx failure on atomic path:
// wrapped error returned, insert does not happen.
func TestCreate_WithRoles_BeginTxFails(t *testing.T) {
	pool := &svcPool{beginErr: errors.New("pool exhausted")}
	s := newService(t, pool, &fakeIssuer{}, &fakeRBAC{})

	_, err := s.Create(context.Background(), CreateInput{
		AID: "archon-bob", CallerAID: "archon-alice",
		Roles: []string{"some-role"},
	})
	if err == nil {
		t.Fatal("err = nil, want begin tx failure")
	}
	if !strings.Contains(err.Error(), "begin tx") {
		t.Errorf("err = %q, want substring begin tx", err)
	}
	if pool.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0 при провале BeginTx", pool.insertCalls)
	}
}

// --- isInSet ---------------------------------------------------------

func TestIsInSet(t *testing.T) {
	cases := []struct {
		name   string
		set    []string
		target string
		want   bool
	}{
		{"empty-set", nil, "archon-alice", false},
		{"empty-slice", []string{}, "archon-alice", false},
		{"single-hit", []string{"archon-alice"}, "archon-alice", true},
		{"single-miss", []string{"archon-alice"}, "archon-bob", false},
		{"multi-hit-first", []string{"archon-alice", "archon-bob"}, "archon-alice", true},
		{"multi-hit-last", []string{"archon-alice", "archon-bob"}, "archon-bob", true},
		{"multi-miss", []string{"archon-alice", "archon-bob"}, "archon-charlie", false},
		{"empty-target", []string{"archon-alice"}, "", false},
		{"case-sensitive", []string{"archon-alice"}, "ARCHON-ALICE", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isInSet(tc.set, tc.target); got != tc.want {
				t.Errorf("isInSet(%v, %q) = %v, want %v", tc.set, tc.target, got, tc.want)
			}
		})
	}
}
