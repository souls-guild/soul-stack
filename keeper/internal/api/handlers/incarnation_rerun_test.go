package handlers

// HANDLER-NATIVE тесты RerunCreateTyped (T5d-2c-full): прямой вызов доменной функции вместо
// httptest+(w,r). 202 → err==nil + view.{ApplyID,Incarnation}; 404/409/422 → wantProblem.

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// newRerunHandler собирает handler со всеми deps для rerun-create
// (runner + resolver + auditWriter). loader не нужен (rerun не валидирует input).
func newRerunHandler(db *fakeIncDB, starter *fakeStarter, aw *fakeAuditWriter) *IncarnationHandler {
	return NewIncarnationHandler(db, starter, nil, nil, &fakeResolver{ok: true}, nil, aw, nil, nil)
}

// rerunDB конструирует fakeIncDB под rerun-поток: SelectByName (status) +
// UnlockForRerun SELECT FOR UPDATE (state, status) тот же status.
func rerunDB(status string) *fakeIncDB {
	return &fakeIncDB{
		selectByNameRow: func(n string) pgx.Row { return makeIncStatusRow(n, status) },
		unlockSelectRow: func(_ string) pgx.Row { return makeUnlockSelectRow(status) },
	}
}

// TestRerunCreate_202_FromErrorLocked — happy path: из error_locked снимается
// блок (UnlockForRerun: applying) и запускается ровно один scenario `create`
// с общим apply_id; ответ 202 {apply_id, incarnation}; audit create_rerun с
// reason + previous_status.
func TestRerunCreate_202_FromErrorLocked(t *testing.T) {
	db := rerunDB("error_locked")
	starter := &fakeStarter{}
	aw := &fakeAuditWriter{}
	h := newRerunHandler(db, starter, aw)

	out, err := h.RerunCreateTyped(context.Background(), claims("archon-alice"), "redis-prod", "rerun bootstrap verified")
	if err != nil {
		t.Fatalf("RerunCreateTyped err = %v", err)
	}
	if out.Incarnation != "redis-prod" {
		t.Errorf("incarnation = %q", out.Incarnation)
	}
	if !audit.IsValidULID(out.ApplyID) {
		t.Errorf("apply_id not ULID: %q", out.ApplyID)
	}
	// Ровно один новый create-прогон с тем же apply_id.
	if starter.calls != 1 {
		t.Fatalf("scenario start calls = %d, want 1", starter.calls)
	}
	if starter.gotSpec.ScenarioName != "create" {
		t.Errorf("ScenarioName = %q, want create", starter.gotSpec.ScenarioName)
	}
	if starter.gotSpec.ApplyID != out.ApplyID {
		t.Errorf("run apply_id = %q, want %q", starter.gotSpec.ApplyID, out.ApplyID)
	}
	if starter.gotSpec.ServiceRef.Ref != "v1" {
		t.Errorf("ServiceRef.Ref = %q, want v1 (развёрнутая версия)", starter.gotSpec.ServiceRef.Ref)
	}
	// audit create_rerun с reason + previous_status=error_locked.
	if !hasEvent(aw, audit.EventIncarnationCreateRerun) {
		t.Fatalf("ожидался audit incarnation.create_rerun")
	}
	var ev *audit.Event
	for _, e := range aw.events {
		if e.EventType == audit.EventIncarnationCreateRerun {
			ev = e
		}
	}
	if ev.Payload["reason"] != "rerun bootstrap verified" {
		t.Errorf("audit reason = %v", ev.Payload["reason"])
	}
	if ev.Payload["previous_status"] != "error_locked" {
		t.Errorf("audit previous_status = %v, want error_locked", ev.Payload["previous_status"])
	}
	if ev.Payload["apply_id"] != out.ApplyID {
		t.Errorf("audit apply_id = %v, want %q", ev.Payload["apply_id"], out.ApplyID)
	}
	// НЕ переиспользует incarnation.unlocked.
	if hasEvent(aw, audit.EventIncarnationUnlocked) {
		t.Errorf("rerun не должен писать incarnation.unlocked")
	}
}

