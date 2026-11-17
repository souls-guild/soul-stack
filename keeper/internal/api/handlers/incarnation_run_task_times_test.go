package handlers

// Guards for the task times of GET .../runs/{apply_id}/tasks (NIM-1170): how a
// task's own start and finish fold out of its hosts' rows, and that a host's row
// reaches that host and no other. The SQL that writes and settles the rows is
// covered by the applyrun integration tests.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
)

var t0 = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

func at(sec int) *time.Time {
	v := t0.Add(time.Duration(sec) * time.Second)
	return &v
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func TestTaskTimes_Fold(t *testing.T) {
	cases := []struct {
		name              string
		rows              []applyrun.RunTaskTime
		started, finished *time.Time
	}{
		{name: "no rows: never sent", rows: nil},
		{
			name:    "one host running: started, no finish",
			rows:    []applyrun.RunTaskTime{{SID: "a", StartedAt: at(5)}},
			started: at(5),
		},
		{
			name: "every host finished: first start, last finish",
			rows: []applyrun.RunTaskTime{
				{SID: "a", StartedAt: at(5), FinishedAt: at(9)},
				{SID: "b", StartedAt: at(3), FinishedAt: at(20)},
			},
			started: at(3), finished: at(20),
		},
		{
			name: "a host still running holds the finish back",
			rows: []applyrun.RunTaskTime{
				{SID: "a", StartedAt: at(5), FinishedAt: at(9)},
				{SID: "b", StartedAt: at(7)},
			},
			started: at(5),
		},
		{
			name: "a host sent the task and not at it yet holds the finish back",
			rows: []applyrun.RunTaskTime{
				{SID: "a", StartedAt: at(5), FinishedAt: at(9)},
				{SID: "b"},
			},
			started: at(5),
		},
		{
			name: "a finish without a seen start still finishes",
			rows: []applyrun.RunTaskTime{
				{SID: "a", FinishedAt: at(9)},
				{SID: "b", StartedAt: at(4), FinishedAt: at(6)},
			},
			started: at(4), finished: at(9),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			started, finished := taskTimes(tc.rows)
			if !sameTime(started, tc.started) || !sameTime(finished, tc.finished) {
				t.Errorf("taskTimes = (%v, %v), want (%v, %v)", started, finished, tc.started, tc.finished)
			}
		})
	}
}

// timeRowsStub — apply_run_plan_host_times rows in selectRunTaskTimesSQL's column
// order: plan_index/sid/started_at/finished_at.
type timeRowsStub struct {
	rows []applyrun.RunTaskTime
	idx  int
}

func (r *timeRowsStub) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}

func (r *timeRowsStub) Scan(dest ...any) error {
	row := r.rows[r.idx-1]
	*(dest[0].(*int)) = row.PlanIndex
	*(dest[1].(*string)) = row.SID
	*(dest[2].(**time.Time)) = row.StartedAt
	*(dest[3].(**time.Time)) = row.FinishedAt
	return nil
}

func (r *timeRowsStub) Err() error                                   { return nil }
func (r *timeRowsStub) Close()                                       {}
func (r *timeRowsStub) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *timeRowsStub) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *timeRowsStub) Values() ([]any, error)                       { return nil, nil }
func (r *timeRowsStub) RawValues() [][]byte                          { return nil }
func (r *timeRowsStub) Conn() *pgx.Conn                              { return nil }

