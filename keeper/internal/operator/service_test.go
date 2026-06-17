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

// discardLogger — non-nil slog.Logger в /dev/null. Нужен, чтобы покрыть
// logger-ветки Create (s.logger != nil), не засоряя вывод теста.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- test harness для Service -----------------------------------------
//
// Service-у нужны ServicePool (ExecQueryRower + BeginTx), JWTIssuer и
// RBACSource. crud_test.go уже даёт staticRow / errRow / fakeRows / assign;
// здесь добавляется только то, чего там нет: SQL-маршрутизирующий pool с
// BeginTx, fake-tx, fake-issuer и fake-rbac.

// fakeIssuer — JWTIssuer-mock. Возвращает фиксированный токен либо ошибку.
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

// fakeRBAC — RBACSource-mock. roles возвращается RolesOf (одинаковый для всех
// AID в тесте достаточно). Lockout-probe (Slice 3) admin-set из RBAC не берёт —
// он читается из БД (svcPool.effectiveAdmins), поэтому ClusterAdmins-поля нет.
type fakeRBAC struct {
	roles []string
}

func (f *fakeRBAC) RolesOf(_ string) []string { return f.roles }

// svcTx — pgx.Tx-stub для Revoke. Делегирует Exec/Query/QueryRow обратно в
// svcPool (маршрутизация по SQL общая), считает Commit/Rollback. Методы вне
// scope-а Revoke — panic (не должны вызываться).
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

// svcPool — ServicePool-mock с SQL-маршрутизацией. Каждое поле управляет
// поведением соответствующего CRUD-вызова, чтобы тесты Service лепили только
// нужный сценарий.
type svcPool struct {
	insertCalls int
	insertErr   error

	// selectFn — ответ SelectByAID; nil → ErrNoRows (not found).
	selectFn func(aid string) (*Operator, error)

	// revokeTag — RowsAffected для UPDATE operators; по умолчанию "UPDATE 1".
	revokeTag pgconn.CommandTag
	revokeErr error

	// effectiveAdmins — активные cluster-admin-ы, которые вернёт
	// rbac.LockEffectiveClusterAdmins (FOR UPDATE-Query по БД-источнику,
	// Slice 3). Раньше lockout-probe брал admin-set из ClusterAdmins()-снимка
	// и пересекал с active-AID-ами; теперь весь admin-set приходит из БД.
	effectiveAdmins []string
	queryErr        error

	// roleGrants — лог успешно вставленных membership-строк (role, aid)
	// для atomic create+grant пути. Тесты валидируют, что все запрошенные
	// роли прошли через tx (или ни одной при rollback).
	roleGrants []roleGrantArgs
	// grantErrFor — если ключ role совпадает — INSERT membership-а вернёт
	// эту ошибку (FK-violation эмуляция для несуществующей роли/aid).
	grantErrFor map[string]error

	beginErr  error
	commitErr error
	tx        *svcTx
}

// roleGrantArgs — лог-запись о вставленной membership-строке. Используется
// тестами atomic create+grant.
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
		return staticRow{values: []any{
			op.AID, op.DisplayName, string(op.AuthMethod), op.CreatedAt,
			createdBy, revoked, []byte("{}"),
		}}
	}
	return errRow{err: errors.New("svcPool.QueryRow: unexpected SQL: " + sql)}
}

func (p *svcPool) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	// Synod-эпик S2 (ADR-049(f)): rbac.LockEffectiveClusterAdmins теперь шлёт ДВА
	// locking-запроса — прямую ветку (FROM rbac_role_operators) и Synod-ветку
	// (FROM synod_operators). Synod-ветку этот mock возвращает пустой: operator-
	// unit-сценарии задают admin-set через effectiveAdmins (прямой путь) и
	// групповых админов не моделируют (их покрывают integration-guard-тесты
	// rbac.synod_security_integration_test.go). Порядок веток проверяется в
	// rbac-пакете; здесь важна лишь корректная маршрутизация по таблице.
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

