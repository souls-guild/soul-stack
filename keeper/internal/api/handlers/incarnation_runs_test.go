package handlers

// Guard-тесты read-view прогонов инкарнации на handler-границе (RunsTyped /
// RunDetailTyped): scope-гейт (deny/nil-scoper/cross-incarnation → 404), валидация
// входа (bad name → 422, bad apply_id → 400) и happy-path (empty list + per-host
// проекция store→View). Точный per-task маппинг из реальной apply_runs-строки
// (task_idx/plan_index/error) покрыт integration-тестом applyrun.SelectRunDetail —
// здесь проверяем именно handler-слой: доменная функция + inScope-предикат + проекция.
//
// Доменные функции принимают inScope-предикат НАПРЯМУЮ (тот же, что huma-слой
// собирает через GetInScopeFor(claims, "history")); тестируем их без HTTP-обёртки.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
)

// allowScope / denyScope — inScope-предикаты для scope-гейта (замена GetInScopeFor
// в прямом вызове доменной функции). Unrestricted-оператор → allow, out-of-scope →
// deny (handler отдаёт 404, не палит существование).
func allowScope(*incarnation.Incarnation) bool { return true }
func denyScope(*incarnation.Incarnation) bool  { return false }

// validApplyID — синтаксически валидный ULID (26 символов Crockford-base32,
// алфавит 0-9A-HJKMNP-TV-Z без I/L/O/U) для путей, где apply_id проходит IsValidULID.
const validApplyID = "01HZZZ00000000000000000000"

// --- RunsTyped (список прогонов) --------------------------------------

func TestRunsTyped_BadName_422(t *testing.T) {
	h := NewIncarnationHandler(&fakeIncDB{}, nil, nil, nil, nil, nil, nil, nil, nil)
	_, err := h.RunsTyped(context.Background(), "Bad_Name", 0, 50, allowScope)
	requireProblemStatus(t, err, 422)
}

func TestRunsTyped_BadLimit_400(t *testing.T) {
	h := NewIncarnationHandler(&fakeIncDB{}, nil, nil, nil, nil, nil, nil, nil, nil)
	_, err := h.RunsTyped(context.Background(), "redis-prod", 0, 99999, allowScope)
	requireProblemStatus(t, err, 400)
}

// TestRunsTyped_OutOfScope_404 — incarnation существует, но inScope деньит →
// 404 (existence-probe не палит чужую инкарнацию как 403).
func TestRunsTyped_OutOfScope_404(t *testing.T) {
	db := &fakeIncDB{selectByNameRow: func(name string) pgx.Row { return makeIncarnationRow(name) }}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	_, err := h.RunsTyped(context.Background(), "redis-prod", 0, 50, denyScope)
	requireProblemStatus(t, err, 404)
}

// TestRunsTyped_NilScope_404 — nil inScope (fail-closed мис-wire-up) → 404, store
// не трогаем.
func TestRunsTyped_NilScope_404(t *testing.T) {
	db := &fakeIncDB{selectByNameRow: func(name string) pgx.Row { return makeIncarnationRow(name) }}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	_, err := h.RunsTyped(context.Background(), "redis-prod", 0, 50, nil)
	requireProblemStatus(t, err, 404)
}

// TestRunsTyped_IncarnationNotFound_404 — инкарнации нет (SelectByName → ErrNoRows)
// → 404 ещё на existence-probe, до захода в apply_runs.
func TestRunsTyped_IncarnationNotFound_404(t *testing.T) {
	db := &fakeIncDB{selectByNameRow: func(string) pgx.Row { return errRow{err: pgx.ErrNoRows} }}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	_, err := h.RunsTyped(context.Background(), "ghost", 0, 50, allowScope)
	requireProblemStatus(t, err, 404)
}

// TestRunsTyped_Empty_OK — инкарнация в scope, прогонов нет: успех (nil error),
// пустой список, Total=0. Доказывает, что после scope-гейта RunsTyped доходит до
// store и корректно проецирует пустой набор.
func TestRunsTyped_Empty_OK(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncarnationRow(name) },
		// COUNT(DISTINCT apply_id)→0 + apply_runs Query→emptyRows (дефолт fake).
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	reply, err := h.RunsTyped(context.Background(), "redis-prod", 0, 50, allowScope)
	if err != nil {
		t.Fatalf("RunsTyped: %v", err)
	}
	if reply.Total != 0 {
		t.Errorf("Total = %d, want 0", reply.Total)
	}
	if len(reply.Items) != 0 {
		t.Errorf("len(Items) = %d, want 0", len(reply.Items))
	}
	if reply.Limit != 50 {
		t.Errorf("Limit = %d, want 50", reply.Limit)
	}
}

