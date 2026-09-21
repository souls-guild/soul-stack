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
//  2. CreateIncarnationOnRoster `test-nginx-live` on top of `smoke-nginx-live@main`:
//     seed the incarnation row, bind the soul as a member, then
//  3. run `create` with input.hostname=soul-live-a.example.com.
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

	// Seed the incarnation row -> bind the roster -> run `create`, all inside
	// CreateIncarnationOnRoster (NIM-192). The bootstrap flow set
	// souls.status='connected', but no membership is bound, and the run's roster
	// resolves members via incarnation_membership
	// (ADR-008 amendment/NIM-124, topology/resolver.go::rosterSQL) - an unbound
	// roster is no_hosts -> zero apply_runs rows -> WaitApplySuccess timeout.
	// Membership cannot precede the row (FK, migration 099), which is why the
	// row is seeded rather than POSTed: POST /v1/incarnations would start the
	// create run in the same call, before anything can be bound.
	inc, applyID := stack.CreateIncarnationOnRoster(t, incName, "smoke-nginx-live@main", "create", []int{0}, map[string]any{
		"hostname": wantSID,
	})

	// 300s - apt-get update + apt-get install nginx + systemctl start
	// on a fresh Debian-12 container on a busy CI machine. The README
	// records the expected run time (~3-5 minutes).
	stack.WaitApplySuccess(t, applyID, 300)

	// apply_runs success != every capture is in: a core.state.<verb> step standing
	// after the host work commits after those hosts report success ([ADR-0084]).
	// Without this wait, AssertIncarnationState reads empty state in the race window.
	stack.WaitIncarnationReady(t, inc, 30)

	// YAML loader (L3b-5): apply_runs / incarnation_state / audit_events /
	// metrics / host_state - one source of truth (smoke-nginx-live/expectations
	// /after-create.yaml). Symmetric to the L3a fixture format (see docs/testing/e2e.md).
	exp := harness.LoadExpectations(t, "smoke-nginx-live/expectations/after-create.yaml")
	stack.AssertExpectations(t, exp, applyID, inc)

	// apply_id in the audit event payload is a runtime value, not expressible via
	// the YAML fixture; checked separately after AssertExpectations. The event is
	// `incarnation.scenario_started`, not `incarnation.created`: the row is seeded
	// and create is an explicit run (NIM-192 bootstrap order). POST
	// /v1/incarnations and its `incarnation.created` payload stay covered by L3a
	// (tests/e2e/smoke_nginx_test.go) and the handler unit tests.
	stack.AssertAuditEvent(t, "incarnation.scenario_started", map[string]any{
		"scenario": "create",
		"apply_id": applyID,
	})
}
