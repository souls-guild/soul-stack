//go:build e2e

// Per-section E2E: the Keeper-side `dry_run` capability gate on the Errand
// dispatch path (ADR-0076(i) / ADR-033 Amendment 2026-08-06, NIM-456).
//
// `dry_run` promises a pure read — the Soul calls SoulModule.Plan instead of
// Apply (ADR-031(b)/(c)). The Soul keeps that promise, not Keeper, so Keeper
// refuses the dispatch unless the target announced the `dry_run` capability.
// The unit tests in keeper/internal/errand cover the gate's logic against a
// stub checker; what they cannot cover is the part that only exists in a
// running cluster:
//
//   - the daemon actually wires a non-nil capability checker (a nil one is
//     fail-closed, so a missing wire-up would refuse EVERY dry_run — and since
//     no other test sends one, nothing else would notice);
//   - the checker reads the same Redis heartbeat field the Soul's Hello wrote,
//     so keeper's notion of "announced" matches the Soul's notion of
//     "announcing".
//
// Both are asserted here by the fact that a dry_run gets THROUGH: the stub
// announces the full capability set of its build (config.SoulCapabilities),
// which contains `dry_run`, so a correctly wired gate must not stand in its way.
//
// Limitation, and it is the interesting one: the soul-stub ignores `dry_run` and
// echoes SUCCESS (L3a contract — it executes no modules). That is precisely the
// behaviour of the old binary this gate exists to stop, which is why the
// assertion here is about the GATE rather than about Plan-vs-Apply. Whether a
// real Soul honours the flag is an L3b question.
//
// ⚠ The harness runs a PREBUILT `keeper/bin/keeper` (locateKeeperBinary), not the
// source in the working tree — so this case only says something about the code you
// are editing after a `make build`. Verified: with the checker deliberately left
// nil in the daemon wire-up, a stale binary still passes here and a rebuilt one
// fails with 409. `make check-all` builds before running the tier; a hand-run
// `go test -tags=e2e` does not.
package e2e_test

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
)

func TestErrandDryRun_AnnouncedCapabilityPassesTheGate(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/noop",
		Souls:       1,
	})
	defer stack.Cleanup()

	stack.ConnectSoulStub(t, 0)
	sid := stack.SoulSID(0)

	// A plain Errand first: it never consults the checker, so if THIS fails the
	// problem is not the gate and the dry_run result below would be misread.
	if res := stack.ExecErrand(t, sid, "core.cmd.shell", map[string]any{"cmd": "echo ok"}); res.Status != "success" {
		t.Fatalf("plain ExecErrand: status=%q, want success (a non-dry-run Errand must not be gated)", res.Status)
	}

	// The gate's own path. 409 here means keeper could not confirm what the stub
	// plainly announced — either no checker was wired, or it is not reading the
	// heartbeat field Hello populates.
	status, body := stack.ExecErrandRaw(t, sid, "core.cmd.shell", map[string]any{"cmd": "echo ok"}, true)
	if status == 409 {
		t.Fatalf("dry_run refused with 409 against a stub that announces dry_run: the capability gate is "+
			"misconfigured (nil checker, or reading a different heartbeat field than Hello writes). body=%s", body)
	}
	if status != 200 {
		t.Fatalf("dry_run: status=%d, want 200; body=%s", status, body)
	}
	if !strings.Contains(body, `"status":"success"`) {
		t.Fatalf("dry_run: body does not carry a terminal success (the stub echoes SUCCESS for any "+
			"ErrandRequest, so anything else means the request did not reach it): %s", body)
	}
}

// TestErrandDryRun_NeverConnectedHost_Is404NotACapabilityRefusal — the diagnosis
// half, against real Redis, which is where the trap lives. `SoulsLackingCapability`
// HGETs the caps field and treats redis.Nil as lacking, so a host with NO heartbeat
// hash at all is indistinguishable from an agent that announced an older capability
// set. Left at that, a typo'd SID answers "this host's binary does not announce
// dry_run — update it", advice about a host that was never there, while both the
// OpenAPI contract and errands.md promise 404 for a Soul that is not connected.
//
// Only a live cluster exercises the conflation: the unit guard uses a stub checker
// whose "lacking" is set by hand, so it cannot reproduce the redis.Nil path that
// creates the ambiguity in the first place.
func TestErrandDryRun_NeverConnectedHost_Is404NotACapabilityRefusal(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/noop",
		Souls:       1,
	})
	defer stack.Cleanup()

	// One Soul connected, so the cluster is healthy and Redis holds a presence
	// record — for a DIFFERENT host. The SID below is well-formed and unknown:
	// no heartbeat hash, no lease.
	stack.ConnectSoulStub(t, 0)

	status, body := stack.ExecErrandRaw(t, "ghost-host.example.com", "core.cmd.shell",
		map[string]any{"cmd": "echo ok"}, true)
	if status == 409 {
		t.Fatalf("a never-connected host answered 409 soul-capability-unsupported: its absent heartbeat hash was "+
			"read as an outdated announcement, so the operator is told to upgrade a binary on a host that does not "+
			"exist. Want 404 (documented for a Soul that is not connected). body=%s", body)
	}
	if status != 404 {
		t.Fatalf("dry_run to an unknown SID: status=%d, want 404; body=%s", status, body)
	}
	if !strings.Contains(body, "not-found") {
		t.Errorf("404 body does not carry the not-found problem type: %s", body)
	}
}
