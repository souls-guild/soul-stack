package voyageorch

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
)

// fakeOrphanReleaser — stub [OrphanLockReleaser]: фиксирует переданные аргументы
// и возвращает заранее заданный (released, err).
type fakeOrphanReleaser struct {
	released bool
	err      error

	calls          int
	gotVoyageID    string
	gotIncarnation string
	gotAttempt     int
	gotKID         string
	gotApplyID     string
}

func (f *fakeOrphanReleaser) ReleaseOrphanLock(_ context.Context, voyageID, name string, attempt int, kid, orphanApplyID string) (bool, error) {
	f.calls++
	f.gotVoyageID = voyageID
	f.gotIncarnation = name
	f.gotAttempt = attempt
	f.gotKID = kid
	f.gotApplyID = orphanApplyID
	return f.released, f.err
}

// targetRows — fake pgx.Rows над срезом voyage_targets-строк (8 колонок порядка
// selectTargetsSQL: voyage_id, target_kind, target_id, batch_index, status,
// apply_id, errand_id, finished_at).
type targetRows struct {
	rows [][]any
	i    int
}

func (r *targetRows) Next() bool { r.i++; return r.i <= len(r.rows) }

func (r *targetRows) Scan(dest ...any) error {
	row := r.rows[r.i-1]
	if len(dest) != len(row) {
		return errors.New("targetRows: dest/row len mismatch")
	}
	for i, v := range row {
		switch p := dest[i].(type) {
		case *string:
			*p = v.(string)
		case **string:
			if v == nil {
				*p = nil
			} else {
				s := v.(string)
				*p = &s
			}
		case *int:
			*p = v.(int)
		default:
			// finished_at (**time.Time) — всегда nil в этих тестах.
			if v != nil {
				return errors.New("targetRows: unexpected non-nil for unsupported dest")
			}
		}
	}
	return nil
}

func (r *targetRows) Close()                                       {}
func (r *targetRows) Err() error                                   { return nil }
func (r *targetRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *targetRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *targetRows) Values() ([]any, error)                       { return nil, nil }
func (r *targetRows) RawValues() [][]byte                          { return nil }
func (r *targetRows) Conn() *pgx.Conn                              { return nil }

// targetRow — одна строка voyage_targets для fake-Query.
func targetRow(voyageID, kind, targetID, status string, applyID *string) []any {
	var ap any
	if applyID != nil {
		ap = *applyID
	}
	return []any{voyageID, kind, targetID, 0, status, ap, nil, nil}
}

func strptr(s string) *string { return &s }

// withTargets конфигурирует fakeDB.Query на возврат заданных voyage_targets.
func withTargets(f *fakeDB, rows ...[]any) {
	f.queryFn = func(_ string, _ []any) (pgx.Rows, error) {
		return &targetRows{rows: rows}, nil
	}
}

// TestReconcileOrphanLock_NilReleaser — детект выключен (OrphanReleaser=nil):
// reconcileOrphanLock — no-op, SelectTargets даже не вызывается (Query не
// сконфигурирован → если бы вызвался, был бы error). nil-ошибка.
func TestReconcileOrphanLock_NilReleaser(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{} // Query "not configured" — упадёт, если детект его дёрнет
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger()}
	run := &voyage.Voyage{VoyageID: "v1", Attempt: 2}

	if err := w.reconcileOrphanLock(context.Background(), run, "a"); err != nil {
		t.Fatalf("reconcileOrphanLock (nil releaser): %v", err)
	}
}

// TestReconcileOrphanLock_OrphanRunning_Released — orphan от прошлого attempt
// (target в running с back-link apply_id) → releaser вызван с этим apply_id,
// voyage_id, attempt, kid; released=true → nil-ошибка.
func TestReconcileOrphanLock_OrphanRunning_Released(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	withTargets(fdb, targetRow("v1", string(voyage.TargetKindIncarnation), "a",
		string(voyage.TargetStatusRunning), strptr("ap-orphan")))
	rel := &fakeOrphanReleaser{released: true}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), OrphanReleaser: rel}
	run := &voyage.Voyage{VoyageID: "v1", Attempt: 2}

	if err := w.reconcileOrphanLock(context.Background(), run, "a"); err != nil {
		t.Fatalf("reconcileOrphanLock: %v", err)
	}
	if rel.calls != 1 {
		t.Fatalf("releaser calls = %d, want 1", rel.calls)
	}
	if rel.gotApplyID != "ap-orphan" {
		t.Errorf("orphan apply_id = %q, want ap-orphan", rel.gotApplyID)
	}
	if rel.gotVoyageID != "v1" || rel.gotIncarnation != "a" || rel.gotAttempt != 2 || rel.gotKID != "k" {
		t.Errorf("releaser args = (voyage=%q inc=%q attempt=%d kid=%q)",
			rel.gotVoyageID, rel.gotIncarnation, rel.gotAttempt, rel.gotKID)
	}
}

// TestReconcileOrphanLock_TerminalTarget_NoOp — target уже терминален
// (succeeded): инкарнация не висит → releaser НЕ вызван (нечего снимать).
func TestReconcileOrphanLock_TerminalTarget_NoOp(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	withTargets(fdb, targetRow("v1", string(voyage.TargetKindIncarnation), "a",
		string(voyage.TargetStatusSucceeded), strptr("ap-done")))
	rel := &fakeOrphanReleaser{released: true}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), OrphanReleaser: rel}
	run := &voyage.Voyage{VoyageID: "v1", Attempt: 2}

	if err := w.reconcileOrphanLock(context.Background(), run, "a"); err != nil {
		t.Fatalf("reconcileOrphanLock: %v", err)
	}
	if rel.calls != 0 {
		t.Errorf("releaser calls = %d, want 0 (terminal target — нечего снимать)", rel.calls)
	}
}

