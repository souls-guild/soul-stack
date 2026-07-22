package grpc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// newClusterRedis spins up a miniredis instance and wraps it in a
// [keeperredis.Client]. Cleanup is registered via t.Cleanup.
func newClusterRedis(t *testing.T) *keeperredis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := keeperredis.NewClient(ctx, keeperredis.Config{Addr: mr.Addr()}, nil)
	if err != nil {
		t.Fatalf("redis NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestOutbound_Cluster_PublishesToRemoteHolder — Keeper-A.SendApply
// for a SID whose lease is held by Keeper-B must:
//   - find an empty local lookup;
//   - publish to `outbound:<sid>` (1 subscriber, raised separately);
//   - return no error.
func TestOutbound_Cluster_PublishesToRemoteHolder(t *testing.T) {
	r := newClusterRedis(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Keeper-B holds the lease.
	leaseB, err := keeperredis.AcquireSoulLease(ctx, r, "host.example.com", "kid-B", 30*time.Second)
	if err != nil {
		t.Fatalf("AcquireSoulLease(B): %v", err)
	}
	defer leaseB.Release(ctx)

	// Keeper-B is subscribed to the outbound channel (simulating its EventStream handler).
	sub, err := keeperredis.SubscribeOutbound(ctx, r, "host.example.com", "kid-B", discardLogger(t))
	if err != nil {
		t.Fatalf("SubscribeOutbound: %v", err)
	}
	defer sub.Close()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	// Keeper-A (the current one) — Outbound with no local stream.
	mA := NewStreamManager(discardLogger(t))
	ca := &captureAudit{}
	ob, err := NewOutbound(OutboundDeps{
		Manager:     mA,
		AuditWriter: ca,
		Logger:      discardLogger(t),
		Redis:       r,
		KID:         "kid-A",
	})
	if err != nil {
		t.Fatalf("NewOutbound: %v", err)
	}

	req := &keeperv1.ApplyRequest{ApplyId: "01HABC", Tasks: []*keeperv1.RenderedTask{{Name: "t1"}}}
	if err := ob.SendApply(ctx, "host.example.com", req); err != nil {
		t.Fatalf("SendApply: %v", err)
	}

	select {
	case got := <-sub.Channel():
		applyReq := got.GetApplyRequest()
		if applyReq == nil {
			t.Fatalf("payload = %T, want ApplyRequest", got.GetPayload())
		}
		if applyReq.GetApplyId() != "01HABC" {
			t.Errorf("apply_id = %q, want 01HABC", applyReq.GetApplyId())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive forwarded ApplyRequest within 2s")
	}

	if evs := ca.snapshot(); len(evs) != 1 {
		t.Errorf("audit count = %d, want 1 (apply.dispatched)", len(evs))
	}
}

// TestOutbound_Cluster_NoHolderReturnsNotConnected — if nobody holds the
// lease → ErrSoulNotConnected.
func TestOutbound_Cluster_NoHolderReturnsNotConnected(t *testing.T) {
	r := newClusterRedis(t)
	ctx := context.Background()

	mA := NewStreamManager(discardLogger(t))
	ob, err := NewOutbound(OutboundDeps{
		Manager:     mA,
		AuditWriter: nopAudit{},
		Logger:      discardLogger(t),
		Redis:       r,
		KID:         "kid-A",
	})
	if err != nil {
		t.Fatalf("NewOutbound: %v", err)
	}

	err = ob.SendApply(ctx, "ghost.example.com", &keeperv1.ApplyRequest{ApplyId: "x"})
	if !errors.Is(err, ErrSoulNotConnected) {
		t.Fatalf("err = %v, want ErrSoulNotConnected", err)
	}
}

// TestOutbound_Cluster_LocalLeaseWithoutStream — the lease exists on the
// current kid, but there's no local stream (race: disconnect, lease still
// alive). → ErrSoulNotConnected, no publish.
func TestOutbound_Cluster_LocalLeaseWithoutStream(t *testing.T) {
	r := newClusterRedis(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	leaseA, err := keeperredis.AcquireSoulLease(ctx, r, "sid", "kid-A", 30*time.Second)
	if err != nil {
		t.Fatalf("AcquireSoulLease: %v", err)
	}
	defer leaseA.Release(ctx)

	mA := NewStreamManager(discardLogger(t))
	ob, err := NewOutbound(OutboundDeps{
		Manager:     mA,
		AuditWriter: nopAudit{},
		Logger:      discardLogger(t),
		Redis:       r,
		KID:         "kid-A",
	})
	if err != nil {
		t.Fatalf("NewOutbound: %v", err)
	}

	err = ob.SendApply(ctx, "sid", &keeperv1.ApplyRequest{ApplyId: "x"})
	if !errors.Is(err, ErrSoulNotConnected) {
		t.Fatalf("err = %v, want ErrSoulNotConnected", err)
	}
}

// TestOutbound_Cluster_LocalStreamWins — if a local stream exists, it
// gets the message even when Redis is present (the lease isn't checked).
func TestOutbound_Cluster_LocalStreamWins(t *testing.T) {
	r := newClusterRedis(t)
	ctx := context.Background()

	mA := NewStreamManager(discardLogger(t))
	out := mA.Register("sid")

	ob, err := NewOutbound(OutboundDeps{
		Manager:     mA,
		AuditWriter: nopAudit{},
		Logger:      discardLogger(t),
		Redis:       r,
		KID:         "kid-A",
	})
	if err != nil {
		t.Fatalf("NewOutbound: %v", err)
	}

	if err := ob.SendApply(ctx, "sid", &keeperv1.ApplyRequest{ApplyId: "x"}); err != nil {
		t.Fatalf("SendApply: %v", err)
	}

	select {
	case got := <-out:
		if got.GetApplyRequest() == nil {
			t.Errorf("payload = %T, want ApplyRequest", got.GetPayload())
		}
	case <-time.After(time.Second):
		t.Fatal("local stream did not receive message")
	}
}

// TestOutbound_Cluster_PublishCancelToRemoteHolder — SendCancel is also
// routed through pub/sub.
func TestOutbound_Cluster_PublishCancelToRemoteHolder(t *testing.T) {
	r := newClusterRedis(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	leaseB, err := keeperredis.AcquireSoulLease(ctx, r, "sid", "kid-B", 30*time.Second)
	if err != nil {
		t.Fatalf("AcquireSoulLease(B): %v", err)
	}
	defer leaseB.Release(ctx)

	sub, err := keeperredis.SubscribeOutbound(ctx, r, "sid", "kid-B", discardLogger(t))
	if err != nil {
		t.Fatalf("SubscribeOutbound: %v", err)
	}
	defer sub.Close()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	mA := NewStreamManager(discardLogger(t))
	ca := &captureAudit{}
	ob, err := NewOutbound(OutboundDeps{
		Manager:     mA,
		AuditWriter: ca,
		Logger:      discardLogger(t),
		Redis:       r,
		KID:         "kid-A",
	})
	if err != nil {
		t.Fatalf("NewOutbound: %v", err)
	}

	if err := ob.SendCancel(ctx, "sid", "01HCANCEL", "test"); err != nil {
		t.Fatalf("SendCancel: %v", err)
	}

	select {
	case got := <-sub.Channel():
		cancel := got.GetCancelApply()
		if cancel == nil {
			t.Fatalf("payload = %T, want CancelApply", got.GetPayload())
		}
		if cancel.GetApplyId() != "01HCANCEL" {
			t.Errorf("apply_id = %q, want 01HCANCEL", cancel.GetApplyId())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive forwarded CancelApply within 2s")
	}
}

// TestOutbound_Cluster_PublishWithZeroSubscribers — a holder exists in the
// lease, but nobody is subscribed to the outbound channel →
// ErrSoulNotConnected annotated with "no subscribers". The self-filter
// guarantees that Keeper-B, which we didn't raise as a subscriber, really
// doesn't get the message.
func TestOutbound_Cluster_PublishWithZeroSubscribers(t *testing.T) {
	r := newClusterRedis(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	leaseB, err := keeperredis.AcquireSoulLease(ctx, r, "sid", "kid-B", 30*time.Second)
	if err != nil {
		t.Fatalf("AcquireSoulLease(B): %v", err)
	}
	defer leaseB.Release(ctx)

	mA := NewStreamManager(discardLogger(t))
	ob, err := NewOutbound(OutboundDeps{
		Manager:     mA,
		AuditWriter: nopAudit{},
		Logger:      discardLogger(t),
		Redis:       r,
		KID:         "kid-A",
	})
	if err != nil {
		t.Fatalf("NewOutbound: %v", err)
	}

	err = ob.SendApply(ctx, "sid", &keeperv1.ApplyRequest{ApplyId: "x"})
	if !errors.Is(err, ErrSoulNotConnected) {
		t.Fatalf("err = %v, want ErrSoulNotConnected", err)
	}
}

// TestNewOutbound_RedisRequiresKID — setting Redis without a KID must be
// rejected by the constructor.
func TestNewOutbound_RedisRequiresKID(t *testing.T) {
	r := newClusterRedis(t)
	mA := NewStreamManager(discardLogger(t))
	_, err := NewOutbound(OutboundDeps{
		Manager:     mA,
		AuditWriter: nopAudit{},
		Logger:      discardLogger(t),
		Redis:       r,
		KID:         "",
	})
	if err == nil || !contains(err.Error(), "KID required when Redis is set") {
		t.Fatalf("err = %v, want KID-required validation", err)
	}
}
