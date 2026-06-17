package pushprovider

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

// fakeDB — мок ExecQueryRower для unit-тестов. Захватывает аргументы
// последнего Exec/QueryRow.
type fakeDB struct {
	execCalls    int
	lastExecSQL  string
	lastExecArgs []any
	execTag      pgconn.CommandTag
	execErr      error

	queryRowCalls int
	lastQuerySQL  string
	rowFunc       func() pgx.Row

	queryFunc func() (pgx.Rows, error)
}

func (f *fakeDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.execCalls++
	f.lastExecSQL = sql
	f.lastExecArgs = args
	return f.execTag, f.execErr
}

func (f *fakeDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.queryRowCalls++
	f.lastQuerySQL = sql
	f.lastExecArgs = args
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

type errRow struct{ err error }

func (r errRow) Scan(_ ...any) error { return r.err }

// staticRow — pgx.Row, копирующий values в Scan-аргументы. Поддерживает
// только типы, реально используемые в pushprovider.scanPushProvider
// (string, time.Time, *string, []byte).
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
	switch d := dest.(type) {
	case *string:
		*d = src.(string)
	case *time.Time:
		*d = src.(time.Time)
	case **string:
		switch v := src.(type) {
		case nil:
			*d = nil
		case *string:
			*d = v
		case string:
			s := v
			*d = &s
		default:
			panic("staticRow.assign: unsupported **string src")
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

// fakeRows — pgx.Rows-stub.
type fakeRows struct {
	rows [][]any
	idx  int
	err  error
}

func (r *fakeRows) Next() bool {
	if r.err != nil || r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}

func (r *fakeRows) Scan(dest ...any) error {
	row := r.rows[r.idx-1]
	for i, d := range dest {
		assign(d, row[i])
	}
	return nil
}

func (r *fakeRows) Err() error                                   { return r.err }
func (r *fakeRows) Close()                                       {}
func (r *fakeRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeRows) Values() ([]any, error)                       { return nil, nil }
func (r *fakeRows) RawValues() [][]byte                          { return nil }
func (r *fakeRows) Conn() *pgx.Conn                              { return nil }

func TestValidName(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"vault-bastion", true},
		{"vault", true},
		{"v", true},
		{"v1", true},
		{"a-b-c-1-2-3", true},
		{"", false},
		{"-vault", false}, // starts with dash
		{"1vault", false}, // starts with digit (env-var-name unsafe)
		{"Vault", false},  // uppercase
		{"vault.bastion", false},
		{"vault_bastion", false},
		{strings.Repeat("a", 64), false}, // > 63
	}
	for _, tc := range cases {
		if got := ValidName(tc.name); got != tc.ok {
			t.Errorf("ValidName(%q) = %v, want %v", tc.name, got, tc.ok)
		}
	}
}

func TestInsert_HappyPath(t *testing.T) {
	now := time.Now().UTC()
	f := &fakeDB{
		rowFunc: func() pgx.Row {
			return staticRow{values: []any{now, now}}
		},
	}
	p := &PushProvider{
		Name:         "vault-bastion",
		Params:       map[string]any{"vault_addr": "https://vault.example.com"},
		CreatedByAID: "archon-alice",
	}
	if err := Insert(context.Background(), f, p); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if !strings.Contains(f.lastQuerySQL, "INSERT INTO push_providers") {
		t.Errorf("SQL: %q", f.lastQuerySQL)
	}
	if len(f.lastExecArgs) != 3 {
		t.Fatalf("args len = %d, want 3", len(f.lastExecArgs))
	}
	if f.lastExecArgs[0] != "vault-bastion" {
		t.Errorf("args[0] name = %v", f.lastExecArgs[0])
	}
	paramsJSON, ok := f.lastExecArgs[1].([]byte)
	if !ok {
		t.Fatalf("args[1] params = %v (%T), want []byte", f.lastExecArgs[1], f.lastExecArgs[1])
	}
	var got map[string]any
	if err := json.Unmarshal(paramsJSON, &got); err != nil {
		t.Fatalf("params jsonb unmarshal: %v", err)
	}
	if got["vault_addr"] != "https://vault.example.com" {
		t.Errorf("params: %v", got)
	}
	if f.lastExecArgs[2] != "archon-alice" {
		t.Errorf("args[2] created_by_aid = %v", f.lastExecArgs[2])
	}
	if !p.CreatedAt.Equal(now) {
		t.Errorf("CreatedAt: %v", p.CreatedAt)
	}
}

func TestInsert_NilParamsBecomeEmptyJSON(t *testing.T) {
	f := &fakeDB{
		rowFunc: func() pgx.Row { return staticRow{values: []any{time.Now(), time.Now()}} },
	}
	p := &PushProvider{Name: "v", Params: nil, CreatedByAID: "archon-alice"}
	if err := Insert(context.Background(), f, p); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	paramsJSON := f.lastExecArgs[1].([]byte)
	if string(paramsJSON) != "{}" {
		t.Errorf("params = %q, want \"{}\"", paramsJSON)
	}
}

func TestInsert_RejectsInvalidName(t *testing.T) {
	f := &fakeDB{}
	p := &PushProvider{Name: "1bad", CreatedByAID: "archon-alice"}
	if err := Insert(context.Background(), f, p); err == nil {
		t.Fatal("Insert(invalid name): no error")
	}
	if f.queryRowCalls != 0 {
		t.Errorf("unexpected DB call for invalid name")
	}
}

func TestInsert_RejectsEmptyCreatedByAID(t *testing.T) {
	f := &fakeDB{}
	p := &PushProvider{Name: "vault", CreatedByAID: ""}
	if err := Insert(context.Background(), f, p); err == nil {
		t.Fatal("Insert(empty AID): no error")
	}
}

func TestInsert_MapsUniqueViolation(t *testing.T) {
	f := &fakeDB{
		rowFunc: func() pgx.Row {
			return errRow{err: &pgconn.PgError{Code: pgErrCodeUniqueViolation, ConstraintName: "push_providers_pkey"}}
		},
	}
	p := &PushProvider{Name: "vault", CreatedByAID: "archon-alice"}
	err := Insert(context.Background(), f, p)
	if !errors.Is(err, ErrPushProviderAlreadyExists) {
		t.Errorf("Insert: got %v, want wrap of ErrPushProviderAlreadyExists", err)
	}
}

func TestSelectByName_HappyPath(t *testing.T) {
	now := time.Now().UTC()
	paramsJSON := []byte(`{"vault_addr":"https://vault.example.com"}`)
	updatedBy := "archon-bob"
	f := &fakeDB{
		rowFunc: func() pgx.Row {
			return staticRow{values: []any{
				"vault-bastion",
				paramsJSON,
				now,
				now,
				"archon-alice",
				&updatedBy,
			}}
		},
	}
	p, err := SelectByName(context.Background(), f, "vault-bastion")
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if p.Name != "vault-bastion" {
		t.Errorf("Name: %q", p.Name)
	}
	if p.Params["vault_addr"] != "https://vault.example.com" {
		t.Errorf("Params: %v", p.Params)
	}
	if p.UpdatedByAID == nil || *p.UpdatedByAID != "archon-bob" {
		t.Errorf("UpdatedByAID: %v", p.UpdatedByAID)
	}
}

func TestSelectByName_NotFound(t *testing.T) {
	f := &fakeDB{}
	_, err := SelectByName(context.Background(), f, "missing")
	if !errors.Is(err, ErrPushProviderNotFound) {
		t.Errorf("err = %v, want ErrPushProviderNotFound", err)
	}
}

func TestUpdate_HappyPath(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 1")}
	err := Update(context.Background(), f, "vault", map[string]any{"role": "keeper"}, "archon-bob")
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !strings.Contains(f.lastExecSQL, "UPDATE push_providers") {
		t.Errorf("SQL: %q", f.lastExecSQL)
	}
	if f.lastExecArgs[0] != "vault" {
		t.Errorf("args[0]: %v", f.lastExecArgs[0])
	}
	if f.lastExecArgs[2] != "archon-bob" {
		t.Errorf("args[2] updated_by_aid: %v", f.lastExecArgs[2])
	}
}

func TestUpdate_NotFound(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 0")}
	err := Update(context.Background(), f, "missing", nil, "archon-bob")
	if !errors.Is(err, ErrPushProviderNotFound) {
		t.Errorf("err = %v, want ErrPushProviderNotFound", err)
	}
}

func TestDelete_HappyPath(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("DELETE 1")}
	if err := Delete(context.Background(), f, "vault"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !strings.Contains(f.lastExecSQL, "DELETE FROM push_providers") {
		t.Errorf("SQL: %q", f.lastExecSQL)
	}
}

func TestDelete_NotFound(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("DELETE 0")}
	err := Delete(context.Background(), f, "missing")
	if !errors.Is(err, ErrPushProviderNotFound) {
		t.Errorf("err = %v, want ErrPushProviderNotFound", err)
	}
}

