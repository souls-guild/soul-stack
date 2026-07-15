package redis

// Cluster-wide invalidation of the Service registry and keeper-settings via
// Redis pub/sub (S2 of moving the registry to Postgres, the ADR-028(d) RBAC
// pattern).
//
// Problem: each Keeper instance holds its own in-memory snapshot of the
// service_registry + keeper_settings registry (`serviceregistry.Holder`),
// built from Postgres. A CRUD mutation (Insert/Update/Delete service /
// SetSetting) commits to the shared DB on Keeper-A, but the other nodes'
// snapshots only see it via TTL-poll (up to
// `serviceregistry.DefaultRefreshInterval`). The staleness window is
// seconds.
//
// Solution mirrors [PublishRBACInvalidate] / [SubscribeRBACInvalidate]:
// after a successful commit, the mutating node PUBLISHes to the
// `service:invalidate` channel, and the other nodes' SUBSCRIBE re-reads the
// snapshot from the DB near-instantly. Self-filter by `origin_kid` drops
// the echo of its own publish — the mutating node has already recorded the
// change in the DB and relies on its own TTL-poll (the same self-filter as
// RBAC; Sigil doesn't have one — its caches live on the Souls, here the
// snapshot is local to the Keeper).
//
// S2 = TTL-poll + pub/sub: TTL-poll is NOT removed. Redis pub/sub has no
// persistence — if a message is lost (node reconnect, broker blip), the
// next TTL-poll picks it up. Pub/sub only shortens the typical delay to
// milliseconds, it doesn't replace the fallback.
//
// Convention `service:invalidate` — a separate namespace (the
// `<subsystem>:<event>` style, like `rbac:invalidate`/`sigil:invalidate`),
// doesn't overlap with `apply:<id>`/`outbound:<sid>`/`soul:<sid>:*`. Wire
// format is a compact JSON envelope: `origin_kid` (self-filter) + `at`
// (diagnostics). No payload — the message is "snapshot is stale, re-read
// from the DB".

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// ServiceInvalidateChannel is the Redis channel for cluster-wide Service
// registry invalidation. Fixed (not per-name): any service/setting CRUD
// mutation on any node publishes here.
const ServiceInvalidateChannel = "service:invalidate"

// serviceInvalidateEnvelope is the JSON wrapper for one invalidate message.
//
// `OriginKID` is the KID of the Keeper instance that published the
// invalidation; the subscriber drops messages where OriginKID == its own
// KID (self-filter, mirrors [rbacInvalidateEnvelope.OriginKID]).
//
// `At` is the publish time (diagnostics); it carries no semantic weight for
// the snapshot rebuild.
type serviceInvalidateEnvelope struct {
	OriginKID string    `json:"origin_kid"`
	At        time.Time `json:"at"`
}

// ServiceInvalidate is the unpacked invalidate message delivered to a
// subscriber. No payload: receiving it at all is the signal to "re-read the
// registry snapshot".
type ServiceInvalidate struct {
	OriginKID string
	At        time.Time
}

// PublishServiceInvalidate publishes an invalidate signal to
// [ServiceInvalidateChannel]. Returns the number of subscribers that
// received the message.
//
// Best-effort: the caller (serviceregistry.Service after a commit) swallows
// the error — the mutation is already committed to the DB, and a lost
// publish isn't critical (TTL-poll will pick it up).
func PublishServiceInvalidate(ctx context.Context, c *Client, originKID string) (int64, error) {
	if c == nil {
		return 0, errors.New("redis.PublishServiceInvalidate: nil client")
	}
	if originKID == "" {
		return 0, errors.New("redis.PublishServiceInvalidate: empty originKID")
	}

	env, err := json.Marshal(serviceInvalidateEnvelope{
		OriginKID: originKID,
		At:        time.Now().UTC(),
	})
	if err != nil {
		return 0, fmt.Errorf("redis.PublishServiceInvalidate: envelope marshal: %w", err)
	}

	n, err := c.underlying().Publish(ctx, ServiceInvalidateChannel, env).Result()
	if err != nil {
		return 0, fmt.Errorf("redis.PublishServiceInvalidate: PUBLISH %q: %w", ServiceInvalidateChannel, err)
	}
	return n, nil
}

// serviceInvalidateSubBufferSize is the Go channel buffer between the
// Redis-PubSub loop and the caller (Holder). Invalidations are rare
// (registry CRUD mutations — dozens per day); the small margin avoids
// drops during bulk-import bursts. Matches rbacInvalidateSubBufferSize —
// the same class of rare administrative traffic.
const serviceInvalidateSubBufferSize = 16

