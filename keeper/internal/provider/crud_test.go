package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeDB — ExecQueryRower-stub для unit-тестов. Захватывает последний SQL и
// аргументы, отдаёт настраиваемый Row / Rows. Паттерн совпадает с
// incarnation.fakeDB.
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

func validProvider() *Provider {
	aid := "archon-alice"
	return &Provider{
		Name:           "aws-eu",
		Type:           "aws",
		Region:         "eu-central-1",
		CredentialsRef: "vault:secret/cloud/aws-eu",
		CreatedByAID:   &aid,
	}
}

func TestInsert_HappyPath(t *testing.T) {
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return staticRow{values: []any{now}}
		},
	}
	p := validProvider()
	if err := Insert(context.Background(), f, p); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if !p.CreatedAt.Equal(now) {
		t.Errorf("RETURNING created_at not assigned: %v", p.CreatedAt)
	}
	if f.queryRowCalls != 1 {
		t.Errorf("queryRowCalls = %d, want 1", f.queryRowCalls)
	}
	if !strings.Contains(f.queryRowSQL, "INSERT INTO providers") {
		t.Errorf("SQL: %q", f.queryRowSQL)
	}
	if len(f.queryRowArgs) != 6 {
		t.Fatalf("args len = %d, want 6", len(f.queryRowArgs))
	}
	if f.queryRowArgs[0] != "aws-eu" || f.queryRowArgs[1] != "aws" {
		t.Errorf("args head = %v / %v", f.queryRowArgs[0], f.queryRowArgs[1])
	}
	if f.queryRowArgs[3] != "vault:secret/cloud/aws-eu" {
		t.Errorf("args[3] credentials_ref = %v", f.queryRowArgs[3])
	}
	if f.queryRowArgs[4] != "archon-alice" {
		t.Errorf("args[4] created_by_aid = %v", f.queryRowArgs[4])
	}
	// args[5] — fqdn_suffix (nil у validProvider, self-onboard не задан).
	if f.queryRowArgs[5] != nil {
		t.Errorf("args[5] fqdn_suffix = %v, want nil", f.queryRowArgs[5])
	}
}

func TestInsert_NilCreatedByAID(t *testing.T) {
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row { return staticRow{values: []any{time.Now()}} },
	}
	p := validProvider()
	p.CreatedByAID = nil
	if err := Insert(context.Background(), f, p); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if f.queryRowArgs[4] != nil {
		t.Errorf("args[4] = %v, want nil", f.queryRowArgs[4])
	}
}

func TestInsert_RejectsInvalidName(t *testing.T) {
	f := &fakeDB{}
	p := validProvider()
	p.Name = "AWS_EU"
	err := Insert(context.Background(), f, p)
	if err == nil || !strings.Contains(err.Error(), "invalid name") {
		t.Fatalf("err = %v, want invalid name", err)
	}
	if f.queryRowCalls != 0 {
		t.Errorf("queryRowCalls = %d on invalid name; want 0", f.queryRowCalls)
	}
}

func TestInsert_RejectsInvalidType(t *testing.T) {
	f := &fakeDB{}
	p := validProvider()
	p.Type = "AWS"
	if err := Insert(context.Background(), f, p); err == nil ||
		!strings.Contains(err.Error(), "invalid type") {
		t.Fatalf("err = %v, want invalid type", err)
	}
}

func TestInsert_RejectsEmptyRegion(t *testing.T) {
	f := &fakeDB{}
	p := validProvider()
	p.Region = ""
	if err := Insert(context.Background(), f, p); err == nil {
		t.Fatal("Insert with empty region returned nil")
	}
}

