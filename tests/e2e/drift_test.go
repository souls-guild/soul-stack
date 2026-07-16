//go:build e2e

// Per-section E2E: Scry / check-drift (ADR-031, Slice B). POST
// /v1/incarnations/{name}/check-drift renders the `converge` scenario against
// the current service snapshot, fans out ApplyRequest{dry_run:true} to all
// roster hosts via the work queue (Acolyte), waits for terminals and collects
// a DriftReport.
//
// soul-stub is extended with a Plan response (SetDryRunPlan, see
// internal/soulstub): on a dry_run ApplyRequest it emits a per-task
// TaskEvent{changed} before RunResult — imitating SoulModule.Plan (mod.Apply
// is not invoked on dry_run, the read-only guarantee of ADR-031). The
// keeper-side accumulateRegister folds register_data into apply_task_register,
// from which CheckDrift assembles per-task drifted/clean. This gives a FULL
// drift e2e: it verifies both changed=true (drifted) and changed=false
// (clean), not just that the endpoint responds.
//
// Limitation (L3a contract, same as scenario_apply / errand_run): the stub
// does NOT execute the core module's real SoulModule.Plan — it replies with a
// scripted changed flag. Realistic Plan for core.file.present is L3b
// territory.
//
// Why it catches regressions:
//   - DryRun not propagated into proto (acolyteEnabled / Recipe.DryRun) → stub
//     gets a regular ApplyRequest, the drift branch never fires → clean
//     instead of drifted;
//   - driftBarrier doesn't wait for terminals / fails on failed → timeout or 500;
//   - assembleDriftReport drops per-task register / miscounts summary;
//   - check-drift handler regresses to non-200.
package e2e_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
)

// TestDrift_CheckDrift_DriftedAndClean — full Scry e2e on the noop service
// (scenario/converge/main.yml, one core.file.present task).
//
// Step 1: stub in Plan{changed:true} mode → DriftReport marks the host
// drifted, per-task changed=true, summary.hosts_drifted=1.
// Step 2: same incarnation, stub switched to Plan{changed:false} → a repeat
// check-drift yields clean, summary.hosts_clean=1.
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

	// Incarnation must be applied (status ready) — otherwise the roster is
	// empty / status blocks the background scan; on-demand check-drift takes
	// the roster from topology.
	_, applyID := stack.CreateIncarnationWithApply(t, "test-drift", "noop@main", nil)
	stack.WaitApplySuccess(t, applyID, 60)

	// Step 1 — drifted: Plan returns changed=true for every converge task.
	stub.SetDryRunPlan(true)
	drifted := stack.CheckDrift(t, "test-drift", nil)

	if drifted.CheckedAt.IsZero() {
		t.Fatalf("DriftReport.checked_at is empty: %+v", drifted)
	}
	if drifted.Incarnation != "test-drift" {
		t.Fatalf("DriftReport.incarnation=%q, expected test-drift", drifted.Incarnation)
	}
	if len(drifted.Hosts) != 1 {
		t.Fatalf("DriftReport.hosts: len=%d, expected 1 (one connected soul); hosts=%+v", len(drifted.Hosts), drifted.Hosts)
	}
	h := drifted.Hosts[0]
	if h.SID != sid {
		t.Fatalf("DriftReport.hosts[0].sid=%q, expected %q", h.SID, sid)
	}
	if h.Status != "drifted" {
		t.Fatalf("DriftReport.hosts[0].status=%q, expected drifted (Plan{changed:true}); host=%+v", h.Status, h)
	}
	if len(h.Tasks) == 0 {
		t.Fatalf("DriftReport.hosts[0].tasks is empty — per-task register did not arrive (TaskEvent lost?); host=%+v", h)
	}
	gotChanged := false
	for _, tk := range h.Tasks {
		if tk.Changed {
			gotChanged = true
			if tk.Module == "" {
				t.Fatalf("drifted task without module (task_idx->RenderedTask mapping broken?): %+v", tk)
			}
		}
	}
	if !gotChanged {
		t.Fatalf("DriftReport: no changed task under Plan{changed:true}; tasks=%+v", h.Tasks)
	}
	if drifted.Summary.HostsDrifted != 1 {
		t.Fatalf("DriftReport.summary.hosts_drifted=%d, expected 1; summary=%+v", drifted.Summary.HostsDrifted, drifted.Summary)
	}

	// Step 2 — clean: same incarnation, Plan{changed:false}.
	stub.SetDryRunPlan(false)
	clean := stack.CheckDrift(t, "test-drift", nil)

	if len(clean.Hosts) != 1 {
		t.Fatalf("clean DriftReport.hosts: len=%d, expected 1; hosts=%+v", len(clean.Hosts), clean.Hosts)
	}
	if clean.Hosts[0].Status != "clean" {
		t.Fatalf("clean DriftReport.hosts[0].status=%q, expected clean (Plan{changed:false}); host=%+v",
			clean.Hosts[0].Status, clean.Hosts[0])
	}
	for _, tk := range clean.Hosts[0].Tasks {
		if tk.Changed {
			t.Fatalf("clean DriftReport: changed task under Plan{changed:false}: %+v", tk)
		}
	}
	if clean.Summary.HostsClean != 1 {
		t.Fatalf("clean DriftReport.summary.hosts_clean=%d, expected 1; summary=%+v", clean.Summary.HostsClean, clean.Summary)
	}
	if clean.Summary.HostsDrifted != 0 {
		t.Fatalf("clean DriftReport.summary.hosts_drifted=%d, expected 0; summary=%+v", clean.Summary.HostsDrifted, clean.Summary)
	}
}
