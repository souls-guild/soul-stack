package handlers

// Guard-тесты read-view задач прогона на handler-границе (RunTasksTyped, NIM-37):
// вход (bad name → 422, bad apply_id → 400), scope-гейт (deny → 404, чужой apply_id
// = EXISTS false → 404), и главная логика джойна плана (apply_run_plan) с per-host
// результатами из audit (task.executed): группировка plan_index→sid, last-wins на
// retry, сортировка по sid, no_log → output подавлен, pending-хост (в плане, но без
// audit-результата) исключён. Реальный SQL-scan плана/audit — в integration store-
// тестах; здесь проверяем handler-слой: доменную функцию + join + inScope.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
)

// planRow — одна строка apply_run_plan (порядок колонок selectRunPlanByApplyIDSQL:
// plan_index/name/module/no_log/passage/params).
type planRow struct {
	planIndex int
	name      string
	module    string
	noLog     bool
	passage   int
	params    []byte
}

// runPlanRowsStub — pgx.Rows-stub над набором строк плана прогона.
type runPlanRowsStub struct {
	rows []planRow
	idx  int
}

func (r *runPlanRowsStub) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}

func (r *runPlanRowsStub) Scan(dest ...any) error {
	row := r.rows[r.idx-1]
	*(dest[0].(*int)) = row.planIndex
	*(dest[1].(*string)) = row.name
	*(dest[2].(*string)) = row.module
	*(dest[3].(*bool)) = row.noLog
	*(dest[4].(*int)) = row.passage
	*(dest[5].(*[]byte)) = row.params
	return nil
}

func (r *runPlanRowsStub) Err() error                                   { return nil }
func (r *runPlanRowsStub) Close()                                       {}
func (r *runPlanRowsStub) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *runPlanRowsStub) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *runPlanRowsStub) Values() ([]any, error)                       { return nil, nil }
func (r *runPlanRowsStub) RawValues() [][]byte                          { return nil }
func (r *runPlanRowsStub) Conn() *pgx.Conn                              { return nil }

// fakeRunTasksAudit — RunTaskAuditReader-fake: отдаёт заранее заданные execs
// (в порядке времени, как реальный SelectTaskExecutions — поздний перезаписывает).
type fakeRunTasksAudit struct {
	execs []auditpg.TaskExecution
	err   error
}

func (f *fakeRunTasksAudit) SelectTaskExecutions(_ context.Context, _ string) ([]auditpg.TaskExecution, error) {
	return f.execs, f.err
}

func withPlan(db *fakeIncDB, rows ...planRow) *fakeIncDB {
	db.selectByNameRow = func(name string) pgx.Row { return makeIncarnationRow(name) }
	db.runPlanRows = func() (pgx.Rows, error) { return &runPlanRowsStub{rows: rows}, nil }
	return db
}

// --- вход + scope-гейт --------------------------------------------------

func TestRunTasksTyped_BadName_422(t *testing.T) {
	h := NewIncarnationHandler(&fakeIncDB{}, nil, nil, nil, nil, nil, nil, nil, nil)
	_, err := h.RunTasksTyped(context.Background(), "Bad_Name", validApplyID, allowScope)
	requireProblemStatus(t, err, 422)
}

func TestRunTasksTyped_BadApplyID_400(t *testing.T) {
	h := NewIncarnationHandler(&fakeIncDB{}, nil, nil, nil, nil, nil, nil, nil, nil)
	_, err := h.RunTasksTyped(context.Background(), "redis-prod", "not-a-ulid", allowScope)
	requireProblemStatus(t, err, 400)
}

// TestRunTasksTyped_OutOfScope_404 — incarnation есть, inScope деньит → 404 (не
// палим существование), store плана не трогаем.
func TestRunTasksTyped_OutOfScope_404(t *testing.T) {
	db := &fakeIncDB{selectByNameRow: func(name string) pgx.Row { return makeIncarnationRow(name) }}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	_, err := h.RunTasksTyped(context.Background(), "redis-prod", validApplyID, denyScope)
	requireProblemStatus(t, err, 404)
}

// TestRunTasksTyped_ForeignApplyID_404 — инкарнация в scope, но apply_id ей НЕ
// принадлежит (EXISTS-probe → false): 404. Изоляция «чужой прогон» на handler-
// границе, до чтения плана.
func TestRunTasksTyped_ForeignApplyID_404(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncarnationRow(name) },
		runExistsRow:    func(string, string) pgx.Row { return staticRow{values: []any{false}} },
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	_, err := h.RunTasksTyped(context.Background(), "redis-prod", validApplyID, allowScope)
	requireProblemStatus(t, err, 404)
}

