package handlers

// Guard tests for the run task read-view at the handler boundary (RunTasksTyped, NIM-37):
// input (bad name → 422, bad apply_id → 400), the scope gate (deny → 404, an apply_id
// that isn't ours = EXISTS false → 404), and the core plan-join logic (apply_run_plan) with
// per-host results from audit (task.executed): grouping by plan_index→sid, last-wins on
// retry, sorting by sid, declared-secret output arriving already masked ([ADR-0083] §8),
// a pending host (present in the plan but without an audit result) excluded. The real SQL scan of the plan/audit is covered in the
// integration store tests; here we test the handler layer: the domain function + join + inScope.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
)

// planRow — a single apply_run_plan row (column order from selectRunPlanByApplyIDSQL:
// plan_index/name/module/passage/params/waves).
type planRow struct {
	planIndex int
	name      string
	module    string
	passage   int
	params    []byte
	waves     []byte
}

// runPlanRowsStub — a pgx.Rows stub over a set of run-plan rows.
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
	*(dest[3].(*int)) = row.passage
	*(dest[4].(*[]byte)) = row.params
	*(dest[5].(*[]byte)) = row.waves
	return nil
}

func (r *runPlanRowsStub) Err() error                                   { return nil }
func (r *runPlanRowsStub) Close()                                       {}
func (r *runPlanRowsStub) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *runPlanRowsStub) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *runPlanRowsStub) Values() ([]any, error)                       { return nil, nil }
func (r *runPlanRowsStub) RawValues() [][]byte                          { return nil }
func (r *runPlanRowsStub) Conn() *pgx.Conn                              { return nil }

// fakeRunTasksAudit — a RunTaskAuditReader fake: returns a predefined set of execs
// (in chronological order, like the real SelectTaskExecutions — the later one wins).
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

// --- input + scope gate --------------------------------------------------

func TestRunTasksTyped_BadName_422(t *testing.T) {
	h := NewIncarnationHandler(&fakeIncDB{}, nil, nil, nil, nil, nil, nil, nil)
	_, err := h.RunTasksTyped(context.Background(), "Bad_Name", validApplyID, allowScope)
	requireProblemStatus(t, err, 422)
}

func TestRunTasksTyped_BadApplyID_400(t *testing.T) {
	h := NewIncarnationHandler(&fakeIncDB{}, nil, nil, nil, nil, nil, nil, nil)
	_, err := h.RunTasksTyped(context.Background(), "redis-prod", "not-a-ulid", allowScope)
	requireProblemStatus(t, err, 400)
}

// TestRunTasksTyped_OutOfScope_404 — the incarnation exists, but inScope denies → 404
// (we don't leak existence), the plan store is not touched.
func TestRunTasksTyped_OutOfScope_404(t *testing.T) {
	db := &fakeIncDB{selectByNameRow: func(name string) pgx.Row { return makeIncarnationRow(name) }}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil)
	_, err := h.RunTasksTyped(context.Background(), "redis-prod", validApplyID, denyScope)
	requireProblemStatus(t, err, 404)
}

// TestRunTasksTyped_ForeignApplyID_404 — the incarnation is in scope, but the apply_id
// does NOT belong to it (EXISTS probe → false): 404. Isolation of a "foreign run" at the
// handler boundary, before the plan is read.
func TestRunTasksTyped_ForeignApplyID_404(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncarnationRow(name) },
		runExistsRow:    func(string, string) pgx.Row { return staticRow{values: []any{false}} },
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil)
	_, err := h.RunTasksTyped(context.Background(), "redis-prod", validApplyID, allowScope)
	requireProblemStatus(t, err, 404)
}

// TestRunTasksTyped_EmptyPlan_OK — the run belongs to the incarnation, but there's no plan
// (failed before render / legacy): success, tasks is empty (not an error).
func TestRunTasksTyped_EmptyPlan_OK(t *testing.T) {
	db := withPlan(&fakeIncDB{}) // no plan rows
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil)
	v, err := h.RunTasksTyped(context.Background(), "redis-prod", validApplyID, allowScope)
	if err != nil {
		t.Fatalf("RunTasksTyped: %v", err)
	}
	if len(v.Tasks) != 0 {
		t.Errorf("len(Tasks) = %d, want 0", len(v.Tasks))
	}
}

