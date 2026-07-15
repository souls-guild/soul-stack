// Package applybus — in-memory pub/sub bus for apply-events keyed by apply_id.
//
// Purpose — connect publishers (EventStream-payload handlers for
// TaskEvent / RunResult, in future keeper-side scenario-runner) with
// subscribers of the SSE stream `GET /mcp/events?apply_id=<ULID>` (M0.7.c).
//
// Contract per PM decisions M0.7.c:
//
//   - in-memory single-Keeper; cluster-wide pub/sub over Redis is a separate
//     layer, activated via [NewBusWithRedis] (M2.6, ADR-006(c));
//   - per-subscriber buffer of 64 events: overflow → drop oldest + warn;
//   - a subscriber lives until its ctx is cancelled (Unsubscribe is idempotent);
//   - Publish is non-blocking: one slow delivery must not block the
//     publisher (EventStream handler).
//
// Cluster-mode (M2.6, ADR-006(c)): with non-nil redis/kid, Publish
// additionally publishes the event to a sharded Redis channel
// `events:shard:<n>` (applyID → shard via [keeperredis.ApplyBusChannel])
// through [keeperredis.PublishApplyEvent]. The Redis bridge is set up
// per-SHARD, not per-applyID (S2 applybus-bottleneck, ADR-006(c) amendment):
// the first Subscribe for any applyID mapped to a given shard opens one
// subscription to the shard channel; later applyIDs on the same shard reuse
// it (refs are counted per shard). The forward-loop filters incoming events
// by `envelope.ApplyID` and delivers only to local subscribers of the
// matching applyID. A self-filter on origin_kid drops echoes of the bus's
// own publications (see doc-comment on `keeper/internal/redis/applybus.go`).
package applybus

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
)

// EventKind — apply-event type. Stable snake_case names, used verbatim as
// the SSE event type (`event: task.executed\n…`). The set is fixed; new
// kinds are added here and in the pub/sub publishers.
type EventKind string

const (
	// KindApplyStarted — apply run started. Source: keeper-side
	// scenario-runner (M2.x). No publisher yet in M0.7.c — kept in the
	// contract for forward-compat with SSE clients.
	KindApplyStarted EventKind = "apply.started"

	// KindTaskExecuted — one task within a run finished (any status, see
	// TaskStatus in proto). Source: events_taskevent.go.
	KindTaskExecuted EventKind = "task.executed"

	// KindApplyCompleted — run finished successfully. Source:
	// events_runresult.go on RUN_STATUS_SUCCESS.
	KindApplyCompleted EventKind = "apply.completed"

	// KindApplyFailed — run finished with an error (FAILED / ERROR_LOCKED).
	// Source: events_runresult.go.
	KindApplyFailed EventKind = "apply.failed"

	// KindApplyCancelled — run was cancelled (CANCELLED). Source:
	// events_runresult.go.
	KindApplyCancelled EventKind = "apply.cancelled"

	// KindErrandCompleted / KindErrandFailed / KindErrandTimedOut /
	// KindErrandCancelled / KindErrandModuleNotAllowed — terminal Errand
	// statuses (ADR-033). Source: events_errand.go (FromSoul.ErrandResult
	// handler). Errand events travel the same shard namespace
	// `events:shard:<n>` as apply runs: errand_id maps to a shard via
	// [keeperredis.ApplyBusChannel] just like apply_id, and the forward-loop
	// filters incoming events by `ev.ApplyID` (see package doc-comment).
	//
	// Kept as a separate family from apply.*: SSE clients filtering by
	// `kind` can distinguish Errand events from apply runs.
	KindErrandCompleted        EventKind = "errand.completed"
	KindErrandFailed           EventKind = "errand.failed"
	KindErrandTimedOut         EventKind = "errand.timed_out"
	KindErrandCancelled        EventKind = "errand.cancelled"
	KindErrandModuleNotAllowed EventKind = "errand.module_not_allowed"
)

// Event — one apply-event delivered to a subscriber.
//
// Payload — an arbitrary typed payload that the publisher serializes for a
// specific SSE event. The SSE handler does json.Marshal(Payload) into the
// `data:` block. Payload shapes are fixed in docs/keeper/mcp-tools.md → § SSE
// (a separate doc slice).
type Event struct {
	ApplyID string
	Kind    EventKind
	At      time.Time
	Payload any
}

// SubscriberBufferSize — per-subscriber channel buffer. A "magic" value: 64
// events comfortably cover a typical apply run (10–30 tasks + start + final)
// with headroom; overflow triggers drop-oldest (see [EventBus.Publish]). Not
// worth making configurable — internal flow-control, symmetric with
// StreamManager (PM decision M2.5 buffered=10).
const SubscriberBufferSize = 64