// TestRunTasksTyped_EmptyPlan_OK — прогон принадлежит, плана нет (упал до render /
// legacy): успех, tasks пуст (не ошибка).
func TestRunTasksTyped_EmptyPlan_OK(t *testing.T) {
	db := withPlan(&fakeIncDB{}) // без строк плана
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	v, err := h.RunTasksTyped(context.Background(), "redis-prod", validApplyID, allowScope)
	if err != nil {
		t.Fatalf("RunTasksTyped: %v", err)
	}
	if len(v.Tasks) != 0 {
		t.Errorf("len(Tasks) = %d, want 0", len(v.Tasks))
	}
}

// TestRunTasksTyped_PlanAuditJoin — центральный guard: план из 3 задач джойнится с
// audit-результатами по plan_index→sid. Проверяет last-wins (retry host-a),
// сортировку хостов по sid, no_log→output подавлен, pending-задачу без audit
// (hosts пуст), и что masked params (S1b) десериализуются из jsonb в object.
func TestRunTasksTyped_PlanAuditJoin(t *testing.T) {
	db := withPlan(&fakeIncDB{},
		planRow{planIndex: 0, name: "install", module: "core.pkg.installed", noLog: false, passage: 0, params: []byte(`{"name":"redis","state":"present"}`)},
		planRow{planIndex: 1, name: "secret", module: "core.exec.run", noLog: true, passage: 0, params: nil},
		planRow{planIndex: 2, name: "restart", module: "core.service.running", noLog: false, passage: 1, params: []byte(`{"unit":"redis"}`)},
	)
	// execs упорядочены по времени: для (idx0, host-a) первый OK перезаписывается
	// поздним FAILED (retry, last-wins). idx1 — no_log: output подавлен на write-
	// path-е (nil). idx2 — без execs → pending (hosts пуст).
	audit := &fakeRunTasksAudit{execs: []auditpg.TaskExecution{
		{SID: "host-b", PlanIndex: 0, Status: "TASK_STATUS_CHANGED", Output: map[string]any{"changed": true}},
		{SID: "host-a", PlanIndex: 0, Status: "TASK_STATUS_OK", Output: map[string]any{"first": true}},
		{SID: "host-a", PlanIndex: 0, Status: "TASK_STATUS_FAILED",
			Error: &auditpg.TaskExecutionError{Code: "E_APPLY", Module: "core.pkg.installed", Message: "boom"}},
		{SID: "host-a", PlanIndex: 1, Status: "TASK_STATUS_OK"}, // no_log → Output nil
	}}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	h.SetRunTasksAuditReader(audit)

	v, err := h.RunTasksTyped(context.Background(), "redis-prod", validApplyID, allowScope)
	if err != nil {
		t.Fatalf("RunTasksTyped: %v", err)
	}
	if len(v.Tasks) != 3 {
		t.Fatalf("len(Tasks) = %d, want 3", len(v.Tasks))
	}

	// task[0]: 2 хоста, отсортированы по sid (host-a, host-b); host-a=last FAILED.
	t0 := v.Tasks[0]
	if t0.PlanIndex != 0 || t0.Name != "install" || t0.NoLog {
		t.Errorf("task0 header = %+v", t0)
	}
	// S1b: masked params из jsonb десериализованы в object.
	if t0.Params == nil || t0.Params["name"] != "redis" || t0.Params["state"] != "present" {
		t.Errorf("task0.Params = %v, want {name:redis, state:present}", t0.Params)
	}
	if len(t0.Hosts) != 2 {
		t.Fatalf("task0 hosts = %d, want 2", len(t0.Hosts))
	}
	if t0.Hosts[0].SID != "host-a" || t0.Hosts[1].SID != "host-b" {
		t.Errorf("task0 hosts не отсортированы по sid: %q,%q", t0.Hosts[0].SID, t0.Hosts[1].SID)
	}
	ha := t0.Hosts[0]
	if ha.Status != "TASK_STATUS_FAILED" {
		t.Errorf("task0 host-a status = %q, want FAILED (last-wins retry)", ha.Status)
	}
	if ha.Error == nil || ha.Error.Code != "E_APPLY" || ha.Error.Message != "boom" {
		t.Errorf("task0 host-a error = %+v, want {E_APPLY,...,boom}", ha.Error)
	}
	if ha.Output != nil {
		t.Errorf("task0 host-a output = %v, want nil (последний exec без register_data)", ha.Output)
	}
	hb := t0.Hosts[1]
	if hb.Status != "TASK_STATUS_CHANGED" || hb.Error != nil {
		t.Errorf("task0 host-b = {%q, err=%v}, want {CHANGED, nil}", hb.Status, hb.Error)
	}
	if hb.Output == nil || hb.Output["changed"] != true {
		t.Errorf("task0 host-b output = %v, want {changed:true}", hb.Output)
	}

	// task[1]: no_log — output подавлен (nil), но задача и её хост видны.
	t1 := v.Tasks[1]
	if t1.PlanIndex != 1 || !t1.NoLog {
		t.Errorf("task1 header = %+v, want plan_index=1 no_log=true", t1)
	}
	// no_log-задача: params НЕ хранятся (NULL) → nil в DTO (симметрия с output).
	if t1.Params != nil {
		t.Errorf("task1 (no_log) Params = %v, want nil (params не хранятся)", t1.Params)
	}
	if len(t1.Hosts) != 1 || t1.Hosts[0].SID != "host-a" {
		t.Fatalf("task1 hosts = %+v, want [host-a]", t1.Hosts)
	}
	if t1.Hosts[0].Output != nil {
		t.Errorf("task1 (no_log) host output = %v, want nil (подавлен на write-path)", t1.Hosts[0].Output)
	}

	// task[2]: pending — в плане есть, но audit-результата нет → hosts пуст.
	t2 := v.Tasks[2]
	if t2.PlanIndex != 2 {
		t.Errorf("task2 plan_index = %d, want 2", t2.PlanIndex)
	}
	if len(t2.Hosts) != 0 {
		t.Errorf("task2 (pending) hosts = %d, want 0 (нет audit-результата)", len(t2.Hosts))
	}
	if t2.Params == nil || t2.Params["unit"] != "redis" {
		t.Errorf("task2.Params = %v, want {unit:redis}", t2.Params)
	}
}

