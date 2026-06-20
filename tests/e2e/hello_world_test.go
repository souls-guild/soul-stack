//go:build e2e

// L3a E2E: service-hello-world happy-path (ADR-039).
//
// Полный pilot-pattern (как smoke-nginx, но с input.required и реальной мутацией
// incarnation.state). Flow:
//  1. NewStack: testcontainers (PG/Redis/Vault) + Keeper-процесс + 1 soul-stub.
//  2. CreateIncarnation `test-hello` поверх service `service-hello-world@main`.
//  3. RunScenario `create` с input.greeting (required).
//  4. WaitApplySuccess → asserts по after-create.yaml expectations:
//     - apply_runs.status == "success";
//     - incarnation.state.greeting_file == "/tmp/soul-stack-hello"
//     (берётся state_changes.sets из scenario/create/main.yml);
//     - audit_log: incarnation.scenario_started с apply_id;
//     - metrics: keeper_scenario_runs_total{result="ok"} >= 1.
package e2e_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
)

func TestE2EServiceHelloWorld_Create(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/hello-world",
		Souls:       1,
	})
	defer stack.Cleanup()

	inc := stack.CreateIncarnation(t, "test-hello", "service-hello-world@main", map[string]any{
		"greeting": "hello from L3a E2E",
	})

	applyID := stack.RunScenario(t, inc, "create", map[string]any{
		"greeting": "hello from L3a E2E",
	})

	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")
	stack.AssertIncarnationState(t, inc, map[string]any{
		"greeting_file": "/tmp/soul-stack-hello",
	})
	stack.AssertAuditEvent(t, "incarnation.scenario_started", map[string]any{
		"apply_id": applyID,
	})
	stack.AssertMetricGE(t, `keeper_scenario_runs_total{result="ok"}`, 1)
}
