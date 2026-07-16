//go:build e2e

// L3a E2E: scenario-apply execution path (ADR-039) -- the foundation of the
// per-section e2e coverage (Tide / push / drift / ...). Proves that the
// apply chain RegisterService -> CreateIncarnation -> ConnectSoulStub ->
// apply_runs success -> incarnation `ready` + state-commit works end-to-end
// on the real stack (PG+Redis+Vault testcontainers + keeper process +
// connected soul-stub).
//
// Why it catches regressions (mirrors errand_run_test.go for the apply path):
//   - missing service-registration -> CreateIncarnation 422 "not registered";
//   - acolyte pool disabled -> apply_runs stuck planned forever ->
//     WaitApplySuccess timeout;
//   - dispatch never reaches the Soul (no live stream / lease) -> orphaned;
//   - state-commit broken (render state_changes.sets / commitSuccess) ->
//     state does not match the scenario `state_changes.sets`.
//
// Documented limitation: soul-stub does NOT execute real modules --
// SetApplyDefaultSuccess makes it answer RunResult{SUCCESS} to any
// ApplyRequest task (L3a contract: we check the keeper-side apply_runs
// lifecycle + state-commit, not core-module execution realism; real exec is L3b).
package e2e_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
)

// TestScenarioApply_NoopCreate_Succeeds -- minimal self-contained
// scenario-apply: the noop service (a single core.exec.run step, no
// required input). CreateIncarnation auto-runs the scenario `create` and
// returns an apply_id; we wait for success + incarnation `ready`.
func TestScenarioApply_NoopCreate_Succeeds(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/noop",
		Souls:       1,
	})
	defer stack.Cleanup()

	// Reusable helper #1: materializing the example -> a file:// git repo +
	// POST /v1/services. Without it CreateIncarnation answers 422.
	stack.RegisterService(t, "noop", "examples/service/noop")

	// Reusable helper #2: live EventStream (Redis SID-lease -> dispatch is
	// routed to the local Outbound). SetApplyDefaultSuccess -- SUCCESS for
	// any task without a per-task script.
	stub := stack.ConnectSoulStub(t, 0)
	stub.SetApplyDefaultSuccess(true)

	// Coven membership: the run's roster resolves via
	// `incarnation.name in coven[]` (ADR-008). Without it the scenario sees
	// no_hosts -> error_locked.
	stack.AddSoulToCoven(t, 0, "test-noop")

	// CreateIncarnation auto-runs the scenario `create` (incarnation.go) and
	// returns that run's apply_id. noop-create has no required input. We use
	// the auto-create's apply_id -- a separate RunScenario(create) would be
	// rejected ("incarnation already applying").
	_, applyID := stack.CreateIncarnationWithApply(t, "test-noop", "noop@main", nil)

	// Reusable helper #3: blocking wait for apply_runs.status=success across
	// all rows of the run (planned->claimed->dispatched->success).
	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")
}

// TestScenarioApply_SmokeNginx_StateCommit -- an apply chain with a
// non-empty state-commit: smoke-nginx-create declares state_changes.sets
// {nginx_package, nginx_service}, rendered keeper-side and committed into
// incarnation.state. Proves that the state-commit branch
// (RenderStateChanges -> mergeStateChanges -> commitSuccess) works in a real
// run, not just in unit tests.
func TestScenarioApply_SmokeNginx_StateCommit(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/smoke-nginx",
		Souls:       1,
	})
	defer stack.Cleanup()

	stack.RegisterService(t, "smoke-nginx", "examples/service/smoke-nginx")

	stub := stack.ConnectSoulStub(t, 0)
	stub.SetApplyDefaultSuccess(true)
	stack.AddSoulToCoven(t, 0, "test-nginx-state")

	inc, applyID := stack.CreateIncarnationWithApply(t, "test-nginx-state", "smoke-nginx@main", map[string]any{
		"hostname": "web-01",
	})

	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")
	stack.AssertIncarnationState(t, inc, map[string]any{
		"nginx_package": "nginx",
		"nginx_service": "nginx",
	})
}

// TestIncarnationCreate_MissingRequiredInput_422 -- regression guard for
// synchronous required-input validation (fix 6ce69ce: a hole where create
// without required input created the incarnation and only failed later in
// async-apply). smoke-nginx-create declares input.hostname required ->
// CreateIncarnation WITHOUT input must respond 422 BEFORE the run starts
// (sync validation), not 202.
func TestIncarnationCreate_MissingRequiredInput_422(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/smoke-nginx",
		Souls:       1,
	})
	defer stack.Cleanup()

	stack.RegisterService(t, "smoke-nginx", "examples/service/smoke-nginx")

	// Without input.hostname (required) -- sync validation must return 422.
	body, status := stack.CreateIncarnationRaw(t, "test-nginx-missing", "smoke-nginx@main", nil)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("CreateIncarnation without required input: status=%d, expected 422 (sync validation, fix 6ce69ce); body=%s",
			status, string(body))
	}
	// Sanity: the problem-detail mentions validation (not "not registered" --
	// that would be a 422 of a different nature, catching a swapped cause).
	if !strings.Contains(string(body), "validation") && !strings.Contains(string(body), "hostname") &&
		!strings.Contains(string(body), "required") && !strings.Contains(string(body), "input") {
		t.Fatalf("CreateIncarnation 422 body does not look like an input validation error: %s", string(body))
	}
}
