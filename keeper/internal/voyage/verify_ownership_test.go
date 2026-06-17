package voyage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// verifyTestDB — ExecQueryRower для VerifyOwnership: QueryRow на verifyOwnershipSQL
// возвращает строку-владельца (Scan 1) либо no-rows (→ ErrLeaseLost) под noRows.
// Фиксирует args последнего QueryRow для проверки fencing-предиката.
type verifyTestDB struct {
	noRows  bool
	queryRX bool
	args    []any
}

func (d *verifyTestDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag("UPDATE 0"), nil
}

func (d *verifyTestDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	d.queryRX = true
	d.args = args
	if !strings.Contains(sql, "claimed_by_kid") || !strings.Contains(sql, "attempt") {
		return ownerRow{err: fmt.Errorf("unexpected verify sql: %s", sql)}
	}
	if d.noRows {
		return ownerRow{err: pgx.ErrNoRows}
	}
	return ownerRow{}
}

func (d *verifyTestDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, nil
}
func (d *verifyTestDB) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 0, errors.New("verifyTestDB: CopyFrom not expected")
}

type ownerRow struct{ err error }

func (r ownerRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) > 0 {
		if p, ok := dest[0].(*int); ok {
			*p = 1
		}
	}
	return nil
}

func TestVerifyOwnership_Owner(t *testing.T) {
	t.Parallel()
	db := &verifyTestDB{}
	if err := VerifyOwnership(context.Background(), db, "v1", "kid-1", 3); err != nil {
		t.Fatalf("owner must verify, got: %v", err)
	}
	if !db.queryRX {
		t.Fatal("VerifyOwnership did not query")
	}
	// fencing-предикат: voyage_id / kid / attempt в args.
	if len(db.args) != 3 || db.args[0] != "v1" || db.args[1] != "kid-1" || db.args[2] != 3 {
		t.Fatalf("verify args = %v, want [v1 kid-1 3]", db.args)
	}
}

func TestVerifyOwnership_LeaseLost(t *testing.T) {
	t.Parallel()
	db := &verifyTestDB{noRows: true}
	err := VerifyOwnership(context.Background(), db, "v1", "kid-1", 3)
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("reclaimed voyage (no row) must be ErrLeaseLost, got: %v", err)
	}
}

func TestVerifyOwnership_ValidatesInput(t *testing.T) {
	t.Parallel()
	db := &verifyTestDB{}
	if err := VerifyOwnership(context.Background(), db, "", "kid-1", 1); err == nil {
		t.Fatal("empty voyage_id must error")
	}
	if err := VerifyOwnership(context.Background(), db, "v1", "", 1); err == nil {
		t.Fatal("empty kid must error")
	}
}
