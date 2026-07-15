package scenario

import (
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
)

// TestBuildHostReport_PerHostDifferentWhere_NoMismatch — ★ GUARD (ADR-056 §S1 fix
// Variant B, symmetric to TestBuildRegisterByHost_PerHostDifferentWhere_NoMismatch):
// a per-host different where: within ONE Passage gives a register task a
// DIFFERENT LOCAL task_idx on different hosts, but the drift report must label
// module/action by GLOBAL plan_index.
//
// Scenario: Passage 0 carries #0 core.file (where: host-A only) + #1 core.exec
// probe (both hosts). On host-A the slice = [#0, #1] → probe at local
// task_idx=1; on host-B the slice = [#1] (#0 filtered by where) → probe at local
// task_idx=0. probe's plan_index is the same (1) on both, local task_idx differs
// (1 vs 0).
//
// ASSERT: on BOTH hosts, task #1 in DriftReport is labeled core.exec.run
// ("probe") with Idx=1 (global). REVERSE: resolving by reg.TaskIdx, host-B would
// have taken taskMeta[0] = core.file.present ("place-marker") and Idx=0 — the
// wrong task's module/action, which is exactly the bug being closed.
func TestBuildHostReport_PerHostDifferentWhere_NoMismatch(t *testing.T) {
	tasks := []*render.RenderedTask{
		{Index: 0, Module: "core.file.present", Name: "place-marker"}, // where: host-A only
		{Index: 1, Module: "core.exec.run", Name: "probe"},            // probe — both hosts
	}
	taskMeta := buildTaskMetaIndex(tasks)

	cases := []struct {
		sid      string
		register applyrun.TaskRegister
	}{
		{
			// host-A: probe at local task_idx=1 (slice [#0,#1]); plan_index=1.
			sid:      "host-A",
			register: applyrun.TaskRegister{ApplyID: "a", SID: "host-A", PlanIndex: 1, TaskIdx: 1, Passage: 0, RegisterData: map[string]any{"changed": true}},
		},
		{
			// host-B: probe at local task_idx=0 (#0 filtered by where);
			// plan_index=1 — same global value.
			sid:      "host-B",
			register: applyrun.TaskRegister{ApplyID: "a", SID: "host-B", PlanIndex: 1, TaskIdx: 0, Passage: 0, RegisterData: map[string]any{"changed": true}},
		},
	}

	for _, c := range cases {
		hs := applyrun.HostStatus{SID: c.sid, Status: applyrun.StatusSuccess}
		hr := buildHostReport(hs, taskMeta, []applyrun.TaskRegister{c.register}, hostTaskFailure{})

		if len(hr.Tasks) != 1 {
			t.Fatalf("%s: tasks len = %d, want 1", c.sid, len(hr.Tasks))
		}
		got := hr.Tasks[0]
		if got.Idx != 1 {
			t.Errorf("★ %s: task.Idx = %d, want 1 (глобальный plan_index, не локальный task_idx=%d)",
				c.sid, got.Idx, c.register.TaskIdx)
		}
		if got.Module != "core.exec.run" {
			t.Errorf("★ %s: task.Module = %q, want core.exec.run (резолв по plan_index, не локальному task_idx)",
				c.sid, got.Module)
		}
		if got.Action != "probe" {
			t.Errorf("★ %s: task.Action = %q, want probe", c.sid, got.Action)
		}
		if !got.Changed {
			t.Errorf("%s: task.Changed = false, want true", c.sid)
		}
		if hr.Status != DriftStatusDrifted {
			t.Errorf("%s: host status = %q, want drifted", c.sid, hr.Status)
		}
	}
}

