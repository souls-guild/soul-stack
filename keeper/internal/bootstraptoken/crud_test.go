package bootstraptoken

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type fakeDB struct {
	execCalls    int
	execTag      pgconn.CommandTag
	execErr      error
	lastExecSQL  string
	lastExecArgs []any

	queryCalls   int
	lastQuerySQL string
	rowFunc      func() pgx.Row
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

func (f *fakeDB) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, errors.New("fakeDB: Query not used in tests")
}

type errRow struct{ err error }

func (r errRow) Scan(_ ...any) error { return r.err }

type insertRow struct {
	tokenID   string
	createdAt time.Time
	err       error
}

func (r insertRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != 2 {
		return errors.New("insertRow: want 2 dest")
	}
	*(dest[0].(*string)) = r.tokenID
	*(dest[1].(*time.Time)) = r.createdAt
	return nil
}

type burnRow struct {
	tokenID string
	err     error
}

func (r burnRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(*string)) = r.tokenID
	return nil
}

// --- helpers for bootstraptoken_test.go ---

func timeNow() time.Time { return time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC) }
func ptrTime(t time.Time) *time.Time {
	tt := t
	return &tt
}

// --- Insert ---

func TestInsert_HappyPath(t *testing.T) {
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	f := &fakeDB{
		rowFunc: func() pgx.Row {
			return insertRow{
				tokenID:   "00000000-0000-0000-0000-000000000001",
				createdAt: now,
			}
		},
	}
	tok, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	creator := "archon-alice"
	rec, err := Insert(context.Background(), f, "host.example.com", tok.Hash(), 24*time.Hour, &creator)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if rec.TokenID == "" {
		t.Errorf("TokenID empty")
	}
	if !rec.ExpiresAt.After(now) {
		t.Errorf("ExpiresAt = %v, not in the future", rec.ExpiresAt)
	}
	if !strings.Contains(f.lastQuerySQL, "INSERT INTO bootstrap_tokens") {
		t.Errorf("SQL = %q", f.lastQuerySQL)
	}
}

func TestInsert_RejectsBadHash(t *testing.T) {
	f := &fakeDB{}
	if _, err := Insert(context.Background(), f, "host.example.com", "short", time.Hour, nil); err == nil {
		t.Fatal("Insert with short hash returned nil err")
	}
	if f.queryCalls != 0 {
		t.Errorf("queryCalls = %d; want 0 (validation before round-trip)", f.queryCalls)
	}
}

func TestInsert_RejectsZeroTTL(t *testing.T) {
	f := &fakeDB{}
	tok, _ := Generate()
	if _, err := Insert(context.Background(), f, "host.example.com", tok.Hash(), 0, nil); err == nil {
		t.Fatal("Insert with ttl=0 returned nil err")
	}
}

func TestInsert_MapsActiveExistsToSentinel(t *testing.T) {
	f := &fakeDB{
		rowFunc: func() pgx.Row {
			return insertRow{err: &pgconn.PgError{
				Code:           pgErrCodeUniqueViolation,
				ConstraintName: "bootstrap_tokens_active_by_sid_idx",
			}}
		},
	}
	tok, _ := Generate()
	_, err := Insert(context.Background(), f, "host.example.com", tok.Hash(), time.Hour, nil)
	if !errors.Is(err, ErrTokenActiveExists) {
		t.Errorf("err = %v, want ErrTokenActiveExists", err)
	}
}

func TestInsert_MapsSoulFKToSentinel(t *testing.T) {
	f := &fakeDB{
		rowFunc: func() pgx.Row {
			return insertRow{err: &pgconn.PgError{
				Code:           pgErrCodeForeignKeyViolation,
				ConstraintName: "bootstrap_tokens_sid_fk",
			}}
		},
	}
	tok, _ := Generate()
	_, err := Insert(context.Background(), f, "host.example.com", tok.Hash(), time.Hour, nil)
	if !errors.Is(err, ErrTokenSoulNotFound) {
		t.Errorf("err = %v, want ErrTokenSoulNotFound", err)
	}
}

// --- Burn ---