// clusterPublishTimeout — deadline for the network Redis PUBLISH in
// cluster-mode (see [EventBus.publishToCluster]): if Redis is unavailable,
// the publisher blocks no longer than this; on error it warns, not returns.
const clusterPublishTimeout = time.Second

// subscriber — one subscriber for a specific apply_id.
//
// ch is NEVER closed (done-channel variant A): closing a channel that
// Publish writes to outside the lock would race unavoidably with delivery
// ("send on closed channel"). Instead, unsubscribe closes done;
// [EventBus.deliver] selecting on done silently stops delivery. The consumer
// tracks its own ctx (see [EventBus.Subscribe]), not close(ch).
type subscriber struct {
	ch   chan Event
	done chan struct{}
	// heldBridge — true if this subscriber incremented the Redis bridge refs
	// on its SHARD (Subscribe with wantBridge=true in cluster-mode).
	// unsubscribe decrements shard refs only for these — otherwise a
	// local-only subscriber (wantBridge=false) could wrongly tear down a
	// bridge raised by a sibling subscriber on the same shard.
	heldBridge bool
}

// clusterBridge — handle for one Redis subscription to a shard channel
// `events:shard:<n>`, alive while the shard has at least one local
// subscriber with heldBridge.
//
// refs — count of held subscribers PER SHARD (multiple applyIDs share one
// subscription). At refs=0 the bridge is closed (see
// [EventBus.unsubscribe]). cancel — cancels the subscription ctx; sub — the
// underlying [keeperredis.ApplyEventSubscription] (closed via sub.Close).
type clusterBridge struct {
	refs   int
	cancel context.CancelFunc
	sub    *keeperredis.ApplyEventSubscription
}

// EventBus — pub/sub bus for apply-events. Thread-safe.
//
// Internally: map[apply_id][]*subscriber under an RWMutex. Publish holds the
// read-lock only briefly (to snapshot the subscriber slice for a given
// apply_id); actual delivery happens outside the lock — otherwise one slow
// subscriber would stall publishing for everyone else on the same apply_id.
//
// With non-nil redis/kid (cluster-mode, M2.6) the bus also keeps a per-SHARD
// Redis bridge via [clusterBridge], forwards cross-Keeper events to local
// subscribers (filtered by applyID), and publishes local events to Redis.
type EventBus struct {
	mu sync.RWMutex
	// subs — local subscribers keyed by applyID (delivery is targeted).
	subs map[string][]*subscriber
	// bridges — Redis subscriptions keyed by shard-channel index (multiple
	// applyIDs share one subscription). Key: [keeperredis.ApplyBusShardIndex](applyID).
	bridges map[uint32]*clusterBridge
	redis   *keeperredis.Client
	kid     string
	logger  *slog.Logger
}

// NewBus builds a single-Keeper bus (no Redis bridge). logger is required —
// drop-oldest warnings and late-publish messages go to slog.
func NewBus(logger *slog.Logger) *EventBus {
	return NewBusWithRedis(logger, nil, "")
}

// NewBusWithRedis builds a bus with an optional cluster bridge over Redis
// (ADR-006(c)). With redis=nil or kid="", cluster-mode is off — behavior is
// identical to [NewBus] (single-Keeper).
//
// kid is required for the self-filter: the cluster bridge drops events with
// origin_kid == own KID, otherwise local-Publish + Redis-echo would
// double-deliver to SSE clients.
func NewBusWithRedis(logger *slog.Logger, redis *keeperredis.Client, kid string) *EventBus {
	if logger == nil {
		logger = slog.Default()
	}
	b := &EventBus{
		subs:   make(map[string][]*subscriber),
		logger: logger,
	}
	if redis != nil && kid != "" {
		b.redis = redis
		b.kid = kid
		b.bridges = make(map[uint32]*clusterBridge)
	}
	return b
}

// clusterEnabled — true if the bus is configured for cluster-mode (has a
// redis client and a KID).
func (b *EventBus) clusterEnabled() bool {
	return b.redis != nil && b.kid != ""
}