func TestSelectAll_HappyPath(t *testing.T) {
	now := time.Now().UTC()
	paramsJSON := []byte(`{"vault_addr":"https://vault.example.com"}`)
	countCalled := false
	listCalled := false
	f := &fakeDB{
		rowFunc: func() pgx.Row {
			countCalled = true
			return staticRow{values: []any{int64(0)}}
		},
		queryFunc: func() (pgx.Rows, error) {
			listCalled = true
			return &fakeRows{rows: [][]any{
				{"vault-bastion", paramsJSON, now, now, "archon-alice", (*string)(nil)},
				{"static", []byte("{}"), now, now, "archon-alice", (*string)(nil)},
			}}, nil
		},
	}
	// Adjust scanner: count returns int64 in staticRow, but SelectAll reads into &total (int).
	// Use direct int64 → assign helper handles *int via panic. Need extra case.
	// Instead, use *int64 inside staticRow but match scanner: SELECT COUNT(*) Scan(&total int).
	// fakeRows count via rowFunc with int conversion. Provide custom row.
	f.rowFunc = func() pgx.Row {
		countCalled = true
		return countRow{n: 2}
	}
	items, total, err := SelectAll(context.Background(), f, ListFilter{}, 0, 10)
	if err != nil {
		t.Fatalf("SelectAll: %v", err)
	}
	if !countCalled || !listCalled {
		t.Errorf("count=%v list=%v", countCalled, listCalled)
	}
	if total != 2 {
		t.Errorf("total = %d", total)
	}
	if len(items) != 2 {
		t.Errorf("items len = %d", len(items))
	}
	if items[0].Name != "vault-bastion" {
		t.Errorf("items[0].Name = %q", items[0].Name)
	}
}

