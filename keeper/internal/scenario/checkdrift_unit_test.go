package scenario

import (
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
)

// TestBuildHostReport_PerHostDifferentWhere_NoMismatch — ★ GUARD (ADR-056 §S1 fix
// Variant B, симметричен TestBuildRegisterByHost_PerHostDifferentWhere_NoMismatch):
// per-host разный where: в ОДНОМ Passage даёт register-задаче РАЗНЫЙ ЛОКАЛЬНЫЙ
// task_idx на разных хостах, но drift-отчёт обязан маркировать module/action по
// ГЛОБАЛЬНОМУ plan_index.
//
// Сценарий: Passage 0 несёт #0 core.file (where: только host-A) + #1 core.exec
// probe (оба хоста). На host-A срез = [#0, #1] → probe на локальной task_idx=1;
// на host-B срез = [#1] (#0 отфильтрован where) → probe на локальной task_idx=0.
// plan_index у probe одинаковый (1) на обоих, локальный task_idx разный (1 vs 0).
//
// ASSERT: на ОБОИХ хостах задача #1 в DriftReport промаркирована core.exec.run
// ("probe") с Idx=1 (глобальный). РЕВЕРС: с резолвом по reg.TaskIdx host-B взял бы
// taskMeta[0] = core.file.present ("place-marker") и Idx=0 — module/action не той
// задачи, что и есть закрываемый баг.
func TestBuildHostReport_PerHostDifferentWhere_NoMismatch(t *testing.T) {
	tasks := []*render.RenderedTask{
		{Index: 0, Module: "core.file.present", Name: "place-marker"}, // where: только host-A
		{Index: 1, Module: "core.exec.run", Name: "probe"},            // probe — оба хоста
	}
	taskMeta := buildTaskMetaIndex(tasks)

	cases := []struct {
		sid      string
		register applyrun.TaskRegister
	}{
		{
			// host-A: probe на локальной task_idx=1 (срез [#0,#1]); plan_index=1.
			sid:      "host-A",
			register: applyrun.TaskRegister{ApplyID: "a", SID: "host-A", PlanIndex: 1, TaskIdx: 1, Passage: 0, RegisterData: map[string]any{"changed": true}},
		},
		{
			// host-B: probe на локальной task_idx=0 (#0 отфильтрован where);
			// plan_index=1 — тот же глобальный.
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
// Variant B, failure-канал — последняя инстанция класса): failure-ветка
// buildHostReport резолвит module/action упавшей задачи по ГЛОБАЛЬНОМУ
// plan_index (apply_runs.failed_plan_index), а НЕ по ЛОКАЛЬНОМУ task_idx.
//
// Сценарий staged/per-host-where: упавшая задача на host-B имеет ЛОКАЛЬНЫЙ
// task_idx=0 (её срез после where: начинается с неё), но ГЛОБАЛЬНЫЙ plan_index=2
// (она третья в полном плане). taskMeta построен по RenderedTask.Index: idx=2 —
// core.exec.run ("the-failing-probe"); idx=0 — core.file.present ("neighbor").
//
// ASSERT: failure-строка в DriftReport промаркирована core.exec.run и Idx=2.
// РЕВЕРС встроен: buildTaskFailureMap кладёт в hostTaskFailure.planIndex именно
// FailedPlanIndex(2); если бы failure-ветка резолвила taskMeta[task_idx]
// (локальный 0), она взяла бы core.file.present ("neighbor") и Idx=0 — module/
// action соседней задачи (mislabel), что и есть закрываемый баг.
func TestBuildHostReport_FailureBranch_GlobalPlanIndex(t *testing.T) {
	tasks := []*render.RenderedTask{
		{Index: 0, Module: "core.file.present", Name: "neighbor"},
		{Index: 1, Module: "core.pkg.installed", Name: "pkg"},
		{Index: 2, Module: "core.exec.run", Name: "the-failing-probe"},
	}
	taskMeta := buildTaskMetaIndex(tasks)

	// Эмулируем persisted-строку apply_runs упавшего хоста: локальный task_idx=0
	// (позиция в ApplyRequest своего Passage), глобальный failed_plan_index=2.
	intp := func(i int) *int { return &i }
	strp := func(s string) *string { return &s }
	hs := applyrun.HostStatus{
		SID:             "host-B",
		Status:          applyrun.StatusFailed,
		TaskIdx:         intp(0), // ЛОКАЛЬНЫЙ — соседняя задача в taskMeta
		FailedPlanIndex: intp(2), // ГЛОБАЛЬНЫЙ — реальная упавшая задача
		ErrorSummary:    strp("task 2 core.exec.run: E: boom"),
	}

	// buildTaskFailureMap выбирает глобальный индекс (failedPlanIndex helper).
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

// TestBuildTaskFailureMap_FallbackToTaskIdx — backward-compat: строка прогона до
// миграции 081 (failed_plan_index=NULL) или старый Soul без эхо plan_index →
// failedPlanIndex fallback на локальный task_idx. Для N=1 (локальный==глобальный)
// это корректное значение; гарантируем, что failure-строка не теряется.
func TestBuildTaskFailureMap_FallbackToTaskIdx(t *testing.T) {
	intp := func(i int) *int { return &i }
	strp := func(s string) *string { return &s }

	hs := applyrun.HostStatus{
		SID:          "host-legacy",
		Status:       applyrun.StatusFailed,
		TaskIdx:      intp(3),
		ErrorSummary: strp("task 3 core.pkg.installed: boom"),
		// FailedPlanIndex == nil (строка до 081)
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
