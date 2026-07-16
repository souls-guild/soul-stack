package applyrun

// Docker-free guard tests for the run plan task store layer (apply_run_plan, NIM-37):
// bulk-upsert form of InsertRunPlan (parallel unnest arrays), no-op/validation,
// scan of SelectRunPlanByApplyID and scope-probe RunExistsForIncarnation. Actual
// round-trip to PG is in integration_test.go; here we check SQL form, column mapping,
// and edge cases without DB.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// planRowsStub is a pgx.Rows stub over a set of apply_run_plan rows in the order of
// selectRunPlanByApplyIDSQL columns (plan_index/name/module/no_log/passage). Scan reuses
// the batch assign helper (crud_test.go).
type planRowsStub struct {
	rows [][]any
	idx  int
}

func (r *planRowsStub) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}

func (r *planRowsStub) Scan(dest ...any) error {
	row := r.rows[r.idx-1]
	if len(dest) != len(row) {
		return errors.New("planRowsStub.Scan: len mismatch")
	}
	for i, d := range dest {
		assign(d, row[i])
	}
	return nil
}

func (r *planRowsStub) Err() error                    { return nil }
func (r *planRowsStub) Close()                        {}
func (r *planRowsStub) CommandTag() pgconn.CommandTag { return pgconn.CommandTag{} }
func (r *planRowsStub) FieldDescriptions() []pgconn.FieldDescription {
	return nil
}
func (r *planRowsStub) Values() ([]any, error) { return nil, nil }
func (r *planRowsStub) RawValues() [][]byte    { return nil }
func (r *planRowsStub) Conn() *pgx.Conn        { return nil }

// TestInsertRunPlan_BulkArrays — план из двух Passage кладётся ОДНИМ bulk-upsert-ом:
// apply_id общий $1, остальные колонки — параллельные типизированные массивы в том
// же порядке, что задачи. Ловит рассинхрон порядка колонок и потерю no_log/passage.
func TestInsertRunPlan_BulkArrays(t *testing.T) {
	f := &fakeDB{}
	tasks := []RunPlanTask{
		{ApplyID: "AP", PlanIndex: 0, Name: "install redis", Module: "core.pkg.installed", NoLog: false, Passage: 0, Params: []byte(`{"name":"redis"}`)},
		{ApplyID: "AP", PlanIndex: 1, Name: "set password", Module: "core.exec.run", NoLog: true, Passage: 0, Params: nil},
		{ApplyID: "AP", PlanIndex: 2, Name: "restart", Module: "core.service.running", NoLog: false, Passage: 1, Params: []byte(`{"unit":"redis"}`)},
	}
	if err := InsertRunPlan(context.Background(), f, "AP", tasks); err != nil {
		t.Fatalf("InsertRunPlan: %v", err)
	}
	if f.execCalls != 1 {
		t.Fatalf("execCalls = %d, want 1 (единственный bulk-upsert)", f.execCalls)
	}
	if !strings.Contains(f.execSQL, "INSERT INTO apply_run_plan") {
		t.Errorf("SQL не INSERT INTO apply_run_plan: %q", f.execSQL)
	}
	if !strings.Contains(f.execSQL, "ON CONFLICT (apply_id, plan_index)") {
		t.Errorf("SQL без идемпотентного ON CONFLICT (apply_id, plan_index): %q", f.execSQL)
	}
	if !strings.Contains(f.execSQL, "params") || !strings.Contains(f.execSQL, "::jsonb") {
		t.Errorf("SQL без params::jsonb: %q", f.execSQL)
	}
	// args: $1 apply_id, $2 plan_index[], $3 name[], $4 module[], $5 no_log[], $6 passage[], $7 params[].
	if len(f.execArgs) != 7 {
		t.Fatalf("execArgs len = %d, want 7", len(f.execArgs))
	}
	if f.execArgs[0] != "AP" {
		t.Errorf("args[0] apply_id = %v, want AP", f.execArgs[0])
	}
	planIdx, ok := f.execArgs[1].([]int)
	if !ok || len(planIdx) != 3 || planIdx[0] != 0 || planIdx[2] != 2 {
		t.Errorf("args[1] plan_index[] = %v, want [0 1 2]", f.execArgs[1])
	}
	names, ok := f.execArgs[2].([]string)
	if !ok || names[1] != "set password" {
		t.Errorf("args[2] name[] = %v, want [...,'set password',...]", f.execArgs[2])
	}
	modules, ok := f.execArgs[3].([]string)
	if !ok || modules[2] != "core.service.running" {
		t.Errorf("args[3] module[] = %v", f.execArgs[3])
	}
	noLogs, ok := f.execArgs[4].([]bool)
	if !ok || noLogs[0] != false || noLogs[1] != true || noLogs[2] != false {
		t.Errorf("args[4] no_log[] = %v, want [false true false]", f.execArgs[4])
	}
	passages, ok := f.execArgs[5].([]int)
	if !ok || passages[2] != 1 {
		t.Errorf("args[5] passage[] = %v, want [0 0 1]", f.execArgs[5])
	}
	// params[] — параллельный *string-массив: JSON или nil (NULL) на позицию.
	params, ok := f.execArgs[6].([]*string)
	if !ok || len(params) != 3 {
		t.Fatalf("args[6] params[] = %v, want []*string len 3", f.execArgs[6])
	}
	if params[0] == nil || *params[0] != `{"name":"redis"}` {
		t.Errorf("params[0] = %v, want {\"name\":\"redis\"}", params[0])
	}
	if params[1] != nil {
		t.Errorf("params[1] = %v, want nil (no_log → NULL)", *params[1])
	}
	if params[2] == nil || *params[2] != `{"unit":"redis"}` {
		t.Errorf("params[2] = %v, want {\"unit\":\"redis\"}", params[2])
	}
}

