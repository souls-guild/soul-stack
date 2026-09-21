package redis

// Cluster-wide invalidation of the dispatcher's Tiding-rule snapshot via Redis
// pub/sub (ADR-052, S4).
//
// Problem: the notification dispatcher keeps a TTL cache of ENABLED Tiding
// rules (DefaultRuleCacheTTL=15s). An operator does a Herald/Tiding CRUD — the
// mutation commits to the shared table on Keeper-A and resets the cache there
// in-process, but Keeper-B keeps a stale snapshot until the TTL expires (a new
// rule won't fire / a deleted one keeps sending for ≤15s).
//
// Solution, symmetric to [PublishPushProvidersChanged] / [PublishSigilInvalidate]:
// after a successful CRUD commit, the mutating node PUBLISHes to the
// `herald:invalidate` channel; every node's SUBSCRIBE handler calls its own
// Dispatcher.InvalidateRules (the next match re-reads the enabled snapshot from
// PG).
//
// Redis pub/sub has no persistence: a lost message (node reconnect, broker
// blip) → convergence happens via the TTL poll (DefaultRuleCacheTTL).
// Acceptable: CRUD mutations are rare (operator-driven), staleness window ≤15s.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// HeraldInvalidateChannel is the Redis pub/sub topic. Matches the channel's
// technical name in naming-rules (`herald:invalidate`, `<subsystem>:<event>`
// style like `rbac:invalidate` / `push-providers:changed`).
const HeraldInvalidateChannel = "herald:invalidate"

// heraldInvalidateEnvelope is the JSON wrapper for one invalidate message.
// `Name` is the name of the changed Herald/Tiding (diagnostics only —
// invalidation is always full, the whole snapshot is re-read, the name doesn't
// target the reset); `At` is the publish time.
type heraldInvalidateEnvelope struct {
	Name string    `json:"name,omitempty"`
	At   time.Time `json:"at"`
}

// HeraldInvalidate is the unpacked invalidate message for a subscriber.
type HeraldInvalidate struct {
	Name string
	At   time.Time
}

// PublishHeraldInvalidate publishes the invalidate signal. Returns the number
// of subscribers that received the message.
//
// Best-effort: the caller (herald.Service after a CRUD operation commits)
// swallows the error — the mutation is already committed to the DB, a lost
// publish is compensated by the dispatcher cache's TTL convergence
// (DefaultRuleCacheTTL).
func PublishHeraldInvalidate(ctx context.Context, c *Client, name string) (int64, error) {
	if c == nil {
		return 0, errors.New("redis.PublishHeraldInvalidate: nil client")
	}
	env, err := json.Marshal(heraldInvalidateEnvelope{Name: name, At: time.Now().UTC()})
	if err != nil {
		return 0, fmt.Errorf("redis.PublishHeraldInvalidate: envelope marshal: %w", err)
	}
	n, err := c.underlying().Publish(ctx, HeraldInvalidateChannel, env).Result()
	if err != nil {
		return 0, fmt.Errorf("redis.PublishHeraldInvalidate: PUBLISH %q: %w", HeraldInvalidateChannel, err)
	}
	return n, nil
}

// heraldInvalidateSubBufferSize is the Go channel buffer between the PubSub
// loop and the caller. Herald/Tiding CRUD mutations are rare; a small margin
// avoids drops on a burst. Matches pushProvidersChangedSubBufferSize.
const heraldInvalidateSubBufferSize = 16

// HeraldInvalidateSubscription is a handle to the subscription. Lifecycle is
// identical to [PushProvidersChangedSubscription].
type HeraldInvalidateSubscription struct {
	ps        *redis.PubSub
	out       chan *HeraldInvalidate
	logger    *slog.Logger
	ready     chan struct{}
	stopped   chan struct{}
	closeOnce func() error
}

// Channel is the read side of the Go channel with unpacked invalidate messages.
func (s *HeraldInvalidateSubscription) Channel() <-chan *HeraldInvalidate {
	if s == nil {
		return nil
	}
	return s.out
}

// Ready blocks until the first subscribe acknowledgement from Redis.
func (s *HeraldInvalidateSubscription) Ready(ctx context.Context) error {
	if s == nil {
		return errors.New("redis.HeraldInvalidateSubscription.Ready: nil subscription")
	}
	select {
	case <-s.ready:
		return nil
	case <-s.stopped:
		return errors.New("redis.HeraldInvalidateSubscription.Ready: subscription stopped before ready")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops the subscribe loop. Idempotent.
func (s *HeraldInvalidateSubscription) Close() error {
	if s == nil {
		return nil
	}
	return s.closeOnce()
}

// SubscribeHeraldInvalidate subscribes to [HeraldInvalidateChannel].
func SubscribeHeraldInvalidate(ctx context.Context, c *Client, logger *slog.Logger) (*HeraldInvalidateSubscription, error) {
	if c == nil {
		return nil, errors.New("redis.SubscribeHeraldInvalidate: nil client")
	}
	if logger == nil {
		return nil, errors.New("redis.SubscribeHeraldInvalidate: nil logger")
	}

	ps := c.underlying().Subscribe(ctx, HeraldInvalidateChannel)

	s := &HeraldInvalidateSubscription{
		ps:      ps,
		out:     make(chan *HeraldInvalidate, heraldInvalidateSubBufferSize),
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

func (s *HeraldInvalidateSubscription) run(ctx context.Context, closed <-chan struct{}) {
	defer close(s.out)
	defer close(s.stopped)

	if _, err := s.ps.Receive(ctx); err != nil {
		s.logger.Warn("redis.SubscribeHeraldInvalidate: initial Receive failed", slog.Any("error", err))
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
			s.logger.Warn("redis.SubscribeHeraldInvalidate: ReceiveMessage failed", slog.Any("error", err))
			return
		}

		ev, ok := s.decodeMessage(msg.Payload)
		if !ok {
			continue
		}

		select {
		case s.out <- ev:
		default:
			// Channel is full — the subscriber didn't catch up. Dropping is safe:
			// each message is just "InvalidateRules"; the next one will re-read
			// the current snapshot in full.
			s.logger.Warn("redis.SubscribeHeraldInvalidate: forward channel full, dropping")
		}
	}
}

func (s *HeraldInvalidateSubscription) decodeMessage(payload string) (*HeraldInvalidate, bool) {
	var env heraldInvalidateEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		s.logger.Warn("redis.SubscribeHeraldInvalidate: envelope unmarshal failed", slog.Any("error", err))
		return nil, false
	}
	return &HeraldInvalidate{Name: env.Name, At: env.At}, true
}