func TestRunTasksTyped_Times(t *testing.T) {
	db := withPlan(&fakeIncDB{},
		planRow{planIndex: 0, name: "install", module: "core.pkg.present"},
		planRow{planIndex: 1, name: "restart", module: "core.service.restarted"},
		planRow{planIndex: 2, name: "report", module: "core.exec.run"},
	)
	db.runTaskTimeRows = func() (pgx.Rows, error) {
		return &timeRowsStub{rows: []applyrun.RunTaskTime{
			{PlanIndex: 0, SID: "a.example", StartedAt: at(0), FinishedAt: at(4)},
			{PlanIndex: 0, SID: "b.example", StartedAt: at(1), FinishedAt: at(6)},
			// b is still at it: no result in audit, so it is in hosts[] as running,
			// and its start is the task's.
			{PlanIndex: 1, SID: "a.example", StartedAt: at(4), FinishedAt: at(5)},
			{PlanIndex: 1, SID: "b.example", StartedAt: at(6)},
		}}, nil
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil)
	h.SetRunTasksAuditReader(&fakeRunTasksAudit{execs: []auditpg.TaskExecution{
		{SID: "a.example", PlanIndex: 0, Status: "TASK_STATUS_OK"},
		{SID: "b.example", PlanIndex: 0, Status: "TASK_STATUS_CHANGED"},
		{SID: "a.example", PlanIndex: 1, Status: "TASK_STATUS_OK"},
		// A result with no time row (a run older than the times) has no times.
		{SID: "a.example", PlanIndex: 2, Status: "TASK_STATUS_OK"},
	}})

	v, err := h.RunTasksTyped(context.Background(), "redis-prod", validApplyID, allowScope)
	if err != nil {
		t.Fatalf("RunTasksTyped: %v", err)
	}
	if len(v.Tasks) != 3 {
		t.Fatalf("tasks = %d, want 3", len(v.Tasks))
	}

	install := v.Tasks[0]
	if !sameTime(install.StartedAt, at(0)) || !sameTime(install.FinishedAt, at(6)) {
		t.Errorf("install = (%v, %v), want (+0s, +6s)", install.StartedAt, install.FinishedAt)
	}
	for _, hv := range install.Hosts {
		want := map[string][2]*time.Time{"a.example": {at(0), at(4)}, "b.example": {at(1), at(6)}}[hv.SID]
		if !sameTime(hv.StartedAt, want[0]) || !sameTime(hv.FinishedAt, want[1]) {
			t.Errorf("install host %s = (%v, %v), want (%v, %v)", hv.SID, hv.StartedAt, hv.FinishedAt, want[0], want[1])
		}
	}

	restart := v.Tasks[1]
	if !sameTime(restart.StartedAt, at(4)) || restart.FinishedAt != nil {
		t.Errorf("restart = (%v, %v), want (+4s, running)", restart.StartedAt, restart.FinishedAt)
	}
	if len(restart.Hosts) != 2 || restart.Hosts[0].Status != "TASK_STATUS_OK" ||
		restart.Hosts[1].SID != "b.example" || restart.Hosts[1].Status != TaskStatusRunning {
		t.Fatalf("restart hosts = %+v, want a.example's result and b.example running", restart.Hosts)
	}

	report := v.Tasks[2]
	if report.StartedAt != nil || report.FinishedAt != nil {
		t.Errorf("report = (%v, %v), want no times", report.StartedAt, report.FinishedAt)
	}
	if len(report.Hosts) != 1 || report.Hosts[0].StartedAt != nil || report.Hosts[0].FinishedAt != nil {
		t.Errorf("report hosts = %+v, want one host without times", report.Hosts)
	}
}

