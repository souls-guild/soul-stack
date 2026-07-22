//go:build e2e

// L3a-MK: presence-gated force-release of the SID-lease on a REAL keeper crash.
//
// Proves END-TO-END finding #2 (eventstream.lease_force_released, ADR-027
// amend (n)) on a real SIGKILL of the stream-holder keeper process --
// a re-proof of finding #3 from the WardRoster test, now WITHOUT the
// 60s SID-lease-TTL wait and without reconnect spam:
//
//  1. Cluster of 2 keepers over SHARED PG/Redis/Vault, acolytes>0.
//  2. soul-stub (SID) in hold-apply+reconnect mode is connected to primary
//     keeper-A (= stream holder, holds the Redis SID-lease soul:<sid>:lock
//     with value A).
//  3. incarnation.run(create) -> dispatched (stub holds the ApplyRequest).
//     The stub "restarts" (ClearActiveWard) -- after reconnect it will
//     announce an empty WardRoster.
//  4. * SIGKILL keeper-A. The SID-lease in Redis STAYS owned by A (Release is
//     graceful-only; TTL 60s, defaultSoulLeaseTTL).
//  5. * The stub reconnects the same SID to the live keeper-B (fallback
//     endpoints).
//  6. Wait for A's Conclave presence to expire (~30s DefaultConclaveTTL) --
//     then InstanceAlive(A)=false.
//  7. * ASSERT: keeper-B does a presence-gated force-release
//     (ForceAcquireSoulLease CAS-by-prev-holder) INSTEAD OF an AlreadyExists
//     rejection -> the stream opens (the stub gets a SECOND HelloReply) ->
//     handleWardRoster is reached -> the dispatched-orphan is reconciled into
//     `orphaned`. The audit event eventstream.lease_force_released is
//     recorded {sid, prev_kid=A, new_kid=B}. Invisibility window < 60s (in
//     practice <= ~Conclave-TTL 30s, NOT the full 60s SID-lease TTL -- this is
//     exactly what eliminates finding #2).
package e2e_test

import (
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
	"github.com/souls-guild/soul-stack/tests/e2e/internal/soulstub"
)

// countHelloReplies returns the number of HelloReply frames received by the
// stub over the lifetime of the stream(s). The first is for the initial Open
// to the holder; the second+ is for a successful reconnect to a live keeper
// after force-release (the stream reopened).
func countHelloReplies(s *soulstub.Stub) int {
	n := 0
	for _, m := range s.Messages() {
		if m.Kind == "HelloReply" {
			n++
		}
	}
	return n
}

