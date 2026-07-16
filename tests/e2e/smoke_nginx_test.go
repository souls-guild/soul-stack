//go:build e2e

// L3a E2E smoke: smoke-nginx happy-path (ADR-039).
//
// Flow:
//  1. NewStack: PG+Redis+Vault testcontainers + Keeper process + 1 soul-stub.
//     In an environment without docker / without a keeper binary -- t.Skip from NewStack (pre-flight).
//  2. CreateIncarnation `test-nginx` on top of service `smoke-nginx@main`.
//  3. RunScenario `create` with input.hostname.
//  4. WaitApplySuccess -> AssertApplyRunsStatus("success") + audit + metrics.
//
// Drift tests (TestValidApplyRunsStatus*) -- real asserts, without Skip --
// guarantee that harness.validApplyRunsStatus does not drift from the real
// enum in keeper/internal/applyrun/applyrun.go.
package e2e_test

import (
	"sort"
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
)

func TestSmokeNginx_InstallAndStart(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/smoke-nginx",
		Souls:       1,
	})
	defer stack.Cleanup()

	// Service registration: without it CreateIncarnation answers 422 "service
	// smoke-nginx is not registered" (ADR-029). RegisterService materializes
	// the example directory into a per-test file:// git repo and POST /v1/services.
	stack.RegisterService(t, "smoke-nginx", "examples/service/smoke-nginx")

	// Live EventStream: captures the Redis SID-lease -> ApplyRequest is
	// routed to the local Outbound. SetApplyDefaultSuccess -- answer SUCCESS
	// for any scenario task (install/start nginx) without a per-task
	// script: apply-e2e checks the apply_runs lifecycle, not execution
	// realism (L3a).
	stub := stack.ConnectSoulStub(t, 0)
	stub.SetApplyDefaultSuccess(true)

	// Coven membership: the run's roster resolves via
	// `incarnation.name in coven[]` (ADR-008). Without it the scenario sees
	// no_hosts -> error_locked.
	stack.AddSoulToCoven(t, 0, "test-nginx")

	inc, applyID := stack.CreateIncarnationWithApply(t, "test-nginx", "smoke-nginx@main", map[string]any{
		"hostname": "web-01",
	})

	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")
	stack.AssertIncarnationState(t, inc, map[string]any{
		"nginx_package": "nginx",
		"nginx_service": "nginx",
	})
	// Audit event: POST /v1/incarnations auto-runs the create scenario and
	// writes `incarnation.created` (router.go) with the auto-create run's
	// `apply_id` in the payload (incarnation.go::Create SetAuditPayload).
	// This is the same apply_id we waited for in WaitApplySuccess -- the
	// audit<->apply-run link.
	stack.AssertAuditEvent(t, "incarnation.created", map[string]any{
		"apply_id": applyID,
	})
	// Successful-run metric: keeper_scenario_runs_total{result="ok"}
	// (scenario/metrics.go -- closed enum ok/failed/locked, incremented at
	// the run.go terminal).
	stack.AssertMetricGE(t, `keeper_scenario_runs_total{result="ok"}`, 1)
}

// TestValidApplyRunsStatus_PilotValueAccepted -- sanity check: "success" is
// present in harness.validApplyRunsStatus. Catches typos in the fixture
// literal itself (in case someone changes the key literal in asserts.go and
// forgets about "success").
func TestValidApplyRunsStatus_PilotValueAccepted(t *testing.T) {
	statuses := harness.ValidApplyRunsStatuses()
	want := map[string]bool{"success": true, "failed": true, "planned": true}
	for s := range want {
		found := false
		for _, v := range statuses {
			if v == s {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("harness.validApplyRunsStatus does not contain %q; full list: %v", s, statuses)
		}
	}
}

// TestValidApplyRunsStatus_RejectsTypo -- typical typos from ADR-039 § Part D
// must be detected as invalid. We use a pure check via IsValidApplyRunsStatus
// to avoid a fake testing.TB (testing.TB contains a private method and
// cannot be implemented/mocked in other packages).
func TestValidApplyRunsStatus_RejectsTypo(t *testing.T) {
	bad := []string{"succeeded", "done", "ok", "completed"}
	for _, status := range bad {
		if harness.IsValidApplyRunsStatus(status) {
			t.Fatalf("IsValidApplyRunsStatus accepted the invalid %q, expected false", status)
		}
	}
}

// TestValidApplyRunsStatus_CoversApplyRunGoEnum -- drift detector: the
// harness literal of apply_runs.status values must cover exactly the set
// that keeper/internal/applyrun/applyrun.go::ValidStatus considers valid.
//
// Pilot phase: the list is hardcoded from the applyrun.go::Status consts as
// of 2026-05-26 (planned/claimed/running/dispatched/success/failed/cancelled/orphaned/no_match).
// In the L3a-implementation slice -- replaced with importing
// `applyrun.ValidStatus` via a replace in tests/e2e/go.mod (testcontainers
// deps don't leak, and the applyrun contract import is lightweight, only
// pulling in pgx-string types).
//
// The test does not do a real check against applyrun.go (that would require
// an import); it is pinned to the expected literal, and when the
// keeper-side enum changes, expected must be updated HERE + validApplyRunsStatus
// in harness/asserts.go. Drift is caught by a plain `go test -tags=e2e ./...`
// (if a value ends up in expected that's missing from harness -- this test fails).
func TestValidApplyRunsStatus_CoversApplyRunGoEnum(t *testing.T) {
	expected := []string{
		"planned",
		"claimed",
		"running",
		"dispatched",
		"success",
		"failed",
		"cancelled",
		"orphaned",
		"no_match",
	}
	got := harness.ValidApplyRunsStatuses()

	sort.Strings(expected)
	sort.Strings(got)

	if len(expected) != len(got) {
		t.Fatalf("length drift: expected=%d got=%d (expected=%v got=%v)", len(expected), len(got), expected, got)
	}
	for i := range expected {
		if expected[i] != got[i] {
			t.Fatalf("drift at index %d: expected=%q got=%q (full expected=%v, full got=%v)", i, expected[i], got[i], expected, got)
		}
	}
}
