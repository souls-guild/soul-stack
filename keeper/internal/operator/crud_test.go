package operator

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeDB — мок execQueryRower для unit-тестов. Захватывает аргументы
// последнего Exec и QueryRow-а. Возврат подменяется через поля execErr /
// rowFunc.
type fakeDB struct {
	execCalls    int
	lastExecSQL  string
	lastExecArgs []any
	execTag      pgconn.CommandTag
	execErr      error

	queryCalls   int
	lastQuerySQL string
	rowFunc      func() pgx.Row

	// queryFunc — реализация Query (multi-row). По умолчанию возвращает
	// rows-stub с одним проходом и пустым результатом (Next() → false).
	queryFunc func() (pgx.Rows, error)
}

func (f *fakeDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.execCalls++
	f.lastExecSQL = sql
	f.lastExecArgs = args
	return f.execTag, f.execErr
}

func (f *fakeDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	f.queryCalls++
	f.lastQuerySQL = sql
	if f.rowFunc != nil {
		return f.rowFunc()
	}
	return errRow{err: pgx.ErrNoRows}
}

func (f *fakeDB) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	f.lastQuerySQL = sql
	if f.queryFunc != nil {
		return f.queryFunc()
	}
	return &fakeRows{}, nil
}

// fakeRows — pgx.Rows-stub для Query-метода fakeDB (execQueryRower требует
// Query). Прогоняет values по одному в Scan; Next() возвращает false после
// исчерпания.
type fakeRows struct {
	values []string
	idx    int
	err    error
}

func (r *fakeRows) Next() bool {
	if r.err != nil {
		return false
	}
	if r.idx >= len(r.values) {
		return false
	}
	r.idx++
	return true
}

func (r *fakeRows) Scan(dest ...any) error {
	if len(dest) != 1 {
		return errors.New("fakeRows: expected single dest")
	}
	p, ok := dest[0].(*string)
	if !ok {
		return errors.New("fakeRows: dest is not *string")
	}
	*p = r.values[r.idx-1]
	return nil
}

func (r *fakeRows) Err() error                                   { return r.err }
func (r *fakeRows) Close()                                       {}
func (r *fakeRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeRows) Values() ([]any, error)                       { return nil, nil }
func (r *fakeRows) RawValues() [][]byte                          { return nil }
func (r *fakeRows) Conn() *pgx.Conn                              { return nil }

// errRow — pgx.Row, всегда возвращающий заранее заданный err при Scan.
type errRow struct{ err error }

func (r errRow) Scan(_ ...any) error { return r.err }

// staticRow — pgx.Row, копирующий значения из values в Scan-аргументы.
type staticRow struct {
	values []any
	err    error
}

func (r staticRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		return errors.New("staticRow: len mismatch")
	}
	for i, d := range dest {
		assign(d, r.values[i])
	}
	return nil
}

// assign — упрощённый Scan: подставляет значение в указатель. Поддерживает
// только типы, реально используемые операторами (string, time.Time, int64,
// *string, *time.Time, []byte). Не пытается универсализировать; добавится
// тип — добавится case.
func assign(dest, src any) {
	switch d := dest.(type) {
	case *string:
		*d = src.(string)
	case *int64:
		*d = src.(int64)
	case *time.Time:
		*d = src.(time.Time)
	case **string:
		if src == nil {
			*d = nil
		} else {
			s := src.(string)
			*d = &s
		}
	case **time.Time:
		if src == nil {
			*d = nil
		} else {
			t := src.(time.Time)
			*d = &t
		}
	case *[]byte:
		if src == nil {
			*d = nil
		} else {
			*d = src.([]byte)
		}
	default:
		panic("staticRow.assign: unsupported dest type")
	}
}