func TestSelectAll_WithNamePatternFilter(t *testing.T) {
	f := &fakeDB{
		rowFunc:   func() pgx.Row { return countRow{n: 0} },
		queryFunc: func() (pgx.Rows, error) { return &fakeRows{}, nil },
	}
	_, _, err := SelectAll(context.Background(), f, ListFilter{NamePattern: "vault%"}, 0, 10)
	if err != nil {
		t.Fatalf("SelectAll: %v", err)
	}
	if !strings.Contains(f.lastQuerySQL, "WHERE name LIKE") {
		t.Errorf("SQL: %q (want WHERE name LIKE)", f.lastQuerySQL)
	}
}

func TestSelectAll_RejectsInvalidBounds(t *testing.T) {
	f := &fakeDB{}
	if _, _, err := SelectAll(context.Background(), f, ListFilter{}, -1, 10); err == nil {
		t.Error("offset=-1: no error")
	}
	if _, _, err := SelectAll(context.Background(), f, ListFilter{}, 0, 0); err == nil {
		t.Error("limit=0: no error")
	}
}

// countRow — pgx.Row-stub для COUNT(*)-запросов. Scan(&total int) кладёт n.
type countRow struct{ n int }

func (r countRow) Scan(dest ...any) error {
	if p, ok := dest[0].(*int); ok {
		*p = r.n
		return nil
	}
	if p, ok := dest[0].(*int64); ok {
		*p = int64(r.n)
		return nil
	}
	return errors.New("countRow: unsupported dest")
}
