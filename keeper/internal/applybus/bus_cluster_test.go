package applybus

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
)

// newClusterRedis spins up a miniredis instance and wraps it in
// [keeperredis.Client]. Same mechanism as grpc/outbound_cluster_test.go
// (miniredis instead of testcontainers — no Docker, deterministic, runs
// under -race). Cleanup via t.Cleanup.
func newClusterRedis(t *testing.T) (*keeperredis.Client, *miniredis.Miniredis) {
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

func clusterTestLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// lossCounter is an slog.Handler that counts the warnings the delivery path
// emits when it sheds an event, so a test can tell a shed event from a lost
// one. The default test logger writes to io.Discard: the code names the buffer
// it overflowed on every drop, and the tests were throwing that away.
//
// The two buffers are counted apart on purpose. Which one overflows is the
// whole question the fan-out test kept getting wrong: applybus.deliver's buffer
// belongs to ONE subscriber (one applyID), redis.applyEventSubBufferSize
// belongs to ONE SHARD and carries every applyID mapped to it.
type lossCounter struct {
	mu     sync.Mutex
	bridge int // redis shard subscription — ApplyEventSubscription.forward
	local  int // applybus.EventBus.deliver — per-subscriber channel
}

func (l *lossCounter) Enabled(context.Context, slog.Level) bool { return true }

func (l *lossCounter) Handle(_ context.Context, r slog.Record) error {
	// Both buffers say "dropped oldest event" when they evict, and "event lost"
	// when the freed slot is gone again — one event either way. Matching the
	// text is deliberate: if a message is reworded, this counts zero and the
	// accounting assert fails as "loss nobody logged", which is the direction a
	// test should fail in.
	if !strings.Contains(r.Message, "dropped oldest event") && !strings.Contains(r.Message, "event lost") {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if strings.HasPrefix(r.Message, "redis.SubscribeApplyEvent:") {
		l.bridge++
	} else {
		l.local++
	}
	return nil
}

func (l *lossCounter) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *lossCounter) WithGroup(string) slog.Handler      { return l }

// counts returns the events shed by the shard bridge and by per-subscriber
// buffers.
func (l *lossCounter) counts() (bridge, local int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.bridge, l.local
}

// missingSeq lists the seq numbers below n that a collector never saw. The
// shape of the answer names the mechanism: gaps in the MIDDLE are a buffer
// evicting its oldest entry, a missing TAIL is a reader that stopped before the
// stream ended. Printing the count alone hides that difference, which is how
// one was read as the other for as long as it was (NIM-318, NIM-363).
func missingSeq(seq []int, n int) []int {
	seen := make(map[int]bool, len(seq))
	for _, s := range seq {
		seen[s] = true
	}
	var out []int
	for i := 0; i < n; i++ {
		if !seen[i] {
			out = append(out, i)
		}
	}
	return out
}

// redisSubCount — number of Redis-side subscribers on the shard channel that
// applyID maps to (per miniredis PUBSUB NUMSUB). 0 if the channel doesn't
// exist. Post-sharding this reads as "is the bridge subscribed to this
// applyID's shard"; multiple applyIDs on one shard share the same count.
func redisSubCount(mr *miniredis.Miniredis, applyID string) int {
	ch := keeperredis.ApplyBusChannel(applyID)
	return mr.PubSubNumSub(ch)[ch]
}

// collidingApplyIDs returns two distinct applyIDs mapped to the SAME shard
// channel (equal ApplyBusShardIndex). Used to verify the forward-loop
// doesn't mix up payloads of colliding applyIDs. Iterates ULID-like strings
// until the first collision; at K=256 that's found within tens of iterations.
func collidingApplyIDs(t *testing.T) (string, string) {
	t.Helper()
	seen := make(map[uint32]string)
	for i := 0; i < 100000; i++ {
		id := "01J0COLLIDE" + string(rune('A'+i%26)) + string(rune('a'+(i/26)%26)) +
			string(rune('0'+(i/676)%10)) + string(rune('0'+(i/6760)%10)) + "00000000000A"
		// The applyID shape doesn't matter for shardIndex.
		shard := keeperredis.ApplyBusShardIndex(id)
		if prev, ok := seen[shard]; ok && prev != id {
			return prev, id
		}
		seen[shard] = id
	}
	t.Fatal("no shard collision found within budget — fnv distribution degenerate?")
	return "", ""
}

// collidingApplyIDsN returns n distinct applyIDs, ALL mapped to one and the
// same shard channel. Iterates ULID-like strings, accumulates them by the
// first shard encountered, and returns the first shard that collects n
// collisions.
func collidingApplyIDsN(t *testing.T, n int) []string {
	t.Helper()
	buckets := make(map[uint32][]string)
	for i := 0; i < 5_000_000; i++ {
		id := "01J0FANOUT" + string(rune('A'+i%26)) + string(rune('a'+(i/26)%26)) +
			string(rune('0'+(i/676)%10)) + string(rune('0'+(i/6760)%10)) +
			string(rune('A'+(i/67600)%26)) + "0000000A"
		shard := keeperredis.ApplyBusShardIndex(id)
		b := buckets[shard]
		// Exclude duplicates by generated string (different i could produce
		// the same id on counter wrap-around — doesn't happen here, but
		// keeps the "all distinct" invariant).
		dup := false
		for _, x := range b {
			if x == id {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		b = append(b, id)
		buckets[shard] = b
		if len(b) >= n {
			return b
		}
	}
	t.Fatalf("could not collect %d colliding applyIDs within budget", n)
	return nil
}

// waitFor — polls until true or fails the test on timeout.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
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

// recvWithin reads an event from the channel or fails the test on timeout.
func recvWithin(t *testing.T, ch <-chan Event, timeout time.Duration) Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("channel closed unexpectedly")
		}
		return ev
	case <-time.After(timeout):
		t.Fatal("no event within timeout")
		return Event{}
	}
}

// TestCluster_NewBusWithRedis_EnablesClusterMode — non-nil redis + kid
// enable cluster-mode.
func TestCluster_NewBusWithRedis_EnablesClusterMode(t *testing.T) {
	c, _ := newClusterRedis(t)
	b := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")
	if !b.clusterEnabled() {
		t.Fatal("clusterEnabled = false, want true (redis+kid set)")
	}
	if b.bridges == nil {
		t.Error("bridges map nil: cluster-mode did not initialize bridges")
	}
}

// TestCluster_NilLoggerFallsBackToDefault — nil logger falls back to
// slog.Default() (defensive branch in the constructor).
func TestCluster_NilLoggerFallsBackToDefault(t *testing.T) {
	b := NewBusWithRedis(nil, nil, "")
	if b.logger == nil {
		t.Fatal("logger nil after NewBusWithRedis(nil, ...): fallback did not work")
	}
	// Smoke: the bus is functional.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := b.Subscribe(ctx, "apply-nil-logger")
	b.Publish(Event{ApplyID: "apply-nil-logger", Kind: KindTaskExecuted})
	recvWithin(t, ch, time.Second)
}