func TestInsert_HappyPath_Bootstrap(t *testing.T) {
	f := &fakeDB{}
	op := &Operator{
		AID:         "archon-alice",
		DisplayName: "Alice Admin",
		AuthMethod:  AuthMethodJWT,
		CreatedVia:  CreatedViaBootstrap,
		// CreatedByAID = nil → bootstrap, NULL в БД.
		// CreatedAt zero → DEFAULT NOW().
	}
	if err := Insert(context.Background(), f, op); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if f.execCalls != 1 {
		t.Fatalf("execCalls = %d, want 1", f.execCalls)
	}
	if !strings.Contains(f.lastExecSQL, "INSERT INTO operators") {
		t.Errorf("SQL: %q", f.lastExecSQL)
	}
	if len(f.lastExecArgs) != 8 {
		t.Fatalf("args len = %d, want 8", len(f.lastExecArgs))
	}
	if f.lastExecArgs[0] != "archon-alice" {
		t.Errorf("args[0] aid = %v", f.lastExecArgs[0])
	}
	if f.lastExecArgs[1] != "Alice Admin" {
		t.Errorf("args[1] display_name = %v", f.lastExecArgs[1])
	}
	if f.lastExecArgs[2] != "jwt" {
		t.Errorf("args[2] auth_method = %v", f.lastExecArgs[2])
	}
	if f.lastExecArgs[3] != nil {
		t.Errorf("args[3] created_at = %v, want nil (DEFAULT NOW())", f.lastExecArgs[3])
	}
	if f.lastExecArgs[4] != nil {
		t.Errorf("args[4] created_by_aid = %v, want nil for bootstrap", f.lastExecArgs[4])
	}
	// created_via явно передан bootstrap (этот тест эмулирует bootstrap-вставку).
	if f.lastExecArgs[5] != CreatedViaBootstrap {
		t.Errorf("args[5] created_via = %v, want %q", f.lastExecArgs[5], CreatedViaBootstrap)
	}
	if f.lastExecArgs[6] != nil {
		t.Errorf("args[6] revoked_at = %v, want nil", f.lastExecArgs[6])
	}
	if b, ok := f.lastExecArgs[7].([]byte); !ok || string(b) != "{}" {
		t.Errorf("args[7] metadata = %v (%T), want \"{}\"", f.lastExecArgs[7], f.lastExecArgs[7])
	}
}

func TestInsert_PassesCreatedByAIDAndMetadata(t *testing.T) {
	f := &fakeDB{}
	parent := "archon-alice"
	op := &Operator{
		AID:          "archon-bob",
		DisplayName:  "Bob",
		AuthMethod:   AuthMethodJWT,
		CreatedByAID: &parent,
		CreatedAt:    time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC),
		Metadata:     map[string]any{"team": "ops"},
	}
	if err := Insert(context.Background(), f, op); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if f.lastExecArgs[3] == nil {
		t.Errorf("args[3] created_at = nil, want non-nil")
	}
	if f.lastExecArgs[4] != "archon-alice" {
		t.Errorf("args[4] created_by_aid = %v, want \"archon-alice\"", f.lastExecArgs[4])
	}
	// CreatedVia не задан в Operator-е → default 'user' (ADR-058(d)).
	if f.lastExecArgs[5] != CreatedViaUser {
		t.Errorf("args[5] created_via = %v, want %q (default)", f.lastExecArgs[5], CreatedViaUser)
	}
	b, ok := f.lastExecArgs[7].([]byte)
	if !ok {
		t.Fatalf("args[7] = %T, want []byte", f.lastExecArgs[7])
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("metadata not JSON: %v", err)
	}
	if got["team"] != "ops" {
		t.Errorf("metadata.team = %v", got["team"])
	}
}

func TestInsert_RejectsInvalidAID(t *testing.T) {
	f := &fakeDB{}
	op := &Operator{AID: ".alice", DisplayName: "x", AuthMethod: AuthMethodJWT}
	err := Insert(context.Background(), f, op)
	if err == nil {
		t.Fatal("Insert with invalid AID returned nil err")
	}
	if !strings.Contains(err.Error(), "invalid AID") {
		t.Errorf("err = %v, want substring \"invalid AID\"", err)
	}
	if f.execCalls != 0 {
		t.Errorf("execCalls = %d on invalid AID; want 0", f.execCalls)
	}
}