// TestInsertRunPlan_EmptyApplyID tests that empty apply_id is rejected BEFORE DB (error, zero Exec).
func TestInsertRunPlan_EmptyApplyID(t *testing.T) {
	f := &fakeDB{}
	err := InsertRunPlan(context.Background(), f, "", []RunPlanTask{{PlanIndex: 0, Name: "x"}})
	if err == nil {
		t.Fatal("empty apply_id: ожидалась ошибка")
	}
	if f.execCalls != 0 {
		t.Errorf("execCalls = %d, want 0 (don't go to DB)", f.execCalls)
	}
}

// TestInsertRunPlan_EmptyTasks_Noop — пустой план = no-op (nil error, ноль Exec):
// прогон упал до render / keeper-only без задач не должен ронять InsertRunPlan.
func TestInsertRunPlan_EmptyTasks_Noop(t *testing.T) {
	f := &fakeDB{}
	if err := InsertRunPlan(context.Background(), f, "AP", nil); err != nil {
		t.Fatalf("empty tasks: want nil, got %v", err)
	}
	if f.execCalls != 0 {
		t.Errorf("execCalls = %d, want 0 (нечего писать)", f.execCalls)
	}
}

// TestSelectRunPlanByApplyID_Scan — маппинг колонок plan_index/name/module/no_log/
// passage/params → RunPlanTask (ApplyID проставляется caller-ом из аргумента).
// params: строка 0 несёт jsonb → []byte, строка 1 (no_log) — NULL → nil.
func TestSelectRunPlanByApplyID_Scan(t *testing.T) {
	f := &fakeDB{
		queryFunc: func(_ string) (pgx.Rows, error) {
			return &planRowsStub{rows: [][]any{
				{0, "install", "core.pkg.installed", false, 0, []byte(`{"name":"redis"}`)},
				{1, "secret", "core.exec.run", true, 1, nil},
			}}, nil
		},
	}
	got, err := SelectRunPlanByApplyID(context.Background(), f, "AP")
	if err != nil {
		t.Fatalf("SelectRunPlanByApplyID: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].ApplyID != "AP" || got[0].PlanIndex != 0 || got[0].Module != "core.pkg.installed" || got[0].NoLog {
		t.Errorf("row0 = %+v", got[0])
	}
	if string(got[0].Params) != `{"name":"redis"}` {
		t.Errorf("row0 Params = %s, want {\"name\":\"redis\"}", got[0].Params)
	}
	if got[1].NoLog != true || got[1].Passage != 1 || got[1].Name != "secret" {
		t.Errorf("row1 = %+v", got[1])
	}
	if got[1].Params != nil {
		t.Errorf("row1 Params = %s, want nil (NULL)", got[1].Params)
	}
}

// TestSelectRunPlanByApplyID_QueryShape — стабильный ORDER BY plan_index + bind $1.
func TestSelectRunPlanByApplyID_QueryShape(t *testing.T) {
	f := &fakeDB{
		queryFunc: func(_ string) (pgx.Rows, error) { return nil, errors.New("stop after capture") },
	}
	_, _ = SelectRunPlanByApplyID(context.Background(), f, "AP")
	if !strings.Contains(f.querySQL, "FROM apply_run_plan") {
		t.Errorf("SQL не из apply_run_plan: %q", f.querySQL)
	}
	if !strings.Contains(f.querySQL, "ORDER BY plan_index ASC") {
		t.Errorf("SQL без ORDER BY plan_index ASC: %q", f.querySQL)
	}
	if len(f.queryArgs) != 1 || f.queryArgs[0] != "AP" {
		t.Errorf("queryArgs = %v, want [AP]", f.queryArgs)
	}
}

// TestRunExistsForIncarnation tests scope-probe of run ownership to incarnation:
// EXISTS true/false пробрасывается; ErrNoRows → (false, nil) (не ошибка).
func TestRunExistsForIncarnation(t *testing.T) {
	cases := []struct {
		name string
		row  pgx.Row
		want bool
		err  bool
	}{
		{"exists", staticRow{values: []any{true}}, true, false},
		{"absent", staticRow{values: []any{false}}, false, false},
		{"no rows → false", errRow{err: pgx.ErrNoRows}, false, false},
		{"db error → err", errRow{err: errors.New("boom")}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeDB{queryRowFunc: func(string) pgx.Row { return tc.row }}
			got, err := RunExistsForIncarnation(context.Background(), f, "AP", "redis-prod")
			if tc.err {
				if err == nil {
					t.Fatal("ожидалась ошибка")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
