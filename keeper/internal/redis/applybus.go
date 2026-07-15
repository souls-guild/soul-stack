package redis

// Cluster-mode SSE routing of apply events via Redis pub/sub (ADR-006(c)).
//
// Problem: the in-memory [applybus.EventBus] (M0.7.c) lives inside a single
// Keeper instance. If an SSE subscriber landed on Keeper-A
// (`GET /mcp/events?apply_id=X` parked there) while the Soul streaming
// TaskEvent/RunResult over EventStream connected to Keeper-B, the publisher
// on Keeper-B would write events only to its own local bus, and the SSE
// client on Keeper-A would never see them.
//
// Solution is symmetric to outbound routing (see [PublishOutbound] /
// [SubscribeOutbound]): the publisher additionally PUBLISHes to a sharded
// Redis channel `events:shard:<n>`; all Keeper instances subscribe to the
// relevant shards and forward events into their local bus. A self-filter on
// `origin_kid` strips echoes of a node's own publishes.
//
// Sharding (ADR-006(c) amendment, S2 applybus-bottleneck): a per-applyID
// channel `apply:<apply_id>` produced as many Redis subscriptions as
// concurrent runs on deployments with many Souls, hitting `maxclients`. The
// channel space is now a fixed set of [ApplyBusShardCount] shards; applyID
// maps deterministically to a shard via [shardIndex] (fnv32a % K). Several
// applyIDs share one shard subscription; the forward-loop on the applybus
// side filters out events belonging to other applyIDs by `envelope.ApplyID`
// (see `keeper/internal/applybus/bus.go`).
//
// Convention `events:shard:<n>` — a separate namespace from
// `outbound:<sid>`/`soul:<sid>:hb`/`soul:<sid>:lock`; the keyword "events" is
// symmetric with EventKinds (`apply.started`/`apply.completed`/
// `errand.completed`/…) and isn't tied to a single opaque-id family.
//
// Wire format is the JSON envelope `applyEventEnvelope`. Payload stays a
// `json.RawMessage` to avoid double re-encoding: the publisher already hands
// it over as a json-serializable `map[string]any` (see events_taskevent.go /
// events_runresult.go).
//
// Semantics: fire-and-forget. Redis pub/sub has no persistence — if a
// subscriber hasn't subscribed yet at PUBLISH time, the message is lost.
// That's acceptable: by contract, an SSE client subscribes BEFORE starting
// an apply (order is "subscribe → tools/call → wait for SSE events"; see the
// doc-comment on [applybus.EventBus.Publish] about late-subscriber semantics).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// applyBusChannelPrefix is the namespace for sharded apply-events channels.
// The word "events" is symmetric with EventKinds and isn't tied to a single
// opaque-id family (apply.* / errand.* / …), unlike the old `apply:<id>`.
const applyBusChannelPrefix = "events"

// ApplyBusShardCount is the fixed number of shard channels. applyID maps
// deterministically to one of them via [ApplyBusShardIndex]. The value is a
// tradeoff: large enough that events from different runs rarely share a
// forward-loop (collision rate ≈ 1/K), and small enough that each Keeper
// instance holds a bounded number of Redis subscriptions regardless of how
// many Souls are in the deployment (the snapshot value was chosen by the
// architect).
const ApplyBusShardCount = 256

// ApplyBusShardIndex maps an applyID to a shard-channel index
// [0, ApplyBusShardCount). fnv32a is a fast, non-allocating hash;
// cryptographic strength isn't needed (this is load distribution, not
// security).
//
// Exported so that applybus keys its per-shard bridge-refs with the same
// index as the channel (a single source of shard resolution, no desync).
func ApplyBusShardIndex(applyID string) uint32 {
	h := fnv.New32a()
	// Hash.Write never errors by contract.
	_, _ = h.Write([]byte(applyID))
	return h.Sum32() % ApplyBusShardCount
}

// ApplyBusChannel builds the sharded Redis channel for an applyID.
// Deterministic: a given applyID always yields the same channel (see
// [ApplyBusShardIndex]).
func ApplyBusChannel(applyID string) string {
	return fmt.Sprintf("%s:shard:%d", applyBusChannelPrefix, ApplyBusShardIndex(applyID))
}

// applyEventEnvelope is the JSON wrapper for one PUBLISH message.
//
// `OriginKID` is the KID of the Keeper instance that published; the
// subscriber filters out messages where OriginKID == its own KID (prevents a
// duplicate of local-publish + Redis-echo on the same Keeper).
//
// `Payload` is `json.RawMessage`: the publisher (events_taskevent.go /
// events_runresult.go) already assembles a `map[string]any` and hands it to
// `applybus.Event.Payload` that way. To avoid `json.Marshal`-ing twice (once
// into the SSE frame, again into the envelope string), the payload is
// marshaled once on the publisher side and stored as bytes in RawMessage.
type applyEventEnvelope struct {
	OriginKID string          `json:"origin_kid"`
	Kind      string          `json:"kind"`
	ApplyID   string          `json:"apply_id"`
	At        time.Time       `json:"at"`
	Payload   json.RawMessage `json:"payload"`
}