// TestCluster_UnsubscribeUnknownIsNoop — unsubscribing a subscriber that's
// not in the map (or already removed) is a no-op (idx<0 branch). White-box:
// calls unsubscribe directly with a foreign subscriber. No panic, no double
// close.
func TestCluster_UnsubscribeUnknownIsNoop(t *testing.T) {
	c, _ := newClusterRedis(t)
	b := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")

	const applyID = "01J0UNKNOWNSUB00000000000A"

	// A live subscriber so applyID is present in the map.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = b.Subscribe(ctx, applyID)
	waitFor(t, 2*time.Second, func() bool { return b.Subscribers(applyID) == 1 },
		"subscriber not registered")

	// A foreign subscriber not in the map → idx<0 → early return.
	ghost := &subscriber{ch: make(chan Event, 1)}
	b.unsubscribe(applyID, ghost) // must not panic; ghost.ch stays open
	select {
	case <-ghost.ch:
		t.Error("ghost.ch unexpectedly readable: unsubscribe touched a foreign channel")
	default:
	}
	// The live subscriber is still there.
	if got := b.Subscribers(applyID); got != 1 {
		t.Errorf("Subscribers = %d after no-op unsubscribe, want 1", got)
	}

	// A completely unknown applyID is also a no-op.
	b.unsubscribe("never-subscribed", &subscriber{ch: make(chan Event, 1)})
}

// TestCluster_FirstSubscribeCreatesBridge — the first Subscribe(applyID) in
// cluster-mode raises a Redis bridge and actually subscribes to channel
// `apply:<id>` (verified via the miniredis-side subscriber count).
func TestCluster_FirstSubscribeCreatesBridge(t *testing.T) {
	c, mr := newClusterRedis(t)
	b := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const applyID = "01J0BRIDGE000000000000000A"
	_ = b.Subscribe(ctx, applyID)

	// Subscribe in cluster-mode waits for Ready synchronously — by the time
	// it returns, the Redis subscription is registered.
	waitFor(t, 2*time.Second, func() bool {
		return redisSubCount(mr, applyID) >= 1
	}, "miniredis did not register subscriber on apply-channel")
}

// TestCluster_SecondSubscribeRefcountsNoSecondRedisSub — a second
// Subscribe of the same applyID does not create a second Redis subscribe
// (ref-count++), visible as an unchanged channel subscriber count in
// miniredis.
func TestCluster_SecondSubscribeRefcountsNoSecondRedisSub(t *testing.T) {
	c, mr := newClusterRedis(t)
	b := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const applyID = "01J0REFCOUNT00000000000000"
	_ = b.Subscribe(ctx, applyID)
	waitFor(t, 2*time.Second, func() bool {
		return redisSubCount(mr, applyID) == 1
	}, "first subscribe did not register exactly 1 redis-subscriber")

	// A second local subscribe of the same applyID.
	_ = b.Subscribe(ctx, applyID)
	if got := b.Subscribers(applyID); got != 2 {
		t.Fatalf("local Subscribers = %d, want 2", got)
	}

	// Redis-side: still exactly one subscriber on the channel (refs++, not a
	// second Subscribe). Give the event loop a bit of time in case a second
	// sub happened erroneously.
	time.Sleep(50 * time.Millisecond)
	if n := redisSubCount(mr, applyID); n != 1 {
		t.Errorf("redis subscribers = %d, want 1 (ref-count, not second Subscribe)", n)
	}
}