// compile-time check: svcPool удовлетворяет ServicePool.
var _ ServicePool = (*svcPool)(nil)

// newService собирает Service с тестовыми deps. TTL фиксирован, logger nil
// (Service nil-логгер терпит — см. NewService).
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

// newServiceLogged — Service с non-nil логгером (для покрытия logger-веток).
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
	// created_at должен прийти из БД (SelectByAID), а не локальный «сейчас».
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
			// SelectByAID вернёт то, что записано в БД (display_name = AID).
			return &Operator{AID: aid, DisplayName: aid, AuthMethod: AuthMethodJWT}, nil
		},
	}
	s := newService(t, pool, &fakeIssuer{}, &fakeRBAC{})

	res, err := s.Create(context.Background(), CreateInput{
		AID:       "archon-bob",
		CallerAID: "archon-alice",
		// DisplayName пуст → default = AID.
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
		t.Errorf("insertCalls = %d, want 0 — нет round-trip на невалидном AID", pool.insertCalls)
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
		t.Errorf("insertCalls = %d, want 0 — нет insert без CallerAID", pool.insertCalls)
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
		t.Errorf("issuer.calls = %d, want 0 — JWT не выпускается при отказе Insert", iss.calls)
	}
}

// TestCreate_IssueFailsAfterInsert фиксирует error-recovery: Insert committed,
// но Issue упал. Caller получает обёрнутую ошибку; operator остаётся в БД
// (orphaned), JWT не выдан — manual reconciliation (документировано в Create).
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
	// Insert уже произошёл — операторо-строка осталась в БД (orphaned).
	if pool.insertCalls != 1 {
		t.Errorf("insertCalls = %d, want 1 — Insert committed до провала Issue", pool.insertCalls)
	}
	// Caller должен иметь возможность развернуть оригинальную причину.
	if !strings.Contains(err.Error(), "vault signing key unavailable") {
		t.Errorf("err = %q, want wrap оригинальной причины Issue", err)
	}
}

// TestCreate_FallsBackOnPostInsertSelectFailure — SelectByAID после Insert
// упал (не ErrNoRows, а транспортная ошибка). Create НЕ проваливается: Insert
// + JWT успешны, created_at падает back на локальное «сейчас».
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
		t.Fatalf("Create: ожидался успех с fallback, got %v", err)
	}
	if res.JWT != "fake-jwt-archon-bob" {
		t.Errorf("JWT = %q", res.JWT)
	}
	// CreatedAt — локальный fallback, не из БД.
	if res.CreatedAt.Before(before) {
		t.Errorf("CreatedAt = %v, want >= %v (локальный fallback)", res.CreatedAt, before)
	}
	// DisplayName взят из локального op (Insert-аргумент), не из БД.
	if res.DisplayName != "Bob" {
		t.Errorf("DisplayName = %q, want Bob (локальный fallback)", res.DisplayName)
	}
}

// TestCreate_IssueFailsAfterInsert_LogsError — то же, что
// TestCreate_IssueFailsAfterInsert, но с non-nil логгером: покрывает
// logger.Error-ветку (s.logger != nil) при провале Issue после Insert.
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

// TestCreate_PostInsertSelectFails_LogsWarn — non-nil логгер при провале
// post-insert SelectByAID: покрывает logger.Warn-ветку и fallback на
// локальный created_at.
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
		t.Fatalf("Create: ожидался успех с fallback, got %v", err)
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

// --- Revoke (unit, через fakeTx) -------------------------------------

func TestRevoke_HappyPath_Service(t *testing.T) {
	pool := &svcPool{
		// target (archon-bob) не входит в admin-set → exclusion ничего не
		// меняет, lockout невозможен.
		effectiveAdmins: []string{"archon-alice"},
	}
	s := newService(t, pool, &fakeIssuer{}, &fakeRBAC{})

	err := s.Revoke(context.Background(), RevokeInput{AID: "archon-bob", Reason: "left team", CallerAID: "archon-alice"})
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if pool.tx == nil || !pool.tx.committed {
		t.Errorf("tx не закоммичена: tx=%v", pool.tx)
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
		t.Errorf("tx открыта на невалидном AID, want нет round-trip")
	}
}

