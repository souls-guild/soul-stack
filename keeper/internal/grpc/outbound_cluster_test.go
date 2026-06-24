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

// newClusterRedis — поднимает miniredis-инстанс и оборачивает в
// [keeperredis.Client]. Cleanup регистрируется через t.Cleanup.
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
// для SID, чей lease держит Keeper-B, должен:
//   - локальный lookup пуст;
//   - публиковать в `outbound:<sid>` (1 subscriber, поднятый отдельно);
//   - не вернуть ошибку.
func TestOutbound_Cluster_PublishesToRemoteHolder(t *testing.T) {
	r := newClusterRedis(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Keeper-B держит lease.
	leaseB, err := keeperredis.AcquireSoulLease(ctx, r, "host.example.com", "kid-B", 30*time.Second)
	if err != nil {
		t.Fatalf("AcquireSoulLease(B): %v", err)
	}
	defer leaseB.Release(ctx)

	// Keeper-B подписан на outbound-канал (симулируем его EventStream-handler).
	sub, err := keeperredis.SubscribeOutbound(ctx, r, "host.example.com", "kid-B", discardLogger(t))
	if err != nil {
		t.Fatalf("SubscribeOutbound: %v", err)
	}
	defer sub.Close()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	// Keeper-A (текущий) — Outbound без локального стрима.
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

// TestOutbound_Cluster_NoHolderReturnsNotConnected — если lease никто
// не держит → ErrSoulNotConnected.
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

// TestOutbound_Cluster_LocalLeaseWithoutStream — lease на текущем
// kid-е есть, но локального стрима нет (race: disconnect, lease ещё
// жив). → ErrSoulNotConnected, без publish-а.
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

// TestOutbound_Cluster_LocalStreamWins — если локальный стрим есть, он
// получает сообщение даже при наличии Redis (lease не проверяется).
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

// TestOutbound_Cluster_PublishCancelToRemoteHolder — SendCancel также
// маршрутизируется через pub/sub.
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

// TestOutbound_Cluster_PublishWithZeroSubscribers — holder в lease есть,
// но никто не подписан на outbound-канал → ErrSoulNotConnected с
// аннотацией "no subscribers". Self-фильтр гарантирует, что Keeper-B,
// который мы не подняли как subscriber, действительно не доходит.
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

// TestNewOutbound_RedisRequiresKID — установка Redis без KID должна
// быть отвергнута на конструкторе.
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
