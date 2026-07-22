//go:build e2e_live

// L3b E2E flagship-smoke: smoke-nginx-live happy-path (ADR-039).
//
// A parallel to tests/e2e/smoke_nginx_test.go (L3a, soul-stub answers scripted),
// but going through a REAL apt-install of nginx inside a Debian-12 soul container.
// Coverage L3a doesn't give: Keeper render -> ApplyRequest on the wire -> real
// soul Apply (core.pkg / core.file.rendered / core.service) -> RunResult ->
// apply_runs success.
//
// Flow:
//  1. NewStack: PG+Redis+Vault testcontainers + Keeper process + 1 privileged
//     debian-12 systemd-PID-1 soul container. The real Bootstrap flow is closed
//     by the L3b-2 slice; here we rely on souls.status = 'connected' already
//     being set after NewStack.
//  2. CreateIncarnation `test-nginx-live` on top of the `smoke-nginx-live@main` service.
//  3. RunScenario `create` with input.hostname=soul-live-a.example.com.
//  4. WaitApplySuccess (timeout 300s - apt-update + install nginx can be
//     slow on a busy CI machine, see the example's README).
//  5. AssertApplyRunsStatus / AssertIncarnationState / AssertAuditEvent /
//     AssertMetricGE - the same contract checks as in L3a.
//
// Container-side asserts - L3b-4: confirm that after apply the nginx package is
// really installed, the systemd unit is active, and the config with server_name got generated.
package e2e_live_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

func TestL3bSmokeNginxLive_InstallAndStart(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/smoke-nginx-live",
		ServiceName: "smoke-nginx-live",
		Souls:       1,
	})
	defer stack.Cleanup()

	if got := len(stack.SoulContainers); got != 1 {
		t.Fatalf("expected 1 soul container, got %d", got)
	}
	const wantSID = "soul-live-a.example.com"
	if sc := stack.SoulContainers[0]; sc.SID != wantSID {
		t.Errorf("SoulContainers[0].SID = %q, expected %q", sc.SID, wantSID)
	}

	const incName = "test-nginx-live"

	// Membership BEFORE Create: the run's roster resolves members via
	// incarnation_membership (ADR-008 amendment/NIM-124, topology/resolver.go::rosterSQL).
	// The bootstrap flow set souls.status='connected', but no membership is bound -
	// without this step the scenario sees no_hosts -> zero apply_runs rows ->
	// WaitApplySuccess timeout. Parallel to L3a (tests/e2e/smoke_nginx_test.go::AddMember).
	stack.AddMember(t, 0, incName)

	// POST /v1/incarnations auto-launches the `create` scenario and returns its
	// apply_id. A separate RunScenario(create) would be rejected by the lock gate
	// ("incarnation already in applying status") and its apply_id would never get any
	// apply_runs rows. We wait for the apply_id of the actual auto-create run (as in L3a).
	inc, applyID := stack.CreateIncarnationWithApply(t, incName, "smoke-nginx-live@main", map[string]any{
		"hostname": wantSID,
	})

	// 300s - apt-get update + apt-get install nginx + systemctl start
	// on a fresh Debian-12 container on a busy CI machine. The README
	// records the expected run time (~3-5 minutes).
	stack.WaitApplySuccess(t, applyID, 300)

	// apply_runs success != incarnation.state committed: state_changes are written in
	// a separate transaction AFTER the barrier (run.go §8, status->ready). Without this
	// wait, AssertIncarnationState reads empty state in the race window.
	stack.WaitIncarnationReady(t, inc, 30)

	// YAML loader (L3b-5): apply_runs / incarnation_state / audit_events /
	// metrics / host_state - one source of truth (smoke-nginx-live/expectations
	// /after-create.yaml). Symmetric to the L3a fixture format (see docs/testing/e2e.md).
	exp := harness.LoadExpectations(t, "smoke-nginx-live/expectations/after-create.yaml")
	stack.AssertExpectations(t, exp, applyID, inc)

	// apply_id in the audit event payload is a runtime value, not expressible via
	// the YAML fixture; checked separately after AssertExpectations. POST
	// /v1/incarnations auto-launches the create scenario and writes `incarnation.created`
	// (huma_incarnation_op.go) with the auto-run's apply_id in the payload - the same apply_id
	// as in WaitApplySuccess. `incarnation.scenario_started` is only written for an
	// explicit RunScenario, which isn't called here (like L3a smoke_nginx_test.go).
	stack.AssertAuditEvent(t, "incarnation.created", map[string]any{
		"apply_id": applyID,
	})
}