// TestCluster_DeliverFromCluster_DeliversToLocalSubscribers — an event
// published by another Keeper (a different origin_kid) reaches a local
// subscriber through the bridge.
func TestCluster_DeliverFromCluster_DeliversToLocalSubscribers(t *testing.T) {
	c, _ := newClusterRedis(t)
	// Two buses on the same Redis, different KIDs — busB publishes, busA receives.
	busA := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")
	busB := NewBusWithRedis(clusterTestLogger(), c, "keeper-B")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const applyID = "01J0DELIVER0000000000000XY"
	ch := busA.Subscribe(ctx, applyID)
	waitFor(t, 2*time.Second, func() bool { return busA.Subscribers(applyID) == 1 },
		"busA subscriber not registered")

	// busB publishes — busB has no local subscribers, the event goes to
	// Redis, busA's bridge forwards it to ch.
	busB.Publish(Event{
		ApplyID: applyID,
		Kind:    KindTaskExecuted,
		Payload: map[string]any{"sid": "host.example", "task_idx": 0},
	})

	ev := recvWithin(t, ch, 3*time.Second)
	if ev.Kind != KindTaskExecuted {
		t.Errorf("Kind = %q, want task.executed", ev.Kind)
	}
	if ev.ApplyID != applyID {
		t.Errorf("ApplyID = %q, want %q", ev.ApplyID, applyID)
	}
	// Payload arrives as json.RawMessage (the bridge doesn't know the typed
	// structure).
	raw, ok := ev.Payload.(json.RawMessage)
	if !ok {
		t.Fatalf("Payload type = %T, want json.RawMessage", ev.Payload)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if decoded["sid"] != "host.example" {
		t.Errorf("payload sid = %v, want host.example", decoded["sid"])
	}
}

// TestCluster_SelfOriginNotDoubleDelivered — a self-publish is delivered to
// a local subscriber exactly once: local delivery + Redis-echo with the same
// origin_kid is filtered out in SubscribeApplyEvent. There must be no
// duplicate.
func TestCluster_SelfOriginNotDoubleDelivered(t *testing.T) {
	c, _ := newClusterRedis(t)
	b := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const applyID = "01J0SELF000000000000000XYZ"
	ch := b.Subscribe(ctx, applyID)
	waitFor(t, 2*time.Second, func() bool { return b.Subscribers(applyID) == 1 },
		"subscriber not registered")

	b.Publish(Event{ApplyID: applyID, Kind: KindApplyCompleted, Payload: map[string]any{"x": 1}})

	// First (local) delivery.
	ev := recvWithin(t, ch, 2*time.Second)
	if ev.Kind != KindApplyCompleted {
		t.Errorf("Kind = %q, want apply.completed", ev.Kind)
	}

	// The Redis echo with the same origin_kid is filtered — no second event.
	select {
	case dup := <-ch:
		t.Errorf("unexpected duplicate from self-echo: %+v", dup)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestCluster_DeliverFromCluster_NoLocalSubscribers — a Redis event for an
// applyID with no local subscribers doesn't panic or block (early return in
// deliverFromCluster when len(subs)==0). Achieved via ref-counting: two
// local subscribes keep the bridge alive, both unsubscribe, but the bridge
// keeps forwarding until its own cancel — the event arrives "into the void".
func TestCluster_DeliverFromCluster_NoLocalSubscribers(t *testing.T) {
	c, _ := newClusterRedis(t)
	busA := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")
	busB := NewBusWithRedis(clusterTestLogger(), c, "keeper-B")

	const applyID = "01J0EMPTY000000000000000AB"

	// busA: raise the bridge via subscribe, then unsubscribe — but the
	// bridge closes asynchronously; to deterministically cover "no
	// subscribers", we use holderCtx (stays alive) plus a separate
	// subscriber that we cancel, then publish.
	holderCtx, holderCancel := context.WithCancel(context.Background())
	defer holderCancel()
	_ = busA.Subscribe(holderCtx, applyID) // holds the bridge

	subCtx, subCancel := context.WithCancel(context.Background())
	chTmp := busA.Subscribe(subCtx, applyID)
	waitFor(t, 2*time.Second, func() bool { return busA.Subscribers(applyID) == 2 },
		"two subscribers not registered")

	// Cancel the second subscriber and drain its channel.
	subCancel()
	waitFor(t, 2*time.Second, func() bool { return busA.Subscribers(applyID) == 1 },
		"second subscriber not unsubscribed")
	go func() {
		for range chTmp {
		}
	}()

	// busB publishes — busA's bridge is alive (holder), delivery goes to the
	// remaining subscriber. This covers the ordinary deliverFromCluster path
	// with a non-empty snapshot.
	busB.Publish(Event{ApplyID: applyID, Kind: KindTaskExecuted})

	// Now drop the holder and publish immediately — a race between
	// bridge-teardown and the event; deliverFromCluster must safely handle an
	// empty snapshot.
	holderCancel()
	busB.Publish(Event{ApplyID: applyID, Kind: KindApplyFailed})
	busB.Publish(Event{ApplyID: applyID, Kind: KindApplyCancelled})

	// Must not panic; give it time.
	time.Sleep(200 * time.Millisecond)
}

// TestCluster_LastUnsubscribeClosesBridge — unsubscribing the last
// subscriber closes the bridge: the Redis channel loses its subscriber
// (cancel + sub.Close), and a subsequent cross-Keeper publish no longer
// reaches a new local subscriber until the bridge is recreated. Verified via
// the miniredis subscriber count.
func TestCluster_LastUnsubscribeClosesBridge(t *testing.T) {
	c, mr := newClusterRedis(t)
	b := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")

	const applyID = "01J0LASTSUB0000000000000AB"
	ctx, cancel := context.WithCancel(context.Background())
	_ = b.Subscribe(ctx, applyID)
	waitFor(t, 2*time.Second, func() bool {
		return redisSubCount(mr, applyID) == 1
	}, "bridge subscriber not registered")

	// Unsubscribe the last one → bridge.refs=0 → cancel + sub.Close → Redis
	// loses its channel subscriber.
	cancel()
	waitFor(t, 3*time.Second, func() bool {
		return redisSubCount(mr, applyID) == 0
	}, "bridge did not close: Redis channel still has a subscriber")

	if got := b.Subscribers(applyID); got != 0 {
		t.Errorf("local Subscribers = %d, want 0", got)
	}
}

// TestCluster_BridgeRecreatedAfterClose — after the bridge closes, a new
// Subscribe of the same applyID raises the bridge again (refs starts at 1
// again) and cross-Keeper delivery works again.
func TestCluster_BridgeRecreatedAfterClose(t *testing.T) {
	c, mr := newClusterRedis(t)
	busA := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")
	busB := NewBusWithRedis(clusterTestLogger(), c, "keeper-B")

	const applyID = "01J0RECREATE000000000000AB"

	ctx1, cancel1 := context.WithCancel(context.Background())
	_ = busA.Subscribe(ctx1, applyID)
	waitFor(t, 2*time.Second, func() bool {
		return redisSubCount(mr, applyID) == 1
	}, "first bridge not up")
	cancel1()
	waitFor(t, 3*time.Second, func() bool {
		return redisSubCount(mr, applyID) == 0
	}, "first bridge not closed")

	// A new subscribe — the bridge is recreated.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	ch := busA.Subscribe(ctx2, applyID)
	waitFor(t, 2*time.Second, func() bool {
		return redisSubCount(mr, applyID) == 1
	}, "recreated bridge not up")

	busB.Publish(Event{ApplyID: applyID, Kind: KindTaskExecuted})
	ev := recvWithin(t, ch, 3*time.Second)
	if ev.Kind != KindTaskExecuted {
		t.Errorf("Kind = %q, want task.executed after bridge recreation", ev.Kind)
	}
}

// TestCluster_BridgeReadyFails_LocalDeliveryStillWorks — if the Redis client
// is closed before Subscribe, sub.Ready fails (Warn), but local delivery
// keeps working and Subscribe neither hangs nor panics. Covers the
// Ready-failure branch in ensureClusterBridgeLocked's forward goroutine.
func TestCluster_BridgeReadyFails_LocalDeliveryStillWorks(t *testing.T) {
	c, _ := newClusterRedis(t)
	b := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")

	// Close the Redis client: the subsequent subscribe-loop can't reach
	// Ready (the initial Receive fails), but Subscribe still returns (via
	// the Ready signal from the forward goroutine, which closes even on
	// error).
	_ = c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const applyID = "01J0READYFAIL00000000000AB"
	done := make(chan (<-chan Event), 1)
	go func() { done <- b.Subscribe(ctx, applyID) }()

	var ch <-chan Event
	select {
	case ch = <-done:
	case <-time.After(7 * time.Second):
		t.Fatal("Subscribe hung past Ready-timeout when redis closed")
	}

	// Local delivery works despite the dead bridge.
	b.Publish(Event{ApplyID: applyID, Kind: KindApplyStarted})
	ev := recvWithin(t, ch, 2*time.Second)
	if ev.Kind != KindApplyStarted {
		t.Errorf("Kind = %q, want apply.started (local delivery)", ev.Kind)
	}
}

// TestCluster_PublishToCluster_Success — Publish in cluster-mode actually
// delivers to Redis: a second bus on the same Redis receives the event
// (verifies the successful publishToCluster path end-to-end).
func TestCluster_PublishToCluster_Success(t *testing.T) {
	c, _ := newClusterRedis(t)
	busPub := NewBusWithRedis(clusterTestLogger(), c, "keeper-pub")
	busSub := NewBusWithRedis(clusterTestLogger(), c, "keeper-sub")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const applyID = "01J0PUBOK00000000000000XYZ"
	ch := busSub.Subscribe(ctx, applyID)
	waitFor(t, 2*time.Second, func() bool { return busSub.Subscribers(applyID) == 1 },
		"subscriber not registered")

	busPub.Publish(Event{ApplyID: applyID, Kind: KindApplyCompleted, Payload: map[string]any{"ok": true}})

	ev := recvWithin(t, ch, 3*time.Second)
	if ev.Kind != KindApplyCompleted {
		t.Errorf("Kind = %q, want apply.completed", ev.Kind)
	}
}

// TestCluster_PublishToCluster_RedisError_Swallowed — with Redis unavailable
// (client closed), Publish doesn't panic, doesn't block, and still delivers
// to the local subscriber. The PUBLISH error is swallowed (warn).
func TestCluster_PublishToCluster_RedisError_Swallowed(t *testing.T) {
	c, _ := newClusterRedis(t)
	b := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")

	// local subscriber before closing Redis.
	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	ch := b.Subscribe(subCtx, "01J0PUBERR000000000000XYZ")
	waitFor(t, 2*time.Second, func() bool { return b.Subscribers("01J0PUBERR000000000000XYZ") == 1 },
		"subscriber not registered")

	// Break Redis — the subsequent publishToCluster will get a PUBLISH error.
	_ = c.Close()

	done := make(chan struct{})
	go func() {
		b.Publish(Event{ApplyID: "01J0PUBERR000000000000XYZ", Kind: KindTaskExecuted})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Publish blocked on closed Redis longer than clusterPublishTimeout")
	}

	// Local delivery happened despite the Redis-publish error.
	ev := recvWithin(t, ch, 2*time.Second)
	if ev.Kind != KindTaskExecuted {
		t.Errorf("Kind = %q, want task.executed (local still delivered)", ev.Kind)
	}
}

// TestCluster_PublishToCluster_PayloadMarshalError_Swallowed — a payload
// that doesn't serialize to JSON (chan) doesn't crash Publish: the marshal
// error is logged and swallowed, local delivery still happens.
func TestCluster_PublishToCluster_PayloadMarshalError_Swallowed(t *testing.T) {
	c, _ := newClusterRedis(t)
	b := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")

	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	const applyID = "01J0MARSHALERR0000000000AB"
	ch := b.Subscribe(subCtx, applyID)
	waitFor(t, 2*time.Second, func() bool { return b.Subscribers(applyID) == 1 },
		"subscriber not registered")

	// chan doesn't serialize via json.Marshal → the marshal-error branch.
	b.Publish(Event{ApplyID: applyID, Kind: KindTaskExecuted, Payload: make(chan int)})

	// Local delivery happened (the snapshot was taken before publishToCluster).
	ev := recvWithin(t, ch, 2*time.Second)
	if ev.Kind != KindTaskExecuted {
		t.Errorf("Kind = %q, want task.executed", ev.Kind)
	}
}

// TestCluster_PublishNilPayload_NoMarshal — a nil payload doesn't trigger
// marshal (the ev.Payload == nil branch in publishToCluster) and publishes
// correctly.
func TestCluster_PublishNilPayload_NoMarshal(t *testing.T) {
	c, _ := newClusterRedis(t)
	busPub := NewBusWithRedis(clusterTestLogger(), c, "keeper-pub")
	busSub := NewBusWithRedis(clusterTestLogger(), c, "keeper-sub")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const applyID = "01J0NILPAYLOAD000000000AB"
	ch := busSub.Subscribe(ctx, applyID)
	waitFor(t, 2*time.Second, func() bool { return busSub.Subscribers(applyID) == 1 },
		"subscriber not registered")

	busPub.Publish(Event{ApplyID: applyID, Kind: KindApplyStarted})

	ev := recvWithin(t, ch, 3*time.Second)
	if ev.Kind != KindApplyStarted {
		t.Errorf("Kind = %q, want apply.started", ev.Kind)
	}
}

// TestCluster_NoForwardLoopGoroutineLeak — after a full bridge teardown
// (last-unsubscribe), the forward-loop goroutine exits, no leak. Verified
// via stabilization of runtime.NumGoroutine.
func TestCluster_NoForwardLoopGoroutineLeak(t *testing.T) {
	c, mr := newClusterRedis(t)
	b := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")

	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()

	const rounds = 20
	for i := 0; i < rounds; i++ {
		applyID := "01J0LEAK00000000000000" + string(rune('A'+i%26)) + string(rune('a'+i%26))
		ctx, cancel := context.WithCancel(context.Background())
		_ = b.Subscribe(ctx, applyID)
		waitFor(t, 2*time.Second, func() bool {
			return redisSubCount(mr, applyID) == 1
		}, "bridge not up in leak-round")
		cancel()
		waitFor(t, 3*time.Second, func() bool {
			return redisSubCount(mr, applyID) == 0
		}, "bridge not closed in leak-round")
	}

	// Let the forward goroutines finish.
	waitFor(t, 5*time.Second, func() bool {
		runtime.GC()
		return runtime.NumGoroutine() <= before+3
	}, "goroutine count did not settle — forward-loop leak suspected")
}

// TestCluster_ConcurrentSubscribeUnsubscribe — concurrent subscribe/
// unsubscribe of the same applyID don't break the bridge's ref-counting and
// don't panic under -race. The Publish-vs-unsubscribe race in local delivery
// is fixed by variant A (see TestConcurrentPublishVsUnsubscribe), so a
// concurrent Publish is added here too — it also checks that delivery under
// churn doesn't panic.
func TestCluster_ConcurrentSubscribeUnsubscribe(t *testing.T) {
	c, mr := newClusterRedis(t)
	b := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")

	const (
		applyID = "01J0CONC0000000000000000AB"
		workers = 16
		rounds  = 25
	)

	pubCtx, pubCancel := context.WithCancel(context.Background())
	var wgP sync.WaitGroup
	wgP.Add(1)
	go func() {
		defer wgP.Done()
		for {
			select {
			case <-pubCtx.Done():
				return
			default:
				b.Publish(Event{ApplyID: applyID, Kind: KindTaskExecuted})
			}
		}
	}()

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				ctx, cancel := context.WithCancel(context.Background())
				ch := b.Subscribe(ctx, applyID)
				// ch is never closed (variant A) — the reader exits on ctx.Done().
				go func() {
					for {
						select {
						case <-ch:
						case <-ctx.Done():
							return
						}
					}
				}()
				cancel()
			}
		}()
	}
	wg.Wait()
	pubCancel()
	wgP.Wait()

	// After full teardown — zero local subscribers and the bridge collapsed
	// (refs=0 → Redis channel with no subscribers). This is the
	// ref-counting invariant: no stuck bridges when sub/unsub counts match.
	waitFor(t, 5*time.Second, func() bool { return b.Subscribers(applyID) == 0 },
		"subscribers not drained to 0 after concurrent churn")
	waitFor(t, 5*time.Second, func() bool { return redisSubCount(mr, applyID) == 0 },
		"bridge did not collapse after concurrent churn: bridge/refs leak")
}

