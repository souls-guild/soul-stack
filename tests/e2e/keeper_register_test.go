//go:build e2e

// L3a E2E: keeper-side module dispatch (ADR-017, docs/keeper/modules.md) -
// foundation of membership epic (S1). Proves task with `on: keeper` executes
// LOCALLY on keeper instance through keeper-side core Registry and really mutates
// `souls` registry (coven binding by core.soul.registered module), rather than
// going to Soul.
//
// Why this catches S1 regressions:
//   - coreReg not passed into scenario-runner (B1) -> keeper task will not execute ->
//     ErrKeeperModulesNotConfigured → error_locked;
//   - render rejects on: keeper (B2 not done) -> render_failed -> error_locked;
//   - keeper task executed, but coven not written (B3 aggregation broken) ->
//     souls.coven assert fails;
//   - keeper apply_runs row not success -> WaitApplySuccess timeout/fatal.
package e2e_test

import (
	"context"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
)

func TestE2EKeeperSideDispatch_CovenRegistered(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/keeper-register",
		Souls:       1,
	})
	defer stack.Cleanup()

	stack.RegisterService(t, "keeper-register", "examples/service/keeper-register")

	// Live EventStream stream: adjacent Soul-side echo step of the run dispatches
	// here (default-success), keeper step executes locally on keeper.
	stub := stack.ConnectSoulStub(t, 0)
	stub.SetApplyDefaultSuccess(true)

	soulSID := stack.SoulSID(0)
	const incName = "test-keeper-register"
	const covenLabel = "keeper-tagged"

	// Run roster resolves by root Coven label incarnation.name (ADR-008): without
	// membership scenario sees no_hosts -> error_locked. The checked label
	// (covenLabel) is assigned by keeper-side step.
	stack.AddSoulToCoven(t, 0, incName)

	// CreateIncarnation auto-starts scenario `create`: keeper-side
	// core.soul.registered (on: keeper) adds covenLabel to souls.coven of this SID
	// plus Soul-side echo on host.
	_, applyID := stack.CreateIncarnationWithApply(t, incName, "keeper-register@main", map[string]any{
		"soul_sid":    soulSID,
		"coven_label": covenLabel,
	})

	// All run rows (keeper-target sid="keeper" + host row) -> success.
	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")

	// Main assert: keeper-side step really wrote coven to souls registry.
	assertSoulHasCoven(t, stack, soulSID, covenLabel)

	// keeper-target of the run exists as separate apply_runs row (proves keeper-side
	// execution goes through the same apply_runs model).
	assertKeeperApplyRun(t, stack, applyID)
}

// assertSoulHasCoven checks that souls.coven for given SID contains label.
func assertSoulHasCoven(t *testing.T, stack *harness.Stack, sid, label string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var covens []string
	if err := stack.DB().QueryRow(ctx,
		"SELECT coven FROM souls WHERE sid = $1", sid).Scan(&covens); err != nil {
		t.Fatalf("assertSoulHasCoven %s: query: %v", sid, err)
	}
	for _, c := range covens {
		if c == label {
			return
		}
	}
	t.Fatalf("assertSoulHasCoven %s: coven=%v does not contain %q - keeper-side core.soul.registered did not write label",
		sid, covens, label)
}

// assertKeeperApplyRun checks presence of success apply_runs row for run's
// keeper-target (sid="keeper" = render.KeeperTargetSID).
func assertKeeperApplyRun(t *testing.T, stack *harness.Stack, applyID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var status string
	if err := stack.DB().QueryRow(ctx,
		"SELECT status FROM apply_runs WHERE apply_id = $1 AND sid = 'keeper'", applyID).Scan(&status); err != nil {
		t.Fatalf("assertKeeperApplyRun %s: no keeper-target apply_runs row (sid='keeper'): %v", applyID, err)
	}
	if status != "success" {
		t.Fatalf("assertKeeperApplyRun %s: keeper-target status=%q, want success", applyID, status)
	}
}
