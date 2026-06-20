//go:build e2e

// L3a E2E: service-long-runner::create (ADR-039).
//
// Простой init scenario; «длинные прогоны» (staggered, serial_waves) — отдельные
// сценарии этого же сервиса, для них нужны multi-host + поллинг mid-run.
// Здесь покрываем только happy-path create + двойную мутацию state.
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
