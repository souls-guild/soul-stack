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
			// b is still at it: no result in audit, so not in hosts[], but its
			// start is the task's.
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
	if len(restart.Hosts) != 1 || restart.Hosts[0].SID != "a.example" {
		t.Fatalf("restart hosts = %+v, want only a.example (b has no result yet)", restart.Hosts)
	}

	report := v.Tasks[2]
	if report.StartedAt != nil || report.FinishedAt != nil {
		t.Errorf("report = (%v, %v), want no times", report.StartedAt, report.FinishedAt)
	}
	if len(report.Hosts) != 1 || report.Hosts[0].StartedAt != nil || report.Hosts[0].FinishedAt != nil {
		t.Errorf("report hosts = %+v, want one host without times", report.Hosts)
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
