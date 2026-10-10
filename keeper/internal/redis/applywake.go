package redis

// Cluster-wide "a row of this run moved" notice (NIM-984): wakes the barrier of
// a scenario run now instead of at its next poll of `apply_runs`.
//
// The row and the barrier are often on different instances: a host's RunResult
// is written by the instance holding its stream, a queued step's failure by the
// Acolyte that claimed it, a Cancel by whichever instance took the request —
// while the barrier runs where the run started. Postgres cannot tell that
// instance; this channel can.
//
// The notice carries no status. Postgres stays the only truth, and a lost notice
// costs the barrier one fallback poll, never a wrong answer — so delivery is
// best-effort and a full forward buffer drops.
//
// Self-filter as in [SubscribeSoulForget]: the publishing instance wakes its own
// barriers synchronously (applywake.Hub.Wake) and drops its echo.
//
// Convention `apply:wake` follows `<subsystem>:<event>` as `apply:summons` does.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/redis/go-redis/v9"
)

// ApplyWakeChannel is the Redis channel carrying barrier wakes. Fixed, not per
// run: the instance holding a barrier cannot be known to the writer.
const ApplyWakeChannel = "apply:wake"

type applyWakeEnvelope struct {
	ApplyID   string `json:"apply_id"`
	OriginKID string `json:"origin_kid"`
}

// PublishApplyWake publishes a wake for applyID.
func PublishApplyWake(ctx context.Context, c *Client, applyID, originKID string) error {
	if c == nil {
		return errors.New("redis.PublishApplyWake: nil client")
	}
	if applyID == "" {
		return errors.New("redis.PublishApplyWake: empty applyID")
	}
	if originKID == "" {
		return errors.New("redis.PublishApplyWake: empty originKID")
	}
	env, err := json.Marshal(applyWakeEnvelope{ApplyID: applyID, OriginKID: originKID})
	if err != nil {
		return fmt.Errorf("redis.PublishApplyWake: envelope marshal: %w", err)
	}
	if err := c.underlying().Publish(ctx, ApplyWakeChannel, env).Err(); err != nil {
		return fmt.Errorf("redis.PublishApplyWake: PUBLISH %q: %w", ApplyWakeChannel, err)
	}
	return nil
}

// applyWakeSubBufferSize is the buffer between the PubSub loop and the consumer.
// Every host terminal of every run in the cluster crosses it, but the consumer
// only does a map lookup per wake.
const applyWakeSubBufferSize = 256

// ApplyWakeSubscription is a handle to the [ApplyWakeChannel] subscription.
// Lifecycle matches [SoulForgetSubscription].
type ApplyWakeSubscription struct {
	ps        *redis.PubSub
	out       chan string
	selfKID   string
	logger    *slog.Logger
	ready     chan struct{}
	stopped   chan struct{}
	closeOnce func() error
}

// Channel carries the apply_id of every wake published by another instance.
// Closed when the subscription stops.
func (s *ApplyWakeSubscription) Channel() <-chan string {
	if s == nil {
		return nil
	}
	return s.out
}

// Ready blocks until the first subscribe acknowledgement from Redis (or
// ctx.Done() / Close()).
func (s *ApplyWakeSubscription) Ready(ctx context.Context) error {
	if s == nil {
		return errors.New("redis.ApplyWakeSubscription.Ready: nil subscription")
	}
	select {
	case <-s.ready:
		return nil
	case <-s.stopped:
		return errors.New("redis.ApplyWakeSubscription.Ready: subscription stopped before ready")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops the subscribe loop and closes the pubsub handle and Go channel.
// Idempotent.
func (s *ApplyWakeSubscription) Close() error {
	if s == nil {
		return nil
	}
	return s.closeOnce()
}

// SubscribeApplyWake subscribes to [ApplyWakeChannel] and starts the forwarder
// goroutine. selfKID drives the self-filter.
func SubscribeApplyWake(ctx context.Context, c *Client, selfKID string, logger *slog.Logger) (*ApplyWakeSubscription, error) {
	if c == nil {
		return nil, errors.New("redis.SubscribeApplyWake: nil client")
	}
	if selfKID == "" {
		return nil, errors.New("redis.SubscribeApplyWake: empty selfKID")
	}
	if logger == nil {
		return nil, errors.New("redis.SubscribeApplyWake: nil logger")
	}

	ps := c.underlying().Subscribe(ctx, ApplyWakeChannel)
	s := &ApplyWakeSubscription{
		ps:      ps,
		out:     make(chan string, applyWakeSubBufferSize),
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

func (s *ApplyWakeSubscription) run(ctx context.Context, closed <-chan struct{}) {
	defer close(s.out)
	defer close(s.stopped)

	if _, err := s.ps.Receive(ctx); err != nil {
		s.logger.Warn("redis.SubscribeApplyWake: initial Receive failed", slog.Any("error", err))
		return
	}
	close(s.ready)

	for {
		msg, err := s.ps.ReceiveMessage(ctx)
		if err != nil {
			select {
			case <-closed:
				return
			case <-ctx.Done():
				return
			default:
			}
			s.logger.Warn("redis.SubscribeApplyWake: ReceiveMessage failed", slog.Any("error", err))
			return
		}
		var env applyWakeEnvelope
		if err := json.Unmarshal([]byte(msg.Payload), &env); err != nil || env.ApplyID == "" {
			s.logger.Warn("redis.SubscribeApplyWake: malformed wake, ignoring", slog.Any("error", err))
			continue
		}
		if env.OriginKID == s.selfKID {
			continue
		}
		select {
		case s.out <- env.ApplyID:
		default:
			s.logger.Debug("redis.SubscribeApplyWake: forward channel full, dropping a wake",
				slog.String("apply_id", env.ApplyID))
		}
	}
}