func TestInsert_RejectsBadCredentialsRef(t *testing.T) {
	f := &fakeDB{}
	for _, ref := range []string{"", "vault:", "secret/path", "env:FOO", "vault"} {
		p := validProvider()
		p.CredentialsRef = ref
		if err := Insert(context.Background(), f, p); err == nil {
			t.Errorf("Insert with credentials_ref %q returned nil", ref)
		}
	}
	if f.queryRowCalls != 0 {
		t.Errorf("queryRowCalls = %d on bad ref; want 0", f.queryRowCalls)
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
				ConstraintName: "providers_pkey",
			}}
		},
	}
	err := Insert(context.Background(), f, validProvider())
	if !errors.Is(err, ErrProviderAlreadyExists) {
		t.Fatalf("err = %v, want errors.Is ErrProviderAlreadyExists", err)
	}
	if !strings.Contains(err.Error(), "providers_pkey") {
		t.Errorf("err = %v; expected constraint name", err)
	}
}

func TestInsert_MapsFKViolation(t *testing.T) {
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return errRow{err: &pgconn.PgError{
				Code:           pgErrCodeForeignKeyViolation,
				ConstraintName: "providers_created_by_aid_fk",
			}}
		},
	}
	err := Insert(context.Background(), f, validProvider())
	if err == nil {
		t.Fatal("Insert with FK-violation returned nil")
	}
	if errors.Is(err, ErrProviderAlreadyExists) {
		t.Errorf("FK-violation should NOT be ErrProviderAlreadyExists; err = %v", err)
	}
	if !strings.Contains(err.Error(), "FK violation") {
		t.Errorf("err = %v; expected \"FK violation\"", err)
	}
}

// --- SelectByName -----------------------------------------------------

func TestSelectByName_HappyPath(t *testing.T) {
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return staticRow{values: []any{
				"aws-eu", "aws", "eu-central-1", "vault:secret/cloud/aws-eu",
				any("archon-alice"), now, any(nil),
			}}
		},
	}
	p, err := SelectByName(context.Background(), f, "aws-eu")
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if p.Name != "aws-eu" || p.Type != "aws" || p.Region != "eu-central-1" {
		t.Errorf("got = %+v", p)
	}
	if p.CredentialsRef != "vault:secret/cloud/aws-eu" {
		t.Errorf("CredentialsRef = %q", p.CredentialsRef)
	}
	if p.CreatedByAID == nil || *p.CreatedByAID != "archon-alice" {
		t.Errorf("CreatedByAID = %v", p.CreatedByAID)
	}
}

func TestSelectByName_NotFound(t *testing.T) {
	f := &fakeDB{} // default → ErrNoRows
	_, err := SelectByName(context.Background(), f, "missing")
	if !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("err = %v, want ErrProviderNotFound", err)
	}
}

// --- SelectAll --------------------------------------------------------