// TestRunTasksTyped_MaskedParamsFlowThrough — masked-значение params (замаскировано
// на write-path в persistRunPlan) доезжает до DTO КАК ЕСТЬ: handler не размаскирует
// и не палит plaintext. Guard read-стороны «/tasks не раскрывает секрет»: строка
// плана несёт уже-masked jsonb, в ответе только ***MASKED***.
func TestRunTasksTyped_MaskedParamsFlowThrough(t *testing.T) {
	db := withPlan(&fakeIncDB{},
		planRow{planIndex: 0, name: "set password", module: "core.exec.run", noLog: false, passage: 0,
			params: []byte(`{"user":"admin","password":"***MASKED***"}`)},
	)
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)

	v, err := h.RunTasksTyped(context.Background(), "redis-prod", validApplyID, allowScope)
	if err != nil {
		t.Fatalf("RunTasksTyped: %v", err)
	}
	if len(v.Tasks) != 1 {
		t.Fatalf("len(Tasks) = %d, want 1", len(v.Tasks))
	}
	p := v.Tasks[0].Params
	if p == nil || p["password"] != "***MASKED***" {
		t.Errorf("params.password = %v, want ***MASKED*** (masked-значение доезжает как есть)", p["password"])
	}
	if p["user"] != "admin" {
		t.Errorf("params.user = %v, want admin (не секрет, сохранён)", p["user"])
	}
}

// TestRunTasksTyped_BrokenParamsJSON_Nil — битый jsonb в строке плана → params nil
// (best-effort: одна кривая строка не роняет весь /tasks).
func TestRunTasksTyped_BrokenParamsJSON_Nil(t *testing.T) {
	db := withPlan(&fakeIncDB{},
		planRow{planIndex: 0, name: "x", module: "core.exec.run", passage: 0, params: []byte(`{not json`)},
	)
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)

	v, err := h.RunTasksTyped(context.Background(), "redis-prod", validApplyID, allowScope)
	if err != nil {
		t.Fatalf("RunTasksTyped: %v", err)
	}
	if len(v.Tasks) != 1 || v.Tasks[0].Params != nil {
		t.Errorf("broken params → %v, want nil", v.Tasks[0].Params)
	}
}
