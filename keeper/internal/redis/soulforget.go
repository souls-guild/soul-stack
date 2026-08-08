package redis

// Cluster-wide "this host has been forgotten" notice, plus the per-SID cache
// purge that goes with it (NIM-386, `soul.forget`).
//
// Problem: erasing the `souls` row makes the host unable to RECONNECT — seed
// auth is an allowlist over `soul_seeds.fingerprint` and the seeds cascade
// away with the row — but it does nothing to a stream that is open right now.
// The seed is checked once, at stream open, so a connected host keeps talking
// to a Keeper that no longer has a row for it, and only the instance holding
// that stream can close it. Postgres cannot reach across instances; this
// channel can.
//
// Shape mirrors [RBACInvalidateChannel], with one deliberate difference: the
// message carries a payload. "Re-read your snapshot" needs no argument, "close
// the stream to THIS SID" does.
//
// Self-filter, and why it is safe here. The publishing instance drops its own
// echo, exactly as [SubscribeRBACInvalidate] does — and for the same reason it
// is not a hole: the origin does the local half itself, synchronously, in
// `soulforget.Erase` (StreamManager.Close), rather than waiting to hear its
// own broadcast come back. That ordering is the NIM-421 lesson applied: the
// node that performed the action must not be the last one to act on it.
//
// Unlike the RBAC channel there is no TTL-poll fallback behind this one, so a
// lost message is not eventually repaired. That is why `soulforget.Erase`
// publishes BEFORE the delete (a publish that cannot even go out aborts the
// whole operation with nothing destroyed) and again after it — see
// [soulforget.ErrTeardownUnavailable]. Adding a periodic "is my streaming SID
// still registered?" sweep would make the delivery guarantee unnecessary; it
// is deliberately not in this change.
//
// Convention `soul:forget` follows `<resource>:<verb>` as `rbac:invalidate`
// does. It cannot collide with the per-SID key namespace `soul:<sid>:*`: those
// are keys, this is a channel, and the key forms carry a third segment.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// SoulForgetChannel is the Redis channel carrying "forget this SID" notices.
// Fixed, not per-SID: an instance cannot subscribe per host, it has to hear
// about hosts it happens to be streaming.
const SoulForgetChannel = "soul:forget"

// soulForgetEnvelope is the JSON wire form of one notice.
type soulForgetEnvelope struct {
	SID       string    `json:"sid"`
	OriginKID string    `json:"origin_kid"`
	At        time.Time `json:"at"`
}

// SoulForget is the unpacked notice delivered to a subscriber: close any
// EventStream you hold for this SID, the host is no longer registered.
type SoulForget struct {
	SID       string
	OriginKID string
	At        time.Time
}

// PublishSoulForget publishes a forget notice for one SID and returns how many
// subscribers received it.
//
// The count is informational — zero subscribers is normal on a single-instance
// cluster and is NOT an error. The error return means the publish itself could
// not happen, which the caller treats as "the cluster cannot be told", and
// that is a hard stop before the delete rather than something to swallow.
func PublishSoulForget(ctx context.Context, c *Client, sid, originKID string) (int64, error) {
	if c == nil {
		return 0, errors.New("redis.PublishSoulForget: nil client")
	}
	if sid == "" {
		return 0, errors.New("redis.PublishSoulForget: empty sid")
	}
	if originKID == "" {
		return 0, errors.New("redis.PublishSoulForget: empty originKID")
	}

	env, err := json.Marshal(soulForgetEnvelope{
		SID:       sid,
		OriginKID: originKID,
		At:        time.Now().UTC(),
	})
	if err != nil {
		return 0, fmt.Errorf("redis.PublishSoulForget: envelope marshal: %w", err)
	}

	n, err := c.underlying().Publish(ctx, SoulForgetChannel, env).Result()
	if err != nil {
		return 0, fmt.Errorf("redis.PublishSoulForget: PUBLISH %q: %w", SoulForgetChannel, err)
	}
	return n, nil
}

// PurgeSoulKeys removes the per-SID Redis state of a forgotten host and
// returns how many keys it deleted.
//
// It covers the heartbeat hash `soul:<sid>:hb` (which also holds the announced
// capabilities and Soul version) and the two utilization keys. The heartbeat
// hash is the one that matters: it carries NO TTL, and no Reaper rule deletes
// it — `purge_souls` and `mark_disconnected` are SQL-only despite what
// heartbeat.go used to claim — so this call is the ONLY thing in the codebase
// that collects it. A host forgotten without it leaves a key behind forever.
//
// Which also means the purge is not airtight: [TouchHeartbeat] re-creates the
// hash unconditionally, so a frame arriving from a stream that has not finished
// closing can resurrect a key nothing will ever collect again. The window is
// small (the stream is cancelled in the same call) and the leak is inert — a
// forgotten SID has no row to join against — but it is a leak, and closing it
// needs a sweep over orphaned `soul:*:hb`, not a bigger DEL here.
//
// The SID lease `soul:<sid>:lock` is deliberately NOT touched. It belongs to
// whichever instance holds the stream and is renewed by that instance's
// goroutine; deleting it from the outside would either be undone by the next
// renewal or, worse, let a second instance take a lease the first still thinks
// it owns. The lease is released by closing the stream, which is what the
// forget notice asks for.
func PurgeSoulKeys(ctx context.Context, c *Client, sid string) (int64, error) {
	if c == nil {
		return 0, errors.New("redis.PurgeSoulKeys: nil client")
	}
	if sid == "" {
		return 0, errors.New("redis.PurgeSoulKeys: empty sid")
	}
	keys := []string{
		HeartbeatKey(sid),
		UtilizationKey(sid),
		UtilizationWindowKey(sid),
	}
	n, err := c.underlying().Del(ctx, keys...).Result()
	if err != nil {
		return 0, fmt.Errorf("redis.PurgeSoulKeys: DEL %v: %w", keys, err)
	}
	return n, nil
}

