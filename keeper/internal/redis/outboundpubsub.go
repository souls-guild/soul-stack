package redis

// Cluster-mode outbound routing via Redis pub/sub (ADR-002, HA).
//
// Problem: StreamManager in keeper/internal/grpc is a per-Keeper-instance
// registry. If Outbound.SendApply is called on Keeper-A for a SID whose
// EventStream is held by Keeper-B, the local Lookup is empty and without
// pub/sub it returns ErrSoulNotConnected.
//
// Solution: Keeper-A checks the SoulLease (`soul:<sid>:lock`), learns
// holder=Keeper-B, and publishes a FromKeeper message to the
// `outbound:<sid>` channel. Keeper-B, when registering the stream,
// subscribes to this channel and forwards incoming messages to the local
// outbound channel.
//
// The `outbound:<sid>` convention mirrors the heartbeat cache
// (`soul:<sid>:hb`) and the lease (`soul:<sid>:lock`).
//
// Wire format is protojson (PM decision: text-friendly, debuggable via
// `redis-cli SUBSCRIBE`, forward-compat on proto fields). On top of it — a
// thin JSON envelope with an `origin_kid` field, so a subscriber can filter
// out its own publications (a race window: the holder changed between
// publish and delivery — the Keeper that became the new holder could
// otherwise receive an echo of its own send).
//
// Semantics: fire-and-forget. Redis pub/sub has no TTL/persistence — if the
// subscriber is disconnected at PUBLISH time, the message is lost. This is
// acceptable for apply commands (the caller will see the result via
// RunResult; missing responses are covered by timeouts at the scenario
// level).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/encoding/protojson"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// OutboundChannelKey builds the Redis outbound-routing channel for a SID.
//
// `outbound:<sid>` is a separate namespace, not overlapping the
// heartbeat cache (`soul:<sid>:hb`) or the lease key
// (`soul:<sid>:lock`); the channel is pub/sub-only, a separate keyspace.
func OutboundChannelKey(sid string) string {
	return "outbound:" + sid
}

// outboundEnvelope is the JSON wrapper for one PUBLISH message.
//
// `OriginKID` is the KID of the Keeper instance that published;
// a subscriber ignores messages where OriginKID == its own KID (see
// [OutboundSubscription.run]).
//
// `Payload` is a protojson-serialized `FromKeeper`. Stored as
// json.RawMessage to avoid double encoding (protojson → byte
// → json string → byte): we drop the raw protojson straight into the
// envelope.
type outboundEnvelope struct {
	OriginKID string          `json:"origin_kid"`
	Payload   json.RawMessage `json:"payload"`
}

// PublishOutbound serializes `FromKeeper` via protojson, wraps it in the
// outbound envelope with OriginKID = the publisher's kid, and publishes to
// the [OutboundChannelKey] channel.
//
// Returns the number of subscribers that received the message (the Redis
// PUBLISH result). The caller (Outbound.SendApply/Cancel/...) interprets 0
// as "no one was subscribed at publish time — the Soul may be reconnecting,
// or the message was lost." In MVP semantics we do NOT auto-retry: for apply
// commands this is correct (even a retransmitted command could collide with
// the same apply_id); for cancel, the Soul's ProteinApplyRunner will read the
// run through to completion anyway.
func PublishOutbound(ctx context.Context, c *Client, sid, originKID string, msg *keeperv1.FromKeeper) (int64, error) {
	if c == nil {
		return 0, errors.New("redis.PublishOutbound: nil client")
	}
	if sid == "" {
		return 0, errors.New("redis.PublishOutbound: empty sid")
	}
	if originKID == "" {
		return 0, errors.New("redis.PublishOutbound: empty originKID")
	}
	if msg == nil {
		return 0, errors.New("redis.PublishOutbound: nil msg")
	}

	payload, err := protojson.Marshal(msg)
	if err != nil {
		return 0, fmt.Errorf("redis.PublishOutbound: protojson.Marshal: %w", err)
	}
	env, err := json.Marshal(outboundEnvelope{OriginKID: originKID, Payload: payload})
	if err != nil {
		return 0, fmt.Errorf("redis.PublishOutbound: envelope marshal: %w", err)
	}

	n, err := c.underlying().Publish(ctx, OutboundChannelKey(sid), env).Result()
	if err != nil {
		return 0, fmt.Errorf("redis.PublishOutbound: PUBLISH %q: %w", OutboundChannelKey(sid), err)
	}
	return n, nil
}