// Subscribe returns a channel that receives all events for applyID
// published AFTER Subscribe returns. A late subscriber misses events
// already delivered to the publisher (this is a pub/sub bus, not a log).
//
// The subscription ends on ctx.Done() (SSE client disconnected or the
// handler context expired) — an internal goroutine does the unsubscribe.
// The channel is NOT closed: the consumer tracks its own ctx, not close(ch)
// (see the subscriber type doc-comment). Callers must not call Unsubscribe
// explicitly.
//
// In cluster-mode, the first Subscribe(applyID) mapped to a given shard
// raises a Redis bridge on channel `events:shard:<n>`; the last Unsubscribe
// of the last held applyID on that shard tears the bridge down. Subscribe
// waits synchronously for the Redis subscription to become Ready before
// returning (to rule out the race "Subscribe returns → Publish on another
// Keeper → subscription not yet registered in Redis").
//
// Equivalent to SubscribeWithBridge(ctx, applyID, true) — kept for
// back-compat with all existing callers.
func (b *EventBus) Subscribe(ctx context.Context, applyID string) <-chan Event {
	return b.SubscribeWithBridge(ctx, applyID, true)
}

// SubscribeWithBridge — a variant of [Subscribe] with explicit control over
// the Redis bridge.
//
// wantBridge=false skips [EventBus.ensureClusterBridgeLocked]: the
// subscription gets only local delivery (via [EventBus.Publish] on the same
// instance), not cross-Keeper events from Redis. The rest of the lifecycle
// (subscriber registration, refs/unsubscribe on ctx, non-closing ch) is
// identical to [Subscribe] — one code path.
//
// Use case (S1, applybus-bottleneck): a caller that already knows the event
// will only come from a local publisher on the same instance (lease-holder
// of the target SID == self-KID) can pass wantBridge=false — this skips the
// per-applyID Redis-Subscribe and removes the maxclients cliff on large
// deployments. If the holder is unknown or may change, the caller must
// request wantBridge=true (conservative default), otherwise a cross-Keeper
// event won't reach this subscription.
//
// In single-Keeper mode (cluster disabled), wantBridge is ignored — no
// bridge is raised either way.
func (b *EventBus) SubscribeWithBridge(ctx context.Context, applyID string, wantBridge bool) <-chan Event {
	if ctx == nil {
		ch := make(chan Event)
		close(ch)
		return ch
	}
	if applyID == "" {
		// Guard against accidental Subscribe(""): a clear caller bug, nothing
		// useful would ever arrive on such a channel.
		ch := make(chan Event)
		close(ch)
		return ch
	}

	sub := &subscriber{
		ch:   make(chan Event, SubscriberBufferSize),
		done: make(chan struct{}),
	}

	b.mu.Lock()
	b.subs[applyID] = append(b.subs[applyID], sub)
	var bridgeReady <-chan struct{}
	if wantBridge && b.clusterEnabled() {
		// refs is incremented only when a bridge is actually raised/reused;
		// heldBridge guarantees a symmetric decrement in unsubscribe (see
		// the subscriber.heldBridge doc-comment).
		sub.heldBridge = true
		bridgeReady = b.ensureClusterBridgeLocked(applyID)
	}
	b.mu.Unlock()

	if bridgeReady != nil {
		// Wait for Ready without holding the lock. If the bridge fails before
		// Ready, just ignore it (local delivery keeps working, cross-Keeper
		// events simply won't reach this subscriber).
		select {
		case <-bridgeReady:
		case <-ctx.Done():
		}
	}

	go func() {
		<-ctx.Done()
		b.unsubscribe(applyID, sub)
	}()

	return sub.ch
}

// ensureClusterBridgeLocked raises (or increments refs on) the Redis bridge
// for the SHARD that applyID maps to. Called under b.mu write-lock.
//
// Returns a chan the subscribe-loop signals Ready on. nil if cluster-mode is
// disabled or a bridge for this shard already existed (refs++).
func (b *EventBus) ensureClusterBridgeLocked(applyID string) <-chan struct{} {
	if !b.clusterEnabled() {
		return nil
	}
	shard := keeperredis.ApplyBusShardIndex(applyID)
	if br, ok := b.bridges[shard]; ok {
		br.refs++
		return nil
	}

	// Background ctx, because the bridge must live until refs=0 explicitly,
	// independent of the first Subscribe's ctx. applyID is passed to
	// SubscribeApplyEvent only as a shard-selector/log-label — the
	// subscription is to the shard channel, which carries all applyIDs of
	// that shard.
	ctx, cancel := context.WithCancel(context.Background())
	sub, err := keeperredis.SubscribeApplyEvent(ctx, b.redis, applyID, b.kid, b.logger)
	if err != nil {
		cancel()
		b.logger.Warn("applybus: cluster-bridge subscribe failed",
			slog.Uint64("shard", uint64(shard)),
			slog.String("apply_id", applyID),
			slog.Any("error", err),
		)
		return nil
	}
	br := &clusterBridge{refs: 1, cancel: cancel, sub: sub}
	b.bridges[shard] = br

	ready := make(chan struct{})
	go func() {
		// Wait for the Redis subscription to become ready and signal the caller.
		readyCtx, readyCancel := context.WithTimeout(ctx, 5*time.Second)
		defer readyCancel()
		if err := sub.Ready(readyCtx); err != nil {
			b.logger.Warn("applybus: cluster-bridge Ready failed",
				slog.Uint64("shard", uint64(shard)),
				slog.String("apply_id", applyID),
				slog.Any("error", err),
			)
		}
		close(ready)

		// Forward-loop: Redis event → local subscribers. applyID comes from
		// the event itself (a shard channel carries many applyIDs); events for
		// other applyIDs are dropped simply by having no local subscribers in
		// [deliverFromCluster]. Ends when sub.Channel() closes (via Close in
		// unsubscribe).
		for ev := range sub.Channel() {
			b.deliverFromCluster(ev)
		}
	}()
	return ready
}