func TestInsert_RejectsInvalidAuthMethod(t *testing.T) {
	f := &fakeDB{}
	op := &Operator{AID: "archon-alice", DisplayName: "x", AuthMethod: AuthMethod("hax")}
	err := Insert(context.Background(), f, op)
	if err == nil {
		t.Fatal("Insert with invalid auth_method returned nil err")
	}
	if f.execCalls != 0 {
		t.Errorf("execCalls = %d on invalid auth_method; want 0", f.execCalls)
	}
}

// TestInsert_DefaultsCreatedViaToUser — ADR-058(d) guard (кейс 2): Insert без
// явного created_via (как из Service.Create / POST /v1/operators) подставляет
// 'user'. Это легализует существующий путь без правок Service.Create.
func TestInsert_DefaultsCreatedViaToUser(t *testing.T) {
	f := &fakeDB{}
	parent := "archon-alice"
	op := &Operator{
		AID:          "archon-bob",
		DisplayName:  "Bob",
		AuthMethod:   AuthMethodJWT,
		CreatedByAID: &parent,
		// CreatedVia не задан → default 'user'.
	}
	if err := Insert(context.Background(), f, op); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if f.lastExecArgs[5] != CreatedViaUser {
		t.Errorf("args[5] created_via = %v, want %q", f.lastExecArgs[5], CreatedViaUser)
	}
}

// TestInsert_PassesExplicitCreatedVia — ADR-058(d) guard: явно заданный
// created_via (ldap/system) пробрасывается в Exec как есть.
func TestInsert_PassesExplicitCreatedVia(t *testing.T) {
	for _, via := range []CreatedVia{CreatedViaLDAP, CreatedViaOIDC, CreatedViaSystem, CreatedViaBootstrap} {
		f := &fakeDB{}
		op := &Operator{AID: "archon-x", DisplayName: "X", AuthMethod: AuthMethodLDAP, CreatedVia: via}
		if err := Insert(context.Background(), f, op); err != nil {
			t.Fatalf("Insert(%q): %v", via, err)
		}
		if f.lastExecArgs[5] != via {
			t.Errorf("created_via = %v, want %q", f.lastExecArgs[5], via)
		}
	}
}

// TestInsert_RejectsInvalidCreatedVia — ADR-058(d) guard: значение вне домена
// отвергается прикладной валидацией до round-trip-а (parity с auth_method).
func TestInsert_RejectsInvalidCreatedVia(t *testing.T) {
	f := &fakeDB{}
	op := &Operator{AID: "archon-alice", DisplayName: "x", AuthMethod: AuthMethodJWT, CreatedVia: "wormhole"}
	err := Insert(context.Background(), f, op)
	if err == nil {
		t.Fatal("Insert with invalid created_via returned nil err")
	}
	if !strings.Contains(err.Error(), "created_via") {
		t.Errorf("err = %v, want substring \"created_via\"", err)
	}
	if f.execCalls != 0 {
		t.Errorf("execCalls = %d on invalid created_via; want 0", f.execCalls)
	}
}

func TestInsert_RejectsEmptyDisplayName(t *testing.T) {
	f := &fakeDB{}
	op := &Operator{AID: "archon-alice", DisplayName: "", AuthMethod: AuthMethodJWT}
	err := Insert(context.Background(), f, op)
	if err == nil {
		t.Fatal("Insert with empty display_name returned nil err")
	}
}

func TestInsert_RejectsNil(t *testing.T) {
	f := &fakeDB{}
	if err := Insert(context.Background(), f, nil); err == nil {
		t.Fatal("Insert(nil) returned nil err")
	}
}

func TestInsert_MapsUniqueViolationToErrOperatorAlreadyExists(t *testing.T) {
	f := &fakeDB{
		execErr: &pgconn.PgError{
			Code:           pgErrCodeUniqueViolation,
			ConstraintName: "operators_pkey",
		},
	}
	op := &Operator{AID: "archon-alice", DisplayName: "x", AuthMethod: AuthMethodJWT}
	err := Insert(context.Background(), f, op)
	if !errors.Is(err, ErrOperatorAlreadyExists) {
		t.Fatalf("err = %v, want errors.Is ErrOperatorAlreadyExists", err)
	}
	if !strings.Contains(err.Error(), "operators_pkey") {
		t.Errorf("err = %v; expected constraint name in message", err)
	}
}