func TestE2E_MultiKeeper_PresenceGatedLeaseForceReleaseAfterCrash(t *testing.T) {
	const (
		keepers     = 2
		serviceName = "service-noop"
		examplePath = "examples/service/noop"
		incarnation = "lfr-orphan-inc"
		scenario    = "create"
	)

	stack := harness.NewMultiKeeperStack(t, harness.MultiKeeperConfig{
		Keepers:        keepers,
		Souls:          1,
		VoyageLeaseTTL: 4 * time.Second,
	})
	defer stack.Cleanup()

	stack.RegisterService(t, serviceName, examplePath)

	// Stub in hold-apply + reconnect+WardRoster mode. Connected to the
	// primary (keeper-A) -- the stream holder (holds the SID-lease).
	soulStub := stack.ConnectSoulStubReconnect(t, 0, true)
	sid := stack.SoulSID(0)
	holderKID := stack.StreamHolderKID(t)
	if helloN := countHelloReplies(soulStub); helloN != 1 {
		t.Fatalf("LFR: expected exactly 1 HelloReply after initial-Open, got %d", helloN)
	}
	t.Logf("LFR: soul-stub %s holds the stream to %s (holder/lease-owner), endpoints=%v", sid, holderKID, stack.AllKeeperGRPCAddrs())

	// Ready incarnation with a single connected host in its coven.
	stack.SeedIncarnationReady(t, incarnation, serviceName, "main", map[string]any{})
	stack.AddMember(t, 0, incarnation)

	// incarnation.run(create): the Acolyte claims planned->dispatched->SendApply
	// into the holder's stream. The stub holds the ApplyRequest -> the row hangs
	// at dispatched.
	applyID := stack.RunScenario(t, incarnation, scenario, nil)
	t.Logf("LFR: incarnation.run(%s) apply_id=%s", scenario, applyID)

	got := stack.WaitApplyRunStatusForSID(t, applyID, sid, []string{"dispatched"}, 30*time.Second)
	t.Logf("LFR: apply_runs(%s,%s).status=%q -- the task was handed to the holder, RunResult held back", applyID, sid, got)

	// Soul-process "restart": there is physically nothing in-flight -> the
	// WardRoster on reconnect will announce an empty set, keeper-B will orphan
	// the dispatched row.
	soulStub.ClearActiveWard()

	// * Mark the moment the holder crashes: the invisibility window is measured
	// from here to the successful force-release reconnect (proof of "< 60s,
	// <= ~Conclave-TTL").
	killAt := time.Now()

	// * REAL SIGKILL of the stream-holder keeper. The SID-lease
	// soul:<sid>:lock in Redis STAYS owned by the holder (Release is
	// graceful-only) until TTL ~60s.
	stack.KillKeeperByKID(t, holderKID)
	t.Logf("LFR: SIGKILL %s (holder/lease-owner) sent at t0 -- the stub should reconnect to a live keeper", holderKID)

	live := stack.LiveKeeperKIDs()
	if len(live) != 1 {
		t.Fatalf("LFR: expected exactly 1 live keeper after kill, %d remain (%v)", len(live), live)
	}
	newKeeperKID := live[0]
	t.Logf("LFR: live keeper for force-release reconnect: %s", newKeeperKID)

	// * ASSERT-1: audit eventstream.lease_force_released recorded with
	// new_kid=live keeper. This only happens AFTER the killed holder's
	// Conclave presence expires (~30s) -- while it is still alive in the
	// Conclave, force-release fail-safe-rejects (AlreadyExists). We wait up
	// to 55s: < 60s SID-lease TTL -- proof that reconnect did NOT wait for
	// the full lease expiry (finding #2 eliminated). If force-release did
	// not work, the event would never appear and reconnect would stall until
	// the 60s lease expiry (the original finding #3 behavior).
	gotNewKID := stack.WaitLeaseForceReleased(t, sid, []string{newKeeperKID}, 55*time.Second)
	elapsed := time.Since(killAt)
	t.Logf("LFR: * audit eventstream.lease_force_released{sid=%s, new_kid=%s} after %s from the holder crash", sid, gotNewKID, elapsed.Round(time.Second))

	// Invisibility window < 60s (SID-lease TTL) -- eliminates finding #2.
	// Conclave-TTL (~30s) is the by-design lower bound (presence must
	// expire); the margin covers reaper tick/reconnect-backoff/sweep. Hard
	// upper bound 60s = lease TTL: if reconnect took >=60s, that would be
	// the original finding's behavior (waiting for the full TTL).
	if elapsed >= 60*time.Second {
		t.Fatalf("LFR: invisibility window %s >= 60s (SID-lease TTL) -- force-release did NOT eliminate the 60s wait (finding #2 NOT closed)", elapsed)
	}

	// ASSERT-2: prev_kid in the audit = the killed holder (the takeover is
	// attributed to the dead owner, not to a random KID).
	prevKID := stack.AuditPayloadField(t, "eventstream.lease_force_released", "sid", sid, "prev_kid")
	if prevKID != holderKID {
		t.Fatalf("LFR: audit prev_kid=%q, expected the killed holder %q (force-release attributed to the wrong owner)", prevKID, holderKID)
	}

	// ASSERT-3: the stream REALLY reopened on the live keeper -- the stub got
	// a SECOND HelloReply (reconnect handshake complete, handleWardRoster
	// reachable).
	stack.Eventually(t, 10*time.Second, func() bool {
		return countHelloReplies(soulStub) >= 2
	}, "the stub did not receive a second HelloReply after the force-release reconnect")
	t.Logf("LFR: * the stub received a second HelloReply -- the stream is open on %s via force-release (handleWardRoster reached)", newKeeperKID)

	// ASSERT-4: handleWardRoster reconciled the dispatched-orphan into
	// `orphaned`. This is the final proof of the end-to-end path
	// crash->force-release->reconnect->roster->terminal, now WITHOUT the 60s
	// wait. The window is short (the stream is already open): WardRoster is
	// sent RIGHT AFTER Hello, keeper terminates the dispatched row
	// immediately.
	final := stack.WaitApplyRunStatusForSID(t, applyID, sid, []string{"orphaned"}, 15*time.Second)
	t.Logf("LFR: * apply_runs(%s,%s).status=%q -- dispatched-orphan reconciled by WardRoster after force-release (END-TO-END)", applyID, sid, final)

	// Terminal durability (single-winner append-only, ADR-027(j)).
	time.Sleep(2 * time.Second)
	if cur := stack.ApplyRunStatusForSID(t, applyID, sid); cur != "orphaned" {
		t.Fatalf("LFR: row status rolled back to %q after orphaned -- single-winner append-only terminal violated", cur)
	}
}