// TestRunTasksTyped_PlanAuditJoin — the central guard: a 3-task plan is joined with
// audit results by plan_index→sid. Checks last-wins (retry on host-a), sorting hosts by
// sid, a declared-secret output field arriving already masked ([ADR-0083] §8), a pending
// task without audit (hosts empty), and that masked params (S1b) are deserialized from
// jsonb into an object.
func TestRunTasksTyped_PlanAuditJoin(t *testing.T) {
	db := withPlan(&fakeIncDB{},
		planRow{planIndex: 0, name: "install", module: "core.pkg.installed", passage: 0, params: []byte(`{"name":"redis","state":"present"}`)},
		planRow{planIndex: 1, name: "read credential", module: "core.vault.kv-read", passage: 0, params: []byte(`{"path":"secret/app/cfg"}`)},
		planRow{planIndex: 2, name: "restart", module: "core.service.running", passage: 1, params: []byte(`{"unit":"redis"}`)},
	)
	// execs are ordered chronologically: for (idx0, host-a) the first OK is overwritten
	// by the later FAILED (retry, last-wins). idx1 returns a declared-secret field, masked
	// on the write path. idx2 has no execs → pending (hosts empty).
	audit := &fakeRunTasksAudit{execs: []auditpg.TaskExecution{
		{SID: "host-b", PlanIndex: 0, Status: "TASK_STATUS_CHANGED", Output: map[string]any{"changed": true}},
		{SID: "host-a", PlanIndex: 0, Status: "TASK_STATUS_OK", Output: map[string]any{"first": true}},
		{SID: "host-a", PlanIndex: 0, Status: "TASK_STATUS_FAILED",
			Error: &auditpg.TaskExecutionError{Code: "E_APPLY", Module: "core.pkg.installed", Message: "boom"}},
		{SID: "host-a", PlanIndex: 1, Status: "TASK_STATUS_OK",
			Output: map[string]any{"data": "***MASKED***", "path": "secret/app/cfg"}},
	}}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil)
	h.SetRunTasksAuditReader(audit)

	v, err := h.RunTasksTyped(context.Background(), "redis-prod", validApplyID, allowScope)
	if err != nil {
		t.Fatalf("RunTasksTyped: %v", err)
	}
	if len(v.Tasks) != 3 {
		t.Fatalf("len(Tasks) = %d, want 3", len(v.Tasks))
	}

	// task[0]: 2 hosts, sorted by sid (host-a, host-b); host-a=last FAILED.
	t0 := v.Tasks[0]
	if t0.PlanIndex != 0 || t0.Name != "install" {
		t.Errorf("task0 header = %+v", t0)
	}
	// S1b: masked params from jsonb are deserialized into an object.
	if t0.Params == nil || t0.Params["name"] != "redis" || t0.Params["state"] != "present" {
		t.Errorf("task0.Params = %v, want {name:redis, state:present}", t0.Params)
	}
	if len(t0.Hosts) != 2 {
		t.Fatalf("task0 hosts = %d, want 2", len(t0.Hosts))
	}
	if t0.Hosts[0].SID != "host-a" || t0.Hosts[1].SID != "host-b" {
		t.Errorf("task0 hosts not sorted by sid: %q,%q", t0.Hosts[0].SID, t0.Hosts[1].SID)
	}
	ha := t0.Hosts[0]
	if ha.Status != "TASK_STATUS_FAILED" {
		t.Errorf("task0 host-a status = %q, want FAILED (last-wins retry)", ha.Status)
	}
	if ha.Error == nil || ha.Error.Code != "E_APPLY" || ha.Error.Message != "boom" {
		t.Errorf("task0 host-a error = %+v, want {E_APPLY,...,boom}", ha.Error)
	}
	if ha.Output != nil {
		t.Errorf("task0 host-a output = %v, want nil (last exec without register_data)", ha.Output)
	}
	hb := t0.Hosts[1]
	if hb.Status != "TASK_STATUS_CHANGED" || hb.Error != nil {
		t.Errorf("task0 host-b = {%q, err=%v}, want {CHANGED, nil}", hb.Status, hb.Error)
	}
	if hb.Output == nil || hb.Output["changed"] != true {
		t.Errorf("task0 host-b output = %v, want {changed:true}", hb.Output)
	}

	// task[1]: a secret-carrying task is fully visible — [ADR-0083] §8 masks the declared
	// field and nothing else. Before §8 this row had NULL params and a nil output, which
	// cost the operator the whole step to hide one value of it.
	t1 := v.Tasks[1]
	if t1.PlanIndex != 1 || t1.Name != "read credential" {
		t.Errorf("task1 header = %+v, want plan_index=1 name=read credential", t1)
	}
	if t1.Params == nil || t1.Params["path"] != "secret/app/cfg" {
		t.Errorf("task1.Params = %v, want {path:secret/app/cfg} (no blanket suppression since §8)", t1.Params)
	}
	if len(t1.Hosts) != 1 || t1.Hosts[0].SID != "host-a" {
		t.Fatalf("task1 hosts = %+v, want [host-a]", t1.Hosts)
	}
	out := t1.Hosts[0].Output
	if out == nil {
		t.Fatalf("task1 host output = nil, want the masked output object")
	}
	if out["data"] != "***MASKED***" {
		t.Errorf("task1 host output.data = %v, want the mask (write-path §8 redaction)", out["data"])
	}
	if out["path"] != "secret/app/cfg" {
		t.Errorf("task1 host output.path = %v, want the non-secret field intact", out["path"])
	}

	// task[2]: pending — present in the plan, but no audit result → hosts empty.
	t2 := v.Tasks[2]
	if t2.PlanIndex != 2 {
		t.Errorf("task2 plan_index = %d, want 2", t2.PlanIndex)
	}
	if len(t2.Hosts) != 0 {
		t.Errorf("task2 (pending) hosts = %d, want 0 (no audit result)", len(t2.Hosts))
	}
	if t2.Params == nil || t2.Params["unit"] != "redis" {
		t.Errorf("task2.Params = %v, want {unit:redis}", t2.Params)
	}
}

