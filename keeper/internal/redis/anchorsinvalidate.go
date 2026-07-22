package redis

// Cluster-wide invalidation of the Sigil signing trust-anchor set via Redis
// pub/sub (ADR-026(h), R3-S6).
//
// Problem: the trust-anchor set (sigil_signing_keys) changes on signing-key
// rotation (Introduce / SetPrimary / Retire, S7). The mutation commits to the
// shared table on Keeper-A, but EVERY node keeps its own in-memory snapshot of
// the set (sigil.Signer + keeper-host AnchorSet) and hands it to its connected
// Souls via connect-time broadcast. After a rotation, nodes must: (1) reload
// the set from the DB, (2) rebuild the Signer (new primary + anchors), (3)
// update the keeper-host verify-set, (4) re-broadcast the fresh set to their
// Souls — otherwise a Soul on another node verifies signatures against a
// stale anchor set (a new primary still "untrusted"; a retired anchor still
// "trusted").
//
// Solution is symmetric to [PublishSigilInvalidate] / [SubscribeSigilInvalidate]:
// after a successful rotation commit, the mutating node PUBLISHes to the
// `sigil:anchors-changed` channel; EVERY node reloads its Signer/set on
// SUBSCRIBE and re-broadcasts SigilTrustAnchors to its Souls.
//
// Self-origin is NOT filtered (like sigil:invalidate, unlike rbac:invalidate).
// The set lives on Souls and in the keeper-host of EVERY node; the mutating
// node must reload and re-broadcast exactly like any other node (its own
// in-memory Signer still holds the old set until reload). A single pub/sub
// path without a separate in-process reload after commit is simpler and
// avoids double handling: the node just receives its own message via the
// subscription and processes it. Hence the envelope carries only `at`
// (diagnostics), no origin_kid.
//
// Redis pub/sub has no persistence: a lost message (node reconnect, broker
// blip) leaves a node with a stale set until its next restart (load-at-start
// in setupSigil). This window is accepted — connect-time broadcast and
// fail-closed verify on the Soul side protect integrity; pub/sub only shrinks
// the anchor-set desync window to milliseconds.
//
// Convention `sigil:anchors-changed` — a separate namespace (`<subsystem>:<event>`
// style, like `sigil:invalidate`), does not overlap with
// `apply:<id>`/`outbound:<sid>`/`soul:<sid>:*`.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// AnchorsChangedChannel is the Redis channel for cluster-wide invalidation of
// the Sigil signing trust-anchor set. Fixed (not per-key): any signing-key
// rotation on any node publishes here; every node reloads the set and
// re-broadcasts it to its Souls.
const AnchorsChangedChannel = "sigil:anchors-changed"

// anchorsChangedEnvelope is the JSON wrapper for one anchors-changed message.
// No payload: receipt itself is the signal "anchor set changed, reload it and
// re-broadcast to your Souls". `At` is the publish time (diagnostics/logging).
type anchorsChangedEnvelope struct {
	At time.Time `json:"at"`
}

// AnchorsChanged is the decoded anchors-changed message delivered to the
// subscriber. No payload: receipt is the reload + re-broadcast signal.
type AnchorsChanged struct {
	At time.Time
}

// PublishAnchorsChanged publishes an anchors-changed signal to
// [AnchorsChangedChannel]. Returns the number of subscribers that received it.
//
// Best-effort: the caller (rotation handler S7, after an Introduce/SetPrimary/
// Retire commit) swallows the error — the mutation is already committed to the
// DB, and a lost publish just leaves nodes with the stale set until restart
// (an accepted window, see the file doc-comment).
func PublishAnchorsChanged(ctx context.Context, c *Client) (int64, error) {
	if c == nil {
		return 0, errors.New("redis.PublishAnchorsChanged: nil client")
	}

	env, err := json.Marshal(anchorsChangedEnvelope{At: time.Now().UTC()})
	if err != nil {
		return 0, fmt.Errorf("redis.PublishAnchorsChanged: envelope marshal: %w", err)
	}

	n, err := c.underlying().Publish(ctx, AnchorsChangedChannel, env).Result()
	if err != nil {
		return 0, fmt.Errorf("redis.PublishAnchorsChanged: PUBLISH %q: %w", AnchorsChangedChannel, err)
	}
	return n, nil
}