// TestRerunCreate_RejectNonErrorLocked — из ready/applying/migration_failed
// rerun отклонён (409 incarnation-locked), прогон НЕ стартует.
func TestRerunCreate_RejectNonErrorLocked(t *testing.T) {
	for _, status := range []string{"ready", "applying", "migration_failed", "destroy_failed", "drift"} {
		t.Run(status, func(t *testing.T) {
			db := rerunDB(status)
			starter := &fakeStarter{}
			h := newRerunHandler(db, starter, &fakeAuditWriter{})

			_, err := h.RerunCreateTyped(context.Background(), claims("archon-alice"), "redis-prod", "x")
			wantProblem(t, err, problem.TypeIncarnationLocked)
			if starter.calls != 0 {
				t.Errorf("status=%s: scenario start calls = %d, want 0", status, starter.calls)
			}
		})
	}
}

// TestRerunCreate_RejectNonCreateScenario_409 — scope=create (GUARD): error_locked,
// но последний упавший сценарий — НЕ create (add_user) → 409 incarnation-locked,
// прогон НЕ стартует. rerun-create перезапускает строго bootstrap `create`.
func TestRerunCreate_RejectNonCreateScenario_409(t *testing.T) {
	db := rerunDB("error_locked")
	db.lastScenarioRow = func(_ string) pgx.Row { return staticRow{values: []any{"add_user"}} }
	starter := &fakeStarter{}
	h := newRerunHandler(db, starter, &fakeAuditWriter{})

	_, err := h.RerunCreateTyped(context.Background(), claims("archon-alice"), "redis-prod", "rerun add_user verified")
	wantProblem(t, err, problem.TypeIncarnationLocked)
	if starter.calls != 0 {
		t.Errorf("scenario start calls = %d, want 0 (отказ scope=create до старта)", starter.calls)
	}
}

// TestRerunCreate_NotFound_404 — несуществующая incarnation → 404.
func TestRerunCreate_NotFound_404(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(_ string) pgx.Row { return errRow{err: pgx.ErrNoRows} },
	}
	starter := &fakeStarter{}
	h := newRerunHandler(db, starter, &fakeAuditWriter{})

	_, err := h.RerunCreateTyped(context.Background(), claims("archon-alice"), "ghost", "x")
	wantProblem(t, err, problem.TypeNotFound)
	if starter.calls != 0 {
		t.Errorf("scenario start calls = %d, want 0", starter.calls)
	}
}

// TestRerunCreate_EmptyReason_422 — пустой reason → 422 (явное подтверждение).
func TestRerunCreate_EmptyReason_422(t *testing.T) {
	db := rerunDB("error_locked")
	starter := &fakeStarter{}
	h := newRerunHandler(db, starter, &fakeAuditWriter{})

	_, err := h.RerunCreateTyped(context.Background(), claims("archon-alice"), "redis-prod", "")
	wantProblem(t, err, problem.TypeValidationFailed)
	if starter.calls != 0 {
		t.Errorf("scenario start calls = %d, want 0 (отказ до старта)", starter.calls)
	}
}

// TestRerunCreate_InvalidName_422 — невалидное имя в path → 422.
func TestRerunCreate_InvalidName_422(t *testing.T) {
	h := newRerunHandler(rerunDB("error_locked"), &fakeStarter{}, &fakeAuditWriter{})
	_, err := h.RerunCreateTyped(context.Background(), claims("archon-alice"), "Bad_Name", "x")
	wantProblem(t, err, problem.TypeValidationFailed)
}