// --- RunDetailTyped (детали прогона) ----------------------------------

func TestRunDetailTyped_BadName_422(t *testing.T) {
	h := NewIncarnationHandler(&fakeIncDB{}, nil, nil, nil, nil, nil, nil, nil, nil)
	_, err := h.RunDetailTyped(context.Background(), "Bad_Name", validApplyID, allowScope)
	requireProblemStatus(t, err, 422)
}

// TestRunDetailTyped_BadApplyID_400 — не-ULID apply_id отбивается 400 ДО
// existence-probe (валидация раньше store).
func TestRunDetailTyped_BadApplyID_400(t *testing.T) {
	h := NewIncarnationHandler(&fakeIncDB{}, nil, nil, nil, nil, nil, nil, nil, nil)
	_, err := h.RunDetailTyped(context.Background(), "redis-prod", "not-a-ulid", allowScope)
	requireProblemStatus(t, err, 400)
}

// TestRunDetailTyped_OutOfScope_404 — incarnation есть, inScope деньит → 404
// (store не трогаем, existence-probe отбил).
func TestRunDetailTyped_OutOfScope_404(t *testing.T) {
	db := &fakeIncDB{selectByNameRow: func(name string) pgx.Row { return makeIncarnationRow(name) }}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	_, err := h.RunDetailTyped(context.Background(), "redis-prod", validApplyID, denyScope)
	requireProblemStatus(t, err, 404)
}

// TestRunDetailTyped_RunNotFound_404 — инкарнация в scope, но apply_id не
// принадлежит ей (SelectRunDetail → 0 строк → ErrApplyRunNotFound): 404. Зеркалит
// cross-incarnation-изоляцию store-слоя на handler-границе.
func TestRunDetailTyped_RunNotFound_404(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncarnationRow(name) },
		applyRunsRows:   func() (pgx.Rows, error) { return &emptyRows{}, nil }, // 0 host-строк
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	_, err := h.RunDetailTyped(context.Background(), "redis-prod", validApplyID, allowScope)
	requireProblemStatus(t, err, 404)
}

// TestRunDetailTyped_PerHostMapping_OK — happy-path проекции store→View на
// handler-границе: два хоста прогона, host-a упал (task_idx/plan_index/error
// заполнены), host-b успех (nil-детали). Проверяем, что RunHostStatusView несёт
// per-host поля и агрегатный статус — failed. Реальный per-task Scan из PG —
// в integration TestIntegration_SelectRunDetail; здесь именно handler-проекция.
func TestRunDetailTyped_PerHostMapping_OK(t *testing.T) {
	failedIdx, failedPlan := 2, 5
	errSummary := "task 2 core.pkg.installed: boom"
	now := time.Now().UTC()
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncarnationRow(name) },
		applyRunsRows: func() (pgx.Rows, error) {
			return &applyRunsHostRows{rows: []applyRunHostRow{
				{ // host-a: упал
					sid: "host-a", status: "failed", passage: 0,
					taskIdx: &failedIdx, failedPlan: &failedPlan, errorSummary: &errSummary,
					attempt: 1, cancelRequested: false,
					scenario: "scale", startedAt: now, finishedAt: &now, startedBy: strp("archon-alice"),
				},
				{ // host-b: успех
					sid: "host-b", status: "success", passage: 0,
					attempt: 1, cancelRequested: false,
					scenario: "scale", startedAt: now, finishedAt: &now, startedBy: strp("archon-alice"),
				},
			}}, nil
		},
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	d, err := h.RunDetailTyped(context.Background(), "redis-prod", validApplyID, allowScope)
	if err != nil {
		t.Fatalf("RunDetailTyped: %v", err)
	}
	if d.Scenario != "scale" {
		t.Errorf("Scenario = %q, want scale", d.Scenario)
	}
	if d.Status != "failed" {
		t.Errorf("Status = %q, want failed (host-a упал)", d.Status)
	}
	if len(d.Hosts) != 2 {
		t.Fatalf("len(Hosts) = %d, want 2", len(d.Hosts))
	}
	// host-a: несёт адрес упавшей задачи.
	ha := d.Hosts[0]
	if ha.SID != "host-a" || ha.Status != "failed" {
		t.Errorf("Hosts[0] = {%q,%q}, want {host-a,failed}", ha.SID, ha.Status)
	}
	if ha.FailedTaskIdx == nil || *ha.FailedTaskIdx != 2 {
		t.Errorf("Hosts[0].FailedTaskIdx = %v, want 2", ha.FailedTaskIdx)
	}
	if ha.FailedPlanIndex == nil || *ha.FailedPlanIndex != 5 {
		t.Errorf("Hosts[0].FailedPlanIndex = %v, want 5", ha.FailedPlanIndex)
	}
	if ha.ErrorSummary == nil || *ha.ErrorSummary != errSummary {
		t.Errorf("Hosts[0].ErrorSummary = %v, want %q", ha.ErrorSummary, errSummary)
	}
	// host-b: успех, детали упавшей задачи nil.
	hb := d.Hosts[1]
	if hb.SID != "host-b" || hb.Status != "success" {
		t.Errorf("Hosts[1] = {%q,%q}, want {host-b,success}", hb.SID, hb.Status)
	}
	if hb.FailedTaskIdx != nil || hb.FailedPlanIndex != nil || hb.ErrorSummary != nil {
		t.Errorf("Hosts[1] несёт детали упавшей задачи на success: %+v", hb)
	}
}

