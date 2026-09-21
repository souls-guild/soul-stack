//go:build e2e

// L3a E2E: service-noop happy-path (ADR-039).
//
// Minimal no-op scenario without input and without incarnation.state mutation --
// verifies the plain apply_runs lifecycle + audit + metrics. This is the
// "lower bound" of an L3a test: less than this has no point.
package e2e_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
)

func TestE2EServiceNoop_Create(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/noop",
		Souls:       1,
	})
	defer stack.Cleanup()

	stack.RegisterService(t, "noop", "examples/service/noop")

	stub := stack.ConnectSoulStub(t, 0)
	stub.SetApplyDefaultSuccess(true)

	// Bare create path on purpose: POST /v1/incarnations WITHOUT create_scenario,
	// then an explicit run. CreateIncarnationOnRoster covers the other shape, and
	// nothing else covers this one (NIM-317).
	inc := stack.CreateIncarnation(t, "test-noop", "noop@main", nil)
	stack.AddMember(t, 0, inc)

	applyID := stack.RunScenario(t, inc, "create", nil)

	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")
	stack.AssertAuditEvent(t, "incarnation.scenario_started", map[string]any{
		"apply_id": applyID,
	})
	stack.AssertMetricGE(t, `keeper_scenario_runs_total{result="ok"}`, 1)
}