// deliverFromCluster delivers a cross-Keeper event to local subscribers of
// exactly the applyID the event carries (`ev.ApplyID`). This is the shard
// filter itself: one shard channel carries events for many applyIDs, but
// each is routed only to b.subs[ev.ApplyID]; if there are no local
// subscribers for that applyID (a colliding applyID's event), the snapshot
// is empty → no-op.
//
// Origin filtering already happened in SubscribeApplyEvent (see the
// self-filter in `keeper/internal/redis/applybus.go`); here we only convert
// the json.RawMessage payload to any (json.RawMessage itself implements
// json.Marshaler, so the SSE handler re-serializes it into the frame
// correctly without double-encoding).
func (b *EventBus) deliverFromCluster(ev *keeperredis.ApplyEvent) {
	b.mu.RLock()
	subs := b.subs[ev.ApplyID]
	if len(subs) == 0 {
		b.mu.RUnlock()
		return
	}
	snapshot := make([]*subscriber, len(subs))
	copy(snapshot, subs)
	b.mu.RUnlock()

	out := Event{
		ApplyID: ev.ApplyID,
		Kind:    EventKind(ev.Kind),
		At:      ev.At,
		Payload: ev.Payload,
	}
	for _, s := range snapshot {
		b.deliver(out, s)
	}
}

// Publish delivers ev to all active subscribers of ev.ApplyID. Purely
// non-blocking: if a subscriber's channel is full, we drop the oldest event
// and log a warn. This preserves the "publisher never blocks" property at
// the cost of possibly dropping an event for a slow client.
//
// Late subscriber: an event sent before Subscribe is lost (in-memory bus).
// A deliberate MVP simplification: the SSE client must subscribe BEFORE the
// async tool call (order: "subscribe → tools/call → wait for SSE events").
//
// In cluster-mode (see [NewBusWithRedis]), after local delivery the event is
// also published to the shard channel `events:shard:<n>` (applyID → shard)
// via [keeperredis.PublishApplyEvent]. Subscribers on other Keeper instances
// subscribed to the same shard receive the event through their cluster
// bridge and filter it by applyID. Redis-publish errors are logged as warn —
// local delivery has already happened, the publisher still doesn't block.
func (b *EventBus) Publish(ev Event) {
	if ev.ApplyID == "" {
		return
	}
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}

	// Snapshot under the read-lock; delivery itself happens without holding
	// the bus lock — a slow subscriber doesn't block the rest.
	b.mu.RLock()
	subs := b.subs[ev.ApplyID]
	snapshot := make([]*subscriber, len(subs))
	copy(snapshot, subs)
	b.mu.RUnlock()

	for _, s := range snapshot {
		b.deliver(ev, s)
	}

	if b.clusterEnabled() {
		b.publishToCluster(ev)
	}
}

// publishToCluster serializes the payload and publishes it to Redis. Pure
// best-effort: errors are logged and ignored (the publisher keeps going,
// local delivery has already happened).
func (b *EventBus) publishToCluster(ev Event) {
	var payload json.RawMessage
	if ev.Payload != nil {
		raw, err := json.Marshal(ev.Payload)
		if err != nil {
			b.logger.Warn("applybus: cluster-publish payload marshal failed",
				slog.String("apply_id", ev.ApplyID),
				slog.String("kind", string(ev.Kind)),
				slog.Any("error", err),
			)
			return
		}
		payload = raw
	}
	// Deadline on the network PUBLISH: don't block the publisher for long if
	// Redis is unavailable. Error → warn, not a returned error.
	ctx, cancel := context.WithTimeout(context.Background(), clusterPublishTimeout)
	defer cancel()
	if _, err := keeperredis.PublishApplyEvent(ctx, b.redis, ev.ApplyID, b.kid, string(ev.Kind), ev.At, payload); err != nil {
		b.logger.Warn("applybus: cluster-publish failed",
			slog.String("apply_id", ev.ApplyID),
			slog.String("kind", string(ev.Kind)),
			slog.Any("error", err),
		)
	}
}

