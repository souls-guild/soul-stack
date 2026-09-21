//go:build integration

// Integration test for the Tempo token bucket (ADR-050) against a real
// redis:7 via testcontainers-go. A real Redis is required (not miniredis):
// the primitive reads time via `redis.call("TIME")`, and miniredis's Lua
// doesn't emulate refill-over-time correctly — the refill/atomicity tests
// would lose their meaning.
//
// The container and `integrationAddr` are set up by the shared TestMain
// (integration_test.go).
//
// Run:
//
//	cd keeper && TESTCONTAINERS_RYUK_DISABLED=true \
//	    SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1 \
//	    go test -tags=integration -race -count=1 ./internal/redis/...

package redis

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func newTokenBucketInt(t *testing.T) *TokenBucket {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := NewClient(ctx, Config{Addr: integrationAddr}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	tb, err := NewTokenBucket(c)
	if err != nil {
		t.Fatalf("NewTokenBucket: %v", err)
	}
	return tb
}

// uniqueAID returns a unique AID per test so test buckets don't collide in
// the shared Redis container.
func uniqueAID(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("archon-test-%s", t.Name())
}

// TestIntegration_TokenBucket_BurstThenSustained — the burst is let through
// in full, then (without refill) requests are cut off with a positive
// retryAfter.
func TestIntegration_TokenBucket_BurstThenSustained(t *testing.T) {
	tb := newTokenBucketInt(t)
	ctx := context.Background()

	const rate = 1.0 // 1 token/sec — refill is slow, unnoticeable within the test window
	const burst = 5

	aid := uniqueAID(t)

	// The first burst requests go through.
	for i := 0; i < burst; i++ {
		allowed, retry, err := tb.Allow(ctx, aid, "voyage_create", rate, burst)
		if err != nil {
			t.Fatalf("Allow #%d: %v", i, err)
		}
		if !allowed {
			t.Fatalf("Allow #%d: expected allow within burst, got deny (retry=%v)", i, retry)
		}
		if retry != 0 {
			t.Fatalf("Allow #%d: on allow retryAfter must be 0, got %v", i, retry)
		}
	}

	// burst+1 — bucket empty, deny + positive retryAfter.
	allowed, retry, err := tb.Allow(ctx, aid, "voyage_create", rate, burst)
	if err != nil {
		t.Fatalf("Allow over burst: %v", err)
	}
	if allowed {
		t.Fatal("Allow over burst: expected deny, got allow")
	}
	if retry <= 0 {
		t.Fatalf("Allow over burst: retryAfter must be > 0, got %v", retry)
	}
	// At rate=1, the next token is ~1s away; allow some slack.
	if retry > 2*time.Second {
		t.Fatalf("Allow over burst: retryAfter unexpectedly large: %v", retry)
	}
}

// TestIntegration_TokenBucket_RefillOverTime — after exhaustion, the bucket
// recovers over time: after waiting, allow succeeds again.
func TestIntegration_TokenBucket_RefillOverTime(t *testing.T) {
	tb := newTokenBucketInt(t)
	ctx := context.Background()

	const rate = 10.0 // 10 tokens/sec → 1 token per 100ms
	const burst = 2

	aid := uniqueAID(t)

	// Drain the bucket.
	for i := 0; i < burst; i++ {
		if allowed, _, err := tb.Allow(ctx, aid, "voyage_create", rate, burst); err != nil || !allowed {
			t.Fatalf("Allow drain #%d: allowed=%v err=%v", i, allowed, err)
		}
	}
	if allowed, _, err := tb.Allow(ctx, aid, "voyage_create", rate, burst); err != nil || allowed {
		t.Fatalf("Allow after drain: expected deny, allowed=%v err=%v", allowed, err)
	}

	// Wait comfortably longer than one refill interval (1 token = 100ms at rate=10).
	time.Sleep(300 * time.Millisecond)

	allowed, retry, err := tb.Allow(ctx, aid, "voyage_create", rate, burst)
	if err != nil {
		t.Fatalf("Allow after refill: %v", err)
	}
	if !allowed {
		t.Fatalf("Allow after refill: expected allow (bucket refilled), got deny (retry=%v)", retry)
	}
}

// TestIntegration_TokenBucket_IsolationByKey — different AIDs and different
// bucket names don't share one bucket.
func TestIntegration_TokenBucket_IsolationByKey(t *testing.T) {
	tb := newTokenBucketInt(t)
	ctx := context.Background()

	const rate = 1.0
	const burst = 2

	aidA := uniqueAID(t) + "-a"
	aidB := uniqueAID(t) + "-b"

	// Fully drain AID-A's bucket.
	for i := 0; i < burst; i++ {
		if allowed, _, err := tb.Allow(ctx, aidA, "voyage_create", rate, burst); err != nil || !allowed {
			t.Fatalf("drain A #%d: allowed=%v err=%v", i, allowed, err)
		}
	}
	if allowed, _, err := tb.Allow(ctx, aidA, "voyage_create", rate, burst); err != nil || allowed {
		t.Fatalf("A over burst: expected deny, allowed=%v err=%v", allowed, err)
	}

	// AID-B — independent bucket, the first request goes through.
	if allowed, _, err := tb.Allow(ctx, aidB, "voyage_create", rate, burst); err != nil || !allowed {
		t.Fatalf("B first: expected allow (different AID), allowed=%v err=%v", allowed, err)
	}

	// Same AID-A, but a different bucket — also independent.
	if allowed, _, err := tb.Allow(ctx, aidA, "voyage_preview", rate, burst); err != nil || !allowed {
		t.Fatalf("A other-bucket: expected allow (different bucket), allowed=%v err=%v", allowed, err)
	}
}

// TestIntegration_TokenBucket_CreateVsPreviewSeparate — the ADR-050
// amendment 2026-06-17 INVARIANT via REAL Redis: voyage_create and
// voyage_preview are different `tempo:<aid>:<bucket>` keys for the same
// AID, and do NOT share quota. Exhausting create doesn't affect preview,
// and symmetrically.
func TestIntegration_TokenBucket_CreateVsPreviewSeparate(t *testing.T) {
	tb := newTokenBucketInt(t)
	ctx := context.Background()

	const rate = 1.0 // slow refill — unnoticeable within the test window
	const burst = 1
	aid := uniqueAID(t)

	// Drain the create bucket (burst=1): first allow, second deny.
	if allowed, _, err := tb.Allow(ctx, aid, "voyage_create", rate, burst); err != nil || !allowed {
		t.Fatalf("create #1: expected allow, allowed=%v err=%v", allowed, err)
	}
	if allowed, _, err := tb.Allow(ctx, aid, "voyage_create", rate, burst); err != nil || allowed {
		t.Fatalf("create #2: expected deny (create exhausted), allowed=%v err=%v", allowed, err)
	}

	// preview for the SAME AID is untouched by create's exhaustion → passes.
	if allowed, _, err := tb.Allow(ctx, aid, "voyage_preview", rate, burst); err != nil || !allowed {
		t.Fatalf("preview #1: expected allow -- preview does not share quota with create, allowed=%v err=%v", allowed, err)
	}
	// preview exhausted by its own burst → deny.
	if allowed, _, err := tb.Allow(ctx, aid, "voyage_preview", rate, burst); err != nil || allowed {
		t.Fatalf("preview #2: expected deny (own preview bucket exhausted), allowed=%v err=%v", allowed, err)
	}

	// Symmetry on a fresh AID: exhausting preview doesn't touch create.
	aid2 := uniqueAID(t) + "-sym"
	if allowed, _, err := tb.Allow(ctx, aid2, "voyage_preview", rate, burst); err != nil || !allowed {
		t.Fatalf("sym preview #1: expected allow, allowed=%v err=%v", allowed, err)
	}
	if allowed, _, err := tb.Allow(ctx, aid2, "voyage_preview", rate, burst); err != nil || allowed {
		t.Fatalf("sym preview #2: expected deny, allowed=%v err=%v", allowed, err)
	}
	if allowed, _, err := tb.Allow(ctx, aid2, "voyage_create", rate, burst); err != nil || !allowed {
		t.Fatalf("sym create #1: expected allow -- create does not share quota with preview, allowed=%v err=%v", allowed, err)
	}
}

// TestIntegration_TokenBucket_RejectsInvalidArgs — invalid arguments are
// rejected with an error, not silently.
func TestIntegration_TokenBucket_RejectsInvalidArgs(t *testing.T) {
	tb := newTokenBucketInt(t)
	ctx := context.Background()

	cases := []struct {
		name   string
		aid    string
		bucket string
		rate   float64
		burst  int
	}{
		{"empty_aid", "", "voyage_create", 1, 1},
		{"empty_bucket", "archon-x", "", 1, 1},
		{"zero_rate", "archon-x", "voyage_create", 0, 1},
		{"negative_rate", "archon-x", "voyage_create", -1, 1},
		{"zero_burst", "archon-x", "voyage_create", 1, 0},
		{"negative_burst", "archon-x", "voyage_create", 1, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := tb.Allow(ctx, tc.aid, tc.bucket, tc.rate, tc.burst); err == nil {
				t.Errorf("Allow(%+v) returned nil err; expected a validation error", tc)
			}
		})
	}
}
