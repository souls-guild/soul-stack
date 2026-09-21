package oracle

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeDB is a minimal ExecQueryRower stub for unit-testing the plumbing
// (without spinning up PG; full SQL behavior is covered in integration_test.go).
//
// insertErr is the outcome of the RETURNING scan for INSERTs (vigils/decrees go
// through QueryRow … RETURNING): nil → a row with timestamps is returned (simulating
// a successful insert), otherwise an error (pgErr 23505 → duplicate). execSQL/execArgs
// record the last Exec (DELETE).
type fakeDB struct {
	queryRowRow pgx.Row
	insertErr   error
	execTag     pgconn.CommandTag
	execErr     error
	execSQL     string
	execArgs    []any
}

func (f *fakeDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.execSQL = sql
	f.execArgs = args
	return f.execTag, f.execErr
}

func (f *fakeDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	if contains(sql, "INSERT INTO") && contains(sql, "RETURNING") {
		if f.insertErr != nil {
			return errRow{err: f.insertErr}
		}
		// RETURNING cooldown?, created_at, updated_at — we return a universal
		// set; staticRow will ignore extra dest or panic on missing ones. For
		// vigils (2 dest) and decrees (3 dest) we give 3 values, the first being cooldown.
		return insertRow{}
	}
	if f.queryRowRow != nil {
		return f.queryRowRow
	}
	return errRow{err: pgx.ErrNoRows}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// insertRow is the RETURNING row for an INSERT: cooldown (string) + created_at +
// updated_at (time). vigils-INSERT scans 2 (created_at, updated_at),
// decrees — 3 (cooldown, created_at, updated_at); we dispatch by dest type.
type insertRow struct{}

func (insertRow) Scan(dest ...any) error {
	now := time.Now()
	for _, d := range dest {
		switch d := d.(type) {
		case *string:
			*d = "0s"
		case *time.Time:
			*d = now
		}
	}
	return nil
}

func (f *fakeDB) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, pgx.ErrNoRows
}

type errRow struct{ err error }

func (r errRow) Scan(_ ...any) error { return r.err }

func TestLastFiredAt_NoRows(t *testing.T) {
	db := &fakeDB{} // QueryRow → ErrNoRows
	_, has, err := LastFiredAt(context.Background(), db, "d", "host-a")
	if err != nil {
		t.Fatalf("LastFiredAt no-rows should yield (zero, false, nil), got err=%v", err)
	}
	if has {
		t.Error("has should be false when the row is absent")
	}
}

func TestDeleteVigil_NotFound(t *testing.T) {
	db := &fakeDB{execTag: pgconn.NewCommandTag("DELETE 0")}
	if err := DeleteVigil(context.Background(), db, "ghost"); err != ErrVigilNotFound {
		t.Errorf("DeleteVigil(ghost) = %v, want ErrVigilNotFound", err)
	}
}

func TestDeleteVigil_OK(t *testing.T) {
	db := &fakeDB{execTag: pgconn.NewCommandTag("DELETE 1")}
	if err := DeleteVigil(context.Background(), db, "web-conf"); err != nil {
		t.Errorf("DeleteVigil(web-conf) = %v, want nil", err)
	}
}

func TestDeleteDecree_NotFound(t *testing.T) {
	db := &fakeDB{execTag: pgconn.NewCommandTag("DELETE 0")}
	if err := DeleteDecree(context.Background(), db, "ghost"); err != ErrDecreeNotFound {
		t.Errorf("DeleteDecree(ghost) = %v, want ErrDecreeNotFound", err)
	}
}

func TestSelectAllVigils_BadPaging(t *testing.T) {
	db := &fakeDB{}
	if _, _, err := SelectAllVigils(context.Background(), db, -1, 10); err == nil {
		t.Error("offset < 0 should produce an error")
	}
	if _, _, err := SelectAllVigils(context.Background(), db, 0, 0); err == nil {
		t.Error("limit < 1 should produce an error")
	}
}

func TestRecordFire_PlumbsUpsert(t *testing.T) {
	db := &fakeDB{}
	if err := RecordFire(context.Background(), db, "d", "host-a", time.Now()); err != nil {
		t.Fatalf("RecordFire: %v", err)
	}
	if db.execSQL == "" {
		t.Fatal("RecordFire should execute Exec")
	}
	if len(db.execArgs) != 3 {
		t.Errorf("RecordFire expects 3 arguments (decree, subject, fired_at), got %d", len(db.execArgs))
	}
}