func TestSelectAll_HappyPath(t *testing.T) {
	now := time.Now()
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return staticRow{values: []any{int(2)}}
		},
		queryFunc: func(_ string) (pgx.Rows, error) {
			return &fakeRows{rows: []staticRow{
				{values: []any{"aws-eu", "aws", "eu", "vault:a", any(nil), now, any(nil)}},
				{values: []any{"yc-ru", "yc", "ru", "vault:b", any("archon-alice"), now, any("ns.vm.clv3")}},
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
	if out[0].Name != "aws-eu" || out[1].Name != "yc-ru" {
		t.Errorf("names = %s, %s", out[0].Name, out[1].Name)
	}
	if !strings.Contains(f.querySQL, "ORDER BY created_at DESC") {
		t.Errorf("ORDER missing in: %q", f.querySQL)
	}
	if len(f.queryArgs) != 2 || f.queryArgs[0] != 0 || f.queryArgs[1] != 50 {
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

// --- ValidName / ValidCredentialsRef ----------------------------------

// execDB — fakeDB-вариант с управляемым результатом Exec (для Delete-тестов).
type execDB struct {
	tag pgconn.CommandTag
	err error
}

func (f *execDB) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return f.tag, f.err
}
func (f *execDB) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	return errRow{err: pgx.ErrNoRows}
}
func (f *execDB) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return &fakeRows{}, nil
}

func TestDelete(t *testing.T) {
	ctx := context.Background()

	t.Run("ok", func(t *testing.T) {
		db := &execDB{tag: pgconn.NewCommandTag("DELETE 1")}
		if err := Delete(ctx, db, "aws"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	})

	t.Run("not-found", func(t *testing.T) {
		db := &execDB{tag: pgconn.NewCommandTag("DELETE 0")}
		if err := Delete(ctx, db, "ghost"); !errors.Is(err, ErrProviderNotFound) {
			t.Fatalf("Delete = %v, want ErrProviderNotFound", err)
		}
	})

	t.Run("has-profiles-fk", func(t *testing.T) {
		db := &execDB{err: &pgconn.PgError{Code: "23503", ConstraintName: "profiles_provider_fk"}}
		if err := Delete(ctx, db, "aws"); !errors.Is(err, ErrProviderHasProfiles) {
			t.Fatalf("Delete = %v, want ErrProviderHasProfiles", err)
		}
	})

	t.Run("invalid-name", func(t *testing.T) {
		db := &execDB{}
		if err := Delete(ctx, db, "Bad_Name"); err == nil {
			t.Fatal("Delete invalid name: want error")
		}
	})
}

func TestValidName(t *testing.T) {
	good := []string{"a", "aws", "aws-eu", "yc-ru-1", "1cloud"}
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

func TestValidCredentialsRef(t *testing.T) {
	good := []string{"vault:secret/x", "vault:a", "vault:secret/cloud/aws-eu"}
	bad := []string{"", "vault:", "vault", "secret/x", "env:FOO", "VAULT:x"}
	for _, r := range good {
		if !ValidCredentialsRef(r) {
			t.Errorf("ValidCredentialsRef(%q) = false, want true", r)
		}
	}
	for _, r := range bad {
		if ValidCredentialsRef(r) {
			t.Errorf("ValidCredentialsRef(%q) = true, want false", r)
		}
	}
}

// TestValidFQDNSuffix — форма fqdn_suffix (self-onboard Вариант T): DNS-labels
// через точку, без ведущей/замыкающей точки и underscore.
func TestValidFQDNSuffix(t *testing.T) {
	good := []string{"clv3", "vm.clv3", "fedorovstepan2-dev.vm.xc.clv3", "a.b.c"}
	bad := []string{"", ".clv3", "clv3.", "vm..clv3", "with_underscore.clv3", "UPPER.clv3", "-lead.clv3"}
	for _, s := range good {
		if !ValidFQDNSuffix(s) {
			t.Errorf("ValidFQDNSuffix(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if ValidFQDNSuffix(s) {
			t.Errorf("ValidFQDNSuffix(%q) = true, want false", s)
		}
	}
}

// TestInsert_FQDNSuffix — Insert прокидывает fqdn_suffix в args[5] и реджектит
// невалидный суффикс до round-trip-а (self-onboard Вариант T).
func TestInsert_FQDNSuffix(t *testing.T) {
	t.Run("valid suffix passed as args[5]", func(t *testing.T) {
		f := &fakeDB{queryRowFunc: func(_ string) pgx.Row { return staticRow{values: []any{time.Now()}} }}
		p := validProvider()
		suffix := "ns.vm.clv3"
		p.FQDNSuffix = &suffix
		if err := Insert(context.Background(), f, p); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		if f.queryRowArgs[5] != "ns.vm.clv3" {
			t.Errorf("args[5] fqdn_suffix = %v, want ns.vm.clv3", f.queryRowArgs[5])
		}
	})
	t.Run("invalid suffix rejected before round-trip", func(t *testing.T) {
		f := &fakeDB{queryRowFunc: func(_ string) pgx.Row { return staticRow{values: []any{time.Now()}} }}
		p := validProvider()
		bad := ".leading-dot"
		p.FQDNSuffix = &bad
		if err := Insert(context.Background(), f, p); err == nil {
			t.Fatal("expected error on invalid fqdn_suffix")
		}
		if f.queryRowCalls != 0 {
			t.Errorf("QueryRow called %d times on invalid suffix; want 0 (reject before DB)", f.queryRowCalls)
		}
	})
}