// TestBuildHostReport_FailureBranch_GlobalPlanIndex — ★ GUARD (ADR-056 §S1 fix
// Variant B, failure channel — the class's last instance): buildHostReport's
// failure branch resolves the failed task's module/action by GLOBAL plan_index
// (apply_runs.failed_plan_index), NOT by LOCAL task_idx.
//
// Staged/per-host-where scenario: the failed task on host-B has LOCAL
// task_idx=0 (its slice after where: starts with it), but GLOBAL plan_index=2
// (it's third in the full plan). taskMeta is built from RenderedTask.Index:
// idx=2 is core.exec.run ("the-failing-probe"); idx=0 is core.file.present
// ("neighbor").
//
// ASSERT: the failure row in DriftReport is labeled core.exec.run with Idx=2.
// The reverse check is built in: buildTaskFailureMap puts exactly
// FailedPlanIndex(2) into hostTaskFailure.planIndex; if the failure branch
// resolved taskMeta[task_idx] (local 0), it would take core.file.present
// ("neighbor") and Idx=0 — the neighboring task's module/action (mislabel),
// which is exactly the bug being closed.
func TestBuildHostReport_FailureBranch_GlobalPlanIndex(t *testing.T) {
	tasks := []*render.RenderedTask{
		{Index: 0, Module: "core.file.present", Name: "neighbor"},
		{Index: 1, Module: "core.pkg.installed", Name: "pkg"},
		{Index: 2, Module: "core.exec.run", Name: "the-failing-probe"},
	}
	taskMeta := buildTaskMetaIndex(tasks)

	// Emulates the persisted apply_runs row of the failed host: local task_idx=0
	// (position within its Passage's ApplyRequest), global failed_plan_index=2.
	intp := func(i int) *int { return &i }
	strp := func(s string) *string { return &s }
	hs := applyrun.HostStatus{
		SID:             "host-B",
		Status:          applyrun.StatusFailed,
		TaskIdx:         intp(0), // LOCAL — the neighboring task in taskMeta
		FailedPlanIndex: intp(2), // GLOBAL — the actual failed task
		ErrorSummary:    strp("task 2 core.exec.run: E: boom"),
	}

	// buildTaskFailureMap picks the global index (failedPlanIndex helper).
	failureMap := buildTaskFailureMap([]applyrun.HostStatus{hs})
	failure := failureMap["host-B"]
	if failure.planIndex != 2 {
		t.Fatalf("★ hostTaskFailure.planIndex = %d, want 2 (глобальный failed_plan_index, не локальный task_idx=0)", failure.planIndex)
	}

	hr := buildHostReport(hs, taskMeta, nil, failure)
	if len(hr.Tasks) != 1 {
		t.Fatalf("tasks len = %d, want 1 (одна failure-строка)", len(hr.Tasks))
	}
	got := hr.Tasks[0]
	if got.Idx != 2 {
		t.Errorf("★ failure task.Idx = %d, want 2 (глобальный plan_index, не локальный task_idx=0)", got.Idx)
	}
	if got.Module != "core.exec.run" {
		t.Errorf("★ failure task.Module = %q, want core.exec.run (резолв по plan_index; реверс по task_idx=0 дал бы core.file.present)", got.Module)
	}
	if got.Action != "the-failing-probe" {
		t.Errorf("★ failure task.Action = %q, want the-failing-probe", got.Action)
	}
	if got.Message != "task 2 core.exec.run: E: boom" {
		t.Errorf("failure task.Message = %q", got.Message)
	}
	if hr.Status != DriftStatusFailed {
		t.Errorf("host status = %q, want failed", hr.Status)
	}
}

// TestBuildTaskFailureMap_FallbackToTaskIdx — backward compat: a run row from
// before migration 081 (failed_plan_index=NULL) or an old Soul without an echoed
// plan_index → failedPlanIndex falls back to local task_idx. For N=1
// (local==global) this is a correct value; guarantees the failure row isn't lost.
func TestBuildTaskFailureMap_FallbackToTaskIdx(t *testing.T) {
	intp := func(i int) *int { return &i }
	strp := func(s string) *string { return &s }

	hs := applyrun.HostStatus{
		SID:          "host-legacy",
		Status:       applyrun.StatusFailed,
		TaskIdx:      intp(3),
		ErrorSummary: strp("task 3 core.pkg.installed: boom"),
		// FailedPlanIndex == nil (a row from before 081)
	}
	m := buildTaskFailureMap([]applyrun.HostStatus{hs})
	f, ok := m["host-legacy"]
	if !ok {
		t.Fatal("failure-строка потеряна при NULL failed_plan_index (нет fallback на task_idx)")
	}
	if f.planIndex != 3 {
		t.Errorf("fallback planIndex = %d, want 3 (== task_idx для N=1)", f.planIndex)
	}
}
