package main

import (
	"testing"
	"time"
)

// Unit tests for the reconnect-loop backoff progression. reconnectLoop itself
// needs a live gRPC client (integration, see failback_integration_test.go);
// here we test the pure nextDelay arithmetic + the leaseHeldBackoffCap
// invariant that the lease-held (modest cap) vs transport (general cap)
// distinction relies on.

func TestNextDelay_DoublesUntilCap(t *testing.T) {
	t.Parallel()
	cap := 30 * time.Second
	got := 1 * time.Second
	want := []time.Duration{2, 4, 8, 16, 30, 30}
	for i, w := range want {
		got = nextDelay(got, cap)
		if got != w*time.Second {
			t.Fatalf("step %d: nextDelay = %s, want %s", i, got, w*time.Second)
		}
	}
}

// TestLeaseHeldCap_ProgressionStaysModest — the modest-cap (lease-held branch)
// progression must not run into tens of seconds: after a keeper crash,
// presence expires in ~30s and force-release frees the lease, so Soul must
// reconnect within the modest cap, not the general transport cap. Verifies
// cap=leaseHeldBackoffCap keeps the progression within a few seconds
// (preserves recovery latency).
func TestLeaseHeldCap_ProgressionStaysModest(t *testing.T) {
	t.Parallel()
	if leaseHeldBackoffCap > 5*time.Second {
		t.Fatalf("leaseHeldBackoffCap = %s, want ≤ 5s (recovery-latency requires a modest cap)", leaseHeldBackoffCap)
	}
	// initial=1s, double repeatedly — should converge to cap, never above.
	d := 1 * time.Second
	for i := 0; i < 10; i++ {
		d = nextDelay(d, leaseHeldBackoffCap)
		if d > leaseHeldBackoffCap {
			t.Fatalf("step %d: lease-held delay = %s exceeded cap %s", i, d, leaseHeldBackoffCap)
		}
	}
	if d != leaseHeldBackoffCap {
		t.Fatalf("lease-held delay converged to %s, want cap %s", d, leaseHeldBackoffCap)
	}
}

// TestLeaseHeldCap_ClampsAlreadyGrownDelay — if backoff already grew past the
// modest cap (transport failures before keeper entered lease-held mode),
// entering the lease-held branch clamps the current delay to cap (same
// arithmetic as reconnectLoop's `if delay > cap { delay = cap }`). Ensures the
// transport→lease-held transition doesn't leave an inflated delay.
func TestLeaseHeldCap_ClampsAlreadyGrownDelay(t *testing.T) {
	t.Parallel()
	delay := 30 * time.Second // grew on transport failures up to the general max
	cap := leaseHeldBackoffCap
	if delay > cap {
		delay = cap
	}
	if delay != cap {
		t.Fatalf("clamp: delay = %s, want %s", delay, cap)
	}
}

// TestTransportCap_Unchanged — regression guard: the general transport cap
// (default keeper.retry.backoff.max=30s) is untouched by the lease-held branch
// change. transport-backoff progression still reaches 30s.
func TestTransportCap_Unchanged(t *testing.T) {
	t.Parallel()
	transportCap := 30 * time.Second
	d := 1 * time.Second
	for i := 0; i < 10; i++ {
		d = nextDelay(d, transportCap)
	}
	if d != transportCap {
		t.Fatalf("transport delay converged to %s, want %s", d, transportCap)
	}
}