// TestRunTasksTyped_MaskedParamsFlowThrough — a masked params value (masked on the
// write path in persistRunPlan) reaches the DTO AS IS: the handler does not unmask it
// and does not leak plaintext. Guard for the read side: "/tasks does not reveal the secret" —
// the plan row carries already-masked jsonb, only ***MASKED*** appears in the response.
func TestRunTasksTyped_MaskedParamsFlowThrough(t *testing.T) {
	db := withPlan(&fakeIncDB{},
		planRow{planIndex: 0, name: "set password", module: "core.exec.run", passage: 0,
			params: []byte(`{"user":"admin","password":"***MASKED***"}`)},
	)
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil)

	v, err := h.RunTasksTyped(context.Background(), "redis-prod", validApplyID, allowScope)
	if err != nil {
		t.Fatalf("RunTasksTyped: %v", err)
	}
	if len(v.Tasks) != 1 {
		t.Fatalf("len(Tasks) = %d, want 1", len(v.Tasks))
	}
	p := v.Tasks[0].Params
	if p == nil || p["password"] != "***MASKED***" {
		t.Errorf("params.password = %v, want ***MASKED*** (masked value passes through as is)", p["password"])
	}
	if p["user"] != "admin" {
		t.Errorf("params.user = %v, want admin (not a secret, preserved)", p["user"])
	}
}

// TestRunTasksTyped_BrokenParamsJSON_Nil — broken jsonb in a plan row → params nil
// (best-effort: one bad row doesn't bring down the whole /tasks).
func TestRunTasksTyped_BrokenParamsJSON_Nil(t *testing.T) {
	db := withPlan(&fakeIncDB{},
		planRow{planIndex: 0, name: "x", module: "core.exec.run", passage: 0, params: []byte(`{not json`)},
	)
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil)

	v, err := h.RunTasksTyped(context.Background(), "redis-prod", validApplyID, allowScope)
	if err != nil {
		t.Fatalf("RunTasksTyped: %v", err)
	}
	if len(v.Tasks) != 1 || v.Tasks[0].Params != nil {
		t.Errorf("broken params → %v, want nil", v.Tasks[0].Params)
	}
}