// TestRerunCreate_ReasonAtMax_202 — reason ровно ReasonMaxLen символов проходит
// (граница включительно): rerun-create стартует, scenario start вызван.
func TestRerunCreate_ReasonAtMax_202(t *testing.T) {
	db := rerunDB("error_locked")
	starter := &fakeStarter{}
	h := newRerunHandler(db, starter, &fakeAuditWriter{})

	reason := strings.Repeat("a", incarnation.ReasonMaxLen)
	out, err := h.RerunCreateTyped(context.Background(), claims("archon-alice"), "redis-prod", reason)
	if err != nil {
		t.Fatalf("RerunCreateTyped err = %v (reason ровно %d допустим)", err, incarnation.ReasonMaxLen)
	}
	if !audit.IsValidULID(out.ApplyID) {
		t.Errorf("apply_id not ULID: %q", out.ApplyID)
	}
	if starter.calls != 1 {
		t.Errorf("scenario start calls = %d, want 1", starter.calls)
	}
}

// TestRerunCreate_ReasonOverMax_422 — reason длиннее ReasonMaxLen → 422 ДО старта
// (верхняя граница reason, поведенческий инвариант).
func TestRerunCreate_ReasonOverMax_422(t *testing.T) {
	db := rerunDB("error_locked")
	starter := &fakeStarter{}
	h := newRerunHandler(db, starter, &fakeAuditWriter{})

	reason := strings.Repeat("a", incarnation.ReasonMaxLen+1)
	_, err := h.RerunCreateTyped(context.Background(), claims("archon-alice"), "redis-prod", reason)
	wantProblem(t, err, problem.TypeValidationFailed)
	if starter.calls != 0 {
		t.Errorf("scenario start calls = %d, want 0 (reason over max → отказ до старта)", starter.calls)
	}
}

// TestRerunCreate_ReasonMultibyteAtMax_202 — ЛОК рунной семантики (спека↔рантайм):
// reason из ReasonMaxLen кириллических рун — это 2*ReasonMaxLen БАЙТ, но ровно
// ReasonMaxLen рун. JSON-Schema maxLength считает руны, значит ДОЛЖЕН пройти, хотя
// по байтам это >maxLen. Ловит регресс len(reason)↔utf8.RuneCountInString.
func TestRerunCreate_ReasonMultibyteAtMax_202(t *testing.T) {
	db := rerunDB("error_locked")
	starter := &fakeStarter{}
	h := newRerunHandler(db, starter, &fakeAuditWriter{})

	reason := strings.Repeat("я", incarnation.ReasonMaxLen) // ReasonMaxLen рун, 2*ReasonMaxLen байт
	if len(reason) <= incarnation.ReasonMaxLen {
		t.Fatalf("предусловие теста нарушено: %d байт не превышает лимит %d — кейс не различает байты/руны",
			len(reason), incarnation.ReasonMaxLen)
	}
	out, err := h.RerunCreateTyped(context.Background(), claims("archon-alice"), "redis-prod", reason)
	if err != nil {
		t.Fatalf("RerunCreateTyped err = %v (ReasonMaxLen рун кириллицей допустимо — считаем руны, не байты)", err)
	}
	if !audit.IsValidULID(out.ApplyID) {
		t.Errorf("apply_id not ULID: %q", out.ApplyID)
	}
	if starter.calls != 1 {
		t.Errorf("scenario start calls = %d, want 1", starter.calls)
	}
}

// TestRerunCreate_ReasonMultibyteOverMax_422 — обратная граница рунной семантики:
// ReasonMaxLen+1 кириллических рун → 422 ДО старта (по рунам превышено).
func TestRerunCreate_ReasonMultibyteOverMax_422(t *testing.T) {
	db := rerunDB("error_locked")
	starter := &fakeStarter{}
	h := newRerunHandler(db, starter, &fakeAuditWriter{})

	reason := strings.Repeat("я", incarnation.ReasonMaxLen+1)
	_, err := h.RerunCreateTyped(context.Background(), claims("archon-alice"), "redis-prod", reason)
	wantProblem(t, err, problem.TypeValidationFailed)
	if starter.calls != 0 {
		t.Errorf("scenario start calls = %d, want 0 (reason over max рунами → отказ до старта)", starter.calls)
	}
}
