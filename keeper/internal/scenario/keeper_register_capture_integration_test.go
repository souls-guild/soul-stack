//go:build integration

// ★ Parity guard for the live provisioned_vm_ids bug (ADR-056 amendment
// 2026-07-02). A class of bug the L0 harness MASKED (trial/harness.go
// broadcasts register to all hosts): a keeper-side task's register accumulates
// under the synthetic SID "keeper" (accumulateKeeperRegister), while the render
// that reads it, before the fix, read only the host's per-host bucket →
// `${ register.provision.* }` → "no such key" → error_locked AFTER a fully
// successful deploy. Under [ADR-0084] the reader is a `core.state.<verb>`
// capture rather than the retired end-of-run block, and the bug class survives
// the move intact: a capture is an `on: keeper` task, so it renders against the
// same keeper bucket (keeperVars, keeper/internal/render/dispatch.go). The test
// goes through the live path: run()+PG, real accumulateKeeperRegister/
// loadRegisterByHost — no mock register broadcast.

package scenario

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// keeperRegisterCaptureRepo mirrors the live create redis-prov: a keeper task
// (on: keeper, register: provision) + a host task, and a capture that reads the
// keeper task's register.
func keeperRegisterCaptureRepo(t *testing.T) string {
	t.Helper()
	return writeServiceRepo(t, `name: create
description: keeper-register in a capture (live provisioned_vm_ids class)
tasks:
  - name: provision vm
    module: core.probe.created
    on: keeper
    register: provision
    params:
      provider: fake
  - name: role on host
    module: core.exec.run
    changed_when: "false"
    params:
      cmd: echo
      args: ["role"]
  - name: record the address
    module: core.state.set
    on: keeper
    params:
      field: provisioned_ip
      value: "${ register.provision.ip }"
`)
}

// TestIntegration_KeeperRegisterInCapture_CommitsToState — the run finishes
// Ready (NOT error_locked), the keeper task's register value commits into
// incarnation.state. Per-host register for hosts is separately covered by
// TestIntegration_RegisterInCapture_CommitsToState (the host-probe class).
func TestIntegration_KeeperRegisterInCapture_CommitsToState(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := keeperRegisterCaptureRepo(t)

	bootstrap := &capturingKeeperModule{output: map[string]any{"ip": "10.0.0.7"}}
	keepers := keeperRegistryWith(map[string]module.SoulModule{"core.probe": bootstrap})

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerKeeperStaged(t, disp, keepers)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Before the fix: deploy success, but the capture's render fails "no such
	// key: provision" → error_locked (waitRunDone fails the status assert).
	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)
	if inc.State["provisioned_ip"] != "10.0.0.7" {
		t.Errorf("incarnation.state.provisioned_ip = %v, want \"10.0.0.7\" (from register.provision.ip of the keeper task)", inc.State["provisioned_ip"])
	}
	if disp.calls != 1 {
		t.Errorf("SendApply calls = %d, want 1 (host task)", disp.calls)
	}
}