// TestRunTasksTyped_Waves — a serial: step's stored waves reach every task of the step
// in dispatch order, the order the web reads the rollout in (NIM-996). A task outside
// the step, a malformed column and a lone wave carry none: the view then shows a flat
// task, never a broken one.
func TestRunTasksTyped_Waves(t *testing.T) {
	waves := []byte(`[["b.example.com"],["a.example.com"],["c.example.com"]]`)
	db := withPlan(&fakeIncDB{},
		planRow{planIndex: 0, name: "probe", module: "core.exec.run", passage: 0},
		planRow{planIndex: 1, name: "upgrade", module: "core.exec.run", passage: 1, waves: waves},
		planRow{planIndex: 2, name: "check", module: "core.exec.run", passage: 1, waves: waves},
		planRow{planIndex: 3, name: "broken", module: "core.exec.run", passage: 2, waves: []byte(`[["a"`)},
		planRow{planIndex: 4, name: "lone", module: "core.exec.run", passage: 3, waves: []byte(`[["a.example.com"]]`)},
	)
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil)

	v, err := h.RunTasksTyped(context.Background(), "redis-prod", validApplyID, allowScope)
	if err != nil {
		t.Fatalf("RunTasksTyped: %v", err)
	}
	want := [][]string{{"b.example.com"}, {"a.example.com"}, {"c.example.com"}}
	for _, i := range []int{1, 2} {
		if !reflect.DeepEqual(v.Tasks[i].Waves, want) {
			t.Errorf("task %d waves = %v, want %v (dispatch order kept, not re-sorted)", i, v.Tasks[i].Waves, want)
		}
	}
	for _, i := range []int{0, 3, 4} {
		if v.Tasks[i].Waves != nil {
			t.Errorf("task %d (%s) waves = %v, want nil", i, v.Tasks[i].Name, v.Tasks[i].Waves)
		}
	}
}

// hostParamRow — one apply_run_plan_host_params row (plan_index/sid/params).
type hostParamRow struct {
	planIndex int
	sid       string
	params    []byte
}

type hostParamRowsStub struct {
	rows []hostParamRow
	idx  int
}

func (r *hostParamRowsStub) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}

func (r *hostParamRowsStub) Scan(dest ...any) error {
	row := r.rows[r.idx-1]
	*(dest[0].(*int)) = row.planIndex
	*(dest[1].(*string)) = row.sid
	*(dest[2].(*[]byte)) = row.params
	return nil
}

func (r *hostParamRowsStub) Err() error                                   { return nil }
func (r *hostParamRowsStub) Close()                                       {}
func (r *hostParamRowsStub) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *hostParamRowsStub) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *hostParamRowsStub) Values() ([]any, error)                       { return nil, nil }
func (r *hostParamRowsStub) RawValues() [][]byte                          { return nil }
func (r *hostParamRowsStub) Conn() *pgx.Conn                              { return nil }

// NIM-985: a task with per-host rows answers params_by_host beside the marked
// params; a task without them answers params alone; a row that does not decode
// costs its own task the view and nothing else.
func TestRunTasksTyped_ParamsByHost(t *testing.T) {
	db := withPlan(&fakeIncDB{},
		planRow{planIndex: 0, name: "echo", module: "core.exec.run", params: []byte(`{"cmd":"<per-host>","port":"6379"}`)},
		planRow{planIndex: 1, name: "plain", module: "core.exec.run", params: []byte(`{"cmd":"true"}`)},
		planRow{planIndex: 2, name: "broken", module: "core.exec.run", params: []byte(`{"cmd":"<per-host>"}`)},
	)
	db.runPlanHostRows = func() (pgx.Rows, error) {
		return &hostParamRowsStub{rows: []hostParamRow{
			{planIndex: 0, sid: "host-a", params: []byte(`{"cmd":"echo a"}`)},
			{planIndex: 0, sid: "host-b", params: []byte(`{"cmd":"echo b"}`)},
			{planIndex: 2, sid: "host-a", params: []byte(`{`)},
		}}, nil
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil)

	v, err := h.RunTasksTyped(context.Background(), "redis-prod", validApplyID, allowScope)
	if err != nil {
		t.Fatalf("RunTasksTyped: %v", err)
	}
	t0 := v.Tasks[0]
	if t0.Params["cmd"] != "<per-host>" || t0.ParamsByHost == nil || t0.ParamsByHost.Hosts != 2 {
		t.Fatalf("task0 = params %v, by host %+v; want the marker and a two-host view", t0.Params, t0.ParamsByHost)
	}
	var cmd, port []string
	for _, c := range t0.ParamsByHost.Cells {
		for _, val := range c.Values {
			entry := fmt.Sprintf("%v@%v", val.Value, val.SIDs)
			switch c.Path {
			case "cmd":
				cmd = append(cmd, entry)
			case "port":
				port = append(port, entry)
			}
		}
	}
	if want := []string{"echo a@[host-a]", "echo b@[host-b]"}; !reflect.DeepEqual(cmd, want) {
		t.Errorf("cmd values = %v, want %v", cmd, want)
	}
	if want := []string{"6379@[]"}; !reflect.DeepEqual(port, want) {
		t.Errorf("port values = %v, want %v (shared, no SIDs)", port, want)
	}
	if v.Tasks[1].ParamsByHost != nil {
		t.Errorf("task1 without rows has a per-host view: %+v", v.Tasks[1].ParamsByHost)
	}
	if v.Tasks[2].ParamsByHost != nil || v.Tasks[2].Params["cmd"] != "<per-host>" {
		t.Errorf("task2 with an undecodable row = %+v, want the plan row only", v.Tasks[2])
	}
}

