package profile

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

// fakeDB — ExecQueryRower-stub для unit-тестов. Паттерн совпадает с
// provider.fakeDB / incarnation.fakeDB.
type fakeDB struct {
	queryRowSQL   string
	queryRowArgs  []any
	queryRowFunc  func(sql string) pgx.Row
	queryRowCalls int

	querySQL   string
	queryArgs  []any
	queryFunc  func(sql string) (pgx.Rows, error)
	queryCalls int
}

func (f *fakeDB) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (f *fakeDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.queryRowCalls++
	f.queryRowSQL = sql
	f.queryRowArgs = args
	if f.queryRowFunc != nil {
		return f.queryRowFunc(sql)
	}
	return errRow{err: pgx.ErrNoRows}
}

func (f *fakeDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	f.queryCalls++
	f.querySQL = sql
	f.queryArgs = args
	if f.queryFunc != nil {
		return f.queryFunc(sql)
	}
	return &fakeRows{}, nil
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

func (r *fakeRows) Scan(dest ...any) error                       { return r.rows[r.idx-1].Scan(dest...) }
func (r *fakeRows) Err() error                                   { return r.err }
func (r *fakeRows) Close()                                       {}
func (r *fakeRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeRows) Values() ([]any, error)                       { return nil, nil }
func (r *fakeRows) RawValues() [][]byte                          { return nil }
func (r *fakeRows) Conn() *pgx.Conn                              { return nil }

// --- Insert -----------------------------------------------------------

func validProfile() *Profile {
	aid := "archon-alice"
	ci := "#cloud-config\npackages: [nginx]"
	return &Profile{
		Name:         "web-small",
		Provider:     "aws-eu",
		Params:       map[string]any{"instance_type": "t3.small"},
		CloudInit:    &ci,
		CreatedByAID: &aid,
	}
}

func TestInsert_HappyPath(t *testing.T) {
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row { return staticRow{values: []any{now}} },
	}
	p := validProfile()
	if err := Insert(context.Background(), f, p); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if !p.CreatedAt.Equal(now) {
		t.Errorf("RETURNING created_at not assigned: %v", p.CreatedAt)
	}
	if !strings.Contains(f.queryRowSQL, "INSERT INTO profiles") {
		t.Errorf("SQL: %q", f.queryRowSQL)
	}
	if len(f.queryRowArgs) != 5 {
		t.Fatalf("args len = %d, want 5", len(f.queryRowArgs))
	}
	if f.queryRowArgs[0] != "web-small" || f.queryRowArgs[1] != "aws-eu" {
		t.Errorf("args head = %v / %v", f.queryRowArgs[0], f.queryRowArgs[1])
	}
	// args[2] — marshalled params.
	b, ok := f.queryRowArgs[2].([]byte)
	if !ok {
		t.Fatalf("args[2] = %T, want []byte", f.queryRowArgs[2])
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("params not JSON: %v", err)
	}
	if got["instance_type"] != "t3.small" {
		t.Errorf("params.instance_type = %v", got["instance_type"])
	}
	if f.queryRowArgs[3] != "#cloud-config\npackages: [nginx]" {
		t.Errorf("args[3] cloud_init = %v", f.queryRowArgs[3])
	}
	if f.queryRowArgs[4] != "archon-alice" {
		t.Errorf("args[4] created_by_aid = %v", f.queryRowArgs[4])
	}
}

func TestInsert_NilCloudInitAndAID(t *testing.T) {
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row { return staticRow{values: []any{time.Now()}} },
	}
	p := validProfile()
	p.CloudInit = nil
	p.CreatedByAID = nil
	if err := Insert(context.Background(), f, p); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if f.queryRowArgs[3] != nil {
		t.Errorf("args[3] cloud_init = %v, want nil", f.queryRowArgs[3])
	}
	if f.queryRowArgs[4] != nil {
		t.Errorf("args[4] created_by_aid = %v, want nil", f.queryRowArgs[4])
	}
}

func TestInsert_NilParamsBecomesEmptyObject(t *testing.T) {
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row { return staticRow{values: []any{time.Now()}} },
	}
	p := validProfile()
	p.Params = nil
	if err := Insert(context.Background(), f, p); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if s, _ := f.queryRowArgs[2].([]byte); string(s) != "{}" {
		t.Errorf("params bytes = %s, want \"{}\"", s)
	}
}