// ApplyEvent is the decoded cluster-bus message delivered to a subscriber.
// Symmetric with `applybus.Event`, but Payload here is `json.RawMessage`:
// the cluster-bridge doesn't know the payload's typed structure, it just
// carries the bytes, and the SSE handler re-serializes them into a frame
// (see mcp/sse.go::writeSSEEvent).
type ApplyEvent struct {
	OriginKID string
	Kind      string
	ApplyID   string
	At        time.Time
	Payload   json.RawMessage
}

// PublishApplyEvent serializes the event and publishes it to the
// [ApplyBusChannel]. payload is an already-serialized JSON object (see
// doc-comment on [applyEventEnvelope]). Returns the number of subscribers
// that received the message.
//
// If at.IsZero, time.Now().UTC() is substituted (symmetric with
// `applybus.EventBus.Publish`).
func PublishApplyEvent(ctx context.Context, c *Client, applyID, originKID, kind string, at time.Time, payload json.RawMessage) (int64, error) {
	if c == nil {
		return 0, errors.New("redis.PublishApplyEvent: nil client")
	}
	if applyID == "" {
		return 0, errors.New("redis.PublishApplyEvent: empty applyID")
	}
	if originKID == "" {
		return 0, errors.New("redis.PublishApplyEvent: empty originKID")
	}
	if kind == "" {
		return 0, errors.New("redis.PublishApplyEvent: empty kind")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}

	env, err := json.Marshal(applyEventEnvelope{
		OriginKID: originKID,
		Kind:      kind,
		ApplyID:   applyID,
		At:        at,
		Payload:   payload,
	})
	if err != nil {
		return 0, fmt.Errorf("redis.PublishApplyEvent: envelope marshal: %w", err)
	}

	n, err := c.underlying().Publish(ctx, ApplyBusChannel(applyID), env).Result()
	if err != nil {
		return 0, fmt.Errorf("redis.PublishApplyEvent: PUBLISH %q: %w", ApplyBusChannel(applyID), err)
	}
	return n, nil
}

// applyEventSubBufferSize is the Go channel buffer between the
// Redis-PubSub loop and the caller (the applybus bridge). Symmetric with
// outboundSubBufferSize. 64 events covers a typical apply (10-30 tasks +
// start/final) plus burst headroom; a single shard channel can receive
// several applyIDs (collision ≈ 1/K), but the forward-loop drains into
// local subs synchronously and fast, so overflow is unlikely.
const applyEventSubBufferSize = 64

// ApplyEventSubscription is a handle on a subscription to a single shard
// channel `events:shard:<n>` (see [ApplyBusChannel]). Events for every
// applyID mapped to this shard flow through it; filtering by a specific
// applyID is done by the forward-loop in applybus.
//
// Identical lifecycle to [OutboundSubscription]: a spawned goroutine reads
// the PubSub channel → decodes the envelope → filters self-origin →
// delivers *ApplyEvent on Channel(). Close() stops the goroutine and closes
// the channel.
type ApplyEventSubscription struct {
	ps        *redis.PubSub
	out       chan *ApplyEvent
	selfKID   string
	applyID   string
	logger    *slog.Logger
	ready     chan struct{}
	stopped   chan struct{}
	closeOnce func() error
}

// Channel is the read side of the Go channel carrying decoded ApplyEvent
// messages.
func (s *ApplyEventSubscription) Channel() <-chan *ApplyEvent {
	if s == nil {
		return nil
	}
	return s.out
}

