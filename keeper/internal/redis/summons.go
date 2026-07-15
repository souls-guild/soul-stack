package redis

// Summons — a Redis pub/sub signal "planned jobs have appeared" (ADR-027(a)).
//
// The problem: on dispatch, the scenario-runner writes `apply_runs` rows
// with status `planned` (slice 1.4 cutover) — but which Keeper instance they
// appeared on isn't known ahead of time. The Acolyte pools on all instances
// already periodically scan `planned` (poll-tick, see acolyte/pool.go), but
// the poll interval is a pickup delay. Summons cuts it down to
// milliseconds: the node that wrote the `planned` row PUBLISHes a signal,
// any Acolyte on SUBSCRIBE wakes up and claims it right away.
//
// Difference from [PublishApplyEvent] / [PublishRBACInvalidate]: no
// self-filter is needed here. A planned job written on the initiator must be
// picked up by any Acolyte on any node — including the initiating one
// itself (its pool is just as much a claim candidate). So the envelope
// carries `origin_kid` only for diagnostics/logs, and the subscriber does
// NOT drop its own echo.
//
// Best-effort + poll-fallback: Redis pub/sub has no persistence. A lost
// signal (a node reconnecting, the broker blinking, no Acolyte subscribed
// yet) doesn't lose the job — the nearest poll-tick picks it up
// (acolyte/pool.go::defaultPollInterval). Summons only SPEEDS UP the
// wakeup.
//
// Convention `apply:summons` (fixed, propose-and-wait passed,
// naming-rules.md) — the `<subsystem>:<event>` style, like `rbac:invalidate`.
// A fixed channel (not per-ID): any planned appearing on any node sends
// here; there's no payload — the signal is "check the queue", the Acolyte
// pulls specifics from Postgres itself (claim via `FOR UPDATE SKIP LOCKED`).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// SummonsChannel — the Redis channel for the Summons signal of planned jobs.
// Fixed (not per-ID): any planned row on any node sends here.
const SummonsChannel = "apply:summons"

// summonsEnvelope — the JSON wrapper for a single Summons signal.
//
// `OriginKID` — the KID of the Keeper instance that published the signal;
// unlike applybus/rbacinvalidate, it's NOT used for self-filter (see the
// file doc-comment) — diagnostics only. `At` — the publish moment
// (log/diagnostics).
type summonsEnvelope struct {
	OriginKID string    `json:"origin_kid"`
	At        time.Time `json:"at"`
}

// PublishSummons publishes a Summons signal to [SummonsChannel]. Returns the
// number of subscribers that received the message.
//
// Best-effort: the caller (the scenario-runner after writing the planned
// row, slice 1.4) swallows the error — the job is already persisted in
// `apply_runs`, losing the publish isn't critical (the Acolyte poll-fallback
// picks it up). The publish call-site is NOT introduced in this slice: only
// the ready-made mechanism lives here.
func PublishSummons(ctx context.Context, c *Client, originKID string) (int64, error) {
	if c == nil {
		return 0, errors.New("redis.PublishSummons: nil client")
	}
	if originKID == "" {
		return 0, errors.New("redis.PublishSummons: empty originKID")
	}

	env, err := json.Marshal(summonsEnvelope{
		OriginKID: originKID,
		At:        time.Now().UTC(),
	})
	if err != nil {
		return 0, fmt.Errorf("redis.PublishSummons: envelope marshal: %w", err)
	}

	n, err := c.underlying().Publish(ctx, SummonsChannel, env).Result()
	if err != nil {
		return 0, fmt.Errorf("redis.PublishSummons: PUBLISH %q: %w", SummonsChannel, err)
	}
	return n, nil
}

// SummonsSubscription — a handle on a [SummonsChannel] subscription.
//
// Lifecycle like [RBACInvalidateSubscription], but without a read-side Go
// channel: Summons carries no payload, the subscriber just needs to "wake
// up". So on every valid signal, the goroutine calls the `onSignal` callback
// (for Acolyte — pool.Notify). Close() stops the goroutine and closes the
// Redis pubsub handle.
type SummonsSubscription struct {
	ps        *redis.PubSub
	onSignal  func()
	logger    *slog.Logger
	ready     chan struct{}
	stopped   chan struct{}
	closeOnce func() error
}

// Ready blocks until the first subscribe acknowledgement from Redis (or
// ctx.Done() / Close()). See doc-comment [RBACInvalidateSubscription.Ready].
func (s *SummonsSubscription) Ready(ctx context.Context) error {
	if s == nil {
		return errors.New("redis.SummonsSubscription.Ready: nil subscription")
	}
	select {
	case <-s.ready:
		return nil
	case <-s.stopped:
		return errors.New("redis.SummonsSubscription.Ready: subscription stopped before ready")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops the subscribe loop and closes the Redis pubsub handle. Idempotent.
func (s *SummonsSubscription) Close() error {
	if s == nil {
		return nil
	}
	return s.closeOnce()
}

// SubscribeSummons subscribes to [SummonsChannel] and starts a
// goroutine-forwarder: calls onSignal on every valid signal. No self-filter
// (see the file doc-comment) — origin_kid is ignored on delivery.
func SubscribeSummons(ctx context.Context, c *Client, onSignal func(), logger *slog.Logger) (*SummonsSubscription, error) {
	if c == nil {
		return nil, errors.New("redis.SubscribeSummons: nil client")
	}
	if onSignal == nil {
		return nil, errors.New("redis.SubscribeSummons: nil onSignal")
	}
	if logger == nil {
		return nil, errors.New("redis.SubscribeSummons: nil logger")
	}

	ps := c.underlying().Subscribe(ctx, SummonsChannel)

	s := &SummonsSubscription{
		ps:       ps,
		onSignal: onSignal,
		logger:   logger,
		ready:    make(chan struct{}),
		stopped:  make(chan struct{}),
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

func (s *SummonsSubscription) run(ctx context.Context, closed <-chan struct{}) {
	defer close(s.stopped)

	if _, err := s.ps.Receive(ctx); err != nil {
		s.logger.Warn("redis.SubscribeSummons: initial Receive failed",
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
			s.logger.Warn("redis.SubscribeSummons: ReceiveMessage failed",
				slog.Any("error", err))
			return
		}

		// The envelope is parsed only to validate/diagnose origin_kid; the
		// payload carries no semantics. A broken envelope must not swallow
		// the wake — better to check the queue an extra time than miss a
		// planned job.
		if env, ok := s.decodeMessage(msg.Payload); ok {
			s.logger.Debug("redis.SubscribeSummons: summons received",
				slog.String("origin_kid", env.OriginKID))
		}
		s.onSignal()
	}
}

func (s *SummonsSubscription) decodeMessage(payload string) (summonsEnvelope, bool) {
	var env summonsEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		s.logger.Warn("redis.SubscribeSummons: envelope unmarshal failed",
			slog.Any("error", err))
		return summonsEnvelope{}, false
	}
	return env, true
}
