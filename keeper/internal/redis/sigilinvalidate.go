package redis

// Cluster-wide Sigil invalidation via Redis pub/sub (ADR-026, S6c).
//
// Problem: a Soul holds an in-memory cache of plugin grants (PluginSigil),
// populated by a connect-time broadcast from whichever Keeper instance its
// stream is connected to. An operator does plugin.allow / plugin.revoke —
// the mutation commits to the shared plugin_sigils table on Keeper-A, but a
// Soul may be sitting on Keeper-B and never learn about the new allow-list:
// its cache stays stale (a revoked grant is still "trusted", a new allow
// hasn't arrived yet).
//
// Solution mirrors [PublishRBACInvalidate] / [SubscribeRBACInvalidate]:
// after a successful Allow/Revoke commit, the mutating node PUBLISHes to
// the `sigil:invalidate` channel, and EVERY node's SUBSCRIBE re-reads the
// active set from the DB and re-broadcasts it to its connected Souls
// (through the same path as the connect-time broadcast). This way a Soul on
// Keeper-B gets the fresh set after a mutation on Keeper-A.
//
// Difference from RBAC: self-origin is NOT filtered here. The mutating
// node's RBAC snapshot is updated by its own in-process commit, so it
// doesn't need to react to its own publish. Sigil caches live on the Souls,
// not on the Keeper; the mutating node must re-broadcast to its own Souls
// exactly like any other node. There's no separate in-process re-broadcast
// after the commit — a single path through pub/sub is simpler and avoids a
// double dispatch: the node receives its own message via the subscription
// and dispatches it. That's why the envelope carries only `at`
// (diagnostics), no origin_kid.
//
// Redis pub/sub has no persistence: a lost message (node reconnect, broker
// blip) means the grant arrives on the Soul's next reconnect (connect-time
// broadcast). Pub/sub only shortens the staleness window to milliseconds,
// it doesn't replace the connect-time fallback.
//
// Convention `sigil:invalidate` — a separate namespace (the
// `<subsystem>:<event>` style, like `rbac:invalidate`), doesn't overlap
// with `apply:<id>`/`outbound:<sid>`/`soul:<sid>:*`.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// SigilInvalidateChannel is the Redis channel for cluster-wide Sigil
// invalidation. Fixed (not per-SID): any allow/revoke mutation on any node
// publishes here, and every node re-broadcasts the active set to all its
// Souls.
const SigilInvalidateChannel = "sigil:invalidate"

// sigilInvalidateEnvelope is the JSON wrapper for one invalidate message.
// No payload: receiving it at all is the signal "allow-list changed,
// re-broadcast the active set to your Souls". `At` is the publish time
// (diagnostics/log).
type sigilInvalidateEnvelope struct {
	At time.Time `json:"at"`
}

// SigilInvalidate is the unpacked invalidate message delivered to a
// subscriber. No payload: receiving it is the re-broadcast signal.
type SigilInvalidate struct {
	At time.Time
}

// PublishSigilInvalidate publishes an invalidate signal to
// [SigilInvalidateChannel]. Returns the number of subscribers that
// received the message.
//
// Best-effort: the caller (sigil.Service after an Allow/Revoke commit)
// swallows the error — the mutation is already committed to the DB, and a
// lost publish is compensated by the connect-time broadcast on the Soul's
// next reconnect.
func PublishSigilInvalidate(ctx context.Context, c *Client) (int64, error) {
	if c == nil {
		return 0, errors.New("redis.PublishSigilInvalidate: nil client")
	}

	env, err := json.Marshal(sigilInvalidateEnvelope{At: time.Now().UTC()})
	if err != nil {
		return 0, fmt.Errorf("redis.PublishSigilInvalidate: envelope marshal: %w", err)
	}

	n, err := c.underlying().Publish(ctx, SigilInvalidateChannel, env).Result()
	if err != nil {
		return 0, fmt.Errorf("redis.PublishSigilInvalidate: PUBLISH %q: %w", SigilInvalidateChannel, err)
	}
	return n, nil
}