func TestInsert_MapsPartialUniqueViolationToErrOperatorAlreadyExists(t *testing.T) {
	// Повторный bootstrap-insert: AID отличается, но партиальный unique
	// index `operators_first_archon_idx` ловит второго с
	// `created_by_aid IS NULL`. Тот же sentinel.
	f := &fakeDB{
		execErr: &pgconn.PgError{
			Code:           pgErrCodeUniqueViolation,
			ConstraintName: "operators_first_archon_idx",
		},
	}
	op := &Operator{AID: "archon-charlie", DisplayName: "x", AuthMethod: AuthMethodJWT}
	if err := Insert(context.Background(), f, op); !errors.Is(err, ErrOperatorAlreadyExists) {
		t.Fatalf("err = %v, want errors.Is ErrOperatorAlreadyExists", err)
	}
}

func TestInsert_MapsFKViolation(t *testing.T) {
	f := &fakeDB{
		execErr: &pgconn.PgError{
			Code:           pgErrCodeForeignKeyViolation,
			ConstraintName: "created_by_aid_fk",
		},
	}
	parent := "archon-ghost"
	op := &Operator{
		AID:          "archon-bob",
		DisplayName:  "Bob",
		AuthMethod:   AuthMethodJWT,
		CreatedByAID: &parent,
	}
	err := Insert(context.Background(), f, op)
	if err == nil {
		t.Fatal("Insert with FK-violation returned nil err")
	}
	if errors.Is(err, ErrOperatorAlreadyExists) {
		t.Errorf("FK-violation should NOT map to ErrOperatorAlreadyExists; err = %v", err)
	}
	if !strings.Contains(err.Error(), "FK violation") {
		t.Errorf("err = %v; expected substring \"FK violation\"", err)
	}
}

func TestSelectByAID_HappyPath(t *testing.T) {
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	f := &fakeDB{
		rowFunc: func() pgx.Row {
			return staticRow{values: []any{
				"archon-bob", "Bob", "jwt", now, any("archon-alice"), "user", any(nil), []byte(`{"team":"ops"}`),
			}}
		},
	}

	op, err := SelectByAID(context.Background(), f, "archon-bob")
	if err != nil {
		t.Fatalf("SelectByAID: %v", err)
	}
	if op.AID != "archon-bob" {
		t.Errorf("AID = %q", op.AID)
	}
	if op.DisplayName != "Bob" {
		t.Errorf("DisplayName = %q", op.DisplayName)
	}
	if op.AuthMethod != AuthMethodJWT {
		t.Errorf("AuthMethod = %q", op.AuthMethod)
	}
	if op.CreatedByAID == nil || *op.CreatedByAID != "archon-alice" {
		t.Errorf("CreatedByAID = %v", op.CreatedByAID)
	}
	if op.RevokedAt != nil {
		t.Errorf("RevokedAt = %v, want nil", op.RevokedAt)
	}
	if op.Metadata["team"] != "ops" {
		t.Errorf("Metadata.team = %v", op.Metadata["team"])
	}
}

func TestSelectByAID_NotFoundMapped(t *testing.T) {
	f := &fakeDB{} // default rowFunc → ErrNoRows
	_, err := SelectByAID(context.Background(), f, "archon-ghost")
	if !errors.Is(err, ErrOperatorNotFound) {
		t.Fatalf("err = %v, want errors.Is ErrOperatorNotFound", err)
	}
}

func TestCount_HappyPath(t *testing.T) {
	f := &fakeDB{
		rowFunc: func() pgx.Row {
			return staticRow{values: []any{int64(3)}}
		},
	}
	n, err := Count(context.Background(), f)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 3 {
		t.Errorf("Count = %d, want 3", n)
	}
}

