//go:build e2e

// L3a E2E: service-long-runner::create (ADR-039).
//
// Simple init scenario; "long runs" (staggered, serial_waves) are separate
// scenarios of the same service that need multi-host + mid-run polling.
// Here we only cover the happy-path create + double state mutation.
package e2e_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
)

func TestE2EServiceLongRunner_Create(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/long-runner",
		Souls:       1,
	})
	defer stack.Cleanup()

	inc := stack.CreateIncarnation(t, "test-long-runner", "service-long-runner@main", nil)

	applyID := stack.RunScenario(t, inc, "create", nil)

	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")
	stack.AssertIncarnationState(t, inc, map[string]any{
		"last_run":      "create",
		"runner_marker": "/tmp/long-runner-init.txt",
	})
	stack.AssertAuditEvent(t, "incarnation.scenario_started", map[string]any{
		"apply_id": applyID,
	})
	stack.AssertMetricGE(t, `keeper_scenario_runs_total{result="ok"}`, 1)
}
