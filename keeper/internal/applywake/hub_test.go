package applywake

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
)

func fired(ch <-chan struct{}, within time.Duration) bool {
	select {
	case <-ch:
		return true
	default:
	}
	select {
	case <-ch:
		return true
	case <-time.After(within):
		return false
	}
}

func TestHub_WakeReachesEveryWaiterOfItsRunOnly(t *testing.T) {
	h := New(nil, "", nil)
	a1, stopA1 := h.Watch("run-a")
	defer stopA1()
	a2, stopA2 := h.Watch("run-a")
	defer stopA2()
	b, stopB := h.Watch("run-b")
	defer stopB()

	h.Wake("run-a")

	if !fired(a1, time.Second) || !fired(a2, time.Second) {
		t.Fatal("a waiter of the woken run was not woken")
	}
	if fired(b, 50*time.Millisecond) {
		t.Fatal("a waiter of another run was woken")
	}
}

// ★ GUARD. The barrier reads its rows, then blocks on the channel; a write that
// lands between the two wakes it before it blocks. The one slot keeps that wake.
//
// Mutation: an unbuffered waiter channel → the wake sent with nobody receiving is
// dropped and the barrier sleeps until its fallback poll.
func TestHub_AWakeBeforeTheWaitIsKept(t *testing.T) {
	h := New(nil, "", nil)
	ch, stop := h.Watch("run-a")
	defer stop()

	h.Wake("run-a")
	h.Wake("run-a")

	if !fired(ch, 0) {
		t.Fatal("a wake sent before the waiter blocked was lost")
	}
	if fired(ch, 50*time.Millisecond) {
		t.Fatal("two wakes left two pending: the barrier would re-read for nothing")
	}
}

func TestHub_UnwatchedWaiterIsForgotten(t *testing.T) {
	h := New(nil, "", nil)
	ch, stop := h.Watch("run-a")
	stop()
	stop()

	h.Wake("run-a")

	if fired(ch, 50*time.Millisecond) {
		t.Fatal("a removed waiter was woken")
	}
	if n := len(h.waiters); n != 0 {
		t.Fatalf("waiters left for %d runs after the last waiter went, want none", n)
	}
}

func TestHub_NilHubWakesNobody(t *testing.T) {
	var h *Hub
	ch, stop := h.Watch("run-a")
	defer stop()
	h.Wake("run-a")
	h.Listen(context.Background())
	if ch != nil {
		t.Fatal("a nil hub returned a channel that could fire")
	}
}

func newMiniredisClient(t *testing.T, mr *miniredis.Miniredis) *keeperredis.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := keeperredis.NewClient(ctx, keeperredis.Config{Addr: mr.Addr()}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func waitSubscribed(t *testing.T, mr *miniredis.Miniredis, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for mr.PubSubNumSub(keeperredis.ApplyWakeChannel)[keeperredis.ApplyWakeChannel] < want {
		if time.Now().After(deadline) {
			t.Fatalf("fewer than %d instances subscribed to %s", want, keeperredis.ApplyWakeChannel)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ★ GUARD (NIM-984). A host's terminal is written by the instance holding its
// stream, the barrier waits where the run started: the wake has to cross, and the
// origin wakes its own waiters without waiting for Redis. That the origin drops its
// echo is held at the subscription, where wakes do not coalesce
// (TestApplyWake_DeliversToOtherInstancesAndSkipsTheOrigin).
//
// Mutation: Wake without the publish → the other instance's barrier is not woken.
func TestHub_WakeCrossesToAnotherInstance(t *testing.T) {
	mr := miniredis.RunT(t)
	logger := slog.New(slog.DiscardHandler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	origin := New(newMiniredisClient(t, mr), "kid-origin", logger)
	peer := New(newMiniredisClient(t, mr), "kid-peer", logger)
	go origin.Listen(ctx)
	go peer.Listen(ctx)
	waitSubscribed(t, mr, 2)

	atPeer, stopPeer := peer.Watch("run-a")
	defer stopPeer()
	atOrigin, stopOrigin := origin.Watch("run-a")
	defer stopOrigin()

	origin.Wake("run-a")

	if !fired(atOrigin, 0) {
		t.Fatal("the barrier on the writing instance was not woken at once")
	}
	if !fired(atPeer, 3*time.Second) {
		t.Fatal("the barrier on another instance was not woken by the write here")
	}
}

// ★ GUARD. Listen outlives a Redis restart: a subscription lost for good would
// leave every barrier here on its fallback poll for the life of the process, with
// runs still ending right — slower, and silently.
//
// Mutation: return from Listen when the subscription dies → no wake after the
// restart.
func TestHub_ListenSubscribesAgainAfterRedisRestarts(t *testing.T) {
	mr := miniredis.RunT(t)
	logger := slog.New(slog.DiscardHandler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	origin := New(newMiniredisClient(t, mr), "kid-origin", logger)
	peer := New(newMiniredisClient(t, mr), "kid-peer", logger)
	go peer.Listen(ctx)
	waitSubscribed(t, mr, 1)

	mr.Close()
	if err := mr.Restart(); err != nil {
		t.Fatalf("restart miniredis: %v", err)
	}

	// Woken until one lands: right after the restart the old subscription may still
	// be counted for a moment before Listen drops it and subscribes again.
	ch, stop := peer.Watch("run-a")
	defer stop()
	deadline := time.Now().Add(5 * time.Second)
	for {
		origin.Wake("run-a")
		if fired(ch, 100*time.Millisecond) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no wake reached the instance after Redis came back")
		}
	}
}