func TestCount_PropagatesError(t *testing.T) {
	wantErr := errors.New("pg down")
	f := &fakeDB{
		rowFunc: func() pgx.Row { return errRow{err: wantErr} },
	}
	_, err := Count(context.Background(), f)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want wrap of %v", err, wantErr)
	}
}

func TestCountNonSystem_HappyPath(t *testing.T) {
	f := &fakeDB{
		rowFunc: func() pgx.Row {
			return staticRow{values: []any{int64(2)}}
		},
	}
	n, err := CountNonSystem(context.Background(), f)
	if err != nil {
		t.Fatalf("CountNonSystem: %v", err)
	}
	if n != 2 {
		t.Errorf("CountNonSystem = %d, want 2", n)
	}
	// Guard: SQL обязан фильтровать по created_via, иначе archon-system снова блокирует bootstrap.
	if !strings.Contains(f.lastQuerySQL, "created_via") {
		t.Errorf("SQL = %q, want фильтр по created_via", f.lastQuerySQL)
	}
}

func TestCountNonSystem_PropagatesError(t *testing.T) {
	wantErr := errors.New("pg down")
	f := &fakeDB{
		rowFunc: func() pgx.Row { return errRow{err: wantErr} },
	}
	_, err := CountNonSystem(context.Background(), f)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want wrap of %v", err, wantErr)
	}
}

func TestRevoke_HappyPath(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 1")}
	if err := Revoke(context.Background(), f, "archon-bob", "left team"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if f.execCalls != 1 {
		t.Errorf("execCalls = %d, want 1", f.execCalls)
	}
	if !strings.Contains(f.lastExecSQL, "UPDATE operators") {
		t.Errorf("SQL: %q", f.lastExecSQL)
	}
	if f.lastExecArgs[0] != "archon-bob" {
		t.Errorf("args[0] = %v", f.lastExecArgs[0])
	}
	if f.lastExecArgs[1] != "left team" {
		t.Errorf("args[1] = %v", f.lastExecArgs[1])
	}
}

func TestRevoke_InvalidAID(t *testing.T) {
	f := &fakeDB{}
	err := Revoke(context.Background(), f, ".bob", "")
	if err == nil {
		t.Fatal("Revoke with invalid AID returned nil")
	}
	if f.execCalls != 0 {
		t.Errorf("execCalls = %d, want 0 on invalid AID", f.execCalls)
	}
}

func TestRevoke_NotFound(t *testing.T) {
	f := &fakeDB{
		execTag: pgconn.NewCommandTag("UPDATE 0"),
		// SelectByAID после Update → возвращаем ErrNoRows.
	}
	err := Revoke(context.Background(), f, "archon-ghost", "")
	if !errors.Is(err, ErrOperatorNotFound) {
		t.Fatalf("err = %v, want ErrOperatorNotFound", err)
	}
}

func TestRevoke_AlreadyRevoked(t *testing.T) {
	now := time.Now()
	f := &fakeDB{
		execTag: pgconn.NewCommandTag("UPDATE 0"),
		rowFunc: func() pgx.Row {
			// SelectByAID видит уже-revoked-оператора.
			return staticRow{values: []any{
				"archon-bob", "Bob", "jwt", now, any(nil), "user", any(now), []byte("{}"),
			}}
		},
	}
	err := Revoke(context.Background(), f, "archon-bob", "")
	if !errors.Is(err, ErrOperatorAlreadyRevoked) {
		t.Fatalf("err = %v, want ErrOperatorAlreadyRevoked", err)
	}
}

// scanOperator-unit покрыт через SelectByAID; отдельный тест выделен только
// для assign-helper-а, гарантирующего корректный mapping типов.
func TestAssign_NilPointerColumns(t *testing.T) {
	var sp *string
	assign(&sp, any(nil))
	if sp != nil {
		t.Errorf("**string from nil src = %v, want nil", sp)
	}
	var tp *time.Time
	assign(&tp, any(nil))
	if tp != nil {
		t.Errorf("**time.Time from nil src = %v, want nil", tp)
	}
}