// soulForgetSubBufferSize is the buffer between the PubSub loop and the
// consumer. Forgets are rare operator actions; a small margin is plenty.
const soulForgetSubBufferSize = 16

// SoulForgetSubscription is a handle to the [SoulForgetChannel] subscription.
// Lifecycle matches [RBACInvalidateSubscription].
type SoulForgetSubscription struct {
	ps        *redis.PubSub
	out       chan *SoulForget
	selfKID   string
	logger    *slog.Logger
	ready     chan struct{}
	stopped   chan struct{}
	closeOnce func() error
}

// Channel is the read side of the Go channel with unpacked notices.
func (s *SoulForgetSubscription) Channel() <-chan *SoulForget {
	if s == nil {
		return nil
	}
	return s.out
}

// Ready blocks until the first subscribe acknowledgement from Redis (or
// ctx.Done() / Close()).
func (s *SoulForgetSubscription) Ready(ctx context.Context) error {
	if s == nil {
		return errors.New("redis.SoulForgetSubscription.Ready: nil subscription")
	}
	select {
	case <-s.ready:
		return nil
	case <-s.stopped:
		return errors.New("redis.SoulForgetSubscription.Ready: subscription stopped before ready")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops the subscribe loop and closes the pubsub handle and Go channel.
// Idempotent.
func (s *SoulForgetSubscription) Close() error {
	if s == nil {
		return nil
	}
	return s.closeOnce()
}

// SubscribeSoulForget subscribes to [SoulForgetChannel] and starts the
// forwarder goroutine. selfKID drives the self-filter.
func SubscribeSoulForget(ctx context.Context, c *Client, selfKID string, logger *slog.Logger) (*SoulForgetSubscription, error) {
	if c == nil {
		return nil, errors.New("redis.SubscribeSoulForget: nil client")
	}
	if selfKID == "" {
		return nil, errors.New("redis.SubscribeSoulForget: empty selfKID")
	}
	if logger == nil {
		return nil, errors.New("redis.SubscribeSoulForget: nil logger")
	}

	ps := c.underlying().Subscribe(ctx, SoulForgetChannel)

	s := &SoulForgetSubscription{
		ps:      ps,
		out:     make(chan *SoulForget, soulForgetSubBufferSize),
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

func (s *SoulForgetSubscription) run(ctx context.Context, closed <-chan struct{}) {
	defer close(s.out)
	defer close(s.stopped)

	if _, err := s.ps.Receive(ctx); err != nil {
		s.logger.Warn("redis.SubscribeSoulForget: initial Receive failed",
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
			s.logger.Warn("redis.SubscribeSoulForget: ReceiveMessage failed",
				slog.Any("error", err))
			return
		}

		ev, ok := s.decodeMessage(msg.Payload)
		if !ok {
			continue
		}
		if ev.OriginKID == s.selfKID {
			// Self-echo. The origin closed its own stream synchronously in
			// soulforget.Erase — it does not learn about its own action from
			// the wire (NIM-421).
			s.logger.Debug("redis.SubscribeSoulForget: ignoring self-origin notice",
				slog.String("sid", ev.SID))
			continue
		}

		select {
		case s.out <- ev:
		default:
			// Full channel means the consumer is behind on stream teardown.
			// Log loudly rather than at Warn-and-forget level: unlike an RBAC
			// invalidate, there is no TTL-poll that will repair a dropped
			// notice, so this one leaves a forgotten host's stream alive.
			s.logger.Error("redis.SubscribeSoulForget: forward channel full, DROPPING a forget notice — a forgotten host may keep its stream on this instance",
				slog.String("sid", ev.SID),
				slog.String("origin_kid", ev.OriginKID))
		}
	}
}

func (s *SoulForgetSubscription) decodeMessage(payload string) (*SoulForget, bool) {
	var env soulForgetEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		s.logger.Warn("redis.SubscribeSoulForget: envelope unmarshal failed",
			slog.Any("error", err))
		return nil, false
	}
	if env.SID == "" {
		s.logger.Warn("redis.SubscribeSoulForget: notice without a SID, ignoring")
		return nil, false
	}
	return &SoulForget{
		SID:       env.SID,
		OriginKID: env.OriginKID,
		At:        env.At,
	}, true
}