func TestBurn_HappyPath(t *testing.T) {
	f := &fakeDB{
		rowFunc: func() pgx.Row {
			return burnRow{tokenID: "00000000-0000-0000-0000-000000000001"}
		},
	}
	tok, _ := Generate()
	id, err := Burn(context.Background(), f, tok.Hash(), "host.example.com", "kid-1")
	if err != nil {
		t.Fatalf("Burn: %v", err)
	}
	if id == "" {
		t.Errorf("tokenID empty")
	}
	if !strings.Contains(f.lastQuerySQL, "UPDATE bootstrap_tokens") {
		t.Errorf("SQL = %q", f.lastQuerySQL)
	}
	if !strings.Contains(f.lastQuerySQL, "used_at    IS NULL") {
		t.Errorf("SQL missing race-safe WHERE used_at IS NULL clause")
	}
}

func TestBurn_NoRowsMapsToErrTokenInvalid(t *testing.T) {
	f := &fakeDB{
		rowFunc: func() pgx.Row { return burnRow{err: pgx.ErrNoRows} },
	}
	tok, _ := Generate()
	_, err := Burn(context.Background(), f, tok.Hash(), "host.example.com", "kid-1")
	if !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("err = %v, want ErrTokenInvalid", err)
	}
}

func TestBurn_RejectsBadHash(t *testing.T) {
	f := &fakeDB{}
	if _, err := Burn(context.Background(), f, "short", "host.example.com", "kid-1"); err == nil {
		t.Fatal("Burn with bad hash returned nil err")
	}
}

func TestBurn_RejectsEmptyClaimedSID(t *testing.T) {
	f := &fakeDB{}
	tok, _ := Generate()
	if _, err := Burn(context.Background(), f, tok.Hash(), "", "kid-1"); err == nil {
		t.Fatal("Burn with empty claimedSID returned nil err")
	}
}

func TestBurn_RejectsEmptyKID(t *testing.T) {
	f := &fakeDB{}
	tok, _ := Generate()
	if _, err := Burn(context.Background(), f, tok.Hash(), "host.example.com", ""); err == nil {
		t.Fatal("Burn with empty kid returned nil err")
	}
}

// --- BurnAllForSID (ADR-017 cascade) ---

func TestBurnAllForSID_HappyPath(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 1")}
	n, err := BurnAllForSID(context.Background(), f, "host.example.com", SystemKIDCloudDestroy)
	if err != nil {
		t.Fatalf("BurnAllForSID: %v", err)
	}
	if n != 1 {
		t.Errorf("rows-affected = %d, want 1", n)
	}
	if !strings.Contains(f.lastExecSQL, "UPDATE bootstrap_tokens") {
		t.Errorf("SQL=%q", f.lastExecSQL)
	}
	if !strings.Contains(f.lastExecSQL, "used_at IS NULL") {
		t.Errorf("SQL missing 'used_at IS NULL' WHERE; got %q", f.lastExecSQL)
	}
	if len(f.lastExecArgs) != 2 || f.lastExecArgs[0] != "host.example.com" || f.lastExecArgs[1] != SystemKIDCloudDestroy {
		t.Errorf("args=%v", f.lastExecArgs)
	}
}

func TestBurnAllForSID_NoActive_ReturnsZero(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("UPDATE 0")}
	n, err := BurnAllForSID(context.Background(), f, "host.example.com", SystemKIDCloudDestroy)
	if err != nil {
		t.Fatalf("BurnAllForSID: %v", err)
	}
	if n != 0 {
		t.Errorf("rows-affected = %d, want 0 (no active tokens)", n)
	}
}

func TestBurnAllForSID_RejectsEmptySID(t *testing.T) {
	f := &fakeDB{}
	if _, err := BurnAllForSID(context.Background(), f, "", SystemKIDCloudDestroy); err == nil {
		t.Fatal("BurnAllForSID with empty sid returned nil err")
	}
	if f.execCalls != 0 {
		t.Errorf("execCalls=%d; want 0 (validation before round-trip)", f.execCalls)
	}
}

func TestBurnAllForSID_RejectsEmptyUsedByKID(t *testing.T) {
	f := &fakeDB{}
	if _, err := BurnAllForSID(context.Background(), f, "host.example.com", ""); err == nil {
		t.Fatal("BurnAllForSID with empty usedByKID returned nil err")
	}
}

// --- DeleteByTokenID ---

func TestDeleteByTokenID_HappyPath(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("DELETE 1")}
	if err := DeleteByTokenID(context.Background(), f, "tid-1"); err != nil {
		t.Fatalf("DeleteByTokenID: %v", err)
	}
}

func TestDeleteByTokenID_NotFound(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("DELETE 0")}
	if err := DeleteByTokenID(context.Background(), f, "tid-missing"); !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("err = %v, want ErrTokenNotFound", err)
	}
}
