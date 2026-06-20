//go:build e2e

// L3a E2E: service-coven-probe::create (ADR-039).
//
// Проверяем init-сценарий: core.file.present на весь incarnation + двойная
// мутация incarnation.state (marker_file, last_target). Coven-таргетинг как
// таковой проверяется отдельными scenarios (mark_a/mark_ab/mark_where) —
// для них нужны N souls с разными covens; вынесено в отдельный slice.
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

	inc := stack.CreateIncarnation(t, "test-coven-probe", "service-coven-probe@main", nil)

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