// TestSubscribe_BackCompat_DefaultBridges — Subscribe(ctx,id) is
// behaviorally equivalent to SubscribeWithBridge(ctx,id,true): in
// cluster-mode both raise a per-applyID Redis bridge (redisSubCount==1), and
// both get local delivery. Guard on back-compat: existing Subscribe callers
// keep their behavior after SubscribeWithBridge was introduced (S1).
func TestSubscribe_BackCompat_DefaultBridges(t *testing.T) {
	c, mr := newClusterRedis(t)
	b := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")

	// legacy Subscribe → bridge raised.
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	const idA = "01J0BACKCOMPATAAAAAAAAAAAAA"
	chA := b.Subscribe(ctxA, idA)
	waitFor(t, 2*time.Second, func() bool { return redisSubCount(mr, idA) == 1 },
		"Subscribe did not raise bridge")

	// SubscribeWithBridge(...,true) → the same effect on a different applyID.
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	const idB = "01J0BACKCOMPATBBBBBBBBBBBBB"
	chB := b.SubscribeWithBridge(ctxB, idB, true)
	waitFor(t, 2*time.Second, func() bool { return redisSubCount(mr, idB) == 1 },
		"SubscribeWithBridge(true) did not raise bridge")

	// Local delivery works for both.
	b.Publish(Event{ApplyID: idA, Kind: KindTaskExecuted})
	if ev := recvWithin(t, chA, 2*time.Second); ev.Kind != KindTaskExecuted {
		t.Errorf("Subscribe local delivery Kind = %q, want task.executed", ev.Kind)
	}
	b.Publish(Event{ApplyID: idB, Kind: KindTaskExecuted})
	if ev := recvWithin(t, chB, 2*time.Second); ev.Kind != KindTaskExecuted {
		t.Errorf("SubscribeWithBridge(true) local delivery Kind = %q, want task.executed", ev.Kind)
	}
}

