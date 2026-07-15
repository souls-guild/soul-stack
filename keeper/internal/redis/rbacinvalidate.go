package redis

// Cluster-wide RBAC invalidation via Redis pub/sub (ADR-028(d), Phase 3 = B2).
//
// Problem: each Keeper instance holds its own RBAC snapshot
// (`rbac.Holder`/`rbac.Enforcer`), built from Postgres. A role mutation
// (CreateRole/DeleteRole/UpdateRolePermissions/GrantOperator/RevokeOperator)
// commits to the shared DB on Keeper-A, but the other nodes' snapshots only
// see it via TTL-poll (B1, up to `rbac.DefaultRefreshInterval`). The
// staleness window is seconds.
//
// Solution mirrors apply-events routing (see [PublishApplyEvent] /
// [SubscribeApplyEvent]): after a successful commit, the mutating node
// PUBLISHes to the `rbac:invalidate` channel, and the other nodes'
// SUBSCRIBE re-reads the snapshot from the DB (near-instant). Self-filter
// by `origin_kid` drops the echo of its own publish — the mutating node
// relies on its own TTL-poll (no need to refresh the snapshot right at
// publish time; the applybus pattern likewise drops self-origin).
//
// B2 = B1 + pub/sub: TTL-poll is NOT removed. Redis pub/sub has no
// persistence — if a message is lost (node reconnects, broker blips), the
// next TTL-poll picks it up. Pub/sub only shortens the typical delay to
// milliseconds, it doesn't replace the fallback.
//
// Convention `rbac:invalidate` — a separate namespace (PM-decision,
// confirmed topic), doesn't overlap with `apply:<id>`/`outbound:<sid>`/
// `soul:<sid>:*`. Wire format is a compact JSON envelope: only `origin_kid`
// (for self-filter) + `at` (diagnostics/log). No payload — the message is
// "snapshot is stale, re-read from the DB", the subscriber pulls specifics
// from Postgres itself.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// RBACInvalidateChannel is the Redis channel for cluster-wide RBAC
// invalidation. Fixed (not per-ID): any role mutation on any node publishes here.
const RBACInvalidateChannel = "rbac:invalidate"

// rbacInvalidateEnvelope is the JSON wrapper for one invalidate message.
//
// `OriginKID` is the KID of the Keeper instance that published the
// invalidation; the subscriber drops messages where OriginKID == its own
// KID (self-filter, mirrors [applyEventEnvelope.OriginKID]).
//
// `At` is the publish time (diagnostics); it carries no semantic weight for
// the snapshot rebuild.
type rbacInvalidateEnvelope struct {
	OriginKID string    `json:"origin_kid"`
	At        time.Time `json:"at"`
}

// RBACInvalidate is the unpacked invalidate message delivered to a
// subscriber. No payload: receiving it at all is the signal to "re-read the
// RBAC snapshot".
type RBACInvalidate struct {
	OriginKID string
	At        time.Time
}

// PublishRBACInvalidate publishes an invalidate signal to
// [RBACInvalidateChannel]. Returns the number of subscribers that received
// the message.
//
// Best-effort: the caller (rbac.Service after a commit) swallows the error
// — the mutation is already committed to the DB, and a lost publish isn't
// critical (TTL-poll will pick it up).
func PublishRBACInvalidate(ctx context.Context, c *Client, originKID string) (int64, error) {
	if c == nil {
		return 0, errors.New("redis.PublishRBACInvalidate: nil client")
	}
	if originKID == "" {
		return 0, errors.New("redis.PublishRBACInvalidate: empty originKID")
	}

	env, err := json.Marshal(rbacInvalidateEnvelope{
		OriginKID: originKID,
		At:        time.Now().UTC(),
	})
	if err != nil {
		return 0, fmt.Errorf("redis.PublishRBACInvalidate: envelope marshal: %w", err)
	}

	n, err := c.underlying().Publish(ctx, RBACInvalidateChannel, env).Result()
	if err != nil {
		return 0, fmt.Errorf("redis.PublishRBACInvalidate: PUBLISH %q: %w", RBACInvalidateChannel, err)
	}
	return n, nil
}

// rbacInvalidateSubBufferSize is the Go channel buffer between the
// Redis-PubSub loop and the caller (Holder). Invalidations are rare (role
// mutations — dozens per day), but the small margin avoids drops during
// bulk-grant bursts. Smaller than applyEventSubBufferSize — this stream is
// orders of magnitude rarer.
const rbacInvalidateSubBufferSize = 16

