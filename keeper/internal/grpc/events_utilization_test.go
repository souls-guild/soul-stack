package grpc

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"

	"github.com/alicebob/miniredis/v2"
)

// newUtilHandler — handler с standalone-miniredis-Redis-ом (TxPipeline требует
// одного слота — cluster-режим тут не подходит) и capture-audit-ом.
func newUtilHandler(t *testing.T) (*eventStreamHandler, *keeperredis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rc, err := keeperredis.NewClient(ctx, keeperredis.Config{Addr: mr.Addr()}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	h := newEventStreamHandler(EventStreamDeps{
		SeedDB:      &fakeSeedDB{},
		Redis:       rc,
		AuditWriter: &recordingAudit{},
		KID:         "kid-test",
	}, discardLogger(t))
	return h, rc
}

func utilizationEvent() *keeperv1.HostUtilization {
	return &keeperv1.HostUtilization{
		CollectedAt: timestamppb.New(time.Now().UTC()),
		CpuPct:      12.5,
		Load1:       0.7,
		MemUsedMb:   1024,
		MemTotalMb:  4096,
		UptimeSec:   3600,
	}
}

// TestHandleHostUtilization_WritesUnderAuthenticatedSID — снимок ложится в Redis
// под SID из параметра dispatch (аутентифицированный peer), не из payload:
// у HostUtilization поля sid нет вовсе, так что маршрут может идти только по
// переданному sid. Read под другим SID пуст.
func TestHandleHostUtilization_WritesUnderAuthenticatedSID(t *testing.T) {
	h, rc := newUtilHandler(t)
	ctx := context.Background()
	const sid = "auth.example.com"

	h.handleHostUtilization(ctx, sid, "session-1", utilizationEvent())

	snap, ok, err := keeperredis.ReadUtilization(ctx, rc, sid)
	if err != nil {
		t.Fatalf("ReadUtilization: %v", err)
	}
	if !ok {
		t.Fatal("ok=false — снимок не записан под аутентифицированным SID")
	}
	if snap.CPUPct != 12.5 || snap.MemUsedMB != 1024 {
		t.Errorf("snapshot mismatch: %+v", snap)
	}
	if _, okOther, _ := keeperredis.ReadUtilization(ctx, rc, "other.example.com"); okOther {
		t.Error("снимок виден под чужим SID — маршрут не по аутентифицированному sid")
	}

	pts, err := keeperredis.ReadUtilizationWindow(ctx, rc, sid, 10)
	if err != nil {
		t.Fatalf("ReadUtilizationWindow: %v", err)
	}
	if len(pts) != 1 {
		t.Errorf("window points = %d, want 1", len(pts))
	}
}

// TestHandleHostUtilization_NilPayloadNoWrite — nil ev → тихий выход, ничего не пишется.
func TestHandleHostUtilization_NilPayloadNoWrite(t *testing.T) {
	h, rc := newUtilHandler(t)
	ctx := context.Background()
	h.handleHostUtilization(ctx, "host.example.com", "session-1", nil)
	if _, ok, _ := keeperredis.ReadUtilization(ctx, rc, "host.example.com"); ok {
		t.Error("nil payload что-то записал")
	}
}

// TestHandleHostUtilization_NilRedisNoPanic — Redis=nil (dev/unit) → no-op без паники.
func TestHandleHostUtilization_NilRedisNoPanic(t *testing.T) {
	h := newTestHandler(t, &recordingAudit{}) // deps.Redis == nil
	h.handleHostUtilization(context.Background(), "host.example.com", "session-1", utilizationEvent())
}

// TestHandleHostUtilization_WriteFailure_GracefulNoPanic — сбой записи в Redis
// (закрытый клиент) НЕ паникует и НЕ всплывает наверх (handler void → warn), и НЕ
// трогает lease/presence: авторитет живости независим от vitals (ADR-071(e)).
func TestHandleHostUtilization_WriteFailure_GracefulNoPanic(t *testing.T) {
	h, rc := newUtilHandler(t)
	if err := rc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// закрытый клиент → WriteUtilization падает внутри; сбой глотается, без паники.
	h.handleHostUtilization(context.Background(), "host.example.com", "session-1", utilizationEvent())
}
