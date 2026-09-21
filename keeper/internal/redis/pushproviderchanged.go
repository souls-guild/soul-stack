package redis

// Cluster-wide push-providers invalidation via Redis pub/sub (ADR-032
// amendment 2026-05-26, S7-2).
//
// Problem: SshDispatcher holds a spawned SshProvider plugin with an
// env-payload `SOUL_SSH_<UPPER_SNAKE(name)>_PARAMS`, populated at keeper
// startup from push_providers. An operator does POST/PUT/DELETE
// /v1/push-providers — the mutation commits to the shared table on Keeper-A,
// but Keeper-B keeps the plugin with the stale env-payload and only finds
// out on its next restart.
//
// Solution mirrors [PublishSigilInvalidate] / [SubscribeSigilInvalidate]:
// after a successful Create/Update/Delete commit, the mutating node
// PUBLISHes to the `push-providers:changed` channel; every node's SUBSCRIBE
// marks its SshProvider handle stale and re-spawns it on the next RPC
// (spawn-on-change, PM-decision S7-2 #6).
//
// Redis pub/sub has no persistence: a lost message (node reconnect, broker
// blip) means the re-spawn only happens on the next keeper restart or the
// next mutation. Acceptable: mutations are rare (operator-driven), staleness
// window is milliseconds in normal operation.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// PushProvidersChangedChannel is the Redis pub/sub topic. Matches
// pushprovider.TopicPushProvidersChanged (the constant is duplicated so the
// redis package doesn't import pushprovider — a direction invariant).
const PushProvidersChangedChannel = "push-providers:changed"

// pushProvidersChangedEnvelope is the JSON wrapper for one invalidate
// message. `Name` is the changed provider's name (empty string for a bulk
// invalidation); `At` is the publish time (diagnostics).
type pushProvidersChangedEnvelope struct {
	Name string    `json:"name,omitempty"`
	At   time.Time `json:"at"`
}

// PushProvidersChanged is the unpacked invalidate message for a subscriber.
type PushProvidersChanged struct {
	Name string
	At   time.Time
}

// PublishPushProvidersChanged publishes an invalidate signal.
// Returns the number of subscribers that received the message.
//
// Best-effort: the caller (pushprovider.Service after a CRUD commit)
// swallows the error — the mutation is already committed to the DB, and a
// lost publish is compensated by a lazy re-spawn on the next mutation/restart.
func PublishPushProvidersChanged(ctx context.Context, c *Client, providerName string) (int64, error) {
	if c == nil {
		return 0, errors.New("redis.PublishPushProvidersChanged: nil client")
	}
	env, err := json.Marshal(pushProvidersChangedEnvelope{
		Name: providerName,
		At:   time.Now().UTC(),
	})
	if err != nil {
		return 0, fmt.Errorf("redis.PublishPushProvidersChanged: envelope marshal: %w", err)
	}
	n, err := c.underlying().Publish(ctx, PushProvidersChangedChannel, env).Result()
	if err != nil {
		return 0, fmt.Errorf("redis.PublishPushProvidersChanged: PUBLISH %q: %w",
			PushProvidersChangedChannel, err)
	}
	return n, nil
}

// pushProvidersChangedSubBufferSize is the Go channel buffer between the
// PubSub loop and the caller. push-providers mutations are rare (at most
// dozens per day); the small margin avoids drops during bulk-operation
// bursts. Matches sigilInvalidateSubBufferSize.
const pushProvidersChangedSubBufferSize = 16

// PushProvidersChangedSubscription is a handle to the subscription.
// Lifecycle is identical to [SigilInvalidateSubscription].
type PushProvidersChangedSubscription struct {
	ps        *redis.PubSub
	out       chan *PushProvidersChanged
	logger    *slog.Logger
	ready     chan struct{}
	stopped   chan struct{}
	closeOnce func() error
}

// Channel is the read side of the Go channel with unpacked invalidate messages.
func (s *PushProvidersChangedSubscription) Channel() <-chan *PushProvidersChanged {
	if s == nil {
		return nil
	}
	return s.out
}

// Ready blocks until the first subscribe acknowledgement from Redis.
func (s *PushProvidersChangedSubscription) Ready(ctx context.Context) error {
	if s == nil {
		return errors.New("redis.PushProvidersChangedSubscription.Ready: nil subscription")
	}
	select {
	case <-s.ready:
		return nil
	case <-s.stopped:
		return errors.New("redis.PushProvidersChangedSubscription.Ready: subscription stopped before ready")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops the subscribe loop. Idempotent.
func (s *PushProvidersChangedSubscription) Close() error {
	if s == nil {
		return nil
	}
	return s.closeOnce()
}

// SubscribePushProvidersChanged subscribes to [PushProvidersChangedChannel].
func SubscribePushProvidersChanged(ctx context.Context, c *Client, logger *slog.Logger) (*PushProvidersChangedSubscription, error) {
	if c == nil {
		return nil, errors.New("redis.SubscribePushProvidersChanged: nil client")
	}
	if logger == nil {
		return nil, errors.New("redis.SubscribePushProvidersChanged: nil logger")
	}

	ps := c.underlying().Subscribe(ctx, PushProvidersChangedChannel)

	s := &PushProvidersChangedSubscription{
		ps:      ps,
		out:     make(chan *PushProvidersChanged, pushProvidersChangedSubBufferSize),
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

func (s *PushProvidersChangedSubscription) run(ctx context.Context, closed <-chan struct{}) {
	defer close(s.out)
	defer close(s.stopped)

	if _, err := s.ps.Receive(ctx); err != nil {
		s.logger.Warn("redis.SubscribePushProvidersChanged: initial Receive failed",
			slog.Any("error", err))
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
			s.logger.Warn("redis.SubscribePushProvidersChanged: ReceiveMessage failed",
				slog.Any("error", err))
			return
		}

		ev, ok := s.decodeMessage(msg.Payload)
		if !ok {
			continue
		}

		select {
		case s.out <- ev:
		default:
			// Channel full — the subscriber hasn't caught up. Drop is
			// safe: each message just means "re-spawn at next RPC", the
			// next one will re-read the full, current set of params.
			s.logger.Warn("redis.SubscribePushProvidersChanged: forward channel full, dropping")
		}
	}
}

func (s *PushProvidersChangedSubscription) decodeMessage(payload string) (*PushProvidersChanged, bool) {
	var env pushProvidersChangedEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		s.logger.Warn("redis.SubscribePushProvidersChanged: envelope unmarshal failed",
			slog.Any("error", err))
		return nil, false
	}
	return &PushProvidersChanged{Name: env.Name, At: env.At}, true
}