// anchorsChangedSubBufferSize is the Go channel buffer between the
// Redis-PubSub loop and the caller. Signing-key rotations are rare (a handful
// over a cluster's lifetime); a small buffer avoids drops on bursts (e.g.
// Introduce+SetPrimary back to back). Matches sigilInvalidateSubBufferSize —
// same class of rare administrative traffic.
const anchorsChangedSubBufferSize = 16

// AnchorsChangedSubscription is a handle on a subscription to
// [AnchorsChangedChannel]. Lifecycle is identical to
// [SigilInvalidateSubscription]: a spawned goroutine reads the PubSub channel
// → decodes the envelope → delivers *AnchorsChanged on Channel(). Close()
// stops the goroutine and closes the channel. Idempotent.
type AnchorsChangedSubscription struct {
	ps        *redis.PubSub
	out       chan *AnchorsChanged
	logger    *slog.Logger
	ready     chan struct{}
	stopped   chan struct{}
	closeOnce func() error
}

// Channel is the read side of the Go channel carrying decoded
// anchors-changed messages.
func (s *AnchorsChangedSubscription) Channel() <-chan *AnchorsChanged {
	if s == nil {
		return nil
	}
	return s.out
}

// Ready blocks until the first subscribe-acknowledgement from Redis (or
// ctx.Done() / Close()). See doc-comment on [SigilInvalidateSubscription.Ready].
func (s *AnchorsChangedSubscription) Ready(ctx context.Context) error {
	if s == nil {
		return errors.New("redis.AnchorsChangedSubscription.Ready: nil subscription")
	}
	select {
	case <-s.ready:
		return nil
	case <-s.stopped:
		return errors.New("redis.AnchorsChangedSubscription.Ready: subscription stopped before ready")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops the subscribe loop and closes the Redis pubsub handle and the
// Go channel. Idempotent.
func (s *AnchorsChangedSubscription) Close() error {
	if s == nil {
		return nil
	}
	return s.closeOnce()
}

// SubscribeAnchorsChanged subscribes to [AnchorsChangedChannel] and starts the
// forwarder goroutine.
func SubscribeAnchorsChanged(ctx context.Context, c *Client, logger *slog.Logger) (*AnchorsChangedSubscription, error) {
	if c == nil {
		return nil, errors.New("redis.SubscribeAnchorsChanged: nil client")
	}
	if logger == nil {
		return nil, errors.New("redis.SubscribeAnchorsChanged: nil logger")
	}

	ps := c.underlying().Subscribe(ctx, AnchorsChangedChannel)

	s := &AnchorsChangedSubscription{
		ps:      ps,
		out:     make(chan *AnchorsChanged, anchorsChangedSubBufferSize),
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

func (s *AnchorsChangedSubscription) run(ctx context.Context, closed <-chan struct{}) {
	defer close(s.out)
	defer close(s.stopped)

	if _, err := s.ps.Receive(ctx); err != nil {
		s.logger.Warn("redis.SubscribeAnchorsChanged: initial Receive failed",
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
			s.logger.Warn("redis.SubscribeAnchorsChanged: ReceiveMessage failed",
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
			// Channel full — the subscriber hasn't caught up on reload +
			// re-broadcast. Dropping is safe: each message just means "reload
			// the set", and the latest reload reads the full up-to-date DB
			// set anyway.
			s.logger.Warn("redis.SubscribeAnchorsChanged: forward channel full, dropping")
		}
	}
}

func (s *AnchorsChangedSubscription) decodeMessage(payload string) (*AnchorsChanged, bool) {
	var env anchorsChangedEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		s.logger.Warn("redis.SubscribeAnchorsChanged: envelope unmarshal failed",
			slog.Any("error", err))
		return nil, false
	}
	return &AnchorsChanged{At: env.At}, true
}