// TestReconcileOrphanLock_NoBacklink_NoOp — target в awaiting без apply_id
// (спавн прошлого attempt не дошёл до MarkTargetRunning): releaser НЕ вызван.
func TestReconcileOrphanLock_NoBacklink_NoOp(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	withTargets(fdb, targetRow("v1", string(voyage.TargetKindIncarnation), "a",
		string(voyage.TargetStatusAwaiting), nil))
	rel := &fakeOrphanReleaser{released: true}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), OrphanReleaser: rel}
	run := &voyage.Voyage{VoyageID: "v1", Attempt: 2}

	if err := w.reconcileOrphanLock(context.Background(), run, "a"); err != nil {
		t.Fatalf("reconcileOrphanLock: %v", err)
	}
	if rel.calls != 0 {
		t.Errorf("releaser calls = %d, want 0 (нет back-link — не наш orphan)", rel.calls)
	}
}

// TestReconcileOrphanLock_LegitInLockWindow_NotPickedUp — guard на «reaudit
// minor»: legit-прогон (прямой run / ЧУЖОЙ Voyage), попавший в окно
// lockRun→Insert(apply_runs) — applying закоммичен, apply_runs ещё нет, его
// apply_id=ap-legit — НЕ снимается orphan-швом. Причина: reconcileOrphanLock
// берёт orphan-кандидата ТОЛЬКО из voyage_targets ЭТОГО Voyage (back-link
// прошлого attempt). voyage_targets нашего Voyage НЕ содержит строки инкарнации
// "a" с apply_id=ap-legit (она принадлежит чужому прогону — у нас здесь строка
// другой инкарнации "b") → orphanApplyID не найден → releaser НЕ вызван. Защита
// держится на связке FENCING-2/3 (back-link под attempt-CAS), а не на одном
// FENCING-1 (live-rival apply_runs-EXISTS): даже если apply_runs ещё пуст
// (окно до Insert), отсутствие back-link под нашим voyage_id уже отсекает чужой
// lock на стадии детекта, до обращения к releaser-у.
func TestReconcileOrphanLock_LegitInLockWindow_NotPickedUp(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	// В voyage_targets ЭТОГО Voyage есть только строка инкарнации "b". Инкарнация
	// "a" (на которой висит legit-прогон с ap-legit) в targets нашего Voyage НЕ
	// фигурирует — back-link под нашим voyage_id для "a" отсутствует.
	withTargets(fdb, targetRow("v1", string(voyage.TargetKindIncarnation), "b",
		string(voyage.TargetStatusRunning), strptr("ap-legit")))
	rel := &fakeOrphanReleaser{released: true}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), OrphanReleaser: rel}
	run := &voyage.Voyage{VoyageID: "v1", Attempt: 2}

	// Реконсиляция для "a" (наш re-run метит её) — у "a" нет back-link → no-op.
	if err := w.reconcileOrphanLock(context.Background(), run, "a"); err != nil {
		t.Fatalf("reconcileOrphanLock: %v", err)
	}
	if rel.calls != 0 {
		t.Errorf("releaser calls = %d, want 0 (legit-прогон чужого apply_id без back-link под нашим voyage НЕ снимается)", rel.calls)
	}
}

// TestReconcileOrphanLock_ReleaserError_Propagates — CRUD-сбой releaser-а
// (например VerifyOwnership транзиент) пробрасывается caller-у (→ fail-closed
// по инкарнации в runOneIncarnation).
func TestReconcileOrphanLock_ReleaserError_Propagates(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	withTargets(fdb, targetRow("v1", string(voyage.TargetKindIncarnation), "a",
		string(voyage.TargetStatusRunning), strptr("ap-orphan")))
	rel := &fakeOrphanReleaser{err: errors.New("pg down")}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), OrphanReleaser: rel}
	run := &voyage.Voyage{VoyageID: "v1", Attempt: 2}

	err := w.reconcileOrphanLock(context.Background(), run, "a")
	if err == nil {
		t.Fatal("reconcileOrphanLock: want error, got nil")
	}
}

// TestReconcileOrphanLock_ReleaserNoOp_NoError — releaser вернул released=false
// (fenced no-op: не applying / нас реклеймнули / apply_id-mismatch) → nil-ошибка,
// caller продолжает re-run (lockRun сам отбракует, если не runnable).
func TestReconcileOrphanLock_ReleaserNoOp_NoError(t *testing.T) {
	t.Parallel()
	fdb := &fakeDB{}
	withTargets(fdb, targetRow("v1", string(voyage.TargetKindIncarnation), "a",
		string(voyage.TargetStatusRunning), strptr("ap-orphan")))
	rel := &fakeOrphanReleaser{released: false}
	w := &VoyageWorker{KID: "k", Pool: fdb, Logger: quietLogger(), OrphanReleaser: rel}
	run := &voyage.Voyage{VoyageID: "v1", Attempt: 2}

	if err := w.reconcileOrphanLock(context.Background(), run, "a"); err != nil {
		t.Fatalf("reconcileOrphanLock (no-op): %v", err)
	}
	if rel.calls != 1 {
		t.Errorf("releaser calls = %d, want 1", rel.calls)
	}
}