func TestInsert_RejectsInvalidName(t *testing.T) {
	f := &fakeDB{}
	p := validProfile()
	p.Name = "Web_Small"
	err := Insert(context.Background(), f, p)
	if err == nil || !strings.Contains(err.Error(), "invalid name") {
		t.Fatalf("err = %v, want invalid name", err)
	}
	if f.queryRowCalls != 0 {
		t.Errorf("queryRowCalls = %d on invalid name; want 0", f.queryRowCalls)
	}
}

func TestInsert_RejectsInvalidProvider(t *testing.T) {
	f := &fakeDB{}
	p := validProfile()
	p.Provider = "AWS_EU"
	if err := Insert(context.Background(), f, p); err == nil ||
		!strings.Contains(err.Error(), "invalid provider") {
		t.Fatalf("err = %v, want invalid provider", err)
	}
}

func TestInsert_RejectsNil(t *testing.T) {
	f := &fakeDB{}
	if err := Insert(context.Background(), f, nil); err == nil {
		t.Fatal("Insert(nil) returned nil")
	}
}

func TestInsert_MapsUniqueViolation(t *testing.T) {
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return errRow{err: &pgconn.PgError{
				Code:           pgErrCodeUniqueViolation,
				ConstraintName: "profiles_pkey",
			}}
		},
	}
	err := Insert(context.Background(), f, validProfile())
	if !errors.Is(err, ErrProfileAlreadyExists) {
		t.Fatalf("err = %v, want errors.Is ErrProfileAlreadyExists", err)
	}
}

func TestInsert_MapsProviderFKToProviderNotFound(t *testing.T) {
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return errRow{err: &pgconn.PgError{
				Code:           pgErrCodeForeignKeyViolation,
				ConstraintName: providerFKConstraint,
			}}
		},
	}
	err := Insert(context.Background(), f, validProfile())
	if !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("err = %v, want errors.Is ErrProviderNotFound", err)
	}
	if errors.Is(err, ErrProfileAlreadyExists) {
		t.Errorf("provider-FK should NOT be ErrProfileAlreadyExists; err = %v", err)
	}
}

func TestInsert_MapsCreatedByFKToGeneric(t *testing.T) {
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return errRow{err: &pgconn.PgError{
				Code:           pgErrCodeForeignKeyViolation,
				ConstraintName: "profiles_created_by_aid_fk",
			}}
		},
	}
	err := Insert(context.Background(), f, validProfile())
	if err == nil {
		t.Fatal("Insert with created_by FK-violation returned nil")
	}
	if errors.Is(err, ErrProviderNotFound) {
		t.Errorf("created_by-FK should NOT map to ErrProviderNotFound; err = %v", err)
	}
	if !strings.Contains(err.Error(), "FK violation") {
		t.Errorf("err = %v; expected generic \"FK violation\"", err)
	}
}

// --- SelectByName -----------------------------------------------------

func TestSelectByName_HappyPath(t *testing.T) {
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return staticRow{values: []any{
				"web-small", "aws-eu",
				[]byte(`{"instance_type":"t3.small"}`),
				any("#cloud-config"),
				any("archon-alice"),
				now,
			}}
		},
	}
	p, err := SelectByName(context.Background(), f, "web-small")
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if p.Name != "web-small" || p.Provider != "aws-eu" {
		t.Errorf("got = %+v", p)
	}
	if p.Params["instance_type"] != "t3.small" {
		t.Errorf("Params.instance_type = %v", p.Params["instance_type"])
	}
	if p.CloudInit == nil || *p.CloudInit != "#cloud-config" {
		t.Errorf("CloudInit = %v", p.CloudInit)
	}
	if p.CreatedByAID == nil || *p.CreatedByAID != "archon-alice" {
		t.Errorf("CreatedByAID = %v", p.CreatedByAID)
	}
}

