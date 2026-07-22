package redis

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func newHeraldQueueMR(t *testing.T) (*HeraldDeliveryQueue, *miniredis.Miniredis) {
	t.Helper()
	c, mr := newClientMR(t)
	q, err := NewHeraldDeliveryQueue(c)
	if err != nil {
		t.Fatalf("NewHeraldDeliveryQueue: %v", err)
	}
	return q, mr
}

// idParse extracts the id from a payload of the form `id:<id>` (test format).
func idParse(payload []byte) (string, bool) {
	s := string(payload)
	if !strings.HasPrefix(s, "id:") {
		return "", false
	}
	return strings.TrimPrefix(s, "id:"), true
}

func TestHeraldQueue_EnqueueClaimAck(t *testing.T) {
	q, _ := newHeraldQueueMR(t)
	ctx := context.Background()

	if err := q.Enqueue(ctx, []byte("id:j1")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	claimed, err := q.Claim(ctx, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if claimed == nil || string(claimed.Payload) != "id:j1" {
		t.Fatalf("Claim = %+v, want id:j1", claimed)
	}
	if err := q.SetLease(ctx, "j1", time.Second); err != nil {
		t.Fatalf("SetLease: %v", err)
	}
	if err := q.Ack(ctx, "j1", claimed.Payload); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	// After Ack, processing is empty, so the next Claim times out (nil, nil).
	next, err := q.Claim(ctx, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("Claim after ack: %v", err)
	}
	if next != nil {
		t.Fatalf("expected empty queue after ack, got %+v", next)
	}
}

func TestHeraldQueue_ClaimEmpty_TimesOut(t *testing.T) {
	q, _ := newHeraldQueueMR(t)
	claimed, err := q.Claim(context.Background(), 50*time.Millisecond)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if claimed != nil {
		t.Fatalf("empty queue must yield nil, got %+v", claimed)
	}
}

func TestHeraldQueue_Requeue_MovesBackToPending(t *testing.T) {
	q, _ := newHeraldQueueMR(t)
	ctx := context.Background()

	_ = q.Enqueue(ctx, []byte("id:r1"))
	claimed, _ := q.Claim(ctx, 100*time.Millisecond)
	_ = q.SetLease(ctx, "r1", time.Second)

	if err := q.Requeue(ctx, "r1", claimed.Payload, []byte("id:r1-retry")); err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	// The requeued job can be claimed again.
	re, err := q.Claim(ctx, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("Claim after requeue: %v", err)
	}
	if re == nil || string(re.Payload) != "id:r1-retry" {
		t.Fatalf("requeued job = %+v, want id:r1-retry", re)
	}
}

// TestHeraldQueue_RequeueExpired_ReclaimsOrphan — a claimed job with an expired
// lease key gets returned to pending by the mini-reaper.
func TestHeraldQueue_RequeueExpired_ReclaimsOrphan(t *testing.T) {
	q, mr := newHeraldQueueMR(t)
	ctx := context.Background()

	_ = q.Enqueue(ctx, []byte("id:orphan"))
	if _, err := q.Claim(ctx, 100*time.Millisecond); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	_ = q.SetLease(ctx, "orphan", time.Second)

	// Lease is still alive → reaper does NOT touch it.
	n, err := q.RequeueExpired(ctx, idParse)
	if err != nil {
		t.Fatalf("RequeueExpired (live lease): %v", err)
	}
	if n != 0 {
		t.Fatalf("live-lease job must not be requeued, got %d", n)
	}

	// Expire the lease (miniredis FastForward advances the virtual clock).
	mr.FastForward(2 * time.Second)

	n, err = q.RequeueExpired(ctx, idParse)
	if err != nil {
		t.Fatalf("RequeueExpired (expired lease): %v", err)
	}
	if n != 1 {
		t.Fatalf("orphaned job must be requeued, got %d", n)
	}
	// The reclaimed job is available for claim again.
	re, _ := q.Claim(ctx, 100*time.Millisecond)
	if re == nil || string(re.Payload) != "id:orphan" {
		t.Fatalf("reclaimed job = %+v, want id:orphan", re)
	}
}

// TestHeraldQueue_RequeueExpired_DropsUnparsable — a corrupt payload in
// processing with no id is removed without requeuing (mini-reaper doesn't loop).
func TestHeraldQueue_RequeueExpired_DropsUnparsable(t *testing.T) {
	q, _ := newHeraldQueueMR(t)
	ctx := context.Background()

	_ = q.Enqueue(ctx, []byte("garbage-no-id"))
	_, _ = q.Claim(ctx, 100*time.Millisecond)

	n, err := q.RequeueExpired(ctx, idParse)
	if err != nil {
		t.Fatalf("RequeueExpired: %v", err)
	}
	if n != 0 {
		t.Fatalf("unparsable job is dropped, not requeued, got %d", n)
	}
	// processing is empty, pending is empty → claim times out.
	next, _ := q.Claim(ctx, 50*time.Millisecond)
	if next != nil {
		t.Fatalf("unparsable job must be removed, got %+v", next)
	}
}