// RBACInvalidateSubscription is a handle to the [RBACInvalidateChannel]
// subscription.
//
// Lifecycle is identical to [ApplyEventSubscription]: a spawned goroutine
// reads the PubSub channel → parses the envelope → filters self-origin →
// delivers *RBACInvalidate to Channel(). Close() stops the goroutine and
// closes the channel.
type RBACInvalidateSubscription struct {
	ps        *redis.PubSub
	out       chan *RBACInvalidate
	selfKID   string
	logger    *slog.Logger
	ready     chan struct{}
	stopped   chan struct{}
	closeOnce func() error
}

// Channel is the read side of the Go channel with unpacked invalidate messages.
func (s *RBACInvalidateSubscription) Channel() <-chan *RBACInvalidate {
	if s == nil {
		return nil
	}
	return s.out
}

// Ready blocks until the first subscribe acknowledgement from Redis (or
// ctx.Done() / Close()). See the doc comment on [ApplyEventSubscription.Ready].
func (s *RBACInvalidateSubscription) Ready(ctx context.Context) error {
	if s == nil {
		return errors.New("redis.RBACInvalidateSubscription.Ready: nil subscription")
	}
	select {
	case <-s.ready:
		return nil
	case <-s.stopped:
		return errors.New("redis.RBACInvalidateSubscription.Ready: subscription stopped before ready")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops the subscribe loop and closes the Redis pubsub handle and Go
// channel. Idempotent.
func (s *RBACInvalidateSubscription) Close() error {
	if s == nil {
		return nil
	}
	return s.closeOnce()
}

// SubscribeRBACInvalidate subscribes to [RBACInvalidateChannel] and starts
// the goroutine-forwarder. selfKID is used for self-filtering (as in
// [SubscribeApplyEvent]).
func SubscribeRBACInvalidate(ctx context.Context, c *Client, selfKID string, logger *slog.Logger) (*RBACInvalidateSubscription, error) {
	if c == nil {
		return nil, errors.New("redis.SubscribeRBACInvalidate: nil client")
	}
	if selfKID == "" {
		return nil, errors.New("redis.SubscribeRBACInvalidate: empty selfKID")
	}
	if logger == nil {
		return nil, errors.New("redis.SubscribeRBACInvalidate: nil logger")
	}

	ps := c.underlying().Subscribe(ctx, RBACInvalidateChannel)

	s := &RBACInvalidateSubscription{
		ps:      ps,
		out:     make(chan *RBACInvalidate, rbacInvalidateSubBufferSize),
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

func (s *RBACInvalidateSubscription) run(ctx context.Context, closed <-chan struct{}) {
	defer close(s.out)
	defer close(s.stopped)

	if _, err := s.ps.Receive(ctx); err != nil {
		s.logger.Warn("redis.SubscribeRBACInvalidate: initial Receive failed",
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
			s.logger.Warn("redis.SubscribeRBACInvalidate: ReceiveMessage failed",
				slog.Any("error", err))
			return
		}

		ev, ok := s.decodeMessage(msg.Payload)
		if !ok {
			continue
		}
		if ev.OriginKID == s.selfKID {
			// Self-echo: we made this mutation ourselves. Don't refresh off
			// our own publish — our own TTL-poll will pick it up
			// (B1-fallback). Same self-filter as in SubscribeApplyEvent.
			s.logger.Debug("redis.SubscribeRBACInvalidate: ignoring self-origin invalidate",
				slog.String("origin_kid", ev.OriginKID))
			continue
		}

		select {
		case s.out <- ev:
		default:
			// Channel full — the subscriber (Holder) hasn't caught up on the
			// snapshot. Drop is safe: each message just means "re-read", the
			// next Refresh will read the full, current DB snapshot anyway.
			s.logger.Warn("redis.SubscribeRBACInvalidate: forward channel full, dropping",
				slog.String("origin_kid", ev.OriginKID))
		}
	}
}

func (s *RBACInvalidateSubscription) decodeMessage(payload string) (*RBACInvalidate, bool) {
	var env rbacInvalidateEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		s.logger.Warn("redis.SubscribeRBACInvalidate: envelope unmarshal failed",
			slog.Any("error", err))
		return nil, false
	}
	return &RBACInvalidate{
		OriginKID: env.OriginKID,
		At:        env.At,
	}, true
}