// Ready blocks until the first subscribe-acknowledgement from Redis (or
// ctx.Done() / Close()). See doc-comment on [OutboundSubscription.Ready].
func (s *ApplyEventSubscription) Ready(ctx context.Context) error {
	if s == nil {
		return errors.New("redis.ApplyEventSubscription.Ready: nil subscription")
	}
	select {
	case <-s.ready:
		return nil
	case <-s.stopped:
		return errors.New("redis.ApplyEventSubscription.Ready: subscription stopped before ready")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops the subscribe loop and closes the Redis pubsub handle and the
// Go channel. Idempotent.
func (s *ApplyEventSubscription) Close() error {
	if s == nil {
		return nil
	}
	return s.closeOnce()
}

// SubscribeApplyEvent subscribes to the shard channel [ApplyBusChannel]
// (applyID) and starts the forwarder goroutine. selfKID is used for
// self-filtering (as in [SubscribeOutbound]). applyID here only picks the
// shard and is used in logs; the channel carries events for every applyID
// in the same shard — filtering down to one specific applyID is the
// caller's job (the applybus forward-loop).
func SubscribeApplyEvent(ctx context.Context, c *Client, applyID, selfKID string, logger *slog.Logger) (*ApplyEventSubscription, error) {
	if c == nil {
		return nil, errors.New("redis.SubscribeApplyEvent: nil client")
	}
	if applyID == "" {
		return nil, errors.New("redis.SubscribeApplyEvent: empty applyID")
	}
	if selfKID == "" {
		return nil, errors.New("redis.SubscribeApplyEvent: empty selfKID")
	}
	if logger == nil {
		return nil, errors.New("redis.SubscribeApplyEvent: nil logger")
	}

	ps := c.underlying().Subscribe(ctx, ApplyBusChannel(applyID))

	s := &ApplyEventSubscription{
		ps:      ps,
		out:     make(chan *ApplyEvent, applyEventSubBufferSize),
		selfKID: selfKID,
		applyID: applyID,
		logger:  logger,
		ready:   make(chan struct{}),
		stopped: make(chan struct{}),
	}

	closed := make(chan struct{})
	s.closeOnce = func() error {
		select {
		case <-closed:
			return nil
		default:
		}
		close(closed)
		err := ps.Close()
		<-s.stopped
		return err
	}

	go s.run(ctx, closed)
	return s, nil
}

func (s *ApplyEventSubscription) run(ctx context.Context, closed <-chan struct{}) {
	defer close(s.out)
	defer close(s.stopped)

	if _, err := s.ps.Receive(ctx); err != nil {
		s.logger.Warn("redis.SubscribeApplyEvent: initial Receive failed",
			slog.String("apply_id", s.applyID), slog.Any("error", err))
		return
	}
	close(s.ready)

	for {
		select {
		case <-closed:
			return
		case <-ctx.Done():
			return
		default:
		}

		msg, err := s.ps.ReceiveMessage(ctx)
		if err != nil {
			select {
			case <-closed:
				return
			case <-ctx.Done():
				return
			default:
			}
			s.logger.Warn("redis.SubscribeApplyEvent: ReceiveMessage failed",
				slog.String("apply_id", s.applyID), slog.Any("error", err))
			return
		}

		ev, ok := s.decodeMessage(msg.Payload)
		if !ok {
			continue
		}
		if ev.OriginKID == s.selfKID {
			// Self-echo: we made this publish ourselves. Don't forward it
			// further — the local bus already delivered it to local subscribers.
			s.logger.Debug("redis.SubscribeApplyEvent: ignoring self-origin event",
				slog.String("apply_id", s.applyID),
				slog.String("origin_kid", ev.OriginKID))
			continue
		}

		s.forward(ev)
	}
}

// forward puts ev into s.out. On a full buffer it drops the OLDEST event
// (reads one element out and writes the new one into the freed slot),
// symmetric with applybus's local buffer (drop-oldest, see
// [applybus.EventBus.deliver]).
//
// Why keep-freshest rather than keep-oldest: on the cross-keeper path the
// freshest event is the run's terminal (`apply.completed`/`apply.failed`),
// and dropping it in favor of a stale "task_idx=0 OK" is worse for the SSE
// client. applybus itself isn't authoritative (PG is the source of truth,
// and the dispatcher-timer catches an undelivered terminal), but
// keep-freshest is the right default policy.
//
// The forward-loop is the sole writer into s.out, so the read+write here has
// no race for the slot: nothing else writes to the channel between selects.
func (s *ApplyEventSubscription) forward(ev *ApplyEvent) {
	select {
	case s.out <- ev:
		return
	default:
	}
	// Buffer full — free a slot by evicting the oldest.
	select {
	case <-s.out:
		s.logger.Warn("redis.SubscribeApplyEvent: forward channel full — dropped oldest event",
			slog.String("apply_id", s.applyID),
			slog.String("origin_kid", ev.OriginKID),
			slog.String("kind", ev.Kind))
	default:
		// Buffer was drained by the reader between selects — falling
		// through to write the new one below.
	}
	select {
	case s.out <- ev:
	default:
		// Unlikely (the reader hasn't drained the freed slot yet) — but we
		// don't block: the "forward-loop never stalls" guarantee matters
		// more than one event. Symmetric with applybus.deliver's final branch.
		s.logger.Warn("redis.SubscribeApplyEvent: forward channel still full after drop — event lost",
			slog.String("apply_id", s.applyID),
			slog.String("origin_kid", ev.OriginKID),
			slog.String("kind", ev.Kind))
	}
}

func (s *ApplyEventSubscription) decodeMessage(payload string) (*ApplyEvent, bool) {
	var env applyEventEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		s.logger.Warn("redis.SubscribeApplyEvent: envelope unmarshal failed",
			slog.String("apply_id", s.applyID), slog.Any("error", err))
		return nil, false
	}
	return &ApplyEvent{
		OriginKID: env.OriginKID,
		Kind:      env.Kind,
		ApplyID:   env.ApplyID,
		At:        env.At,
		Payload:   env.Payload,
	}, true
}
