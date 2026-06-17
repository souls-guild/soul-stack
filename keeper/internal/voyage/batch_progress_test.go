package voyage

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// batchProgressDB — ExecQueryRower-стаб для UpdateBatchProgress: фиксирует
// последний Exec-SQL + args и эмулирует RowsAffected (0 → чужой voyage /
// сменился attempt).
type batchProgressDB struct {
	execSQL  string
	execArgs []any
	affected int64
	execErr  error
}

func (d *batchProgressDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	d.execSQL = sql
	d.execArgs = args
	if d.execErr != nil {
		return pgconn.CommandTag{}, d.execErr
	}
	tag := "UPDATE 1"
	if d.affected == 0 {
		tag = "UPDATE 0"
	}
	return pgconn.NewCommandTag(tag), nil
}
func (d *batchProgressDB) QueryRow(context.Context, string, ...any) pgx.Row { return errRowBP{} }
func (d *batchProgressDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("not expected")
}
func (d *batchProgressDB) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 0, errors.New("not expected")
}

type errRowBP struct{}

func (errRowBP) Scan(...any) error { return pgx.ErrNoRows }

func TestUpdateBatchProgress_OwnershipGuardedUpdate(t *testing.T) {
	t.Parallel()
	db := &batchProgressDB{affected: 1}
	if err := UpdateBatchProgress(context.Background(), db, "v1", "kid-1", 3, 2); err != nil {
		t.Fatalf("UpdateBatchProgress: %v", err)
	}
	// ownership-guard в WHERE: voyage_id + claimed_by_kid + attempt; SET
	// current_batch_index.
	for _, frag := range []string{"current_batch_index", "claimed_by_kid", "attempt"} {
		if !strings.Contains(db.execSQL, frag) {
			t.Errorf("SQL missing %q:\n%s", frag, db.execSQL)
		}
	}
	// args: $1 voyage_id, $2 completedBatches (SET), $3 kid, $4 attempt — порядок
	// зависит от реализации, проверяем присутствие всех значений.
	if !argsContain(db.execArgs, "v1") || !argsContain(db.execArgs, "kid-1") ||
		!argsContain(db.execArgs, 3) || !argsContain(db.execArgs, 2) {
		t.Fatalf("args = %v, want [v1 kid-1 attempt=3 completed=2]", db.execArgs)
	}
}

// UpdateBatchProgress на чужой voyage / после смены attempt → 0 rows. Best-effort:
// НЕ ошибка (правда живёт в voyage_targets, прогресс — лишь UI-подсказка).
func TestUpdateBatchProgress_OwnershipMismatchNoError(t *testing.T) {
	t.Parallel()
	db := &batchProgressDB{affected: 0}
	if err := UpdateBatchProgress(context.Background(), db, "v1", "kid-1", 99, 2); err != nil {
		t.Fatalf("0 rows (чужой voyage) не должен быть ошибкой, got: %v", err)
	}
}

func TestUpdateBatchProgress_ValidatesInput(t *testing.T) {
	t.Parallel()
	db := &batchProgressDB{affected: 1}
	if err := UpdateBatchProgress(context.Background(), db, "", "kid-1", 1, 1); err == nil {
		t.Fatal("empty voyage_id must error")
	}
	if err := UpdateBatchProgress(context.Background(), db, "v1", "", 1, 1); err == nil {
		t.Fatal("empty kid must error")
	}
}

func argsContain(args []any, want any) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
