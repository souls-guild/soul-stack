//go:build e2e

// L3a E2E: service-coven-probe::create (ADR-039).
//
// Verifies the init scenario: core.file.present across the whole incarnation +
// a double mutation of incarnation.state (marker_file, last_target). Coven
// targeting itself is verified by separate scenarios (mark_a/mark_ab/mark_where) —
// they need N souls with different covens; deferred to a separate slice.
package e2e_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
)

func TestE2EServiceCovenProbe_Create(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/coven-probe",
		Souls:       1,
	})
	defer stack.Cleanup()

	stack.RegisterService(t, "coven-probe", "examples/service/coven-probe")

	stub := stack.ConnectSoulStub(t, 0)
	stub.SetApplyDefaultSuccess(true)

	// Bare create path on purpose — see the note in noop_test.go (NIM-317).
	inc := stack.CreateIncarnation(t, "test-coven-probe", "coven-probe@main", nil)
	stack.AddMember(t, 0, inc)

	applyID := stack.RunScenario(t, inc, "create", nil)

	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")
	stack.AssertIncarnationState(t, inc, map[string]any{
		"marker_file": "/tmp/coven-init.txt",
		"last_target": "create",
	})
	stack.AssertAuditEvent(t, "incarnation.scenario_started", map[string]any{
		"apply_id": applyID,
	})
	stack.AssertMetricGE(t, `keeper_scenario_runs_total{result="ok"}`, 1)
}
