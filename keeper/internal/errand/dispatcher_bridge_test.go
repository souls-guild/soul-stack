package errand

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
)

// S1 guard tests (applybus-bottleneck): holder==self skips the per-applyID
// Redis Subscribe; holder!=self raises the bridge; holder-flip after the skip
// decision finishes by wait timer (timed_out), NOT by hanging.
//
// busBridge adapts the real *applybus.EventBus to the narrow errand.ApplyBus
// (mirrors the production errandApplyBusBridge wrapper from
// cmd/keeper/daemon.go). The dispatcher uses it to drive the REAL bus over
// miniredis, and Redis Subscribe calls are measured for real (PUBSUB NUMSUB).
type busBridge struct{ bus *applybus.EventBus }

func (b busBridge) Subscribe(ctx context.Context, applyID string) <-chan applybus.Event {
	return b.bus.Subscribe(ctx, applyID)
}

func (b busBridge) SubscribeWithBridge(ctx context.Context, applyID string, wantBridge bool) <-chan applybus.Event {
	return b.bus.SubscribeWithBridge(ctx, applyID, wantBridge)
}

func bridgeTestLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// newBridgeRedis creates miniredis + keeperredis.Client (without Docker, as in
// applybus/bus_cluster_test.go).
func newBridgeRedis(t *testing.T) (*keeperredis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := keeperredis.NewClient(ctx, keeperredis.Config{Addr: mr.Addr()}, nil)
	if err != nil {
		t.Fatalf("redis NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, mr
}

// redisSubCountFor returns the Redis-side subscriber count for apply:<id> from
// miniredis. It is 0 if the channel/subscription is missing. This directly
// measures whether the bus raised a per-applyID Redis Subscribe.
func redisSubCountFor(mr *miniredis.Miniredis, applyID string) int {
	ch := keeperredis.ApplyBusChannel(applyID)
	return mr.PubSubNumSub(ch)[ch]
}

const bridgeTestKID = "kid-self"

// buildBridgeDispatcher builds a Dispatcher on top of the real applybus bus and
// fakeLease. KID = bridgeTestKID matches the bus KID (self-publish is filtered
// as in production). lease controls which holder resolveHolder returns.
func buildBridgeDispatcher(t *testing.T, bus *applybus.EventBus, lease *fakeLease, ob *fakeOutbound, cap time.Duration) *Dispatcher {
	t.Helper()
	return &Dispatcher{deps: Deps{
		Store:       newFakeStore(),
		Outbound:    ob,
		Publisher:   ob,
		LeaseLookup: lease,
		ApplyBus:    busBridge{bus: bus},
		Logger:      bridgeTestLogger(),
		KID:         bridgeTestKID,
		ServerCap:   cap,
		Clock:       time.Now,
	}}
}

// TestErrand_SingleHolderSelf_NoRedisSubscribe checks that holder == self-KID
// makes the dispatcher request wantBridge=false, so the per-applyID Redis
// Subscribe is NOT raised (redisSubCount==0 for the whole subscription
// lifetime), and the Errand is delivered through the same instance's local bus.
// Direct guard for removing the applybus-bottleneck.
func TestErrand_SingleHolderSelf_NoRedisSubscribe(t *testing.T) {
	c, mr := newBridgeRedis(t)
	bus := applybus.NewBusWithRedis(bridgeTestLogger(), c, bridgeTestKID)

	const sid = "host.local"
	// holder == self -> skip decision.
	lease := &fakeLease{holders: map[string]string{sid: bridgeTestKID}}
	ob := &fakeOutbound{}

	d := buildBridgeDispatcher(t, bus, lease, ob, 2*time.Second)

	// Run Dispatch in a goroutine; while it waits for the event, measure Redis
	// Subscribe and deliver the result locally (through the same bus -> local
	// delivery without bridge).
	type out struct {
		res DispatchResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := d.Dispatch(context.Background(), DispatchRequest{
			SID: sid, Module: "core.cmd.shell", TimeoutSec: 2, StartedByAID: "archon-alice",
		})
		done <- out{res, err}
	}()

	// Wait until the local subscriber appears (subscription registered).
	var errandID string
	waitUntil(t, 2*time.Second, func() bool {
		errandID = ob.firstSentErrandID()
		return errandID != "" && bus.Subscribers(errandID) == 1
	}, "errand not subscribed locally")

	// Check window: the subscription is alive, Redis Subscribe must remain 0.
	for i := 0; i < 10; i++ {
		if n := redisSubCountFor(mr, errandID); n != 0 {
			t.Fatalf("redis subscribers = %d while holder==self, want 0 (bridge must be skipped)", n)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Deliver the result through the local bus (publish from the same instance).
	bus.Publish(applybus.Event{
		ApplyID: errandID,
		Kind:    applybus.KindErrandCompleted,
		Payload: ResultEvent{ErrandID: errandID, Status: StatusSuccess, ExitCode: ptrInt32(0), Stdout: "ok"},
	})

	o := recvDispatch(t, done, 3*time.Second)
	if o.err != nil {
		t.Fatalf("Dispatch: %v", o.err)
	}
	if o.res.Status != StatusSuccess {
		t.Fatalf("status = %q, want success (local-bus delivery)", o.res.Status)
	}
	if o.res.Async {
		t.Fatalf("Async=true, want sync success")
	}
}

// TestErrand_HolderOther_StillBridges checks that holder != self makes the
// dispatcher request wantBridge=true, so the per-applyID Redis bridge is raised
// (redisSubCount>=1), and cross-keeper delivery works: an event published by
// the "other" keeper (busOther, different KID) reaches the dispatcher through
// the bridge.
func TestErrand_HolderOther_StillBridges(t *testing.T) {
	c, mr := newBridgeRedis(t)
	busSelf := applybus.NewBusWithRedis(bridgeTestLogger(), c, bridgeTestKID)
	busOther := applybus.NewBusWithRedis(bridgeTestLogger(), c, "kid-other")

	const sid = "host.remote"
	lease := &fakeLease{holders: map[string]string{sid: "kid-other"}}
	ob := &fakeOutbound{}

	d := buildBridgeDispatcher(t, busSelf, lease, ob, 3*time.Second)

	type out struct {
		res DispatchResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := d.Dispatch(context.Background(), DispatchRequest{
			SID: sid, Module: "core.cmd.shell", TimeoutSec: 3, StartedByAID: "archon-alice",
		})
		done <- out{res, err}
	}()

	var errandID string
	waitUntil(t, 3*time.Second, func() bool {
		errandID = ob.firstSentErrandID()
		return errandID != "" && busSelf.Subscribers(errandID) == 1
	}, "errand not subscribed")

	// holder!=self -> the bridge must be raised (Redis Subscribe on the channel).
	waitUntil(t, 3*time.Second, func() bool {
		return redisSubCountFor(mr, errandID) >= 1
	}, "bridge not raised for holder!=self")

	// Cross-keeper delivery: busOther publishes, busSelf's bridge forwards.
	busOther.Publish(applybus.Event{
		ApplyID: errandID,
		Kind:    applybus.KindErrandCompleted,
		Payload: ResultEvent{ErrandID: errandID, Status: StatusSuccess, ExitCode: ptrInt32(0)},
	})

	o := recvDispatch(t, done, 4*time.Second)
	if o.err != nil {
		t.Fatalf("Dispatch: %v", o.err)
	}
	if o.res.Status != StatusSuccess {
		t.Fatalf("status = %q, want success (cross-keeper bridge delivery)", o.res.Status)
	}
}

// TestErrand_HolderFlipAfterSkip_TimesOut checks holder==self at the skip
// decision (bridge skipped), but the event actually arrives only on the
// "other" bus (holder changed after skip; the real executor ended up on another
// keeper). Dispatch does NOT receive the event through its local-only
// subscription and finishes timed_out by wait timer. Guarantee: event loss on
// holder-flip does not hang.
func TestErrand_HolderFlipAfterSkip_TimesOut(t *testing.T) {
	c, mr := newBridgeRedis(t)
	busSelf := applybus.NewBusWithRedis(bridgeTestLogger(), c, bridgeTestKID)
	busOther := applybus.NewBusWithRedis(bridgeTestLogger(), c, "kid-other")

	const sid = "host.flip"
	// Decision at Dispatch time: holder==self -> skip (wantBridge=false).
	lease := &fakeLease{holders: map[string]string{sid: bridgeTestKID}}
	ob := &fakeOutbound{}

	// Short timeout: TimeoutSec=1, ServerCap=2s -> 1s sync wait, no event ->
	// timed_out (sync, not async).
	d := buildBridgeDispatcher(t, busSelf, lease, ob, 2*time.Second)

	type out struct {
		res DispatchResult
		err error
	}
	done := make(chan out, 1)
	start := time.Now()
	go func() {
		res, err := d.Dispatch(context.Background(), DispatchRequest{
			SID: sid, Module: "core.cmd.shell", TimeoutSec: 1, StartedByAID: "archon-alice",
		})
		done <- out{res, err}
	}()

	var errandID string
	waitUntil(t, 2*time.Second, func() bool {
		errandID = ob.firstSentErrandID()
		return errandID != "" && busSelf.Subscribers(errandID) == 1
	}, "errand not subscribed locally")

	// bridge skipped; Redis Subscribe was not raised.
	if n := redisSubCountFor(mr, errandID); n != 0 {
		t.Fatalf("redis subscribers = %d, want 0 (skip-decision)", n)
	}

	// The event arrives ONLY on the "other" bus (holder-flip). Because busSelf
	// has no bridge, it will not be delivered to Dispatch's subscription.
	busOther.Publish(applybus.Event{
		ApplyID: errandID,
		Kind:    applybus.KindErrandCompleted,
		Payload: ResultEvent{ErrandID: errandID, Status: StatusSuccess, ExitCode: ptrInt32(0)},
	})

	o := recvDispatch(t, done, 4*time.Second)
	if o.err != nil {
		t.Fatalf("Dispatch: %v", o.err)
	}
	if o.res.Status != StatusTimedOut {
		t.Fatalf("status = %q, want timed_out (event lost on flip → timer fires)", o.res.Status)
	}
	if o.res.Async {
		t.Fatalf("Async=true, want sync timed_out")
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("elapsed = %v, want ≥1s (timer must fire, not hang or instant)", elapsed)
	}
}

// --- helpers (file-local) ---

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s: %s", timeout, msg)
}

func recvDispatch[T any](t *testing.T, ch <-chan T, timeout time.Duration) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(timeout):
		t.Fatal("Dispatch did not return within timeout (possible hang)")
		var zero T
		return zero
	}
}