func TestSelectByName_NullCloudInit(t *testing.T) {
	now := time.Now()
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return staticRow{values: []any{
				"web-small", "aws-eu", []byte("{}"),
				any(nil), any(nil), now,
			}}
		},
	}
	p, err := SelectByName(context.Background(), f, "web-small")
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if p.CloudInit != nil {
		t.Errorf("CloudInit = %v, want nil", p.CloudInit)
	}
	if p.CreatedByAID != nil {
		t.Errorf("CreatedByAID = %v, want nil", p.CreatedByAID)
	}
}

func TestSelectByName_NotFound(t *testing.T) {
	f := &fakeDB{} // default → ErrNoRows
	_, err := SelectByName(context.Background(), f, "missing")
	if !errors.Is(err, ErrProfileNotFound) {
		t.Fatalf("err = %v, want ErrProfileNotFound", err)
	}
}

// --- SelectAll --------------------------------------------------------

func TestSelectAll_HappyPath(t *testing.T) {
	now := time.Now()
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row { return staticRow{values: []any{int(2)}} },
		queryFunc: func(_ string) (pgx.Rows, error) {
			return &fakeRows{rows: []staticRow{
				{values: []any{"web-small", "aws-eu", []byte("{}"), any(nil), any(nil), now}},
				{values: []any{"db-large", "aws-eu", []byte("{}"), any(nil), any(nil), now}},
			}}, nil
		},
	}
	out, total, err := SelectAll(context.Background(), f, 0, 50)
	if err != nil {
		t.Fatalf("SelectAll: %v", err)
	}
	if total != 2 || len(out) != 2 {
		t.Fatalf("total = %d, len = %d, want 2/2", total, len(out))
	}
	if !strings.Contains(f.querySQL, "ORDER BY created_at DESC") {
		t.Errorf("ORDER missing in: %q", f.querySQL)
	}
	if strings.Contains(f.querySQL, "WHERE provider") {
		t.Errorf("SelectAll must not filter by provider; SQL=%q", f.querySQL)
	}
	if len(f.queryArgs) != 2 || f.queryArgs[0] != 0 || f.queryArgs[1] != 50 {
		t.Errorf("args = %v", f.queryArgs)
	}
}

func TestSelectByProvider_FiltersAndArgs(t *testing.T) {
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row { return staticRow{values: []any{int(0)}} },
		queryFunc:    func(_ string) (pgx.Rows, error) { return &fakeRows{}, nil },
	}
	_, _, err := SelectByProvider(context.Background(), f, "aws-eu", 5, 10)
	if err != nil {
		t.Fatalf("SelectByProvider: %v", err)
	}
	if !strings.Contains(f.querySQL, "WHERE provider = $1") {
		t.Errorf("filter SQL: %q", f.querySQL)
	}
	if !strings.Contains(f.queryRowSQL, "WHERE provider = $1") {
		t.Errorf("count SQL must also filter: %q", f.queryRowSQL)
	}
	// Args: [provider, offset, limit].
	if len(f.queryArgs) != 3 {
		t.Fatalf("args len = %d, want 3", len(f.queryArgs))
	}
	if f.queryArgs[0] != "aws-eu" || f.queryArgs[1] != 5 || f.queryArgs[2] != 10 {
		t.Errorf("args = %v", f.queryArgs)
	}
}

func TestSelectAll_RejectsNegativeOffset(t *testing.T) {
	f := &fakeDB{}
	if _, _, err := SelectAll(context.Background(), f, -1, 50); err == nil {
		t.Fatal("expected error on negative offset")
	}
}

func TestSelectAll_RejectsZeroLimit(t *testing.T) {
	f := &fakeDB{}
	if _, _, err := SelectAll(context.Background(), f, 0, 0); err == nil {
		t.Fatal("expected error on zero limit")
	}
}

// --- ValidName --------------------------------------------------------

func TestValidName(t *testing.T) {
	good := []string{"a", "web", "web-small", "db-large-1", "1web"}
	bad := []string{"", "Upper", "with_underscore", "x:colon", strings.Repeat("a", 64)}
	for _, n := range good {
		if !ValidName(n) {
			t.Errorf("ValidName(%q) = false, want true", n)
		}
	}
	for _, n := range bad {
		if ValidName(n) {
			t.Errorf("ValidName(%q) = true, want false", n)
		}
	}
}
