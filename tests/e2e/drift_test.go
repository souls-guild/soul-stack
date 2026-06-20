//go:build e2e

// Per-section E2E: Scry / check-drift (ADR-031, Slice B). POST
// /v1/incarnations/{name}/check-drift рендерит scenario `converge` под текущим
// service-snapshot-ом, рассылает ApplyRequest{dry_run:true} всем roster-хостам
// через work-queue (Acolyte), ждёт терминалов и собирает DriftReport.
//
// soul-stub расширен Plan-ответом (SetDryRunPlan, см. internal/soulstub): на
// dry_run-ApplyRequest он эмитит per-task TaskEvent{changed} перед RunResult —
// имитация SoulModule.Plan (mod.Apply на dry_run не вызывается, read-only
// гарантия ADR-031). Keeper-side accumulateRegister складывает register_data в
// apply_task_register, откуда CheckDrift собирает per-task drifted/clean. Это
// даёт ПОЛНЫЙ drift-e2e: проверяем и changed=true (drifted) и changed=false
// (clean), а не только факт работы endpoint-а.
//
// Ограничение (L3a-контракт, как у scenario_apply / errand_run): stub НЕ
// исполняет реальный SoulModule.Plan core-модуля — он отвечает scripted
// changed-флагом. Реализм Plan core.file.present — L3b territory.
//
// Почему ловит регрессии:
//   - DryRun не пробрасывается в proto (acolyteEnabled / Recipe.DryRun) → stub
//     получает обычный ApplyRequest, drift-ветка не сработает → clean вместо
//     drifted;
//   - driftBarrier не дожидается терминалов / падает на failed → таймаут или 500;
//   - assembleDriftReport теряет per-task register / summary считает неверно;
//   - check-drift handler регрессировал в не-200.
package e2e_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
)

// TestDrift_CheckDrift_DriftedAndClean — full Scry e2e на noop-service
// (scenario/converge/main.yml, один core.file.present-task).
//
// Шаг 1: stub в режиме Plan{changed:true} → DriftReport помечает хост drifted,
// per-task changed=true, summary.hosts_drifted=1.
// Шаг 2: тот же incarnation, stub переключён в Plan{changed:false} → повторный
// check-drift даёт clean, summary.hosts_clean=1.
func TestDrift_CheckDrift_DriftedAndClean(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/noop",
		Souls:       1,
	})
	defer stack.Cleanup()

	stack.RegisterService(t, "noop", "examples/service/noop")

	stub := stack.ConnectSoulStub(t, 0)
	stub.SetApplyDefaultSuccess(true)
	sid := stack.SoulSID(0)

	stack.AddSoulToCoven(t, 0, "test-drift")

	// Incarnation должен быть applied (status ready) — иначе roster пуст / status
	// не позволит фоновый scan; on-demand check-drift roster берёт из topology.
	_, applyID := stack.CreateIncarnationWithApply(t, "test-drift", "noop@main", nil)
	stack.WaitApplySuccess(t, applyID, 60)

	// Шаг 1 — drifted: Plan на каждую converge-задачу возвращает changed=true.
	stub.SetDryRunPlan(true)
	drifted := stack.CheckDrift(t, "test-drift", nil)

	if drifted.CheckedAt.IsZero() {
		t.Fatalf("DriftReport.checked_at пуст: %+v", drifted)
	}
	if drifted.Incarnation != "test-drift" {
		t.Fatalf("DriftReport.incarnation=%q, ожидался test-drift", drifted.Incarnation)
	}
	if len(drifted.Hosts) != 1 {
		t.Fatalf("DriftReport.hosts: len=%d, ожидался 1 (один connected-soul); hosts=%+v", len(drifted.Hosts), drifted.Hosts)
	}
	h := drifted.Hosts[0]
	if h.SID != sid {
		t.Fatalf("DriftReport.hosts[0].sid=%q, ожидался %q", h.SID, sid)
	}
	if h.Status != "drifted" {
		t.Fatalf("DriftReport.hosts[0].status=%q, ожидался drifted (Plan{changed:true}); host=%+v", h.Status, h)
	}
	if len(h.Tasks) == 0 {
		t.Fatalf("DriftReport.hosts[0].tasks пуст — per-task register не дошёл (TaskEvent потерян?); host=%+v", h)
	}
	gotChanged := false
	for _, tk := range h.Tasks {
		if tk.Changed {
			gotChanged = true
			if tk.Module == "" {
				t.Fatalf("drifted task без module (task_idx→RenderedTask маппинг сломан?): %+v", tk)
			}
		}
	}
	if !gotChanged {
		t.Fatalf("DriftReport: ни одной changed-задачи при Plan{changed:true}; tasks=%+v", h.Tasks)
	}
	if drifted.Summary.HostsDrifted != 1 {
		t.Fatalf("DriftReport.summary.hosts_drifted=%d, ожидался 1; summary=%+v", drifted.Summary.HostsDrifted, drifted.Summary)
	}

	// Шаг 2 — clean: тот же incarnation, Plan{changed:false}.
	stub.SetDryRunPlan(false)
	clean := stack.CheckDrift(t, "test-drift", nil)

	if len(clean.Hosts) != 1 {
		t.Fatalf("clean DriftReport.hosts: len=%d, ожидался 1; hosts=%+v", len(clean.Hosts), clean.Hosts)
	}
	if clean.Hosts[0].Status != "clean" {
		t.Fatalf("clean DriftReport.hosts[0].status=%q, ожидался clean (Plan{changed:false}); host=%+v",
			clean.Hosts[0].Status, clean.Hosts[0])
	}
	for _, tk := range clean.Hosts[0].Tasks {
		if tk.Changed {
			t.Fatalf("clean DriftReport: changed-задача при Plan{changed:false}: %+v", tk)
		}
	}
	if clean.Summary.HostsClean != 1 {
		t.Fatalf("clean DriftReport.summary.hosts_clean=%d, ожидался 1; summary=%+v", clean.Summary.HostsClean, clean.Summary)
	}
	if clean.Summary.HostsDrifted != 0 {
		t.Fatalf("clean DriftReport.summary.hosts_drifted=%d, ожидался 0; summary=%+v", clean.Summary.HostsDrifted, clean.Summary)
	}
}