// OutboundSubscription is a handle to an active `outbound:<sid>` subscription.
//
// Created via [SubscribeOutbound]; a goroutine reads the Redis PubSub
// channel, parses the envelope, filters by origin_kid (drops echoes of its
// own publications), and puts *FromKeeper on the Go channel
// [OutboundSubscription.Channel]. Close() closes the Redis pubsub and the
// Go channel.
//
// Subscribe-loop lifecycle: spawned under Register(sid) in StreamManager,
// Close() on Unregister. Not thread-safe between Close() and reading from
// Channel() — the caller must stop reading before Close (typically —
// handler-defer LIFO: Unregister → Close first, then wait for the
// receive loop).
type OutboundSubscription struct {
	ps        *redis.PubSub
	out       chan *keeperv1.FromKeeper
	selfKID   string
	sid       string
	logger    *slog.Logger
	ready     chan struct{}
	stopped   chan struct{}
	closeOnce func() error
}

// Channel is the read side of the Go channel with unpacked FromKeeper messages.
//
// Closes on [OutboundSubscription.Close] or on a fatal Redis error
// (connection loss — go-redis retries reconnecting on its own, but
// ReceiveMessage can return an error after several failures). On close,
// the caller's forward loop terminates.
func (s *OutboundSubscription) Channel() <-chan *keeperv1.FromKeeper {
	if s == nil {
		return nil
	}
	return s.out
}

// Ready blocks until the first `subscribe` acknowledgement from Redis
// (or ctx.Done() / Close()). Guarantees that once it returns, the channel
// is actually registered on the Redis side; without this,
// PublishOutbound called right after SubscribeOutbound could miss (the
// subscribe hadn't reached the broker yet).
//
// Used by tests; production code calls it once after
// [SubscribeOutbound], to guarantee ordering between Register and the
// first possible publication.
func (s *OutboundSubscription) Ready(ctx context.Context) error {
	if s == nil {
		return errors.New("redis.OutboundSubscription.Ready: nil subscription")
	}
	select {
	case <-s.ready:
		return nil
	case <-s.stopped:
		return errors.New("redis.OutboundSubscription.Ready: subscription stopped before ready")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops the subscribe loop, closes the Redis pubsub handle and the
// Go channel. Idempotent — a repeat Close is a no-op.
func (s *OutboundSubscription) Close() error {
	if s == nil {
		return nil
	}
	return s.closeOnce()
}

// SubscribeOutbound subscribes to `outbound:<sid>` and starts the
// forwarder goroutine. selfKID is used for self-filtering (see the type
// comment on [OutboundSubscription]).
//
// The goroutine terminates on:
//   - the caller's Close() (Unregister in StreamManager);
//   - ctx.Done() — external cancellation;
//   - a fatal ReceiveMessage error (Redis unreachable for a long time).
//
// On any non-fatal envelope parse error we log a warning and continue
// (a corrupt message is a serializer bug, not a reason to bring down the
// whole channel).
func SubscribeOutbound(ctx context.Context, c *Client, sid, selfKID string, logger *slog.Logger) (*OutboundSubscription, error) {
	if c == nil {
		return nil, errors.New("redis.SubscribeOutbound: nil client")
	}
	if sid == "" {
		return nil, errors.New("redis.SubscribeOutbound: empty sid")
	}
	if selfKID == "" {
		return nil, errors.New("redis.SubscribeOutbound: empty selfKID")
	}
	if logger == nil {
		return nil, errors.New("redis.SubscribeOutbound: nil logger")
	}

	ps := c.underlying().Subscribe(ctx, OutboundChannelKey(sid))

	s := &OutboundSubscription{
		ps:      ps,
		out:     make(chan *keeperv1.FromKeeper, outboundSubBufferSize),
		selfKID: selfKID,
		sid:     sid,
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
		// ps.Close interrupts ReceiveMessage in the forward loop (it returns an
		// error); the loop terminates, closing the out channel and s.stopped.
		err := ps.Close()
		<-s.stopped
		return err
	}

	go s.run(ctx, closed)
	return s, nil
}

// outboundSubBufferSize is the Go channel buffer between the Redis PubSub
// loop and the caller (the StreamManager forward goroutine). Mirrors
// outboundBufferSize in the grpc package: one Soul sees infrequent
// FromKeeper messages, 10 elements cover a burst.
const outboundSubBufferSize = 10

func (s *OutboundSubscription) run(ctx context.Context, closed <-chan struct{}) {
	defer close(s.out)
	defer close(s.stopped)

	// Wait for the subscribe acknowledgement, then signal Ready. On a
	// fatal Receive error we return (closeOnce below won't double-close).
	if _, err := s.ps.Receive(ctx); err != nil {
		s.logger.Warn("redis.SubscribeOutbound: initial Receive failed",
			slog.String("sid", s.sid), slog.Any("error", err))
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
				// Expected termination via Close().
				return
			case <-ctx.Done():
				return
			default:
			}
			s.logger.Warn("redis.SubscribeOutbound: ReceiveMessage failed",
				slog.String("sid", s.sid), slog.Any("error", err))
			return
		}

		fromKeeper, originKID, ok := s.decodeMessage(msg.Payload)
		if !ok {
			continue
		}
		if originKID == s.selfKID {
			// Self-echo: we made this publication ourselves (a race window
			// during a lease-holder change — we became the holder after the
			// publish but before the subscribe). Ignore it to avoid looping.
			s.logger.Debug("redis.SubscribeOutbound: ignoring self-origin message",
				slog.String("sid", s.sid), slog.String("origin_kid", originKID))
			continue
		}

		select {
		case s.out <- fromKeeper:
		default:
			// Forward channel is full — the local stream receiver isn't keeping
			// up. Drop+log: there's no delivery guarantee at the pub/sub level
			// anyway; dropping at the forward boundary mirrors the drop in
			// Outbound.send.
			s.logger.Warn("redis.SubscribeOutbound: forward channel full, dropping",
				slog.String("sid", s.sid), slog.String("origin_kid", originKID))
		}
	}
}

