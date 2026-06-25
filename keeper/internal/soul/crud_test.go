package soul

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Тестовые fake-ы повторяют паттерн из keeper/internal/operator/crud_test.go.
// Не объединяем в общий test-only helper-пакет: API CRUD у каждого реестра
// своё, single-pattern мутирует случайно при общем helper-е.

type fakeDB struct {
	execCalls    int
	lastExecSQL  string
	lastExecArgs []any
	execTag      pgconn.CommandTag
	execErr      error

	queryCalls   int
	lastQuerySQL string
	rowFunc      func() pgx.Row
	queryFunc    func() (pgx.Rows, error)
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

// bulkFakePool оборачивает fakeDB в BulkPool. BeginTx не вызывается тестами,
// проверяющими отказ ДО БД (out-of-scope label / bad label / replace mode) —
// поэтому возвращает ошибку: реальный чанк-путь в этих тестах недостижим.
type bulkFakePool struct {
	*fakeDB
}

func (bulkFakePool) BeginTx(_ context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	return nil, errors.New("bulkFakePool: BeginTx not expected in this test")
}

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
	if len(dest) != len(r.values) {
		return errors.New("staticRow: len mismatch")
	}
	for i, d := range dest {
		assign(d, r.values[i])
	}
	return nil
}

func assign(dest, src any) {
	// Нормализуем typed-nil (например, (*time.Time)(nil) из теста) в untyped nil,
	// чтобы case-ы ниже не пытались type-assert на конкретный тип nil-значения.
	if v, ok := src.(*time.Time); ok && v == nil {
		src = nil
	}
	if v, ok := src.(*string); ok && v == nil {
		src = nil
	}
	switch d := dest.(type) {
	case *string:
		*d = src.(string)
	case *int:
		*d = src.(int)
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
	case *[]string:
		if src == nil {
			*d = nil
		} else {
			*d = src.([]string)
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

type fakeRows struct {
	rows []staticRow
	idx  int
	err  error
}

func (r *fakeRows) Next() bool {
	if r.err != nil {
		return false
	}
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}

func (r *fakeRows) Scan(dest ...any) error {
	return r.rows[r.idx-1].Scan(dest...)
}

func (r *fakeRows) Err() error                                   { return r.err }
func (r *fakeRows) Close()                                       {}
func (r *fakeRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeRows) Values() ([]any, error)                       { return nil, nil }
func (r *fakeRows) RawValues() [][]byte                          { return nil }
func (r *fakeRows) Conn() *pgx.Conn                              { return nil }

// --- ValidSID ---

func TestValidSID(t *testing.T) {
	good := []string{"host1.example.com", "host", "h.e.x", "a1.b2-c.example"}
	bad := []string{"", "-bad", ".bad", "HOST", "host_underscore", strings.Repeat("a", 255)}
	for _, s := range good {
		if !ValidSID(s) {
			t.Errorf("ValidSID(%q) = false; want true", s)
		}
	}
	for _, s := range bad {
		if ValidSID(s) {
			t.Errorf("ValidSID(%q) = true; want false", s)
		}
	}
}

func TestValidCoven(t *testing.T) {
	good := []string{"prod", "dc-eu", "redis-prod", "a1", "x-1-y"}
	bad := []string{"", "Prod", "db_main", "-edge", "edge-", "x y", "a--b", strings.Repeat("a", 64)}
	for _, s := range good {
		if !ValidCoven(s) {
			t.Errorf("ValidCoven(%q) = false; want true", s)
		}
	}
	for _, s := range bad {
		if ValidCoven(s) {
			t.Errorf("ValidCoven(%q) = true; want false", s)
		}
	}
}

// --- Insert ---

func TestInsert_HappyPath_Pending(t *testing.T) {
	now := time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC)
	f := &fakeDB{
		rowFunc: func() pgx.Row {
			// RETURNING registered_at, requested_at.
			return staticRow{values: []any{now, now}}
		},
	}
	s := &Soul{
		SID:       "host.example.com",
		Transport: TransportAgent,
		Status:    StatusPending,
	}
	if err := Insert(context.Background(), f, s); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if f.queryCalls != 1 {
		t.Fatalf("queryCalls = %d, want 1", f.queryCalls)
	}
	if !strings.Contains(f.lastQuerySQL, "INSERT INTO souls") {
		t.Errorf("SQL: %q", f.lastQuerySQL)
	}
	if s.RegisteredAt.IsZero() {
		t.Errorf("RegisteredAt not populated from RETURNING")
	}
	// requested_at должен заполниться из RETURNING (PG проставляет NOW(),
	// если caller не задал) — нужен Reaper-правилу pending→expired.
	if s.RequestedAt == nil || s.RequestedAt.IsZero() {
		t.Errorf("RequestedAt not populated from RETURNING: %v", s.RequestedAt)
	}
}

func TestInsert_DefaultsTransportAndStatus(t *testing.T) {
	now := time.Now()
	f := &fakeDB{
		rowFunc: func() pgx.Row {
			return staticRow{values: []any{now, now}}
		},
	}
	s := &Soul{SID: "host.example.com"}
	if err := Insert(context.Background(), f, s); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if s.Transport != TransportAgent {
		t.Errorf("Transport = %q, want %q", s.Transport, TransportAgent)
	}
	if s.Status != StatusPending {
		t.Errorf("Status = %q, want %q", s.Status, StatusPending)
	}
}

func TestInsert_RejectsInvalidSID(t *testing.T) {
	f := &fakeDB{}
	s := &Soul{SID: "HOST_BAD"}
	if err := Insert(context.Background(), f, s); err == nil {
		t.Fatal("Insert with invalid SID returned nil err")
	}
	if f.queryCalls != 0 {
		t.Errorf("queryCalls = %d, want 0 (validation before round-trip)", f.queryCalls)
	}
}

func TestInsert_RejectsInvalidStatus(t *testing.T) {
	f := &fakeDB{}
	s := &Soul{SID: "host.example.com", Status: Status("hax")}
	if err := Insert(context.Background(), f, s); err == nil {
		t.Fatal("Insert with invalid status returned nil err")
	}
}

func TestInsert_RejectsInvalidTransport(t *testing.T) {
	f := &fakeDB{}
	s := &Soul{SID: "host.example.com", Transport: Transport("ftp")}
	if err := Insert(context.Background(), f, s); err == nil {
		t.Fatal("Insert with invalid transport returned nil err")
	}
}

func TestInsert_RejectsNil(t *testing.T) {
	f := &fakeDB{}
	if err := Insert(context.Background(), f, nil); err == nil {
		t.Fatal("Insert(nil) returned nil err")
	}
}

func TestInsert_MapsUniqueViolationToErrAlreadyExists(t *testing.T) {
	f := &fakeDB{
		rowFunc: func() pgx.Row {
			return errRow{err: &pgconn.PgError{
				Code:           pgErrCodeUniqueViolation,
				ConstraintName: "souls_pkey",
			}}
		},
	}
	s := &Soul{SID: "host.example.com"}
	err := Insert(context.Background(), f, s)
	if !errors.Is(err, ErrSoulAlreadyExists) {
		t.Fatalf("err = %v, want errors.Is ErrSoulAlreadyExists", err)
	}
}

func TestInsert_MapsCreatorFKToErrSoulCreatorNotFound(t *testing.T) {
	f := &fakeDB{
		rowFunc: func() pgx.Row {
			return errRow{err: &pgconn.PgError{
				Code:           pgErrCodeForeignKeyViolation,
				ConstraintName: "souls_created_by_aid_fk",
			}}
		},
	}
	parent := "archon-ghost"
	s := &Soul{SID: "host.example.com", CreatedByAID: &parent}
	err := Insert(context.Background(), f, s)
	if !errors.Is(err, ErrSoulCreatorNotFound) {
		t.Errorf("err = %v, want errors.Is ErrSoulCreatorNotFound", err)
	}
}

func TestInsert_MapsOtherFKToGenericError(t *testing.T) {
	f := &fakeDB{
		rowFunc: func() pgx.Row {
			return errRow{err: &pgconn.PgError{
				Code:           pgErrCodeForeignKeyViolation,
				ConstraintName: "some_other_fk",
			}}
		},
	}
	s := &Soul{SID: "host.example.com"}
	err := Insert(context.Background(), f, s)
	if errors.Is(err, ErrSoulCreatorNotFound) {
		t.Errorf("non-creator FK should NOT map to ErrSoulCreatorNotFound: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "some_other_fk") {
		t.Errorf("err = %v; want generic FK violation message", err)
	}
}

// --- SelectBySID ---

func TestSelectBySID_NotFound(t *testing.T) {
	f := &fakeDB{rowFunc: func() pgx.Row { return errRow{err: pgx.ErrNoRows} }}
	if _, err := SelectBySID(context.Background(), f, "missing"); !errors.Is(err, ErrSoulNotFound) {
		t.Errorf("err = %v, want ErrSoulNotFound", err)
	}
}

func TestSelectBySID_HappyPath(t *testing.T) {
	now := time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC)
	f := &fakeDB{
		rowFunc: func() pgx.Row {
			return staticRow{values: []any{
				"host.example.com",     // sid
				"agent",                // transport
				"connected",            // status
				[]string{"prod", "db"}, // coven
				[]byte(nil),            // traits (jsonb '{}' / NULL → пустой map)
				now,                    // registered_at
				(*time.Time)(nil),      // last_seen_at — передаём как nil-указатель
				(*string)(nil),         // last_seen_by_kid
				(*string)(nil),         // created_by_aid
				(*time.Time)(nil),      // requested_at
				(*string)(nil),         // note
			}}
		},
	}
	// Дальше нам нужен Scan, который умеет переключать на nil; адаптируем
	// staticRow.assign: ниже отдельный helper rows-builder, но пока тест
	// читает только заполненные поля.
	got, err := SelectBySID(context.Background(), f, "host.example.com")
	if err != nil {
		t.Fatalf("SelectBySID: %v", err)
	}
	if got.SID != "host.example.com" {
		t.Errorf("SID = %q", got.SID)
	}
	if got.Transport != TransportAgent {
		t.Errorf("Transport = %q", got.Transport)
	}
	if got.Status != StatusConnected {
		t.Errorf("Status = %q", got.Status)
	}
	if len(got.Coven) != 2 || got.Coven[0] != "prod" {
		t.Errorf("Coven = %v", got.Coven)
	}
}

// --- UpdateStatus ---

func TestUpdateStatus_HappyPath(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 1")}
	kid := "kid-1"
	if err := UpdateStatus(context.Background(), f, "host.example.com", StatusConnected, &kid); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	if f.execCalls != 1 {
		t.Errorf("execCalls = %d, want 1", f.execCalls)
	}
}

func TestUpdateStatus_RejectsInvalidStatus(t *testing.T) {
	f := &fakeDB{}
	if err := UpdateStatus(context.Background(), f, "host.example.com", Status("hax"), nil); err == nil {
		t.Fatal("UpdateStatus with invalid status returned nil err")
	}
	if f.execCalls != 0 {
		t.Errorf("execCalls = %d; want 0 (validation before round-trip)", f.execCalls)
	}
}

func TestUpdateStatus_RowsAffectedZero_NotFound(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 0")}
	err := UpdateStatus(context.Background(), f, "host.example.com", StatusConnected, nil)
	if !errors.Is(err, ErrSoulNotFound) {
		t.Errorf("err = %v, want ErrSoulNotFound", err)
	}
}

func TestUpdateStatus_NilKidKeepsExisting(t *testing.T) {
	// Защита от регрессии: nil-kid должен передаваться как nil-argument
	// (PG COALESCE сохранит старое значение), а не как пустая строка
	// (которая перетёрла бы значение).
	f := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 1")}
	if err := UpdateStatus(context.Background(), f, "host.example.com", StatusDisconnected, nil); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	if len(f.lastExecArgs) != 3 {
		t.Fatalf("args len = %d, want 3", len(f.lastExecArgs))
	}
	if f.lastExecArgs[2] != nil {
		t.Errorf("args[2] kid = %v, want nil", f.lastExecArgs[2])
	}
}

// --- UpdateLastSeen ---

func TestUpdateLastSeen_HappyPath(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 1")}
	if err := UpdateLastSeen(context.Background(), f, "host.example.com", "kid-1", time.Now()); err != nil {
		t.Fatalf("UpdateLastSeen: %v", err)
	}
	if f.execCalls != 1 {
		t.Errorf("execCalls = %d, want 1", f.execCalls)
	}
}

func TestUpdateLastSeen_RejectsInvalidSID(t *testing.T) {
	f := &fakeDB{}
	if err := UpdateLastSeen(context.Background(), f, "BAD_SID", "kid-1", time.Now()); err == nil {
		t.Error("invalid SID returned nil err")
	}
	if f.execCalls != 0 {
		t.Errorf("execCalls = %d; want 0", f.execCalls)
	}
}

func TestUpdateLastSeen_RejectsEmptyKID(t *testing.T) {
	f := &fakeDB{}
	if err := UpdateLastSeen(context.Background(), f, "host.example.com", "", time.Now()); err == nil {
		t.Error("empty kid returned nil err")
	}
}

func TestUpdateLastSeen_NotFound(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 0")}
	err := UpdateLastSeen(context.Background(), f, "host.example.com", "kid-1", time.Now())
	if !errors.Is(err, ErrSoulNotFound) {
		t.Errorf("err = %v, want ErrSoulNotFound", err)
	}
}

// --- UpdateSoulprint ---

func TestUpdateSoulprint_HappyPath(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 1")}
	now := time.Now()
	if err := UpdateSoulprint(context.Background(), f, "host.example.com",
		[]byte(`{"os":{"family":"debian"}}`), now, now); err != nil {
		t.Fatalf("UpdateSoulprint: %v", err)
	}
	if f.execCalls != 1 {
		t.Errorf("execCalls = %d, want 1", f.execCalls)
	}
	if len(f.lastExecArgs) != 4 {
		t.Fatalf("args len = %d, want 4", len(f.lastExecArgs))
	}
	if _, ok := f.lastExecArgs[1].([]byte); !ok {
		t.Errorf("args[1] facts type = %T, want []byte", f.lastExecArgs[1])
	}
}

func TestUpdateSoulprint_EmptyFactsPassedAsNil(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 1")}
	if err := UpdateSoulprint(context.Background(), f, "host.example.com",
		nil, time.Time{}, time.Time{}); err != nil {
		t.Fatalf("UpdateSoulprint: %v", err)
	}
	if f.lastExecArgs[1] != nil {
		t.Errorf("args[1] = %v, want nil for empty facts", f.lastExecArgs[1])
	}
	if f.lastExecArgs[2] != nil {
		t.Errorf("args[2] = %v, want nil for zero collected_at", f.lastExecArgs[2])
	}
}

func TestUpdateSoulprint_NotFound(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 0")}
	err := UpdateSoulprint(context.Background(), f, "host.example.com", []byte("{}"), time.Now(), time.Now())
	if !errors.Is(err, ErrSoulNotFound) {
		t.Errorf("err = %v, want ErrSoulNotFound", err)
	}
}

func TestUpdateSoulprint_RejectsInvalidSID(t *testing.T) {
	f := &fakeDB{}
	if err := UpdateSoulprint(context.Background(), f, "BAD_SID", nil, time.Now(), time.Now()); err == nil {
		t.Error("invalid SID returned nil err")
	}
	if f.execCalls != 0 {
		t.Errorf("execCalls = %d, want 0", f.execCalls)
	}
}

// --- SelectAll ---

func TestSelectAll_RejectsNegativeOffset(t *testing.T) {
	f := &fakeDB{}
	if _, _, err := SelectAll(context.Background(), f, ListFilter{}, ListScope{Unrestricted: true}, -1, 10); err == nil {
		t.Fatal("SelectAll with offset=-1 returned nil err")
	}
}

func TestSelectAll_RejectsZeroLimit(t *testing.T) {
	f := &fakeDB{}
	if _, _, err := SelectAll(context.Background(), f, ListFilter{}, ListScope{Unrestricted: true}, 0, 0); err == nil {
		t.Fatal("SelectAll with limit=0 returned nil err")
	}
}

// --- SelectSoulprint ---

func TestSelectSoulprint_HappyPath(t *testing.T) {
	collected := time.Date(2026, 5, 26, 10, 0, 0, 0, time.UTC)
	received := collected.Add(2 * time.Second)
	facts := []byte(`{"sid":"soul.example.com","os":{"family":"debian"}}`)
	f := &fakeDB{
		rowFunc: func() pgx.Row {
			// **time.Time dest: assign() ожидает либо nil, либо time.Time (не *time.Time).
			return staticRow{values: []any{"soul.example.com", facts, collected, received}}
		},
	}
	rec, err := SelectSoulprint(context.Background(), f, "soul.example.com")
	if err != nil {
		t.Fatalf("SelectSoulprint: %v", err)
	}
	if rec.SID != "soul.example.com" {
		t.Errorf("SID = %q", rec.SID)
	}
	if string(rec.FactsJSON) != string(facts) {
		t.Errorf("FactsJSON = %s", rec.FactsJSON)
	}
	if !rec.CollectedAt.Equal(collected) || !rec.ReceivedAt.Equal(received) {
		t.Errorf("timestamps = %v / %v", rec.CollectedAt, rec.ReceivedAt)
	}
}

func TestSelectSoulprint_NotFound(t *testing.T) {
	f := &fakeDB{rowFunc: func() pgx.Row { return errRow{err: pgx.ErrNoRows} }}
	_, err := SelectSoulprint(context.Background(), f, "ghost.example.com")
	if !errors.Is(err, ErrSoulNotFound) {
		t.Errorf("err = %v, want ErrSoulNotFound", err)
	}
}

func TestSelectSoulprint_NotReceived(t *testing.T) {
	f := &fakeDB{
		rowFunc: func() pgx.Row {
			// soulprint_facts IS NULL, collected/received NULL — запись Soul
			// есть, фактов нет.
			return staticRow{values: []any{"soul.example.com", []byte(nil), (*time.Time)(nil), (*time.Time)(nil)}}
		},
	}
	_, err := SelectSoulprint(context.Background(), f, "soul.example.com")
	if !errors.Is(err, ErrSoulprintNotReceived) {
		t.Errorf("err = %v, want ErrSoulprintNotReceived", err)
	}
}

func TestSelectSoulprint_RejectsInvalidSID(t *testing.T) {
	f := &fakeDB{}
	if _, err := SelectSoulprint(context.Background(), f, "BAD-UPPER"); err == nil {
		t.Fatal("expected error on invalid SID")
	}
}