// TestRevoke_WouldLockOutCluster — target — единственный активный
// cluster-admin. Service возвращает ErrWouldLockOutCluster, UPDATE не идёт,
// tx откатывается.
func TestRevoke_WouldLockOutCluster(t *testing.T) {
	pool := &svcPool{
		effectiveAdmins: []string{"archon-alice"}, // только сам target активен
	}
	s := newService(t, pool, &fakeIssuer{}, &fakeRBAC{})

	err := s.Revoke(context.Background(), RevokeInput{AID: "archon-alice", CallerAID: "archon-alice"})
	if !errors.Is(err, ErrWouldLockOutCluster) {
		t.Fatalf("err = %v, want ErrWouldLockOutCluster", err)
	}
	if pool.tx != nil && pool.tx.committed {
		t.Errorf("tx закоммичена при lockout — want rollback")
	}
}

// TestRevoke_AdminButNotLast — target в admin-set, но активных admin-ов
// больше одного → revoke проходит (lockout-инвариант не нарушается).
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
		t.Errorf("tx не закоммичена")
	}
}

func TestRevoke_NotFound_Service(t *testing.T) {
	pool := &svcPool{
		effectiveAdmins: nil,
		// UPDATE 0 rows + SelectByAID → ErrNoRows → ErrOperatorNotFound.
		revokeTag: pgconn.NewCommandTag("UPDATE 0"),
		// selectFn nil → QueryRow вернёт ErrNoRows.
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
		t.Errorf("tx закоммичена при провале lock-query")
	}
}

// TestRevoke_EmptyAdminSet — RBAC-таблицы не содержат ни одного эффективного
// `*`-admin-а (Slice 3: lockout-probe ВСЕГДА бьёт в БД, ветки «admin-set пуст,
// пропускаем Query» больше нет). Query вернул пустой набор → target не в
// admin-set → lockout невозможен, revoke проходит.
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
		t.Errorf("tx не закоммичена")
	}
}

// TestRevoke_CommitFails — UPDATE прошёл, но COMMIT упал. Service оборачивает
// в "commit tx"-ошибку (caller маппит в 500).
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

// TestCreate_WithRoles_GrantsAtomically — happy path UX-fix: Create принимает
// roles[], INSERT operator-а + GrantOperator-ы для всех ролей идут одной tx,
// commit успешен → возвращён список granted-ролей.
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
		t.Errorf("tx не закоммичена: tx=%v", pool.tx)
	}
	if len(res.GrantedRoles) != 2 || res.GrantedRoles[0] != "cluster-readonly" {
		t.Errorf("GrantedRoles = %v, want [cluster-readonly, incarnation-operator]", res.GrantedRoles)
	}
}

// TestCreate_WithRoles_UnknownRole_Rollback — FK-violation на role_name →
// rbac.ErrRoleNotFound пробрасывается caller-у, tx откатывается, оператор
// НЕ создан (insertCalls=1 в моке — Insert прошёл, но Commit не было),
// roleGrants пуст для проваленной роли.
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

// TestCreate_WithRoles_InvalidRoleName_Pre — pre-валидация role-имени до tx:
// мусорное имя ловится regex-ом, tx не открывается.
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

// TestCreate_WithRoles_PublishesInvalidate — после успешного atomic create+grant
// service дёргает Invalidator (cluster-wide RBAC-инвалидация — parity с
// rbac.Service.GrantOperator). Без ролей publish НЕ ходит (нет membership-
// изменения).
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

// TestCreate_WithoutRoles_NoInvalidate — обратная сторона: без ролей в
// запросе publish не дёргается (нет membership-изменения, экономим трафик).
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
// Invalidate НЕ вызывается (membership не зафиксирован, нет смысла шуметь).
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

// TestCreate_WithRoles_BeginTxFails — провал BeginTx на atomic-пути:
// возвращается обёрнутая ошибка, insert не происходит.
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