func (s *OutboundSubscription) decodeMessage(payload string) (*keeperv1.FromKeeper, string, bool) {
	var env outboundEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		s.logger.Warn("redis.SubscribeOutbound: envelope unmarshal failed",
			slog.String("sid", s.sid), slog.Any("error", err))
		return nil, "", false
	}
	if len(env.Payload) == 0 {
		s.logger.Warn("redis.SubscribeOutbound: empty payload in envelope",
			slog.String("sid", s.sid))
		return nil, env.OriginKID, false
	}
	out := &keeperv1.FromKeeper{}
	if err := protojson.Unmarshal(env.Payload, out); err != nil {
		s.logger.Warn("redis.SubscribeOutbound: protojson.Unmarshal failed",
			slog.String("sid", s.sid), slog.Any("error", err))
		return nil, env.OriginKID, false
	}
	return out, env.OriginKID, true
}

// ReadSoulLeaseHolder returns the current kid-holder of the SID's lease, or
// "" if the key doesn't exist. Used by Outbound for cluster-mode
// routing: holder != self → publish to the outbound channel; holder ==
// self with no local stream → a lease-inconsistency error; holder ==
// "" → ErrSoulNotConnected.
//
// This is a plain GET on the lease key (SoulLeaseKey) — not a lease
// acquire, not a modify. No race protection on top: the holder can
// change between the read and the subsequent Publish, and that's fine
// (worst case — the publication goes out with no subscriber and is
// lost, preserving the "fire-and-forget" semantics).
func ReadSoulLeaseHolder(ctx context.Context, c *Client, sid string) (string, error) {
	if c == nil {
		return "", errors.New("redis.ReadSoulLeaseHolder: nil client")
	}
	if sid == "" {
		return "", errors.New("redis.ReadSoulLeaseHolder: empty sid")
	}
	v, err := c.underlying().Get(ctx, SoulLeaseKey(sid)).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("redis.ReadSoulLeaseHolder: GET %q: %w", SoulLeaseKey(sid), err)
	}
	return v, nil
}