// sigilInvalidateSubBufferSize is the Go channel buffer between the
// Redis-PubSub loop and the caller. Allow/revoke mutations are rare
// (dozens per day); the small margin avoids drops during bulk-allow
// bursts. Matches rbacInvalidateSubBufferSize — the same class of rare
// administrative traffic.
const sigilInvalidateSubBufferSize = 16

// SigilInvalidateSubscription is a handle to the [SigilInvalidateChannel]
// subscription. Lifecycle is identical to [RBACInvalidateSubscription]: a
// spawned goroutine reads the PubSub channel → parses the envelope →
// delivers *SigilInvalidate to Channel(). Close() stops the goroutine and
// closes the channel. Idempotent.
type SigilInvalidateSubscription struct {
	ps        *redis.PubSub
	out       chan *SigilInvalidate
	logger    *slog.Logger
	ready     chan struct{}
	stopped   chan struct{}
	closeOnce func() error
}

// Channel is the read side of the Go channel with unpacked invalidate messages.
func (s *SigilInvalidateSubscription) Channel() <-chan *SigilInvalidate {
	if s == nil {
		return nil
	}
	return s.out
}

// Ready blocks until the first subscribe acknowledgement from Redis (or
// ctx.Done() / Close()). See the doc comment on [RBACInvalidateSubscription.Ready].
func (s *SigilInvalidateSubscription) Ready(ctx context.Context) error {
	if s == nil {
		return errors.New("redis.SigilInvalidateSubscription.Ready: nil subscription")
	}
	select {
	case <-s.ready:
		return nil
	case <-s.stopped:
		return errors.New("redis.SigilInvalidateSubscription.Ready: subscription stopped before ready")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops the subscribe loop and closes the Redis pubsub handle and Go
// channel. Idempotent.
func (s *SigilInvalidateSubscription) Close() error {
	if s == nil {
		return nil
	}
	return s.closeOnce()
}

// SubscribeSigilInvalidate subscribes to [SigilInvalidateChannel] and
// starts the goroutine-forwarder.
func SubscribeSigilInvalidate(ctx context.Context, c *Client, logger *slog.Logger) (*SigilInvalidateSubscription, error) {
	if c == nil {
		return nil, errors.New("redis.SubscribeSigilInvalidate: nil client")
	}
	if logger == nil {
		return nil, errors.New("redis.SubscribeSigilInvalidate: nil logger")
	}

	ps := c.underlying().Subscribe(ctx, SigilInvalidateChannel)

	s := &SigilInvalidateSubscription{
		ps:      ps,
		out:     make(chan *SigilInvalidate, sigilInvalidateSubBufferSize),
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

func (s *SigilInvalidateSubscription) run(ctx context.Context, closed <-chan struct{}) {
	defer close(s.out)
	defer close(s.stopped)

	if _, err := s.ps.Receive(ctx); err != nil {
		s.logger.Warn("redis.SubscribeSigilInvalidate: initial Receive failed",
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
			s.logger.Warn("redis.SubscribeSigilInvalidate: ReceiveMessage failed",
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
			// Channel full — the subscriber hasn't re-broadcast yet. Drop
			// is safe: each message just means "re-broadcast the active
			// set", the next re-broadcast will read the full, current DB
			// set anyway.
			s.logger.Warn("redis.SubscribeSigilInvalidate: forward channel full, dropping")
		}
	}
}

func (s *SigilInvalidateSubscription) decodeMessage(payload string) (*SigilInvalidate, bool) {
	var env sigilInvalidateEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		s.logger.Warn("redis.SubscribeSigilInvalidate: envelope unmarshal failed",
			slog.Any("error", err))
		return nil, false
	}
	return &SigilInvalidate{At: env.At}, true
}
