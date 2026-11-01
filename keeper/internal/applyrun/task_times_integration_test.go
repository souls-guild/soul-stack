//go:build integration

package applyrun

import (
	"context"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// NIM-1170 (migration 137): the time rows a host's send and its TaskEvents write,
// and how the end of the host's run settles the rows no TaskEvent closed.

type timeState struct {
	started, finished bool
}

func readTimes(t *testing.T, applyID string) map[int]RunTaskTime {
	t.Helper()
	rows, err := SelectRunTaskTimes(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectRunTaskTimes: %v", err)
	}
	out := map[int]RunTaskTime{}
	for _, r := range rows {
		if _, dup := out[r.PlanIndex]; dup {
			t.Fatalf("task %d read twice for one host: %+v", r.PlanIndex, rows)
		}
		out[r.PlanIndex] = r
	}
	return out
}

func requireTimes(t *testing.T, step, applyID string, want map[int]timeState) map[int]RunTaskTime {
	t.Helper()
	got := readTimes(t, applyID)
	if len(got) != len(want) {
		t.Fatalf("%s: rows = %+v, want plan indexes %v", step, got, want)
	}
	for idx, w := range want {
		r, ok := got[idx]
		if !ok {
			t.Fatalf("%s: no row for task %d", step, idx)
		}
		if (r.StartedAt != nil) != w.started || (r.FinishedAt != nil) != w.finished {
			t.Errorf("%s: task %d started=%v finished=%v, want started=%v finished=%v",
				step, idx, r.StartedAt != nil, r.FinishedAt != nil, w.started, w.finished)
		}
	}
	return got
}

func seedTimedRun(t *testing.T, sid string, tasks int) string {
	t.Helper()
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()
	applyID := audit.NewULID()
	plan := make([]RunPlanTask, tasks)
	for i := range plan {
		plan[i] = RunPlanTask{PlanIndex: i, Name: "t", Module: "core.exec.run"}
	}
	if err := InsertRunPlan(ctx, integrationPool, applyID, plan); err != nil {
		t.Fatalf("InsertRunPlan: %v", err)
	}
	if err := Insert(ctx, integrationPool, &ApplyRun{
		ApplyID: applyID, SID: sid, IncarnationName: "redis-prod", Scenario: "create", Status: StatusRunning,
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	return applyID
}

func TestIntegration_RunTaskTimes_SendAndEvents(t *testing.T) {
	ctx := context.Background()
	const sid = "a.example.com"
	applyID := seedTimedRun(t, sid, 4)

	// Sent tasks 0-2 in any order; task 3 is not this host's.
	if err := StartHostTasks(ctx, integrationPool, applyID, sid, []int{2, 0, 1}); err != nil {
		t.Fatalf("StartHostTasks: %v", err)
	}
	requireTimes(t, "sent", applyID, map[int]timeState{0: {started: true}, 1: {}, 2: {}})

	if err := FinishHostTask(ctx, integrationPool, applyID, sid, 0, true); err != nil {
		t.Fatalf("FinishHostTask(0): %v", err)
	}
	got := requireTimes(t, "task 0 reported", applyID, map[int]timeState{0: {true, true}, 1: {started: true}, 2: {}})
	if !got[1].StartedAt.Equal(*got[0].FinishedAt) {
		t.Errorf("task 1 started %v, want the moment task 0 finished %v", got[1].StartedAt, got[0].FinishedAt)
	}

	// A duplicate TaskEvent, and the record of a task the host was never sent, start
	// nothing the host has not reached.
	if err := FinishHostTask(ctx, integrationPool, applyID, sid, 0, true); err != nil {
		t.Fatalf("FinishHostTask(0) again: %v", err)
	}
	if err := FinishHostTask(ctx, integrationPool, applyID, sid, 3, true); err != nil {
		t.Fatalf("FinishHostTask(3): %v", err)
	}
	again := requireTimes(t, "duplicate and unsent", applyID, map[int]timeState{0: {true, true}, 1: {started: true}, 2: {}})
	if !again[0].FinishedAt.Equal(*got[0].FinishedAt) {
		t.Errorf("a duplicate TaskEvent moved task 0's finish from %v to %v", got[0].FinishedAt, again[0].FinishedAt)
	}

	for _, idx := range []int{1, 2} {
		if err := FinishHostTask(ctx, integrationPool, applyID, sid, idx, true); err != nil {
			t.Fatalf("FinishHostTask(%d): %v", idx, err)
		}
	}
	requireTimes(t, "all reported", applyID, map[int]timeState{0: {true, true}, 1: {true, true}, 2: {true, true}})

	// Sent again (a re-claimed step): the host runs them anew.
	if err := StartHostTasks(ctx, integrationPool, applyID, sid, []int{0, 1, 2}); err != nil {
		t.Fatalf("StartHostTasks again: %v", err)
	}
	requireTimes(t, "re-sent", applyID, map[int]timeState{0: {started: true}, 1: {}, 2: {}})

	// The send failed: the run counts the host as not sent.
	if err := ForgetHostTasks(ctx, integrationPool, applyID, sid, []int{0, 1, 2}); err != nil {
		t.Fatalf("ForgetHostTasks: %v", err)
	}
	requireTimes(t, "send failed", applyID, map[int]timeState{})
}

// An `async:` task reports when it ends, the host's flow already further on: it
// starts nothing. Sent [0 async, 1, 2, 3, 4]: 1 reports first and
// starts 2; 0 reports while 2 runs and must not start 3; 2 then starts 3.
func TestIntegration_RunTaskTimes_LateAsyncResultStartsNothing(t *testing.T) {
	ctx := context.Background()
	const sid = "a.example.com"
	applyID := seedTimedRun(t, sid, 5)

	if err := StartHostTasks(ctx, integrationPool, applyID, sid, []int{0, 1, 2, 3, 4}); err != nil {
		t.Fatalf("StartHostTasks: %v", err)
	}
	finish := func(idx int) {
		t.Helper()
		if err := FinishHostTask(ctx, integrationPool, applyID, sid, idx, true); err != nil {
			t.Fatalf("FinishHostTask(%d): %v", idx, err)
		}
	}
	finish(1)
	requireTimes(t, "1 reported", applyID, map[int]timeState{0: {started: true}, 1: {finished: true}, 2: {started: true}, 3: {}, 4: {}})
	finish(0)
	requireTimes(t, "async 0 reported late", applyID, map[int]timeState{0: {true, true}, 1: {finished: true}, 2: {started: true}, 3: {}, 4: {}})
	finish(2)
	requireTimes(t, "2 reported", applyID, map[int]timeState{0: {true, true}, 1: {finished: true}, 2: {true, true}, 3: {started: true}, 4: {}})
}

// The record the Keeper writes for a task it never sent, between two it did, starts
// nothing: only a TaskEvent that finished a row moves the host on.
func TestIntegration_RunTaskTimes_UnsentRecordBetweenSentTasks(t *testing.T) {
	ctx := context.Background()
	const sid = "a.example.com"
	applyID := seedTimedRun(t, sid, 3)

	if err := StartHostTasks(ctx, integrationPool, applyID, sid, []int{0, 2}); err != nil {
		t.Fatalf("StartHostTasks: %v", err)
	}
	if err := FinishHostTask(ctx, integrationPool, applyID, sid, 1, true); err != nil {
		t.Fatalf("FinishHostTask(1): %v", err)
	}
	requireTimes(t, "record for unsent 1", applyID, map[int]timeState{0: {started: true}, 2: {}})
}

// An earlier step can leave a row that never started (its host cancelled before
// it). The next step's TaskEvents move the host through their own step's rows.
func TestIntegration_RunTaskTimes_EarlierStepLeftARowUnstarted(t *testing.T) {
	ctx := context.Background()
	const sid = "a.example.com"
	applyID := seedTimedRun(t, sid, 1)
	if err := InsertRunPlan(ctx, integrationPool, applyID, []RunPlanTask{
		{PlanIndex: 1, Name: "t", Module: "core.exec.run", Passage: 1},
		{PlanIndex: 2, Name: "t", Module: "core.exec.run", Passage: 0},
		{PlanIndex: 3, Name: "t", Module: "core.exec.run", Passage: 1},
	}); err != nil {
		t.Fatalf("InsertRunPlan: %v", err)
	}
	if err := StartHostTasks(ctx, integrationPool, applyID, sid, []int{0, 2}); err != nil {
		t.Fatalf("StartHostTasks(step 0): %v", err)
	}
	if err := FinishHostTask(ctx, integrationPool, applyID, sid, 0, false); err != nil {
		t.Fatalf("FinishHostTask(0): %v", err)
	}
	if err := StartHostTasks(ctx, integrationPool, applyID, sid, []int{1, 3}); err != nil {
		t.Fatalf("StartHostTasks(step 1): %v", err)
	}
	if err := FinishHostTask(ctx, integrationPool, applyID, sid, 1, true); err != nil {
		t.Fatalf("FinishHostTask(1): %v", err)
	}
	requireTimes(t, "step 1, task 1 reported", applyID, map[int]timeState{0: {true, true}, 2: {}, 1: {true, true}, 3: {started: true}})
}

// A staged Passage need not follow the plan order: a host is sent [0, 2], then
// [1, 3]. Task 2 of the earlier step, finished, must not stop task 1 from starting 3.
func TestIntegration_RunTaskTimes_StepsOutOfPlanOrder(t *testing.T) {
	ctx := context.Background()
	const sid = "a.example.com"
	applyID := seedTimedRun(t, sid, 1)
	if err := InsertRunPlan(ctx, integrationPool, applyID, []RunPlanTask{
		{PlanIndex: 1, Name: "t", Module: "core.exec.run", Passage: 1},
		{PlanIndex: 2, Name: "t", Module: "core.exec.run", Passage: 0},
		{PlanIndex: 3, Name: "t", Module: "core.exec.run", Passage: 1},
	}); err != nil {
		t.Fatalf("InsertRunPlan: %v", err)
	}
	finish := func(idx int) {
		t.Helper()
		if err := FinishHostTask(ctx, integrationPool, applyID, sid, idx, true); err != nil {
			t.Fatalf("FinishHostTask(%d): %v", idx, err)
		}
	}
	if err := StartHostTasks(ctx, integrationPool, applyID, sid, []int{0, 2}); err != nil {
		t.Fatalf("StartHostTasks(step 0): %v", err)
	}
	finish(0)
	finish(2)
	if err := StartHostTasks(ctx, integrationPool, applyID, sid, []int{1, 3}); err != nil {
		t.Fatalf("StartHostTasks(step 1): %v", err)
	}
	finish(1)
	requireTimes(t, "step 1, task 1 reported", applyID, map[int]timeState{0: {true, true}, 2: {true, true}, 1: {true, true}, 3: {started: true}})
}

// A cancelled task ends its host's run: nothing after it starts.
func TestIntegration_RunTaskTimes_CancelledTaskStartsNothing(t *testing.T) {
	ctx := context.Background()
	const sid = "a.example.com"
	applyID := seedTimedRun(t, sid, 3)

	if err := StartHostTasks(ctx, integrationPool, applyID, sid, []int{0, 1, 2}); err != nil {
		t.Fatalf("StartHostTasks: %v", err)
	}
	if err := FinishHostTask(ctx, integrationPool, applyID, sid, 0, false); err != nil {
		t.Fatalf("FinishHostTask: %v", err)
	}
	requireTimes(t, "cancelled", applyID, map[int]timeState{0: {true, true}, 1: {}, 2: {}})
}

// The settling reads the host's row of the task's own step: a host whose first step
// ended is still running the next, and its rows there read as stamped.
func TestIntegration_RunTaskTimes_SettledByTheTasksOwnStep(t *testing.T) {
	ctx := context.Background()
	const sid = "a.example.com"
	applyID := seedTimedRun(t, sid, 1)
	if err := InsertRunPlan(ctx, integrationPool, applyID, []RunPlanTask{
		{PlanIndex: 1, Name: "t", Module: "core.exec.run", Passage: 1},
		{PlanIndex: 2, Name: "t", Module: "core.exec.run", Passage: 1},
	}); err != nil {
		t.Fatalf("InsertRunPlan: %v", err)
	}
	if err := StartHostTasks(ctx, integrationPool, applyID, sid, []int{0}); err != nil {
		t.Fatalf("StartHostTasks(step 0): %v", err)
	}
	if err := FinishHostTask(ctx, integrationPool, applyID, sid, 0, true); err != nil {
		t.Fatalf("FinishHostTask(0): %v", err)
	}
	if err := UpdateStatus(ctx, integrationPool, applyID, sid, 0, StatusSuccess, nil); err != nil {
		t.Fatalf("UpdateStatus(step 0): %v", err)
	}
	if err := Insert(ctx, integrationPool, &ApplyRun{
		ApplyID: applyID, SID: sid, IncarnationName: "redis-prod", Scenario: "create", Status: StatusRunning, Passage: 1,
	}); err != nil {
		t.Fatalf("Insert(step 1): %v", err)
	}
	if err := StartHostTasks(ctx, integrationPool, applyID, sid, []int{1, 2}); err != nil {
		t.Fatalf("StartHostTasks(step 1): %v", err)
	}
	requireTimes(t, "step 1 running", applyID, map[int]timeState{0: {true, true}, 1: {started: true}, 2: {}})
}

// A host whose run ended without reporting every task: the task it was at finished
// when its run did, and the tasks it never reached are not returned. While the run
// is live the same rows read as stamped.
func TestIntegration_RunTaskTimes_SettledByTheHostsRun(t *testing.T) {
	ctx := context.Background()
	const sid = "a.example.com"
	applyID := seedTimedRun(t, sid, 3)

	if err := StartHostTasks(ctx, integrationPool, applyID, sid, []int{0, 1, 2}); err != nil {
		t.Fatalf("StartHostTasks: %v", err)
	}
	if err := FinishHostTask(ctx, integrationPool, applyID, sid, 0, true); err != nil {
		t.Fatalf("FinishHostTask: %v", err)
	}
	requireTimes(t, "live", applyID, map[int]timeState{0: {true, true}, 1: {started: true}, 2: {}})

	time.Sleep(10 * time.Millisecond)
	if err := UpdateStatus(ctx, integrationPool, applyID, sid, 0, StatusFailed, nil); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	run, err := SelectByApplyID(ctx, integrationPool, applyID, sid)
	if err != nil || run.FinishedAt == nil {
		t.Fatalf("SelectByApplyID: run=%+v err=%v, want a finished run", run, err)
	}
	got := requireTimes(t, "run ended", applyID, map[int]timeState{0: {true, true}, 1: {true, true}})
	if !got[1].FinishedAt.Equal(*run.FinishedAt) {
		t.Errorf("task 1 finished %v, want the end of the host's run %v", got[1].FinishedAt, run.FinishedAt)
	}
	if got[0].FinishedAt.Equal(*run.FinishedAt) {
		t.Errorf("task 0 finished %v, the end of the run: a reported finish must stand", got[0].FinishedAt)
	}
}

// Rows hang off the plan: one with no plan row is refused, and they leave with it —
// purge_apply_run_plan deletes plan rows only, so the cascade IS the retention.
func TestIntegration_RunTaskTimes_HangOffThePlan(t *testing.T) {
	ctx := context.Background()
	const sid = "a.example.com"
	applyID := seedTimedRun(t, sid, 1)

	if err := StartHostTasks(ctx, integrationPool, applyID, sid, []int{5}); err == nil {
		t.Error("a row for a plan_index with no plan row was accepted")
	}
	if err := StartHostTasks(ctx, integrationPool, applyID, sid, []int{0}); err != nil {
		t.Fatalf("StartHostTasks: %v", err)
	}
	if _, err := integrationPool.Exec(ctx, `DELETE FROM apply_run_plan WHERE apply_id = $1`, applyID); err != nil {
		t.Fatalf("delete plan: %v", err)
	}
	var n int
	if err := integrationPool.QueryRow(ctx, `SELECT count(*) FROM apply_run_plan_host_times WHERE apply_id = $1`, applyID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("after the plan row went: %d rows, err = %v; want none", n, err)
	}
}
