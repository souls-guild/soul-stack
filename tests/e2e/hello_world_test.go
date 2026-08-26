//go:build e2e

// L3a E2E: service-hello-world happy-path (ADR-039).
//
// Full pilot pattern (like smoke-nginx, but with input.required and real mutation
// incarnation.state). Flow:
//  1. NewStack: testcontainers (PG/Redis/Vault) + Keeper process + 1 soul-stub.
//  2. CreateIncarnation `test-hello` on top of service `hello-world@main`.
//  3. RunScenario `create` with input.greeting (required).
//  4. WaitApplySuccess -> asserts by after-create.yaml expectations:
//     - apply_runs.status == "success";
//     - incarnation.state.greeting_file == "/tmp/soul-stack-hello"
//     (taken from the capture step in scenario/create/main.yml);
//     - audit_log: incarnation.scenario_started with apply_id;
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

	stack.RegisterService(t, "hello-world", "examples/service/hello-world")

	stub := stack.ConnectSoulStub(t, 0)
	stub.SetApplyDefaultSuccess(true)

	// Bare create path on purpose — see the note in noop_test.go (NIM-317).
	// `greeting` is declared by scenario/create, not by service.yml, so it is
	// passed to the run and NOT to the incarnation spec.
	inc := stack.CreateIncarnation(t, "test-hello", "hello-world@main", nil)
	stack.AddMember(t, 0, inc)

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
