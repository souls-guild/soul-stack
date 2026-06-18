//go:build e2e

// L3a E2E: service-noop happy-path (ADR-039).
//
// Минимальный no-op scenario без input и без мутации incarnation.state —
// проверяем чистый apply_runs lifecycle + audit + metrics. Это «нижняя граница»
// L3a-теста: меньше уже не имеет смысла.
package e2e_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
)

func TestE2EServiceNoop_Create(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/service-noop",
		Souls:       1,
	})
	defer stack.Cleanup()

	inc := stack.CreateIncarnation(t, "test-noop", "service-noop@main", nil)

	applyID := stack.RunScenario(t, inc, "create", nil)

	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")
	stack.AssertAuditEvent(t, "incarnation.scenario_started", map[string]any{
		"apply_id": applyID,
	})
	stack.AssertMetricGE(t, `keeper_scenario_runs_total{result="ok"}`, 1)
}