// NIM-1172: a host still at a task is in hosts[] as running with its start alone;
// a result beats the row it has not closed yet, and a host sent the task and not at
// it, or one whose row finished without a result, is not listed. Package api runs
// the same over the tables (TestIntegration_RunTasks_RunningHosts).
func TestRunTasksTyped_RunningHosts(t *testing.T) {
	db := withPlan(&fakeIncDB{},
		planRow{planIndex: 0, name: "install", module: "core.pkg.present"},
		planRow{planIndex: 1, name: "restart", module: "core.service.restarted"},
	)
	db.runTaskTimeRows = func() (pgx.Rows, error) {
		return &timeRowsStub{rows: []applyrun.RunTaskTime{
			{PlanIndex: 0, SID: "a.example", StartedAt: at(0), FinishedAt: at(4)},
			{PlanIndex: 0, SID: "b.example", StartedAt: at(1)},
			// The audit event lands before the TaskEvent closes the row.
			{PlanIndex: 0, SID: "c.example", StartedAt: at(2)},
			{PlanIndex: 0, SID: "d.example"},
			{PlanIndex: 0, SID: "e.example", FinishedAt: at(6)},
			// What the read makes of the row of a host whose run ended at the task.
			{PlanIndex: 0, SID: "f.example", StartedAt: at(3), FinishedAt: at(7)},
			{PlanIndex: 1, SID: "a.example"},
		}}, nil
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil)
	h.SetRunTasksAuditReader(&fakeRunTasksAudit{execs: []auditpg.TaskExecution{
		{SID: "a.example", PlanIndex: 0, Status: "TASK_STATUS_OK"},
		{SID: "c.example", PlanIndex: 0, Status: "TASK_STATUS_CHANGED", Output: map[string]any{"changed": true}},
	}})

	v, err := h.RunTasksTyped(context.Background(), "redis-prod", validApplyID, allowScope)
	if err != nil {
		t.Fatalf("RunTasksTyped: %v", err)
	}
	install := v.Tasks[0].Hosts
	if len(install) != 3 {
		t.Fatalf("install hosts = %+v, want a, b and c", install)
	}
	for i, want := range []struct {
		sid, status string
		started     *time.Time
	}{
		{"a.example", "TASK_STATUS_OK", at(0)},
		{"b.example", TaskStatusRunning, at(1)},
		{"c.example", "TASK_STATUS_CHANGED", at(2)},
	} {
		got := install[i]
		if got.SID != want.sid || got.Status != want.status || !sameTime(got.StartedAt, want.started) {
			t.Errorf("install host %d = %s %s from %v, want %s %s from %v",
				i, got.SID, got.Status, got.StartedAt, want.sid, want.status, want.started)
		}
	}
	if b := install[1]; b.Output != nil || b.Error != nil || b.FinishedAt != nil {
		t.Errorf("running host = %+v, want its start and nothing else", b)
	}
	if c := install[2]; c.Output == nil || c.FinishedAt != nil {
		t.Errorf("result over an open row = %+v, want the result's output and no finish", c)
	}
	if hosts := v.Tasks[1].Hosts; len(hosts) != 0 {
		t.Errorf("restart hosts = %+v, want none: a was sent it and is not at it yet", hosts)
	}
}

// finishingAudit and the times stub below share one host that finishes between the
// two reads: the read made first sees it at the task, the second sees it done.
type finishingAudit struct{ finished *bool }

func (a *finishingAudit) SelectTaskExecutions(context.Context, string) ([]auditpg.TaskExecution, error) {
	defer func() { *a.finished = true }()
	if *a.finished {
		return []auditpg.TaskExecution{{SID: "a.example", PlanIndex: 0, Status: "TASK_STATUS_OK"}}, nil
	}
	return nil, nil
}

// The writers record a result before they close its row, so with the times read
// first the host is running or done in every interleaving; read second, it would be
// in neither read.
func TestRunTasksTyped_HostFinishingBetweenTheReadsIsNeverMissing(t *testing.T) {
	finished := false
	db := withPlan(&fakeIncDB{}, planRow{planIndex: 0, name: "install", module: "core.pkg.present"})
	db.runTaskTimeRows = func() (pgx.Rows, error) {
		row := applyrun.RunTaskTime{PlanIndex: 0, SID: "a.example", StartedAt: at(0)}
		if finished {
			row.FinishedAt = at(4)
		}
		finished = true
		return &timeRowsStub{rows: []applyrun.RunTaskTime{row}}, nil
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil)
	h.SetRunTasksAuditReader(&finishingAudit{finished: &finished})

	v, err := h.RunTasksTyped(context.Background(), "redis-prod", validApplyID, allowScope)
	if err != nil {
		t.Fatalf("RunTasksTyped: %v", err)
	}
	if hosts := v.Tasks[0].Hosts; len(hosts) != 1 || hosts[0].SID != "a.example" {
		t.Fatalf("hosts = %+v, want a.example, running or done", hosts)
	}
}

// A failed times read costs the answer its times, not the answer.
func TestRunTasksTyped_TimesReadFails_TasksStillAnswer(t *testing.T) {
	db := withPlan(&fakeIncDB{}, planRow{planIndex: 0, name: "install", module: "core.pkg.present"})
	db.runTaskTimeRows = func() (pgx.Rows, error) { return nil, context.DeadlineExceeded }
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil)

	v, err := h.RunTasksTyped(context.Background(), "redis-prod", validApplyID, allowScope)
	if err != nil {
		t.Fatalf("RunTasksTyped: %v", err)
	}
	if len(v.Tasks) != 1 || v.Tasks[0].StartedAt != nil {
		t.Errorf("tasks = %+v, want the plan row without times", v.Tasks)
	}
}
