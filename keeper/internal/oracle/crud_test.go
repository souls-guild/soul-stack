package oracle

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeDB — минимальный ExecQueryRower-stub для unit-проверки пламбинга
// (без подъёма PG; полное поведение SQL — в integration_test.go).
//
// insertErr — исход RETURNING-scan-а INSERT-ов (vigils/decrees идут через
// QueryRow … RETURNING): nil → отдаётся row с timestamp-ами (имитация успешной
// вставки), иначе ошибка (pgErr 23505 → дубликат). execSQL/execArgs фиксируют
// последний Exec (DELETE).
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
		// RETURNING cooldown?, created_at, updated_at — отдаём универсальный
		// набор; лишние/недостающие dest staticRow проигнорирует/упадёт. Для
		// vigils (2 dest) и decrees (3 dest) даём 3 значения, первое — cooldown.
		return insertRow{}
	}
	if f.queryRowRow != nil {
		return f.queryRowRow
	}
	return errRow{err: pgx.ErrNoRows}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// insertRow — RETURNING-строка INSERT-а: cooldown (string) + created_at +
// updated_at (time). vigils-INSERT сканирует 2 (created_at, updated_at),
// decrees — 3 (cooldown, created_at, updated_at); раздаём по типу dest.
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
		t.Fatalf("LastFiredAt no-rows должен давать (zero, false, nil), got err=%v", err)
	}
	if has {
		t.Error("has должен быть false при отсутствии строки")
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
		t.Error("offset < 0 должен давать ошибку")
	}
	if _, _, err := SelectAllVigils(context.Background(), db, 0, 0); err == nil {
		t.Error("limit < 1 должен давать ошибку")
	}
}

func TestRecordFire_PlumbsUpsert(t *testing.T) {
	db := &fakeDB{}
	if err := RecordFire(context.Background(), db, "d", "host-a", time.Now()); err != nil {
		t.Fatalf("RecordFire: %v", err)
	}
	if db.execSQL == "" {
		t.Fatal("RecordFire должен выполнить Exec")
	}
	if len(db.execArgs) != 3 {
		t.Errorf("RecordFire ожидает 3 аргумента (decree, subject, fired_at), got %d", len(db.execArgs))
	}
}