// ServiceInvalidateSubscription is a handle to the [ServiceInvalidateChannel]
// subscription.
//
// Lifecycle is identical to [RBACInvalidateSubscription]: a spawned
// goroutine reads the PubSub channel → parses the envelope → filters
// self-origin → delivers *ServiceInvalidate to Channel(). Close() stops the
// goroutine and closes the channel.
type ServiceInvalidateSubscription struct {
	ps        *redis.PubSub
	out       chan *ServiceInvalidate
	selfKID   string
	logger    *slog.Logger
	ready     chan struct{}
	stopped   chan struct{}
	closeOnce func() error
}

// Channel is the read side of the Go channel with unpacked invalidate messages.
func (s *ServiceInvalidateSubscription) Channel() <-chan *ServiceInvalidate {
	if s == nil {
		return nil
	}
	return s.out
}

// Ready blocks until the first subscribe acknowledgement from Redis (or
// ctx.Done() / Close()). See the doc comment on [RBACInvalidateSubscription.Ready].
func (s *ServiceInvalidateSubscription) Ready(ctx context.Context) error {
	if s == nil {
		return errors.New("redis.ServiceInvalidateSubscription.Ready: nil subscription")
	}
	select {
	case <-s.ready:
		return nil
	case <-s.stopped:
		return errors.New("redis.ServiceInvalidateSubscription.Ready: subscription stopped before ready")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops the subscribe loop and closes the Redis pubsub handle and Go
// channel. Idempotent.
func (s *ServiceInvalidateSubscription) Close() error {
	if s == nil {
		return nil
	}
	return s.closeOnce()
}

// SubscribeServiceInvalidate subscribes to [ServiceInvalidateChannel] and
// starts the goroutine-forwarder. selfKID is used for self-filtering (as in
// [SubscribeRBACInvalidate]).
func SubscribeServiceInvalidate(ctx context.Context, c *Client, selfKID string, logger *slog.Logger) (*ServiceInvalidateSubscription, error) {
	if c == nil {
		return nil, errors.New("redis.SubscribeServiceInvalidate: nil client")
	}
	if selfKID == "" {
		return nil, errors.New("redis.SubscribeServiceInvalidate: empty selfKID")
	}
	if logger == nil {
		return nil, errors.New("redis.SubscribeServiceInvalidate: nil logger")
	}

	ps := c.underlying().Subscribe(ctx, ServiceInvalidateChannel)

	s := &ServiceInvalidateSubscription{
		ps:      ps,
		out:     make(chan *ServiceInvalidate, serviceInvalidateSubBufferSize),
		selfKID: selfKID,
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

func (s *ServiceInvalidateSubscription) run(ctx context.Context, closed <-chan struct{}) {
	defer close(s.out)
	defer close(s.stopped)

	if _, err := s.ps.Receive(ctx); err != nil {
		s.logger.Warn("redis.SubscribeServiceInvalidate: initial Receive failed",
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
			s.logger.Warn("redis.SubscribeServiceInvalidate: ReceiveMessage failed",
				slog.Any("error", err))
			return
		}

		ev, ok := s.decodeMessage(msg.Payload)
		if !ok {
			continue
		}
		if ev.OriginKID == s.selfKID {
			// Self-echo: we made this mutation ourselves. Don't refresh off
			// our own publish — our own TTL-poll will pick it up. Same
			// self-filter as in SubscribeRBACInvalidate.
			s.logger.Debug("redis.SubscribeServiceInvalidate: ignoring self-origin invalidate",
				slog.String("origin_kid", ev.OriginKID))
			continue
		}

		select {
		case s.out <- ev:
		default:
			// Channel full — the subscriber (Holder) hasn't caught up on
			// the snapshot. Drop is safe: each message just means
			// "re-read", the next rebuild will read the full, current DB
			// snapshot anyway.
			s.logger.Warn("redis.SubscribeServiceInvalidate: forward channel full, dropping",
				slog.String("origin_kid", ev.OriginKID))
		}
	}
}

func (s *ServiceInvalidateSubscription) decodeMessage(payload string) (*ServiceInvalidate, bool) {
	var env serviceInvalidateEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		s.logger.Warn("redis.SubscribeServiceInvalidate: envelope unmarshal failed",
			slog.Any("error", err))
		return nil, false
	}
	return &ServiceInvalidate{
		OriginKID: env.OriginKID,
		At:        env.At,
	}, true
}
