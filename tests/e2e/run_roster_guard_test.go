//go:build e2e

// L3a contract-e2e: the pre-flight assert gate on the RUN path (NIM-270).
//
// WHAT THIS PINS. NIM-235 established that a topology `assert:` cannot be
// answered on the create path — pre-flight runs before `incarnation.Create`, and
// since NIM-124 membership FKs that row, so there is no roster to measure. That
// left the form-A promise of the ADR-009 amendment 2026-06-23 ("422 to the
// operator, no incarnation, no error_locked") with nowhere to live: the flow
// that HAS a real roster is the operator's bind-then-run (ADR-008 amendment /
// NIM-209), and RunTyped called only ValidateInput. A topology mismatch there
// always became an `error_locked` plus a manual unlock.
//
// The gate now sits on both paths, and this test holds the two halves apart:
//
//  1. roster bound, topology does NOT converge -> 422 assert-failed, the run
//     never starts, the incarnation stays `ready` (no `applying`, no
//     `error_locked`, no apply_runs row);
//  2. roster bound, topology converges -> 202 and the run succeeds;
//  3. a plan that BUILDS its own roster, run explicitly on an EMPTY roster ->
//     202, not 422. This is the regression that matters most: gating such a
//     plan on the roster it has at request time would be NIM-235 inverted, and
//     it is why the gate shares its bypass predicate with run.go §3.
package e2e_test

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
)

func TestE2ERunRosterGuard_PreflightOnRunPath(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/create-roster-guard",
		Souls:       3,
	})
	defer stack.Cleanup()

	stack.RegisterService(t, "create-roster-guard", "examples/service/create-roster-guard")

	ips := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}
	for i := range ips {
		stub := stack.ConnectSoulStub(t, i)
		stub.SetApplyDefaultSuccess(true)
		stack.SeedSoulprint(t, i, map[string]any{
			"os": map[string]any{
				"family":      "debian",
				"distro":      "debian",
				"version":     "12",
				"arch":        "amd64",
				"pkg_mgr":     "apt",
				"init_system": "systemd",
			},
			"network": map[string]any{"primary_ip": ips[i]},
		})
	}

	// ── Case 3 first: a roster-building plan run explicitly on an EMPTY roster.
	// The incarnation row is seeded with no members at all; `create` carries a
	// refresh emitter, so run.go §3 lets it start and the gate must agree.
	// Running it first also warms the service-registry snapshot, which is why
	// RunScenarioRaw below needs no transient-422 polling.
	const incName = "run-roster-guard"
	stack.SeedIncarnationReady(t, incName, "create-roster-guard", "main", map[string]any{})

	sids := make([]string, 0, len(ips))
	for i := range ips {
		sids = append(sids, stack.SoulSID(i))
	}
	buildApply := stack.RunScenario(t, incName, "create", map[string]any{
		"roster_sids":  sids,
		"expect_hosts": 3,
	})
	stack.WaitApplySuccess(t, buildApply, 120)
	stack.WaitIncarnationReady(t, incName, 60)

	// ── Case 1: the roster is now bound (3 hosts) and `verify_roster` builds
	// nothing, so the gate answers up front. Ask for 2 against a roster of 3.
	body, status := stack.RunScenarioRaw(t, incName, "verify_roster", map[string]any{"expect_hosts": 2})
	if status != 422 {
		t.Fatalf("mismatched topology on a bound roster: status %d, want 422 — the gate did not answer on the run path (body=%s)", status, string(body))
	}
	if !strings.Contains(string(body), "assert-failed") {
		t.Errorf("422 body does not carry the assert-failed type: %s", string(body))
	}
	if !strings.Contains(string(body), "expect_hosts") {
		t.Errorf("422 body does not carry the author's message: %s", string(body))
	}

	// The whole point of answering on the request path: nothing moved. Not
	// `applying`, not `error_locked` — and no run to unlock.
	gotStatus, details := stack.IncarnationStatusDetails(t, incName)
	if gotStatus != "ready" {
		t.Errorf("incarnation status = %q, want ready — a rejected pre-flight must not start or lock the run (details=%s)", gotStatus, details)
	}

	// ── Case 2: the same scenario with the topology it actually has -> the run
	// proceeds. This is the no-false-positive half: without it, case 1 would
	// also pass if the gate rejected everything.
	okApply := stack.RunScenario(t, incName, "verify_roster", map[string]any{"expect_hosts": 3})
	stack.WaitApplySuccess(t, okApply, 60)
	stack.WaitIncarnationReady(t, incName, 60)
}