// requireProblemStatus проверяет, что err — доменный *problemError с ожидаемым
// HTTP-статусом (маппинг problem-type → код). t.Helper для читаемого стека.
func requireProblemStatus(t *testing.T, err error, want int) {
	t.Helper()
	if err == nil {
		t.Fatalf("ожидалась ошибка со статусом %d, получено nil", want)
	}
	d, ok := AsProblemDetails(err)
	if !ok {
		t.Fatalf("ошибка не *problemError: %v", err)
	}
	if d.Status != want {
		t.Errorf("problem status = %d, want %d (%v)", d.Status, want, err)
	}
}

// applyRunHostRow — одна host-строка apply_runs для detail-rows-stub (порядок и
// типы колонок selectRunHostsSQL: sid/status/passage/task_idx/failed_plan_index/
// error_summary/attempt/cancel_requested/scenario/started_at/finished_at/started_by).
type applyRunHostRow struct {
	sid             string
	status          string
	passage         int
	taskIdx         *int
	failedPlan      *int
	errorSummary    *string
	attempt         int32
	cancelRequested bool
	scenario        string
	startedAt       time.Time
	finishedAt      *time.Time
	startedBy       *string
}

// applyRunsHostRows — pgx.Rows-stub над набором apply_runs host-строк. Scan
// поддерживает ровно типы колонок selectRunHostsSQL (в т.ч. *int32/*bool/**int/
// **time.Time/**string, которые общий staticRow не покрывает).
type applyRunsHostRows struct {
	rows []applyRunHostRow
	idx  int
}

func (r *applyRunsHostRows) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}

func (r *applyRunsHostRows) Scan(dest ...any) error {
	row := r.rows[r.idx-1]
	vals := []any{
		row.sid, row.status, row.passage, row.taskIdx, row.failedPlan, row.errorSummary,
		row.attempt, row.cancelRequested, row.scenario, row.startedAt, row.finishedAt, row.startedBy,
	}
	for i, d := range dest {
		if err := scanApplyRunCol(d, vals[i]); err != nil {
			return err
		}
	}
	return nil
}

func (r *applyRunsHostRows) Err() error                                   { return nil }
func (r *applyRunsHostRows) Close()                                       {}
func (r *applyRunsHostRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *applyRunsHostRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *applyRunsHostRows) Values() ([]any, error)                       { return nil, nil }
func (r *applyRunsHostRows) RawValues() [][]byte                          { return nil }
func (r *applyRunsHostRows) Conn() *pgx.Conn                              { return nil }

// scanApplyRunCol присваивает v в dest-указатель нужного типа (узкий набор колонок
// apply_runs; неизвестный тип → ошибка, чтобы дрейф схемы был виден).
func scanApplyRunCol(dest, v any) error {
	switch d := dest.(type) {
	case *string:
		*d = v.(string)
	case *int:
		*d = v.(int)
	case *int32:
		*d = v.(int32)
	case *bool:
		*d = v.(bool)
	case *time.Time:
		*d = v.(time.Time)
	case **int:
		*d = v.(*int)
	case **string:
		*d = v.(*string)
	case **time.Time:
		*d = v.(*time.Time)
	default:
		return errors.New("applyRunsHostRows.Scan: неподдержанный тип dest")
	}
	return nil
}