// A failed per-host select degrades the reply to the plan rows; it is not a 500.
func TestRunTasksTyped_ParamsByHostSelectErrorDegrades(t *testing.T) {
	db := withPlan(&fakeIncDB{},
		planRow{planIndex: 0, name: "echo", module: "core.exec.run", params: []byte(`{"cmd":"<per-host>"}`)},
	)
	db.runPlanHostRows = func() (pgx.Rows, error) { return nil, errors.New("relation does not exist") }
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil)

	v, err := h.RunTasksTyped(context.Background(), "redis-prod", validApplyID, allowScope)
	if err != nil {
		t.Fatalf("RunTasksTyped: %v, want the plan rows without the per-host view", err)
	}
	if len(v.Tasks) != 1 || v.Tasks[0].ParamsByHost != nil || v.Tasks[0].Params["cmd"] != "<per-host>" {
		t.Errorf("tasks = %+v", v.Tasks)
	}
}

// ★ One read of /tasks draws every task's line diffs from ONE budget, so a run
// with many long per-host texts costs a bounded amount per poll. Two identical
// tasks on a budget that fits one: the first is split into hunks, the second is
// shown whole.
// Mutation: create the budget inside the task loop → both are split.
func TestRunTasksTyped_ParamsByHostDiffBudgetIsPerRead(t *testing.T) {
	text := func(own string) []byte {
		b, _ := json.Marshal(map[string]any{"content": "a\nb\n" + own + "\nc"})
		return b
	}
	db := withPlan(&fakeIncDB{},
		planRow{planIndex: 0, name: "one", module: "core.file.present", params: []byte(`{"content":"<per-host>"}`)},
		planRow{planIndex: 1, name: "two", module: "core.file.present", params: []byte(`{"content":"<per-host>"}`)},
	)
	db.runPlanHostRows = func() (pgx.Rows, error) {
		return &hostParamRowsStub{rows: []hostParamRow{
			{planIndex: 0, sid: "host-a", params: text("x")},
			{planIndex: 0, sid: "host-b", params: text("y")},
			{planIndex: 1, sid: "host-a", params: text("x")},
			{planIndex: 1, sid: "host-b", params: text("y")},
		}}, nil
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil)

	h.runTasksDiffBudget = 1 << 30
	v, err := h.RunTasksTyped(context.Background(), "redis-prod", validApplyID, allowScope)
	if err != nil || len(v.Tasks[0].ParamsByHost.Texts) != 1 || len(v.Tasks[1].ParamsByHost.Texts) != 1 {
		t.Fatalf("on an ample budget both tasks must split into hunks: %+v, %v", v.Tasks, err)
	}
	one := applyrun.RequestDiffBudget
	if _, err := applyrun.GroupParamsByHostWithin([]byte(`{"content":"<per-host>"}`), []applyrun.RunPlanHostParams{
		{SID: "host-a", Params: text("x")}, {SID: "host-b", Params: text("y")},
	}, &one); err != nil {
		t.Fatal(err)
	}
	cost := applyrun.RequestDiffBudget - one

	h.runTasksDiffBudget = cost + cost/2
	v, err = h.RunTasksTyped(context.Background(), "redis-prod", validApplyID, allowScope)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Tasks[0].ParamsByHost.Texts) != 1 || len(v.Tasks[1].ParamsByHost.Texts) != 0 {
		t.Errorf("texts = %d / %d, want 1 / 0 - the second task must find the read's budget spent",
			len(v.Tasks[0].ParamsByHost.Texts), len(v.Tasks[1].ParamsByHost.Texts))
	}
}