// deliver pushes an event into the subscriber's channel. On a full channel,
// drops the oldest entry (reads one element to free a slot) and logs a
// warning. Drop-oldest beats drop-newest because for an SSE client the
// freshest state is more useful than a stale "task_idx=0 OK".
func (b *EventBus) deliver(ev Event, s *subscriber) {
	select {
	case s.ch <- ev:
		return
	case <-s.done:
		// Subscriber unsubscribed — delivery silently stops (ch isn't closed,
		// no panic).
		return
	default:
	}
	// Channel full — try to free a slot. Reading from a never-closed channel
	// is safe and doesn't race with unsubscribe.
	select {
	case <-s.ch:
		b.logger.Warn("applybus: subscriber buffer full — dropped oldest event",
			slog.String("apply_id", ev.ApplyID),
			slog.String("kind", string(ev.Kind)),
			slog.Int("buffer", SubscriberBufferSize),
		)
	default:
		// Channel was already drained by a concurrent reader between the two
		// selects — just write the new event below.
	}
	select {
	case s.ch <- ev:
	case <-s.done:
		return
	default:
		// Unlikely (subscriber fully deadlocked), but don't block: the
		// publisher-non-block guarantee matters more than one event.
		b.logger.Warn("applybus: subscriber still full after drop — event lost",
			slog.String("apply_id", ev.ApplyID),
			slog.String("kind", string(ev.Kind)),
		)
	}
}

// unsubscribe removes the subscriber from the map and closes its done
// channel. Idempotent: a repeat call on an already-removed subscriber is a
// no-op (early return on idx<0, done is closed exactly once).
//
// s.ch is NEVER closed. Closing a channel that Publish writes to outside the
// lock would race unavoidably with delivery. Instead we close done; deliver
// selecting on done silently stops delivery (see [EventBus.deliver]). The
// consumer exits on its own ctx, not on close(ch).
//
// On the last Unsubscribe of the last held applyID on a shard (cluster-mode),
// the corresponding Redis bridge is torn down — bridge.refs is decremented,
// and at zero, bridge.cancel + sub.Close stop the forward-loop. The bridge is
// closed after releasing the lock.
func (b *EventBus) unsubscribe(applyID string, s *subscriber) {
	b.mu.Lock()
	subs := b.subs[applyID]
	idx := -1
	for i, x := range subs {
		if x == s {
			idx = i
			break
		}
	}
	if idx < 0 {
		b.mu.Unlock()
		return
	}
	// Order-preserving removal not needed — O(1) swap-and-truncate.
	subs[idx] = subs[len(subs)-1]
	subs[len(subs)-1] = nil
	subs = subs[:len(subs)-1]
	if len(subs) == 0 {
		delete(b.subs, applyID)
	} else {
		b.subs[applyID] = subs
	}

	var bridgeToClose *clusterBridge
	if b.clusterEnabled() && s.heldBridge {
		// Decrement the SHARD's refs only if this subscriber held a bridge ref
		// (wantBridge=true). A local-only subscriber (wantBridge=false) leaves
		// refs untouched — otherwise it would tear down a sibling's bridge on
		// the same shard.
		//
		// refs >= 1 is what keeps THIS bridge instance alive: as long as one
		// held subscriber is alive, bridge.refs > 0 and the shard subscription
		// stays up. The decrement is addressed by shardIndex(applyID) under
		// b.mu — it hits exactly the bridge of its own shard; a bridge on a
		// different shard (a different map key) is untouched, and a concurrent
		// unsubscribe on another bridge instance is likewise serialized by the
		// same b.mu, so refs never goes negative.
		shard := keeperredis.ApplyBusShardIndex(applyID)
		if br, ok := b.bridges[shard]; ok {
			br.refs--
			if br.refs <= 0 {
				delete(b.bridges, shard)
				bridgeToClose = br
			}
		}
	}
	b.mu.Unlock()

	close(s.done)

	if bridgeToClose != nil {
		bridgeToClose.cancel()
		_ = bridgeToClose.sub.Close()
	}
}

// Subscribers returns the number of active subscribers for applyID. For
// tests / diagnostics only.
func (b *EventBus) Subscribers(applyID string) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs[applyID])
}