// TestSubscribeWithBridge_False_NoRedisSubscribe — wantBridge=false in
// cluster-mode doesn't raise a per-applyID Redis-Subscribe (redisSubCount==0),
// but local delivery via Publish on the same instance still works. A direct
// unit guard on bridge-skip (S1), independent of the dispatcher.
func TestSubscribeWithBridge_False_NoRedisSubscribe(t *testing.T) {
	c, mr := newClusterRedis(t)
	b := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const id = "01J0NOBRIDGE000000000000AB"
	ch := b.SubscribeWithBridge(ctx, id, false)
	waitFor(t, 2*time.Second, func() bool { return b.Subscribers(id) == 1 },
		"local subscriber not registered")

	// No Redis-Subscribe raised — bridge skipped.
	for i := 0; i < 10; i++ {
		if n := redisSubCount(mr, id); n != 0 {
			t.Fatalf("redis subscribers = %d, want 0 (wantBridge=false)", n)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// local delivery works.
	b.Publish(Event{ApplyID: id, Kind: KindApplyCompleted})
	if ev := recvWithin(t, ch, 2*time.Second); ev.Kind != KindApplyCompleted {
		t.Errorf("Kind = %q, want apply.completed (local delivery)", ev.Kind)
	}
}

// TestSubscribeWithBridge_MixedRefcount_NoPrematureClose — a local-only
// subscriber (wantBridge=false) does NOT decrement the refs of a bridge
// raised by a sibling subscriber on the same applyID. Guard on the
// heldBridge invariant: unsubscribing a local-only sub must not tear down
// someone else's bridge.
func TestSubscribeWithBridge_MixedRefcount_NoPrematureClose(t *testing.T) {
	c, mr := newClusterRedis(t)
	b := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")

	const id = "01J0MIXREFCOUNT00000000AB"

	// subBridge holds the bridge (wantBridge=true).
	ctxBridge, cancelBridge := context.WithCancel(context.Background())
	defer cancelBridge()
	_ = b.SubscribeWithBridge(ctxBridge, id, true)
	waitFor(t, 2*time.Second, func() bool { return redisSubCount(mr, id) == 1 },
		"bridge not raised by wantBridge=true subscriber")

	// subLocal — local-only, doesn't touch refs.
	ctxLocal, cancelLocal := context.WithCancel(context.Background())
	_ = b.SubscribeWithBridge(ctxLocal, id, false)
	waitFor(t, 2*time.Second, func() bool { return b.Subscribers(id) == 2 },
		"two subscribers not registered")

	// Unsubscribe local-only — the bridge MUST remain (refs==1 from subBridge).
	cancelLocal()
	waitFor(t, 2*time.Second, func() bool { return b.Subscribers(id) == 1 },
		"local-only subscriber not unsubscribed")
	time.Sleep(50 * time.Millisecond)
	if n := redisSubCount(mr, id); n != 1 {
		t.Fatalf("redis subscribers = %d after local-only unsubscribe, want 1 (bridge must survive)", n)
	}

	// Now unsubscribe the bridge holder — the bridge collapses.
	cancelBridge()
	waitFor(t, 3*time.Second, func() bool { return redisSubCount(mr, id) == 0 },
		"bridge did not close after last bridge-holder unsubscribed")
}

// TestCluster_ConcurrentMixedBridgeChurn_HeldBridgeInvariant — concurrent
// subscribe/unsubscribe of the same applyID with a MIX of wantBridge (some
// local-only false, some bridge true) under churn. Guard on the heldBridge
// invariant (S1, holder==self skip):
//
//   - a local-only subscriber (wantBridge=false) does NOT decrement the
//     bridge's refs → doesn't tear down someone else's bridge (no
//     preliminary close);
//   - symmetric refs++/refs-- only for heldBridge subscribers → no refs
//     leak (with equal bridge-sub/unsub counts, the bridge collapses to 0).
//
// An "anchor" bridge-holder is kept alive for the whole churn duration: we
// verify that under a flurry of mixed sub/unsub it does NOT close
// prematurely (redisSubCount stays ≥1). After dropping the anchor and full
// teardown — refs=0, bridge closed. Runs under -race.
func TestCluster_ConcurrentMixedBridgeChurn_HeldBridgeInvariant(t *testing.T) {
	c, mr := newClusterRedis(t)
	b := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")

	const (
		applyID = "01J0MIXCHURN00000000000AB"
		workers = 16
		rounds  = 25
	)

	// Anchor: a bridge-holder alive for the whole churn duration. Guarantees
	// redisSubCount must not drop to 0 during the churn window — a premature
	// close of someone else's bridge would surface immediately.
	anchorCtx, anchorCancel := context.WithCancel(context.Background())
	_ = b.SubscribeWithBridge(anchorCtx, applyID, true)
	waitFor(t, 2*time.Second, func() bool { return redisSubCount(mr, applyID) >= 1 },
		"anchor bridge not raised")

	// Concurrent publisher — also checks that delivery under churn doesn't
	// panic (Publish-vs-unsubscribe).
	pubCtx, pubCancel := context.WithCancel(context.Background())
	var wgP sync.WaitGroup
	wgP.Add(1)
	go func() {
		defer wgP.Done()
		for {
			select {
			case <-pubCtx.Done():
				return
			default:
				b.Publish(Event{ApplyID: applyID, Kind: KindTaskExecuted})
			}
		}
	}()

	// Premature-close detector: while the anchor is alive, redisSubCount must
	// stay ≥1. Any drop to 0 means the heldBridge invariant is broken
	// (a local-only sub tore down someone else's bridge).
	monCtx, monCancel := context.WithCancel(context.Background())
	prematureClose := make(chan struct{}, 1)
	var monWG sync.WaitGroup
	monWG.Add(1)
	go func() {
		defer monWG.Done()
		for {
			select {
			case <-monCtx.Done():
				return
			default:
				if redisSubCount(mr, applyID) == 0 {
					select {
					case prematureClose <- struct{}{}:
					default:
					}
					return
				}
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				ctx, cancel := context.WithCancel(context.Background())
				// Mix: even rounds — local-only (false), odd rounds — bridge (true).
				wantBridge := (seed+i)%2 == 0
				ch := b.SubscribeWithBridge(ctx, applyID, wantBridge)
				go func() {
					for {
						select {
						case <-ch:
						case <-ctx.Done():
							return
						}
					}
				}()
				cancel()
			}
		}(w)
	}
	wg.Wait()

	// Stop the monitor before dropping the anchor.
	monCancel()
	monWG.Wait()
	pubCancel()
	wgP.Wait()

	select {
	case <-prematureClose:
		t.Fatal("bridge closed prematurely during churn with a live anchor: heldBridge invariant broken (local-only sub dropped another bridge)")
	default:
	}

	// The anchor still holds the bridge. wg.Wait above only proves every worker
	// STOPPED subscribing — a churned subscriber is torn down by its own context,
	// asynchronously, so the counts converge shortly after rather than at this
	// instant. Asserting the value directly here reads a straggler as a refs leak
	// on a loaded box; the settled value is the invariant, same as the teardown
	// assertions below.
	waitFor(t, 5*time.Second, func() bool { return redisSubCount(mr, applyID) == 1 },
		"redis subscribers did not settle at 1 after churn (anchor alive): refs leak or premature close")
	waitFor(t, 5*time.Second, func() bool { return b.Subscribers(applyID) == 1 },
		"local subscribers did not settle at 1 after churn (anchor only)")

	// Drop the anchor — the bridge must collapse (refs=0). Proves there's no
	// refs leak: a stray increment would leave refs>0 forever.
	anchorCancel()
	waitFor(t, 5*time.Second, func() bool { return redisSubCount(mr, applyID) == 0 },
		"bridge did not collapse after anchor removal: refs leak from mixed churn")
	waitFor(t, 5*time.Second, func() bool { return b.Subscribers(applyID) == 0 },
		"local subscribers did not return to zero after full teardown")
}

// TestCluster_ShardChannel_CrossKeeperErrand — cross-Keeper delivery of an
// Errand result over the SHARDED channel. A dispatches for a SID, the Soul
// is on B; B publishes errand.completed (busB, a different KID), A is
// subscribed to the same shard → the event arrives. Ports the cross-Keeper
// case to the shard form (Errand family of EventKinds).
func TestCluster_ShardChannel_CrossKeeperErrand(t *testing.T) {
	c, mr := newClusterRedis(t)
	busA := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")
	busB := NewBusWithRedis(clusterTestLogger(), c, "keeper-B")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const errandID = "01J0ERRANDSHARD00000000XYZ"
	ch := busA.Subscribe(ctx, errandID)
	// The bridge is raised specifically on this applyID's shard.
	waitFor(t, 2*time.Second, func() bool { return redisSubCount(mr, errandID) >= 1 },
		"busA bridge not registered on shard")

	busB.Publish(Event{
		ApplyID: errandID,
		Kind:    KindErrandCompleted,
		Payload: map[string]any{"errand_id": errandID, "status": "success", "exit_code": 0},
	})

	ev := recvWithin(t, ch, 3*time.Second)
	if ev.Kind != KindErrandCompleted {
		t.Errorf("Kind = %q, want errand.completed", ev.Kind)
	}
	if ev.ApplyID != errandID {
		t.Errorf("ApplyID = %q, want %q", ev.ApplyID, errandID)
	}
	raw, ok := ev.Payload.(json.RawMessage)
	if !ok {
		t.Fatalf("Payload type = %T, want json.RawMessage", ev.Payload)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if decoded["status"] != "success" {
		t.Errorf("payload status = %v, want success", decoded["status"])
	}
}

// TestCluster_ShardChannel_CrossKeeperSSE — a cross-Keeper scenario-run
// (the apply.* family, as an SSE stream) arrives over the shard: A holds an
// SSE-style subscription, B (a different KID) publishes task.executed +
// apply.completed on the same shard, both arrive in the correct order. Ports
// the SSE cross-Keeper case to the shard form (in-memory bus level, no HTTP
// — the HTTP variant is in integration sse_cluster_test.go).
func TestCluster_ShardChannel_CrossKeeperSSE(t *testing.T) {
	c, mr := newClusterRedis(t)
	busA := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")
	busB := NewBusWithRedis(clusterTestLogger(), c, "keeper-B")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const applyID = "01J0SSESHARD0000000000XYZ1"
	ch := busA.Subscribe(ctx, applyID)
	waitFor(t, 2*time.Second, func() bool { return redisSubCount(mr, applyID) >= 1 },
		"SSE-style bridge not registered on shard")

	busB.Publish(Event{ApplyID: applyID, Kind: KindTaskExecuted, Payload: map[string]any{"task_idx": 0}})
	busB.Publish(Event{ApplyID: applyID, Kind: KindApplyCompleted, Payload: map[string]any{"run_status": "RUN_STATUS_SUCCESS"}})

	ev1 := recvWithin(t, ch, 3*time.Second)
	if ev1.Kind != KindTaskExecuted {
		t.Errorf("first frame Kind = %q, want task.executed", ev1.Kind)
	}
	ev2 := recvWithin(t, ch, 3*time.Second)
	if ev2.Kind != KindApplyCompleted {
		t.Errorf("second frame Kind = %q, want apply.completed", ev2.Kind)
	}
}

// TestCluster_ShardCollision_NoPayloadMix — ★critical guard: TWO applyIDs
// colliding on ONE shard are published simultaneously. Each local subscriber
// receives ONLY events for its own applyID (the forward-loop filters by
// envelope.ApplyID, not by shard). Without this filter, one run's payload
// would leak into another run's SSE stream — an isolation breach.
func TestCluster_ShardCollision_NoPayloadMix(t *testing.T) {
	c, mr := newClusterRedis(t)
	busA := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")
	busB := NewBusWithRedis(clusterTestLogger(), c, "keeper-B")

	idX, idY := collidingApplyIDs(t)
	if keeperredis.ApplyBusShardIndex(idX) != keeperredis.ApplyBusShardIndex(idY) {
		t.Fatalf("test setup broken: %q and %q are not on the same shard", idX, idY)
	}
	if idX == idY {
		t.Fatal("test setup broken: colliding IDs must be distinct")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	chX := busA.Subscribe(ctx, idX)
	chY := busA.Subscribe(ctx, idY)
	// Both applyIDs on one shard → one Redis subscription (refs=2).
	waitFor(t, 2*time.Second, func() bool { return redisSubCount(mr, idX) == 1 },
		"shared shard bridge not registered as single redis-subscriber")
	if got := redisSubCount(mr, idY); got != 1 {
		t.Fatalf("redis subscribers for shared shard = %d, want 1 (idX and idY share one bridge)", got)
	}

	// Publish both simultaneously from another Keeper (busB).
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		busB.Publish(Event{ApplyID: idX, Kind: KindApplyCompleted, Payload: map[string]any{"who": "X"}})
	}()
	go func() {
		defer wg.Done()
		busB.Publish(Event{ApplyID: idY, Kind: KindApplyFailed, Payload: map[string]any{"who": "Y"}})
	}()
	wg.Wait()

	// chX must receive ONLY the X event; chY only Y. Read one each and check
	// payload + applyID.
	assertOwnEvent := func(name string, ch <-chan Event, wantID, wantWho string, wantKind EventKind) {
		ev := recvWithin(t, ch, 3*time.Second)
		if ev.ApplyID != wantID {
			t.Errorf("%s: ApplyID = %q, want %q (cross-applyID leak on shared shard)", name, ev.ApplyID, wantID)
		}
		if ev.Kind != wantKind {
			t.Errorf("%s: Kind = %q, want %q", name, ev.Kind, wantKind)
		}
		raw, ok := ev.Payload.(json.RawMessage)
		if !ok {
			t.Fatalf("%s: Payload type = %T, want json.RawMessage", name, ev.Payload)
		}
		var dec map[string]any
		if err := json.Unmarshal(raw, &dec); err != nil {
			t.Fatalf("%s: payload unmarshal: %v", name, err)
		}
		if dec["who"] != wantWho {
			t.Errorf("%s: payload who = %v, want %q (payload mixed across colliding applyIDs)", name, dec["who"], wantWho)
		}
	}
	assertOwnEvent("chX", chX, idX, "X", KindApplyCompleted)
	assertOwnEvent("chY", chY, idY, "Y", KindApplyFailed)

	// No extra (foreign) events on either channel.
	select {
	case extra := <-chX:
		t.Errorf("chX got extra event after its own: %+v (cross-applyID leak)", extra)
	case <-time.After(300 * time.Millisecond):
	}
	select {
	case extra := <-chY:
		t.Errorf("chY got extra event after its own: %+v (cross-applyID leak)", extra)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestCluster_ShardBridge_RefcountAcrossApplyIDs — two DIFFERENT applyIDs on
// one shard share a single Redis subscription (refs per shard). Unsubscribing
// the first does NOT close the bridge (the second applyID still holds the
// shard); unsubscribing the second does. Guard on per-shard refcounting (S2):
// the bridge lives as long as the shard has at least one held subscriber,
// regardless of applyID.
func TestCluster_ShardBridge_RefcountAcrossApplyIDs(t *testing.T) {
	c, mr := newClusterRedis(t)
	b := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")

	idX, idY := collidingApplyIDs(t)
	if keeperredis.ApplyBusShardIndex(idX) != keeperredis.ApplyBusShardIndex(idY) {
		t.Fatalf("test setup broken: %q and %q not on same shard", idX, idY)
	}

	ctxX, cancelX := context.WithCancel(context.Background())
	_ = b.Subscribe(ctxX, idX)
	waitFor(t, 2*time.Second, func() bool { return redisSubCount(mr, idX) == 1 },
		"first applyID did not raise shard bridge")

	ctxY, cancelY := context.WithCancel(context.Background())
	defer cancelY()
	_ = b.Subscribe(ctxY, idY)
	// Same shard → refs++, not a second Redis subscription.
	waitFor(t, 2*time.Second, func() bool { return b.Subscribers(idY) == 1 },
		"second applyID subscriber not registered")
	time.Sleep(50 * time.Millisecond)
	if n := redisSubCount(mr, idY); n != 1 {
		t.Fatalf("redis subscribers = %d for shared shard, want 1 (refs++, not second subscribe)", n)
	}

	// Unsubscribe the first applyID — the bridge MUST survive (idY still
	// holds the shard).
	cancelX()
	waitFor(t, 2*time.Second, func() bool { return b.Subscribers(idX) == 0 },
		"first applyID not unsubscribed")
	time.Sleep(50 * time.Millisecond)
	if n := redisSubCount(mr, idX); n != 1 {
		t.Fatalf("redis subscribers = %d after first unsubscribe, want 1 (bridge must survive for idY)", n)
	}

	// Unsubscribe the second — now the shard's refs = 0 → the bridge closes.
	cancelY()
	waitFor(t, 3*time.Second, func() bool { return redisSubCount(mr, idY) == 0 },
		"shard bridge did not close after last applyID unsubscribed")
}

// TestConcurrentPublishVsUnsubscribe — regression guard for "send on closed
// channel": Publish snapshots subscribers under RLock and delivers OUTSIDE
// the lock, while unsubscribe concurrently removes a subscriber. Under
// variant A (done-channel, s.ch never closed), deliver selecting on done
// silently stops delivery — no panic, no race writing to a closed channel.
// Reproducible on the local bus too (no Redis): this is NOT cluster-specific.
//
// Must pass cleanly under `go test -race ./internal/applybus/...` with no
// special flags. The reader exits on ctx.Done() (ch is never closed).
func TestConcurrentPublishVsUnsubscribe(t *testing.T) {
	b := NewBus(clusterTestLogger())
	const applyID = "01J0PROBE000000000000000AB"
	var wg sync.WaitGroup
	for w := 0; w < 32; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				ctx, cancel := context.WithCancel(context.Background())
				ch := b.Subscribe(ctx, applyID)
				go func() {
					// ch is never closed — exit on ctx cancellation.
					for {
						select {
						case <-ch:
						case <-ctx.Done():
							return
						}
					}
				}()
				b.Publish(Event{ApplyID: applyID, Kind: KindTaskExecuted})
				cancel()
			}
		}()
	}
	wg.Wait()
}

// TestCluster_ShardFanout_NoSilentLossNoMix — fan-out: N>10 distinct applyIDs
// colliding on ONE shard are published from another Keeper at a reasonable rate
// (readers drain concurrently). One forward-loop per shard serves all applyIDs:
// each channel receives its own events in order, nothing leaks between
// applyIDs, and every event that does not arrive was shed by a buffer that said
// so. Extends TestCluster_ShardCollision_NoPayloadMix to N>2 channels.
//
// The test was called NoDropNoMix and demanded all perID events on every
// channel. The bus never promised that: both buffers on this path are
// drop-oldest by policy, and the one that governs here belongs to the SHARD,
// not to the applyID. redis.applyEventSubBufferSize is 64 slots per shard
// subscription while this test deliberately collides fanout×perID = 320 events
// onto one shard, so overflow is not an anomaly here — it is the design working
// as documented. The reassuring arithmetic "20 events, 64 slots, nothing can be
// dropped" reads applybus.SubscriberBufferSize, which is a different buffer and
// says nothing about the bridge. A shortfall is therefore not by itself a
// defect; a shortfall nobody logged is (NIM-363).
//
// So this asserts what the bus actually guarantees:
//
//   - order and ownership ALWAYS, including on a shortfall. The old assert did
//     `continue` when the count was short, which skipped the order check
//     precisely in the runs where something had gone wrong — the NIM-238 shape:
//     the guarantee went unchecked exactly when it mattered.
//   - accounting: events missing == events the code reported shedding. Policy
//     loss passes, silent loss fails.
//   - no surplus or duplicated delivery.
func TestCluster_ShardFanout_NoSilentLossNoMix(t *testing.T) {
	losses := &lossCounter{}
	c, mr := newClusterRedis(t)
	busA := NewBusWithRedis(slog.New(losses), c, "keeper-A")
	busB := NewBusWithRedis(clusterTestLogger(), c, "keeper-B")

	const (
		fanout       = 16 // N>10 applyIDs on one shard
		perID        = 20 // events per applyID
		drainTimeout = 5 * time.Second
	)
	ids := collidingApplyIDsN(t, fanout)
	shard := keeperredis.ApplyBusShardIndex(ids[0])
	for _, id := range ids {
		if keeperredis.ApplyBusShardIndex(id) != shard {
			t.Fatalf("setup broken: %q not on shard %d", id, shard)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Subscriptions to all applyIDs of one shard → one shared Redis
	// subscription (refs=N).
	chans := make(map[string]<-chan Event, fanout)
	for _, id := range ids {
		chans[id] = busA.Subscribe(ctx, id)
	}
	waitFor(t, 2*time.Second, func() bool { return redisSubCount(mr, ids[0]) == 1 },
		"fanout subscriptions did not collapse to single shard bridge")

	// Concurrent readers: collect seq for their own applyID. Draining
	// concurrently keeps the shard buffer from overflowing most runs, but
	// whether it did is measured below, not assumed here.
	type collected struct {
		mu       sync.Mutex
		seq      []int
		timedOut bool
	}
	got := make(map[string]*collected, fanout)
	var rwg sync.WaitGroup
	for _, id := range ids {
		got[id] = &collected{}
		ch := chans[id]
		col := got[id]
		wantID := id
		rwg.Add(1)
		go func() {
			defer rwg.Done()
			// Read until perID events have arrived, or give up on ONE hard
			// deadline — never on silence.
			//
			// This loop used to re-arm a 500 ms "idle" timer after every event
			// and return when it fired. That cannot distinguish "the stream is
			// over" from "the scheduler stalled", and under load it read the
			// second as the first: the collector returned while the tail of the
			// stream was still in flight, the length assert below found 18 or 19
			// of 20, and the message it printed said "drop or loss under
			// fanout". Nothing had been dropped — SubscriberBufferSize is 64 and
			// only 20 events per applyID are ever published, so EventBus.deliver
			// cannot overflow here. The reader left first.
			//
			// It took the first CI run over the release to surface (NIM-297,
			// three applyIDs short) and a loaded machine to reproduce: 20 runs
			// on an idle box are green, 8 of 20 fail at loadavg ~5, and the
			// affected subset varies — the signature of a shared stall, not of a
			// systematic drop. A test that reports a scheduling artefact as
			// event loss spends the reader's time on the wrong subsystem, and if
			// it were "fixed" by widening the window it would stop being able to
			// tell the two apart at all (NIM-318).
			//
			// The deadline no longer decides whether events were lost — the
			// shed-log does (see the accounting assert below). It only bounds how
			// long this reader is willing to wait, and it records giving up so a
			// shortfall can name which of the two happened.
			hard := time.NewTimer(drainTimeout)
			defer hard.Stop()
			for {
				col.mu.Lock()
				done := len(col.seq) >= perID
				col.mu.Unlock()
				if done {
					break
				}
				select {
				case ev := <-ch:
					if ev.ApplyID != wantID {
						t.Errorf("cross-applyID leak: got ApplyID=%q on channel for %q", ev.ApplyID, wantID)
					}
					var dec struct {
						Seq int `json:"seq"`
					}
					raw, ok := ev.Payload.(json.RawMessage)
					if !ok {
						t.Errorf("payload type %T, want json.RawMessage", ev.Payload)
						continue
					}
					if err := json.Unmarshal(raw, &dec); err != nil {
						t.Errorf("payload unmarshal: %v", err)
						continue
					}
					col.mu.Lock()
					col.seq = append(col.seq, dec.Seq)
					col.mu.Unlock()
				case <-hard.C:
					// Short of perID after drainTimeout. Whether that is loss or
					// a stall is not this goroutine's call — record that it gave
					// up and let the accounting below say which.
					col.mu.Lock()
					col.timedOut = true
					col.mu.Unlock()
					return
				}
			}
			// perID reached. Keep the "no duplicate / no surplus delivery" half
			// of the guarantee: a brief look for one more event. Silence here is
			// weak evidence under load, but the asymmetry is in our favour —
			// a surplus event is a positive observation, so a short window can
			// only miss one, never invent one.
			select {
			case ev := <-ch:
				extra := "unreadable payload"
				if raw, ok := ev.Payload.(json.RawMessage); ok {
					extra = string(raw)
				}
				t.Errorf("surplus delivery for %q: an event beyond the %d published (%s)", wantID, perID, extra)
			case <-time.After(200 * time.Millisecond):
			}
		}()
	}

	// Publish from busB (a different KID) per-applyID sequentially: one
	// applyID — monotonic seq 0..perID-1; a reasonable rate (no burst in a
	// single tick).
	var pwg sync.WaitGroup
	for _, id := range ids {
		id := id
		pwg.Add(1)
		go func() {
			defer pwg.Done()
			for s := 0; s < perID; s++ {
				busB.Publish(Event{
					ApplyID: id,
					Kind:    KindTaskExecuted,
					Payload: map[string]any{"seq": s},
				})
				time.Sleep(time.Millisecond)
			}
		}()
	}
	pwg.Wait()
	rwg.Wait()

	// Order and ownership are checked for EVERY applyID, short or not: the run
	// that lost something is exactly the run where scrambling would matter, and
	// skipping the check there is how a guarantee stops being guarded. How many
	// events arrived is settled once, below, against what the buses shed.
	shortfall, gaveUp := 0, 0
	for _, id := range ids {
		col := got[id]
		col.mu.Lock()
		seq := append([]int(nil), col.seq...)
		timedOut := col.timedOut
		col.mu.Unlock()

		prev := -1
		for i, s := range seq {
			if s < 0 || s >= perID {
				t.Errorf("applyID %q: event[%d] seq = %d, outside the %d published", id, i, s, perID)
				break
			}
			if s <= prev {
				t.Errorf("applyID %q: event[%d] seq = %d after %d — per-applyID order scrambled or duplicated",
					id, i, s, prev)
				break
			}
			prev = s
		}

		if missing := perID - len(seq); missing > 0 {
			shortfall += missing
			if timedOut {
				gaveUp++
			}
			t.Logf("applyID %q: %d of %d events, missing seq %v (reader gave up: %t)",
				id, len(seq), perID, missingSeq(seq, perID), timedOut)
		}
	}

	// The count that decides. Every event that did not arrive must be an event a
	// buffer reported shedding; anything beyond that is loss nobody reported,
	// which is what this test exists to catch. No timeout can be widened into
	// making this assert pass.
	bridge, local := losses.counts()
	switch {
	case shortfall != bridge+local:
		t.Errorf("%d events missing but %d shed (%d shard bridge, %d subscriber buffers) — the two must match. "+
			"%d of %d readers hit the %s deadline; a missing TAIL behind a reader that gave up is a stall, "+
			"gaps in the MIDDLE are a buffer evicting without saying so",
			shortfall, bridge+local, bridge, local, gaveUp, fanout, drainTimeout)
	case shortfall > 0:
		t.Logf("%d events shed under fan-out (%d shard bridge, %d subscriber buffers), all of them logged — "+
			"drop-oldest working as documented, not loss", shortfall, bridge, local)
	}
}
